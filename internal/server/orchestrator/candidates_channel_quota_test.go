package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

func TestChannelQuotaSelector_FiltersExhaustedHighPriorityCandidate(t *testing.T) {
	ctx, client := setupTest(t)

	p, err := client.Project.Create().
		SetName("p").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	exhausted, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("exhausted-high-priority").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key-1"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetPriority(100).
		SetStatus(channel.StatusEnabled).
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

	fallback, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("fallback-low-priority").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key-2"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetPriority(10).
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	createdAt := time.Now().UTC().Add(-time.Minute)
	req, err := client.Request.Create().
		SetProjectID(p.ID).
		SetAPIKeyID(1).
		SetModelID("gpt-4").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		SetCreatedAt(createdAt).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.UsageLog.Create().
		SetRequestID(req.ID).
		SetAPIKeyID(1).
		SetProjectID(p.ID).
		SetChannelID(exhausted.ID).
		SetModelID("gpt-4").
		SetCreatedAt(createdAt).
		Save(ctx)
	require.NoError(t, err)

	systemService := newTestSystemService(client)
	quotaService := biz.NewQuotaService(client, systemService)
	check, err := quotaService.CheckChannelQuota(ctx, exhausted.ID, exhausted.Settings.Quota)
	require.NoError(t, err)
	require.False(t, check.Allowed)

	inner := &mockSelector{
		candidates: []*ChannelModelsCandidate{
			{
				Channel:  &biz.Channel{Channel: exhausted},
				Priority: 0,
				Models:   []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
			},
			{
				Channel:  &biz.Channel{Channel: fallback},
				Priority: 0,
				Models:   []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
			},
		},
	}

	quotaSelector := WithChannelQuotaSelector(inner, quotaService)
	loadBalanced := WithLoadBalancedSelector(
		quotaSelector,
		newTestLoadBalancer(t, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 2}, &mockStrategy{name: "equal", score: 1}),
		systemService,
	)

	got, err := loadBalanced.Select(ctx, &llm.Request{Model: "gpt-4"})
	require.NoError(t, err)
	require.Equal(t, 1, quotaSelector.FilteredCount)
	require.Len(t, got, 1)
	require.Equal(t, fallback.ID, got[0].Channel.ID)
}

func TestChannelQuotaSelector_KeepsPriorityOrderWhenQuotaAvailable(t *testing.T) {
	quotaSelector := WithChannelQuotaSelector(&mockSelector{
		candidates: []*ChannelModelsCandidate{
			{Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "high", Priority: 100}}, Priority: 0},
			{Channel: &biz.Channel{Channel: &ent.Channel{ID: 2, Name: "low", Priority: 10}}, Priority: 0},
		},
	}, nil)

	loadBalanced := WithLoadBalancedSelector(
		quotaSelector,
		newTestLoadBalancer(t, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 2}, &mockStrategy{name: "equal", score: 1}),
		&mockSystemService{retryPolicy: &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 2}},
	)

	got, err := loadBalanced.Select(context.Background(), &llm.Request{Model: "gpt-4"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 1, got[0].Channel.ID)
	require.Equal(t, 2, got[1].Channel.ID)
}
