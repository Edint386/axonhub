package orchestrator

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// TestChatCompletionOrchestrator_Process_SkipsChannelOverQuota guards the custom
// `QuotaService: processor.QuotaService` line in Process's PersistenceState
// literal. Channel quota filtering reads the service from that state, and a nil
// service silently keeps every channel, so dropping the line in an upstream merge
// would still compile and pass the selector-level tests.
func TestChatCompletionOrchestrator_Process_SkipsChannelOverQuota(t *testing.T) {
	ctx := context.Background()
	ctx = authz.WithTestBypass(ctx)

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
	quotaService := biz.NewQuotaService(client, systemService)

	ch, err := createTestChannel(t, ctx, client).Update().
		SetSettings(&objects.ChannelSettings{
			Quota: &objects.APIKeyQuota{
				Requests: lo.ToPtr(int64(1)),
				Period: objects.APIKeyQuotaPeriod{
					Type: objects.APIKeyQuotaPeriodTypeAllTime,
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// One earlier request already used the channel's whole quota.
	usedAt := time.Now().UTC().Add(-time.Minute)
	req, err := client.Request.Create().
		SetProjectID(project.ID).
		SetAPIKeyID(1).
		SetModelID("gpt-4").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		SetCreatedAt(usedAt).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.UsageLog.Create().
		SetRequestID(req.ID).
		SetAPIKeyID(1).
		SetProjectID(project.ID).
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetCreatedAt(usedAt).
		Save(ctx)
	require.NoError(t, err)

	check, err := quotaService.CheckChannelQuota(ctx, ch.ID, ch.Settings.Quota, ch.Settings.QuotaTimezone)
	require.NoError(t, err)
	require.False(t, check.Allowed)

	outbound, err := openai.NewOutboundTransformer(ch.BaseURL, ch.Credentials.APIKey)
	require.NoError(t, err)

	// A working upstream, so a missing quota filter shows up as a served request.
	executor := &mockExecutor{
		response: &httpclient.Response{
			StatusCode: http.StatusOK,
			Body:       buildMockOpenAIResponse("chatcmpl-channel-quota", "gpt-4", "ok", 10, 20),
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
		},
	}
	orchestrator := &ChatCompletionOrchestrator{
		channelSelector: &staticChannelSelector{
			candidates: channelsToTestCandidates([]*biz.Channel{{Channel: ch, Outbound: outbound}}, "gpt-4"),
		},
		Inbound:               openai.NewInboundTransformer(),
		RequestService:        requestService,
		ChannelService:        channelService,
		PromptProvider:        &stubPromptProvider{},
		SystemService:         systemService,
		UsageLogService:       usageLogService,
		QuotaService:          quotaService,
		PipelineFactory:       pipeline.NewFactory(executor),
		ModelMapper:           NewModelMapper(),
		channelLimiterManager: NewChannelLimiterManager(),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
	}

	_, err = orchestrator.Process(ctx, buildTestRequest("gpt-4", "Hello!", false))
	require.Zero(t, executor.requestCalls.Load(), "a channel over its quota must not be called")
	require.Error(t, err)
	require.Contains(t, err.Error(), "quota exhausted")
}
