package bailian

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// Frames below are the live Bailian qwen3-asr-flash-realtime shapes recorded on 2026-11-03.
const (
	asrCreated = `{"event_id":"event_1","type":"session.created","session":{"object":"realtime.session","id":"sess_asr","model":"qwen3-asr-flash-realtime","modalities":["text"],"input_audio_format":"pcm","sample_rate":16000,"input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":{"type":"server_vad","threshold":0.2,"silence_duration_ms":800}}}`
	asrUpdated = `{"event_id":"event_2","type":"session.updated","session":{"object":"realtime.session","id":"sess_asr","model":"qwen3-asr-flash-realtime","input_audio_format":"pcm","sample_rate":16000}}`
)

func newTestASR(t *testing.T) *ASRDialect {
	t.Helper()

	d, err := NewASR(ASROptions{RequestedModel: "gpt-4o-transcribe", ActualModel: "qwen3-asr-flash-realtime"})
	require.NoError(t, err)

	return d
}

func upstream(t *testing.T, d dialect.Dialect, frame string) dialect.Output {
	t.Helper()

	out, err := d.UpstreamFrame([]byte(frame))
	require.NoError(t, err)

	return out
}

func client(t *testing.T, d dialect.Dialect, frame string) dialect.Output {
	t.Helper()

	out, err := d.ClientFrame([]byte(frame))
	require.NoError(t, err)

	return out
}

func single(t *testing.T, frames [][]byte) gjson.Result {
	t.Helper()
	require.Len(t, frames, 1)

	return gjson.ParseBytes(frames[0])
}

func asrAppend(samples int) string {
	return `{"event_id":"a1","type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, samples*2)) + `"}`
}

func TestSupportedASR(t *testing.T) {
	require.True(t, SupportedASR("qwen3-asr-flash-realtime"))
	require.True(t, SupportedASR("qwen3-asr-flash-realtime-2025-10-27"))
	require.True(t, SupportedASR(" Qwen3-ASR-Flash-Realtime "))
	require.False(t, SupportedASR("qwen3-asr-flash"), "the file-transcription model speaks another protocol")
	require.False(t, SupportedASR("qwen3.8-omni-flash-realtime"))
	require.False(t, SupportedASR("gummy-realtime-v1"))
	require.False(t, Supported("qwen3-asr-flash-realtime"), "ASR models must not be admitted as omni models")
}

func TestASR_SessionCreatedRendersGATranscriptionSession(t *testing.T) {
	d := newTestASR(t)

	created := single(t, upstream(t, d, asrCreated).ToClient)

	require.Equal(t, "session.created", created.Get("type").String())
	require.Equal(t, "transcription", created.Get("session.type").String())
	require.Equal(t, "sess_asr", created.Get("session.id").String())
	require.Equal(t, "realtime.transcription_session", created.Get("session.object").String())
	require.Equal(t, "audio/pcm", created.Get("session.audio.input.format.type").String())
	require.EqualValues(t, 24000, created.Get("session.audio.input.format.rate").Int())
	require.Equal(t, "gpt-4o-transcribe", created.Get("session.audio.input.transcription.model").String())
	require.Equal(t, gjson.Null, created.Get("session.audio.input.transcription.language").Type)
	require.Equal(t, gjson.Null, created.Get("session.audio.input.noise_reduction").Type)
	require.Equal(t, "server_vad", created.Get("session.audio.input.turn_detection.type").String())
	require.InDelta(t, 0.2, created.Get("session.audio.input.turn_detection.threshold").Float(), 1e-9)
	require.EqualValues(t, 800, created.Get("session.audio.input.turn_detection.silence_duration_ms").Int())
	require.False(t, created.Get("session.modalities").Exists())
	require.False(t, created.Get("session.audio.input.turn_detection.create_response").Exists())
}

func TestASR_ConfigIsAnsweredLocallyAndSentOnceBeforeTheFirstAudio(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)

	// Every client update is merged and answered locally; nothing goes upstream yet.
	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-live-transcribe","language":"cmn","prompt":"ignored"},"noise_reduction":{"type":"near_field"},"turn_detection":{"type":"server_vad","threshold":0.5,"prefix_padding_ms":300,"silence_duration_ms":100}}},"include":["item.input_audio_transcription.logprobs"]}}`)
	require.Empty(t, out.ToUpstream)

	updated := single(t, out.ToClient)
	require.Equal(t, "session.updated", updated.Get("type").String())
	require.Equal(t, "gpt-live-transcribe", updated.Get("session.audio.input.transcription.model").String())
	require.Equal(t, "zh", updated.Get("session.audio.input.transcription.language").String())
	require.InDelta(t, 0.5, updated.Get("session.audio.input.turn_detection.threshold").Float(), 1e-9)
	require.EqualValues(t, 200, updated.Get("session.audio.input.turn_detection.silence_duration_ms").Int(), "clamped to Bailian's minimum")
	require.Equal(t, gjson.Null, updated.Get("session.audio.input.noise_reduction").Type)

	out = client(t, d, `{"event_id":"u2","type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":null}}}}`)
	require.Empty(t, out.ToUpstream)
	require.Equal(t, gjson.Null, single(t, out.ToClient).Get("session.audio.input.turn_detection").Type)

	// The first append sends the single merged upstream update and closes the session gate;
	// the audio itself waits for the acknowledgement.
	out = client(t, d, asrAppend(2400)) // 100 ms at 24 kHz
	require.Len(t, out.ToUpstream, 1)
	require.True(t, out.CloseGate)

	init := gjson.ParseBytes(out.ToUpstream[0])
	require.Equal(t, "session.update", init.Get("type").String())
	require.Equal(t, asrInitEventID, init.Get("event_id").String())
	require.Equal(t, "pcm", init.Get("session.input_audio_format").String())
	require.EqualValues(t, 16000, init.Get("session.sample_rate").Int())
	require.Equal(t, gjson.Null, init.Get("session.turn_detection").Type)
	require.Equal(t, "zh", init.Get("session.input_audio_transcription.language").String())
	require.False(t, init.Get("session.input_audio_transcription.model").Exists())

	ack := upstream(t, d, asrUpdated)
	require.True(t, ack.OpenGate)
	require.Empty(t, ack.ToClient, "the client already got its own session.updated")
	require.Len(t, ack.ToUpstream, 1)

	app := gjson.ParseBytes(ack.ToUpstream[0])
	require.Equal(t, "input_audio_buffer.append", app.Get("type").String())
	require.Equal(t, "a1", app.Get("event_id").String())

	pcm, err := base64.StdEncoding.DecodeString(app.Get("audio").String())
	require.NoError(t, err)
	require.InDelta(t, 3200, len(pcm), 64, "24 kHz -> 16 kHz")

	// Later appends go alone, without touching the gate; a stray ack is consumed.
	out = client(t, d, asrAppend(2400))
	require.Len(t, out.ToUpstream, 1)
	require.False(t, out.CloseGate)
	require.Empty(t, upstream(t, d, asrUpdated).ToClient)
	require.False(t, upstream(t, d, asrUpdated).OpenGate)

	// Configuration is now locked: the upstream would close the session on a second update.
	out = client(t, d, `{"event_id":"u3","type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"language":"en"}}}}}`)
	require.Empty(t, out.ToUpstream)

	errEv := single(t, out.ToClient)
	require.Equal(t, "error", errEv.Get("type").String())
	require.Equal(t, "unsupported_on_upstream", errEv.Get("error.code").String())
	require.Equal(t, "u3", errEv.Get("error.event_id").String())
}

func TestASR_DefaultConfigIsPinnedOnTheFirstAudio(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)

	out := client(t, d, asrAppend(2400))
	require.Len(t, out.ToUpstream, 1)
	require.True(t, out.CloseGate)

	init := gjson.ParseBytes(out.ToUpstream[0])
	require.EqualValues(t, 16000, init.Get("session.sample_rate").Int())
	require.Equal(t, "server_vad", init.Get("session.turn_detection.type").String())
	require.False(t, init.Get("session.turn_detection.threshold").Exists(), "unset fields keep Bailian's defaults")
	require.False(t, init.Get("session.input_audio_transcription").Exists())
}

func TestASR_RejectedUpdates(t *testing.T) {
	tests := []struct {
		name      string
		update    string
		code      string
		param     string
		closeCode int
	}{
		{"conversation session type", `{"type":"realtime"}`, "unsupported_session_type", "session.type", 0},
		{"G.711 input", `{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcmu"}}}}`, "unsupported_on_upstream", "session.audio.input.format.type", 1003},
		{"wrong PCM rate", `{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":16000}}}}`, "invalid_value", "session.audio.input.format.rate", 0},
		{"unknown VAD", `{"type":"transcription","audio":{"input":{"turn_detection":{"type":"magic_vad"}}}}`, "invalid_value", "session.audio.input.turn_detection.type", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestASR(t)
			upstream(t, d, asrCreated)

			out := client(t, d, `{"event_id":"bad","type":"session.update","session":`+tt.update+`}`)
			require.Empty(t, out.ToUpstream)

			errEv := single(t, out.ToClient)
			require.Equal(t, "error", errEv.Get("type").String())
			require.Equal(t, tt.code, errEv.Get("error.code").String())
			require.Equal(t, tt.param, errEv.Get("error.param").String())
			require.Equal(t, "bad", errEv.Get("error.event_id").String())

			if tt.closeCode != 0 {
				require.NotNil(t, out.Close)
				require.Equal(t, tt.closeCode, out.Close.Code)
			} else {
				require.Nil(t, out.Close)
			}

			// A rejected update leaves the config untouched.
			out = client(t, d, asrAppend(240))
			require.Equal(t, "server_vad", gjson.ParseBytes(out.ToUpstream[0]).Get("session.turn_detection.type").String())
		})
	}
}

func TestASR_SemanticVADBecomesASilenceWindow(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)

	client(t, d, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`)

	init := gjson.ParseBytes(client(t, d, asrAppend(240)).ToUpstream[0])
	require.Equal(t, "server_vad", init.Get("session.turn_detection.type").String())
	require.EqualValues(t, 400, init.Get("session.turn_detection.silence_duration_ms").Int())
}

func TestASR_ClientEventsWithoutAnUpstreamCounterpart(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)

	// server_vad with no utterance in progress: nothing to cut, answered like an empty buffer.
	out := client(t, d, `{"event_id":"c0","type":"input_audio_buffer.commit"}`)
	require.Empty(t, out.ToUpstream)
	require.Equal(t, "input_audio_buffer_commit_empty", single(t, out.ToClient).Get("error.code").String())

	for _, typ := range []string{"input_audio_buffer.clear", "response.create", "conversation.item.create", "output_audio_buffer.clear"} {
		out := client(t, d, `{"event_id":"x","type":"`+typ+`"}`)
		require.Empty(t, out.ToUpstream, typ)
		require.Equal(t, "error", single(t, out.ToClient).Get("type").String(), typ)
	}

	// Manual mode forwards commit (after the config, when no audio was sent yet).
	m := newTestASR(t)
	upstream(t, m, asrCreated)
	client(t, m, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":null}}}}`)

	out = client(t, m, `{"event_id":"c1","type":"input_audio_buffer.commit"}`)
	require.Len(t, out.ToUpstream, 1)
	require.Equal(t, "session.update", gjson.ParseBytes(out.ToUpstream[0]).Get("type").String())
	require.True(t, out.CloseGate)

	released := upstream(t, m, asrUpdated)
	require.True(t, released.OpenGate)
	require.Equal(t, "input_audio_buffer.commit", single(t, released.ToUpstream).Get("type").String())
}

// collectClient flattens the client frames of several outputs.
func collectClient(outs ...dialect.Output) []gjson.Result {
	var frames []gjson.Result

	for _, o := range outs {
		for _, f := range o.ToClient {
			frames = append(frames, gjson.ParseBytes(f))
		}
	}

	return frames
}

func deltas(frames []gjson.Result) string {
	var b strings.Builder

	for _, f := range frames {
		if f.Get("type").String() == "conversation.item.input_audio_transcription.delta" {
			b.WriteString(f.Get("delta").String())
		}
	}

	return b.String()
}

func TestASR_ServerVADUtterancesBecomeGAEvents(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)

	text := func(item, stash string) string {
		return `{"event_id":"e","type":"conversation.item.input_audio_transcription.text","item_id":"` + item + `","content_index":0,"text":"","stash":"` + stash + `","language":"zh","emotion":"neutral"}`
	}

	var outs []dialect.Output

	outs = append(outs,
		upstream(t, d, `{"event_id":"s1","type":"input_audio_buffer.speech_started","audio_start_ms":0,"item_id":"item_A"}`),
		upstream(t, d, `{"event_id":"c1","type":"conversation.item.created","item":{"id":"item_A","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[{"type":"input_audio"}]}}`),
	)

	for _, h := range []string{"今天", "今天。", "今天天气", "今天天气。", "今天天气不错", "今天天气不错。", "今天天气不错，阳光明媚", "今天天气不错，阳光明媚。"} {
		outs = append(outs, upstream(t, d, text("item_A", h)))
	}

	outs = append(outs,
		upstream(t, d, `{"event_id":"s2","type":"input_audio_buffer.speech_stopped","audio_end_ms":2500,"item_id":"item_A"}`),
		upstream(t, d, `{"event_id":"m1","type":"input_audio_buffer.committed","item_id":"item_A"}`),
	)

	done := upstream(t, d, `{"event_id":"d1","type":"conversation.item.input_audio_transcription.completed","item_id":"item_A","content_index":0,"transcript":"今天天气不错，阳光明媚。","language":"zh","emotion":"neutral","usage":{"duration":3,"total_tokens":58,"input_tokens":44,"output_tokens":14,"input_tokens_details":{"text_tokens":3,"audio_tokens":41},"output_tokens_details":{"text_tokens":14}}}`)
	outs = append(outs, done)

	frames := collectClient(outs...)

	types := make([]string, 0, len(frames))
	for _, f := range frames {
		types = append(types, f.Get("type").String())
	}

	require.Equal(t, "input_audio_buffer.speech_started", types[0])
	require.Equal(t, "conversation.item.added", types[1])
	require.Equal(t, "conversation.item.done", types[len(types)-1])
	require.Equal(t, "conversation.item.input_audio_transcription.completed", types[len(types)-2])

	added := frames[1]
	require.Equal(t, "user", added.Get("item.role").String())
	require.Equal(t, "item_A", added.Get("item.id").String())
	require.Equal(t, gjson.Null, added.Get("previous_item_id").Type)
	require.Equal(t, gjson.Null, added.Get("item.content.0.transcript").Type)

	// The deltas plus the catch-up spell the final transcript exactly: interim punctuation
	// that was later revised ("今天。" -> "今天天气") never leaked into an append-only delta.
	require.Equal(t, "今天天气不错，阳光明媚。", deltas(frames))

	var committed, completed, itemDone gjson.Result

	for _, f := range frames {
		switch f.Get("type").String() {
		case "input_audio_buffer.committed":
			committed = f
		case "conversation.item.input_audio_transcription.completed":
			completed = f
		case "conversation.item.done":
			itemDone = f
		}
	}

	require.Equal(t, "item_A", committed.Get("item_id").String())
	require.Equal(t, gjson.Null, committed.Get("previous_item_id").Type)

	require.Equal(t, "tokens", completed.Get("usage.type").String())
	require.EqualValues(t, 58, completed.Get("usage.total_tokens").Int())
	require.EqualValues(t, 41, completed.Get("usage.input_token_details.audio_tokens").Int())
	require.Equal(t, "zh", completed.Get("languages.0.code").String())
	require.False(t, completed.Get("emotion").Exists())

	require.Equal(t, "completed", itemDone.Get("item.status").String())
	require.Equal(t, "今天天气不错，阳光明媚。", itemDone.Get("item.content.0.transcript").String())

	require.Len(t, done.Usage, 1)
	require.Equal(t, "transcription", done.Usage[0].Kind)
	require.Equal(t, "item_A", done.Usage[0].ItemID)
	require.Equal(t, "qwen3-asr-flash-realtime", done.Usage[0].Model)
	require.EqualValues(t, 44, done.Usage[0].Usage.PromptTokens)
	require.EqualValues(t, 14, done.Usage[0].Usage.CompletionTokens)
	require.EqualValues(t, 41, done.Usage[0].Usage.PromptTokensDetails.AudioTokens)

	// The second utterance: Bailian's usage is a running total, so only the difference is
	// reported and billed; committed links to the previous item.
	upstream(t, d, `{"event_id":"c2","type":"conversation.item.created","item":{"id":"item_B","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[{"type":"input_audio"}]}}`)

	second := collectClient(upstream(t, d, `{"event_id":"m2","type":"input_audio_buffer.committed","item_id":"item_B"}`))
	require.Equal(t, "item_A", second[0].Get("previous_item_id").String())

	// A repeated committed for the same item is dropped.
	require.Empty(t, upstream(t, d, `{"event_id":"m3","type":"input_audio_buffer.committed","item_id":"item_B"}`).ToClient)

	done2 := upstream(t, d, `{"event_id":"d2","type":"conversation.item.input_audio_transcription.completed","item_id":"item_B","content_index":0,"transcript":"我们一起去公园散步吧。","language":"zh","emotion":"neutral","usage":{"duration":5,"total_tokens":98,"input_tokens":71,"output_tokens":27,"input_tokens_details":{"text_tokens":3,"audio_tokens":68},"output_tokens_details":{"text_tokens":27}}}`)

	completed2 := collectClient(done2)
	require.Equal(t, "我们一起去公园散步吧。", deltas(completed2), "no hypotheses: the whole transcript is the catch-up delta")

	for _, f := range completed2 {
		if f.Get("type").String() == "conversation.item.input_audio_transcription.completed" {
			require.EqualValues(t, 40, f.Get("usage.total_tokens").Int())
			require.EqualValues(t, 27, f.Get("usage.input_token_details.audio_tokens").Int())
			require.EqualValues(t, 0, f.Get("usage.input_token_details.text_tokens").Int())
		}

		if f.Get("type").String() == "conversation.item.done" {
			require.Equal(t, "item_A", f.Get("previous_item_id").String())
		}
	}

	require.EqualValues(t, 27, done2.Usage[0].Usage.PromptTokens)
	require.EqualValues(t, 13, done2.Usage[0].Usage.CompletionTokens)
	require.InDelta(t, 2, done2.Usage[0].DurationSeconds, 1e-9)
}

func TestASR_ManualModeEventsGetSynthesisedItemIDs(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)
	client(t, d, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":null}}}}`)

	frames := collectClient(
		upstream(t, d, `{"event_id":"c","type":"conversation.item.created","item":{"object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[{"type":"input_audio"}]}}`),
		upstream(t, d, `{"event_id":"t1","type":"conversation.item.input_audio_transcription.text","content_index":0,"text":"","stash":"你好"}`),
		upstream(t, d, `{"event_id":"t2","type":"conversation.item.input_audio_transcription.text","content_index":0,"text":"","stash":"你好世界"}`),
		upstream(t, d, `{"event_id":"m","type":"input_audio_buffer.committed"}`),
		upstream(t, d, `{"event_id":"d","type":"conversation.item.input_audio_transcription.completed","content_index":0,"transcript":"你好世界。","language":"zh","usage":{"duration":2,"total_tokens":30,"input_tokens":20,"output_tokens":10}}`),
	)

	for _, f := range frames {
		id := f.Get("item_id").String()
		if id == "" {
			id = f.Get("item.id").String()
		}

		require.Equal(t, "item_axh_1", id, f.Raw)
	}

	require.Equal(t, "你好世界。", deltas(frames))

	// The next manual utterance gets a new id.
	next := collectClient(upstream(t, d, `{"event_id":"c","type":"conversation.item.created","item":{"object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[{"type":"input_audio"}]}}`))
	require.Equal(t, "item_axh_2", next[0].Get("item.id").String())
	require.Equal(t, "item_axh_1", next[0].Get("previous_item_id").String())
}

func TestASR_UpstreamErrorsAndFinish(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)
	client(t, d, `{"event_id":"cfg","type":"session.update","session":{"type":"transcription"}}`)
	client(t, d, asrAppend(240))

	errEv := single(t, upstream(t, d, `{"event_id":"e","type":"error","error":{"code":"invalid_value","message":"bad","event_id":"axh_asr_init"}}`).ToClient)
	require.Equal(t, "server_error", errEv.Get("error.type").String(), "Bailian omits error.type")
	require.Equal(t, "cfg", errEv.Get("error.event_id").String(), "points at the client's update")

	finished := upstream(t, d, `{"event_id":"f","type":"session.finished"}`)
	require.True(t, finished.UpstreamFinished)
	require.Empty(t, finished.ToClient)

	require.Equal(t, [][]byte{[]byte(`{"type":"session.finish"}`)}, d.CloseUpstream())
	require.True(t, d.FinishDrain())
	require.Equal(t, asrDialectName, d.Name())
}

func TestASRPassthrough_ForwardsUnchangedAndMetersDeltas(t *testing.T) {
	d := NewASRPassthrough(ASROptions{RequestedModel: "my-asr", ActualModel: "qwen3-asr-flash-realtime"})

	update := `{"event_id":"u","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000}}`
	require.Equal(t, [][]byte{[]byte(update)}, client(t, d, update).ToUpstream)

	created := single(t, upstream(t, d, asrCreated).ToClient)
	require.Equal(t, "my-asr", created.Get("session.model").String())
	require.Equal(t, "server_vad", created.Get("session.turn_detection.type").String(), "everything else is untouched")

	text := `{"event_id":"t","type":"conversation.item.input_audio_transcription.text","item_id":"item_A","content_index":0,"text":"","stash":"今天"}`
	require.Equal(t, [][]byte{[]byte(text)}, upstream(t, d, text).ToClient)

	first := upstream(t, d, `{"event_id":"d1","type":"conversation.item.input_audio_transcription.completed","item_id":"item_A","transcript":"a","usage":{"duration":3,"total_tokens":58,"input_tokens":44,"output_tokens":14}}`)
	second := upstream(t, d, `{"event_id":"d2","type":"conversation.item.input_audio_transcription.completed","transcript":"b","usage":{"duration":5,"total_tokens":98,"input_tokens":71,"output_tokens":27}}`)

	require.Contains(t, string(second.ToClient[0]), `"total_tokens":98`, "the client sees Bailian's own cumulative usage")
	require.EqualValues(t, 58, first.Usage[0].Usage.TotalTokens)
	require.EqualValues(t, 40, second.Usage[0].Usage.TotalTokens)
	require.Equal(t, "item_axh_1", second.Usage[0].ItemID)

	finished := upstream(t, d, `{"event_id":"f","type":"session.finished"}`)
	require.True(t, finished.UpstreamFinished)
	require.Len(t, finished.ToClient, 1, "a beta client expects session.finished")

	require.Len(t, d.CloseUpstream(), 1)
	client(t, d, `{"type":"session.finish"}`)
	require.Empty(t, d.CloseUpstream(), "the client already finished the session")
}

func TestHypothesisTracker(t *testing.T) {
	var tr hypothesisTracker

	var out strings.Builder
	for _, h := range []string{"今天", "今天。", "今天天", "今天天。", "今天天气", "今天天气不错，", "今天天气不错，阳光"} {
		out.WriteString(tr.next(h))
	}

	require.Equal(t, "今天天气不错", out.String(), "only text two hypotheses agree on, without trailing punctuation")
	out.WriteString(tr.complete("今天天气不错，阳光明媚。"))
	require.Equal(t, "今天天气不错，阳光明媚。", out.String())

	// A revision inside the agreed text cannot be fixed with append-only deltas.
	var rev hypothesisTracker
	rev.next("hello wor")
	rev.next("hello work")
	require.Empty(t, rev.complete("yellow world"))
}

func TestCommonPrefixStaysOnRuneBoundaries(t *testing.T) {
	require.Equal(t, "今天", commonPrefix("今天天", "今天太"), "天 and 太 share their first two UTF-8 bytes")
	require.Empty(t, commonPrefix("", "abc"))
	require.Equal(t, "abc", commonPrefix("abc", "abcd"))
}

func TestUsageMeter(t *testing.T) {
	var m usageMeter

	first := m.next(asrUsage{Duration: 3, Total: 58, Input: 44, Output: 14, InputAudio: 41})
	require.EqualValues(t, 58, first.Total)

	second := m.next(asrUsage{Duration: 5, Total: 98, Input: 71, Output: 27, InputAudio: 68})
	require.EqualValues(t, 40, second.Total)
	require.EqualValues(t, 27, second.InputAudio)
	require.EqualValues(t, 2, second.Duration)

	// Counters that go down mean the upstream restarted them: take the report as is.
	reset := m.next(asrUsage{Total: 10, Input: 6, Output: 4})
	require.EqualValues(t, 10, reset.Total)
}

func TestASRLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"zh": "zh", "cmn": "zh", "zh-TW": "zh", "zh_CN": "zh", "zh-hk": "yue", "yue": "yue",
		"en": "en", "en-US": "en", "eng": "en", "tl": "fil", "nb": "no", "pt-BR": "pt",
		"xx": "", "": "", "klingon": "",
	} {
		require.Equal(t, want, asrLanguage(in), in)
	}
}

func TestASR_ServerVADCommitCutsTheUtteranceInProgress(t *testing.T) {
	d := newTestASR(t)
	upstream(t, d, asrCreated)
	client(t, d, asrAppend(240))
	upstream(t, d, asrUpdated)

	upstream(t, d, `{"event_id":"s","type":"input_audio_buffer.speech_started","audio_start_ms":0,"item_id":"item_A"}`)

	// A client streaming a file needs commit for a last sentence without trailing silence.
	out := client(t, d, `{"event_id":"c","type":"input_audio_buffer.commit"}`)
	require.Equal(t, "input_audio_buffer.commit", single(t, out.ToUpstream).Get("type").String())

	// The cut commits the item; Bailian's repeat when the speech later stops is dropped, and
	// with no speech in progress any more a further commit is answered locally.
	require.Len(t, upstream(t, d, `{"event_id":"m1","type":"input_audio_buffer.committed","item_id":"item_A"}`).ToClient, 1)
	require.Empty(t, upstream(t, d, `{"event_id":"m2","type":"input_audio_buffer.committed","item_id":"item_A"}`).ToClient)
	require.Empty(t, client(t, d, `{"type":"input_audio_buffer.commit"}`).ToUpstream)
}

func TestUsageMeter_ACompletionWithoutUsageKeepsTheBaseline(t *testing.T) {
	var m usageMeter

	m.next(asrUsage{Duration: 3, Total: 58, Input: 44, Output: 14})
	require.Equal(t, asrUsage{}, m.next(asrUsage{}), "no usage reported: nothing billed")

	second := m.next(asrUsage{Duration: 5, Total: 98, Input: 71, Output: 27})
	require.EqualValues(t, 40, second.Total, "earlier utterances are not billed again")
}
