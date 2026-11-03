package realtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
)

func TestUsageForAccounting_CopiesDurationSeconds(t *testing.T) {
	rec := dialect.UsageRecord{
		Kind:            "transcription",
		DurationSeconds: 4,
	}
	usage := usageForAccounting(rec)
	require.InDelta(t, 4, usage.DurationSeconds, 0)
	require.Zero(t, usage.PromptTokens)
	require.Zero(t, usage.CompletionTokens)
}

func TestUsageForAccounting_PreservesTokenUsageAndAddsDuration(t *testing.T) {
	orig := &llm.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	rec := dialect.UsageRecord{
		Usage:           orig,
		DurationSeconds: 3,
	}
	usage := usageForAccounting(rec)
	require.Equal(t, int64(10), usage.PromptTokens)
	require.Equal(t, int64(2), usage.CompletionTokens)
	require.InDelta(t, 3, usage.DurationSeconds, 0)
	// Original pointer must not be mutated.
	require.Zero(t, orig.DurationSeconds)
}
