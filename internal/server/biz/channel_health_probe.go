package biz

import (
	"context"
	"fmt"
	"slices"
	"strings"
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
)

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
	Enabled         bool
	IntervalMinutes int
	Models          []objects.ChannelHealthProbeModel
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
}

type ChannelHealthProbePolicy struct {
	Enabled                      bool
	AcceptableLatencyMs          int
	ExtraChannels                int
	APIKeyMaxFirstTokenLatencyMs *float64
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
	ModelID   string
	Enabled   bool
	Stream    bool
	LatestRun *ChannelHealthProbeRunRecord
}

type ChannelHealthProbeChannelOverview struct {
	ChannelID       objects.GUID
	ChannelName     string
	ChannelStatus   string
	Priority        int
	Enabled         bool
	IntervalMinutes int
	Models          []*ChannelHealthProbeModelOverview
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
	overview := &ChannelHealthProbePolicy{
		Enabled:             policy.Enabled,
		AcceptableLatencyMs: policy.AcceptableLatencyMs,
		ExtraChannels:       policy.ExtraChannels,
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

	policy := ActiveHealthProbeScanSetting{
		Enabled:             input.Enabled,
		AcceptableLatencyMs: input.AcceptableLatencyMs,
		ExtraChannels:       input.ExtraChannels,
	}
	err := authz.RunWithSystemBypassVoid(ctx, "update-active-channel-health-probe-policy", func(bypassCtx context.Context) error {
		return svc.systemService.UpdateChannelSetting(bypassCtx, UpdateSystemChannelSettings{ActiveHealthProbeScan: &policy})
	})
	if err != nil {
		return nil, err
	}

	return svc.Policy(ctx)
}

func normalizeAndValidateChannelHealthProbeSettings(
	settings *objects.ChannelHealthProbeSettings,
	isModelSupported func(string) bool,
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

	models := make([]objects.ChannelHealthProbeModel, 0, len(settings.Models))
	seen := make(map[string]struct{}, len(settings.Models))
	hasEnabledModel := false
	for _, model := range settings.Models {
		model.ModelID = strings.TrimSpace(model.ModelID)
		if model.ModelID == "" {
			return fmt.Errorf("health probe model ID must not be empty")
		}
		if _, ok := seen[model.ModelID]; ok {
			return fmt.Errorf("health probe model %q is configured more than once", model.ModelID)
		}
		if isModelSupported != nil && !isModelSupported(model.ModelID) {
			return fmt.Errorf("model %q is not supported by this channel", model.ModelID)
		}

		seen[model.ModelID] = struct{}{}
		hasEnabledModel = hasEnabledModel || model.Enabled
		models = append(models, model)
	}
	settings.Models = models

	if settings.Enabled && !hasEnabledModel {
		return fmt.Errorf("enable at least one model before enabling health probes")
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
	probeSettings := &objects.ChannelHealthProbeSettings{
		Enabled:         input.Enabled,
		IntervalMinutes: input.IntervalMinutes,
		Models:          slices.Clone(input.Models),
	}

	runtimeChannel, err := svc.channelService.GetChannel(ctx, input.ChannelID.ID)
	if err != nil {
		return nil, err
	}
	if err := normalizeAndValidateChannelHealthProbeSettings(probeSettings, runtimeChannel.IsModelSupported); err != nil {
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

	result := make([]*ChannelHealthProbeChannelOverview, 0, len(channels))
	for _, ch := range channels {
		result = append(result, buildChannelHealthProbeOverview(ch, latestRuns))
	}
	return result, nil
}

func (svc *ChannelHealthProbeService) overviewForChannel(
	ctx context.Context,
	ch *ent.Channel,
) (*ChannelHealthProbeChannelOverview, error) {
	latestRuns, err := svc.latestRunsByChannelAndModel(ctx, []int{ch.ID})
	if err != nil {
		return nil, err
	}
	return buildChannelHealthProbeOverview(ch, latestRuns), nil
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

func buildChannelHealthProbeOverview(
	ch *ent.Channel,
	latestRuns map[string]*ChannelHealthProbeRunRecord,
) *ChannelHealthProbeChannelOverview {
	configured := map[string]objects.ChannelHealthProbeModel{}
	enabled := false
	interval := objects.DefaultChannelHealthProbeIntervalMinutes
	if ch.Settings != nil && ch.Settings.HealthProbe != nil {
		settings := *ch.Settings.HealthProbe
		settings.Normalize()
		enabled = settings.Enabled
		interval = settings.IntervalMinutes
		for _, model := range settings.Models {
			configured[model.ModelID] = model
		}
	}

	modelIDs := make([]string, 0, len(ch.SupportedModels)+len(configured)+1)
	seen := make(map[string]struct{}, len(ch.SupportedModels)+len(configured)+1)
	appendModel := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		if _, ok := seen[modelID]; ok {
			return
		}
		seen[modelID] = struct{}{}
		modelIDs = append(modelIDs, modelID)
	}
	appendModel(ch.DefaultTestModel)
	for _, modelID := range ch.SupportedModels {
		appendModel(modelID)
	}
	for modelID := range configured {
		appendModel(modelID)
	}

	models := make([]*ChannelHealthProbeModelOverview, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		setting := configured[modelID]
		models = append(models, &ChannelHealthProbeModelOverview{
			ModelID:   modelID,
			Enabled:   setting.Enabled,
			Stream:    setting.Stream,
			LatestRun: latestRuns[channelHealthProbeModelKey(ch.ID, modelID)],
		})
	}

	return &ChannelHealthProbeChannelOverview{
		ChannelID:       objects.GUID{Type: "Channel", ID: ch.ID},
		ChannelName:     ch.Name,
		ChannelStatus:   ch.Status.String(),
		Priority:        ch.Priority,
		Enabled:         enabled,
		IntervalMinutes: interval,
		Models:          models,
	}
}

func (svc *ChannelHealthProbeService) History(
	ctx context.Context,
	input ChannelHealthProbeHistoryInput,
) (*ChannelHealthProbeHistoryPage, error) {
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
	now = now.UTC()
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
	for _, ch := range channels {
		if ch.Settings == nil || ch.Settings.HealthProbe == nil {
			continue
		}
		settings := *ch.Settings.HealthProbe
		settings.Normalize()
		if !settings.Enabled || settings.IntervalMinutes < MinChannelHealthProbeIntervalMinutes ||
			settings.IntervalMinutes > MaxChannelHealthProbeIntervalMinutes {
			continue
		}

		interval := time.Duration(settings.IntervalMinutes) * time.Minute
		bucket := now.Truncate(interval)
		channelIncluded := false
		for _, model := range settings.Models {
			if !model.Enabled || strings.TrimSpace(model.ModelID) == "" {
				continue
			}
			modelID := strings.TrimSpace(model.ModelID)
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
	modelID := strings.TrimSpace(input.ModelID)
	if modelID == "" {
		return nil, fmt.Errorf("health probe model ID must not be empty")
	}
	ch, err := svc.channelService.GetChannel(ctx, input.ChannelID.ID)
	if err != nil {
		return nil, err
	}
	if !ch.IsModelSupported(modelID) {
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
