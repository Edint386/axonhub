package bailianasr

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/looplj/axonhub/llm/auth"
)

// DefaultNativeBaseURL is the public DashScope host used when the channel has no base URL.
const DefaultNativeBaseURL = "https://dashscope.aliyuncs.com"

const (
	defaultPollInterval    = time.Second
	defaultMaxPollInterval = 5 * time.Second
	defaultStartTimeout    = 60 * time.Second
)

// Config configures one wrapped Bailian outbound.
type Config struct {
	// BaseURL is the channel (or endpoint) base URL; the native DashScope API host is derived
	// from it with NativeBaseURL.
	BaseURL string

	// APIKeyProvider supplies the DashScope API key, the same key compatible-mode chat uses.
	APIKeyProvider auth.APIKeyProvider

	// CacheScope keeps the job-cache entries of outbounds with different scopes apart. The biz
	// hook leaves it empty, so a resend that reaches another Bailian channel with the same
	// DashScope endpoint still resumes its job (see jobCache).
	CacheScope string

	// PollInterval is the delay before the first task poll; every later poll waits one
	// PollInterval longer, up to MaxPollInterval. Zero values mean 1 s and 5 s.
	PollInterval    time.Duration
	MaxPollInterval time.Duration

	// StartTimeout bounds every step of an async job before its task exists, which the pipeline's
	// response timeouts no longer cover for a long-running job: each getPolicy and submit call,
	// and the temporary-storage upload while it makes no progress. Zero means 60 s.
	StartTimeout time.Duration

	// MessageFrameInterval is the pause between two 100 ms audio frames sent to the -message
	// model. Zero uses AXONHUB_BAILIAN_ASR_MESSAGE_PACE (frames at that multiple of real time,
	// default 4; 0 sends without pausing); a negative value sends without pausing.
	MessageFrameInterval time.Duration

	// MessageMaxDuration caps the audio length the -message model accepts. Zero uses
	// AXONHUB_BAILIAN_ASR_MESSAGE_MAX_SECONDS (default 600).
	MessageMaxDuration time.Duration
}

const (
	defaultMessagePace       = 4.0
	defaultMessageMaxSeconds = 600
)

type messageSettings struct {
	pace       float64
	maxSeconds float64
}

// messageEnv reads the -message knobs once; invalid values fall back to the defaults.
var messageEnv = sync.OnceValue(func() messageSettings {
	settings := messageSettings{pace: defaultMessagePace, maxSeconds: defaultMessageMaxSeconds}

	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("AXONHUB_BAILIAN_ASR_MESSAGE_PACE")), 64); err == nil && v >= 0 {
		settings.pace = v
	}

	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("AXONHUB_BAILIAN_ASR_MESSAGE_MAX_SECONDS")), 64); err == nil && v > 0 {
		settings.maxSeconds = v
	}

	return settings
})

func (c Config) messageFrameInterval() time.Duration {
	switch {
	case c.MessageFrameInterval > 0:
		return c.MessageFrameInterval
	case c.MessageFrameInterval < 0:
		return 0
	}

	pace := messageEnv().pace
	if pace <= 0 {
		return 0
	}

	return time.Duration(float64(messageFrameDuration) / pace)
}

func (c Config) messageMaxDuration() time.Duration {
	if c.MessageMaxDuration > 0 {
		return c.MessageMaxDuration
	}

	return time.Duration(messageEnv().maxSeconds * float64(time.Second))
}

func (c Config) startTimeout() time.Duration {
	if c.StartTimeout > 0 {
		return c.StartTimeout
	}

	return defaultStartTimeout
}

func (c Config) pollIntervals() (time.Duration, time.Duration) {
	first, maxInterval := c.PollInterval, c.MaxPollInterval
	if first <= 0 {
		first = defaultPollInterval
	}

	if maxInterval < first {
		maxInterval = max(first, defaultMaxPollInterval)
	}

	return first, maxInterval
}

// dashScopePathMarkers start the API part of a DashScope base URL path.
var dashScopePathMarkers = []string{"/compatible-mode", "/apps/anthropic", "/api/v1", "/api-ws/v1"}

// NativeBaseURL derives the native DashScope API origin from a channel base URL. Everything from
// the first DashScope API path segment on is dropped and any proxy prefix before it is kept:
// "https://dashscope.aliyuncs.com/compatible-mode/v1" becomes "https://dashscope.aliyuncs.com".
func NativeBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return DefaultNativeBaseURL
	}

	cut := len(base)
	for _, marker := range dashScopePathMarkers {
		if i := strings.Index(base, marker); i >= 0 && i < cut {
			cut = i
		}
	}

	return base[:cut]
}
