package biz

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeSystemChannelSettingsPrompts(t *testing.T) {
	setting := SystemChannelSettings{TestSystemPrompt: " \n", TestUserPrompt: "自定义"}
	normalizeSystemChannelSettings(&setting)

	require.Equal(t, defaultChannelTestSystemPrompt, setting.TestSystemPrompt)
	require.Equal(t, "自定义", setting.TestUserPrompt)
	require.NoError(t, validateSystemChannelSettings(&setting))
}

func TestValidateSystemChannelSettingsPromptLength(t *testing.T) {
	setting := SystemChannelSettings{
		TestSystemPrompt: strings.Repeat("a", maxChannelTestPromptRunes+1),
		TestUserPrompt:   defaultChannelTestUserPrompt,
	}

	require.Error(t, validateSystemChannelSettings(&setting))
}

func TestNormalizeSystemChannelSettingsBackfillsActiveProbePolicy(t *testing.T) {
	setting := SystemChannelSettings{}
	normalizeSystemChannelSettings(&setting)

	require.NotNil(t, setting.ActiveHealthProbeScan)
	require.False(t, setting.ActiveHealthProbeScan.Enabled)
	require.Equal(t, 60_000, setting.ActiveHealthProbeScan.AcceptableLatencyMs)
	require.Equal(t, 1, setting.ActiveHealthProbeScan.ExtraChannels)
	require.Equal(t, 24, setting.ActiveHealthProbeScan.P95LookbackHours)
	require.NoError(t, validateSystemChannelSettings(&setting))
}

func TestValidateSystemChannelSettingsActiveProbePolicyLimits(t *testing.T) {
	setting := SystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{AcceptableLatencyMs: 1, ExtraChannels: 0},
	}
	normalizeSystemChannelSettings(&setting)
	require.NoError(t, validateSystemChannelSettings(&setting))

	setting.ActiveHealthProbeScan.AcceptableLatencyMs = maxActiveHealthProbeAcceptableLatencyMs + 1
	require.ErrorContains(t, validateSystemChannelSettings(&setting), "acceptable latency")
	setting.ActiveHealthProbeScan.AcceptableLatencyMs = 1
	setting.ActiveHealthProbeScan.ExtraChannels = maxActiveHealthProbeExtraChannels + 1
	require.ErrorContains(t, validateSystemChannelSettings(&setting), "extra channels")
	setting.ActiveHealthProbeScan.ExtraChannels = 0
	setting.ActiveHealthProbeScan.P95LookbackHours = maxActiveHealthProbeP95LookbackHours + 1
	require.ErrorContains(t, validateSystemChannelSettings(&setting), "P95 lookback")
}

func TestValidateSystemChannelSettingsGlobalProbeModels(t *testing.T) {
	setting := SystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			AcceptableLatencyMs: 1,
			Models: []ActiveHealthProbeModelSetting{
				{ModelID: " gpt-5.6-sol ", Enabled: true},
			},
		},
	}
	normalizeSystemChannelSettings(&setting)
	require.Equal(t, "gpt-5.6-sol", setting.ActiveHealthProbeScan.Models[0].ModelID)
	require.NoError(t, validateSystemChannelSettings(&setting))

	setting.ActiveHealthProbeScan.Models = append(setting.ActiveHealthProbeScan.Models, ActiveHealthProbeModelSetting{ModelID: "gpt-5.6-sol"})
	require.ErrorContains(t, validateSystemChannelSettings(&setting), "configured more than once")
}
