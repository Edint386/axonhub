package bailian

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// renames maps Bailian beta-era event names to their GA names.
var renames = map[string]string{
	"response.audio.delta":            "response.output_audio.delta",
	"response.audio.done":             "response.output_audio.done",
	"response.audio_transcript.delta": "response.output_audio_transcript.delta",
	"response.audio_transcript.done":  "response.output_audio_transcript.done",
	"response.text.delta":             "response.output_text.delta",
	"response.text.done":              "response.output_text.done",
}

// errorParamRewrites maps Bailian error.param prefixes back to GA session paths, longest
// prefix first.
var errorParamRewrites = [][2]string{
	{"session.audio.input.format.sample_rate", "session.audio.input.format.rate"},
	{"session.audio.output.format.sample_rate", "session.audio.output.format.rate"},
	{"session.input_audio_transcription", "session.audio.input.transcription"},
	{"session.turn_detection", "session.audio.input.turn_detection"},
	{"session.modalities", "session.output_modalities"},
	{"session.max_tokens", "session.max_output_tokens"},
	{"session.voice", "session.audio.output.voice"},
}

// UpstreamFrame implements dialect.Dialect for frames received from Bailian.
func (d *Dialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	typ := dialect.EventType(frame)

	if ga, ok := renames[typ]; ok {
		// Fast path for audio and text deltas: rename only, the payload is untouched.
		out, err := sjson.SetBytes(frame, "type", ga)
		if err != nil {
			return dialect.Output{}, fmt.Errorf("bailian: rename %s: %w", typ, err)
		}

		return d.hold(dialect.Output{ToClient: [][]byte{out}}), nil
	}

	var (
		out dialect.Output
		err error
	)

	switch typ {
	case "session.created":
		out, err = d.upstreamSessionCreated(frame)
	case "session.updated":
		out, err = d.upstreamSessionUpdated()
	case "error":
		out, err = d.upstreamError(frame)
	case "response.created":
		out, err = d.upstreamResponseCreated(frame)
	case "response.done":
		out, err = d.upstreamResponseDone(frame)
	case "response.output_item.added":
		out, err = d.upstreamOutputItemAdded(frame)
	case "response.output_item.done":
		out, err = d.upstreamOutputItemDone(frame)
	case "response.content_part.added", "response.content_part.done":
		out = dialect.Output{ToClient: [][]byte{normalizeContentPart(frame)}}
	case "conversation.item.created":
		out = d.upstreamItemCreated(frame)
	case "conversation.item.input_audio_transcription.delta":
		out = d.upstreamTranscriptionDelta(frame)
	case "conversation.item.input_audio_transcription.completed":
		out, err = d.upstreamTranscriptionCompleted(frame)
	case "conversation.item.deleted":
		d.items.remove(gjson.GetBytes(frame, "item_id").String())
		out = dialect.Output{ToClient: [][]byte{frame}}
	case "input_audio_buffer.committed":
		itemID := gjson.GetBytes(frame, "item_id").String()
		d.itemBytes[itemID] = d.appendedBytes
		d.appendedBytes = 0
		out = dialect.Output{ToClient: [][]byte{frame}}
	case "input_audio_buffer.cleared":
		// The client-side clear already reset the resampler and counters (in FIFO order with
		// later appends); resetting again here would wipe audio appended since.
		out = dialect.Output{ToClient: [][]byte{frame}}
	case "session.finished":
		// Bailian-only acknowledgement of session.finish; GA has no equivalent.
		out = dialect.Output{}
	default:
		out = dialect.Output{ToClient: [][]byte{frame}}
	}

	if err != nil {
		return dialect.Output{}, err
	}

	return d.hold(out), nil
}

// hold buffers client-bound frames until the GA session.created has been rendered so the
// client never sees an event before it.
func (d *Dialect) hold(out dialect.Output) dialect.Output {
	if d.initState == initDone || d.initState == initFailed || len(out.ToClient) == 0 {
		return out
	}

	// Errors are never held: GA allows an error before session.created, and an upstream
	// that fails during initialisation must not leave the client with a bare close.
	var pass [][]byte

	for _, frame := range out.ToClient {
		if dialect.EventType(frame) == "error" {
			pass = append(pass, frame)
			continue
		}

		d.held = append(d.held, frame)
	}

	out.ToClient = pass

	return out
}

// upstreamSessionCreated seeds the effective config and sends the init session.update.
func (d *Dialect) upstreamSessionCreated(frame []byte) (dialect.Output, error) {
	if d.initState != initWaitCreated {
		log.Debug(d.ctx, "bailian: dropping repeated session.created")

		return dialect.Output{}, nil
	}

	d.committed = seedConfig(gjson.GetBytes(frame, "session"), d.opts.WantTranscription, d.caps)
	d.committed.modalities = []string{"text", "audio"}
	d.initState = initPending

	d.pending = append(d.pending, pendingUpdate{
		upstreamEventID: initEventID1,
		cfg:             d.committed.clone(),
		init:            true,
	})

	return dialect.Output{ToUpstream: [][]byte{d.initUpdate(initEventID1, false)}}, nil
}

// upstreamSessionUpdated commits the oldest pending update. The init ack is suppressed
// and replaced by the single GA session.created; client updates get a GA session.updated.
func (d *Dialect) upstreamSessionUpdated() (dialect.Output, error) {
	if len(d.pending) == 0 {
		log.Debug(d.ctx, "bailian: dropping unsolicited session.updated")

		return dialect.Output{}, nil
	}

	p := d.pending[0]
	d.pending = d.pending[1:]
	d.committed = p.cfg

	if !p.init {
		return d.renderSessionEvent("session.updated", d.committed)
	}

	d.initState = initDone

	out, err := d.renderSessionEvent("session.created", d.committed)
	if err != nil {
		return dialect.Output{}, err
	}

	out.ToClient = append(out.ToClient, d.held...)
	d.held = nil
	out.OpenGate = true

	return out, nil
}

// upstreamError correlates an error with pending updates/deletes, rolls back and rewrites
// Bailian parameter paths to GA ones.
func (d *Dialect) upstreamError(frame []byte) (dialect.Output, error) {
	ev := gjson.ParseBytes(frame)
	errEventID := ev.Get("error.event_id").String()
	param := ev.Get("error.param").String()

	idx := -1

	if errEventID != "" {
		idx = lo.IndexOf(lo.Map(d.pending, func(p pendingUpdate, _ int) string { return p.upstreamEventID }), errEventID)
	}

	if idx < 0 && len(d.pending) > 0 && strings.HasPrefix(param, "session.") {
		idx = 0
	}

	if idx >= 0 {
		p := d.pending[idx]
		d.pending = append(d.pending[:idx], d.pending[idx+1:]...)

		if p.init {
			return d.initFailed(ev)
		}

		out := gaError(frame, param)

		if p.clientEventID != "" {
			out, _ = sjson.SetBytes(out, "error.event_id", p.clientEventID)
		} else if errEventID != "" {
			out, _ = sjson.DeleteBytes(out, "error.event_id")
		}

		return dialect.Output{ToClient: [][]byte{out}}, nil
	}

	if del, ok := d.pendingDeletes[errEventID]; ok {
		delete(d.pendingDeletes, errEventID)
		d.items.remove(del.itemID)

		log.Debug(d.ctx, "bailian: upstream rejected conversation.item.delete; acknowledging locally",
			log.String("item_id", del.itemID), log.String("message", ev.Get("error.message").String()))

		return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
			"type":    "conversation.item.deleted",
			"item_id": del.itemID,
		})}}, nil
	}

	out := gaError(frame, param)
	if strings.HasPrefix(errEventID, "axh_") {
		out, _ = sjson.DeleteBytes(out, "error.event_id")
	}

	return dialect.Output{ToClient: [][]byte{out}}, nil
}

// gaError rewrites a forwarded Bailian error for GA clients: the parameter path is mapped
// back to the GA session path and a missing error.type (Bailian's COMMON_ERROR frames carry
// only code and message) defaults to server_error, which GA clients expect to be present.
func gaError(frame []byte, param string) []byte {
	out := rewriteErrorParam(frame, param)

	if typ := gjson.GetBytes(out, "error.type"); typ.Type != gjson.String || typ.String() == "" {
		if withType, err := sjson.SetBytes(out, "error.type", "server_error"); err == nil {
			out = withType
		}
	}

	return out
}

// initFailed retries the init once with the minimal update, then gives up.
func (d *Dialect) initFailed(ev gjson.Result) (dialect.Output, error) {
	message := ev.Get("error.message").String()

	if !d.initRetried {
		d.initRetried = true

		log.Warn(d.ctx, "bailian: init session.update rejected; retrying with minimal update",
			log.String("model", d.opts.ActualModel), log.String("message", message), log.String("param", ev.Get("error.param").String()))

		d.pending = append(d.pending, pendingUpdate{
			upstreamEventID: initEventID2,
			cfg:             d.committed.clone(),
			init:            true,
		})

		return dialect.Output{ToUpstream: [][]byte{d.initUpdate(initEventID2, true)}}, nil
	}

	d.initState = initFailed

	log.Warn(d.ctx, "bailian: init session.update rejected twice; closing session",
		log.String("model", d.opts.ActualModel), log.String("message", message))

	toClient := append([][]byte{}, d.held...)
	d.held = nil

	toClient = append(toClient, dialect.ErrorEvent(d.ids, "server_error", "upstream_init_failed",
		"Upstream session initialisation failed: "+message, "", ""))

	return dialect.Output{
		ToClient: toClient,
		Close:    &dialect.CloseRequest{Code: 1011, Reason: "upstream session init failed"},
	}, nil
}

// rewriteErrorParam maps a Bailian error.param path back to the GA path.
func rewriteErrorParam(frame []byte, param string) []byte {
	if param == "" {
		return frame
	}

	for _, r := range errorParamRewrites {
		if strings.HasPrefix(param, r[0]) {
			rest := param[len(r[0]):]
			if rest == "" || rest[0] == '.' || rest[0] == '[' {
				out, err := sjson.SetBytes(frame, "error.param", r[1]+rest)
				if err == nil {
					return out
				}
			}

			break
		}
	}

	return frame
}

// upstreamResponseCreated reshapes the response object and marks a response in progress.
func (d *Dialect) upstreamResponseCreated(frame []byte) (dialect.Output, error) {
	d.activeMetadata = d.pendingMetadata
	d.pendingMetadata = nil

	out, err := d.reshapeResponse(frame)
	if err != nil {
		return dialect.Output{}, err
	}

	return dialect.Output{ToClient: [][]byte{out}, ResponseInProgress: lo.ToPtr(true)}, nil
}

// upstreamResponseDone reshapes the final response and extracts its usage.
func (d *Dialect) upstreamResponseDone(frame []byte) (dialect.Output, error) {
	out, err := d.reshapeResponse(frame)
	if err != nil {
		return dialect.Output{}, err
	}

	d.activeMetadata = nil

	resp := gjson.GetBytes(frame, "response")

	record := dialect.UsageRecord{
		Kind:       "response",
		ResponseID: resp.Get("id").String(),
		Usage:      dialect.GAUsage(resp.Get("usage")),
		Summary:    rawCopy(gjson.GetBytes(out, "response")),
	}

	return dialect.Output{
		ToClient:           [][]byte{out},
		Usage:              []dialect.UsageRecord{record},
		ResponseInProgress: lo.ToPtr(false),
	}, nil
}

// reshapeResponse converts the Bailian response object inside a response.created/done
// frame to the GA shape.
func (d *Dialect) reshapeResponse(frame []byte) ([]byte, error) {
	resp := gjson.GetBytes(frame, "response")
	if !resp.IsObject() {
		return frame, nil
	}

	voice := resp.Get("voice").String()
	if voice == "" {
		voice = d.committed.voice
	}

	hasAudio := d.committed.hasAudio()
	if mods := resp.Get("modalities"); mods.IsArray() {
		hasAudio = lo.ContainsBy(mods.Array(), func(m gjson.Result) bool { return m.String() == "audio" })
	}

	out := frame

	for _, key := range []string{"response.voice", "response.output_audio_format", "response.modalities"} {
		out, _ = sjson.DeleteBytes(out, key)
	}

	modalities := []string{"text"}
	if hasAudio {
		modalities = []string{"audio"}
	}

	audioOut := map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": clientRate}}
	if voice != "" {
		audioOut["voice"] = voice
	}

	var maxTokens any = "inf"
	if d.committed.maxTokens > 0 {
		maxTokens = d.committed.maxTokens
	}

	var err error

	for _, kv := range []struct {
		key   string
		value any
	}{
		{"response.output_modalities", modalities},
		{"response.audio", map[string]any{"output": audioOut}},
		{"response.max_output_tokens", maxTokens},
	} {
		out, err = sjson.SetBytes(out, kv.key, kv.value)
		if err != nil {
			return nil, fmt.Errorf("bailian: reshape response %s: %w", kv.key, err)
		}
	}

	if d.activeMetadata != nil {
		out, _ = sjson.SetRawBytes(out, "response.metadata", d.activeMetadata)
	}

	if usage := resp.Get("usage"); usage.IsObject() {
		out, _ = sjson.SetRawBytes(out, "response.usage", convertUsage(usage))
	}

	if output := resp.Get("output"); output.IsArray() {
		for i, item := range output.Array() {
			out, _ = sjson.SetRawBytes(out, "response.output."+itoa(i), normalizeItem([]byte(item.Raw)))
		}
	}

	return out, nil
}

// convertUsage maps Bailian's plural *_tokens_details usage to the GA singular shape,
// padding the cached/image counters GA clients expect.
func convertUsage(u gjson.Result) []byte {
	in := u.Get("input_tokens_details")
	if !in.Exists() {
		in = u.Get("input_token_details")
	}

	out := u.Get("output_tokens_details")
	if !out.Exists() {
		out = u.Get("output_token_details")
	}

	inputTokens := u.Get("input_tokens").Int()
	outputTokens := u.Get("output_tokens").Int()

	total := u.Get("total_tokens").Int()
	if total == 0 {
		total = inputTokens + outputTokens
	}

	b, _ := json.Marshal(map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  total,
		"input_token_details": map[string]any{
			"text_tokens":   in.Get("text_tokens").Int(),
			"audio_tokens":  in.Get("audio_tokens").Int(),
			"image_tokens":  in.Get("image_tokens").Int(),
			"cached_tokens": in.Get("cached_tokens").Int(),
			"cached_tokens_details": map[string]any{
				"text_tokens":  0,
				"audio_tokens": 0,
				"image_tokens": 0,
			},
		},
		"output_token_details": map[string]any{
			"text_tokens":  out.Get("text_tokens").Int(),
			"audio_tokens": out.Get("audio_tokens").Int(),
		},
	})

	return b
}

// upstreamOutputItemAdded records response provenance and normalises the item.
func (d *Dialect) upstreamOutputItemAdded(frame []byte) (dialect.Output, error) {
	item := gjson.GetBytes(frame, "item")
	if !item.IsObject() {
		return dialect.Output{ToClient: [][]byte{frame}}, nil
	}

	d.responseItems[item.Get("id").String()] = true

	out, err := sjson.SetRawBytes(frame, "item", normalizeItem([]byte(item.Raw)))
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: normalize output item: %w", err)
	}

	return dialect.Output{ToClient: [][]byte{out}}, nil
}

// upstreamOutputItemDone forwards the normalised item and emits conversation.item.done.
func (d *Dialect) upstreamOutputItemDone(frame []byte) (dialect.Output, error) {
	item := gjson.GetBytes(frame, "item")
	if !item.IsObject() {
		return dialect.Output{ToClient: [][]byte{frame}}, nil
	}

	id := item.Get("id").String()
	normalized := normalizeItem([]byte(item.Raw))

	out, err := sjson.SetRawBytes(frame, "item", normalized)
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: normalize output item: %w", err)
	}

	d.items.put(id, normalized)

	toClient := [][]byte{out}

	if !d.doneEmitted[id] {
		d.doneEmitted[id] = true
		toClient = append(toClient, d.synth(map[string]any{
			"type": "conversation.item.done",
			"item": json.RawMessage(normalized),
		}))
	}

	return dialect.Output{ToClient: toClient}, nil
}

// upstreamItemCreated replaces conversation.item.created with GA added/done events,
// classified by provenance: items the model is generating get their done on
// response.output_item.done, everything else is complete immediately.
func (d *Dialect) upstreamItemCreated(frame []byte) dialect.Output {
	ev := gjson.ParseBytes(frame)

	item := ev.Get("item")
	if !item.IsObject() {
		return dialect.Output{ToClient: [][]byte{frame}}
	}

	id := item.Get("id").String()
	normalized := normalizeItem([]byte(item.Raw))

	fromResponse := d.responseItems[id] ||
		(!isAllInputContent(item) && item.Get("status").String() == "in_progress")

	added := map[string]any{
		"type": "conversation.item.added",
		"item": json.RawMessage(normalized),
	}

	// Bailian's previous_item_id on mcp_approval_request is the approved mcp_call, not the
	// conversation predecessor, so it is not a GA previous_item_id.
	if prev := ev.Get("previous_item_id"); prev.Type == gjson.String && item.Get("type").String() != "mcp_approval_request" {
		added["previous_item_id"] = prev.String()
	}

	toClient := [][]byte{d.synth(added)}

	if fromResponse {
		d.items.put(id, normalized)

		return dialect.Output{ToClient: toClient}
	}

	done := normalized
	if item.Get("type").String() == "message" || item.Get("status").Exists() {
		done, _ = sjson.SetBytes(done, "status", "completed")
	}

	d.items.put(id, done)
	d.doneEmitted[id] = true

	toClient = append(toClient, d.synth(map[string]any{
		"type": "conversation.item.done",
		"item": json.RawMessage(done),
	}))

	return dialect.Output{ToClient: toClient}
}

// upstreamTranscriptionDelta turns Bailian's confirmed-prefix text into a GA delta.
func (d *Dialect) upstreamTranscriptionDelta(frame []byte) dialect.Output {
	ev := gjson.ParseBytes(frame)
	itemID := ev.Get("item_id").String()

	tr := d.transcripts[itemID]
	if tr == nil {
		tr = &transcriptTracker{}
		d.transcripts[itemID] = tr
	}

	delta := tr.delta(ev.Get("text").String())
	if delta == "" {
		return dialect.Output{}
	}

	out := map[string]any{
		"event_id":      ev.Get("event_id").String(),
		"type":          "conversation.item.input_audio_transcription.delta",
		"item_id":       itemID,
		"content_index": ev.Get("content_index").Int(),
		"delta":         delta,
	}

	if out["event_id"] == "" {
		out["event_id"] = d.ids.Next()
	}

	b, _ := json.Marshal(out)

	return dialect.Output{ToClient: [][]byte{b}}
}

// upstreamTranscriptionCompleted emits any catch-up delta, adds the GA usage object and
// produces the transcription usage record.
func (d *Dialect) upstreamTranscriptionCompleted(frame []byte) (dialect.Output, error) {
	ev := gjson.ParseBytes(frame)
	itemID := ev.Get("item_id").String()
	transcript := ev.Get("transcript").String()
	contentIndex := int(ev.Get("content_index").Int())

	var toClient [][]byte

	// Without any prior delta (short utterances) the whole transcript is the catch-up delta,
	// so delta-driven GA clients still see the text.
	tr := d.transcripts[itemID]
	if tr == nil {
		tr = &transcriptTracker{}
	}

	if catch := tr.complete(transcript); catch != "" {
		toClient = append(toClient, d.synth(map[string]any{
			"type":          "conversation.item.input_audio_transcription.delta",
			"item_id":       itemID,
			"content_index": contentIndex,
			"delta":         catch,
		}))
	}

	delete(d.transcripts, itemID)

	seconds := audio.ByteLenToSeconds(int(d.itemBytes[itemID]), clientRate)
	delete(d.itemBytes, itemID)

	out, err := sjson.SetBytes(frame, "usage", map[string]any{"type": "duration", "seconds": seconds})
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: add transcription usage: %w", err)
	}

	toClient = append(toClient, out)
	d.items.setTranscript(itemID, contentIndex, transcript)

	return dialect.Output{
		ToClient: toClient,
		Usage: []dialect.UsageRecord{{
			Kind:            "transcription",
			ItemID:          itemID,
			Model:           d.caps.TranscriptionModel,
			DurationSeconds: seconds,
			Summary:         append(json.RawMessage(nil), out...),
		}},
	}, nil
}
