package biz

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

// Custom guard: the upstream settings patch (UpdateChannelSettingsInput) must
// carry the custom channel quota, its timezone and the health probe settings.
// Without these fields a patch could neither set them, and a merge that drops
// them from Apply would silently keep the stored values forever.
func TestUpdateChannelSettingsInput_Apply_CustomFields(t *testing.T) {
	enabled := true
	base := &objects.ChannelSettings{
		ExtraModelPrefix: "deepseek",
		Quota:            &objects.APIKeyQuota{Requests: lo.ToPtr(int64(10))},
		QuotaTimezone:    lo.ToPtr("Asia/Shanghai"),
		HealthProbe:      &objects.ChannelHealthProbeSettings{ProbeEnabled: &enabled},
	}

	t.Run("absent custom fields keep stored values", func(t *testing.T) {
		merged := (&UpdateChannelSettingsInput{ExtraModelPrefix: lo.ToPtr("anthropic")}).Apply(base)
		require.Equal(t, "anthropic", merged.ExtraModelPrefix)
		require.Equal(t, base.Quota, merged.Quota)
		require.Equal(t, base.QuotaTimezone, merged.QuotaTimezone)
		require.Equal(t, base.HealthProbe, merged.HealthProbe)
	})

	t.Run("present custom fields replace stored values", func(t *testing.T) {
		disabled := false
		patch := &UpdateChannelSettingsInput{
			Quota:         &objects.APIKeyQuota{TotalTokens: lo.ToPtr(int64(5000))},
			QuotaTimezone: lo.ToPtr("Europe/Berlin"),
			HealthProbe:   &objects.ChannelHealthProbeSettings{ProbeEnabled: &disabled},
		}
		merged := patch.Apply(base)
		require.Equal(t, patch.Quota, merged.Quota)
		require.Equal(t, "Europe/Berlin", *merged.QuotaTimezone)
		require.False(t, *merged.HealthProbe.ProbeEnabled)
		// Untouched fields survive.
		require.Equal(t, "deepseek", merged.ExtraModelPrefix)
	})
}

func TestChannelService_UpdateChannelSettings_KeepsCustomFields(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := channelAutoPriceTestContext(client)
	enabled := true

	created, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:            channel.TypeOpenai,
		Name:            "Custom Settings Patch",
		BaseURL:         lo.ToPtr("https://api.openai.com/v1"),
		Credentials:     objects.ChannelCredentials{APIKey: "key"},
		SupportedModels: []string{"gpt-test"},
		Settings: &objects.ChannelSettings{
			Quota: &objects.APIKeyQuota{
				Requests: lo.ToPtr(int64(10)),
				Period:   objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
			},
			QuotaTimezone: lo.ToPtr("Asia/Shanghai"),
			HealthProbe:   &objects.ChannelHealthProbeSettings{ProbeEnabled: &enabled},
		},
	})
	require.NoError(t, err)

	// A patch to an upstream field must not touch the custom settings.
	updated, err := svc.UpdateChannelSettings(ctx, created.ID, &UpdateChannelSettingsInput{
		ExtraModelPrefix: lo.ToPtr("anthropic"),
	})
	require.NoError(t, err)
	require.Equal(t, "anthropic", updated.Settings.ExtraModelPrefix)
	require.NotNil(t, updated.Settings.Quota)
	require.Equal(t, int64(10), *updated.Settings.Quota.Requests)
	require.Equal(t, "Asia/Shanghai", *updated.Settings.QuotaTimezone)
	require.NotNil(t, updated.Settings.HealthProbe)
	require.True(t, *updated.Settings.HealthProbe.ProbeEnabled)

	// A patch to the custom settings goes through the same validation as a
	// full update and leaves the upstream fields alone.
	updated, err = svc.UpdateChannelSettings(ctx, created.ID, &UpdateChannelSettingsInput{
		Quota: &objects.APIKeyQuota{
			TotalTokens: lo.ToPtr(int64(5000)),
			Period:      objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
		},
		QuotaTimezone: lo.ToPtr(" Europe/Berlin "),
	})
	require.NoError(t, err)
	require.Equal(t, "anthropic", updated.Settings.ExtraModelPrefix)
	require.Nil(t, updated.Settings.Quota.Requests)
	require.Equal(t, int64(5000), *updated.Settings.Quota.TotalTokens)
	require.Equal(t, "Europe/Berlin", *updated.Settings.QuotaTimezone)
	require.True(t, *updated.Settings.HealthProbe.ProbeEnabled)

	_, err = svc.UpdateChannelSettings(ctx, created.ID, &UpdateChannelSettingsInput{
		QuotaTimezone: lo.ToPtr("Mars/Olympus"),
	})
	require.Error(t, err)
}
