package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestRandomizeDuplicateRequestModelGroups(t *testing.T) {
	models := []biz.ChannelModelEntry{
		{RequestModel: "fast", ActualModel: "gpt-4o-mini", Source: "mapping"},
		{RequestModel: "fast", ActualModel: "gpt-4.1-mini", Source: "mapping"},
		{RequestModel: "premium", ActualModel: "gpt-4o", Source: "mapping"},
	}

	randomized := randomizeDuplicateRequestModelGroupsWithRand(models, func(n int) int {
		require.Equal(t, 2, n)
		return 0
	})

	require.Equal(t, []biz.ChannelModelEntry{
		{RequestModel: "fast", ActualModel: "gpt-4.1-mini", Source: "mapping"},
		{RequestModel: "fast", ActualModel: "gpt-4o-mini", Source: "mapping"},
		{RequestModel: "premium", ActualModel: "gpt-4o", Source: "mapping"},
	}, randomized)
	require.Equal(t, "gpt-4o-mini", models[0].ActualModel, "randomization must not mutate cached candidate models")
}

func TestRandomizeDuplicateRequestModelGroups_NoDuplicateAliasReturnsSameSlice(t *testing.T) {
	models := []biz.ChannelModelEntry{
		{RequestModel: "gpt-4o-mini", ActualModel: "gpt-4o-mini", Source: "direct"},
		{RequestModel: "gpt-4.1-mini", ActualModel: "gpt-4.1-mini", Source: "direct"},
	}

	randomized := randomizeDuplicateRequestModelGroupsWithRand(models, func(n int) int {
		t.Fatalf("random function should not be called without duplicate request models")
		return 0
	})

	require.Same(t, &models[0], &randomized[0])
}

func TestRandomizeAliasModelOrder_KeepsModelAPIFormatsAligned(t *testing.T) {
	candidate := &ChannelModelsCandidate{
		Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "mixed-protocols"}},
		Models: []biz.ChannelModelEntry{
			{RequestModel: "general", ActualModel: "responses-only", Source: "mapping"},
			{RequestModel: "general", ActualModel: "chat-only", Source: "mapping"},
			{RequestModel: "premium", ActualModel: "gpt-4o", Source: "mapping"},
		},
		APIFormat:       "openai/responses",
		modelAPIFormats: []string{"openai/responses", "openai/chat_completions", "openai/chat_completions"},
	}

	randomized := randomizeAliasModelOrderWithRand([]*ChannelModelsCandidate{candidate}, func(n int) int {
		require.Equal(t, 2, n)
		return 0
	})

	require.Len(t, randomized, 1)
	require.Equal(t, []string{"chat-only", "responses-only", "gpt-4o"}, []string{
		randomized[0].Models[0].ActualModel,
		randomized[0].Models[1].ActualModel,
		randomized[0].Models[2].ActualModel,
	})
	require.Equal(t, []string{"openai/chat_completions", "openai/responses", "openai/chat_completions"}, randomized[0].modelAPIFormats,
		"each target must keep its own endpoint format after the shuffle")
	require.Equal(t, "openai/chat_completions", randomized[0].APIFormat, "the first attempt uses the format of the new first target")

	require.Equal(t, "responses-only", candidate.Models[0].ActualModel, "randomization must not mutate the selected candidate")
	require.Equal(t, []string{"openai/responses", "openai/chat_completions", "openai/chat_completions"}, candidate.modelAPIFormats)
	require.Equal(t, "openai/responses", candidate.APIFormat)
}

func TestRandomizeAliasModelOrder_ShufflesEveryAliasGroup(t *testing.T) {
	models := []biz.ChannelModelEntry{
		{RequestModel: "general", ActualModel: "llama", Source: "mapping"},
		{RequestModel: "general", ActualModel: "qwen", Source: "mapping"},
		{RequestModel: "solo", ActualModel: "solo", Source: "direct"},
		{RequestModel: "fast", ActualModel: "gemma", Source: "mapping"},
		{RequestModel: "fast", ActualModel: "mistral", Source: "mapping"},
	}

	randomized := randomizeDuplicateRequestModelGroupsWithRand(models, func(int) int { return 0 })

	require.Equal(t, []string{"qwen", "llama", "solo", "mistral", "gemma"}, []string{
		randomized[0].ActualModel,
		randomized[1].ActualModel,
		randomized[2].ActualModel,
		randomized[3].ActualModel,
		randomized[4].ActualModel,
	}, "alias targets move only within the positions their alias already holds")
}
