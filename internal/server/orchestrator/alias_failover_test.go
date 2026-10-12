package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func aliasFailoverTransformer(enabled bool, models ...biz.ChannelModelEntry) *PersistentOutboundTransformer {
	return &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			AliasFailoverEnabled: enabled,
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: &biz.Channel{
					Channel:  &ent.Channel{ID: 1, Name: "groq"},
					Outbound: &mockTransformer{},
				},
				Models: models,
			},
			RequestExec: &ent.RequestExecution{ID: 1},
		},
	}
}

func generalAlias(actualModels ...string) []biz.ChannelModelEntry {
	entries := make([]biz.ChannelModelEntry, 0, len(actualModels))
	for _, actual := range actualModels {
		entries = append(entries, biz.ChannelModelEntry{RequestModel: "general", ActualModel: actual, Source: "mapping"})
	}

	return entries
}

func TestPersistentOutboundTransformer_HasNextAliasTarget(t *testing.T) {
	t.Run("next entry of the same alias", func(t *testing.T) {
		transformer := aliasFailoverTransformer(true, generalAlias("llama", "qwen")...)
		require.True(t, transformer.HasNextAliasTarget())
	})

	t.Run("retry policy disabled", func(t *testing.T) {
		transformer := aliasFailoverTransformer(false, generalAlias("llama", "qwen")...)
		require.False(t, transformer.HasNextAliasTarget())
	})

	t.Run("last alias target", func(t *testing.T) {
		transformer := aliasFailoverTransformer(true, generalAlias("llama", "qwen")...)
		transformer.state.CurrentModelIndex = 1
		require.False(t, transformer.HasNextAliasTarget())
	})

	t.Run("next entry belongs to another request model", func(t *testing.T) {
		models := append(generalAlias("llama"), biz.ChannelModelEntry{RequestModel: "fast", ActualModel: "gemma", Source: "mapping"})
		transformer := aliasFailoverTransformer(true, models...)
		require.False(t, transformer.HasNextAliasTarget(), "moving to another request model must keep spending the same-channel budget")
	})

	t.Run("no candidate", func(t *testing.T) {
		transformer := aliasFailoverTransformer(true)
		transformer.state.CurrentCandidate = nil
		require.False(t, transformer.HasNextAliasTarget())
	})
}

func TestIsAliasFailoverError(t *testing.T) {
	ch := &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "groq"}}
	chWithPatterns := &biz.Channel{Channel: &ent.Channel{
		ID:   2,
		Name: "groq-patterns",
		Settings: &objects.ChannelSettings{
			RetryableErrorPatterns: []objects.RetryableErrorPattern{{Pattern: "model_decommissioned"}},
		},
	}}

	cases := []struct {
		name string
		err  error
		ch   *biz.Channel
		want bool
	}{
		{name: "429 with Retry-After", err: &httpclient.Error{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": []string{"7"}}}, ch: ch, want: true},
		{name: "429 without Retry-After", err: &httpclient.Error{StatusCode: http.StatusTooManyRequests}, ch: ch, want: true},
		{name: "500", err: &httpclient.Error{StatusCode: http.StatusInternalServerError}, ch: ch, want: true},
		{name: "503", err: &httpclient.Error{StatusCode: http.StatusServiceUnavailable}, ch: ch, want: true},
		{name: "404 model not found", err: &httpclient.Error{StatusCode: http.StatusNotFound}, ch: ch, want: true},
		{name: "413 request too large for the model", err: &httpclient.Error{StatusCode: http.StatusRequestEntityTooLarge}, ch: ch, want: true},
		{name: "empty response", err: pipeline.ErrEmptyResponse, ch: ch, want: true},
		{name: "empty aggregated body", err: pipeline.ErrEmptyAggregatedBody, ch: ch, want: true},
		{name: "400", err: &httpclient.Error{StatusCode: http.StatusBadRequest}, ch: ch, want: false},
		{name: "401", err: &httpclient.Error{StatusCode: http.StatusUnauthorized}, ch: ch, want: false},
		{name: "403", err: &httpclient.Error{StatusCode: http.StatusForbidden}, ch: ch, want: false},
		{name: "400 matching a channel retry pattern", err: &llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Message: "the model has been decommissioned", Code: "model_decommissioned"}}, ch: chWithPatterns, want: true},
		{name: "400 response error without a pattern", err: &llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Message: "context length exceeded"}}, ch: chWithPatterns, want: false},
		{name: "429 response error", err: &llm.ResponseError{StatusCode: http.StatusTooManyRequests, Detail: llm.ErrorDetail{Message: "rate limit reached"}}, ch: ch, want: true},
		{name: "local queue rejection carries a synthetic 429", err: asChannelQueueError(ch, ErrChannelQueueFull), ch: ch, want: false},
		{name: "local RPM rejection carries a synthetic 429", err: newLocalRPMExhaustedError(ch, 10), ch: ch, want: false},
		{name: "circuit breaker skip", err: errSkipCandidateByCircuitBreaker, ch: ch, want: false},
		{name: "connection reset", err: syscall.ECONNRESET, ch: ch, want: true},
		{name: "dial timeout hits every target on the host", err: fmt.Errorf("failed to do request: %w", &url.Error{Op: "Post", URL: "https://api.groq.com", Err: os.ErrDeadlineExceeded}), ch: ch, want: false},
		{name: "nil", err: nil, ch: ch, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isAliasFailoverError(tc.err, tc.ch))
		})
	}
}

func TestPersistentOutboundTransformer_NextAliasTarget(t *testing.T) {
	ctx := context.Background()
	rateLimited := &httpclient.Error{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": []string{"30"}}}

	t.Run("switches to the next alias target", func(t *testing.T) {
		transformer := aliasFailoverTransformer(true, generalAlias("llama", "qwen", "gpt-oss")...)

		switched, abandon := transformer.NextAliasTarget(ctx, rateLimited)
		require.True(t, switched)
		require.False(t, abandon)
		require.Equal(t, 1, transformer.state.CurrentModelIndex)
		require.Nil(t, transformer.state.RequestExec, "the next target must get its own execution record")
		require.Equal(t, "qwen", transformer.GetCurrentModelID())
	})

	t.Run("keeps the budgeted path for errors every target shares", func(t *testing.T) {
		transformer := aliasFailoverTransformer(true, generalAlias("llama", "qwen")...)

		switched, abandon := transformer.NextAliasTarget(ctx, &httpclient.Error{StatusCode: http.StatusUnauthorized})
		require.False(t, switched)
		require.False(t, abandon)
		require.Zero(t, transformer.state.CurrentModelIndex)
		require.NotNil(t, transformer.state.RequestExec)
	})

	t.Run("does nothing when the retry policy is disabled", func(t *testing.T) {
		transformer := aliasFailoverTransformer(false, generalAlias("llama", "qwen")...)

		switched, abandon := transformer.NextAliasTarget(ctx, rateLimited)
		require.False(t, switched)
		require.False(t, abandon)
		require.Zero(t, transformer.state.CurrentModelIndex)
	})

	t.Run("abandons the channel when it left service", func(t *testing.T) {
		dbCtx, client := setupTest(t)
		channelService := newTestChannelServiceForChannels(client)

		disabled := createRetryGuardChannel(t, dbCtx, client, "alias-disabled",
			objects.ChannelCredentials{APIKeys: []string{"key-1"}})
		client.Channel.UpdateOneID(disabled.ID).SetStatus(channel.StatusDisabled).SaveX(dbCtx)

		snapshot := pinnedRetryGuardChannel(t, disabled.ID, []string{"key-1"})
		transformer := newRetryGuardTransformer(channelService, snapshot, "key-1")
		transformer.state.AliasFailoverEnabled = true
		transformer.state.CurrentCandidate.Models = generalAlias("llama", "qwen")

		switched, abandon := transformer.NextAliasTarget(dbCtx, rateLimited)
		require.False(t, switched)
		require.True(t, abandon)
		require.Zero(t, transformer.state.CurrentModelIndex)
	})
}

// newAliasFailoverOrchestrator wires an orchestrator whose only candidate is a
// channel serving the "general" alias through the given upstream models.
func newAliasFailoverOrchestrator(
	t *testing.T,
	policy *biz.RetryPolicy,
	steps []executorStep,
	actualModels ...string,
) (context.Context, *ent.Client, *ChatCompletionOrchestrator, *sequenceExecutor) {
	t.Helper()

	ctx := authz.WithTestBypass(context.Background())

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	ch := createTestChannel(t, ctx, client)
	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)

	require.NoError(t, systemService.SetRetryPolicy(ctx, policy))

	outbound, err := openai.NewOutboundTransformer(ch.BaseURL, ch.Credentials.APIKey)
	require.NoError(t, err)

	executor := &sequenceExecutor{steps: steps}
	orchestrator := &ChatCompletionOrchestrator{
		channelSelector: &staticChannelSelector{
			candidates: []*ChannelModelsCandidate{{
				Channel: &biz.Channel{Channel: ch, Outbound: outbound},
				Models:  generalAlias(actualModels...),
			}},
		},
		Inbound:               openai.NewInboundTransformer(),
		RequestService:        requestService,
		ChannelService:        channelService,
		PromptProvider:        &stubPromptProvider{},
		SystemService:         systemService,
		UsageLogService:       usageLogService,
		PipelineFactory:       pipeline.NewFactory(executor),
		ModelMapper:           NewModelMapper(),
		channelLimiterManager: NewChannelLimiterManager(),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
	}

	return contexts.WithProjectID(ctx, project.ID), client, orchestrator, executor
}

func aliasRateLimitStep() executorStep {
	return executorStep{err: &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": []string{"30"}},
		Body:       []byte(`{"error":{"message":"rate limit reached for model","type":"tokens"}}`),
	}}
}

func requestedUpstreamModels(t *testing.T, executor *sequenceExecutor) []any {
	t.Helper()

	models := make([]any, 0, len(executor.requests))
	for _, request := range executor.requests {
		var body map[string]any
		require.NoError(t, json.Unmarshal(request.Body, &body))
		models = append(models, body["model"])
	}

	return models
}

func TestChatCompletionOrchestrator_Process_AliasFailoverIgnoresSameChannelBudget(t *testing.T) {
	ctx, client, orchestrator, executor := newAliasFailoverOrchestrator(t,
		&biz.RetryPolicy{
			Enabled:                 true,
			MaxChannelRetries:       0,
			MaxSingleChannelRetries: 0,
			RetryDelayMs:            0,
			LoadBalancerStrategy:    "adaptive",
		},
		[]executorStep{
			aliasRateLimitStep(),
			aliasRateLimitStep(),
			{resp: &httpclient.Response{
				StatusCode: http.StatusOK,
				Body:       buildMockOpenAIResponse("chatcmpl-alias", "gpt-oss", "Recovered", 10, 20),
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
			}},
		},
		"llama", "qwen", "gpt-oss",
	)

	result, err := orchestrator.Process(ctx, buildTestRequest("general", "Hello!", false))
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletion)

	require.Len(t, executor.requests, 3, "every alias target is tried although the same-channel budget is zero")
	assert.ElementsMatch(t, []any{"llama", "qwen", "gpt-oss"}, requestedUpstreamModels(t, executor))

	executions, err := client.RequestExecution.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, executions, 3)
}

func TestChatCompletionOrchestrator_Process_AliasFailoverFollowsRetrySwitch(t *testing.T) {
	ctx, _, orchestrator, executor := newAliasFailoverOrchestrator(t,
		&biz.RetryPolicy{
			Enabled:                 false,
			MaxChannelRetries:       3,
			MaxSingleChannelRetries: 2,
			LoadBalancerStrategy:    "adaptive",
		},
		[]executorStep{aliasRateLimitStep(), aliasRateLimitStep()},
		"llama", "qwen",
	)

	_, err := orchestrator.Process(ctx, buildTestRequest("general", "Hello!", false))
	require.Error(t, err)
	require.Len(t, executor.requests, 1, "a disabled retry policy must not switch alias targets")
}

func TestChatCompletionOrchestrator_Process_AliasFailoverKeepsBudgetForSharedErrors(t *testing.T) {
	ctx, _, orchestrator, executor := newAliasFailoverOrchestrator(t,
		&biz.RetryPolicy{
			Enabled:                 true,
			MaxChannelRetries:       0,
			MaxSingleChannelRetries: 0,
			LoadBalancerStrategy:    "adaptive",
		},
		[]executorStep{
			{err: &httpclient.Error{
				StatusCode: http.StatusUnauthorized,
				Body:       []byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`),
			}},
			aliasRateLimitStep(),
		},
		"llama", "qwen",
	)

	_, err := orchestrator.Process(ctx, buildTestRequest("general", "Hello!", false))
	require.Error(t, err)
	require.Len(t, executor.requests, 1, "a 401 hits every target alike, so it keeps spending the zero same-channel budget")
}
