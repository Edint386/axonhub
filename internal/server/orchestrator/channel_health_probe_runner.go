package orchestrator

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/fx"
	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/scheduler"
)

const (
	channelHealthProbeMaxConcurrency = 4
	channelHealthProbeTimeout        = 2 * time.Minute
	channelHealthProbePersistTimeout = 10 * time.Second
	channelHealthProbeMaxErrorRunes  = 2048
)

type ChannelHealthProbeRunnerParams struct {
	fx.In

	Service *biz.ChannelHealthProbeService
	Tester  *TestChannelOrchestrator
}

// ChannelHealthProbeRunner executes synthetic requests. Persistence and
// configuration live in biz.ChannelHealthProbeService so this layer only owns
// request orchestration and scheduling.
type ChannelHealthProbeRunner struct {
	service *biz.ChannelHealthProbeService
	tester  *TestChannelOrchestrator

	// scanning guards against overlapping scans. The cron fires every minute, but a
	// priority scan walks its channels sequentially and a single probe may take up to
	// channelHealthProbeTimeout, so a scan can easily outlive its own tick. Without
	// this, ticks pile up and several scans walk the list concurrently. Duplicate work
	// is already impossible (ScheduleKey is unique per bucket), so this only avoids
	// the wasted queries and goroutines.
	scanning atomic.Bool
}

func NewChannelHealthProbeRunner(params ChannelHealthProbeRunnerParams) *ChannelHealthProbeRunner {
	return &ChannelHealthProbeRunner{service: params.Service, tester: params.Tester}
}

func (runner *ChannelHealthProbeRunner) RegisterScheduledTasks(
	ctx context.Context,
	s *scheduler.Scheduler,
) error {
	return s.Register(ctx, scheduler.TaskSpec{
		Name:        "active-channel-health-probe",
		Description: "Run configured synthetic channel generation probes every minute",
		CronExpr:    "* * * * *",
		Timezone:    "UTC",
	}, runner.runScheduled)
}

func (runner *ChannelHealthProbeRunner) runScheduled(ctx context.Context) {
	if !runner.scanning.CompareAndSwap(false, true) {
		log.Debug(ctx, "skipping scheduled channel health probe scan, previous scan still running")

		return
	}
	defer runner.scanning.Store(false)

	ctx = authz.WithSystemBypass(ctx, "active-channel-health-probe")
	ctx = contexts.WithSource(ctx, request.SourceTest)

	policy, err := runner.service.ScanPolicy(ctx)
	if err != nil {
		log.Error(ctx, "failed to load scheduled channel health probe policy", log.Cause(err))
		return
	}
	targets, err := runner.service.DueTargetsWithPolicy(ctx, time.Now().UTC(), policy)
	if err != nil {
		log.Error(ctx, "failed to list scheduled channel health probes", log.Cause(err))
		return
	}

	if policy.Enabled {
		// The chain's stop condition is the EFFECTIVE ceiling, not the operator's
		// global setting: see ChannelHealthProbeService.EffectiveAcceptableLatencyMs.
		// A read failure degrades to the global value rather than abandoning the scan,
		// which would leave every channel without fresh probe data.
		acceptableLatencyMs, err := runner.service.EffectiveAcceptableLatencyMs(ctx)
		if err != nil {
			log.Error(ctx, "failed to resolve effective acceptable latency for scheduled channel health probes",
				log.Cause(err))

			acceptableLatencyMs = policy.AcceptableLatencyMs
		}

		runner.runPriorityScheduled(ctx, targets, policy, acceptableLatencyMs)

		return
	}

	runner.runConcurrentScheduled(ctx, targets)
}

func (runner *ChannelHealthProbeRunner) runConcurrentScheduled(
	ctx context.Context,
	targets []biz.ChannelHealthProbeTarget,
) {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(channelHealthProbeMaxConcurrency)
	for _, target := range targets {
		target := target
		group.Go(func() error {
			runner.executeScheduledTarget(groupCtx, target)
			return nil
		})
	}
	_ = group.Wait()
}

func (runner *ChannelHealthProbeRunner) runPriorityScheduled(
	ctx context.Context,
	targets []biz.ChannelHealthProbeTarget,
	policy biz.ActiveHealthProbeScanSetting,
	acceptableLatencyMs int,
) {
	groups := make(map[string][]biz.ChannelHealthProbeTarget)
	modelIDs := make([]string, 0)
	for _, target := range targets {
		if _, ok := groups[target.ModelID]; !ok {
			modelIDs = append(modelIDs, target.ModelID)
		}
		groups[target.ModelID] = append(groups[target.ModelID], target)
	}
	for _, modelID := range modelIDs {
		modelTargets := groups[modelID]
		sort.SliceStable(modelTargets, func(i, j int) bool {
			if modelTargets[i].Priority != modelTargets[j].Priority {
				return modelTargets[i].Priority > modelTargets[j].Priority
			}
			if modelTargets[i].OrderingWeight != modelTargets[j].OrderingWeight {
				return modelTargets[i].OrderingWeight > modelTargets[j].OrderingWeight
			}
			return modelTargets[i].ChannelID < modelTargets[j].ChannelID
		})
		groups[modelID] = modelTargets
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(channelHealthProbeMaxConcurrency)
	for _, modelID := range modelIDs {
		modelTargets := groups[modelID]
		group.Go(func() error {
			skipped := runPriorityProbeTargets(
				groupCtx,
				modelTargets,
				acceptableLatencyMs,
				policy.ExtraChannels,
				runner.executeScheduledTarget,
			)
			if len(skipped) == 0 {
				return nil
			}
			if err := runner.service.SkipScheduledTargets(groupCtx, skipped, time.Now().UTC()); err != nil {
				log.Error(groupCtx, "failed to persist skipped scheduled channel health probes",
					log.String("model_id", modelID),
					log.Cause(err))
			}
			return nil
		})
	}
	_ = group.Wait()
}

type scheduledProbeTargetExecutor func(
	context.Context,
	biz.ChannelHealthProbeTarget,
) (*biz.ChannelHealthProbeRunRecord, bool)

// runPriorityProbeTargets returns targets that should be persisted as skipped.
// A false executor result means another scheduler instance owns this ordered
// group, so this instance exits without skipping anything.
func runPriorityProbeTargets(
	ctx context.Context,
	targets []biz.ChannelHealthProbeTarget,
	acceptableLatencyMs int,
	extraChannels int,
	execute scheduledProbeTargetExecutor,
) []biz.ChannelHealthProbeTarget {
	accepted := false
	remainingExtra := 0
	for i, target := range targets {
		if accepted && remainingExtra == 0 {
			return targets[i:]
		}

		wasAccepted := accepted
		record, proceed := execute(ctx, target)
		if !proceed {
			return nil
		}
		if wasAccepted {
			remainingExtra--
		} else if channelHealthProbeRunIsAcceptable(record, acceptableLatencyMs) {
			accepted = true
			remainingExtra = extraChannels
		}
	}

	return nil
}

func channelHealthProbeRunIsAcceptable(run *biz.ChannelHealthProbeRunRecord, thresholdMs int) bool {
	if run == nil || run.Status != "healthy" || thresholdMs <= 0 {
		return false
	}

	latencyMs := run.TotalMs
	if run.Stream {
		if run.TTFTMs != nil {
			latencyMs = *run.TTFTMs
		} else if run.TTFBMs != nil {
			latencyMs = *run.TTFBMs
		}
	} else if run.TTFBMs != nil {
		latencyMs = *run.TTFBMs
	} else if run.TTFTMs != nil {
		latencyMs = *run.TTFTMs
	}

	return latencyMs >= 0 && latencyMs <= float64(thresholdMs)
}

func (runner *ChannelHealthProbeRunner) executeScheduledTarget(
	ctx context.Context,
	target biz.ChannelHealthProbeTarget,
) (*biz.ChannelHealthProbeRunRecord, bool) {
	startedAt := time.Now().UTC()
	run, claimed, err := runner.service.ClaimScheduledRun(ctx, target, startedAt)
	if err != nil {
		log.Error(ctx, "failed to claim scheduled channel health probe",
			log.Int("channel_id", target.ChannelID),
			log.String("model_id", target.ModelID),
			log.Cause(err))
		return nil, true
	}
	if !claimed {
		return nil, false
	}

	record, err := runner.execute(ctx, run)
	if err != nil {
		log.Error(ctx, "failed to persist scheduled channel health probe result",
			log.Int("channel_id", target.ChannelID),
			log.String("model_id", target.ModelID),
			log.Cause(err))
		return nil, true
	}

	return record, true
}

func (runner *ChannelHealthProbeRunner) RunManual(
	ctx context.Context,
	input biz.RunChannelHealthProbeInput,
) (*biz.ChannelHealthProbeRunRecord, error) {
	ctx = contexts.WithSource(ctx, request.SourceTest)
	run, err := runner.service.CreateManualRun(ctx, input, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return runner.execute(ctx, run)
}

func (runner *ChannelHealthProbeRunner) execute(
	ctx context.Context,
	run *ent.ChannelHealthProbeRun,
) (*biz.ChannelHealthProbeRunRecord, error) {
	probeCtx, cancel := context.WithTimeout(ctx, channelHealthProbeTimeout)
	modelID := run.ModelID
	stream := run.Stream
	result, probeErr := runner.tester.TestChannelWithOptions(
		probeCtx,
		objects.GUID{Type: "Channel", ID: run.ChannelID},
		&modelID,
		nil,
		TestChannelOptions{Stream: &stream},
	)
	cancel()

	healthy := result != nil && result.Success && probeErr == nil
	var (
		ttfbMs  *float64
		ttftMs  *float64
		totalMs float64
	)
	if result != nil {
		ttfbMs = result.TTFBMs
		ttftMs = result.TTFTMs
		totalMs = result.TotalMs
	}
	if totalMs <= 0 {
		totalMs = float64(time.Since(run.StartedAt)) / float64(time.Millisecond)
	}
	errorMessage := channelHealthProbeError(result, probeErr)

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), channelHealthProbePersistTimeout)
	defer persistCancel()
	return runner.service.CompleteRun(
		persistCtx,
		run.ID,
		healthy,
		ttfbMs,
		ttftMs,
		totalMs,
		errorMessage,
		time.Now().UTC(),
	)
}

func channelHealthProbeError(result *TestChannelResult, probeErr error) *string {
	message := ""
	if probeErr != nil {
		message = probeErr.Error()
	} else if result != nil && result.Error != nil {
		message = *result.Error
	} else if result == nil || !result.Success {
		message = "probe failed without an upstream error message"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}

	runes := []rune(message)
	if len(runes) > channelHealthProbeMaxErrorRunes {
		message = string(runes[:channelHealthProbeMaxErrorRunes])
	}
	return &message
}
