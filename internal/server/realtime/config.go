// Package realtime implements the OpenAI Realtime API relay: WebSocket sessions
// (GET /v1/realtime), WebRTC call signalling (POST /v1/realtime/calls, POST /v1/live) and
// sideband relays (GET /v1/realtime?call_id=, GET /v1/live/{call_id}).
//
// Everything in this package is custom (fork) code. Configuration comes from AXONHUB_REALTIME_*
// environment variables so that no upstream-owned config struct has to change; the one
// exception is AXONHUB_CODEX_LIVE_FORWARD_ATTESTATION, which keeps the Codex prefix because it
// only concerns the Codex live (ChatGPT backend) flow.
package realtime

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the relay settings. Zero values are replaced by defaults in LoadConfig.
type Config struct {
	// ConnectBudget bounds the whole pre-upgrade phase (selection, limiter, dial).
	ConnectBudget time.Duration
	// HandshakeTimeout bounds a single upstream WebSocket handshake.
	HandshakeTimeout time.Duration
	// MaxSessionDuration is the maximum lifetime of one session. Zero means "per dialect
	// default" (OpenAI 60m, Bailian 119m).
	MaxSessionDuration time.Duration
	// IdleTimeout closes a session after this long without a data frame in either direction.
	IdleTimeout time.Duration
	// PingInterval is the keepalive ping interval for both sockets.
	PingInterval time.Duration
	// PongWait is the liveness read deadline, refreshed by pongs and data frames.
	PongWait time.Duration
	// WriteTimeout bounds a single WriteMessage call.
	WriteTimeout time.Duration
	// CloseGrace is how long the session waits for close handshakes before force-closing.
	CloseGrace time.Duration
	// ResponseDrain is how long the session keeps reading upstream for response.done after the
	// client hung up mid-response.
	ResponseDrain time.Duration
	// MaxMessageSize is the read limit for both sockets.
	MaxMessageSize int64
	// WriteQueueFrames is the number of frames one socket writer buffers before producers block.
	WriteQueueFrames int
	// WriteQueueBytes is the byte budget of one socket writer queue; it is always at least
	// MaxMessageSize + 4MiB so that one maximum-size frame fits.
	WriteQueueBytes int64
	// GateTimeout bounds the dialect initialisation phase during which client frames are queued.
	GateTimeout time.Duration
	// MaxSessions caps concurrent relay sessions (WebSocket sessions, sidebands and call monitors).
	MaxSessions int
	// MaxSessionsPerKey caps concurrent relay sessions per API key.
	MaxSessionsPerKey int
	// CallBindingTTL is how long a WebRTC call binding is remembered.
	CallBindingTTL time.Duration
	// CallBindingsPerProject caps remembered WebRTC calls per project; beyond it the oldest
	// binding of the project is evicted. It must stay above the number of calls a project can
	// start within CallBindingTTL, or sidebands of live calls start answering 404.
	CallBindingsPerProject int
	// DefaultTranscriptionModel is used for intent=transcription sessions without a model.
	DefaultTranscriptionModel string
	// MonitorCalls enables the gateway-owned usage monitor for OpenAI-family WebRTC calls.
	MonitorCalls bool
	// ForwardAttestation forwards the Codex x-oai-attestation header to the Codex backend.
	ForwardAttestation bool
	// QuotaCheckInterval is the period of the in-session quota/key/channel re-check.
	QuotaCheckInterval time.Duration
	// CallBodyLimit caps the WebRTC call-create request body.
	CallBodyLimit int64
}

const (
	defaultConnectBudget      = 8 * time.Second
	defaultHandshakeTimeout   = 5 * time.Second
	defaultIdleTimeout        = 10 * time.Minute
	defaultPingInterval       = 30 * time.Second
	defaultWriteTimeout       = 10 * time.Second
	defaultCloseGrace         = 4 * time.Second
	defaultResponseDrain      = 5 * time.Second
	defaultMaxMessageSize     = 16 << 20
	defaultWriteQueueFrames   = 16
	defaultGateTimeout        = 10 * time.Second
	defaultMaxSessions        = 256
	defaultMaxSessionsPerKey  = 8
	defaultCallBindingTTL     = 2 * time.Hour
	defaultBindingsPerProject = 1024
	defaultTranscriptionModel = "gpt-4o-transcribe"
	defaultQuotaCheckInterval = 30 * time.Second
	defaultCallBodyLimit      = 1 << 20

	// DefaultOpenAISessionDuration mirrors OpenAI's 60 minute session cap.
	DefaultOpenAISessionDuration = 60 * time.Minute
	// DefaultBailianSessionDuration ends the session just before Bailian's 120 minute cut so
	// that the normal timeout is recorded as completed rather than failed.
	DefaultBailianSessionDuration = 119 * time.Minute
)

// LoadConfig reads the AXONHUB_REALTIME_* environment variables and applies defaults.
func LoadConfig() Config {
	cfg := Config{
		ConnectBudget:             envDuration("AXONHUB_REALTIME_CONNECT_BUDGET", defaultConnectBudget),
		HandshakeTimeout:          envDuration("AXONHUB_REALTIME_HANDSHAKE_TIMEOUT", defaultHandshakeTimeout),
		MaxSessionDuration:        envDuration("AXONHUB_REALTIME_MAX_SESSION_DURATION", 0),
		IdleTimeout:               envDuration("AXONHUB_REALTIME_IDLE_TIMEOUT", defaultIdleTimeout),
		PingInterval:              envDuration("AXONHUB_REALTIME_PING_INTERVAL", defaultPingInterval),
		WriteTimeout:              envDuration("AXONHUB_REALTIME_WRITE_TIMEOUT", defaultWriteTimeout),
		CloseGrace:                envDuration("AXONHUB_REALTIME_CLOSE_GRACE", defaultCloseGrace),
		ResponseDrain:             envDuration("AXONHUB_REALTIME_RESPONSE_DRAIN", defaultResponseDrain),
		MaxMessageSize:            envInt64("AXONHUB_REALTIME_MAX_MESSAGE_SIZE", defaultMaxMessageSize),
		WriteQueueFrames:          envInt("AXONHUB_REALTIME_WRITE_QUEUE_FRAMES", defaultWriteQueueFrames),
		GateTimeout:               envDuration("AXONHUB_REALTIME_GATE_TIMEOUT", defaultGateTimeout),
		MaxSessions:               envInt("AXONHUB_REALTIME_MAX_SESSIONS", defaultMaxSessions),
		MaxSessionsPerKey:         envInt("AXONHUB_REALTIME_MAX_SESSIONS_PER_KEY", defaultMaxSessionsPerKey),
		CallBindingTTL:            envDuration("AXONHUB_REALTIME_CALL_BINDING_TTL", defaultCallBindingTTL),
		CallBindingsPerProject:    envInt("AXONHUB_REALTIME_CALL_BINDINGS_PER_PROJECT", defaultBindingsPerProject),
		DefaultTranscriptionModel: envString("AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL", defaultTranscriptionModel),
		MonitorCalls:              envBool("AXONHUB_REALTIME_MONITOR_CALLS", true),
		ForwardAttestation:        envBool("AXONHUB_CODEX_LIVE_FORWARD_ATTESTATION", true),
		QuotaCheckInterval:        envDuration("AXONHUB_REALTIME_QUOTA_CHECK_INTERVAL", defaultQuotaCheckInterval),
		CallBodyLimit:             envInt64("AXONHUB_REALTIME_CALL_BODY_LIMIT", defaultCallBodyLimit),
	}

	return cfg.withDefaults()
}

// withDefaults fills zero values and derived settings.
func (c Config) withDefaults() Config {
	if c.ConnectBudget <= 0 {
		c.ConnectBudget = defaultConnectBudget
	}

	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}

	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultIdleTimeout
	}

	if c.PingInterval <= 0 {
		c.PingInterval = defaultPingInterval
	}

	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}

	if c.PongWait <= 0 {
		c.PongWait = 2*c.PingInterval + c.WriteTimeout
	}

	if c.CloseGrace <= 0 {
		c.CloseGrace = defaultCloseGrace
	}

	if c.ResponseDrain <= 0 {
		c.ResponseDrain = defaultResponseDrain
	}

	if c.MaxMessageSize <= 0 {
		c.MaxMessageSize = defaultMaxMessageSize
	}

	if c.WriteQueueFrames <= 0 {
		c.WriteQueueFrames = defaultWriteQueueFrames
	}

	if minBytes := c.MaxMessageSize + 4<<20; c.WriteQueueBytes < minBytes {
		c.WriteQueueBytes = minBytes
	}

	if c.GateTimeout <= 0 {
		c.GateTimeout = defaultGateTimeout
	}

	if c.MaxSessions <= 0 {
		c.MaxSessions = defaultMaxSessions
	}

	if c.MaxSessionsPerKey <= 0 {
		c.MaxSessionsPerKey = defaultMaxSessionsPerKey
	}

	if c.CallBindingTTL <= 0 {
		c.CallBindingTTL = defaultCallBindingTTL
	}

	if c.CallBindingsPerProject <= 0 {
		c.CallBindingsPerProject = defaultBindingsPerProject
	}

	if c.DefaultTranscriptionModel == "" {
		c.DefaultTranscriptionModel = defaultTranscriptionModel
	}

	if c.QuotaCheckInterval <= 0 {
		c.QuotaCheckInterval = defaultQuotaCheckInterval
	}

	if c.CallBodyLimit <= 0 {
		c.CallBodyLimit = defaultCallBodyLimit
	}

	return c
}

// SessionDuration returns the effective maximum session duration for a dialect.
func (c Config) SessionDuration(dialectName string) time.Duration {
	if c.MaxSessionDuration > 0 {
		return c.MaxSessionDuration
	}

	if dialectName == "bailian" {
		return DefaultBailianSessionDuration
	}

	return DefaultOpenAISessionDuration
}

func envString(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}

	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}

	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}

	return def
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}

	if n, err := strconv.Atoi(v); err == nil {
		return n
	}

	return def
}

func envInt64(name string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}

	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}

	return def
}

func envBool(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}

	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}

	return def
}
