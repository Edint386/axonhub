package bailianasr

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
)

const messageModel = "qwen-audio-3.1-asr-flash-message"

func newMessageOutbound(t *testing.T, baseURL string, frameInterval time.Duration) *Outbound {
	t.Helper()

	return Wrap(&recordingOutbound{}, Config{
		BaseURL:              baseURL,
		APIKeyProvider:       auth.NewStaticKeyProvider(fakeAPIKey),
		CacheScope:           t.Name(),
		MessageFrameInterval: frameInterval,
	})
}

func messageRequest(format string, stream bool, name string, audio []byte) *llm.Request {
	req := transcriptionRequest(messageModel, format, stream, audio)
	req.Transcription.FileName = name

	return req
}

func TestMessage_WAVIsResampledToMonoPCMAndUsageIsTheLastReport(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), -1)

	// 1 s of 44.1 kHz stereo: the windowed-sinc resampler does not take 44100/16000, so the
	// linear fallback runs.
	resp, err := runNonStream(t.Context(), t, out, messageRequest("verbose_json", false, "voice.wav", withListChunk(toneWAV(44100, 2, 1))))
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Len(t, calls.runTasks, 1)
	require.Equal(t, "Bearer "+fakeAPIKey, calls.authorizations[0])
	require.False(t, calls.audioBeforeRun, "no audio before task-started")

	runTask := gjson.Parse(calls.runTasks[0])
	require.Equal(t, "run-task", runTask.Get("header.action").String())
	require.Len(t, runTask.Get("header.task_id").String(), 32)
	require.Equal(t, "duplex", runTask.Get("header.streaming").String())
	require.Equal(t, messageModel, runTask.Get("payload.model").String())
	require.Equal(t, "pcm", runTask.Get("payload.parameters.format").String())
	require.Equal(t, gjson.Number, runTask.Get("payload.parameters.sample_rate").Type, "sample_rate is an integer")
	require.EqualValues(t, 16000, runTask.Get("payload.parameters.sample_rate").Int())
	require.Equal(t, `["zh"]`, runTask.Get("payload.parameters.language_hints").Raw)
	require.False(t, runTask.Get("payload.parameters.heartbeat").Bool())

	// 16 kHz mono PCM16, sent in 100 ms frames.
	require.InDelta(t, 32000, len(calls.audio), 8)
	for _, n := range calls.frames {
		require.LessOrEqual(t, n, messageFrameBytes)
	}

	require.Greater(t, rms(calls.audio), 4000.0, "the tone survives downmix and resampling")

	require.Equal(t, "application/json", resp.Transcription.RawContentType)
	body := gjson.ParseBytes(resp.Transcription.Raw)
	require.Equal(t, clipText, body.Get("text").String())
	require.Len(t, body.Get("segments").Array(), 2)
	require.InDelta(t, 4.4, body.Get("segments.1.start").Float(), 0.001)
	require.EqualValues(t, 108, body.Get("usage.total_tokens").Int(), "usage is cumulative: the last report wins")

	require.EqualValues(t, 90, resp.Usage.PromptTokens)
	require.EqualValues(t, 18, resp.Usage.CompletionTokens)
	require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.001)
}

func TestMessage_StreamEmitsOneDeltaPerFinal(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), -1)

	responses, raw, httpReq := runStream(t.Context(), t, out, messageRequest("", true, "voice.wav", toneWAV(16000, 1, 0.5)))

	require.Equal(t, committedEventType, raw[0].Type, "the stream commits once the task started")
	require.NotNil(t, responses[0].Transcription, "the committed response counts as content")
	require.NotEmpty(t, responses[0].Transcription.Raw)
	require.Nil(t, responses[0].TranscriptionStreamEvent)

	events := transcriptionEvents(responses)
	require.Len(t, events, 3)
	require.Equal(t, "今天天气不错，我们去公园散步。", events[0].Delta)
	require.Equal(t, "明天下午三点开会，记得带电脑。", events[1].Delta)
	require.Equal(t, "transcript.text.done", events[2].Type)
	require.Equal(t, clipText, events[2].Text)
	require.EqualValues(t, 108, events[2].Usage.TotalTokens)

	body, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, raw)
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"`+clipText+`"}`, string(body))
	require.True(t, meta.Completed)
	require.Equal(t, llm.AccountingEvidenceExplicitFinal, meta.AccountingEvidence)
	require.EqualValues(t, 90, meta.Usage.PromptTokens)
	require.InDelta(t, 8, meta.Usage.DurationSeconds, 0.001)
}

func TestMessage_EnglishSentencesAreJoinedWithASpace(t *testing.T) {
	fake := newFakeInference(t)
	fake.set(func(f *fakeInference) {
		f.sentences = []fakeSentence{
			{text: "Hello there.", begin: 0, end: 900, usage: `{"duration":1,"input_tokens":20,"output_tokens":3,"total_tokens":23}`},
			{text: "See you tomorrow.", begin: 1500, end: 2600, usage: `{"duration":3,"input_tokens":50,"output_tokens":7,"total_tokens":57}`},
		}
	})
	out := newMessageOutbound(t, fake.baseURL(), -1)

	responses, _, _ := runStream(t.Context(), t, out, messageRequest("text", true, "voice.wav", toneWAV(16000, 1, 0.2)))

	events := transcriptionEvents(responses)
	require.Equal(t, "Hello there.", events[0].Delta)
	require.Equal(t, " See you tomorrow.", events[1].Delta)
	require.Equal(t, "Hello there. See you tomorrow.", events[2].Text)
}

func TestMessage_CompressedAudioIsSentAsIs(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), -1)

	mp3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0xFF, 0xFB, 0x90, 0x00}, 2000)...)

	_, err := runNonStream(t.Context(), t, out, messageRequest("text", false, "note.mp3", mp3))
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Equal(t, "mp3", gjson.Get(calls.runTasks[0], "payload.parameters.format").String())
	require.EqualValues(t, 16000, gjson.Get(calls.runTasks[0], "payload.parameters.sample_rate").Int())
	require.Equal(t, mp3, calls.audio)
}

func TestMessage_FramesArePaced(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), 20*time.Millisecond)

	// 1 s of 16 kHz mono = 10 frames; 20 ms between frames is 5x real time.
	started := time.Now()
	_, err := runNonStream(t.Context(), t, out, messageRequest("text", false, "voice.wav", toneWAV(16000, 1, 1)))
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Len(t, calls.frames, 10)
	require.GreaterOrEqual(t, time.Since(started), 180*time.Millisecond)
	require.GreaterOrEqual(t, calls.frameTimes[9].Sub(calls.frameTimes[0]), 170*time.Millisecond)

	if os.Getenv("AXONHUB_BAILIAN_ASR_MESSAGE_PACE") == "" {
		require.Equal(t, 25*time.Millisecond, Config{}.messageFrameInterval(), "4x real time by default")
	}

	require.Zero(t, Config{MessageFrameInterval: -1}.messageFrameInterval())
}

func TestMessage_RequestValidation(t *testing.T) {
	out := newMessageOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1", -1)
	out.cfg.MessageMaxDuration = 2 * time.Second

	cases := map[string]struct {
		req  *llm.Request
		want string
	}{
		"too long wav":         {messageRequest("", false, "a.wav", toneWAV(16000, 1, 3)), "accepts at most 2s"},
		"too large compressed": {messageRequest("", false, "a.mp3", append([]byte("ID3"), make([]byte, 90000)...)), "limited to 80000 bytes"},
		"flac":                 {messageRequest("", false, "a.flac", []byte("fLaC0000")), "unsupported audio format"},
		"ogg vorbis":           {messageRequest("", false, "a.ogg", []byte("OggS\x00\x02vorbis")), "unsupported audio format"},
		"amr-wb":               {messageRequest("", false, "a.amr", []byte("#!AMR-WB\n")), "unsupported audio format"},
		"diarization":          {messageRequest("diarized_json", false, "a.wav", toneWAV(16000, 1, 0.1)), "does not support speaker diarization"},
		"srt stream":           {messageRequest("srt", true, "a.wav", toneWAV(16000, 1, 0.1)), "stream=true supports"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := out.TransformRequest(t.Context(), tc.req)
			require.ErrorIs(t, err, transformer.ErrInvalidRequest)
			require.ErrorContains(t, err, tc.want)
		})
	}

	ctx := WithAudioSource(t.Context(), NewURLSource("https://media.example.com/a.wav"))
	_, err := out.TransformRequest(ctx, messageRequest("", false, "a.wav", nil))
	require.ErrorIs(t, err, transformer.ErrInvalidRequest)
	require.ErrorContains(t, err, "url input is not supported")
}

func TestMessage_HeadlessPCMUsesTheSampleRateField(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), -1)

	pcm := toneWAV(8000, 1, 0.5)[44:]
	req := messageRequest("text", false, "voice.pcm", pcm)
	req.Transcription.Extra = map[string][]string{"sample_rate": {"8000"}}

	_, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Equal(t, "pcm", gjson.Get(calls.runTasks[0], "payload.parameters.format").String())
	require.InDelta(t, 16000, len(calls.audio), 8, "8 kHz upsampled to 16 kHz")
}

func TestMessage_PersistedBodyCarriesNoAudio(t *testing.T) {
	out := newMessageOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1", -1)

	wav := toneWAV(16000, 1, 0.2)
	httpReq := buildProviderRequest(t.Context(), t, out, messageRequest("", false, "a.wav", wav))

	require.Equal(t, "https://dashscope.aliyuncs.com/api-ws/v1/inference", httpReq.URL)
	require.Equal(t, httpReq.Body, httpReq.JSONBody)
	require.Less(t, len(httpReq.Body), 400)
	require.False(t, bytes.Contains(httpReq.Body, wav[44:80]))
	require.Equal(t, "wss://dashscope.aliyuncs.com/api-ws/v1/inference", websocketURL(httpReq.URL))
}

func TestMessage_TaskFailedAfterAudioIsNotRetryable(t *testing.T) {
	fake := newFakeInference(t)
	fake.set(func(f *fakeInference) { f.failAfterAudio = [2]string{"InternalError", "decoder crashed"} })
	out := newMessageOutbound(t, fake.baseURL(), -1)

	_, err := runNonStream(t.Context(), t, out, messageRequest("", false, "a.wav", toneWAV(16000, 1, 1)))

	respErr := responseError(t, err)
	require.Equal(t, http.StatusFailedDependency, respErr.StatusCode, "audio was billed: no retry")
	require.Contains(t, respErr.Detail.Message, "decoder crashed")
}

func TestMessage_HandshakeRejectionKeepsDashScopeError(t *testing.T) {
	fake := newFakeInference(t)
	fake.set(func(f *fakeInference) { f.rejectHandshake = http.StatusUnauthorized })
	out := newMessageOutbound(t, fake.baseURL(), -1)

	_, err := runNonStream(t.Context(), t, out, messageRequest("", false, "a.wav", toneWAV(16000, 1, 0.1)))

	respErr := responseError(t, err)
	require.Equal(t, http.StatusUnauthorized, respErr.StatusCode)
	require.Equal(t, "InvalidApiKey", respErr.Detail.Code)
	require.Equal(t, "authentication_error", respErr.Detail.Type)
}

func TestMessage_ClosingTheStreamStopsTheTask(t *testing.T) {
	fake := newFakeInference(t)
	out := newMessageOutbound(t, fake.baseURL(), 50*time.Millisecond)

	httpReq := buildProviderRequest(t.Context(), t, out, messageRequest("", true, "a.wav", toneWAV(16000, 1, 30)))

	stream, err := executor(out).DoStream(t.Context(), httpReq)
	require.NoError(t, err)
	require.True(t, stream.Next())
	require.Equal(t, committedEventType, stream.Current().Type)

	closed := make(chan struct{})
	go func() {
		_ = stream.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the task")
	}

	// Frames written before Close may still be on their way through the socket: count them once
	// the fake has seen the connection end, then check that nothing follows.
	require.Eventually(t, func() bool { return fake.snapshot().sessionsEnded == 1 }, 5*time.Second, 10*time.Millisecond)

	sent := len(fake.snapshot().frames)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, sent, len(fake.snapshot().frames), "no audio after Close")
	require.Less(t, sent, 300)
}

func TestReadWAVHeader_Layouts(t *testing.T) {
	wav := withListChunk(toneWAV(48000, 2, 0.1))

	info, err := readWAVHeader(bytes.NewReader(wav), int64(len(wav)))
	require.NoError(t, err)
	require.NoError(t, info.validate())
	require.Equal(t, 2, info.Channels)
	require.Equal(t, 48000, info.SampleRate)
	require.EqualValues(t, 4800*4, info.DataSize)
	require.InDelta(t, 0.1, info.seconds(), 0.0001)

	// A streamed WAV leaves the data size at 0: the rest of the file is the data.
	streamed := toneWAV(16000, 1, 0.1)
	binary.LittleEndian.PutUint32(streamed[40:44], 0)
	info, err = readWAVHeader(bytes.NewReader(streamed), int64(len(streamed)))
	require.NoError(t, err)
	require.EqualValues(t, 3200, info.DataSize)

	float := toneWAV(16000, 1, 0.1)
	binary.LittleEndian.PutUint16(float[20:22], wavFloat)
	info, err = readWAVHeader(bytes.NewReader(float), int64(len(float)))
	require.NoError(t, err)
	require.Error(t, info.validate(), "16-bit float does not exist")

	_, err = readWAVHeader(bytes.NewReader(wav[:40]), 40)
	require.Error(t, err)
}

func TestPCMConverter_DecodesEncodingsToMono16k(t *testing.T) {
	// Two channels of 24-bit samples: +0.5 and -0.25 full scale average to +0.125.
	frame := []byte{0x00, 0x00, 0x40, 0x00, 0x00, 0xE0}
	data := bytes.Repeat(frame, 160)

	conv := newPCMConverter(io.NopCloser(nil), bytes.NewReader(data), wavPCM, 2, 24, 16000)
	out, err := io.ReadAll(conv)
	require.NoError(t, err)
	require.Len(t, out, 320)
	require.InDelta(t, 4096, int16(binary.LittleEndian.Uint16(out[100:])), 2)

	// mu-law silence (0xFF) decodes to 0.
	conv = newPCMConverter(io.NopCloser(nil), bytes.NewReader(bytes.Repeat([]byte{0xFF}, 80)), wavMuLaw, 1, 8, 8000)
	out, err = io.ReadAll(conv)
	require.NoError(t, err)
	require.InDelta(t, 320, len(out), 4, "80 samples at 8 kHz become 160 at 16 kHz")
	require.Equal(t, make([]byte, len(out)), out)
}

func TestLinearResampler_IsChunkIndependent(t *testing.T) {
	pcm := toneWAV(44100, 1, 0.5)[44:]

	whole := newLinearResampler(44100, 16000).process(pcm)
	require.InDelta(t, 22050*16000/44100, len(whole)/2, 2)

	chunked := newLinearResampler(44100, 16000)

	var parts []byte
	for i := 0; i < len(pcm); i += 1234 {
		parts = append(parts, chunked.process(pcm[i:min(i+1234, len(pcm))])...)
	}

	require.Equal(t, whole, parts)
}

func TestSentenceDelta(t *testing.T) {
	require.Equal(t, "明天", sentenceDelta("今天。", "明天"))
	require.Equal(t, " World.", sentenceDelta("Hello.", "World."))
	require.Equal(t, "World.", sentenceDelta("Hello. ", "World."))
	require.Equal(t, "你好", sentenceDelta("Hello.", "你好"))
	require.Equal(t, "first", sentenceDelta("", "first"))
}

func TestMessageFolder_IgnoresPartialsAndHeartbeats(t *testing.T) {
	folder := &messageFolder{}

	for _, frame := range []string{
		`{"header":{"task_id":"t","event":"result-generated"},"payload":{"output":{"sentence":{"sentence_id":1,"text":"","sentence_end":false}}}}`,
		`{"header":{"task_id":"t","event":"result-generated"},"payload":{"output":{"sentence":{"sentence_id":0,"heartbeat":true,"sentence_end":true,"text":"x"}}}}`,
		`{"header":{"task_id":"t","event":"result-generated"},"payload":{"output":{"sentence":{"sentence_id":1,"text":"好。","sentence_end":true,"begin_time":0,"end_time":500}},"usage":{"duration":1,"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
		`{"header":{"task_id":"t","event":"task-finished"},"payload":{"output":{}}}`,
	} {
		_, err := folder.fold(&httpclient.StreamEvent{Type: gjson.Get(frame, "header.event").String(), Data: []byte(frame)})
		require.NoError(t, err)
	}

	res := folder.result()
	require.Equal(t, "好。", res.text)
	require.True(t, res.final)
	require.EqualValues(t, 12, res.usage.TotalTokens, "a task-finished without usage keeps the last report")
	require.Equal(t, "t", res.id)
}

func TestProducerStream_ErrIsSetWhenNextEnds(t *testing.T) {
	boom := errors.New("boom")

	for range 50 {
		s := newProducerStream(t.Context(), nil, func(context.Context, func(*httpclient.StreamEvent) error) error { return boom })
		require.False(t, s.Next())
		require.ErrorIs(t, s.Err(), boom)
		require.NoError(t, s.Close())
	}
}

func rms(pcm []byte) float64 {
	var sum float64

	n := len(pcm) / 2
	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += v * v
	}

	if n == 0 {
		return 0
	}

	return math.Sqrt(sum / float64(n))
}

// A -message final that covers two sentences (no pause long enough between them, as in the live
// run of 2026-11-06) becomes one segment per sentence, while the text stays one delta.
func TestMessageFolder_SplitsAFinalThatCoversSeveralSentences(t *testing.T) {
	folder := &messageFolder{}

	deltas, err := folder.fold(&httpclient.StreamEvent{Data: []byte(`{"header":{"task_id":"t","event":"result-generated"},"payload":{"output":{"sentence":{"sentence_id":1,"begin_time":160,"end_time":8020,"sentence_end":true,` +
		`"text":"今天天气不错。明天开会。","words":[` +
		`{"begin_time":160,"end_time":640,"text":"今天","punctuation":""},` +
		`{"begin_time":640,"end_time":1920,"text":"天气不错","punctuation":"。"},` +
		`{"begin_time":4720,"end_time":5240,"text":"明天","punctuation":""},` +
		`{"begin_time":5240,"end_time":8020,"text":"开会","punctuation":"。"}]}}}}`)})
	require.NoError(t, err)
	require.Equal(t, []string{"今天天气不错。明天开会。"}, deltas)

	tr := folder.transcript()
	require.Equal(t, "今天天气不错。明天开会。", tr.Text)
	require.Len(t, tr.Segments, 2)
	require.Equal(t, "今天天气不错。", tr.Segments[0].Text)
	require.InDelta(t, 0.16, tr.Segments[0].Start, 1e-9)
	require.InDelta(t, 1.92, tr.Segments[0].End, 1e-9)
	require.Equal(t, "明天开会。", tr.Segments[1].Text)
	require.InDelta(t, 4.72, tr.Segments[1].Start, 1e-9)
	require.InDelta(t, 8.02, tr.Segments[1].End, 1e-9)
}
