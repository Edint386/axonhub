package bailian

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// ClientFrame implements dialect.Dialect for frames received from the GA client.
func (d *Dialect) ClientFrame(frame []byte) (dialect.Output, error) {
	if !gjson.ValidBytes(frame) {
		d.mu.Lock()
		defer d.mu.Unlock()

		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_json", "The client event is not a JSON object.", "", "")}}, nil
	}

	ev := gjson.ParseBytes(frame)
	if !ev.IsObject() {
		d.mu.Lock()
		defer d.mu.Unlock()

		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_json", "The client event is not a JSON object.", "", "")}}, nil
	}

	// Image decoding is CPU-heavy and must not stall the upstream pump: do it before locking.
	images := prepareImages(frame)

	d.mu.Lock()
	defer d.mu.Unlock()

	clientEventID := ev.Get("event_id").String()

	switch ev.Get("type").String() {
	case "session.update":
		return d.clientSessionUpdate(ev, clientEventID)
	case "input_audio_buffer.append":
		return d.clientAppend(ev, frame, clientEventID)
	case "input_audio_buffer.clear":
		d.resetAudio()

		return dialect.Output{ToUpstream: [][]byte{frame}}, nil
	case "input_audio_buffer.commit":
		d.bufferHasAudio = false

		return dialect.Output{ToUpstream: [][]byte{frame}}, nil
	case "conversation.item.create":
		return d.clientItemCreate(ev, frame, images, clientEventID)
	case "conversation.item.retrieve":
		return d.clientItemRetrieve(ev, clientEventID), nil
	case "conversation.item.delete":
		return d.clientItemDelete(ev, frame, clientEventID)
	case "conversation.item.truncate":
		return d.clientItemTruncate(ev), nil
	case "response.create":
		return d.clientResponseCreate(ev, clientEventID)
	case "output_audio_buffer.clear":
		// WebRTC-only on GA; nothing to do on a WebSocket relay.
		return dialect.Output{}, nil
	case "":
		return dialect.Output{ToClient: [][]byte{d.clientError("missing_required_parameter", "Missing required parameter: 'type'.", "type", clientEventID)}}, nil
	default:
		// input_audio_buffer.commit, response.cancel and unknown events pass through unchanged.
		return dialect.Output{ToUpstream: [][]byte{frame}}, nil
	}
}

// clientSessionUpdate merges the GA delta into the full upstream config and sends it whole.
func (d *Dialect) clientSessionUpdate(ev gjson.Result, clientEventID string) (dialect.Output, error) {
	session := ev.Get("session")
	if !session.IsObject() {
		return dialect.Output{ToClient: [][]byte{d.clientError("missing_required_parameter", "Missing required parameter: 'session'.", "session", clientEventID)}}, nil
	}

	base := d.latestConfig()

	cfg, dropped, rej := mergeSessionUpdate(base, session, d.caps)
	if rej != nil {
		return d.rejectionOutput(rej, clientEventID), nil
	}

	if len(dropped) > 0 {
		log.Debug(d.ctx, "bailian: dropped unsupported session fields", log.Strings("fields", dropped))
	}

	if cfg.equal(base) {
		// Nothing left after stripping adapter-owned fields: answer from the effective config.
		return d.renderSessionEvent("session.updated", cfg)
	}

	body, err := cfg.upstreamSession(d.caps, !d.audioStarted)
	if err != nil {
		return dialect.Output{}, err
	}

	upstreamEventID := d.upstreamID("axh_upd_")

	update, err := json.Marshal(map[string]any{
		"event_id": upstreamEventID,
		"type":     "session.update",
		"session":  json.RawMessage(body),
	})
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: marshal session.update: %w", err)
	}

	d.pending = append(d.pending, pendingUpdate{
		upstreamEventID: upstreamEventID,
		clientEventID:   clientEventID,
		cfg:             cfg,
	})

	return dialect.Output{ToUpstream: [][]byte{update}}, nil
}

// renderSessionEvent renders a GA session.created/session.updated from cfg.
func (d *Dialect) renderSessionEvent(eventType string, cfg sessionConfig) (dialect.Output, error) {
	session, err := cfg.renderGA(d.caps, d.opts.RequestedModel)
	if err != nil {
		return dialect.Output{}, err
	}

	frame := d.synth(map[string]any{
		"type":    eventType,
		"session": json.RawMessage(session),
	})

	return dialect.Output{ToClient: [][]byte{frame}}, nil
}

// clientAppend forwards input audio, resampling it to the upstream rate when needed. Audio is
// only marked as started once a frame actually reaches the upstream.
func (d *Dialect) clientAppend(ev gjson.Result, frame []byte, clientEventID string) (dialect.Output, error) {
	audioRaw := ev.Get("audio")

	if !d.caps.Resample {
		// Fast path: forward unchanged while the chunk is at most one upstream chunk long;
		// larger appends are split so the upstream never sees multi-second frames.
		rawLen := int64(len(audioRaw.Raw))
		if rawLen >= 2 {
			rawLen -= 2 // strip the JSON quotes
		}

		decoded := base64DecodedLenRaw(audioRaw.Raw)
		if decoded <= 0 {
			return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}, nil
		}

		if decoded <= int64(maxPassthroughAppend(d.caps.InputRate)) {
			d.appendedBytes += decoded
			d.markAudio()

			return dialect.Output{ToUpstream: [][]byte{frame}}, nil
		}

		pcm, err := base64.StdEncoding.DecodeString(audioRaw.String())
		if err != nil {
			return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}, nil
		}

		d.appendedBytes += int64(len(pcm))
		d.markAudio()

		return dialect.Output{ToUpstream: d.appendFrames(pcm, clientEventID)}, nil
	}

	pcm, err := base64.StdEncoding.DecodeString(audioRaw.String())
	if err != nil || len(pcm) == 0 {
		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}, nil
	}

	d.appendedBytes += int64(len(pcm))

	frames := d.appendFrames(d.resampler.Process(pcm), clientEventID)
	if len(frames) > 0 {
		d.markAudio()
	}

	return dialect.Output{ToUpstream: frames}, nil
}

// markAudio records that audio reached the upstream input buffer.
func (d *Dialect) markAudio() {
	d.audioStarted = true
	d.bufferHasAudio = true
}

// maxPassthroughAppend is the largest decoded append (bytes) forwarded without splitting:
// one upstreamChunk of 16-bit mono PCM at rate.
func maxPassthroughAppend(rate int) int {
	return rate * 2 * int(upstreamChunk.Milliseconds()) / 1000
}

// appendFrames splits upstream-rate PCM into ≤100 ms input_audio_buffer.append frames.
func (d *Dialect) appendFrames(pcm []byte, clientEventID string) [][]byte {
	chunks := audio.SplitPCM(pcm, d.caps.InputRate, upstreamChunk)
	frames := make([][]byte, 0, len(chunks))

	for i, c := range chunks {
		ev := map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(c),
		}

		if i == 0 && clientEventID != "" {
			ev["event_id"] = clientEventID
		}

		b, _ := json.Marshal(ev)
		frames = append(frames, b)
	}

	return frames
}

// clientItemCreate translates conversation.item.create. images holds the pre-converted
// input_image parts of a user message (see prepareImages).
func (d *Dialect) clientItemCreate(ev gjson.Result, frame []byte, images map[int]imageResult, clientEventID string) (dialect.Output, error) {
	item := ev.Get("item")
	if !item.IsObject() {
		return dialect.Output{ToClient: [][]byte{d.clientError("missing_required_parameter", "Missing required parameter: 'item'.", "item", clientEventID)}}, nil
	}

	out := frame
	if ev.Get("previous_item_id").Exists() {
		out, _ = sjson.DeleteBytes(out, "previous_item_id")
	}

	switch item.Get("type").String() {
	case "mcp_approval_response":
		if item.Get("reason").Exists() {
			out, _ = sjson.DeleteBytes(out, "item.reason")
		}

		return dialect.Output{ToUpstream: [][]byte{out}}, nil
	case "message":
		return d.clientMessageItem(item, out, images, clientEventID)
	default:
		// function_call_output and anything else is forwarded unchanged.
		return dialect.Output{ToUpstream: [][]byte{out}}, nil
	}
}

// clientMessageItem translates a message item by role.
func (d *Dialect) clientMessageItem(item gjson.Result, frame []byte, images map[int]imageResult, clientEventID string) (dialect.Output, error) {
	parts := item.Get("content").Array()

	switch item.Get("role").String() {
	case "assistant":
		content := make([]json.RawMessage, 0, len(parts))

		for _, p := range parts {
			switch p.Get("type").String() {
			case "output_text":
				b, _ := json.Marshal(map[string]any{"type": "text", "text": p.Get("text").String()})
				content = append(content, b)
			case "output_audio":
				b, _ := json.Marshal(map[string]any{"type": "audio", "text": p.Get("transcript").String()})
				content = append(content, b)
			default:
				content = append(content, rawCopy(p))
			}
		}

		out, err := sjson.SetBytes(frame, "item.content", content)
		if err != nil {
			return dialect.Output{}, fmt.Errorf("bailian: rewrite assistant item: %w", err)
		}

		return dialect.Output{ToUpstream: [][]byte{out}}, nil
	case "system":
		return dialect.Output{ToUpstream: [][]byte{frame}}, nil
	default:
		return d.clientUserMessageItem(item, frame, parts, images, clientEventID)
	}
}

// clientUserMessageItem handles user messages: text is forwarded as a message item, audio
// becomes append+commit in manual mode, images go through input_image_buffer.append.
//
// Bailian only attaches images to the audio buffer they are committed with, so images are
// emitted first, followed by the audio (or 100 ms of silence when the buffer is empty) and a
// commit, and only then the text message.
func (d *Dialect) clientUserMessageItem(item gjson.Result, frame []byte, parts []gjson.Result, images map[int]imageResult, clientEventID string) (dialect.Output, error) {
	if len(parts) == 0 {
		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Message items need at least one content part.", "item.content", clientEventID)}}, nil
	}

	var (
		out         dialect.Output
		keep        []json.RawMessage
		imageFrames [][]byte
		audioFrames [][]byte
		imageParts  int
	)

	for i, p := range parts {
		switch p.Get("type").String() {
		case "input_audio":
			if !d.latestConfig().manualMode() {
				return dialect.Output{ToClient: [][]byte{d.clientError(
					"unsupported_on_upstream",
					"input_audio content items require turn_detection to be null (manual mode) on the upstream model; stream audio with input_audio_buffer.append instead.",
					fmt.Sprintf("item.content[%d]", i),
					clientEventID,
				)}}, nil
			}

			pcm, err := base64.StdEncoding.DecodeString(p.Get("audio").String())
			if err != nil || len(pcm) == 0 {
				return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", fmt.Sprintf("item.content[%d].audio", i), clientEventID)}}, nil
			}

			d.appendedBytes += int64(len(pcm))

			if d.caps.Resample {
				// A fresh resampler per content part: the item's audio is self-contained.
				r, err := audio.NewResampler(clientRate, d.caps.InputRate)
				if err != nil {
					return dialect.Output{}, err
				}

				pcm = r.Process(pcm)
			}

			audioFrames = append(audioFrames, d.appendFrames(pcm, "")...)
		case "input_image":
			res, ok := images[i]
			if !ok {
				res = imageResult{err: fmt.Errorf("bailian: image part was not prepared")}
			}

			if res.err != nil {
				return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", res.err.Error(), fmt.Sprintf("item.content[%d].image_url", i), clientEventID)}}, nil
			}

			b, _ := json.Marshal(map[string]any{"type": "input_image_buffer.append", "image": res.b64})
			imageFrames = append(imageFrames, b)
			imageParts++
		default:
			keep = append(keep, rawCopy(p))
		}
	}

	if imageParts > 0 && len(audioFrames) == 0 && !d.bufferHasAudio {
		// Images must follow at least one audio append; pad the buffer with silence.
		audioFrames = d.appendFrames(audio.Silence(d.caps.InputRate, imageSilence), "")
	}

	// Images first (so they belong to this buffer), then the audio, then the commit that
	// attaches them to a conversation item.
	out.ToUpstream = append(out.ToUpstream, imageFrames...)
	out.ToUpstream = append(out.ToUpstream, audioFrames...)

	if len(audioFrames) > 0 {
		d.markAudio()
	}

	if imageParts > 0 || len(audioFrames) > 0 {
		out.ToUpstream = append(out.ToUpstream, []byte(`{"type":"input_audio_buffer.commit"}`))
		d.bufferHasAudio = false
	}

	switch {
	case len(keep) > 0:
		msg, err := sjson.SetBytes(frame, "item.content", keep)
		if err != nil {
			return dialect.Output{}, fmt.Errorf("bailian: rewrite user item: %w", err)
		}

		out.ToUpstream = append(out.ToUpstream, msg)
	case imageParts > 0:
		// A pure image item never reaches the conversation as an item; acknowledge it locally.
		id := item.Get("id").String()
		if id == "" {
			id = d.upstreamID("item_axh_")
		}

		content := make([]map[string]any, imageParts)
		for i := range content {
			content[i] = map[string]any{"type": "input_image"}
		}

		synthetic, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "realtime.item",
			"type":    "message",
			"role":    "user",
			"status":  "completed",
			"content": content,
		})

		d.items.put(id, synthetic)
		d.doneEmitted[id] = true

		out.ToClient = append(out.ToClient,
			d.synth(map[string]any{"type": "conversation.item.added", "item": json.RawMessage(synthetic)}),
			d.synth(map[string]any{"type": "conversation.item.done", "item": json.RawMessage(synthetic)}),
		)
	}

	return out, nil
}

// clientItemRetrieve answers conversation.item.retrieve from the local item cache.
func (d *Dialect) clientItemRetrieve(ev gjson.Result, clientEventID string) dialect.Output {
	id := ev.Get("item_id").String()

	item, ok := d.items.get(id)
	if !ok {
		return dialect.Output{ToClient: [][]byte{d.clientError("item_not_found", fmt.Sprintf("Item with id '%s' does not exist.", id), "item_id", clientEventID)}}
	}

	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type": "conversation.item.retrieved",
		"item": item,
	})}}
}

// clientItemDelete forwards conversation.item.delete under an adapter event id so that an
// upstream rejection can be turned into a synthesised conversation.item.deleted.
func (d *Dialect) clientItemDelete(ev gjson.Result, frame []byte, clientEventID string) (dialect.Output, error) {
	upstreamEventID := d.upstreamID("axh_del_")

	out, err := sjson.SetBytes(frame, "event_id", upstreamEventID)
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: rewrite delete event id: %w", err)
	}

	d.pendingDeletes[upstreamEventID] = pendingDelete{itemID: ev.Get("item_id").String(), clientEventID: clientEventID}

	return dialect.Output{ToUpstream: [][]byte{out}}, nil
}

// clientItemTruncate acknowledges conversation.item.truncate locally; Bailian has no such event.
func (d *Dialect) clientItemTruncate(ev gjson.Result) dialect.Output {
	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type":          "conversation.item.truncated",
		"item_id":       ev.Get("item_id").String(),
		"content_index": ev.Get("content_index").Int(),
		"audio_end_ms":  ev.Get("audio_end_ms").Int(),
	})}}
}

// clientResponseCreate rebuilds response.create with only the fields Bailian understands.
func (d *Dialect) clientResponseCreate(ev gjson.Result, clientEventID string) (dialect.Output, error) {
	resp := ev.Get("response")
	body := map[string]any{}

	if resp.IsObject() {
		if c := resp.Get("conversation"); c.Type == gjson.String && c.String() == "none" {
			return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream", "Out-of-band responses (conversation: \"none\") are not supported by the upstream model.", "response.conversation", clientEventID)}}, nil
		}

		if in := resp.Get("input"); in.Exists() && in.Type != gjson.Null {
			return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream", "response.input is not supported by the upstream model.", "response.input", clientEventID)}}, nil
		}

		if m := resp.Get("metadata"); m.IsObject() {
			d.pendingMetadata = rawCopy(m)
		}

		if ins := resp.Get("instructions"); ins.Type == gjson.String {
			body["instructions"] = ins.String()
		}

		if mods := resp.Get("output_modalities"); mods.IsArray() {
			body["modalities"] = bailianModalities(mods)
		}
	}

	ev2 := map[string]any{"type": "response.create"}
	if clientEventID != "" {
		ev2["event_id"] = clientEventID
	}

	if len(body) > 0 {
		ev2["response"] = body
	}

	b, err := json.Marshal(ev2)
	if err != nil {
		return dialect.Output{}, fmt.Errorf("bailian: marshal response.create: %w", err)
	}

	return dialect.Output{ToUpstream: [][]byte{b}}, nil
}

// base64DecodedLen returns the decoded byte length of a standard base64 string without
// decoding it.
func base64DecodedLen(b64 string) int64 {
	n := len(b64)
	if n == 0 {
		return 0
	}

	pad := len(b64) - len(strings.TrimRight(b64, "="))

	return int64(n/4*3 - pad)
}

// base64DecodedLenRaw is base64DecodedLen for a raw JSON string token (with its quotes); it
// returns -1 when the token is not a plain JSON string.
func base64DecodedLenRaw(raw string) int64 {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || strings.IndexByte(raw, '\\') >= 0 {
		return -1
	}

	return base64DecodedLen(raw[1 : len(raw)-1])
}
