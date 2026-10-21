//nolint:exhaustruct_v5 // Test fixtures intentionally set only fields relevant to each scenario.
package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const realtimeTestAccountID = "acct_realtime_orchestrator"

// realtimeTestJWT builds a JWT carrying the ChatGPT account id claim that
// codex.ExtractChatGPTAccountIDFromJWT reads, so the Codex channel resolves an OAuth credential.
func realtimeTestJWT(t *testing.T) string {
	t.Helper()

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": realtimeTestAccountID},
		"exp":                         time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("test-secret"))
	require.NoError(t, err)

	return token
}

// realtimeTestFixture holds the channels and the orchestrator shared by the selection tests.
type realtimeTestFixture struct {
	ctx          context.Context
	client       *ent.Client
	orchestrator *ChatCompletionOrchestrator

	openaiPrimary   *ent.Channel
	openaiSecondary *ent.Channel
	anthropic       *ent.Channel
	codexOAuth      *ent.Channel
}

// newRealtimeTestFixture seeds the in-memory database with two OpenAI channels, one Anthropic
// channel (all serving gpt-realtime) and one Codex OAuth channel (serving gpt-live-1-codex), then
// builds a real orchestrator on top of them.
func newRealtimeTestFixture(t *testing.T) *realtimeTestFixture {
	t.Helper()

	ctx, client := setupTest(t)

	openaiPrimary := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("openai-realtime-primary").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-openai-primary"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SetOrderingWeight(100).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	openaiSecondary := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("openai-realtime-secondary").
		SetBaseURL("https://relay.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-openai-secondary"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SetOrderingWeight(50).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	anthropic := client.Channel.Create().
		SetType(channel.TypeAnthropic).
		SetName("anthropic-no-realtime").
		SetBaseURL("https://api.anthropic.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-ant"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SetOrderingWeight(200).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	codexOAuth := client.Channel.Create().
		SetType(channel.TypeCodex).
		SetName("codex-oauth").
		SetCredentials(objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
			AccessToken:  realtimeTestJWT(t),
			RefreshToken: "refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}}).
		SetSupportedModels([]string{"gpt-live-1-codex"}).
		SetDefaultTestModel("gpt-live-1-codex").
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	return &realtimeTestFixture{
		ctx:             ctx,
		client:          client,
		orchestrator:    newRealtimeTestOrchestrator(t, client),
		openaiPrimary:   openaiPrimary,
		openaiSecondary: openaiSecondary,
		anthropic:       anthropic,
		codexOAuth:      codexOAuth,
	}
}

// newRealtimeTestOrchestrator builds an orchestrator through the production constructor so the
// load balancers, the RPM tracker and the limiter manager are the real ones. The channel service
// loads the enabled channels from the database at construction time.
func newRealtimeTestOrchestrator(t *testing.T, client *ent.Client) *ChatCompletionOrchestrator {
	t.Helper()

	systemService := newTestSystemService(client)
	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           client,
		SystemService: systemService,
		HttpClient:    httpclient.NewHttpClient(),
	})
	t.Cleanup(channelService.Stop)

	requestService := newTestRequestServiceForChannels(client, systemService)
	usageLogService := biz.NewUsageLogService(client, systemService, channelService)
	selector := NewDefaultSelector(channelService, newTestModelService(client), systemService)

	return NewChatCompletionOrchestrator(
		channelService,
		selector,
		requestService,
		httpclient.NewHttpClient(),
		openai.NewInboundTransformer(),
		systemService,
		usageLogService,
		nil,
		nil,
		nil,
		biz.NewLiveStreamRegistry(),
		NewChannelLimiterManager(),
		nil,
	)
}

func realtimeCandidateChannelIDs(candidates []*ChannelModelsCandidate) []int {
	return lo.Map(candidates, func(candidate *ChannelModelsCandidate, _ int) int {
		return candidate.Channel.ID
	})
}

func realtimeTestAPIKey(profile objects.APIKeyProfile) *ent.APIKey {
	profile.Name = "realtime"

	return &ent.APIKey{
		ID:   1,
		Name: "realtime-key",
		Profiles: &objects.APIKeyProfiles{
			ActiveProfile: "realtime",
			Profiles:      []objects.APIKeyProfile{profile},
		},
	}
}

func TestPrepareRealtimeContext(t *testing.T) {
	ctx := PrepareRealtimeContext(t.Context())

	require.NoError(t, authz.RequireSystemPrincipal(ctx), "the realtime context carries the system bypass principal")

	// The container is installed, so providers that cannot return a derived context can still
	// record values visible through the original context.
	contexts.WithChannelAPIKey(ctx, "sk-recorded")

	recorded, ok := contexts.GetChannelAPIKey(ctx)
	require.True(t, ok)
	require.Equal(t, "sk-recorded", recorded)
}

func TestSelectRealtimeCandidates_WebSocketKeepsOnlyRealtimeCapableChannels(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-realtime", selection.RequestedModel)

	ids := realtimeCandidateChannelIDs(selection.Candidates)
	require.Len(t, ids, 2, "both openai channels survive the retry-budget truncation")
	require.ElementsMatch(t, []int{f.openaiPrimary.ID, f.openaiSecondary.ID}, ids)
	require.NotContains(t, ids, f.anthropic.ID, "anthropic channels have no realtime endpoint")
	require.NotContains(t, ids, f.codexOAuth.ID, "the codex channel does not serve gpt-realtime")

	for _, candidate := range selection.Candidates {
		require.Equal(t, "gpt-realtime", RealtimeActualModel(candidate, selection.RequestedModel))
	}
}

func TestSelectRealtimeCandidates_WebSocketWithHeadersAndTargetMetadata(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)

	headers := http.Header{}
	headers.Set("User-Agent", "realtime-test")

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, headers)
	require.NoError(t, err)
	require.NotEmpty(t, selection.Candidates)

	for _, candidate := range selection.Candidates {
		target, err := candidate.Channel.RealtimeTarget(biz.RealtimeModeWebSocket)
		require.NoError(t, err)
		require.Equal(t, biz.RealtimeDialectGA, target.Dialect)
		require.NotEmpty(t, target.WSURL)
	}
}

func TestSelectRealtimeCandidates_CodexOAuthOnlyServesWebRTC(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)

	t.Run("webrtc returns the codex oauth channel", func(t *testing.T) {
		selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-live-1-codex", biz.RealtimeModeWebRTC, nil)
		require.NoError(t, err)
		require.Equal(t, "gpt-live-1-codex", selection.RequestedModel)
		require.Equal(t, []int{f.codexOAuth.ID}, realtimeCandidateChannelIDs(selection.Candidates))

		target, err := selection.Candidates[0].Channel.RealtimeTarget(biz.RealtimeModeWebRTC)
		require.NoError(t, err)
		require.True(t, target.Codex)

		cred, err := selection.Candidates[0].Channel.RealtimeCredential(ctx)
		require.NoError(t, err)
		require.True(t, cred.OAuth)
		require.Equal(t, realtimeTestAccountID, cred.AccountID)
	})

	t.Run("websocket rejects the codex oauth channel", func(t *testing.T) {
		selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-live-1-codex", biz.RealtimeModeWebSocket, nil)
		require.Error(t, err)
		require.ErrorIs(t, err, biz.ErrInvalidModel)
		require.Nil(t, selection)
	})
}

func TestSelectRealtimeCandidates_UnknownModelIsInvalidModel(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)

	_, err := f.orchestrator.SelectRealtimeCandidates(ctx, "no-such-model", biz.RealtimeModeWebSocket, nil)
	require.ErrorIs(t, err, biz.ErrInvalidModel)

	_, err = f.orchestrator.SelectRealtimeCandidates(ctx, "", biz.RealtimeModeWebSocket, nil)
	require.ErrorIs(t, err, biz.ErrInvalidModel, "an empty model is rejected before selection")
}

func TestSelectRealtimeCandidates_ProfileModelIDsDenial(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)
	ctx = contexts.WithAPIKey(ctx, realtimeTestAPIKey(objects.APIKeyProfile{
		ModelIDs: []string{"gpt-4o"},
	}))

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, biz.ErrInvalidModel)
	require.Nil(t, selection)
}

func TestSelectRealtimeCandidates_ProfileModelIDsAllow(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)
	ctx = contexts.WithAPIKey(ctx, realtimeTestAPIKey(objects.APIKeyProfile{
		ModelIDs: []string{"gpt-realtime"},
	}))

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-realtime", selection.RequestedModel)
	require.ElementsMatch(t, []int{f.openaiPrimary.ID, f.openaiSecondary.ID}, realtimeCandidateChannelIDs(selection.Candidates))
}

func TestSelectRealtimeCandidates_ProfileModelMappingRewritesRequestedModel(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)
	ctx = contexts.WithAPIKey(ctx, realtimeTestAPIKey(objects.APIKeyProfile{
		ModelMappings: []objects.ModelMapping{{From: "voice-fast", To: "gpt-realtime"}},
	}))

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "voice-fast", biz.RealtimeModeWebSocket, nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-realtime", selection.RequestedModel, "the mapped name is the one echoed to clients")
	require.ElementsMatch(t, []int{f.openaiPrimary.ID, f.openaiSecondary.ID}, realtimeCandidateChannelIDs(selection.Candidates))

	for _, candidate := range selection.Candidates {
		require.Equal(t, "gpt-realtime", RealtimeActualModel(candidate, selection.RequestedModel))
	}
}

func TestSelectRealtimeCandidates_ProfileChannelIDsNarrowSelection(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)
	ctx = contexts.WithAPIKey(ctx, realtimeTestAPIKey(objects.APIKeyProfile{
		ChannelIDs: []int{f.openaiSecondary.ID, f.anthropic.ID},
	}))

	selection, err := f.orchestrator.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, nil)
	require.NoError(t, err)
	require.Equal(t, []int{f.openaiSecondary.ID}, realtimeCandidateChannelIDs(selection.Candidates),
		"profile channel filters still apply on top of the realtime capability filter")
}

func TestSelectRealtimeCandidates_ChannelModelMappingResolvesActualModel(t *testing.T) {
	ctx, client := setupTest(t)

	aliasChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("openai-alias").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-alias"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SetSettings(&objects.ChannelSettings{
			ModelMappings: []objects.ModelMapping{{From: "voice-alias", To: "gpt-realtime"}},
		}).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	orchestrator := newRealtimeTestOrchestrator(t, client)
	ctx = PrepareRealtimeContext(ctx)

	selection, err := orchestrator.SelectRealtimeCandidates(ctx, "voice-alias", biz.RealtimeModeWebSocket, nil)
	require.NoError(t, err)
	require.Equal(t, "voice-alias", selection.RequestedModel)
	require.Equal(t, []int{aliasChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates))
	require.Equal(t, "gpt-realtime", RealtimeActualModel(selection.Candidates[0], selection.RequestedModel))
}

func TestRealtimeActualModel(t *testing.T) {
	tests := []struct {
		name      string
		candidate *ChannelModelsCandidate
		requested string
		want      string
	}{
		{
			name:      "nil candidate falls back to the requested name",
			candidate: nil,
			requested: "gpt-realtime",
			want:      "gpt-realtime",
		},
		{
			name:      "no model entries falls back to the requested name",
			candidate: &ChannelModelsCandidate{},
			requested: "gpt-realtime",
			want:      "gpt-realtime",
		},
		{
			name: "matching request model wins",
			candidate: &ChannelModelsCandidate{Models: []biz.ChannelModelEntry{
				{RequestModel: "other", ActualModel: "gpt-4o-realtime-preview"},
				{RequestModel: "voice-alias", ActualModel: "gpt-realtime"},
			}},
			requested: "voice-alias",
			want:      "gpt-realtime",
		},
		{
			name: "no matching request model uses the first entry",
			candidate: &ChannelModelsCandidate{Models: []biz.ChannelModelEntry{
				{RequestModel: "a", ActualModel: "actual-a"},
				{RequestModel: "b", ActualModel: "actual-b"},
			}},
			requested: "c",
			want:      "actual-a",
		},
		{
			name: "matching entry with an empty actual model falls back to the first entry",
			candidate: &ChannelModelsCandidate{Models: []biz.ChannelModelEntry{
				{RequestModel: "other", ActualModel: "actual-other"},
				{RequestModel: "voice-alias", ActualModel: ""},
			}},
			requested: "voice-alias",
			want:      "actual-other",
		},
		{
			name: "all actual models empty falls back to the requested name",
			candidate: &ChannelModelsCandidate{Models: []biz.ChannelModelEntry{
				{RequestModel: "x", ActualModel: ""},
			}},
			requested: "voice-alias",
			want:      "voice-alias",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, RealtimeActualModel(tt.candidate, tt.requested))
		})
	}
}

func TestRealtimeRPMAdmit(t *testing.T) {
	ctx := t.Context()

	newChannel := func(id int, rpm *int64) *biz.Channel {
		return &biz.Channel{Channel: &ent.Channel{
			ID:   id,
			Name: "rpm-channel",
			Settings: &objects.ChannelSettings{
				RateLimit: &objects.ChannelRateLimit{RPM: rpm},
			},
		}}
	}

	t.Run("rejects the second call when the limit is one", func(t *testing.T) {
		processor := &ChatCompletionOrchestrator{rateLimitTracker: NewChannelRequestTracker()}
		ch := newChannel(1, lo.ToPtr(int64(1)))

		require.NoError(t, processor.RealtimeRPMAdmit(ctx, ch))

		err := processor.RealtimeRPMAdmit(ctx, ch)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrRealtimeRPMExceeded)
		require.ErrorIs(t, err, ErrLocalRPMExhausted, "the chat pipeline's sentinel is wrapped too")

		var rpmErr *LocalRPMExhaustedError
		require.ErrorAs(t, err, &rpmErr)
		require.Equal(t, 1, rpmErr.ChannelID)
		require.Equal(t, int64(1), rpmErr.Limit)

		var httpErr *httpclient.Error
		require.ErrorAs(t, err, &httpErr)
		require.Equal(t, http.StatusTooManyRequests, httpErr.StatusCode)

		require.Equal(t, int64(1), processor.rateLimitTracker.GetRequestCount(1), "a rejected call consumes no slot")
	})

	t.Run("channels are tracked independently", func(t *testing.T) {
		processor := &ChatCompletionOrchestrator{rateLimitTracker: NewChannelRequestTracker()}

		require.NoError(t, processor.RealtimeRPMAdmit(ctx, newChannel(1, lo.ToPtr(int64(1)))))
		require.NoError(t, processor.RealtimeRPMAdmit(ctx, newChannel(2, lo.ToPtr(int64(1)))))
		require.ErrorIs(t, processor.RealtimeRPMAdmit(ctx, newChannel(1, lo.ToPtr(int64(1)))), ErrRealtimeRPMExceeded)
	})

	t.Run("unlimited channels are always admitted", func(t *testing.T) {
		processor := &ChatCompletionOrchestrator{rateLimitTracker: NewChannelRequestTracker()}

		for range 3 {
			require.NoError(t, processor.RealtimeRPMAdmit(ctx, newChannel(1, nil)))
			require.NoError(t, processor.RealtimeRPMAdmit(ctx, newChannel(2, lo.ToPtr(int64(0)))))
			require.NoError(t, processor.RealtimeRPMAdmit(ctx, &biz.Channel{Channel: &ent.Channel{ID: 3, Name: "no-settings"}}))
		}

		require.Equal(t, int64(0), processor.rateLimitTracker.GetRequestCount(1), "unlimited channels consume no slot")
	})

	t.Run("nil tracker and nil channels never panic", func(t *testing.T) {
		processor := &ChatCompletionOrchestrator{}

		require.NotPanics(t, func() {
			require.NoError(t, processor.RealtimeRPMAdmit(ctx, newChannel(1, lo.ToPtr(int64(1)))))
		})

		tracked := &ChatCompletionOrchestrator{rateLimitTracker: NewChannelRequestTracker()}

		require.NotPanics(t, func() {
			require.NoError(t, tracked.RealtimeRPMAdmit(ctx, nil))
			require.NoError(t, tracked.RealtimeRPMAdmit(ctx, &biz.Channel{}))
		})
	})
}

func TestRealtimeLimiter(t *testing.T) {
	manager := NewChannelLimiterManager()
	processor := &ChatCompletionOrchestrator{channelLimiterManager: manager}

	require.Same(t, manager, processor.RealtimeLimiter())

	limited := &biz.Channel{Channel: &ent.Channel{
		ID:   7,
		Name: "limited",
		Settings: &objects.ChannelSettings{
			RateLimit: &objects.ChannelRateLimit{MaxConcurrent: lo.ToPtr(int64(2))},
		},
	}}
	require.NotNil(t, processor.RealtimeLimiter().GetOrCreate(limited))

	unlimited := &biz.Channel{Channel: &ent.Channel{ID: 8, Name: "unlimited"}}
	require.Nil(t, processor.RealtimeLimiter().GetOrCreate(unlimited), "channels without a concurrency limit need no admission")

	require.Nil(t, (&ChatCompletionOrchestrator{}).RealtimeLimiter())
}

func TestRealtimeCapableSelector(t *testing.T) {
	f := newRealtimeTestFixture(t)
	ctx := PrepareRealtimeContext(f.ctx)

	byID := make(map[int]*biz.Channel)
	for _, ch := range f.orchestrator.ChannelService.GetEnabledChannels() {
		byID[ch.ID] = ch
	}

	require.Len(t, byID, 4)

	candidateFor := func(id int) *ChannelModelsCandidate {
		ch, ok := byID[id]
		require.True(t, ok, "channel %d must be loaded by the channel service", id)

		return &ChannelModelsCandidate{Channel: ch, Models: []biz.ChannelModelEntry{{RequestModel: "m", ActualModel: "m"}}}
	}

	noKey := &biz.Channel{Channel: &ent.Channel{
		ID: 999, Name: "openai-no-key", Type: channel.TypeOpenai, BaseURL: "https://api.openai.com/v1",
	}}

	all := []*ChannelModelsCandidate{
		candidateFor(f.anthropic.ID),
		candidateFor(f.openaiPrimary.ID),
		candidateFor(f.codexOAuth.ID),
		nil,
		{Channel: nil},
		{Channel: &biz.Channel{}},
		{Channel: noKey},
		candidateFor(f.openaiSecondary.ID),
	}

	req := &llm.Request{Model: "m", RequestType: llm.RequestTypeRealtime, APIFormat: llm.APIFormatOpenAIRealtime}

	tests := []struct {
		name string
		mode biz.RealtimeMode
		want []int
	}{
		{
			name: "websocket keeps api key channels with a realtime target in order",
			mode: biz.RealtimeModeWebSocket,
			want: []int{f.openaiPrimary.ID, f.openaiSecondary.ID},
		},
		{
			name: "webrtc also keeps the codex oauth channel",
			mode: biz.RealtimeModeWebRTC,
			want: []int{f.openaiPrimary.ID, f.codexOAuth.ID, f.openaiSecondary.ID},
		},
		{
			name: "unknown mode drops everything",
			mode: biz.RealtimeMode("sip"),
			want: []int{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := withRealtimeCapableSelector(&staticChannelSelector{candidates: all}, tt.mode, "")

			got, err := selector.Select(ctx, req)
			require.NoError(t, err)
			require.Equal(t, tt.want, realtimeCandidateChannelIDs(got))
		})
	}

	t.Run("filtering resolves no credential", func(t *testing.T) {
		ctx := PrepareRealtimeContext(f.ctx)
		selector := withRealtimeCapableSelector(&staticChannelSelector{candidates: all}, biz.RealtimeModeWebSocket, "")

		_, err := selector.Select(ctx, req)
		require.NoError(t, err)

		_, recorded := contexts.GetChannelAPIKey(ctx)
		require.False(t, recorded, "the capability filter must not record a channel api key; the relay records the key it dials with")
	})

	t.Run("wrapped errors propagate unchanged", func(t *testing.T) {
		wantErr := errors.New("boom")
		selector := withRealtimeCapableSelector(&failingChannelSelector{err: wantErr}, biz.RealtimeModeWebSocket, "")

		_, err := selector.Select(ctx, req)
		require.ErrorIs(t, err, wantErr)
	})

	t.Run("empty input stays empty", func(t *testing.T) {
		selector := withRealtimeCapableSelector(&staticChannelSelector{}, biz.RealtimeModeWebSocket, "")

		got, err := selector.Select(ctx, req)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}

// newRealtimeBailianFixture seeds a Bailian channel (serving a Qwen-Omni realtime model and
// gpt-realtime) with a higher ordering weight than an OpenAI channel serving gpt-realtime, so a
// load balancer truncated to one candidate would keep the Bailian channel unless the capability
// filter drops it first.
func newRealtimeBailianFixture(t *testing.T) (context.Context, *ChatCompletionOrchestrator, *ent.Channel, *ent.Channel) {
	t.Helper()

	ctx, client := setupTest(t)

	bailianChannel := client.Channel.Create().
		SetType(channel.TypeBailian).
		SetName("bailian-realtime").
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-bailian"}).
		SetSupportedModels([]string{"qwen3-omni-flash-realtime", "gpt-realtime"}).
		SetDefaultTestModel("qwen3-omni-flash-realtime").
		SetOrderingWeight(100).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	openaiChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("openai-realtime").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-openai"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SetOrderingWeight(10).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	orchestrator := newRealtimeTestOrchestrator(t, client)

	// Retry disabled: the load balancer keeps a single candidate.
	policy := orchestrator.SystemService.RetryPolicyOrDefault(ctx)
	policy.Enabled = false
	require.NoError(t, orchestrator.SystemService.SetRetryPolicy(ctx, policy))

	return PrepareRealtimeContext(ctx), orchestrator, bailianChannel, openaiChannel
}

func TestSelectRealtime_BailianIntentAndModelFilter(t *testing.T) {
	ctx, orch, bailianChannel, openaiChannel := newRealtimeBailianFixture(t)

	t.Run("omni model on websocket keeps the bailian channel", func(t *testing.T) {
		selection, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3-omni-flash-realtime", Mode: biz.RealtimeModeWebSocket})
		require.NoError(t, err)
		require.Equal(t, []int{bailianChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates))
	})

	t.Run("transcription intent drops the bailian channel", func(t *testing.T) {
		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3-omni-flash-realtime", Mode: biz.RealtimeModeWebSocket, Intent: realtimeIntentTranscription})
		require.ErrorIs(t, err, biz.ErrInvalidModel)
	})

	t.Run("quicksilver intent drops the bailian channel", func(t *testing.T) {
		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3-omni-flash-realtime", Mode: biz.RealtimeModeWebSocket, Intent: realtimeIntentQuicksilver})
		require.ErrorIs(t, err, biz.ErrInvalidModel)
	})

	t.Run("non-omni model is dropped before truncation so the openai channel survives", func(t *testing.T) {
		selection, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "gpt-realtime", Mode: biz.RealtimeModeWebSocket})
		require.NoError(t, err)
		require.Equal(t, []int{openaiChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates),
			"the heavier bailian channel cannot serve gpt-realtime and must not push the openai channel out of the single-candidate list")
	})

	t.Run("webrtc never selects a bailian channel", func(t *testing.T) {
		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3-omni-flash-realtime", Mode: biz.RealtimeModeWebRTC})
		require.ErrorIs(t, err, biz.ErrInvalidModel)
	})

	t.Run("the wrapper is selection without intent", func(t *testing.T) {
		selection, err := orch.SelectRealtimeCandidates(ctx, "qwen3-omni-flash-realtime", biz.RealtimeModeWebSocket, nil)
		require.NoError(t, err)
		require.Equal(t, []int{bailianChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates))
	})

	t.Run("selection records no channel api key", func(t *testing.T) {
		ctx := PrepareRealtimeContext(ctx)

		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "gpt-realtime", Mode: biz.RealtimeModeWebSocket})
		require.NoError(t, err)

		_, recorded := contexts.GetChannelAPIKey(ctx)
		require.False(t, recorded)
	})
}

// failingChannelSelector always returns its error.
type failingChannelSelector struct {
	err error
}

func (s *failingChannelSelector) Select(context.Context, *llm.Request) ([]*ChannelModelsCandidate, error) {
	return nil, s.err
}
