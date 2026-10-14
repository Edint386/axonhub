package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestChannelTestFormatStreams(t *testing.T) {
	require.False(t, channelTestFormatStreams(llm.APIFormatTypeSafeSystemOne))
	require.False(t, channelTestFormatStreams(llm.APIFormatOpenAIDecisions))
	require.True(t, channelTestFormatStreams(llm.APIFormatOpenAIChatCompletion))
}

func TestChannelHealthProbeRunnerProbesSingleResponseFormatsWithoutStreaming(t *testing.T) {
	ctx, client := setupTest(t)
	channelService, _, _, _ := setupTestServices(t, client)

	createChannel := func(name string, channelType channel.Type, endpoints []objects.ChannelEndpoint) int {
		create := client.Channel.Create().SetType(channelType).SetName(name).
			SetBaseURL("https://example.com/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
			SetSupportedModels([]string{"test-model"}).SetDefaultTestModel("test-model")
		if endpoints != nil {
			create.SetEndpoints(endpoints)
		}
		ch, err := create.Save(ctx)
		require.NoError(t, err)

		return ch.ID
	}
	systemOneID := createChannel("System One", channel.TypeTypesafe, nil)
	decisionsID := createChannel("Decisions", channel.TypeOpenai, []objects.ChannelEndpoint{
		{APIFormat: llm.APIFormatOpenAIDecisions.String(), Path: "/decisions"},
	})
	chatID := createChannel("Chat", channel.TypeOpenai, nil)
	// The channel test always streams over a Responses WebSocket endpoint, so a
	// requested non-streaming probe must be recorded as streaming.
	webSocketID := createChannel("Responses WebSocket", channel.TypeOpenaiResponses, []objects.ChannelEndpoint{
		{APIFormat: llm.APIFormatOpenAIResponseWebSocket.String()},
	})

	runner := &ChannelHealthProbeRunner{tester: &TestChannelOrchestrator{channelService: channelService}}
	targets := runner.withSupportedStreamModes(ctx, []biz.ChannelHealthProbeTarget{
		{ChannelID: systemOneID, ModelID: "test-model", Stream: true},
		{ChannelID: decisionsID, ModelID: "test-model", Stream: true},
		{ChannelID: chatID, ModelID: "test-model", Stream: true},
		{ChannelID: chatID, ModelID: "test-model", Stream: false},
		{ChannelID: webSocketID, ModelID: "test-model", Stream: false},
		{ChannelID: webSocketID, ModelID: "test-model", Stream: true},
	})

	require.Equal(t, []bool{false, false, true, false, true, true}, lo.Map(targets, func(target biz.ChannelHealthProbeTarget, _ int) bool {
		return target.Stream
	}))
}

func TestChannelHealthProbeRunnerManualSystemOneProbeRecordsNonStreaming(t *testing.T) {
	ctx, client := setupTest(t)
	ctx = contexts.WithProjectID(ctx, createTestProject(t, ctx, client).ID)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"test-model","answers":{"connection":{"type":"noul","noul":0.95}},"usage":{"input_tokens":10,"output_tokens":0}}`))
	}))
	t.Cleanup(server.Close)

	ch, err := client.Channel.Create().SetType(channel.TypeTypesafe).SetName("System One probe").
		SetBaseURL(server.URL + "/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"test-model"}).SetDefaultTestModel("test-model").Save(ctx)
	require.NoError(t, err)

	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
	protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{Ent: client})
	t.Cleanup(protection.Stop)
	runner := NewChannelHealthProbeRunner(ChannelHealthProbeRunnerParams{
		Service: biz.NewChannelHealthProbeService(biz.ChannelHealthProbeServiceParams{
			Ent:            client,
			ChannelService: channelService,
			SystemService:  systemService,
		}),
		Tester: NewTestChannelOrchestrator(channelService, requestService, systemService, usageLogService, protection, httpclient.NewHttpClient()),
	})

	record, err := runner.RunManual(ctx, biz.RunChannelHealthProbeInput{
		ChannelID: objects.GUID{Type: "Channel", ID: ch.ID},
		ModelID:   "test-model",
		Stream:    true,
	})
	require.NoError(t, err)
	require.False(t, record.Stream, "System One answers in one response, so the run must be stored as non-streaming")
	require.Equal(t, "healthy", record.Status, lo.FromPtr(record.ErrorMessage))
	require.EqualValues(t, 1, calls.Load())
}
