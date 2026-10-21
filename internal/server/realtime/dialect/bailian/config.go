package bailian

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
)

// sessionConfig is the adapter-owned, complete Bailian session configuration. Every client
// session.update is merged into a copy of it and the whole object is sent upstream, so a
// partial GA update can never reset Bailian fields to their defaults.
type sessionConfig struct {
	id string
	// modalities is the Bailian value: ["text"] or ["text","audio"].
	modalities []string
	voice      string
	// instructions is nil until the client sets it ("" clears it on both sides).
	instructions *string
	// turnDetection is the Bailian-shaped object or the literal null; nil means the client
	// never set it and the value seeded from session.created is in effect.
	turnDetection json.RawMessage
	transcription bool
	// tools holds the Bailian-shaped entries (including server_url and authorization, which
	// Bailian never echoes); nil means never set.
	tools []json.RawMessage
	// gaTools is the client's own tools array, rendered back to it unchanged.
	gaTools        json.RawMessage
	toolChoiceNone bool
	// maxTokens is 0 for "inf".
	maxTokens int64
	// extras are Bailian-native keys the client passed through (temperature, top_p, …).
	extras map[string]json.RawMessage
}

// passThroughKeys are Bailian-native session keys a client may set directly.
var passThroughKeys = map[string]struct{}{
	"temperature":        {},
	"top_p":              {},
	"top_k":              {},
	"repetition_penalty": {},
	"presence_penalty":   {},
	"seed":               {},
	"enable_search":      {},
	"search_options":     {},
	"smooth_output":      {},
	"video":              {},
}

// droppedKeys are GA session keys with no Bailian counterpart; they are logged and ignored.
var droppedKeys = []string{
	"tracing", "truncation", "include", "parallel_tool_calls", "reasoning",
	"audio.input.noise_reduction", "audio.output.speed",
}

// rejection describes why a client frame was not forwarded. closeCode is non-zero when the
// session must also be closed.
type rejection struct {
	errType     string
	code        string
	message     string
	param       string
	closeCode   int
	closeReason string
}

// seedConfig initialises the effective config from the upstream session.created payload. The
// upstream's default voice is replaced by the family default when the family cannot speak it
// (qwen3.8 announces Chelsie and then fails every response with it).
func seedConfig(session gjson.Result, wantTranscription bool, caps Capabilities) sessionConfig {
	cfg := sessionConfig{
		id:            session.Get("id").String(),
		modalities:    []string{"text", "audio"},
		voice:         effectiveVoice(session.Get("voice").String(), caps),
		transcription: wantTranscription,
	}

	if mods := session.Get("modalities"); mods.IsArray() {
		cfg.modalities = bailianModalities(mods)
	}

	if td := session.Get("turn_detection"); td.Exists() {
		cfg.turnDetection = rawCopy(td)
	}

	if ins := session.Get("instructions"); ins.Type == gjson.String {
		cfg.instructions = lo.ToPtr(ins.String())
	}

	return cfg
}

// clone returns a deep copy safe to mutate.
func (c sessionConfig) clone() sessionConfig {
	out := c
	out.modalities = append([]string(nil), c.modalities...)

	if c.instructions != nil {
		out.instructions = lo.ToPtr(*c.instructions)
	}

	if c.turnDetection != nil {
		out.turnDetection = append(json.RawMessage(nil), c.turnDetection...)
	}

	if c.tools != nil {
		out.tools = make([]json.RawMessage, len(c.tools))
		for i, t := range c.tools {
			out.tools[i] = append(json.RawMessage(nil), t...)
		}
	}

	if c.gaTools != nil {
		out.gaTools = append(json.RawMessage(nil), c.gaTools...)
	}

	if c.extras != nil {
		out.extras = make(map[string]json.RawMessage, len(c.extras))
		for k, v := range c.extras {
			out.extras[k] = append(json.RawMessage(nil), v...)
		}
	}

	return out
}

// equal reports whether two configs would produce the same upstream session object.
func (c sessionConfig) equal(o sessionConfig) bool {
	if c.id != o.id || c.voice != o.voice || c.transcription != o.transcription ||
		c.toolChoiceNone != o.toolChoiceNone || c.maxTokens != o.maxTokens {
		return false
	}

	if strings.Join(c.modalities, ",") != strings.Join(o.modalities, ",") {
		return false
	}

	if (c.instructions == nil) != (o.instructions == nil) {
		return false
	}

	if c.instructions != nil && *c.instructions != *o.instructions {
		return false
	}

	if !bytes.Equal(c.turnDetection, o.turnDetection) || !bytes.Equal(c.gaTools, o.gaTools) {
		return false
	}

	if (c.tools == nil) != (o.tools == nil) || len(c.tools) != len(o.tools) {
		return false
	}

	for i := range c.tools {
		if !bytes.Equal(c.tools[i], o.tools[i]) {
			return false
		}
	}

	if len(c.extras) != len(o.extras) {
		return false
	}

	for k, v := range c.extras {
		if ov, ok := o.extras[k]; !ok || !bytes.Equal(v, ov) {
			return false
		}
	}

	return true
}

// hasAudio reports whether audio output is enabled.
func (c sessionConfig) hasAudio() bool {
	return lo.Contains(c.modalities, "audio")
}

// manualMode reports whether turn detection is disabled.
func (c sessionConfig) manualMode() bool {
	return isJSONNull(c.turnDetection)
}

// upstreamSession renders the full Bailian session object. includeFormats adds the
// adapter-owned audio formats, which is only allowed before the first audio append.
func (c sessionConfig) upstreamSession(caps Capabilities, includeFormats bool) ([]byte, error) {
	s := map[string]any{
		"modalities": c.modalities,
	}

	if c.voice != "" {
		s["voice"] = c.voice
	}

	if c.instructions != nil {
		s["instructions"] = *c.instructions
	}

	if c.turnDetection != nil {
		s["turn_detection"] = c.turnDetection
	}

	if c.transcription {
		s["input_audio_transcription"] = map[string]any{"model": caps.TranscriptionModel}
	} else {
		s["input_audio_transcription"] = nil
	}

	if c.tools != nil {
		if c.toolChoiceNone {
			s["tools"] = []json.RawMessage{}
		} else {
			s["tools"] = c.tools
		}
	}

	if c.maxTokens > 0 {
		s["max_tokens"] = c.maxTokens
	}

	for k, v := range c.extras {
		// video.input.representation_compact must be set before the first audio append and
		// cannot change afterwards, so it travels with the formats only.
		if k == "video" && !includeFormats {
			continue
		}

		s[k] = v
	}

	audioObj := map[string]any{}

	if caps.NestedAudio && c.voice != "" {
		audioObj["output"] = map[string]any{"voice": c.voice}
	}

	if includeFormats {
		if caps.NestedAudio {
			audioObj["input"] = map[string]any{"format": pcmFormat(caps.InputRate)}

			if caps.OutputFormatSettable {
				out, _ := audioObj["output"].(map[string]any)
				if out == nil {
					out = map[string]any{}
				}

				out["format"] = pcmFormat(clientRate)
				audioObj["output"] = out
			}
		} else {
			s["input_audio_format"] = "pcm"
			s["output_audio_format"] = "pcm"
		}
	}

	if len(audioObj) > 0 {
		s["audio"] = audioObj
	}

	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("bailian: marshal upstream session: %w", err)
	}

	return b, nil
}

// pcmFormat renders a Bailian nested PCM format object at the given sample rate.
func pcmFormat(rate int) map[string]any {
	return map[string]any{"type": "pcm", "sample_rate": rate}
}

// renderGA renders the GA-shaped session object. Both audio formats are always reported
// as 24 kHz PCM because resampling is internal to the adapter.
func (c sessionConfig) renderGA(caps Capabilities, model string) ([]byte, error) {
	format := map[string]any{"type": "audio/pcm", "rate": clientRate}

	input := map[string]any{
		"format":          format,
		"noise_reduction": nil,
		"turn_detection":  gaTurnDetection(c.turnDetection),
	}

	if c.transcription {
		input["transcription"] = map[string]any{"model": caps.TranscriptionModel}
	} else {
		input["transcription"] = nil
	}

	output := map[string]any{
		"format": format,
		"speed":  1.0,
	}

	if c.voice != "" {
		output["voice"] = c.voice
	}

	var modalities []string
	if c.hasAudio() {
		modalities = []string{"audio"}
	} else {
		modalities = []string{"text"}
	}

	s := map[string]any{
		"id":                c.id,
		"object":            "realtime.session",
		"type":              "realtime",
		"model":             model,
		"output_modalities": modalities,
		"audio":             map[string]any{"input": input, "output": output},
		"tools":             json.RawMessage("[]"),
		"tool_choice":       "auto",
		"max_output_tokens": "inf",
	}

	if c.instructions != nil {
		s["instructions"] = *c.instructions
	}

	if c.gaTools != nil {
		s["tools"] = c.gaTools
	}

	if c.toolChoiceNone {
		s["tool_choice"] = "none"
	}

	if c.maxTokens > 0 {
		s["max_output_tokens"] = c.maxTokens
	}

	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("bailian: marshal ga session: %w", err)
	}

	return b, nil
}

// gaTurnDetection converts the Bailian turn_detection value to its GA shape.
func gaTurnDetection(raw json.RawMessage) any {
	if raw == nil || isJSONNull(raw) {
		return nil
	}

	td := gjson.ParseBytes(raw)
	if !td.IsObject() {
		return nil
	}

	typ := td.Get("type").String()
	if typ == "" {
		typ = "server_vad"
	}

	out := map[string]any{"type": typ}

	copyField := func(key string) {
		if v := td.Get(key); v.Exists() && v.Type != gjson.Null {
			out[key] = rawCopy(v)
		}
	}

	copyField("create_response")
	copyField("interrupt_response")

	if typ == "server_vad" {
		copyField("threshold")
		copyField("prefix_padding_ms")
		copyField("silence_duration_ms")
		copyField("idle_timeout_ms")
	}

	return out
}

// mergeSessionUpdate applies the GA session delta to a copy of base. It returns the merged
// config and the names of GA keys that were dropped, or a rejection when the update must
// not be forwarded.
func mergeSessionUpdate(base sessionConfig, delta gjson.Result, caps Capabilities) (sessionConfig, []string, *rejection) {
	cfg := base.clone()

	if p := delta.Get("prompt"); p.Exists() && p.Type != gjson.Null {
		return cfg, nil, &rejection{
			code:    "unsupported_on_upstream",
			message: "session.prompt is not supported by the upstream model; send instructions instead.",
			param:   "session.prompt",
		}
	}

	for _, dir := range []string{"input", "output"} {
		if rej := validateAudioFormat(delta.Get("audio."+dir+".format"), dir); rej != nil {
			return cfg, nil, rej
		}
	}

	var dropped []string

	for _, k := range droppedKeys {
		if delta.Get(k).Exists() {
			dropped = append(dropped, k)
		}
	}

	if v := delta.Get("output_modalities"); v.IsArray() {
		cfg.modalities = bailianModalities(v)
	}

	if v := delta.Get("instructions"); v.Exists() {
		if v.Type == gjson.Null {
			cfg.instructions = nil
		} else {
			cfg.instructions = lo.ToPtr(v.String())
		}
	}

	if v := delta.Get("audio.input.transcription"); v.Exists() {
		cfg.transcription = v.Type != gjson.Null
	}

	if v := delta.Get("audio.input.turn_detection"); v.Exists() {
		cfg.turnDetection = translateTurnDetection(v, caps)

		if v.IsObject() && v.Get("eagerness").Exists() {
			dropped = append(dropped, "audio.input.turn_detection.eagerness")
		}
	}

	if v := delta.Get("audio.output.voice"); v.Exists() && v.Type != gjson.Null {
		if name, ok := canonicalVoice(v.String(), caps); ok && v.Type == gjson.String {
			cfg.voice = name
		} else {
			// Bailian would accept and echo an unknown voice (alloy, a voice of another
			// family) and only fail at response time, so the current voice stays, or the
			// family default when the current one is not usable either.
			cfg.voice = effectiveVoice(cfg.voice, caps)
			dropped = append(dropped, "audio.output.voice")
		}
	}

	if v := delta.Get("tools"); v.Exists() {
		if v.Type == gjson.Null {
			cfg.tools = nil
			cfg.gaTools = nil
		} else if v.IsArray() {
			tools, rej := translateTools(v)
			if rej != nil {
				return cfg, nil, rej
			}

			cfg.tools = tools
			cfg.gaTools = rawCopy(v)
		}
	}

	if v := delta.Get("tool_choice"); v.Exists() {
		cfg.toolChoiceNone = v.Type == gjson.String && v.String() == "none"

		if v.IsObject() {
			dropped = append(dropped, "tool_choice")
		}
	}

	if v := delta.Get("max_output_tokens"); v.Exists() {
		if v.Type == gjson.Number && v.Int() > 0 {
			cfg.maxTokens = v.Int()
		} else {
			cfg.maxTokens = 0
		}
	}

	delta.ForEach(func(key, value gjson.Result) bool {
		if _, ok := passThroughKeys[key.String()]; ok {
			if cfg.extras == nil {
				cfg.extras = map[string]json.RawMessage{}
			}

			cfg.extras[key.String()] = rawCopy(value)
		}

		return true
	})

	return cfg, dropped, nil
}

// validateAudioFormat checks a GA audio format object. G.711 is rejected with a close.
func validateAudioFormat(f gjson.Result, dir string) *rejection {
	if !f.Exists() || f.Type == gjson.Null {
		return nil
	}

	param := "session.audio." + dir + ".format"

	typ := f.Get("type").String()

	switch typ {
	case "", "audio/pcm":
		if rate := f.Get("rate"); rate.Exists() && rate.Type != gjson.Null && rate.Int() != clientRate {
			return &rejection{
				code:    "invalid_value",
				message: fmt.Sprintf("Only a 24000 Hz sample rate is supported for audio/pcm, got %s.", rate.Raw),
				param:   param + ".rate",
			}
		}

		return nil
	case "audio/pcmu", "audio/pcma":
		return &rejection{
			code:        "unsupported_audio_format",
			message:     fmt.Sprintf("%s is not supported by the upstream model; use audio/pcm at 24000 Hz.", typ),
			param:       param + ".type",
			closeCode:   1003,
			closeReason: "unsupported audio format",
		}
	default:
		return &rejection{
			code:    "invalid_value",
			message: fmt.Sprintf("Unknown audio format %q.", typ),
			param:   param + ".type",
		}
	}
}

// bailianModalities maps GA output_modalities to the Bailian modalities value.
func bailianModalities(v gjson.Result) []string {
	for _, m := range v.Array() {
		if m.String() == "audio" {
			return []string{"text", "audio"}
		}
	}

	return []string{"text"}
}

// translateTurnDetection maps GA audio.input.turn_detection to the Bailian value.
func translateTurnDetection(td gjson.Result, caps Capabilities) json.RawMessage {
	if td.Type == gjson.Null || !td.IsObject() {
		return json.RawMessage("null")
	}

	typ := td.Get("type").String()
	if typ == "" {
		typ = "server_vad"
	}

	if typ == "semantic_vad" && !caps.SemanticVAD {
		typ = "server_vad"
	}

	out := map[string]any{"type": typ}

	if v := td.Get("threshold"); v.Type == gjson.Number {
		out["threshold"] = v.Float()
	}

	if v := td.Get("silence_duration_ms"); v.Type == gjson.Number {
		out["silence_duration_ms"] = min(max(v.Int(), 200), 6000)
	}

	if v := td.Get("prefix_padding_ms"); v.Type == gjson.Number {
		out["prefix_padding_ms"] = v.Int()
	}

	for _, key := range []string{"create_response", "interrupt_response"} {
		if v := td.Get(key); v.IsBool() {
			out[key] = v.Bool()
		}
	}

	if v := td.Get("idle_timeout_ms"); v.Type == gjson.Number && caps.IdleTimeout && typ == "server_vad" {
		out["idle_timeout_ms"] = v.Int()
	}

	b, _ := json.Marshal(out)

	return b
}

// translateTools maps the GA tools array to Bailian's nested entries.
func translateTools(tools gjson.Result) ([]json.RawMessage, *rejection) {
	out := make([]json.RawMessage, 0, len(tools.Array()))

	for i, t := range tools.Array() {
		switch t.Get("type").String() {
		case "function":
			fn := map[string]any{"name": t.Get("name").String()}

			if v := t.Get("description"); v.Type == gjson.String {
				fn["description"] = v.String()
			}

			if v := t.Get("parameters"); v.IsObject() {
				fn["parameters"] = rawCopy(v)
			}

			b, _ := json.Marshal(map[string]any{"type": "function", "function": fn})
			out = append(out, b)
		case "mcp":
			entry, rej := translateMCPTool(t, i)
			if rej != nil {
				return nil, rej
			}

			out = append(out, entry)
		default:
			// Unknown tool types have no Bailian counterpart and are dropped.
		}
	}

	return out, nil
}

// translateMCPTool maps one GA MCP tool entry.
func translateMCPTool(t gjson.Result, index int) (json.RawMessage, *rejection) {
	param := fmt.Sprintf("session.tools[%d]", index)

	for _, key := range []string{"connector_id", "tunnel_id"} {
		if v := t.Get(key); v.Exists() && v.Type != gjson.Null && v.String() != "" {
			return nil, &rejection{
				code:    "unsupported_on_upstream",
				message: fmt.Sprintf("MCP %s is not supported by the upstream model; provide server_url.", key),
				param:   param + "." + key,
			}
		}
	}

	if t.Get("server_url").String() == "" {
		return nil, &rejection{
			code:    "missing_required_parameter",
			message: "MCP tools require server_url on the upstream model.",
			param:   param + ".server_url",
		}
	}

	entry := map[string]any{
		"type":         "mcp",
		"server_label": t.Get("server_label").String(),
		"server_url":   t.Get("server_url").String(),
	}

	if v := t.Get("authorization"); v.Type == gjson.String {
		entry["authorization"] = v.String()
	}

	if v := t.Get("headers"); v.IsObject() {
		entry["headers"] = rawCopy(v)
	}

	var (
		allowed      []string
		allowedKnown bool
	)

	if v := t.Get("allowed_tools"); v.Exists() && v.Type != gjson.Null {
		names := v

		if v.IsObject() {
			names = v.Get("tool_names")

			if v.Get("read_only").Bool() && !names.IsArray() {
				// A read-only-only filter cannot be expressed upstream; dropping it would
				// expose every tool, so refuse the update instead.
				return nil, &rejection{
					code:    "unsupported_on_upstream",
					message: "MCP allowed_tools.read_only without tool_names is not supported by the upstream model; list the allowed tool names.",
					param:   param + ".allowed_tools.read_only",
				}
			}
		}

		if names.IsArray() {
			allowed = lo.Map(names.Array(), func(n gjson.Result, _ int) string { return n.String() })
			allowedKnown = true
			entry["allowed_tools"] = allowed
		}
	}

	if v := t.Get("require_approval"); v.Exists() && v.Type != gjson.Null {
		switch {
		case v.Type == gjson.String:
			if v.String() == "never" {
				entry["require_approval"] = "never"
			} else {
				entry["require_approval"] = "always"
			}
		case v.IsObject():
			entry["require_approval"] = mapRequireApproval(v, allowed, allowedKnown)
		}
	}

	b, _ := json.Marshal(entry)

	return b, nil
}

// mapRequireApproval collapses a GA approval filter to Bailian's always/never: "never" only
// when the never filter provably covers every allowed tool.
func mapRequireApproval(obj gjson.Result, allowed []string, allowedKnown bool) string {
	always := obj.Get("always")
	if always.IsObject() {
		if names := always.Get("tool_names"); names.IsArray() && len(names.Array()) > 0 {
			return "always"
		}

		if always.Get("read_only").Bool() {
			return "always"
		}
	}

	never := obj.Get("never")
	if !never.IsObject() {
		return "always"
	}

	if never.Get("read_only").Bool() {
		// "Only read-only tools skip approval" cannot be expressed upstream; approving
		// everything is the safe direction.
		return "always"
	}

	names := never.Get("tool_names")
	if !names.Exists() || names.Type == gjson.Null {
		return "never"
	}

	if !allowedKnown {
		return "always"
	}

	set := lo.SliceToMap(names.Array(), func(n gjson.Result) (string, struct{}) { return n.String(), struct{}{} })

	for _, a := range allowed {
		if _, ok := set[a]; !ok {
			return "always"
		}
	}

	return "never"
}

// rawCopy returns an owned copy of a gjson value's raw JSON.
func rawCopy(v gjson.Result) json.RawMessage {
	return json.RawMessage(strings.Clone(v.Raw))
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) > 0 && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
