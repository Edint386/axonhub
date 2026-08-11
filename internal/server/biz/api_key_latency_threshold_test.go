package biz

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

func TestValidateProfileLatencyThresholds(t *testing.T) {
	require.NoError(t, validateProfileLatencyThresholds([]objects.APIKeyProfile{{
		Name:                   "default",
		MaxFirstTokenLatencyMs: lo.ToPtr(int64(1500)),
	}}))
	require.NoError(t, validateProfileLatencyThresholds([]objects.APIKeyProfile{{Name: "default"}}))
	require.Error(t, validateProfileLatencyThresholds([]objects.APIKeyProfile{{
		Name:                   "default",
		MaxFirstTokenLatencyMs: lo.ToPtr(int64(0)),
	}}))
}

func TestAPIKeyProfileCloneCopiesLatencyThreshold(t *testing.T) {
	profile := &objects.APIKeyProfile{
		Name:                   "default",
		MaxFirstTokenLatencyMs: lo.ToPtr(int64(1500)),
	}

	clone := profile.Clone()
	require.NotSame(t, profile.MaxFirstTokenLatencyMs, clone.MaxFirstTokenLatencyMs)
	require.Equal(t, int64(1500), *clone.MaxFirstTokenLatencyMs)

	*clone.MaxFirstTokenLatencyMs = 2000
	require.Equal(t, int64(1500), *profile.MaxFirstTokenLatencyMs)
}
