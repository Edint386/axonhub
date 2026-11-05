package bailianasr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestAsyncJob_ParaformerUploadSubmitPollResult(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	resp, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.policyCalls)
	require.Equal(t, 1, calls.submitCalls)
	require.Equal(t, 3, calls.pollCalls, "PENDING, RUNNING, SUCCEEDED")
	require.Equal(t, 1, calls.resultCalls)

	// OSS PostObject: policy fields first, file last, no DashScope key, exact Content-Length.
	require.Len(t, calls.uploads, 1)
	upload := calls.uploads[0]
	require.Equal(t, []string{
		"OSSAccessKeyId", "Signature", "policy", "x-oss-object-acl", "x-oss-forbid-overwrite",
		"key", "success_action_status", "file",
	}, upload.fields)
	require.Equal(t, audio, upload.file)
	require.Empty(t, upload.authorization)
	require.Positive(t, upload.contentLength, "the upload must not be chunked")
	require.Equal(t, "LTAI-test", upload.values["OSSAccessKeyId"])
	require.True(t, strings.HasPrefix(upload.values["key"], "dashscope-instant/abc/2026-11-05/paraformer-v2/"))
	require.True(t, strings.HasSuffix(upload.values["key"], ".wav"))

	// Submit: async + OSS resolve headers, the uploaded oss:// URL, language hints.
	header := calls.submitHeaders[0]
	require.Equal(t, "enable", header.Get("X-DashScope-Async"))
	require.Equal(t, "enable", header.Get("X-DashScope-OssResourceResolve"))
	require.Equal(t, "Bearer "+fakeAPIKey, header.Get("Authorization"))

	submit := gjson.Parse(calls.submitBodies[0])
	require.Equal(t, "paraformer-v2", submit.Get("model").String())
	require.Equal(t, "oss://"+upload.values["key"], submit.Get("input.file_urls.0").String())
	require.Equal(t, `["zh"]`, submit.Get("parameters.language_hints").Raw)

	require.Equal(t, []string{"Bearer " + fakeAPIKey, "Bearer " + fakeAPIKey, "Bearer " + fakeAPIKey}, calls.pollAuth)
	require.Equal(t, []string{""}, calls.resultAuth, "the pre-signed result URL gets no credentials")

	require.Equal(t, fakeTaskID, resp.ID)
	require.Equal(t, llm.RequestTypeTranscription, resp.RequestType)
	require.JSONEq(t, `{"text":"`+paraformerTxt+`","usage":{"type":"duration","seconds":8}}`, string(resp.Transcription.Raw))
	require.Equal(t, contentTypeJSON, resp.Transcription.RawContentType)
	require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.001)
	require.Zero(t, resp.Usage.PromptTokens)
}

func TestAsyncJob_SubmitBodyHasNoAudioAndKeepsPlaceholder(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))

	require.Equal(t, "https://dashscope.aliyuncs.com/api/v1/services/audio/asr/transcription", httpReq.URL)
	require.Equal(t, "enable", httpReq.Headers.Get("X-DashScope-Async"))
	require.Equal(t, string(llm.APIFormatOpenAITranscription), httpReq.APIFormat)
	require.Equal(t, httpReq.Body, httpReq.JSONBody)
	require.JSONEq(t, `{"model":"paraformer-v2","input":{"file_urls":["`+pendingUploadURL+`"]},"parameters":{"language_hints":["zh"]}}`,
		string(httpReq.Body))
}

func TestAsyncJob_FailedTaskIsNotRetryableAndForgotten(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		wantStatus int
	}{
		{name: "bad audio", code: "InvalidFile.DecodeFailed", wantStatus: http.StatusBadRequest},
		{name: "free tier exhausted", code: "AllocationQuota.FreeTierOnly", wantStatus: http.StatusForbidden},
		{name: "upstream fault", code: "InternalError.Algo", wantStatus: http.StatusFailedDependency},
		{name: "throttled task", code: "Throttling.RateQuota", wantStatus: http.StatusFailedDependency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDashScope(t)
			fake.set(func(f *fakeDashScope) {
				f.statuses = []string{"PENDING", "FAILED"}
				f.taskFailure = [2]string{tt.code, "task failed"}
			})
			out := newTestOutbound(t, fake.baseURL())
			req := transcriptionRequest("qwen-audio-3.1-asr-flash-filetrans", "", false, wavClip(800))

			_, err := runNonStream(t.Context(), t, out, req)
			respErr := responseError(t, err)
			require.Equal(t, tt.wantStatus, respErr.StatusCode)
			require.False(t, httpclient.IsHTTPStatusCodeRetryable(respErr.StatusCode), "post-submit errors must not be retryable")
			require.Contains(t, respErr.Detail.Message, fakeTaskID)
			require.Equal(t, tt.code, respErr.Detail.Code)

			// A failed task is not billed, so the next request may submit again.
			fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })
			_, err = runNonStream(t.Context(), t, out, req)
			require.NoError(t, err)
			require.Equal(t, 2, fake.snapshot().submitCalls)
		})
	}
}

func TestAsyncJob_RetryAfterDeadlinePollsTheSameTask(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
	out := newTestOutbound(t, fake.baseURL())
	req := transcriptionRequest("paraformer-v2", "srt", false, wavClip(800))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	_, err := runNonStream(ctx, t, out, req)
	respErr := responseError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, respErr.StatusCode)
	require.Contains(t, respErr.Detail.Message, fakeTaskID, "the deadline error names the task")
	require.True(t, errors.Is(err, context.DeadlineExceeded))

	// The retry (a new attempt with the same audio) resumes the running task.
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })

	resp, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err)
	require.Equal(t, contentTypeText, resp.Transcription.RawContentType)
	require.Contains(t, string(resp.Transcription.Raw), paraformerTxt)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls, "a retry must never resubmit the job")
	require.Len(t, calls.uploads, 1)

	// Once finished, a further retry is served from the cached result.
	_, err = runNonStream(t.Context(), t, out, req)
	require.NoError(t, err)
	require.Equal(t, calls.pollCalls, fake.snapshot().pollCalls)
	require.Equal(t, 1, fake.snapshot().resultCalls)
}

func TestAsyncJob_DifferentParametersAreDifferentJobs(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })
	out := newTestOutbound(t, fake.baseURL())

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "json", false, wavClip(800)))
	require.NoError(t, err)

	_, err = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "diarized_json", false, wavClip(800)))
	require.NoError(t, err)

	require.Equal(t, 2, fake.snapshot().submitCalls)
}

func TestAsyncJob_TransientPollErrorsAreRetriedInside(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.pollErrors = 2 })
	out := newTestOutbound(t, fake.baseURL())

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-8k-v2", "text", false, wavClip(800)))
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls)
	require.Equal(t, 5, calls.pollCalls, "two 500s, then PENDING, RUNNING, SUCCEEDED")
}

func TestAsyncJob_ResultDownloadIsRetriedWithoutLeakingTheSignedURL(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) {
		f.statuses = []string{"SUCCEEDED"}
		f.resultErrors = 2
	})
	out := newTestOutbound(t, fake.baseURL())
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	_, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err, "download failures are retried with a fresh poll")
	require.Equal(t, 3, fake.snapshot().resultCalls)

	fake.set(func(f *fakeDashScope) { f.resultErrors = maxResultFetchFailures })

	_, err = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(900)))
	respErr := responseError(t, err)
	require.Equal(t, http.StatusFailedDependency, respErr.StatusCode)
	require.Contains(t, respErr.Detail.Message, fakeTaskID)
	require.NotContains(t, err.Error(), "s3cr3t", "the pre-signed result URL must not leak into errors")
}

func TestAsyncJob_ConcurrentIdenticalRequestsShareOneSubmit(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	var wg sync.WaitGroup

	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, errs[i] = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
		}()
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, 1, fake.snapshot().submitCalls)
	require.Len(t, fake.snapshot().uploads, 1)
}

func TestAsyncJob_NoSpeechSubtaskIsAnEmptyTranscript(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) {
		f.statuses = []string{"SUCCEEDED"}
		f.subtask = `{"file_url":"oss://x","code":"SUCCESS_WITH_NO_VALID_FRAGMENT","message":"no speech","subtask_status":"FAILED"}`
		f.usage = `{"duration":0}`
	})
	out := newTestOutbound(t, fake.baseURL())

	resp, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "text", false, wavClip(800)))
	require.NoError(t, err)
	require.Equal(t, "\n", string(resp.Transcription.Raw), "the body is never empty")
	require.Zero(t, fake.snapshot().resultCalls)
}

func TestAsyncJob_FailedSubtaskInsideSucceededTask(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) {
		f.statuses = []string{"SUCCEEDED"}
		f.subtask = `{"file_url":"oss://x","code":"InvalidFile.DownloadFailed","message":"The audio file cannot be downloaded.","subtask_status":"FAILED"}`
	})
	out := newTestOutbound(t, fake.baseURL())

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
	respErr := responseError(t, err)
	require.Equal(t, http.StatusBadRequest, respErr.StatusCode)
	require.Equal(t, "InvalidFile.DownloadFailed", respErr.Detail.Code)
}

func TestAsyncJob_ExpiredUploadPolicyIsRefreshedOnce(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.expiredPolicies = 1 })
	out := newTestOutbound(t, fake.baseURL())

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
	require.NoError(t, err)
	require.Equal(t, 2, fake.snapshot().policyCalls)
	require.Len(t, fake.snapshot().uploads, 1)

	fake.set(func(f *fakeDashScope) { f.expiredPolicies = 2 })

	_, err = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(900)))
	respErr := responseError(t, err)
	require.Equal(t, http.StatusBadGateway, respErr.StatusCode, "nothing was submitted, so the error stays retryable")
	require.True(t, errors.Is(err, errPolicyExpired))
}

func TestAsyncJob_PreSubmitErrorsKeepUpstreamStatus(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	out.native = fake.server.URL + "/missing" // getPolicy 404s with a DashScope body

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
	respErr := responseError(t, err)
	require.Equal(t, http.StatusNotFound, respErr.StatusCode)
	require.Equal(t, "NotFound", respErr.Detail.Code)
	require.Zero(t, fake.snapshot().submitCalls)
}

type remoteAudio struct {
	url string
}

func (r remoteAudio) Name() string { return "" }
func (r remoteAudio) Size() int64  { return 0 }
func (r remoteAudio) Open() (io.ReadCloser, error) {
	return nil, errors.New("remote audio is not readable")
}
func (r remoteAudio) Digest() (string, error) { return r.url, nil }
func (r remoteAudio) URL() string             { return r.url }

func TestAsyncJob_RemoteAudioSkipsTheUpload(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	ctx := WithAudioSource(t.Context(), remoteAudio{url: "https://media.example.com/meeting.mp3"})

	req := transcriptionRequest("paraformer-v2", "", false, nil)

	_, err := runNonStream(ctx, t, out, req)
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Zero(t, calls.policyCalls)
	require.Empty(t, calls.uploads)
	require.Equal(t, "https://media.example.com/meeting.mp3", gjson.Get(calls.submitBodies[0], "input.file_urls.0").String())
	require.Empty(t, calls.submitHeaders[0].Get("X-DashScope-OssResourceResolve"))
}

func TestAsyncJob_StreamReplaysSentencesAsDeltas(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) {
		f.result = qwenFiletransResult
		f.usage = `{"duration":6,"input_tokens":100,"output_tokens":18,"total_tokens":118}`
	})
	out := newTestOutbound(t, fake.baseURL())

	responses, raw, httpReq := runStream(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash-filetrans", "", true, wavClip(800)))

	events := transcriptionEvents(responses)
	require.Len(t, events, 3)
	require.Equal(t, "今天天气不错，我们去公园散步。", events[0].Delta)
	require.Equal(t, "明天下午三点开会，记得带电脑。", events[1].Delta)
	require.Equal(t, "transcript.text.done", events[2].Type)
	require.Equal(t, clipText, events[2].Text)
	require.Same(t, llm.DoneResponse, responses[len(responses)-1])

	done := responses[len(responses)-2]
	require.EqualValues(t, 100, done.Usage.PromptTokens)
	require.EqualValues(t, 18, done.Usage.CompletionTokens)
	require.InDelta(t, 6, done.Usage.DurationSeconds, 0.001)

	body, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, raw)
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"`+clipText+`"}`, string(body))
	require.Equal(t, fakeTaskID, meta.ID)
	require.True(t, meta.Completed)
	require.Equal(t, llm.AccountingEvidenceExplicitFinal, meta.AccountingEvidence)
	require.EqualValues(t, 118, meta.Usage.TotalTokens)
}

func TestParseTaskState_ReadsMessyShapes(t *testing.T) {
	state := parseTaskState([]byte(`{"request_id":"r","output":{"task_id":"t","task_status":"succeeded",` +
		`"transcription_url":"https://oss.example/fallback.json","results":[{"subtask_status":"SUCCEEDED"}],` +
		`"output":{"task_status":"RUNNING"}},"usage":{"duration":9}}`))
	require.Equal(t, "SUCCEEDED", state.status)
	require.Equal(t, "https://oss.example/fallback.json", state.resultURL)
	require.InDelta(t, 9, state.usage.Duration, 0.001)

	flattened := parseTaskState([]byte(`{"task_status":"FAILED","code":"InvalidParameter","message":"bad"}`))
	require.Equal(t, "FAILED", flattened.status)
	require.Equal(t, "InvalidParameter", flattened.code)
}

func TestJobCache_ExpiresAndStaysBounded(t *testing.T) {
	cache := newJobCache(time.Minute, 2)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	cache.putTask("a", "task-a", nil)
	now = now.Add(10 * time.Second)
	cache.putTask("b", "task-b", nil)
	now = now.Add(10 * time.Second)
	cache.putTask("c", "task-c", nil)

	_, ok := cache.lookup("a")
	require.False(t, ok, "the oldest entry is evicted beyond maxEntries")

	entry, ok := cache.lookup("c")
	require.True(t, ok)
	require.Equal(t, "task-c", entry.taskID)

	now = now.Add(2 * time.Minute)
	_, ok = cache.lookup("c")
	require.False(t, ok, "entries expire after the TTL")
}

func TestAsyncEnvelope_DropsWordsUnlessRequested(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "verbose_json", false, wavClip(800)))
	resp, err := executor(out).Do(t.Context(), httpReq)
	require.NoError(t, err)

	var envelope transcript
	require.NoError(t, json.Unmarshal(resp.Body, &envelope))
	require.Len(t, envelope.Segments, 1)
	require.Empty(t, envelope.Segments[0].Words, "the persisted envelope keeps words only when they are rendered")
}
