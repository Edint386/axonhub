// Package dialect translates between the OpenAI Realtime GA wire protocol spoken by clients
// and the protocol spoken by a given upstream (OpenAI itself, or a provider dialect such as
// Alibaba Bailian Qwen-Omni-Realtime).
//
// A Dialect is owned by exactly one relay session. Its two entry points are called from two
// different goroutines (the client reader pump and the upstream reader pump), so every
// implementation must guard its internal state with a mutex.
package dialect

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
)

// Output is the result of translating one WebSocket frame.
//
// Frames in ToUpstream are written to the upstream socket in order; frames in ToClient are
// written to the client socket in order. The session guarantees strict FIFO per direction
// across all producers, so a dialect may synthesize client-bound events from either entry
// point.
type Output struct {
	ToUpstream [][]byte
	ToClient   [][]byte

	// Usage carries usage records extracted from the frame (normally from upstream frames).
	Usage []UsageRecord

	// OpenGate is set by the dialect once the upstream session is configured and queued
	// client frames may be flushed. It is only meaningful while the session gate is closed
	// (see Dialect.Start).
	OpenGate bool

	// Close asks the session to end with the given close code after the frames above have
	// been written. Dialects use it for unsupported configurations (e.g. G.711 audio).
	Close *CloseRequest

	// ResponseInProgress reports a change of the upstream response state: true after
	// response.created, false after response.done. The session uses it to decide whether to
	// wait for usage when the client hangs up mid-response.
	ResponseInProgress *bool
}

// UsageRecord is one billable unit extracted from the upstream stream.
type UsageRecord struct {
	// Kind is "response" (response.done) or "transcription"
	// (conversation.item.input_audio_transcription.completed).
	Kind string
	// ResponseID is the upstream response id for Kind == "response"; used for de-duplication.
	ResponseID string
	// ItemID is the conversation item id for Kind == "transcription".
	ItemID string
	// Model overrides the billed model (transcription model); empty means the session model.
	Model string
	// Usage is the token usage; nil for duration-only transcription usage.
	Usage *llm.Usage
	// DurationSeconds is set for transcription usage of type "duration".
	DurationSeconds float64
	// Summary is a compact JSON document describing the unit (no audio payloads). It is
	// persisted as the request body of the per-turn request row.
	Summary json.RawMessage
}

// CloseRequest asks the session to close both sockets.
type CloseRequest struct {
	Code   int
	Reason string
}

// Dialect translates frames between the GA client protocol and the upstream protocol.
//
// All methods must be safe for concurrent use: ClientFrame runs on the client reader
// goroutine and UpstreamFrame on the upstream reader goroutine.
type Dialect interface {
	// Name returns the dialect name used in logs and metadata ("ga", "bailian").
	Name() string

	// Start is called once, after the upstream socket is connected and the client socket is
	// upgraded, before any client frame is forwarded. It returns frames to send upstream
	// (e.g. a provider-specific initial session.update) and whether the session gate must
	// stay closed until the dialect reports OpenGate from UpstreamFrame. While the gate is
	// closed the session queues client frames and does not call ClientFrame.
	Start() (toUpstream [][]byte, gated bool)

	// ClientFrame translates one frame received from the client.
	ClientFrame(frame []byte) (Output, error)

	// UpstreamFrame translates one frame received from the upstream.
	UpstreamFrame(frame []byte) (Output, error)

	// CloseUpstream returns frames to send upstream right before the session closes the
	// upstream socket (e.g. Bailian's session.finish). It may return nil.
	CloseUpstream() [][]byte
}

// EventType returns the "type" field of a JSON event without unmarshalling the frame.
func EventType(frame []byte) string {
	return gjson.GetBytes(frame, "type").String()
}

// EventIDs generates ids for synthesized server events ("event_axh_<n>").
type EventIDs struct {
	n atomic.Uint64
}

// Next returns the next synthetic event id.
func (e *EventIDs) Next() string {
	return fmt.Sprintf("event_axh_%d", e.n.Add(1))
}

// ErrorEvent builds a GA error event. eventID is the client event id that caused the error
// and may be empty.
func ErrorEvent(ids *EventIDs, errType, code, message, param, eventID string) []byte {
	type errBody struct {
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message"`
		Param   string `json:"param,omitempty"`
		EventID string `json:"event_id,omitempty"`
	}

	type errEvent struct {
		Type    string  `json:"type"`
		EventID string  `json:"event_id"`
		Error   errBody `json:"error"`
	}

	if errType == "" {
		errType = "invalid_request_error"
	}

	payload, _ := json.Marshal(errEvent{
		Type:    "error",
		EventID: ids.Next(),
		Error: errBody{
			Type:    errType,
			Code:    code,
			Message: message,
			Param:   param,
			EventID: eventID,
		},
	})

	return payload
}

// GAUsage converts a GA response usage object (response.usage) into llm.Usage. It accepts
// both the GA singular spelling (input_token_details) and the plural spelling used by
// Bailian (input_tokens_details).
func GAUsage(usage gjson.Result) *llm.Usage {
	if !usage.Exists() || !usage.IsObject() {
		return nil
	}

	in := usage.Get("input_token_details")
	if !in.Exists() {
		in = usage.Get("input_tokens_details")
	}

	out := usage.Get("output_token_details")
	if !out.Exists() {
		out = usage.Get("output_tokens_details")
	}

	u := &llm.Usage{
		PromptTokens:     usage.Get("input_tokens").Int(),
		CompletionTokens: usage.Get("output_tokens").Int(),
		TotalTokens:      usage.Get("total_tokens").Int(),
	}

	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}

	if in.Exists() {
		u.PromptTokensDetails = &llm.PromptTokensDetails{
			AudioTokens:  in.Get("audio_tokens").Int(),
			CachedTokens: in.Get("cached_tokens").Int(),
		}
	}

	if out.Exists() {
		u.CompletionTokensDetails = &llm.CompletionTokensDetails{
			AudioTokens: out.Get("audio_tokens").Int(),
		}
	}

	return u
}
