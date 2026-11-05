package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestSelectRealtime_TranslationOnlyReachesBailianLiveTranslate(t *testing.T) {
	ctx, client := setupTest(t)

	bailianChannel := client.Channel.Create().
		SetType(channel.TypeBailian).
		SetName("bailian-livetranslate").
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-bailian"}).
		SetSupportedModels([]string{"qwen3.8-livetranslate-flash-realtime", "qwen3.8-omni-flash-realtime", "gpt-realtime"}).
		SetDefaultTestModel("qwen3.8-omni-flash-realtime").
		SetOrderingWeight(100).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	openaiChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("openai-realtime").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-openai"}).
		SetSupportedModels([]string{"gpt-realtime", "gpt-realtime-translate"}).
		SetDefaultTestModel("gpt-realtime").
		SetOrderingWeight(10).
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	orch := newRealtimeTestOrchestrator(t, client)

	// Retry disabled: the load balancer keeps a single candidate.
	policy := orch.SystemService.RetryPolicyOrDefault(ctx)
	policy.Enabled = false
	require.NoError(t, orch.SystemService.SetRetryPolicy(ctx, policy))

	ctx = PrepareRealtimeContext(ctx)

	t.Run("a translation session selects the livetranslate channel", func(t *testing.T) {
		selection, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3.8-livetranslate-flash-realtime", Mode: biz.RealtimeModeWebSocket, Translation: true})
		require.NoError(t, err)
		require.Equal(t, []int{bailianChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates))
		require.Equal(t, "qwen3.8-livetranslate-flash-realtime", selection.RequestedModel)
	})

	t.Run("a livetranslate model is refused on /v1/realtime", func(t *testing.T) {
		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3.8-livetranslate-flash-realtime", Mode: biz.RealtimeModeWebSocket})
		require.ErrorIs(t, err, ErrRealtimeTranslationOnly)

		_, err = orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3.8-livetranslate-flash-realtime", Mode: biz.RealtimeModeWebSocket, Intent: realtimeIntentTranscription})
		require.ErrorIs(t, err, ErrRealtimeTranslationOnly)
	})

	t.Run("other models are refused on the translation surface", func(t *testing.T) {
		for _, model := range []string{"qwen3.8-omni-flash-realtime", "gpt-realtime", "gpt-realtime-translate"} {
			_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: model, Mode: biz.RealtimeModeWebSocket, Translation: true})
			require.ErrorIs(t, err, ErrRealtimeNotTranslationModel, model)
		}

		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "no-such-model", Mode: biz.RealtimeModeWebSocket, Translation: true})
		require.ErrorIs(t, err, biz.ErrInvalidModel)
		require.NotErrorIs(t, err, ErrRealtimeNotTranslationModel)
	})

	t.Run("conversation selection is unchanged", func(t *testing.T) {
		selection, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "gpt-realtime", Mode: biz.RealtimeModeWebSocket})
		require.NoError(t, err)
		require.Equal(t, []int{openaiChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates),
			"the heavier bailian channel cannot serve gpt-realtime")

		selection, err = orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3.8-omni-flash-realtime", Mode: biz.RealtimeModeWebSocket})
		require.NoError(t, err)
		require.Equal(t, []int{bailianChannel.ID}, realtimeCandidateChannelIDs(selection.Candidates))
	})

	t.Run("webrtc never selects a livetranslate channel", func(t *testing.T) {
		_, err := orch.SelectRealtime(ctx, RealtimeSelectionRequest{Model: "qwen3.8-livetranslate-flash-realtime", Mode: biz.RealtimeModeWebRTC})
		require.ErrorIs(t, err, biz.ErrInvalidModel)
		require.NotErrorIs(t, err, ErrRealtimeTranslationOnly)
	})
}
