package dialect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Event types the GA passthrough dialect inspects. Every other frame is forwarded after the
// type read alone.
const (
	gaEventSessionUpdate          = "session.update"
	gaEventSessionCreated         = "session.created"
	gaEventSessionUpdated         = "session.updated"
	gaEventResponseCreated        = "response.created"
	gaEventResponseDone           = "response.done"
	gaEventTranscriptionCompleted = "conversation.item.input_audio_transcription.completed"
)

// gaPathSessionModel is the model field of a session frame. It is only rewritten when the
// frame already carries it: transcription sessions and Codex frameless events have no
// top-level model and must never gain one.
const gaPathSessionModel = "session.model"

// gaPathTranscriptionModel is where GA session frames keep the input transcription model.
const gaPathTranscriptionModel = "session.audio.input.transcription.model"

// GAOptions configures the GA passthrough dialect.
type GAOptions struct {
	// RequestedModel is the model name the client asked for. It is echoed back to the client
	// in session.created and session.updated frames.
	RequestedModel string
	// ActualModel is the upstream model name written into client session.update frames.
	ActualModel string
	// RewriteModel enables the model rewrite in both directions. Sideband and Codex variants
	// set it to false so that every frame passes through untouched.
	RewriteModel bool
	// IDs generates ids for synthesized server events (the error event emitted for a rejected
	// transcription model). Nil means a private generator.
	IDs *EventIDs
	// TranscriptionModelAllowed reports whether the client may bill transcription under model
	// (session.audio.input.transcription.model). Nil allows every model. A rejected
	// session.update is not forwarded; the client gets a GA error event instead.
	TranscriptionModelAllowed func(model string) bool
}

// Error event fields for a rejected transcription model.
const (
	gaErrorTypeInvalidRequest  = "invalid_request_error"
	gaErrorCodeModelNotAllowed = "model_not_allowed"
)

// gaDialect forwards GA frames unchanged apart from the model rewrite and usage extraction.
type gaDialect struct {
	opts GAOptions

	mu sync.Mutex
	// transcriptionModel is the last audio.input.transcription.model seen in a client
	// session.update or an upstream session.created/session.updated frame. It is attached
	// to transcription usage records so the session can bill the transcription model.
	transcriptionModel string
}

// NewGA returns the GA passthrough dialect.
func NewGA(opts GAOptions) Dialect {
	if opts.IDs == nil {
		opts.IDs = &EventIDs{}
	}

	return &gaDialect{opts: opts}
}

// Name implements Dialect.
func (d *gaDialect) Name() string {
	return "ga"
}

// Start implements Dialect. The GA upstream needs no initialisation and no gate.
func (d *gaDialect) Start() ([][]byte, bool) {
	return nil, false
}

// CloseUpstream implements Dialect. The GA upstream needs no farewell frame.
func (d *gaDialect) CloseUpstream() [][]byte {
	return nil
}

// ClientFrame implements Dialect. Only session.update is inspected beyond its type: its
// transcription model is checked against TranscriptionModelAllowed and remembered and, when
// enabled and present, its model is rewritten to the upstream model.
func (d *gaDialect) ClientFrame(frame []byte) (Output, error) {
	if EventType(frame) != gaEventSessionUpdate {
		return Output{ToUpstream: [][]byte{frame}}, nil
	}

	if rejected, ok := d.rejectTranscriptionModel(frame); ok {
		return Output{ToClient: [][]byte{rejected}}, nil
	}

	d.rememberTranscriptionModel(frame)

	if d.opts.RewriteModel {
		rewritten, err := gaRewriteModel(frame, d.opts.ActualModel)
		if err != nil {
			return Output{}, fmt.Errorf("ga: rewrite client session.model: %w", err)
		}

		frame = rewritten
	}

	return Output{ToUpstream: [][]byte{frame}}, nil
}

// UpstreamFrame implements Dialect. Frames pass through unchanged; session echoes get the
// requested model name back, response.created/response.done report the response state and
// response.done and transcription completions yield usage records.
func (d *gaDialect) UpstreamFrame(frame []byte) (Output, error) {
	out := Output{ToClient: [][]byte{frame}}

	switch EventType(frame) {
	case gaEventSessionCreated, gaEventSessionUpdated:
		d.rememberTranscriptionModel(frame)

		if d.opts.RewriteModel {
			rewritten, err := gaRewriteModel(frame, d.opts.RequestedModel)
			if err != nil {
				return Output{}, fmt.Errorf("ga: rewrite upstream session.model: %w", err)
			}

			out.ToClient = [][]byte{rewritten}
		}
	case gaEventResponseCreated:
		out.ResponseInProgress = lo.ToPtr(true)
	case gaEventResponseDone:
		out.ResponseInProgress = lo.ToPtr(false)

		if record, ok := gaResponseUsage(frame); ok {
			out.Usage = []UsageRecord{record}
		}
	case gaEventTranscriptionCompleted:
		if record, ok := d.transcriptionUsage(frame); ok {
			out.Usage = []UsageRecord{record}
		}
	}

	return out, nil
}

// TranscriptionModel returns session.audio.input.transcription.model from a session.update,
// session.created or session.updated frame. It returns "" when the field is absent.
func TranscriptionModel(sessionUpdate []byte) string {
	return gjson.GetBytes(sessionUpdate, gaPathTranscriptionModel).String()
}

// rejectTranscriptionModel returns the GA error event answering a client session.update whose
// transcription model TranscriptionModelAllowed rejects. It reports false when the frame carries
// no transcription model or the model is allowed.
func (d *gaDialect) rejectTranscriptionModel(frame []byte) ([]byte, bool) {
	if d.opts.TranscriptionModelAllowed == nil {
		return nil, false
	}

	model := TranscriptionModel(frame)
	if model == "" || d.opts.TranscriptionModelAllowed(model) {
		return nil, false
	}

	message := fmt.Sprintf("The transcription model %q is not allowed for this API key.", model)
	eventID := gjson.GetBytes(frame, "event_id").String()

	return ErrorEvent(d.opts.IDs, gaErrorTypeInvalidRequest, gaErrorCodeModelNotAllowed, message, gaPathTranscriptionModel, eventID), true
}

// rememberTranscriptionModel stores the transcription model carried by a session frame.
// Frames without a model leave the remembered value alone, because a partial session.update
// that does not mention transcription keeps the previous transcription configuration.
func (d *gaDialect) rememberTranscriptionModel(frame []byte) {
	model := TranscriptionModel(frame)
	if model == "" {
		return
	}

	d.mu.Lock()
	d.transcriptionModel = model
	d.mu.Unlock()
}

// currentTranscriptionModel returns the remembered transcription model, or "".
func (d *gaDialect) currentTranscriptionModel() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.transcriptionModel
}

// gaRewriteModel sets session.model to model when the frame already carries the field. It
// returns the frame itself otherwise, so frames without a model never gain one.
func gaRewriteModel(frame []byte, model string) ([]byte, error) {
	if !gjson.GetBytes(frame, gaPathSessionModel).Exists() {
		return frame, nil
	}

	rewritten, err := sjson.SetBytes(frame, gaPathSessionModel, model)
	if err != nil {
		return nil, fmt.Errorf("set %s: %w", gaPathSessionModel, err)
	}

	return rewritten, nil
}

// gaResponseUsage extracts the usage record of a response.done frame. It reports false when
// the frame carries no usage object.
func gaResponseUsage(frame []byte) (UsageRecord, bool) {
	response := gjson.GetBytes(frame, "response")

	usage := GAUsage(response.Get("usage"))
	if usage == nil {
		return UsageRecord{}, false
	}

	return UsageRecord{
		Kind:       "response",
		ResponseID: response.Get("id").String(),
		Usage:      usage,
		Summary:    gaResponseSummary(response),
	}, true
}

// gaResponseSummary copies the response object and removes every audio payload from
// output[].content[] so that the summary can be persisted without base64 audio.
func gaResponseSummary(response gjson.Result) json.RawMessage {
	summary := []byte(response.Raw)
	itemIndex := 0

	response.Get("output").ForEach(func(_, item gjson.Result) bool {
		partIndex := 0

		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("audio").Exists() {
				path := "output." + strconv.Itoa(itemIndex) + ".content." + strconv.Itoa(partIndex) + ".audio"

				if stripped, err := sjson.DeleteBytes(summary, path); err == nil {
					summary = stripped
				}
			}

			partIndex++

			return true
		})

		itemIndex++

		return true
	})

	return summary
}

// transcriptionUsage extracts the usage record of a
// conversation.item.input_audio_transcription.completed frame. Usage of type "duration" is
// reported as DurationSeconds without token usage; everything else is mapped like a GA
// response usage. It reports false when the frame carries no usage object.
func (d *gaDialect) transcriptionUsage(frame []byte) (UsageRecord, bool) {
	usage := gjson.GetBytes(frame, "usage")
	if !usage.IsObject() {
		return UsageRecord{}, false
	}

	record := UsageRecord{
		Kind:    "transcription",
		ItemID:  gjson.GetBytes(frame, "item_id").String(),
		Model:   d.currentTranscriptionModel(),
		Summary: json.RawMessage(bytes.Clone(frame)),
	}

	if usage.Get("type").String() == "duration" {
		record.DurationSeconds = usage.Get("seconds").Float()
	} else {
		record.Usage = GAUsage(usage)
	}

	return record, true
}
