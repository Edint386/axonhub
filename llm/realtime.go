package llm

// Realtime constants live in their own file so the custom realtime relay does not touch
// the upstream-owned constants.go.

const (
	// RequestTypeRealtime is the OpenAI Realtime API session request type. It is used for
	// channel selection by the realtime relay (GET /v1/realtime, POST /v1/realtime/calls,
	// POST /v1/live) and never reaches the chat pipeline.
	RequestTypeRealtime RequestType = "realtime"
)

const (
	// APIFormatOpenAIRealtime identifies the OpenAI Realtime API (WebSocket sessions and
	// WebRTC call signalling) for channel endpoint configuration and persisted
	// request/execution/usage metadata. Requests with this format are served by the
	// realtime relay, not by the transformer pipeline.
	APIFormatOpenAIRealtime APIFormat = "openai/realtime"
)
