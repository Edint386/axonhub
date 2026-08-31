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
			OpencodeGo: &objects.OpenCodeGoQuotaSettings{
				WorkspaceID: "wk_1",
				AuthCookie:  "auth=live-session-cookie",
			},
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

		merged := mergeChannelSettingsForUpdate(existing, input)
		require.Equal(t, "new-", merged.ExtraModelPrefix)
		require.NotNil(t, merged.Quota)
		require.Equal(t, int64(10), *merged.Quota.Requests)
		require.Equal(t, existing.ProviderQuota, merged.ProviderQuota)
		require.Equal(t, existing.HealthProbe, merged.HealthProbe)
	})

	t.Run("empty auth cookie keeps stored credential", func(t *testing.T) {
		input := &objects.ChannelSettings{
			ProviderQuota: &objects.ChannelProviderQuotaSettings{
				OpencodeGo: &objects.OpenCodeGoQuotaSettings{
					WorkspaceID: "wk_2",
					AuthCookie:  "  ",
				},
			},
		}

		merged := mergeChannelSettingsForUpdate(existing, input)
		require.NotNil(t, merged.ProviderQuota)
		require.NotNil(t, merged.ProviderQuota.OpencodeGo)
		require.Equal(t, "wk_2", merged.ProviderQuota.OpencodeGo.WorkspaceID)
		require.Equal(t, "auth=live-session-cookie", merged.ProviderQuota.OpencodeGo.AuthCookie)
		require.Equal(t, existing.HealthProbe, merged.HealthProbe)
	})

	t.Run("explicit probe update is applied", func(t *testing.T) {
		disabled := false
		input := &objects.ChannelSettings{
			HealthProbe: &objects.ChannelHealthProbeSettings{
				ProbeEnabled: &disabled,
			},
		}

		merged := mergeChannelSettingsForUpdate(existing, input)
		require.NotNil(t, merged.HealthProbe)
		require.NotNil(t, merged.HealthProbe.ProbeEnabled)
		require.False(t, *merged.HealthProbe.ProbeEnabled)
		require.Equal(t, existing.ProviderQuota, merged.ProviderQuota)
	})
}
