package bailianasr

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func TestOutbound_DelegatesEverythingElse(t *testing.T) {
	inner := &recordingOutbound{}
	out := Wrap(inner, Config{BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"})

	requests := []*llm.Request{
		{Model: "qwen3.7-max", RequestType: llm.RequestTypeChat},
		transcriptionRequest("qwen3-asr-flash", "", false, wavClip(10)),
		transcriptionRequest("qwen-audio-3.1-asr-flash-streaming", "", false, wavClip(10)),
		{Model: "paraformer-v2", RequestType: llm.RequestTypeTranslation, Translation: &llm.TranslationRequest{File: wavClip(10)}},
	}

	for _, req := range requests {
		httpReq, err := out.TransformRequest(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, "https://inner.example/compatible-mode/v1/chat/completions", httpReq.URL)

		_, err = out.TransformResponse(t.Context(), &httpclient.Response{Request: httpReq})
		require.NoError(t, err)

		_, err = out.TransformStream(t.Context(), httpReq, streams.SliceStream[*httpclient.StreamEvent](nil))
		require.ErrorIs(t, err, errFromInner)

		_, _, err = out.AggregateStreamChunks(t.Context(), httpReq, nil)
		require.ErrorIs(t, err, errFromInner)
	}

	require.Equal(t, len(requests), inner.requests)
	require.Equal(t, len(requests), inner.responses)
	require.Equal(t, llm.APIFormatOpenAIChatCompletion, out.APIFormat())
}

type recordingExecutor struct {
	do, doStream int
}

func (e *recordingExecutor) Do(context.Context, *httpclient.Request) (*httpclient.Response, error) {
	e.do++
	return &httpclient.Response{StatusCode: http.StatusOK}, nil
}

func (e *recordingExecutor) DoStream(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	e.doStream++
	return streams.SliceStream[*httpclient.StreamEvent](nil), nil
}

func TestOutbound_ExecutorPassesOtherRequestsThrough(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	inner := &recordingExecutor{}
	exec := out.CustomizeExecutor(inner)

	chat := &httpclient.Request{URL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"}
	_, err := exec.Do(t.Context(), chat)
	require.NoError(t, err)

	_, err = exec.DoStream(t.Context(), chat)
	require.NoError(t, err)

	// The flash call is a plain HTTP request as well.
	flash := buildProviderRequest(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, wavClip(10)))
	_, err = exec.Do(t.Context(), flash)
	require.NoError(t, err)

	require.Equal(t, 2, inner.do)
	require.Equal(t, 1, inner.doStream)
}

func TestOutbound_TransformErrorReadsNativeDashScopeBodies(t *testing.T) {
	inner := &recordingOutbound{}
	out := Wrap(inner, Config{})

	native := out.TransformError(t.Context(), &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		URL:        "https://dashscope.aliyuncs.com/api/v1/services/audio/asr/transcription",
		Body:       []byte(`{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded","request_id":"req-1"}`),
	})
	require.Equal(t, http.StatusTooManyRequests, native.StatusCode)
	require.Equal(t, "Throttling.RateQuota", native.Detail.Code)
	require.Equal(t, "rate_limit_error", native.Detail.Type)
	require.Equal(t, "req-1", native.Detail.RequestID)

	compatible := out.TransformError(t.Context(), &httpclient.Error{
		StatusCode: http.StatusBadRequest,
		URL:        "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
		Body:       []byte(`{"code":"InvalidParameter","message":"bad"}`),
	})
	require.Equal(t, 418, compatible.StatusCode, "compatible-mode errors stay with the wrapped outbound")
	require.Equal(t, 1, inner.errors)
}

func TestOutbound_VetoesPassThroughForDashScopeRequests(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(10))
	providerReq := buildProviderRequest(t.Context(), t, out, req)

	require.False(t, out.AllowPassThroughBody(t.Context(), req, providerReq))
	require.False(t, out.AllowPassThroughResponse(t.Context(), req, providerReq))

	chat := &httpclient.Request{URL: "https://x/chat/completions"}
	require.True(t, out.AllowPassThroughBody(t.Context(), req, chat))
	require.True(t, out.AllowPassThroughResponse(t.Context(), req, chat))
}

func TestBuildRequest_ParametersPerModel(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1")

	tests := []struct {
		name     string
		model    string
		language string
		format   string
		params   string
	}{
		{name: "paraformer-8k never takes language_hints", model: "paraformer-8k-v2", language: "zh", params: `{}`},
		{name: "paraformer-v2 drops unknown languages", model: "paraformer-v2", language: "es", params: `{}`},
		{name: "paraformer-v2 diarization", model: "paraformer-v2", language: "yue", format: "diarized_json",
			params: `{"language_hints":["yue"],"diarization_enabled":true}`},
		{name: "qwen filetrans normalizes BCP-47", model: "qwen-audio-3.1-asr-flash-filetrans", language: "zh-CN", params: `{"language_hints":["zh"]}`},
		{name: "qwen filetrans diarization", model: "qwen-audio-3.1-asr-flash-filetrans", format: "diarized_json",
			params: `{"speaker_diarization_enabled":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := transcriptionRequest(tt.model, tt.format, false, wavClip(10))
			req.Transcription.Language = tt.language

			httpReq := buildProviderRequest(t.Context(), t, out, req)
			require.Equal(t, "https://dashscope-intl.aliyuncs.com/api/v1/services/audio/asr/transcription", httpReq.URL)
			require.JSONEq(t, tt.params, gjson.GetBytes(httpReq.Body, "parameters").Raw)
		})
	}

	flash := transcriptionRequest("qwen-audio-3.1-asr-flash", "diarized_json", false, wavClip(10))
	httpReq := buildProviderRequest(t.Context(), t, out, flash)
	require.True(t, gjson.GetBytes(httpReq.Body, "parameters.speaker_diarization_enabled").Bool())
}

func TestBuildRequest_RejectsUnsupportedOptions(t *testing.T) {
	out := newTestOutbound(t, "")

	badFormat := transcriptionRequest("paraformer-v2", "xml", false, wavClip(10))
	_, err := out.TransformRequest(t.Context(), badFormat)
	require.ErrorIs(t, err, transformer.ErrInvalidRequest)

	streamSRT := transcriptionRequest("paraformer-v2", "srt", true, wavClip(10))
	_, err = out.TransformRequest(t.Context(), streamSRT)
	require.ErrorIs(t, err, transformer.ErrInvalidRequest)

	noFile := transcriptionRequest("paraformer-v2", "", false, nil)
	_, err = out.TransformRequest(t.Context(), noFile)
	require.ErrorIs(t, err, transformer.ErrInvalidRequest)
}

func TestTimestampGranularities(t *testing.T) {
	words, segments := timestampGranularities(nil)
	require.False(t, words)
	require.True(t, segments, "OpenAI defaults to segment timestamps")

	words, segments = timestampGranularities(map[string][]string{"timestamp_granularities[]": {"word"}})
	require.True(t, words)
	require.False(t, segments)

	words, segments = timestampGranularities(map[string][]string{"timestamp_granularities": {"word,segment"}})
	require.True(t, words)
	require.True(t, segments)
}

func TestNativeBaseURL(t *testing.T) {
	tests := map[string]string{
		"": DefaultNativeBaseURL,
		"https://dashscope.aliyuncs.com/compatible-mode/v1":            "https://dashscope.aliyuncs.com",
		"https://dashscope-intl.aliyuncs.com/compatible-mode/v1/":      "https://dashscope-intl.aliyuncs.com",
		"https://dashscope.aliyuncs.com/apps/anthropic":                "https://dashscope.aliyuncs.com",
		"https://dashscope.aliyuncs.com/api/v1":                        "https://dashscope.aliyuncs.com",
		"https://ws-1.cn-beijing.maas.aliyuncs.com/compatible-mode/v1": "https://ws-1.cn-beijing.maas.aliyuncs.com",
		"https://proxy.example.com/dashscope/compatible-mode/v1":       "https://proxy.example.com/dashscope",
		"https://dashscope.aliyuncs.com":                               "https://dashscope.aliyuncs.com",
	}

	for base, want := range tests {
		require.Equal(t, want, NativeBaseURL(base), base)
	}
}

func TestDetectAudioFormat(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		file string
		want string
	}{
		{name: "wav", head: wavClip(1), want: "wav"},
		{name: "mp3 id3", head: []byte("ID3\x04\x00"), want: "mp3"},
		{name: "mp3 frame", head: []byte{0xFF, 0xFB, 0x90}, want: "mp3"},
		{name: "aac adts", head: []byte{0xFF, 0xF1, 0x50}, want: "aac"},
		{name: "opus", head: []byte("OggS\x00\x02................OpusHead"), want: "opus"},
		{name: "ogg vorbis", head: []byte("OggS\x00\x02......vorbis"), want: "ogg"},
		{name: "flac", head: []byte("fLaC\x00"), want: "flac"},
		{name: "m4a", head: []byte("\x00\x00\x00\x20ftypM4A \x00"), want: "m4a"},
		{name: "mp4", head: []byte("\x00\x00\x00\x20ftypisom\x00"), want: "mp4"},
		{name: "webm", head: []byte{0x1A, 0x45, 0xDF, 0xA3, 'w', 'e', 'b', 'm'}, want: "webm"},
		{name: "amr", head: []byte("#!AMR\n"), want: "amr"},
		{name: "extension fallback", head: []byte("????"), file: "voice.MPGA", want: "mp3"},
		{name: "url extension", file: "https://media.example.com/a/b.wma?sig=1", want: "wma"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := detectAudioFormat(tt.head, tt.file)
			require.True(t, ok)
			require.Equal(t, tt.want, got.Name)
		})
	}

	_, ok := detectAudioFormat([]byte("????"), "notes.txt")
	require.False(t, ok)

	rate, ok := wavSampleRate(wavClip(1))
	require.True(t, ok)
	require.Equal(t, "16000", rate)

	_, ok = wavSampleRate([]byte("ID3"))
	require.False(t, ok)
}

func TestWithAudioSource_OverridesTheMultipartFile(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	ctx := WithAudioSource(t.Context(), NewBytesSource("big.flac", []byte("fLaC-from-disk")))

	httpReq := buildProviderRequest(ctx, t, out, transcriptionRequest("paraformer-v2", "", false, nil))
	j, ok := jobOf(httpReq, kindAsync)
	require.True(t, ok)
	require.Equal(t, "big.flac", j.audio.Name())
	require.Equal(t, "flac", j.ext)
}

func TestSupportsModel(t *testing.T) {
	for _, model := range []string{
		"qwen-audio-3.1-asr-flash", "qwen-audio-3.1-asr-flash-filetrans", "paraformer-v2", "paraformer-8k-v2",
		"qwen-audio-3.1-asr-flash-message",
	} {
		require.True(t, SupportsModel(model), model)
	}

	require.False(t, SupportsModel("qwen-audio-3.1-asr-flash-streaming"))
	require.False(t, SupportsModel("qwen3-asr-flash"))

	for _, model := range []string{"qwen-audio-3.1-asr-flash-filetrans", "paraformer-v2", "paraformer-8k-v2"} {
		require.True(t, SupportsLargeUpload(model), model)
		require.True(t, SupportsURLInput(model), model)
		require.True(t, StreamsWithKeepAlive(model), model)
	}

	require.False(t, SupportsLargeUpload("qwen-audio-3.1-asr-flash"))
	require.True(t, SupportsURLInput("qwen-audio-3.1-asr-flash"))
	require.False(t, StreamsWithKeepAlive("qwen-audio-3.1-asr-flash"))
	require.False(t, SupportsLargeUpload("qwen-audio-3.1-asr-flash-message"))
	require.False(t, SupportsURLInput("qwen-audio-3.1-asr-flash-message"))
	require.True(t, StreamsWithKeepAlive("qwen-audio-3.1-asr-flash-message"))
	require.False(t, SupportsURLInput("qwen3-asr-flash"))
}

func TestStatusForCodeAndErrorTypes(t *testing.T) {
	require.Equal(t, http.StatusRequestEntityTooLarge, statusForCode("FILE_TOO_LARGE", 0))
	require.Equal(t, http.StatusBadRequest, statusForCode("FILE_DOWNLOAD_FAILED", 0))
	require.Equal(t, 0, statusForCode("FILE_SERVER_ERROR", 0))
	require.Equal(t, http.StatusUnauthorized, statusForCode("InvalidApiKey", 0))
	require.Equal(t, http.StatusNotFound, statusForCode("ModelNotFound", 0))

	unfinished := unfinishedTaskError("t-1", context.Canceled, true)
	require.True(t, errors.Is(unfinished, context.Canceled))
	require.Equal(t, "timeout_error", unfinished.Detail.Type)
	require.False(t, isJobFailure(unfinished), "an unfinished task stays cached for the retry")
	require.True(t, isJobFailure(taskFailedError("t-1", "InternalError", "")))
	require.Equal(t, http.StatusFailedDependency, taskFailedError("t-1", "", "").StatusCode)
}
