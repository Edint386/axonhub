package biz

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

func TestMergeChannelSettingsForUpdatePreservesProbeAndProviderQuota(t *testing.T) {
	enabled := true
	existing := &objects.ChannelSettings{
		ExtraModelPrefix: "old-",
		ProviderQuota: &objects.ChannelProviderQuotaSettings{
			CommandCode: &objects.CommandCodeQuotaSettings{AuthCookie: "session=live"},
		},
		HealthProbe: &objects.ChannelHealthProbeSettings{
			ProbeEnabled: &enabled,
		},
	}

	t.Run("omitted probe and quota are kept", func(t *testing.T) {
		input := &objects.ChannelSettings{
			ExtraModelPrefix: "new-",
			Quota:            &objects.APIKeyQuota{Requests: lo.ToPtr(int64(10))},
		}

		merged := mergeChannelSettingsForUpdate(t.Context(), existing, input)
		require.Equal(t, "new-", merged.ExtraModelPrefix)
		require.NotNil(t, merged.Quota)
		require.Equal(t, int64(10), *merged.Quota.Requests)
		require.Equal(t, existing.ProviderQuota, merged.ProviderQuota)
		require.Equal(t, existing.HealthProbe, merged.HealthProbe)
	})

	t.Run("explicit probe update is applied", func(t *testing.T) {
		disabled := false
		input := &objects.ChannelSettings{
			HealthProbe: &objects.ChannelHealthProbeSettings{
				ProbeEnabled: &disabled,
			},
		}

		merged := mergeChannelSettingsForUpdate(t.Context(), existing, input)
		require.NotNil(t, merged.HealthProbe)
		require.NotNil(t, merged.HealthProbe.ProbeEnabled)
		require.False(t, *merged.HealthProbe.ProbeEnabled)
		require.Equal(t, existing.ProviderQuota, merged.ProviderQuota)
	})

	t.Run("explicit empty Ollama cookie clears stored credential", func(t *testing.T) {
		existingWithOllama := &objects.ChannelSettings{
			ProviderQuota: &objects.ChannelProviderQuotaSettings{
				Ollama: &objects.OllamaQuotaSettings{AuthCookie: "__Secure-session=live"},
			},
		}
		input := &objects.ChannelSettings{
			ProviderQuota: &objects.ChannelProviderQuotaSettings{
				Ollama: &objects.OllamaQuotaSettings{AuthCookie: ""},
			},
		}

		merged := mergeChannelSettingsForUpdate(t.Context(), existingWithOllama, input)
		require.NotNil(t, merged.ProviderQuota)
		require.NotNil(t, merged.ProviderQuota.Ollama)
		require.Empty(t, merged.ProviderQuota.Ollama.AuthCookie)
	})
}

func TestMergeChannelSettingsForUpdateQuota(t *testing.T) {
	existing := &objects.ChannelSettings{
		Quota: &objects.APIKeyQuota{
			Requests: lo.ToPtr(int64(100)),
			Period:   objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
		},
	}

	t.Run("omitted quota keeps the stored quota", func(t *testing.T) {
		merged := mergeChannelSettingsForUpdate(t.Context(), existing, &objects.ChannelSettings{ExtraModelPrefix: "new-"})
		require.Equal(t, "new-", merged.ExtraModelPrefix)
		require.Equal(t, existing.Quota, merged.Quota)
	})

	t.Run("explicit null quota clears the stored quota", func(t *testing.T) {
		ctx := WithChannelSettingsExplicitNulls(t.Context(), []string{"quota"})
		merged := mergeChannelSettingsForUpdate(ctx, existing, &objects.ChannelSettings{})
		require.Nil(t, merged.Quota)
	})

	t.Run("explicit null on another field keeps the stored quota", func(t *testing.T) {
		ctx := WithChannelSettingsExplicitNulls(t.Context(), []string{"rateLimit"})
		merged := mergeChannelSettingsForUpdate(ctx, existing, &objects.ChannelSettings{})
		require.Equal(t, existing.Quota, merged.Quota)
	})

	t.Run("submitted quota replaces the stored quota", func(t *testing.T) {
		quota := &objects.APIKeyQuota{
			TotalTokens: lo.ToPtr(int64(5000)),
			Period:      objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
		}
		merged := mergeChannelSettingsForUpdate(t.Context(), existing, &objects.ChannelSettings{Quota: quota})
		require.Equal(t, quota, merged.Quota)
	})
}
