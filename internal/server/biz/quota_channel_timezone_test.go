package biz

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
)

func TestNormalizeQuotaTimezone(t *testing.T) {
	require.NoError(t, NormalizeQuotaTimezone(nil))
	require.NoError(t, NormalizeQuotaTimezone(lo.ToPtr("UTC")))

	blank := lo.ToPtr("  ")
	require.NoError(t, NormalizeQuotaTimezone(blank))
	require.NotNil(t, blank, "an explicit system choice must stay a pointer")
	require.Empty(t, *blank)

	padded := lo.ToPtr("  Asia/Shanghai  ")
	require.NoError(t, NormalizeQuotaTimezone(padded))
	require.Equal(t, "Asia/Shanghai", *padded, "the stored value is trimmed")

	require.ErrorContains(t, NormalizeQuotaTimezone(lo.ToPtr("Mars/Olympus")), "unknown timezone")
	require.ErrorContains(t, NormalizeQuotaTimezone(lo.ToPtr("Local")), "unknown timezone", "Local is the server timezone, not the system setting")
}

func TestChannelQuotaLocationOnlyForCalendarPeriods(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	svc := NewQuotaService(client, NewSystemService(SystemServiceParams{Ent: client}))
	ctx := t.Context()
	timezone := lo.ToPtr("Asia/Shanghai")

	calendar := &objects.APIKeyQuota{Period: objects.APIKeyQuotaPeriod{
		Type:             objects.APIKeyQuotaPeriodTypeCalendarDuration,
		CalendarDuration: &objects.APIKeyQuotaCalendarDuration{Unit: objects.APIKeyQuotaCalendarDurationUnitDay},
	}}
	pastDuration := &objects.APIKeyQuota{Period: objects.APIKeyQuotaPeriod{
		Type:         objects.APIKeyQuotaPeriodTypePastDuration,
		PastDuration: &objects.APIKeyQuotaPastDuration{Value: 1, Unit: objects.APIKeyQuotaPastDurationUnitDay},
	}}

	loc := svc.channelQuotaLocation(ctx, calendar, timezone)
	require.NotNil(t, loc)
	require.Equal(t, "Asia/Shanghai", loc.String())
	require.Nil(t, svc.channelQuotaLocation(ctx, pastDuration, timezone), "rolling periods do not use a timezone")
	require.Nil(t, svc.channelQuotaLocation(ctx, calendar, lo.ToPtr("Mars/Olympus")), "an unloadable value falls back to the system timezone")
	require.Nil(t, svc.channelQuotaLocation(ctx, calendar, nil))
}

// TestQuotaService_ChannelQuotaUsesChannelTimezone checks that a calendar-day
// channel quota splits days in the channel's QuotaTimezone, and falls back to the
// system timezone when the channel has none or an unloadable one.
func TestQuotaService_ChannelQuotaUsesChannelTimezone(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	systemService := NewSystemService(SystemServiceParams{Ent: client})
	require.NoError(t, systemService.SetGeneralSettings(ctx, SystemGeneralSettings{
		CurrencyCode: "USD",
		Timezone:     "America/New_York",
	}))
	svc := NewQuotaService(client, systemService)

	quota := &objects.APIKeyQuota{
		Requests: lo.ToPtr(int64(1)),
		Period: objects.APIKeyQuotaPeriod{
			Type:             objects.APIKeyQuotaPeriodTypeCalendarDuration,
			CalendarDuration: &objects.APIKeyQuotaCalendarDuration{Unit: objects.APIKeyQuotaCalendarDurationUnitDay},
		},
	}

	// today returns [midnight, next midnight) in the zone. It is computed before
	// and after the call so a run that crosses midnight still matches one of them.
	// The end comes from the next calendar day, so 23h and 25h DST days count.
	type day struct{ start, end time.Time }
	today := func(name string) day {
		loc, err := time.LoadLocation(name)
		require.NoError(t, err)
		now := time.Now().In(loc)
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

		return day{start: start.UTC(), end: start.AddDate(0, 0, 1).UTC()}
	}
	requireWindowStartsAtMidnight := func(timezone *string, zone string) {
		t.Helper()

		before := today(zone)
		result, err := svc.GetChannelQuota(ctx, 1, quota, timezone)
		require.NoError(t, err)
		after := today(zone)

		require.NotNil(t, result.Window.Start)
		require.NotNil(t, result.Window.End)
		got := day{start: result.Window.Start.UTC(), end: result.Window.End.UTC()}
		require.Contains(t, []day{before, after}, got, "window must run from midnight to the next midnight in %s", zone)

		check, err := svc.CheckChannelQuota(ctx, 1, quota, timezone)
		require.NoError(t, err)
		require.Contains(t, []time.Time{before.start, after.start}, check.Window.Start.UTC())
	}

	requireWindowStartsAtMidnight(lo.ToPtr("Asia/Shanghai"), "Asia/Shanghai")
	requireWindowStartsAtMidnight(nil, "America/New_York")
	requireWindowStartsAtMidnight(lo.ToPtr(""), "America/New_York")
	requireWindowStartsAtMidnight(lo.ToPtr("Mars/Olympus"), "America/New_York")
}

func TestMergeChannelSettingsForUpdateQuotaTimezone(t *testing.T) {
	existing := &objects.ChannelSettings{QuotaTimezone: lo.ToPtr("Asia/Shanghai")}

	t.Run("omitted timezone keeps the stored one", func(t *testing.T) {
		merged := mergeChannelSettingsForUpdate(t.Context(), existing, &objects.ChannelSettings{ExtraModelPrefix: "new-"})
		require.Equal(t, "Asia/Shanghai", lo.FromPtr(merged.QuotaTimezone))
	})

	t.Run("explicit null clears the stored timezone", func(t *testing.T) {
		ctx := WithChannelSettingsExplicitNulls(t.Context(), []string{"quotaTimezone"})
		merged := mergeChannelSettingsForUpdate(ctx, existing, &objects.ChannelSettings{})
		require.Nil(t, merged.QuotaTimezone)
	})

	t.Run("submitted timezone replaces the stored one", func(t *testing.T) {
		merged := mergeChannelSettingsForUpdate(t.Context(), existing, &objects.ChannelSettings{QuotaTimezone: lo.ToPtr("Europe/London")})
		require.Equal(t, "Europe/London", lo.FromPtr(merged.QuotaTimezone))
	})

	t.Run("empty timezone switches back to the system timezone", func(t *testing.T) {
		merged := mergeChannelSettingsForUpdate(t.Context(), existing, &objects.ChannelSettings{QuotaTimezone: lo.ToPtr("")})
		require.Empty(t, lo.FromPtr(merged.QuotaTimezone))
	})
}

func TestChannelService_CreateChannel_RejectsUnknownQuotaTimezone(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	input := func(timezone string) ent.CreateChannelInput {
		return ent.CreateChannelInput{
			Type:             channel.TypeOpenai,
			BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
			Name:             "quota-timezone-" + timezone,
			Credentials:      objects.ChannelCredentials{APIKey: "key"},
			SupportedModels:  []string{"gpt-4"},
			DefaultTestModel: "gpt-4",
			Settings:         &objects.ChannelSettings{QuotaTimezone: lo.ToPtr(timezone)},
		}
	}

	_, err := svc.CreateChannel(ctx, input("Mars/Olympus"))
	require.ErrorContains(t, err, "invalid quota timezone")

	created, err := svc.CreateChannel(ctx, input(" Asia/Shanghai "))
	require.NoError(t, err)
	require.Equal(t, "Asia/Shanghai", lo.FromPtr(created.Settings.QuotaTimezone), "the stored value is trimmed")
}
