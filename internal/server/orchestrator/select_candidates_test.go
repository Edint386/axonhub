package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

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
