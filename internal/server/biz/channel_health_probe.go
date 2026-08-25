package biz

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelhealthproberun"
	"github.com/looplj/axonhub/internal/ent/predicate"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

const (
	MinChannelHealthProbeIntervalMinutes  = 1
	MaxChannelHealthProbeIntervalMinutes  = 24 * 60
	DefaultChannelHealthProbeHistoryLimit = 50
	MaxChannelHealthProbeHistoryLimit     = 200
	channelHealthProbeSettingsMaxRetries  = 3
	channelHealthProbeMetricsSampleLimit  = 10_000
	channelHealthProbeRecentRunsLimit     = 15
	channelHealthProbeMetricsCacheTTL     = 30 * time.Second
)

func channelHealthProbeDescColumn(column string) entsql.Querier {
	return entsql.ExprFunc(func(builder *entsql.Builder) {
		builder.Ident(column).WriteString(" DESC")
	})
}

type ChannelHealthProbeServiceParams struct {
	fx.In

	Ent            *ent.Client
	ChannelService *ChannelService
	SystemService  *SystemService
}

// ChannelHealthProbeService owns active probe configuration and persisted run
// state. Executing the upstream request remains in the orchestrator layer.
type ChannelHealthProbeService struct {
	*AbstractService

	channelService *ChannelService
	systemService  *SystemService
	metricsCacheMu sync.RWMutex
	metricsCache   *channelHealthProbeMetricsCache
}

func NewChannelHealthProbeService(params ChannelHealthProbeServiceParams) *ChannelHealthProbeService {
	return &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: params.Ent},
		channelService:  params.ChannelService,
		systemService:   params.SystemService,
	}
}

type UpdateChannelHealthProbeSettingsInput struct {
	ChannelID       objects.GUID
	IntervalMinutes int
}

type RunChannelHealthProbeInput struct {
	ChannelID objects.GUID
	ModelID   string
	Stream    bool
}

type UpdateChannelHealthProbePolicyInput struct {
	Enabled             bool
	AcceptableLatencyMs int
	ExtraChannels       int
	P95LookbackHours    int
	Models              []ActiveHealthProbeModelSetting
}

type ChannelHealthProbePolicy struct {
	Enabled                      bool
	AcceptableLatencyMs          int
	ExtraChannels                int
	P95LookbackHours             int
	APIKeyMaxFirstTokenLatencyMs *float64
	AvailableModels              []string
	Models                       []ActiveHealthProbeModelSetting
}

type ChannelHealthProbeHistoryInput struct {
	ChannelID *objects.GUID
	ModelID   *string
	Status    *string
	Source    *string
	Offset    int
	Limit     int
}

type ChannelHealthProbeRunRecord struct {
	ID           objects.GUID
	ChannelID    objects.GUID
	ModelID      string
	Source       string
	Status       string
	Stream       bool
	TTFBMs       *float64
	TTFTMs       *float64
	TotalMs      float64
	ErrorMessage *string
	StartedAt    time.Time
	CompletedAt  *time.Time
	CreatedAt    time.Time
}

type ChannelHealthProbeModelOverview struct {
	ModelID      string
	Enabled      bool
	Stream       bool
	FirstTokenMs *float64
	P95Ms        *float64
	LastProbedAt *time.Time
	SampleCount  int
	LatestRun    *ChannelHealthProbeRunRecord
}

type ChannelHealthProbeChannelOverview struct {
	ChannelID            objects.GUID
	ChannelName          string
	ChannelStatus        string
	Priority             int
	Enabled              bool
	IntervalMinutes      int
	ModelPriceMultiplier float64
	PrimaryModelID       *string
	RecentRuns           []*ChannelHealthProbeRunRecord
	Models               []*ChannelHealthProbeModelOverview
}

type ChannelHealthProbeHistoryPage struct {
	Items      []*ChannelHealthProbeRunRecord
	TotalCount int
}

type ChannelHealthProbeTarget struct {
	ChannelID       int
	ModelID         string
	Stream          bool
	Priority        int
	OrderingWeight  int
	IntervalMinutes int
	ScheduleKey     string
}

type channelHealthProbeMetricsCache struct {
	key       string
	expiresAt time.Time
	metrics   map[string]channelHealthProbeModelMetrics
}

func (svc *ChannelHealthProbeService) ScanPolicy(ctx context.Context) (ActiveHealthProbeScanSetting, error) {
	if svc.systemService == nil {
		return defaultActiveHealthProbeScanSetting, nil
	}

	setting, err := authz.RunWithSystemBypass(ctx, "active-channel-health-probe-policy", func(bypassCtx context.Context) (*SystemChannelSettings, error) {
		return svc.systemService.ChannelSetting(bypassCtx)
	})
	if err != nil {
		return ActiveHealthProbeScanSetting{}, err
	}
	if setting.ActiveHealthProbeScan == nil {
		return defaultActiveHealthProbeScanSetting, nil
	}

	return *setting.ActiveHealthProbeScan, nil
}

func (svc *ChannelHealthProbeService) Policy(ctx context.Context) (*ChannelHealthProbePolicy, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}

	policy, err := svc.ScanPolicy(ctx)
	if err != nil {
		return nil, err
	}
	availableModels, err := svc.availableRealModelIDs(ctx)
	if err != nil {
		return nil, err
	}
	models := slices.Clone(policy.Models)
	if models == nil {
		models = []ActiveHealthProbeModelSetting{}
	}
	overview := &ChannelHealthProbePolicy{
		Enabled:             policy.Enabled,
		AcceptableLatencyMs: policy.AcceptableLatencyMs,
		ExtraChannels:       policy.ExtraChannels,
		P95LookbackHours:    policy.P95LookbackHours,
		AvailableModels:     availableModels,
		Models:              models,
	}

	if !authz.HasScope(ctx, scopes.ScopeReadAPIKeys) {
		return overview, nil
	}
	apiKeys, err := svc.entFromContext(ctx).APIKey.Query().
		Where(apikey.StatusEQ(apikey.StatusEnabled)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query API key latency thresholds: %w", err)
	}
	for _, key := range apiKeys {
		profile := key.GetActiveProfile()
		if profile == nil || profile.MaxFirstTokenLatencyMs == nil || *profile.MaxFirstTokenLatencyMs <= 0 {
			continue
		}
		latency := float64(*profile.MaxFirstTokenLatencyMs)
		if overview.APIKeyMaxFirstTokenLatencyMs == nil || latency < *overview.APIKeyMaxFirstTokenLatencyMs {
			overview.APIKeyMaxFirstTokenLatencyMs = &latency
		}
	}

	return overview, nil
}

func (svc *ChannelHealthProbeService) availableRealModelIDs(ctx context.Context) ([]string, error) {
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusNEQ(channel.StatusArchived)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query real channel model IDs: %w", err)
	}

	seen := make(map[string]struct{})
	for _, ch := range channels {
		for _, modelID := range ch.SupportedModels {
			modelID = strings.TrimSpace(modelID)
			if modelID != "" {
				seen[modelID] = struct{}{}
			}
		}
		if modelID := strings.TrimSpace(ch.DefaultTestModel); modelID != "" {
			seen[modelID] = struct{}{}
		}
	}

	result := make([]string, 0, len(seen))
	for modelID := range seen {
		result = append(result, modelID)
	}
	slices.Sort(result)
	return result, nil
}

func (svc *ChannelHealthProbeService) UpdatePolicy(
	ctx context.Context,
	input UpdateChannelHealthProbePolicyInput,
) (*ChannelHealthProbePolicy, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	if svc.systemService == nil {
		return nil, fmt.Errorf("system service is not available")
	}

	current, err := svc.ScanPolicy(ctx)
	if err != nil {
		return nil, err
	}
	models := input.Models
	if models == nil {
		models = slices.Clone(current.Models)
	}
	policy := ActiveHealthProbeScanSetting{
		Enabled:             input.Enabled,
		AcceptableLatencyMs: input.AcceptableLatencyMs,
		ExtraChannels:       input.ExtraChannels,
		P95LookbackHours:    input.P95LookbackHours,
		Models:              slices.Clone(models),
	}
	err = authz.RunWithSystemBypassVoid(ctx, "update-active-channel-health-probe-policy", func(bypassCtx context.Context) error {
		return svc.systemService.UpdateChannelSetting(bypassCtx, UpdateSystemChannelSettings{ActiveHealthProbeScan: &policy})
	})
	if err != nil {
		return nil, err
	}

	return svc.Policy(ctx)
}

func normalizeAndValidateChannelHealthProbeSettings(
	settings *objects.ChannelHealthProbeSettings,
) error {
	if settings == nil {
		return fmt.Errorf("health probe settings must not be nil")
	}

	settings.Normalize()
	if settings.IntervalMinutes < MinChannelHealthProbeIntervalMinutes ||
		settings.IntervalMinutes > MaxChannelHealthProbeIntervalMinutes {
		return fmt.Errorf(
			"health probe interval must be between %d and %d minutes",
			MinChannelHealthProbeIntervalMinutes,
			MaxChannelHealthProbeIntervalMinutes,
		)
	}

	return nil
}

// UpdateSettings applies only the health-probe portion of ChannelSettings. An
// optimistic timestamp check prevents concurrent edits to unrelated settings
// from being overwritten.
func (svc *ChannelHealthProbeService) UpdateSettings(
	ctx context.Context,
	input UpdateChannelHealthProbeSettingsInput,
) (*ChannelHealthProbeChannelOverview, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	probeSettings := &objects.ChannelHealthProbeSettings{
		IntervalMinutes: input.IntervalMinutes,
	}

	_, err := svc.channelService.GetChannel(ctx, input.ChannelID.ID)
	if err != nil {
		return nil, err
	}
	if err := normalizeAndValidateChannelHealthProbeSettings(probeSettings); err != nil {
		return nil, err
	}

	var updated *ent.Channel
	for attempt := 0; attempt < channelHealthProbeSettingsMaxRetries; attempt++ {
		current, err := svc.entFromContext(ctx).Channel.Get(ctx, input.ChannelID.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to load channel health probe settings: %w", err)
		}

		settings := objects.ChannelSettings{}
		if current.Settings != nil {
			settings = *current.Settings
		}
		settings.HealthProbe = probeSettings

		updated, err = svc.entFromContext(ctx).Channel.UpdateOneID(current.ID).
			Where(channel.UpdatedAtEQ(current.UpdatedAt)).
			SetSettings(&settings).
			Save(ctx)
		if err == nil {
			break
		}
		if !ent.IsNotFound(err) {
			return nil, fmt.Errorf("failed to update channel health probe settings: %w", err)
		}
	}
	if updated == nil {
		return nil, fmt.Errorf("channel was updated concurrently; retry the operation")
	}

	svc.channelService.reloadChannelsAfterCommit(ctx)
	return svc.overviewForChannel(ctx, updated)
}

func (svc *ChannelHealthProbeService) Overview(ctx context.Context) ([]*ChannelHealthProbeChannelOverview, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	policy, err := svc.ScanPolicy(ctx)
	if err != nil {
		return nil, err
	}
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusNEQ(channel.StatusArchived)).
		Order(
			ent.Desc(channel.FieldPriority),
			ent.Desc(channel.FieldOrderingWeight),
			ent.Asc(channel.FieldID),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query channels for health overview: %w", err)
	}
	if len(channels) == 0 {
		return []*ChannelHealthProbeChannelOverview{}, nil
	}

	channelIDs := make([]int, len(channels))
	for i, ch := range channels {
		channelIDs[i] = ch.ID
	}
	latestRuns, err := svc.latestRunsByChannelAndModel(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	metrics, err := svc.metricsByChannelAndModel(ctx, channelIDs, time.Duration(policy.P95LookbackHours)*time.Hour)
	if err != nil {
		return nil, err
	}
	recentRuns, err := svc.recentRunsByChannel(ctx, channels, policy.Models)
	if err != nil {
		return nil, err
	}
	result := make([]*ChannelHealthProbeChannelOverview, 0, len(channels))
	for _, ch := range channels {
		result = append(result, buildChannelHealthProbeOverview(ch, latestRuns, metrics, recentRuns[ch.ID], policy.Models))
	}
	return result, nil
}

func (svc *ChannelHealthProbeService) overviewForChannel(
	ctx context.Context,
	ch *ent.Channel,
) (*ChannelHealthProbeChannelOverview, error) {
	policy, err := svc.ScanPolicy(ctx)
	if err != nil {
		return nil, err
	}
	latestRuns, err := svc.latestRunsByChannelAndModel(ctx, []int{ch.ID})
	if err != nil {
		return nil, err
	}
	metrics, err := svc.metricsByChannelAndModel(ctx, []int{ch.ID}, time.Duration(policy.P95LookbackHours)*time.Hour)
	if err != nil {
		return nil, err
	}
	recentRuns, err := svc.recentRunsByChannel(ctx, []*ent.Channel{ch}, policy.Models)
	if err != nil {
		return nil, err
	}
	return buildChannelHealthProbeOverview(ch, latestRuns, metrics, recentRuns[ch.ID], policy.Models), nil
}

// recentRunsByChannel returns the latest channelHealthProbeRecentRunsLimit runs
// for each channel/model pair, ordered oldest first so the primary model can
// render a complete recent-probe strip.
func (svc *ChannelHealthProbeService) recentRunsByChannel(
	ctx context.Context,
	channels []*ent.Channel,
	globalModels []ActiveHealthProbeModelSetting,
) (map[int][]*ChannelHealthProbeRunRecord, error) {
	if len(channels) == 0 {
		return map[int][]*ChannelHealthProbeRunRecord{}, nil
	}

	pairs := make([]predicate.ChannelHealthProbeRun, 0, len(channels))
	for _, ch := range channels {
		if modelID := primaryRealProbeModelID(ch, globalModels); modelID != nil {
			pairs = append(pairs, channelhealthproberun.And(
				channelhealthproberun.ChannelIDEQ(ch.ID),
				channelhealthproberun.ModelIDEQ(*modelID),
			))
		}
	}
	if len(pairs) == 0 {
		return map[int][]*ChannelHealthProbeRunRecord{}, nil
	}
	runs, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Query().
		Where(channelhealthproberun.Or(pairs...)).
		Modify(limitChannelHealthProbeMetricsPerModel(channelHealthProbeRecentRunsLimit)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query recent channel health probe runs: %w", err)
	}

	grouped := make(map[int][]*ChannelHealthProbeRunRecord, len(channels))
	for _, run := range runs {
		grouped[run.ChannelID] = append(grouped[run.ChannelID], channelHealthProbeRunRecord(run))
	}
	for channelID, bucket := range grouped {
		sort.SliceStable(bucket, func(left, right int) bool {
			if bucket[left].StartedAt.Equal(bucket[right].StartedAt) {
				return bucket[left].ID.ID < bucket[right].ID.ID
			}
			return bucket[left].StartedAt.Before(bucket[right].StartedAt)
		})
		grouped[channelID] = bucket
	}
	return grouped, nil
}

type latestChannelHealthProbeRunID struct {
	ChannelID int    `json:"channel_id"`
	ModelID   string `json:"model_id"`
	LatestID  int    `json:"latest_id"`
}

type latestChannelHealthRealRequestID struct {
	ChannelID int    `json:"channel_id"`
	ModelID   string `json:"model_id"`
	LatestID  int    `json:"latest_id"`
}

func (svc *ChannelHealthProbeService) latestRunsByChannelAndModel(
	ctx context.Context,
	channelIDs []int,
) (map[string]*ChannelHealthProbeRunRecord, error) {
	return svc.latestRunsByChannelAndModelSince(ctx, channelIDs, time.Time{}, false)
}

func (svc *ChannelHealthProbeService) latestRunsByChannelAndModelSince(
	ctx context.Context,
	channelIDs []int,
	since time.Time,
	includeSkipped bool,
) (map[string]*ChannelHealthProbeRunRecord, error) {
	if len(channelIDs) == 0 {
		return map[string]*ChannelHealthProbeRunRecord{}, nil
	}

	query := svc.entFromContext(ctx).ChannelHealthProbeRun.Query().
		Where(channelhealthproberun.ChannelIDIn(channelIDs...))
	if !includeSkipped {
		query = query.Where(channelhealthproberun.StatusNEQ(channelhealthproberun.StatusSkipped))
	}
	if !since.IsZero() {
		query = query.Where(channelhealthproberun.StartedAtGTE(since))
	}

	var latestIDs []latestChannelHealthProbeRunID
	err := query.
		GroupBy(channelhealthproberun.FieldChannelID, channelhealthproberun.FieldModelID).
		Aggregate(func(selector *entsql.Selector) string {
			return entsql.As(entsql.Max(selector.C(channelhealthproberun.FieldID)), "latest_id")
		}).
		Scan(ctx, &latestIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to query latest channel health probe IDs: %w", err)
	}

	ids := make([]int, 0, len(latestIDs))
	for _, row := range latestIDs {
		ids = append(ids, row.LatestID)
	}
	if len(ids) == 0 {
		return map[string]*ChannelHealthProbeRunRecord{}, nil
	}

	runs, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Query().
		Where(channelhealthproberun.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load latest channel health probe runs: %w", err)
	}

	result := make(map[string]*ChannelHealthProbeRunRecord, len(runs))
	for _, run := range runs {
		result[channelHealthProbeModelKey(run.ChannelID, run.ModelID)] = channelHealthProbeRunRecord(run)
	}
	return result, nil
}

type channelHealthProbeMetricAccumulator struct {
	latencies []float64
}

type channelHealthProbeModelMetrics struct {
	P95Ms       *float64
	SampleCount int
}

func channelHealthProbeMetricsKey(channelIDs []int, lookback time.Duration) string {
	sortedIDs := slices.Clone(channelIDs)
	slices.Sort(sortedIDs)
	return fmt.Sprintf("%s|%d", strings.Trim(strings.ReplaceAll(fmt.Sprint(sortedIDs), " ", ","), "[]"), lookback/time.Hour)
}

func (svc *ChannelHealthProbeService) invalidateMetricsCache() {
	svc.metricsCacheMu.Lock()
	svc.metricsCache = nil
	svc.metricsCacheMu.Unlock()
}

// limitChannelHealthProbeMetricsPerModel keeps the metrics cap independent for
// every channel/model pair. A single busy channel must not consume the sample
// budget of all other channels.
func limitChannelHealthProbeMetricsPerModel(limit int) func(*entsql.Selector) {
	return func(selector *entsql.Selector) {
		dialect := entsql.Dialect(selector.Dialect())
		rowNumber := entsql.RowNumber().
			PartitionBy(channelhealthproberun.FieldChannelID, channelhealthproberun.FieldModelID).
			OrderExpr(
				channelHealthProbeDescColumn(channelhealthproberun.FieldStartedAt),
				channelHealthProbeDescColumn(channelhealthproberun.FieldID),
			)
		with := dialect.With("channel_health_probe_source").
			As(selector.Clone()).
			With("channel_health_probe_limited").
			As(
				dialect.Select("*").
					AppendSelectExprAs(rowNumber, "channel_health_probe_row_number").
					From(dialect.Table("channel_health_probe_source")),
			)
		limited := dialect.Table("channel_health_probe_limited").As(selector.TableName())
		*selector = *dialect.Select(selector.UnqualifiedColumns()...).
			From(limited).
			Where(entsql.LTE(limited.C("channel_health_probe_row_number"), limit)).
			Prefix(with)
	}
}

func (svc *ChannelHealthProbeService) metricsByChannelAndModel(
	ctx context.Context,
	channelIDs []int,
	lookback time.Duration,
) (map[string]channelHealthProbeModelMetrics, error) {
	if len(channelIDs) == 0 {
		return map[string]channelHealthProbeModelMetrics{}, nil
	}

	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	cacheKey := channelHealthProbeMetricsKey(channelIDs, lookback)
	now := time.Now().UTC()
	svc.metricsCacheMu.RLock()
	cache := svc.metricsCache
	if cache != nil && cache.key == cacheKey && now.Before(cache.expiresAt) {
		metrics := cache.metrics
		svc.metricsCacheMu.RUnlock()
		return metrics, nil
	}
	svc.metricsCacheMu.RUnlock()

	runs, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Query().
		Where(
			channelhealthproberun.ChannelIDIn(channelIDs...),
			channelhealthproberun.StatusEQ(channelhealthproberun.StatusHealthy),
			channelhealthproberun.StartedAtGTE(time.Now().UTC().Add(-lookback)),
		).
		Modify(limitChannelHealthProbeMetricsPerModel(channelHealthProbeMetricsSampleLimit)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query channel health probe metrics: %w", err)
	}

	accumulators := make(map[string]*channelHealthProbeMetricAccumulator, len(runs))
	for _, run := range runs {
		latency, ok := channelHealthProbeFirstTokenMs(channelHealthProbeRunRecord(run))
		if !ok {
			continue
		}
		key := channelHealthProbeModelKey(run.ChannelID, run.ModelID)
		accumulator := accumulators[key]
		if accumulator == nil {
			accumulator = &channelHealthProbeMetricAccumulator{}
			accumulators[key] = accumulator
		}
		accumulator.latencies = append(accumulator.latencies, latency)
	}

	metrics := make(map[string]channelHealthProbeModelMetrics, len(accumulators))
	for key, accumulator := range accumulators {
		if len(accumulator.latencies) == 0 {
			continue
		}
		slices.Sort(accumulator.latencies)
		index := int(math.Ceil(float64(len(accumulator.latencies))*0.95)) - 1
		index = max(index, 0)
		p95 := accumulator.latencies[index]
		metrics[key] = channelHealthProbeModelMetrics{P95Ms: &p95, SampleCount: len(accumulator.latencies)}
	}
	svc.metricsCacheMu.Lock()
	svc.metricsCache = &channelHealthProbeMetricsCache{
		key:       cacheKey,
		expiresAt: now.Add(channelHealthProbeMetricsCacheTTL),
		metrics:   metrics,
	}
	svc.metricsCacheMu.Unlock()
	return metrics, nil
}

func channelHealthProbeFirstTokenMs(run *ChannelHealthProbeRunRecord) (float64, bool) {
	if run == nil {
		return 0, false
	}
	value := run.TTFBMs
	if run.Stream {
		value = run.TTFTMs
		if value == nil {
			value = run.TTFBMs
		}
	} else if value == nil {
		value = run.TTFTMs
	}
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return 0, false
	}
	return *value, true
}

func activeHealthProbeModelMap(models []ActiveHealthProbeModelSetting) map[string]ActiveHealthProbeModelSetting {
	if len(models) == 0 {
		return nil
	}
	result := make(map[string]ActiveHealthProbeModelSetting, len(models))
	for _, model := range models {
		model.ModelID = strings.TrimSpace(model.ModelID)
		if model.ModelID == "" {
			continue
		}
		result[model.ModelID] = model
	}
	return result
}

// latestRealRequestActivityByChannelAndModel returns the start time of the
// latest non-test request that selected each channel/request-model pair. Test
// requests are excluded because manual and scheduled probes are tracked in the
// dedicated health-probe table.
func (svc *ChannelHealthProbeService) latestRealRequestActivityByChannelAndModel(
	ctx context.Context,
	channelIDs []int,
	since time.Time,
) (map[string]time.Time, error) {
	if len(channelIDs) == 0 {
		return map[string]time.Time{}, nil
	}

	var latestIDs []latestChannelHealthRealRequestID
	err := svc.entFromContext(ctx).Request.Query().
		Where(
			request.ChannelIDIn(channelIDs...),
			request.SourceNEQ(request.SourceTest),
			request.CreatedAtGTE(since),
		).
		GroupBy(request.FieldChannelID, request.FieldModelID).
		Aggregate(func(selector *entsql.Selector) string {
			return entsql.As(entsql.Max(selector.C(request.FieldID)), "latest_id")
		}).
		Scan(ctx, &latestIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to query latest real channel request IDs: %w", err)
	}

	ids := make([]int, 0, len(latestIDs))
	for _, row := range latestIDs {
		ids = append(ids, row.LatestID)
	}
	if len(ids) == 0 {
		return map[string]time.Time{}, nil
	}

	requests, err := svc.entFromContext(ctx).Request.Query().
		Where(request.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load latest real channel requests: %w", err)
	}

	result := make(map[string]time.Time, len(requests))
	for _, req := range requests {
		result[channelHealthProbeModelKey(req.ChannelID, req.ModelID)] = req.CreatedAt
	}
	return result, nil
}

func isRealProbeModelSupported(ch *ent.Channel, modelID string) bool {
	if strings.TrimSpace(ch.DefaultTestModel) == modelID {
		return true
	}
	for _, supportedModelID := range ch.SupportedModels {
		if strings.TrimSpace(supportedModelID) == modelID {
			return true
		}
	}
	return false
}

func primaryRealProbeModelID(ch *ent.Channel, globalModels []ActiveHealthProbeModelSetting) *string {
	for _, setting := range globalModels {
		modelID := strings.TrimSpace(setting.ModelID)
		if setting.Enabled && modelID != "" && isRealProbeModelSupported(ch, modelID) {
			return &modelID
		}
	}
	return nil
}

func buildChannelHealthProbeOverview(
	ch *ent.Channel,
	latestRuns map[string]*ChannelHealthProbeRunRecord,
	metrics map[string]channelHealthProbeModelMetrics,
	recentRuns []*ChannelHealthProbeRunRecord,
	globalModels []ActiveHealthProbeModelSetting,
) *ChannelHealthProbeChannelOverview {
	enabled := ch.Status == channel.StatusEnabled
	interval := objects.DefaultChannelHealthProbeIntervalMinutes
	if ch.Settings != nil && ch.Settings.HealthProbe != nil {
		settings := *ch.Settings.HealthProbe
		settings.Normalize()
		interval = settings.IntervalMinutes
	}

	modelIDs := make([]string, 0, len(globalModels))
	seenModels := make(map[string]struct{}, len(globalModels))
	for _, setting := range globalModels {
		modelID := strings.TrimSpace(setting.ModelID)
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		if _, ok := seenModels[modelID]; ok {
			continue
		}
		if !isRealProbeModelSupported(ch, modelID) {
			continue
		}
		seenModels[modelID] = struct{}{}
		modelIDs = append(modelIDs, modelID)
	}

	models := make([]*ChannelHealthProbeModelOverview, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		globalSetting := ActiveHealthProbeModelSetting{ModelID: modelID}
		for _, setting := range globalModels {
			if strings.TrimSpace(setting.ModelID) == modelID {
				globalSetting = setting
				break
			}
		}
		enabledModel := globalSetting.Enabled
		stream := globalSetting.Stream
		latestRun := latestRuns[channelHealthProbeModelKey(ch.ID, modelID)]
		metric := metrics[channelHealthProbeModelKey(ch.ID, modelID)]
		var firstTokenMs *float64
		if value, ok := channelHealthProbeFirstTokenMs(latestRun); ok {
			firstTokenMs = &value
		}
		var lastProbedAt *time.Time
		if latestRun != nil {
			value := latestRun.StartedAt
			if latestRun.CompletedAt != nil {
				value = *latestRun.CompletedAt
			}
			lastProbedAt = &value
		}
		models = append(models, &ChannelHealthProbeModelOverview{
			ModelID:      modelID,
			Enabled:      enabledModel,
			Stream:       stream,
			FirstTokenMs: firstTokenMs,
			P95Ms:        metric.P95Ms,
			LastProbedAt: lastProbedAt,
			SampleCount:  metric.SampleCount,
			LatestRun:    latestRun,
		})
	}

	primaryModelID := primaryRealProbeModelID(ch, globalModels)
	if primaryModelID != nil {
		primaryRuns := make([]*ChannelHealthProbeRunRecord, 0, len(recentRuns))
		for _, run := range recentRuns {
			if run != nil && run.ModelID == *primaryModelID {
				primaryRuns = append(primaryRuns, run)
			}
		}
		recentRuns = primaryRuns
	} else {
		recentRuns = []*ChannelHealthProbeRunRecord{}
	}

	if recentRuns == nil {
		recentRuns = []*ChannelHealthProbeRunRecord{}
	}
	return &ChannelHealthProbeChannelOverview{
		ChannelID:            objects.GUID{Type: "Channel", ID: ch.ID},
		ChannelName:          ch.Name,
		ChannelStatus:        ch.Status.String(),
		Priority:             ch.Priority,
		Enabled:              enabled,
		IntervalMinutes:      interval,
		ModelPriceMultiplier: ch.ModelPriceMultiplier,
		PrimaryModelID:       primaryModelID,
		RecentRuns:           recentRuns,
		Models:               models,
	}
}

func (svc *ChannelHealthProbeService) History(
	ctx context.Context,
	input ChannelHealthProbeHistoryInput,
) (*ChannelHealthProbeHistoryPage, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	if input.Offset < 0 {
		return nil, fmt.Errorf("history offset must not be negative")
	}
	if input.Limit == 0 {
		input.Limit = DefaultChannelHealthProbeHistoryLimit
	}
	if input.Limit < 1 || input.Limit > MaxChannelHealthProbeHistoryLimit {
		return nil, fmt.Errorf("history limit must be between 1 and %d", MaxChannelHealthProbeHistoryLimit)
	}

	predicates := make([]predicate.ChannelHealthProbeRun, 0, 4)
	if input.ChannelID != nil {
		predicates = append(predicates, channelhealthproberun.ChannelIDEQ(input.ChannelID.ID))
	}
	if input.ModelID != nil && strings.TrimSpace(*input.ModelID) != "" {
		predicates = append(predicates, channelhealthproberun.ModelIDEQ(strings.TrimSpace(*input.ModelID)))
	}
	if input.Status != nil && strings.TrimSpace(*input.Status) != "" {
		status := channelhealthproberun.Status(strings.TrimSpace(*input.Status))
		if err := channelhealthproberun.StatusValidator(status); err != nil {
			return nil, err
		}
		predicates = append(predicates, channelhealthproberun.StatusEQ(status))
	}
	if input.Source != nil && strings.TrimSpace(*input.Source) != "" {
		source := channelhealthproberun.Source(strings.TrimSpace(*input.Source))
		if err := channelhealthproberun.SourceValidator(source); err != nil {
			return nil, err
		}
		predicates = append(predicates, channelhealthproberun.SourceEQ(source))
	}

	query := svc.entFromContext(ctx).ChannelHealthProbeRun.Query().Where(predicates...)
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to count channel health probe history: %w", err)
	}
	runs, err := query.
		Order(ent.Desc(channelhealthproberun.FieldCreatedAt), ent.Desc(channelhealthproberun.FieldID)).
		Offset(input.Offset).
		Limit(input.Limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query channel health probe history: %w", err)
	}

	items := make([]*ChannelHealthProbeRunRecord, 0, len(runs))
	for _, run := range runs {
		items = append(items, channelHealthProbeRunRecord(run))
	}
	return &ChannelHealthProbeHistoryPage{Items: items, TotalCount: total}, nil
}

func (svc *ChannelHealthProbeService) DueTargets(
	ctx context.Context,
	now time.Time,
) ([]ChannelHealthProbeTarget, error) {
	policy, err := svc.ScanPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return svc.DueTargetsWithPolicy(ctx, now, policy)
}

func (svc *ChannelHealthProbeService) DueTargetsWithPolicy(
	ctx context.Context,
	now time.Time,
	policy ActiveHealthProbeScanSetting,
) ([]ChannelHealthProbeTarget, error) {
	now = now.UTC()
	if !policy.Enabled {
		return []ChannelHealthProbeTarget{}, nil
	}
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusEQ(channel.StatusEnabled)).
		Order(
			ent.Desc(channel.FieldPriority),
			ent.Desc(channel.FieldOrderingWeight),
			ent.Asc(channel.FieldID),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query channels for scheduled health probes: %w", err)
	}

	channelIDs := make([]int, 0, len(channels))
	candidates := make([]ChannelHealthProbeTarget, 0)
	maxInterval := time.Duration(0)
	globalModels := activeHealthProbeModelMap(policy.Models)
	if len(globalModels) == 0 {
		return []ChannelHealthProbeTarget{}, nil
	}
	for _, ch := range channels {
		settings := objects.ChannelHealthProbeSettings{IntervalMinutes: objects.DefaultChannelHealthProbeIntervalMinutes}
		if ch.Settings != nil && ch.Settings.HealthProbe != nil {
			settings = *ch.Settings.HealthProbe
			settings.Normalize()
		}
		if settings.IntervalMinutes < MinChannelHealthProbeIntervalMinutes ||
			settings.IntervalMinutes > MaxChannelHealthProbeIntervalMinutes {
			continue
		}

		interval := time.Duration(settings.IntervalMinutes) * time.Minute
		bucket := now.Truncate(interval)
		channelIncluded := false
		models := make([]ActiveHealthProbeModelSetting, 0, len(globalModels))
		for _, model := range globalModels {
			if model.Enabled {
				models = append(models, model)
			}
		}
		slices.SortFunc(models, func(left, right ActiveHealthProbeModelSetting) int {
			return strings.Compare(left.ModelID, right.ModelID)
		})
		for _, model := range models {
			modelID := strings.TrimSpace(model.ModelID)
			if !isRealProbeModelSupported(ch, modelID) {
				continue
			}
			candidates = append(candidates, ChannelHealthProbeTarget{
				ChannelID:       ch.ID,
				ModelID:         modelID,
				Stream:          model.Stream,
				Priority:        ch.Priority,
				OrderingWeight:  ch.OrderingWeight,
				IntervalMinutes: settings.IntervalMinutes,
				ScheduleKey: fmt.Sprintf(
					"%d:%s:%t:%d",
					ch.ID,
					modelID,
					model.Stream,
					bucket.Unix(),
				),
			})
			channelIncluded = true
		}
		if channelIncluded {
			channelIDs = append(channelIDs, ch.ID)
			if interval > maxInterval {
				maxInterval = interval
			}
		}
	}
	if len(candidates) == 0 {
		return []ChannelHealthProbeTarget{}, nil
	}

	since := now.Add(-maxInterval)
	latestProbeRuns, err := svc.latestRunsByChannelAndModelSince(ctx, channelIDs, since, true)
	if err != nil {
		return nil, err
	}
	latestRealRequests, err := svc.latestRealRequestActivityByChannelAndModel(ctx, channelIDs, since)
	if err != nil {
		return nil, err
	}

	targets := make([]ChannelHealthProbeTarget, 0, len(candidates))
	for _, target := range candidates {
		modelKey := channelHealthProbeModelKey(target.ChannelID, target.ModelID)
		latestActivityAt := latestRealRequests[modelKey]
		if latestRun := latestProbeRuns[modelKey]; latestRun != nil && latestRun.StartedAt.After(latestActivityAt) {
			latestActivityAt = latestRun.StartedAt
		}
		interval := time.Duration(target.IntervalMinutes) * time.Minute
		if !latestActivityAt.IsZero() && now.Before(latestActivityAt.Add(interval)) {
			continue
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func (svc *ChannelHealthProbeService) SkipScheduledTargets(
	ctx context.Context,
	targets []ChannelHealthProbeTarget,
	skippedAt time.Time,
) error {
	for _, target := range targets {
		_, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Create().
			SetChannelID(target.ChannelID).
			SetModelID(target.ModelID).
			SetSource(channelhealthproberun.SourceScheduled).
			SetStatus(channelhealthproberun.StatusSkipped).
			SetStream(target.Stream).
			SetScheduleKey(target.ScheduleKey).
			SetStartedAt(skippedAt).
			SetCompletedAt(skippedAt).
			SetTotalMs(0).
			Save(ctx)
		if err != nil && !ent.IsConstraintError(err) {
			return fmt.Errorf("failed to persist skipped scheduled channel health probe: %w", err)
		}
	}

	return nil
}

func (svc *ChannelHealthProbeService) ClaimScheduledRun(
	ctx context.Context,
	target ChannelHealthProbeTarget,
	startedAt time.Time,
) (*ent.ChannelHealthProbeRun, bool, error) {
	run, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Create().
		SetChannelID(target.ChannelID).
		SetModelID(target.ModelID).
		SetSource(channelhealthproberun.SourceScheduled).
		SetStatus(channelhealthproberun.StatusPending).
		SetStream(target.Stream).
		SetScheduleKey(target.ScheduleKey).
		SetStartedAt(startedAt).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to claim scheduled channel health probe: %w", err)
	}
	return run, true, nil
}

func (svc *ChannelHealthProbeService) CreateManualRun(
	ctx context.Context,
	input RunChannelHealthProbeInput,
	startedAt time.Time,
) (*ent.ChannelHealthProbeRun, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	modelID := strings.TrimSpace(input.ModelID)
	if modelID == "" {
		return nil, fmt.Errorf("health probe model ID must not be empty")
	}
	ch, err := svc.channelService.GetChannel(ctx, input.ChannelID.ID)
	if err != nil {
		return nil, err
	}
	if !isRealProbeModelSupported(ch.Channel, modelID) {
		return nil, fmt.Errorf("model %q is not supported by this channel", modelID)
	}

	run, err := svc.entFromContext(ctx).ChannelHealthProbeRun.Create().
		SetChannelID(input.ChannelID.ID).
		SetModelID(modelID).
		SetSource(channelhealthproberun.SourceManual).
		SetStatus(channelhealthproberun.StatusPending).
		SetStream(input.Stream).
		SetStartedAt(startedAt).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create manual channel health probe: %w", err)
	}
	return run, nil
}

func (svc *ChannelHealthProbeService) CompleteRun(
	ctx context.Context,
	runID int,
	healthy bool,
	ttfbMs *float64,
	ttftMs *float64,
	totalMs float64,
	errorMessage *string,
	completedAt time.Time,
) (*ChannelHealthProbeRunRecord, error) {
	status := channelhealthproberun.StatusUnhealthy
	if healthy {
		status = channelhealthproberun.StatusHealthy
		errorMessage = nil
	}

	run, err := svc.entFromContext(ctx).ChannelHealthProbeRun.UpdateOneID(runID).
		Where(channelhealthproberun.StatusEQ(channelhealthproberun.StatusPending)).
		SetStatus(status).
		SetNillableTtfbMs(ttfbMs).
		SetNillableTtftMs(ttftMs).
		SetTotalMs(totalMs).
		SetNillableErrorMessage(errorMessage).
		SetCompletedAt(completedAt).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to complete channel health probe: %w", err)
	}
	svc.invalidateMetricsCache()
	return channelHealthProbeRunRecord(run), nil
}

func channelHealthProbeRunRecord(run *ent.ChannelHealthProbeRun) *ChannelHealthProbeRunRecord {
	if run == nil {
		return nil
	}
	return &ChannelHealthProbeRunRecord{
		ID:           objects.GUID{Type: "ChannelHealthProbeRun", ID: run.ID},
		ChannelID:    objects.GUID{Type: "Channel", ID: run.ChannelID},
		ModelID:      run.ModelID,
		Source:       run.Source.String(),
		Status:       run.Status.String(),
		Stream:       run.Stream,
		TTFBMs:       run.TtfbMs,
		TTFTMs:       run.TtftMs,
		TotalMs:      run.TotalMs,
		ErrorMessage: run.ErrorMessage,
		StartedAt:    run.StartedAt,
		CompletedAt:  run.CompletedAt,
		CreatedAt:    run.CreatedAt,
	}
}

func channelHealthProbeModelKey(channelID int, modelID string) string {
	return fmt.Sprintf("%d\x00%s", channelID, modelID)
}
