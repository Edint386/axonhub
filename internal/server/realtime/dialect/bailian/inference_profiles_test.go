package bailian

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
)

const (
	qaStreaming = "qwen-audio-3.1-asr-flash-streaming"
	qaMessage   = "qwen-audio-3.1-asr-flash-message"
	paraformer  = "paraformer-realtime-v2"
)

// qaResult renders a result-generated event shaped like the 2026-11-05 live probe of the
// qwen-audio inference models: payload.output.sentence, its payload.output.text copy and the
// nested payload.output.output duplicate. end < 0 renders end_time null (a partial). usage, when
// set, goes to payload.usage.
func qaResult(t *testing.T, id, begin, end int, text string, final, sentenceBegin bool, usage string) string {
	t.Helper()

	sentence := map[string]any{
		"sentence_id": id, "begin_time": begin, "end_time": nil, "text": text, "channel_id": 0,
		"speaker_id": nil, "sentence_end": final, "words": []any{},
	}
	if end >= 0 {
		sentence["end_time"] = end
	}
	if sentenceBegin {
		sentence["sentence_begin"] = true
	}
	if text != "" {
		sentence["words"] = []any{map[string]any{"begin_time": begin, "end_time": max(end, begin), "text": text, "punctuation": "", "fixed": final, "speaker_id": nil}}
	}

	output := map[string]any{"sentence": sentence, "text": text}
	output["output"] = map[string]any{"sentence": sentence, "text": text}

	payload := map[string]any{"output": output}
	if usage != "" {
		payload["usage"] = json.RawMessage(usage)
	}

	b, err := json.Marshal(map[string]any{
		"header":  map[string]any{"task_id": "0123456789abcdef0123456789abcdef", "event": "result-generated", "attributes": map[string]any{}},
		"payload": payload,
	})
	require.NoError(t, err)

	return string(b)
}

const (
	taskStarted      = `{"header":{"task_id":"0123456789abcdef0123456789abcdef","event":"task-started","attributes":{}},"payload":{}}`
	taskFinishedLive = `{"header":{"task_id":"0123456789abcdef0123456789abcdef","event":"task-finished","attributes":{}},"payload":{"output":{}}}`
)

func taskFinishedWith(usage string) string {
	return `{"header":{"event":"task-finished","attributes":{}},"payload":{"output":{},"usage":` + usage + `}}`
}

// audioMs is an append of ms milliseconds of 24 kHz PCM.
func audioMs(ms int) string { return asrAppend(ms * 24) }

// startTask sends the first audio and acknowledges the run-task it triggers.
func startTask(t *testing.T, d *ParaformerDialect, ms int) gjson.Result {
	t.Helper()

	out := client(t, d, audioMs(ms))
	run := single(t, out.ToUpstream)
	require.Equal(t, "run-task", run.Get("header.action").String())
	_ = upstream(t, d, taskStarted)

	return run
}

func requireNoEmptyDeltas(t *testing.T, frames [][]byte) {
	t.Helper()

	for _, f := range frames {
		ev := gjson.ParseBytes(f)
		if ev.Get("type").String() == "conversation.item.input_audio_transcription.delta" {
			require.NotEmpty(t, ev.Get("delta").String(), "empty delta reached the client: %s", f)
		}
	}
}

func findEvent(t *testing.T, frames [][]byte, typ string) gjson.Result {
	t.Helper()

	for _, f := range frames {
		if ev := gjson.ParseBytes(f); ev.Get("type").String() == typ {
			return ev
		}
	}

	require.Failf(t, "event missing", "no %s in %s", typ, eventTypes(frames))

	return gjson.Result{}
}

func TestInferenceAdmission(t *testing.T) {
	cases := []struct {
		model string
		ok    bool
		name  string
		usage inferenceUsage
	}{
		{"paraformer-realtime-v2", true, "bailian_paraformer", usageReportedSeconds},
		{"paraformer-realtime-8k-v2", true, "bailian_paraformer", usageReportedSeconds},
		{qaStreaming, true, "bailian_qwen_audio", usageAudioSeconds},
		{" Qwen-Audio-3.1-ASR-Flash-Streaming ", true, "bailian_qwen_audio", usageAudioSeconds},
		{qaStreaming + "-2026-10-30", true, "bailian_qwen_audio", usageAudioSeconds},
		{qaMessage, true, "bailian_qwen_audio", usageReportedTokens},
		{qaMessage + "-20261030", true, "bailian_qwen_audio", usageReportedTokens},
		{"qwen-audio-3.1-asr-flash", false, "", 0},
		{"qwen-audio-3.1-asr-flash-filetrans", false, "", 0},
		{"qwen-audio-3.0-asr-flash-streaming", false, "", 0},
		{"qwen-audio-3.1-asr-flash-streaming-realtime", false, "", 0},
		{"qwen3-asr-flash-realtime", false, "", 0},
		{"paraformer-v2", false, "", 0},
	}

	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			require.Equal(t, c.ok, SupportedParaformer(c.model))

			prof, ok := inferenceProfileFor(c.model)
			require.Equal(t, c.ok, ok)

			if !c.ok {
				return
			}

			require.Equal(t, c.name, prof.name)
			require.Equal(t, c.usage, prof.usage)
			require.True(t, SupportedBailianTranscription(c.model))
			require.False(t, SupportedASR(c.model), "not a Qwen-ASR (beta-shaped) model")
			require.False(t, Supported(c.model), "not an omni model")

			d, err := NewParaformer(ParaformerOptions{RequestedModel: "gpt-4o-transcribe", ActualModel: c.model})
			require.NoError(t, err)
			require.Equal(t, c.name, d.Name())
			require.True(t, strings.HasPrefix(d.Name(), "bailian"), "keeps the Bailian session-duration cap")
		})
	}

	require.Equal(t, "wss://dashscope.aliyuncs.com/api-ws/v1/inference",
		ParaformerWSURL("wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model="+qaMessage))
}

// TestParaformerRunTaskIsByteIdentical pins the Paraformer run-task bytes from before the
// profile table, so adding families cannot change what Paraformer is sent.
func TestParaformerRunTaskIsByteIdentical(t *testing.T) {
	cases := []struct {
		model, update, want string
	}{
		{paraformer, "", `{"header":{"action":"run-task","streaming":"duplex","task_id":"task-1"},"payload":{"function":"recognition","input":{},"model":"paraformer-realtime-v2","parameters":{"format":"pcm","max_sentence_silence":1300,"sample_rate":16000,"semantic_punctuation_enabled":false},"task":"asr","task_group":"audio"}}`},
		{"paraformer-realtime-8k-v2", "", `{"header":{"action":"run-task","streaming":"duplex","task_id":"task-1"},"payload":{"function":"recognition","input":{},"model":"paraformer-realtime-8k-v2","parameters":{"format":"pcm","max_sentence_silence":1300,"sample_rate":8000,"semantic_punctuation_enabled":false},"task":"asr","task_group":"audio"}}`},
		{
			paraformer, `{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"zh"},"turn_detection":{"type":"server_vad","silence_duration_ms":800}}}}}`,
			`{"header":{"action":"run-task","streaming":"duplex","task_id":"task-1"},"payload":{"function":"recognition","input":{},"model":"paraformer-realtime-v2","parameters":{"format":"pcm","language_hints":["zh"],"max_sentence_silence":800,"sample_rate":16000,"semantic_punctuation_enabled":false},"task":"asr","task_group":"audio"}}`,
		},
		{
			paraformer, `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`,
			`{"header":{"action":"run-task","streaming":"duplex","task_id":"task-1"},"payload":{"function":"recognition","input":{},"model":"paraformer-realtime-v2","parameters":{"format":"pcm","max_sentence_silence":400,"sample_rate":16000,"semantic_punctuation_enabled":true},"task":"asr","task_group":"audio"}}`,
		},
		{
			paraformer, `{"type":"session.update","session":{"audio":{"input":{"turn_detection":null,"noise_reduction":{"type":"near_field"}}}}}`,
			`{"header":{"action":"run-task","streaming":"duplex","task_id":"task-1"},"payload":{"function":"recognition","input":{},"model":"paraformer-realtime-v2","parameters":{"format":"pcm","max_sentence_silence":6000,"sample_rate":16000,"semantic_punctuation_enabled":false},"task":"asr","task_group":"audio"}}`,
		},
	}

	for _, c := range cases {
		d := newTestParaformer(t, c.model)
		if c.update != "" {
			updated := single(t, client(t, d, c.update).ToClient)
			require.Equal(t, gjson.Null, updated.Get("session.audio.input.noise_reduction").Type, "Paraformer has no vad_model")
		}

		d.taskID = "task-1"
		require.Equal(t, c.want, single(t, client(t, d, audioMs(100)).ToUpstream).Raw)
	}
}

func TestQwenAudioRunTaskParameters(t *testing.T) {
	cases := []struct {
		name   string
		update string
		want   string
	}{
		{"defaults", "", `{"format":"pcm","sample_rate":16000,"max_sentence_silence":800,"heartbeat":true}`},
		{
			"server_vad silence, near field, language",
			`{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"zh"},"noise_reduction":{"type":"near_field"},"turn_detection":{"type":"server_vad","silence_duration_ms":500}}}}}`,
			`{"format":"pcm","sample_rate":16000,"language_hints":["zh"],"max_sentence_silence":500,"vad_model":"near_meeting_16k","heartbeat":true}`,
		},
		{
			"semantic_vad folds eagerness and sends no semantic punctuation",
			`{"type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"far_field"},"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`,
			`{"format":"pcm","sample_rate":16000,"max_sentence_silence":400,"vad_model":"far_field_meeting_16k","heartbeat":true}`,
		},
		{"manual", `{"type":"session.update","session":{"audio":{"input":{"turn_detection":null}}}}`, `{"format":"pcm","sample_rate":16000,"max_sentence_silence":6000,"heartbeat":true}`},
		{"silence clamped low", `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":50}}}}}`, `{"format":"pcm","sample_rate":16000,"max_sentence_silence":200,"heartbeat":true}`},
		{"silence clamped high", `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":9000}}}}}`, `{"format":"pcm","sample_rate":16000,"max_sentence_silence":6000,"heartbeat":true}`},
		{"Filipino is tl", `{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"fil"}}}}}`, `{"format":"pcm","sample_rate":16000,"language_hints":["tl"],"max_sentence_silence":800,"heartbeat":true}`},
		{"Cantonese is auto", `{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"yue"}}}}}`, `{"format":"pcm","sample_rate":16000,"max_sentence_silence":800,"heartbeat":true}`},
		{"Dutch is known", `{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"nl"}}}}}`, `{"format":"pcm","sample_rate":16000,"language_hints":["nl"],"max_sentence_silence":800,"heartbeat":true}`},
		{"G.711 is still sent at 16 kHz", `{"type":"session.update","session":{"audio":{"input":{"format":{"type":"audio/pcmu"}}}}}`, `{"format":"pcm","sample_rate":16000,"max_sentence_silence":800,"heartbeat":true}`},
	}

	for _, model := range []string{qaStreaming, qaMessage} {
		for _, c := range cases {
			t.Run(model+"/"+c.name, func(t *testing.T) {
				d := newTestParaformer(t, model)
				if c.update != "" {
					require.Equal(t, "session.updated", single(t, client(t, d, c.update).ToClient).Get("type").String())
				}

				run := single(t, client(t, d, audioMs(100)).ToUpstream)
				require.Equal(t, "run-task", run.Get("header.action").String())
				require.Equal(t, "duplex", run.Get("header.streaming").String())
				require.Equal(t, model, run.Get("payload.model").String())
				require.Equal(t, "asr", run.Get("payload.task").String())
				require.JSONEq(t, `{}`, run.Get("payload.input").Raw)
				require.JSONEq(t, c.want, run.Get("payload.parameters").Raw)

				id := run.Get("header.task_id").String()
				require.Len(t, id, 32, "the task id form the live probe used")
				require.NotContains(t, id, "-")
			})
		}
	}
}

func TestQwenAudioLanguage(t *testing.T) {
	cases := map[string]string{
		"zh": "zh", "ZH-cn": "zh", "cmn": "zh", "zh-TW": "zh", "en-US": "en", "pt_BR": "pt", "ja": "ja",
		"fil": "tl", "tl": "tl", "tgl": "tl", "nld": "nl", "el": "el", "gre": "el", "rum": "ro", "slo": "sk", "hr": "hr",
		"yue": "", "zh-HK": "", "tr": "", "uk": "", "xx": "", "": "",
	}

	for in, want := range cases {
		require.Equal(t, want, qwenAudioLanguage(in), in)
	}

	// Paraformer keeps its own list.
	require.Equal(t, "yue", paraformerLanguage("yue"))
	require.Empty(t, paraformerLanguage("nl"))
}

func TestQwenAudioSessionEchoesNoiseReduction(t *testing.T) {
	d := newTestParaformer(t, qaStreaming)

	created := single(t, d.Greeting())
	require.EqualValues(t, 800, created.Get("session.audio.input.turn_detection.silence_duration_ms").Int())
	require.Equal(t, gjson.Null, created.Get("session.audio.input.noise_reduction").Type)

	updated := single(t, client(t, d, `{"type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"near_field"}}}}}`).ToClient)
	require.Equal(t, "near_field", updated.Get("session.audio.input.noise_reduction.type").String())

	rejected := single(t, client(t, d, `{"event_id":"u2","type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"studio"}}}}}`).ToClient)
	require.Equal(t, "invalid_value", rejected.Get("error.code").String())
	require.Equal(t, "session.audio.input.noise_reduction.type", rejected.Get("error.param").String())
	require.Equal(t, "u2", rejected.Get("error.event_id").String())

	run := single(t, client(t, d, audioMs(100)).ToUpstream)
	require.Equal(t, "near_meeting_16k", run.Get("payload.parameters.vad_model").String(), "a rejected update changes nothing")

	// null goes back to the upstream default (no vad_model).
	d = newTestParaformer(t, qaMessage)
	_ = client(t, d, `{"type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"far_field"}}}}}`)
	cleared := single(t, client(t, d, `{"type":"session.update","session":{"audio":{"input":{"noise_reduction":null}}}}`).ToClient)
	require.Equal(t, gjson.Null, cleared.Get("session.audio.input.noise_reduction").Type)
	require.False(t, single(t, client(t, d, audioMs(100)).ToUpstream).Get("payload.parameters.vad_model").Exists())
}

// TestQwenAudioResultsAndUsage replays the two-sentence live-probe clip (8.0 s; sentence 1
// ends at 3.02 s, sentence 2 at 7.4 s) through both models.
func TestQwenAudioResultsAndUsage(t *testing.T) {
	const (
		text1 = "今天天气不错，我们去公园散步。"
		text2 = "明天下午三点开会，记得带电脑。"
	)

	type step struct {
		frame string
		// types are the client event types it must produce (nil: none).
		types []string
		// usage is the completed event's usage object, records the usage records it bills.
		usage   string
		records []dialect.UsageRecord
	}

	finalTypes := []string{
		"input_audio_buffer.speech_stopped", "input_audio_buffer.committed",
		"conversation.item.input_audio_transcription.completed", "conversation.item.done",
	}
	openTypes := []string{"input_audio_buffer.speech_started", "conversation.item.added"}
	delta := "conversation.item.input_audio_transcription.delta"

	cases := []struct {
		model string
		steps []step
		// tail is task-finished after the client closes; tailRecords what it bills.
		tail        string
		tailRecords int
	}{
		{
			model: qaStreaming,
			steps: []step{
				{frame: qaResult(t, 1, 160, -1, "", false, true, ""), types: nil},
				{frame: qaResult(t, 1, 160, -1, "今天", false, false, ""), types: openTypes},
				{frame: qaResult(t, 1, 160, -1, "今天天气不错", false, false, ""), types: []string{delta}},
				{
					frame: qaResult(t, 1, 160, 3020, text1, true, false, ""), types: append([]string{delta}, finalTypes...),
					usage:   `{"type":"duration","seconds":4}`,
					records: []dialect.UsageRecord{{ItemID: "item_axh_1", DurationSeconds: 4}},
				},
				{frame: qaResult(t, 2, 4220, -1, "", false, false, ""), types: nil},
				{frame: qaResult(t, 2, 4220, -1, "明天下午", false, false, ""), types: openTypes},
				{
					frame: qaResult(t, 2, 4220, 7400, text2, true, false, ""), types: append([]string{delta}, finalTypes...),
					usage:   `{"type":"duration","seconds":4}`,
					records: []dialect.UsageRecord{{ItemID: "item_axh_2", DurationSeconds: 4}},
				},
			},
			tail: taskFinishedLive, // 8.0 s sent, 8 s billed already
		},
		{
			model: qaMessage,
			steps: []step{
				{frame: qaResult(t, 1, 160, -1, "", false, true, ""), types: nil},
				{
					frame: qaResult(t, 1, 160, 3020, text1, true, false, `{"duration":3,"input_tokens":44,"output_tokens":8,"total_tokens":52}`),
					types: append(append(append([]string{}, openTypes...), delta), finalTypes...),
					usage: `{"type":"tokens","input_tokens":44,"output_tokens":8,"total_tokens":52}`,
					records: []dialect.UsageRecord{{
						ItemID: "item_axh_1", DurationSeconds: 3,
						Usage: gaUsageOf(44, 8, 52),
					}},
				},
				{frame: qaResult(t, 2, 4220, -1, "", false, false, ""), types: nil},
				{
					frame: qaResult(t, 2, 4220, 7400, text2, true, false, `{"duration":8,"input_tokens":90,"output_tokens":18,"total_tokens":108}`),
					types: append(append(append([]string{}, openTypes...), delta), finalTypes...),
					usage: `{"type":"tokens","input_tokens":46,"output_tokens":10,"total_tokens":56}`,
					records: []dialect.UsageRecord{{
						ItemID: "item_axh_2", DurationSeconds: 5,
						Usage: gaUsageOf(46, 10, 56),
					}},
				},
			},
			tail: taskFinishedWith(`{"duration":8,"input_tokens":90,"output_tokens":18,"total_tokens":108}`), // equals the last final
		},
	}

	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			d := newTestParaformer(t, c.model)
			_ = client(t, d, `{"type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"near_field"},"turn_detection":{"type":"server_vad","silence_duration_ms":500}}}}}`)
			_ = startTask(t, d, 100)
			_ = client(t, d, audioMs(7900))

			transcripts := map[string]string{}

			for i, s := range c.steps {
				out := upstream(t, d, s.frame)
				require.Equal(t, s.types, nilIfEmpty(eventTypes(out.ToClient)), "step %d", i)
				requireNoEmptyDeltas(t, out.ToClient)

				for _, f := range out.ToClient {
					ev := gjson.ParseBytes(f)
					if ev.Get("type").String() == delta {
						transcripts[ev.Get("item_id").String()] += ev.Get("delta").String()
					}
				}

				if s.usage == "" {
					require.Empty(t, out.Usage, "step %d", i)
					continue
				}

				completed := findEvent(t, out.ToClient, "conversation.item.input_audio_transcription.completed")
				require.JSONEq(t, s.usage, completed.Get("usage").Raw, "step %d", i)
				require.Equal(t, transcripts[completed.Get("item_id").String()], completed.Get("transcript").String(),
					"the deltas spell the completed transcript")

				require.Len(t, out.Usage, len(s.records))
				for j, want := range s.records {
					got := out.Usage[j]
					require.Equal(t, "transcription", got.Kind)
					require.Equal(t, c.model, got.Model)
					require.Equal(t, want.ItemID, got.ItemID)
					require.InDelta(t, want.DurationSeconds, got.DurationSeconds, 1e-9)
					require.NotNil(t, got.Usage)
					if want.Usage != nil {
						require.Equal(t, want.Usage.PromptTokens, got.Usage.PromptTokens)
						require.Equal(t, want.Usage.CompletionTokens, got.Usage.CompletionTokens)
						require.Equal(t, want.Usage.TotalTokens, got.Usage.TotalTokens)
					} else {
						require.Zero(t, got.Usage.TotalTokens, "no token counts to read")
					}
					require.Equal(t, "qwen_audio_sentence", gjson.GetBytes(got.Summary, "type").String())
					require.Equal(t, want.ItemID, gjson.GetBytes(got.Summary, "item_id").String())
				}
			}

			require.Equal(t, text1, transcripts["item_axh_1"])
			require.Equal(t, text2, transcripts["item_axh_2"])

			bye := single(t, d.CloseUpstream())
			require.Equal(t, "finish-task", bye.Get("header.action").String())

			out := upstream(t, d, c.tail)
			require.True(t, out.UpstreamFinished)
			require.Len(t, out.Usage, c.tailRecords)
		})
	}
}

func gaUsageOf(in, out, total int64) *llm.Usage {
	return &llm.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: total}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestQwenAudioMessageUsageFromOutputEcho(t *testing.T) {
	d := newTestParaformer(t, qaMessage)
	_ = startTask(t, d, 2000)

	final := `{"header":{"event":"result-generated","attributes":{}},"payload":{"output":{"sentence":{"sentence_id":1,"begin_time":120,"end_time":1700,"text":"欢迎使用阿里云。","sentence_end":true,"words":[]},"usage":{"duration":2,"input_tokens":84,"output_tokens":4,"total_tokens":88}}}}`
	out := upstream(t, d, final)
	require.Len(t, out.Usage, 1)
	require.EqualValues(t, 88, out.Usage[0].Usage.TotalTokens)
}

func TestQwenAudioTaskTails(t *testing.T) {
	t.Run("message bills task-finished usage past the last final", func(t *testing.T) {
		d := newTestParaformer(t, qaMessage)
		_ = startTask(t, d, 3500)
		_ = upstream(t, d, qaResult(t, 1, 160, 3020, "好。", true, false, `{"duration":3,"input_tokens":44,"output_tokens":8,"total_tokens":52}`))

		_ = d.CloseUpstream()
		out := upstream(t, d, taskFinishedWith(`{"duration":4,"input_tokens":50,"output_tokens":8,"total_tokens":58}`))
		require.True(t, out.UpstreamFinished)

		rec := single1(t, out.Usage)
		require.Equal(t, "item_axh_1", rec.ItemID)
		require.EqualValues(t, 6, rec.Usage.PromptTokens)
		require.EqualValues(t, 0, rec.Usage.CompletionTokens)
		require.InDelta(t, 1, rec.DurationSeconds, 1e-9)
		require.Equal(t, "qwen_audio_task_tail", gjson.GetBytes(rec.Summary, "type").String())
	})

	t.Run("message never bills a task-finished report below its finals", func(t *testing.T) {
		for _, usage := range []string{`{"duration":8}`, `{"duration":8,"input_tokens":80,"output_tokens":18,"total_tokens":98}`} {
			d := newTestParaformer(t, qaMessage)
			_ = startTask(t, d, 8000)
			_ = upstream(t, d, qaResult(t, 1, 160, 7400, "好。", true, false, `{"duration":8,"input_tokens":90,"output_tokens":18,"total_tokens":108}`))

			_ = d.CloseUpstream()
			require.Empty(t, upstream(t, d, taskFinishedWith(usage)).Usage, usage)
		}
	})

	t.Run("streaming bills the audio after the last final", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 8400)
		final := upstream(t, d, qaResult(t, 1, 160, 7400, "好。", true, false, ""))
		require.InDelta(t, 8, single1(t, final.Usage).DurationSeconds, 1e-9)

		_ = d.CloseUpstream()
		out := upstream(t, d, taskFinishedLive)

		rec := single1(t, out.Usage)
		require.Equal(t, "item_axh_1", rec.ItemID)
		require.InDelta(t, 1, rec.DurationSeconds, 1e-9, "8.4 s sent, 9 s billed in all")
		require.Zero(t, rec.Usage.TotalTokens)
	})

	t.Run("streaming bills a session without any final", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1500)

		_ = d.CloseUpstream()
		rec := single1(t, upstream(t, d, taskFinishedLive).Usage)
		require.True(t, strings.HasPrefix(rec.ItemID, "task_"), rec.ItemID)
		require.InDelta(t, 2, rec.DurationSeconds, 1e-9)
	})

	t.Run("paraformer bills its finals only", func(t *testing.T) {
		d := newTestParaformer(t, paraformer)
		_ = startTask(t, d, 1500)

		_ = d.CloseUpstream()
		require.Empty(t, upstream(t, d, taskFinishedWith(`{"duration":2}`)).Usage)
	})
}

func single1(t *testing.T, recs []dialect.UsageRecord) dialect.UsageRecord {
	t.Helper()
	require.Len(t, recs, 1)

	return recs[0]
}

// inferenceModel is one family in the commit tables, with how its finals report usage.
type inferenceModel struct {
	model     string
	usageType string
	// finalUsage renders the cumulative usage a final reports after seconds of the task.
	finalUsage func(seconds int) string
}

var inferenceModels = []inferenceModel{
	{paraformer, "duration", func(s int) string { return `{"duration":` + itoa(s) + `}` }},
	{qaStreaming, "duration", func(int) string { return "" }},
	{qaMessage, "tokens", func(s int) string {
		return `{"duration":` + itoa(s) + `,"input_tokens":` + itoa(15*s) + `,"output_tokens":` + itoa(s) + `,"total_tokens":` + itoa(16*s) + `}`
	}},
}

func TestInferenceCommitIsAlwaysAnswered(t *testing.T) {
	for _, m := range inferenceModels {
		t.Run(m.model+"/empty buffer", func(t *testing.T) {
			d := newTestParaformer(t, m.model)
			require.Equal(t, "input_audio_buffer_commit_empty",
				single(t, client(t, d, `{"type":"input_audio_buffer.commit"}`).ToClient).Get("error.code").String())

			_ = startTask(t, d, 50)
			out := client(t, d, `{"type":"input_audio_buffer.commit"}`)
			require.Empty(t, out.ToUpstream)
			require.Contains(t, single(t, out.ToClient).Get("error.message").String(), "50.00ms")
		})

		t.Run(m.model+"/a final answers the commit and the next task starts over", func(t *testing.T) {
			d := newTestParaformer(t, m.model)
			run1 := startTask(t, d, 1500)

			out := client(t, d, `{"type":"input_audio_buffer.commit"}`)
			require.True(t, out.CloseGate)
			finish := single(t, out.ToUpstream)
			require.Equal(t, "finish-task", finish.Get("header.action").String())
			require.Equal(t, run1.Get("header.task_id").String(), finish.Get("header.task_id").String())

			final := upstream(t, d, qaResult(t, 1, 100, 1200, "第一句。", true, false, m.finalUsage(2)))
			completed := findEvent(t, final.ToClient, "conversation.item.input_audio_transcription.completed")
			require.Equal(t, m.usageType, completed.Get("usage.type").String())
			first := single1(t, final.Usage)

			done := upstream(t, d, taskFinishedLive)
			require.True(t, done.OpenGate)
			require.False(t, done.UpstreamFinished)
			require.Empty(t, done.ToClient, "already answered by the final")
			require.Empty(t, done.Usage, "nothing left to bill: 1.5 s sent, 2 s billed")

			// The next audio starts a new task: new id, usage from zero, session timeline.
			run2 := startTask(t, d, 1500)
			require.NotEqual(t, run1.Get("header.task_id").String(), run2.Get("header.task_id").String())

			opened := upstream(t, d, qaResult(t, 1, 200, -1, "第二", false, false, ""))
			require.EqualValues(t, 1700, findEvent(t, opened.ToClient, "input_audio_buffer.speech_started").Get("audio_start_ms").Int())

			// Upstream counters restart with the task, so 3 s here is this task's alone.
			second := upstream(t, d, qaResult(t, 1, 200, 1000, "第二句。", true, false, m.finalUsage(3)))
			require.EqualValues(t, 2500, findEvent(t, second.ToClient, "input_audio_buffer.speech_stopped").Get("audio_end_ms").Int())

			rec := single1(t, second.Usage)
			require.Equal(t, "item_axh_2", rec.ItemID)

			if m.model == qaStreaming {
				require.InDelta(t, 1, rec.DurationSeconds, 1e-9, "2.5 s of session audio: 3 s billed in all")
			} else {
				require.InDelta(t, 3, rec.DurationSeconds, 1e-9, "the baseline restarts with the task")
			}

			if m.usageType == "tokens" {
				require.EqualValues(t, 30, first.Usage.PromptTokens)
				require.EqualValues(t, 45, rec.Usage.PromptTokens, "the meter restarts with the task")
				require.EqualValues(t, 48, findEvent(t, second.ToClient, "conversation.item.input_audio_transcription.completed").Get("usage.total_tokens").Int())
			}
		})

		t.Run(m.model+"/a commit of silence gets an empty item", func(t *testing.T) {
			d := newTestParaformer(t, m.model)
			_ = startTask(t, d, 500)
			_ = client(t, d, `{"type":"input_audio_buffer.commit"}`)

			done := upstream(t, d, taskFinishedLive)
			require.Equal(t, []string{
				"input_audio_buffer.committed", "conversation.item.added",
				"conversation.item.input_audio_transcription.completed", "conversation.item.done",
			}, eventTypes(done.ToClient))

			completed := gjson.ParseBytes(done.ToClient[2])
			require.Empty(t, completed.Get("transcript").String())
			require.Equal(t, m.usageType, completed.Get("usage.type").String())

			if m.model == qaStreaming {
				// No final billed the half second: the empty item carries it.
				require.EqualValues(t, 1, completed.Get("usage.seconds").Int())
				rec := single1(t, done.Usage)
				require.Equal(t, completed.Get("item_id").String(), rec.ItemID)
				require.InDelta(t, 1, rec.DurationSeconds, 1e-9)
			} else {
				require.Empty(t, done.Usage)
			}
		})

		t.Run(m.model+"/server_vad commits at the speech end plus the silence", func(t *testing.T) {
			d := newTestParaformer(t, m.model)
			_ = client(t, d, `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":500}}}}}`)
			_ = startTask(t, d, 1550)
			_ = upstream(t, d, qaResult(t, 1, 0, 1000, "好。", true, false, m.finalUsage(2)))

			out := client(t, d, `{"type":"input_audio_buffer.commit"}`)
			require.Empty(t, out.ToUpstream, "only 50 ms past 1000+500 ms")
			require.Equal(t, "input_audio_buffer_commit_empty", single(t, out.ToClient).Get("error.code").String())

			_ = client(t, d, audioMs(50))
			require.Equal(t, "finish-task", single(t, client(t, d, `{"type":"input_audio_buffer.commit"}`).ToUpstream).Get("header.action").String())
		})

		t.Run(m.model+"/semantic_vad commits at the speech end", func(t *testing.T) {
			d := newTestParaformer(t, m.model)
			_ = client(t, d, `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`)
			_ = startTask(t, d, 1150)
			_ = upstream(t, d, qaResult(t, 1, 0, 1000, "好。", true, false, m.finalUsage(2)))

			require.Equal(t, "finish-task", single(t, client(t, d, `{"type":"input_audio_buffer.commit"}`).ToUpstream).Get("header.action").String())
		})
	}
}

func TestQwenAudioG711CountsDuration(t *testing.T) {
	d := newTestParaformer(t, qaStreaming)
	_ = client(t, d, `{"type":"session.update","session":{"audio":{"input":{"format":{"type":"audio/pcma"}}}}}`)

	alaw := `{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, 8000)) + `"}` // 1 s
	run := single(t, client(t, d, alaw).ToUpstream)
	require.EqualValues(t, 16000, run.Get("payload.parameters.sample_rate").Int())

	started := upstream(t, d, taskStarted)
	require.True(t, started.OpenGate)
	require.Len(t, started.ToUpstream, 1)
	require.InDelta(t, 32000, len(started.ToUpstream[0]), 64, "1 s of 16 kHz PCM16")

	_ = d.CloseUpstream()
	require.InDelta(t, 1, single1(t, upstream(t, d, taskFinishedLive).Usage).DurationSeconds, 1e-9)
}
