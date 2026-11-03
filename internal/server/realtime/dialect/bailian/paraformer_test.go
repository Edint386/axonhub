package bailian

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newTestParaformer(t *testing.T, actual string) *ParaformerDialect {
	t.Helper()
	d, err := NewParaformer(ParaformerOptions{RequestedModel: "gpt-4o-transcribe", ActualModel: actual})
	require.NoError(t, err)
	return d
}

func TestSupportedParaformer(t *testing.T) {
	require.True(t, SupportedParaformer("paraformer-realtime-v2"))
	require.True(t, SupportedParaformer("paraformer-realtime-8k-v2"))
	require.True(t, SupportedParaformer(" Paraformer-Realtime-V2 "))
	require.False(t, SupportedParaformer("paraformer-v2"), "file/batch model")
	require.False(t, SupportedParaformer("qwen3-asr-flash-realtime"))
	require.True(t, SupportedBailianTranscription("paraformer-realtime-v2"))
	require.True(t, SupportedBailianTranscription("qwen3-asr-flash-realtime"))
	require.False(t, SupportedASR("paraformer-realtime-v2"))
	require.False(t, Supported("paraformer-realtime-v2"))
}

func TestParaformerWSURL(t *testing.T) {
	require.Equal(t, "wss://dashscope.aliyuncs.com/api-ws/v1/inference",
		ParaformerWSURL("wss://dashscope.aliyuncs.com/api-ws/v1/realtime"))
	require.Equal(t, "wss://dashscope.aliyuncs.com/api-ws/v1/inference",
		ParaformerWSURL("wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=paraformer-realtime-v2"))
	require.Equal(t, "wss://ws.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference?foo=1",
		ParaformerWSURL("wss://ws.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=x&foo=1"))
}

func TestParaformer_GreetingIsGASessionCreated(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	created := single(t, d.Greeting())
	require.Equal(t, "session.created", created.Get("type").String())
	require.Equal(t, "transcription", created.Get("session.type").String())
	require.Equal(t, "realtime.transcription_session", created.Get("session.object").String())
	require.Equal(t, "audio/pcm", created.Get("session.audio.input.format.type").String())
	require.EqualValues(t, 24000, created.Get("session.audio.input.format.rate").Int())
	require.Equal(t, "gpt-4o-transcribe", created.Get("session.audio.input.transcription.model").String())
	require.Equal(t, "server_vad", created.Get("session.audio.input.turn_detection.type").String())
	require.EqualValues(t, 1300, created.Get("session.audio.input.turn_detection.silence_duration_ms").Int())
}

func TestParaformer_RunTaskBeforeFirstAudioThenBinary(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = d.Greeting()

	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"language":"zh"},"turn_detection":{"type":"server_vad","silence_duration_ms":800}}}}}`)
	require.Empty(t, out.ToUpstream)
	require.Equal(t, "zh", single(t, out.ToClient).Get("session.audio.input.transcription.language").String())
	require.EqualValues(t, 800, single(t, out.ToClient).Get("session.audio.input.turn_detection.silence_duration_ms").Int())

	out = client(t, d, asrAppend(2400))
	require.True(t, out.CloseGate)
	require.Len(t, out.ToUpstream, 1)
	require.False(t, out.BinaryUpstream)

	run := gjson.ParseBytes(out.ToUpstream[0])
	require.Equal(t, "run-task", run.Get("header.action").String())
	require.Equal(t, "paraformer-realtime-v2", run.Get("payload.model").String())
	require.EqualValues(t, 16000, run.Get("payload.parameters.sample_rate").Int())
	require.Equal(t, "pcm", run.Get("payload.parameters.format").String())
	require.Equal(t, "zh", run.Get("payload.parameters.language_hints.0").String())
	require.EqualValues(t, 800, run.Get("payload.parameters.max_sentence_silence").Int())

	started := `{"header":{"task_id":"` + run.Get("header.task_id").String() + `","event":"task-started","attributes":{}},"payload":{}}`
	out = upstream(t, d, started)
	require.True(t, out.OpenGate)
	require.True(t, out.BinaryUpstream)
	require.NotEmpty(t, out.ToUpstream)
	require.True(t, len(out.ToUpstream[0]) > 0)
	require.False(t, json.Valid(out.ToUpstream[0]), "audio must be raw PCM bytes")

	out = client(t, d, asrAppend(2400))
	require.True(t, out.BinaryUpstream)
	require.Len(t, out.ToUpstream, 1)
}

func TestParaformer_8kModelUses8kRate(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-8k-v2")
	out := client(t, d, asrAppend(2400))
	run := gjson.ParseBytes(out.ToUpstream[0])
	require.EqualValues(t, 8000, run.Get("payload.parameters.sample_rate").Int())
}

func TestParaformer_ResultGeneratedMapsToGA(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, asrAppend(2400))
	_ = upstream(t, d, `{"header":{"event":"task-started"},"payload":{}}`)

	partial1 := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":100,"end_time":null,"text":"你好","heartbeat":false,"sentence_end":false}},"usage":null}}`
	out := upstream(t, d, partial1)
	types := eventTypes(out.ToClient)
	require.Contains(t, types, "input_audio_buffer.speech_started")
	require.Contains(t, types, "conversation.item.added")

	partial2 := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":100,"end_time":null,"text":"你好世","heartbeat":false,"sentence_end":false}},"usage":null}}`
	out = upstream(t, d, partial2)
	types = eventTypes(out.ToClient)
	require.Contains(t, types, "conversation.item.input_audio_transcription.delta")

	final := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":100,"end_time":900,"text":"你好世界。","heartbeat":false,"sentence_end":true}},"usage":{"duration":2}}}`
	out = upstream(t, d, final)
	types = eventTypes(out.ToClient)
	require.Contains(t, types, "conversation.item.input_audio_transcription.completed")
	require.Contains(t, types, "conversation.item.done")
	require.Len(t, out.Usage, 1)
	require.Equal(t, "transcription", out.Usage[0].Kind)
	require.Equal(t, "paraformer-realtime-v2", out.Usage[0].Model)
	require.InDelta(t, 2, out.Usage[0].DurationSeconds, 1e-9)

	final2 := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":1000,"end_time":1800,"text":"再见","heartbeat":false,"sentence_end":true}},"usage":{"duration":5}}}`
	out = upstream(t, d, final2)
	require.Len(t, out.Usage, 1)
	require.InDelta(t, 3, out.Usage[0].DurationSeconds, 1e-9, "cumulative duration diff")
}

func TestParaformer_FinishTaskAndFinished(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, asrAppend(2400))
	_ = upstream(t, d, `{"header":{"event":"task-started"},"payload":{}}`)

	bye := d.CloseUpstream()
	require.Len(t, bye, 1)
	require.Equal(t, "finish-task", gjson.GetBytes(bye[0], "header.action").String())

	out := upstream(t, d, `{"header":{"event":"task-finished"},"payload":{}}`)
	require.True(t, out.UpstreamFinished)
	require.True(t, d.FinishDrain())
}

func TestParaformer_G711Accepted(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"input":{"format":{"type":"audio/pcmu"}}}}}`)
	require.Equal(t, "audio/pcmu", single(t, out.ToClient).Get("session.audio.input.format.type").String())

	mulaw := make([]byte, 160) // 20 ms at 8 kHz
	for i := range mulaw {
		mulaw[i] = 0xff
	}
	frame := `{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(mulaw) + `"}`
	out = client(t, d, frame)
	require.Equal(t, "run-task", gjson.GetBytes(out.ToUpstream[0], "header.action").String())
}

func TestParaformer_ConfigRejectedAfterStart(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, asrAppend(2400))
	out := client(t, d, `{"event_id":"u2","type":"session.update","session":{"type":"transcription"}}`)
	require.Equal(t, "error", single(t, out.ToClient).Get("type").String())
	require.Equal(t, "unsupported_on_upstream", single(t, out.ToClient).Get("error.code").String())
}

func eventTypes(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, gjson.GetBytes(f, "type").String())
	}
	return out
}

func TestParaformer_EmptyPartialIgnored(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, asrAppend(2400))
	_ = upstream(t, d, `{"header":{"event":"task-started"},"payload":{}}`)

	empty := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":0,"end_time":null,"text":"","heartbeat":false,"sentence_end":false,"sentence_begin":true}},"usage":null}}`
	out := upstream(t, d, empty)
	require.Empty(t, out.ToClient)

	partial := `{"header":{"event":"result-generated"},"payload":{"output":{"sentence":{"begin_time":0,"end_time":null,"text":"你好","heartbeat":false,"sentence_end":false}},"usage":null}}`
	out = upstream(t, d, partial)
	require.Contains(t, eventTypes(out.ToClient), "input_audio_buffer.speech_started")
}

func TestParaformer_TaskFailedBeforeStartedOpensGateAndCloses(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	out := client(t, d, asrAppend(2400))
	require.True(t, out.CloseGate)
	require.True(t, d.awaiting)

	failed := `{"header":{"event":"task-failed","error_code":"NO_VALID_AUDIO_ERROR","error_message":"no valid audio data"},"payload":{}}`
	out = upstream(t, d, failed)
	require.Equal(t, "error", single(t, out.ToClient).Get("type").String())
	require.Equal(t, "NO_VALID_AUDIO_ERROR", single(t, out.ToClient).Get("error.code").String())
	require.Equal(t, "no valid audio data", single(t, out.ToClient).Get("error.message").String())
	require.NotNil(t, out.Close)
	require.Equal(t, 1011, out.Close.Code)
	require.Equal(t, "NO_VALID_AUDIO_ERROR: no valid audio data", out.Close.Reason)
	require.True(t, out.OpenGate, "must open gate so session does not wait for gate timeout")
	require.False(t, d.awaiting)
	require.Nil(t, d.pending)
}

func TestParaformer_TaskFailedAfterStartedCloses(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, asrAppend(2400))
	_ = upstream(t, d, `{"header":{"event":"task-started"},"payload":{}}`)
	require.False(t, d.awaiting)

	failed := `{"header":{"event":"task-failed","error_code":"TASK_FAILED","error_message":"upstream boom"},"payload":{}}`
	out := upstream(t, d, failed)
	require.Equal(t, "error", single(t, out.ToClient).Get("type").String())
	require.Equal(t, "TASK_FAILED", single(t, out.ToClient).Get("error.code").String())
	require.NotNil(t, out.Close)
	require.Equal(t, 1011, out.Close.Code)
	require.Equal(t, "TASK_FAILED: upstream boom", out.Close.Reason)
	require.False(t, out.OpenGate, "gate already open after task-started")
}

func TestParaformer_SemanticVADMapsEagernessAndEchoes(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = d.Greeting()

	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`)
	updated := single(t, out.ToClient)
	require.Equal(t, "session.updated", updated.Get("type").String())
	require.Equal(t, "semantic_vad", updated.Get("session.audio.input.turn_detection.type").String())
	require.Equal(t, "high", updated.Get("session.audio.input.turn_detection.eagerness").String())
	require.False(t, updated.Get("session.audio.input.turn_detection.silence_duration_ms").Exists())

	out = client(t, d, asrAppend(2400))
	run := gjson.ParseBytes(out.ToUpstream[0])
	require.Equal(t, "run-task", run.Get("header.action").String())
	require.True(t, run.Get("payload.parameters.semantic_punctuation_enabled").Bool())
	require.EqualValues(t, 400, run.Get("payload.parameters.max_sentence_silence").Int(), "high eagerness → 400ms silence")
}

func TestParaformer_SemanticVADDefaultEagerness(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad"}}}}}`)
	updated := single(t, out.ToClient)
	require.Equal(t, "semantic_vad", updated.Get("session.audio.input.turn_detection.type").String())
	require.Equal(t, "medium", updated.Get("session.audio.input.turn_detection.eagerness").String())

	out = client(t, d, asrAppend(2400))
	run := gjson.ParseBytes(out.ToUpstream[0])
	require.True(t, run.Get("payload.parameters.semantic_punctuation_enabled").Bool())
	require.EqualValues(t, 800, run.Get("payload.parameters.max_sentence_silence").Int())
}

func TestParaformer_SemanticVADLowEagerness(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	out := client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"low"}}}}}`)
	require.Equal(t, "low", single(t, out.ToClient).Get("session.audio.input.turn_detection.eagerness").String())
	out = client(t, d, asrAppend(2400))
	require.EqualValues(t, 1200, gjson.GetBytes(out.ToUpstream[0], "payload.parameters.max_sentence_silence").Int())
	require.True(t, gjson.GetBytes(out.ToUpstream[0], "payload.parameters.semantic_punctuation_enabled").Bool())
}

func TestParaformer_ServerVADClearsSemantic(t *testing.T) {
	d := newTestParaformer(t, "paraformer-realtime-v2")
	_ = client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad","eagerness":"high"}}}}}`)
	out := client(t, d, `{"event_id":"u2","type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":900}}}}}`)
	updated := single(t, out.ToClient)
	require.Equal(t, "server_vad", updated.Get("session.audio.input.turn_detection.type").String())
	require.EqualValues(t, 900, updated.Get("session.audio.input.turn_detection.silence_duration_ms").Int())
	require.False(t, updated.Get("session.audio.input.turn_detection.eagerness").Exists())

	out = client(t, d, asrAppend(2400))
	run := gjson.ParseBytes(out.ToUpstream[0])
	require.False(t, run.Get("payload.parameters.semantic_punctuation_enabled").Bool())
	require.EqualValues(t, 900, run.Get("payload.parameters.max_sentence_silence").Int())
}
