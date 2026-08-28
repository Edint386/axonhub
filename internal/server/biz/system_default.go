package biz

const (
	defaultChannelTestSystemPrompt = "You are a helpful assistant."
	defaultChannelTestUserPrompt   = "Hello world, I'm AxonHub.\nPlease tell me who you are?"
	maxChannelTestPromptRunes      = 4096

	minActiveHealthProbeAcceptableLatencyMs = 1
	maxActiveHealthProbeAcceptableLatencyMs = 10 * 60 * 1000
	minActiveHealthProbeExtraChannels       = 0
	maxActiveHealthProbeExtraChannels       = 20
	minActiveHealthProbeP95LookbackHours    = 1
	maxActiveHealthProbeP95LookbackHours    = 30 * 24
	minActiveHealthProbeGateWindowMinutes   = 1
	maxActiveHealthProbeGateWindowMinutes   = 24 * 60
)

var defaultActiveHealthProbeScanSetting = ActiveHealthProbeScanSetting{
	Enabled:             false,
	IntervalMinutes:     5,
	AcceptableLatencyMs: 60 * 1000,
	ExtraChannels:       1,
	P95LookbackHours:    24,
	// 30 minutes at the default 5-minute interval leaves ~6 probe samples, which is
	// twice the routing ceiling's minimum of 3 -- enough that one cold-start blip
	// cannot evict a channel, while still describing the channel's CURRENT speed.
	//
	// This is deliberately NOT P95LookbackHours. That window answers "how has this
	// channel behaved lately" for the dashboard, where a day of history is the point;
	// the gate answers "is this channel fast right now", where a day of history is
	// precisely what must not dilute a change that happened minutes ago.
	GateWindowMinutes: 30,
}

func defaultCleanupOptions() []CleanupOption {
	return []CleanupOption{
		{
			ResourceType: CleanupResourceRequests,
			Enabled:      false,
			CleanupDays:  3,
		},
		{
			ResourceType: CleanupResourceUsageLogs,
			Enabled:      false,
			CleanupDays:  30,
		},
		{
			ResourceType: CleanupResourceRequestBodies,
			Enabled:      false,
			CleanupDays:  7,
		},
		{
			ResourceType: CleanupResourceResponseBodies,
			Enabled:      false,
			CleanupDays:  7,
		},
		{
			ResourceType: CleanupResourceResponseChunks,
			Enabled:      false,
			CleanupDays:  3,
		},
		{
			ResourceType: CleanupResourceChannelHealthProbeRuns,
			Enabled:      true,
			CleanupDays:  30,
		},
	}
}

// mergeCleanupOptions keeps existing entries and appends any missing defaults.
func mergeCleanupOptions(existing []CleanupOption) []CleanupOption {
	byType := make(map[string]CleanupOption, len(existing))
	order := make([]string, 0, len(existing)+5)

	for _, opt := range existing {
		if _, seen := byType[opt.ResourceType]; !seen {
			order = append(order, opt.ResourceType)
		}

		byType[opt.ResourceType] = opt
	}

	for _, def := range defaultCleanupOptions() {
		if _, seen := byType[def.ResourceType]; seen {
			continue
		}

		byType[def.ResourceType] = def
		order = append(order, def.ResourceType)
	}

	merged := make([]CleanupOption, 0, len(order))
	for _, resourceType := range order {
		merged = append(merged, byType[resourceType])
	}

	return merged
}

var defaultStoragePolicy = StoragePolicy{
	StoreChunks:       false,
	LivePreview:       false,
	StoreRequestBody:  true,
	StoreResponseBody: true,
	CleanupOptions:    defaultCleanupOptions(),
}

var defaultRetryPolicy = RetryPolicy{
	MaxChannelRetries:       3,
	MaxSingleChannelRetries: 2,
	RetryDelayMs:            1000,
	LoadBalancerStrategy:    "adaptive",
	TraceStickyMode:         TraceStickyPreferPreviousChannel,
	Enabled:                 true,
	UpstreamErrorPolicy: UpstreamErrorPolicy{
		Mode: UpstreamErrorModePassthrough,
	},
}

var defaultModelSettings = SystemModelSettings{
	FallbackToChannelsOnModelNotFound: true,
	QueryAllChannelModels:             true,
	DefaultModelAPIIncludeAll:         false,
	AutoReasoningEffort:               false,
	ModelBlacklistRegex:               "",
	HideUnroutableModelsInList:        false,
	DeveloperSettings:                 []*DeveloperModelSettings{},
}

var defaultChannelSetting = SystemChannelSettings{
	Probe: ChannelProbeSetting{
		Enabled:   true,
		Frequency: ProbeFrequency5Min,
	},
	AutoSync: ChannelModelAutoSyncSetting{
		Frequency: AutoSyncFrequencyOneHour,
	},
	ActiveHealthProbeScan: &defaultActiveHealthProbeScanSetting,
	TestSystemPrompt:      defaultChannelTestSystemPrompt,
	TestUserPrompt:        defaultChannelTestUserPrompt,
}

var defaultGeneralSettings = SystemGeneralSettings{
	CurrencyCode: "USD",
	Timezone:     "UTC",
}

var defaultAutoBackupSettings = AutoBackupSettings{
	Enabled:              false,
	Frequency:            BackupFrequencyDaily,
	IncludeSystemConfigs: false,
	IncludeChannels:      true,
	IncludeModels:        true,
	IncludeAPIKeys:       false,
	IncludeModelPrices:   true,
	IncludeUsageStats:    false,
	IncludeRequestLogs:   false,
	RetentionDays:        30,
}

var defaultVideoStorageSettings = VideoStorageSettings{
	Enabled:             false,
	DataStorageID:       0,
	ScanIntervalMinutes: 1,
	ScanLimit:           50,
}

var defaultQuotaEnforcementSettings = QuotaEnforcementSettings{
	Enabled: false,
	Mode:    QuotaEnforcementModeExhaustedOnly,
}

var defaultSecuritySettings = SecuritySettings{
	BlockedIPs:              []string{},
	ShowRequestLogIPBanIcon: true,
}
