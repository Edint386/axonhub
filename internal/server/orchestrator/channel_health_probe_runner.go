package orchestrator

import (
	"context"
	"strings"
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
	ctx = authz.WithSystemBypass(ctx, "active-channel-health-probe")
	ctx = contexts.WithSource(ctx, request.SourceTest)

	targets, err := runner.service.DueTargets(ctx, time.Now().UTC())
	if err != nil {
		log.Error(ctx, "failed to list scheduled channel health probes", log.Cause(err))
		return
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(channelHealthProbeMaxConcurrency)
	for _, target := range targets {
		target := target
		group.Go(func() error {
			startedAt := time.Now().UTC()
			run, claimed, err := runner.service.ClaimScheduledRun(groupCtx, target, startedAt)
			if err != nil {
				log.Error(groupCtx, "failed to claim scheduled channel health probe",
					log.Int("channel_id", target.ChannelID),
					log.String("model_id", target.ModelID),
					log.Cause(err))
				return nil
			}
			if !claimed {
				return nil
			}

			if _, err := runner.execute(groupCtx, run); err != nil {
				log.Error(groupCtx, "failed to persist scheduled channel health probe result",
					log.Int("channel_id", target.ChannelID),
					log.String("model_id", target.ModelID),
					log.Cause(err))
			}
			return nil
		})
	}
	_ = group.Wait()
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
