package bailianasr

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestFlash_RequestShape(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	audio := wavClip(800)
	req := transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, audio)
	req.Transcription.Prompt = "会议纪要，人名：张三"

	httpReq := buildProviderRequest(t.Context(), t, out, req)
	require.Equal(t, "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation", httpReq.URL)
	require.Equal(t, "Bearer "+fakeAPIKey, httpReq.Headers.Get("Authorization"))
	require.Empty(t, httpReq.Headers.Get("X-DashScope-SSE"))
	require.Equal(t, string(llm.RequestTypeTranscription), httpReq.RequestType)

	body := gjson.ParseBytes(httpReq.Body)
	require.Equal(t, "qwen-audio-3.1-asr-flash", body.Get("model").String())
	require.Equal(t, "user", body.Get("input.messages.0.role").String())
	require.Equal(t, "input_text", body.Get("input.messages.0.content.0.type").String())
	require.Equal(t, "会议纪要，人名：张三", body.Get("input.messages.0.content.0.text").String())
	require.Equal(t, "input_audio", body.Get("input.messages.1.content.0.type").String())
	require.Equal(t, "data:audio/wav;base64,"+base64.StdEncoding.EncodeToString(audio),
		body.Get("input.messages.1.content.0.input_audio.data").String())
	require.JSONEq(t, `{"format":"wav","sample_rate":"16000","language_hints":["zh"]}`, body.Get("parameters").Raw)

	// The persisted copy names the audio instead of carrying it.
	logged := gjson.ParseBytes(httpReq.JSONBody)
	require.Equal(t, "<audio bytes: 1644, format: wav>", logged.Get("input.messages.1.content.0.input_audio.data").String())
	require.NotContains(t, string(httpReq.JSONBody), "base64")
}

func TestFlash_NonStreamSplitsTheAggregatedSentence(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	// 8 s of audio, the length the fake response describes.
	req := transcriptionRequest("qwen-audio-3.1-asr-flash", "verbose_json", false, wavClip(8*16000))
	req.Transcription.Extra = map[string][]string{"timestamp_granularities[]": {"segment", "word"}}

	resp, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err)
	require.Equal(t, 1, fake.snapshot().flashCalls)

	raw := gjson.ParseBytes(resp.Transcription.Raw)
	require.Equal(t, "chinese", raw.Get("language").String())
	require.InDelta(t, 8, raw.Get("duration").Float(), 0.001)
	require.Equal(t, clipText, raw.Get("text").String())
	require.Len(t, raw.Get("segments").Array(), 2)
	require.Equal(t, "今天天气不错，我们去公园散步。", raw.Get("segments.0.text").String())
	require.InDelta(t, 0.16, raw.Get("segments.0.start").Float(), 0.0001)
	require.InDelta(t, 3.2, raw.Get("segments.0.end").Float(), 0.0001)
	require.Equal(t, "明天下午三点开会，记得带电脑。", raw.Get("segments.1.text").String())
	require.Len(t, raw.Get("words").Array(), 4)
	require.Equal(t, "今天天气不错", raw.Get("words.0.word").String())
	require.JSONEq(t, `{"type":"tokens","input_tokens":101,"output_tokens":17,"total_tokens":118}`, raw.Get("usage").Raw)

	require.Equal(t, "req-flash-1", resp.ID)
	require.EqualValues(t, 101, resp.Usage.PromptTokens)
	require.EqualValues(t, 17, resp.Usage.CompletionTokens)
	require.EqualValues(t, 118, resp.Usage.TotalTokens)
	require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.001)
}

func TestFlash_StreamEmitsOnlyNewSuffixes(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())

	responses, raw, httpReq := runStream(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "json", true, wavClip(800)))

	headers := fake.snapshot().flashHeaders[0]
	require.Equal(t, "enable", headers.Get("X-DashScope-SSE"))
	require.Equal(t, "text/event-stream", headers.Get("Accept"))

	events := transcriptionEvents(responses)
	require.Len(t, events, 3)
	require.Equal(t, "transcript.text.delta", events[0].Type)
	require.Equal(t, "今天天气不错，我们去公园散步。", events[0].Delta)
	require.Equal(t, "明天下午三点开会，记得带电脑。", events[1].Delta, "sentence.text is cumulative, only the suffix is new")
	require.Equal(t, "transcript.text.done", events[2].Type)
	require.Equal(t, clipText, events[2].Text)
	require.Equal(t, events[0].Delta+events[1].Delta, events[2].Text)

	done := responses[len(responses)-2]
	require.EqualValues(t, 118, done.Usage.TotalTokens, "usage comes from the last event")
	require.InDelta(t, 8, done.Usage.DurationSeconds, 0.001)
	require.Same(t, done.Usage, events[2].Usage)
	require.Same(t, llm.DoneResponse, responses[len(responses)-1])

	body, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, raw)
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"`+clipText+`"}`, string(body))
	require.Equal(t, "req-flash-sse", meta.ID)
	require.True(t, meta.Completed)
	require.Equal(t, llm.AccountingEvidenceExplicitFinal, meta.AccountingEvidence)
	require.EqualValues(t, 101, meta.Usage.PromptTokens)
}

func TestFlashFolder_NonCumulativeSentencesAreNotLost(t *testing.T) {
	folder := &flashFolder{}

	for _, data := range []string{
		`{"output":{"sentence":{"text":"第一句。","sentence_end":true}},"request_id":"r"}`,
		`{"output":{"sentence":{"text":"第二句。","sentence_end":true}},"usage":{"duration":3},"request_id":"r"}`,
	} {
		_, err := folder.fold(&httpclient.StreamEvent{Data: []byte(data)})
		require.NoError(t, err)
	}

	require.Equal(t, "第一句。第二句。", folder.result().text)
}

func TestFlash_ErrorBodiesMapToResponseErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"request_id":"req-403","code":"AllocationQuota.FreeTierOnly","message":"The free tier of the model has been exhausted."}`))
	}))
	t.Cleanup(server.Close)

	out := newTestOutbound(t, server.URL+"/compatible-mode/v1")

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, wavClip(800)))
	respErr := responseError(t, err)
	require.Equal(t, http.StatusForbidden, respErr.StatusCode)
	require.Equal(t, "AllocationQuota.FreeTierOnly", respErr.Detail.Code)
	require.Equal(t, "permission_error", respErr.Detail.Type)
	require.Equal(t, "req-403", respErr.Detail.RequestID)
	require.True(t, strings.Contains(respErr.Detail.Message, "free tier"))
}

func TestFlash_StreamErrorEventFailsTheStream(t *testing.T) {
	folder := &flashFolder{}

	_, err := folder.fold(&httpclient.StreamEvent{Type: "error", Data: []byte(`{"code":"InvalidParameter","message":"The audio is empty","request_id":"r"}`)})
	respErr := responseError(t, err)
	require.Equal(t, http.StatusBadRequest, respErr.StatusCode)
}

func TestFlash_UnknownAudioFormatIsRejected(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	req := transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, []byte("not audio at all"))
	req.Transcription.FileName = "notes.txt"

	_, err := out.TransformRequest(t.Context(), req)
	require.ErrorContains(t, err, "unsupported audio format")
}
