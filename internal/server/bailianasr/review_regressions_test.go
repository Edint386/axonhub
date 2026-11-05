package bailianasr

// Regression tests for the 2026-11-05 review of the HTTP file transcription: billed jobs that
// must never run twice, the resume cache, channel settings on the extra DashScope calls,
// accounting of failed flash streams, language hints, durations, resampling and URL redaction.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func newScopedOutbound(t *testing.T, baseURL, scope string, keys auth.APIKeyProvider) *Outbound {
	t.Helper()

	if keys == nil {
		keys = auth.NewStaticKeyProvider(fakeAPIKey)
	}

	out := Wrap(&recordingOutbound{}, Config{
		BaseURL:         baseURL,
		APIKeyProvider:  keys,
		CacheScope:      scope,
		PollInterval:    time.Millisecond,
		MaxPollInterval: 3 * time.Millisecond,
	})
	out.cache = newJobCache(jobCacheTTL, 16)

	return out
}

func TestBilledFailuresCarryNoRetry(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*fakeDashScope)
		code   string
		billed bool
	}{
		{
			name:   "result download keeps failing after SUCCEEDED",
			setup:  func(f *fakeDashScope) { f.statuses, f.resultErrors = []string{"SUCCEEDED"}, maxResultFetchFailures },
			code:   "bailian_result_unavailable",
			billed: true,
		},
		{
			name:   "result file is no transcript",
			setup:  func(f *fakeDashScope) { f.statuses, f.result = []string{"SUCCEEDED"}, "not a transcript" },
			code:   "bailian_result_invalid",
			billed: true,
		},
		{
			name:   "task can no longer be queried",
			setup:  func(f *fakeDashScope) { f.pollReject = http.StatusUnauthorized },
			code:   "InvalidApiKey",
			billed: true,
		},
		{
			name: "task failed",
			setup: func(f *fakeDashScope) {
				f.statuses, f.taskFailure = []string{"FAILED"}, [2]string{"InternalError.Algo", "task failed"}
			},
			code: "InternalError.Algo",
		},
		{
			name: "subtask failed",
			setup: func(f *fakeDashScope) {
				f.statuses = []string{"SUCCEEDED"}
				f.subtask = `{"file_url":"oss://x","code":"InvalidFile.DownloadFailed","message":"no","subtask_status":"FAILED"}`
			},
			code: "InvalidFile.DownloadFailed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDashScope(t)
			fake.set(tt.setup)
			out := newTestOutbound(t, fake.baseURL())

			_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
			respErr := responseError(t, err)
			require.Equal(t, tt.code, respErr.Detail.Code)
			require.Equal(t, tt.billed, errors.Is(err, pipeline.ErrNoRetry), "billed failures must end the whole pipeline")
			require.Equal(t, !tt.billed, isJobFailure(err),
				"the cache forgets only a task that failed unbilled; a billed one is polled again by a resend")
			require.NotEqual(t, http.StatusUnauthorized, respErr.StatusCode, "a rejected task query is not the caller's key failing")
		})
	}

	t.Run("deadline while the task runs", func(t *testing.T) {
		fake := newFakeDashScope(t)
		fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
		out := newTestOutbound(t, fake.baseURL())

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		_, err := runNonStream(ctx, t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
		require.ErrorIs(t, err, pipeline.ErrNoRetry)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("-message task fails after its audio", func(t *testing.T) {
		fake := newFakeInference(t)
		fake.set(func(f *fakeInference) { f.failAfterAudio = [2]string{"InternalError", "decoder crashed"} })
		out := newMessageOutbound(t, fake.baseURL(), -1)

		_, err := runNonStream(t.Context(), t, out, messageRequest("", false, "a.wav", toneWAV(16000, 1, 1)))
		require.ErrorIs(t, err, pipeline.ErrNoRetry)
	})

	t.Run("-message handshake rejected", func(t *testing.T) {
		fake := newFakeInference(t)
		fake.set(func(f *fakeInference) { f.rejectHandshake = http.StatusUnauthorized })
		out := newMessageOutbound(t, fake.baseURL(), -1)

		_, err := runNonStream(t.Context(), t, out, messageRequest("", false, "a.wav", toneWAV(16000, 1, 0.1)))
		require.Error(t, err)
		require.NotErrorIs(t, err, pipeline.ErrNoRetry, "nothing was sent, so another attempt may run")
	})
}

// channelSwitcher is a pipeline outbound over several Bailian channels: it offers a
// same-channel retry for every error and switches channels like the orchestrator does.
type channelSwitcher struct {
	channels           []*Outbound
	current            int
	switches           int
	sameChannelRetries int
}

var (
	_ pipeline.Retryable                 = (*channelSwitcher)(nil)
	_ pipeline.ChannelRetryable          = (*channelSwitcher)(nil)
	_ pipeline.ChannelCustomizedExecutor = (*channelSwitcher)(nil)
)

func (s *channelSwitcher) cur() *Outbound { return s.channels[s.current] }

func (s *channelSwitcher) APIFormat() llm.APIFormat { return s.cur().APIFormat() }

func (s *channelSwitcher) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	return s.cur().TransformRequest(ctx, req)
}

func (s *channelSwitcher) TransformResponse(ctx context.Context, resp *httpclient.Response) (*llm.Response, error) {
	return s.cur().TransformResponse(ctx, resp)
}

func (s *channelSwitcher) TransformStream(
	ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*llm.Response], error) {
	return s.cur().TransformStream(ctx, req, stream)
}

func (s *channelSwitcher) TransformError(ctx context.Context, err *httpclient.Error) *llm.ResponseError {
	return s.cur().TransformError(ctx, err)
}

func (s *channelSwitcher) AggregateStreamChunks(
	ctx context.Context, req *httpclient.Request, chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return s.cur().AggregateStreamChunks(ctx, req, chunks)
}

func (s *channelSwitcher) CustomizeExecutor(executor pipeline.Executor) pipeline.Executor {
	return s.cur().CustomizeExecutor(executor)
}

func (s *channelSwitcher) HasMoreChannels() bool { return s.current+1 < len(s.channels) }

func (s *channelSwitcher) NextChannel(context.Context) error {
	s.current++
	s.switches++

	return nil
}

func (s *channelSwitcher) CanRetry(error) bool { return true }

func (s *channelSwitcher) PrepareForRetry(context.Context) error {
	s.sameChannelRetries++
	return nil
}

func multipartTranscription(t *testing.T, model string, audio []byte) *httpclient.Request {
	t.Helper()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", model))

	part, err := writer.CreateFormFile("file", "clip.wav")
	require.NoError(t, err)
	_, err = part.Write(audio)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	return &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "/v1/audio/transcriptions",
		Path:    "/v1/audio/transcriptions",
		Headers: http.Header{"Content-Type": []string{writer.FormDataContentType()}},
		Body:    body.Bytes(),
	}
}

// TestPipeline_BilledJobIsNeverResubmitted runs the real pipeline over two Bailian channels with
// every retry kind on: once a job was billed, no other attempt may upload and submit it again.
func TestPipeline_BilledJobIsNeverResubmitted(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses, f.resultErrors = []string{"SUCCEEDED"}, 1000 })

	switcher := &channelSwitcher{channels: []*Outbound{
		newScopedOutbound(t, fake.baseURL(), "channel:1", nil),
		newScopedOutbound(t, fake.baseURL(), "channel:2", nil),
	}}
	p := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(openai.NewTranscriptionInboundTransformer(), switcher,
		pipeline.WithRetry(3, 3, 0))

	_, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, 1, fake.snapshot().submitCalls, "the billed job ran once")
	require.Len(t, fake.snapshot().uploads, 1)
	require.Zero(t, switcher.switches)
	require.Zero(t, switcher.sameChannelRetries)

	// A task that failed unbilled may still run again.
	unbilled := newFakeDashScope(t)
	unbilled.set(func(f *fakeDashScope) {
		f.statuses, f.taskFailure = []string{"FAILED", "SUCCEEDED"}, [2]string{"InternalError.Algo", "try again"}
	})

	switcher = &channelSwitcher{channels: []*Outbound{
		newScopedOutbound(t, unbilled.baseURL(), "channel:1", nil),
		newScopedOutbound(t, unbilled.baseURL(), "channel:2", nil),
	}}
	p = pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(openai.NewTranscriptionInboundTransformer(), switcher,
		pipeline.WithRetry(3, 3, 0))

	result, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.NoError(t, err)
	require.Contains(t, string(result.Response.Body), paraformerTxt)
	require.Equal(t, 2, unbilled.snapshot().submitCalls)
}

// TestLongRunningJobsAreMarkedForThePipeline: every kind is exempt from the pipeline's
// per-attempt response timeouts, the flash call too (it answers after about twice the audio
// length, all SSE events at the end; a stream_first_event_timeout would cut it and send the same
// audio to the next channel, paying again).
func TestLongRunningJobsAreMarkedForThePipeline(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")

	for _, model := range []string{"paraformer-v2", "qwen-audio-3.1-asr-flash-filetrans", messageModel, "qwen-audio-3.1-asr-flash"} {
		for _, stream := range []bool{false, true} {
			httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest(model, "", stream, toneWAV(16000, 1, 0.2)))
			require.Equal(t, "true", httpReq.Metadata[pipeline.MetadataLongRunningJob], "%s stream=%v", model, stream)
		}
	}
}

func TestJobCache_ResumeWindowStartsWhenTheRequestEnds(t *testing.T) {
	cache := newJobCache(30*time.Minute, 16)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	e := &jobExecutor{inner: httpclient.NewHttpClient(), cfg: Config{PollInterval: time.Hour}, cache: cache}
	task := taskRef{id: "task-1", headers: http.Header{"Authorization": []string{"Bearer sk-first"}}}

	cache.putTask("key", task.id, task.headers) // submitted at 10:00

	// The request ends at 10:20 with the task still running.
	now = now.Add(20 * time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := e.finish(ctx, &job{audio: NewBytesSource("a.wav", []byte("RIFF"))}, "key", task)
	require.Equal(t, http.StatusGatewayTimeout, responseError(t, err).StatusCode)

	// A resend at 10:35 (35 minutes after the submit) is within the promised 30 minutes.
	now = now.Add(15 * time.Minute)
	entry, ok := cache.lookup("key")
	require.True(t, ok, "the resume window starts when the request ends")
	require.Equal(t, "task-1", entry.taskID)
	require.Equal(t, "Bearer sk-first", entry.headers.Get("Authorization"))
}

func TestJobCache_TouchKeepsResultsAndEvictionSparesRunningTasks(t *testing.T) {
	cache := newJobCache(time.Minute, 2)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	result := &transcript{ID: "done", Text: "好。"}
	cache.putResult("finished", "task-f", result)
	cache.touch("finished", taskRef{id: "task-f"})

	entry, ok := cache.lookup("finished")
	require.True(t, ok)
	require.Same(t, result, entry.result, "touch must not drop a stored result")

	now = now.Add(time.Second)
	cache.putTask("running", "task-r", nil)
	now = now.Add(time.Second)
	cache.putTask("newest", "task-n", nil)

	_, ok = cache.lookup("running")
	require.True(t, ok, "a task that may still run outlives an older finished result")
	_, ok = cache.lookup("finished")
	require.False(t, ok)
}

// rotatingKeys hands out its keys in turn, like a multi-key channel without a sticky trace.
type rotatingKeys struct {
	mu   sync.Mutex
	keys []string
	next int
}

func (r *rotatingKeys) Get(context.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := r.keys[r.next%len(r.keys)]
	r.next++

	return key
}

func TestAsyncJob_ResendThroughAnotherKeyResumesTheTask(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
	out := newScopedOutbound(t, fake.baseURL(), "channel:1", &rotatingKeys{keys: []string{fakeAPIKey, "sk-second-key"}})
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := runNonStream(ctx, t, out, req)
	require.Equal(t, http.StatusGatewayTimeout, responseError(t, err).StatusCode)

	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })

	resp, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err, "the resend arrives with the channel's second key")
	require.Contains(t, string(resp.Transcription.Raw), paraformerTxt)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls, "the resend resumes the task instead of paying again")
	require.Len(t, calls.uploads, 1)

	for _, authorization := range calls.pollAuth {
		require.Equal(t, "Bearer "+fakeAPIKey, authorization, "the task is polled with the key that submitted it")
	}
}

func TestAsyncJob_URLResultsAreNotReused(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
	out := newTestOutbound(t, fake.baseURL())
	req := transcriptionRequest("paraformer-v2", "", false, nil)
	ctx := WithAudioSource(t.Context(), NewURLSource("https://media.example.com/room-3/latest.m4a"))

	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_, err := runNonStream(deadline, t, out, req)
	respErr := responseError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, respErr.StatusCode)
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Contains(t, respErr.Detail.Message, "not resumed", "the 504 must not promise a resume for URL input")

	// The content behind the URL may have changed since: neither the running task nor a result
	// is handed to a later request, which submits again.
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })

	_, err = runNonStream(ctx, t, out, req)
	require.NoError(t, err)
	require.Equal(t, 2, fake.snapshot().submitCalls)

	_, err = runNonStream(ctx, t, out, req)
	require.NoError(t, err)
	require.Equal(t, 3, fake.snapshot().submitCalls)
	require.Empty(t, fake.snapshot().uploads, "URL audio is never uploaded")
}

func TestSideCallsCarryChannelHeaderOverrides(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
	httpReq.Headers.Set("X-Relay-Token", "relay-1") // as applyOverrideRequestHeaders sets it

	_, err := executor(out).Do(t.Context(), httpReq)
	require.NoError(t, err)

	calls := fake.snapshot()
	require.Equal(t, []string{"relay-1"}, calls.policyRelay)
	require.NotEmpty(t, calls.pollRelay)

	for _, relay := range calls.pollRelay {
		require.Equal(t, "relay-1", relay)
	}

	require.Empty(t, calls.resultAuth[0], "the pre-signed result URL still gets no headers")

	inference := newFakeInference(t)
	message := newMessageOutbound(t, inference.baseURL(), -1)

	msgReq := buildProviderRequest(t.Context(), t, message, messageRequest("", false, "a.wav", toneWAV(16000, 1, 0.2)))
	msgReq.Headers.Set("X-Relay-Token", "relay-1")
	msgReq.Headers.Set("Sec-WebSocket-Version", "99") // a handshake header override must not break the dial

	_, err = executor(message).Do(t.Context(), msgReq)
	require.NoError(t, err)
	require.Equal(t, []string{"relay-1"}, inference.snapshot().relayTokens)
}

// startSOCKS5 runs a minimal no-auth SOCKS5 CONNECT proxy and counts its connections.
func startSOCKS5(t *testing.T) (string, *atomic.Int32) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var connections atomic.Int32

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			connections.Add(1)

			go serveSOCKS5(conn)
		}
	}()

	return ln.Addr().String(), &connections
}

func serveSOCKS5(conn net.Conn) {
	defer conn.Close()

	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
		return
	}

	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}

	var host string

	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return
		}

		host = net.IP(ip).String()
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return
		}

		name := make([]byte, size[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}

		host = string(name)
	default:
		return
	}

	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}

	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
	if err != nil {
		return
	}
	defer target.Close()

	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	done := make(chan struct{}, 2)

	go func() { _, _ = io.Copy(target, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, target); done <- struct{}{} }()

	<-done
}

func TestMessage_WebSocketGoesThroughTheChannelSOCKSProxy(t *testing.T) {
	fake := newFakeInference(t)
	proxyAddr, connections := startSOCKS5(t)
	out := newMessageOutbound(t, fake.baseURL(), -1)

	client := httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "socks5://" + proxyAddr})
	httpReq := buildProviderRequest(t.Context(), t, out, messageRequest("", false, "a.wav", toneWAV(16000, 1, 0.2)))

	_, err := out.CustomizeExecutor(client).Do(t.Context(), httpReq)
	require.NoError(t, err)
	require.Positive(t, connections.Load(), "the handshake must use the channel's SOCKS proxy")
	require.Len(t, fake.snapshot().runTasks, 1)
}

func flashEvent(data string) *httpclient.StreamEvent {
	return &httpclient.StreamEvent{Type: "result", Data: []byte(data)}
}

const (
	flashFirstFinal = `{"output":{"sentence":{"sentence_id":1,"text":"今天天气不错，我们去公园散步。","sentence_end":true}},` +
		`"usage":{"duration":4},"request_id":"req-flash-sse"}`
	flashLastFinal = `{"output":{"sentence":{"sentence_id":2,"text":"` + clipText + `","sentence_end":true}},` +
		`"usage":{"duration":8,"input_tokens":101,"output_tokens":17,"total_tokens":118},"request_id":"req-flash-sse"}`
	flashErrorEvent = `{"code":"InternalError","message":"the model failed","request_id":"req-flash-sse"}`
)

func TestFlashAggregate_FailedOrCutStreamIsNotBilled(t *testing.T) {
	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", true, wavClip(800)))

	for name, chunks := range map[string][]*httpclient.StreamEvent{
		"in-band error after the first sentence":  {flashEvent(flashFirstFinal), flashEvent(flashErrorEvent)},
		"connection cut after the first sentence": {flashEvent(flashFirstFinal)},
	} {
		_, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, chunks)
		require.NoError(t, err, name)
		require.False(t, meta.Completed, name)
		require.Nil(t, meta.Usage, "%s: a partial usage must not be billed", name)
		require.NotEqual(t, llm.AccountingEvidenceExplicitFinal, meta.AccountingEvidence, name)
	}

	_, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, []*httpclient.StreamEvent{flashEvent(flashFirstFinal), flashEvent(flashLastFinal)})
	require.NoError(t, err)
	require.True(t, meta.Completed)
	require.Equal(t, llm.AccountingEvidenceExplicitFinal, meta.AccountingEvidence)
	require.EqualValues(t, 118, meta.Usage.TotalTokens)
}

func TestFlashFolder_RevisedCumulativeTextIsNotRepeated(t *testing.T) {
	folder := &flashFolder{}

	var deltas []string

	for _, text := range []string{
		"今天天气不错，我们去公园散步。",
		// ITN and punctuation revise the first sentence while the second one is added.
		"今天天气不错,我们去公园散步。明天下午3点开会，",
		"今天天气不错,我们去公园散步。明天下午3点开会，记得带电脑。",
	} {
		got, err := folder.fold(flashEvent(`{"output":{"sentence":{"text":"` + text + `","sentence_end":true}},"request_id":"r"}`))
		require.NoError(t, err)

		deltas = append(deltas, got...)
	}

	require.Equal(t, []string{"今天天气不错，我们去公园散步。", "明天下午3点开会，", "记得带电脑。"}, deltas)
	require.Equal(t, "今天天气不错,我们去公园散步。明天下午3点开会，记得带电脑。", folder.result().text,
		"the final text is the latest cumulative text")
}

func TestLanguageHints_PerFamilyAllowLists(t *testing.T) {
	tests := []struct {
		family family
		lang   string
		want   []string
	}{
		{familyQwenFiletrans, "zh", []string{"zh"}},
		{familyQwenFiletrans, "he", nil},
		{familyQwenFiletrans, "yue", nil},
		{familyQwenFiletrans, "tr", nil},
		{familyQwenFiletrans, "nb", []string{"no"}},
		{familyQwenFiletrans, "fil", []string{"tl"}},
		{familyMessage, "uk", nil},
		{familyMessage, "in", []string{"id"}},
		{familyFlash, "tr", []string{"tr"}},
		{familyFlash, "he", nil},
		{familyFlash, "eng", []string{"en"}},
		{familyParaformer, "yue", []string{"yue"}},
		{familyParaformer, "es", nil},
		{familyParaformer8k, "zh", nil},
		{familyFlash, "", nil},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, tt.family.languageHints(tt.lang), "family %d, %q", tt.family, tt.lang)
	}

	out := newTestOutbound(t, "https://dashscope.aliyuncs.com/compatible-mode/v1")

	flash := transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, wavClip(800))
	flash.Transcription.Language = "he-IL"
	httpReq := buildProviderRequest(t.Context(), t, out, flash)
	require.False(t, gjson.GetBytes(httpReq.Body, "parameters.language_hints").Exists(), "an unknown code means auto-detect")

	message := messageRequest("", false, "a.wav", toneWAV(16000, 1, 0.2))
	message.Transcription.Language = "nb"
	httpReq = buildProviderRequest(t.Context(), t, out, message)
	require.Equal(t, `["no"]`, gjson.GetBytes(httpReq.Body, "payload.parameters.language_hints").Raw)
}

func TestExactAudioDurationIsReported(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())

	// The fake answers with usage.duration 8 (billed whole seconds) for 7.5 s of audio.
	resp, err := runNonStream(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "verbose_json", false, wavClip(120000)))
	require.NoError(t, err)
	require.InDelta(t, 7.5, gjson.GetBytes(resp.Transcription.Raw, "duration").Float(), 0.0001)
	require.InDelta(t, 7.5, *resp.Transcription.Duration, 0.0001)
	require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.0001, "billing keeps DashScope's seconds")

	inference := newFakeInference(t)
	message := newMessageOutbound(t, inference.baseURL(), -1)

	resp, err = runNonStream(t.Context(), t, message, messageRequest("verbose_json", false, "a.wav", toneWAV(16000, 1, 1.5)))
	require.NoError(t, err)
	require.InDelta(t, 1.5, gjson.GetBytes(resp.Transcription.Raw, "duration").Float(), 0.0001)
	require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.0001)
}

// toneAmplitude estimates the amplitude of freq in mono PCM16 (a single DFT bin).
func toneAmplitude(pcm []byte, freq, rate float64) float64 {
	n := len(pcm) / 2

	var re, im float64

	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		angle := 2 * math.Pi * freq * float64(i) / rate
		re += v * math.Cos(angle)
		im -= v * math.Sin(angle)
	}

	return 2 * math.Hypot(re, im) / float64(n)
}

func sinePCM(freq, rate float64, samples int, amplitude float64) []byte {
	pcm := make([]byte, samples*2)
	for i := range samples {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(amplitude*math.Sin(2*math.Pi*freq*float64(i)/rate))))
	}

	return pcm
}

func TestMessageResampler_44kHzIsAntiAliased(t *testing.T) {
	const amplitude = 10000

	resample := newMessageResampler(44100)

	var out []byte
	// 1 s of a 10 kHz tone, in uneven chunks: it would fold back to 6 kHz at 16 kHz.
	in := sinePCM(10000, 44100, 44100, amplitude)
	for i := 0; i < len(in); i += 3001 * 2 {
		out = append(out, resample(in[i:min(i+3001*2, len(in))])...)
	}

	require.InDelta(t, 16000, len(out)/2, 8)
	require.Less(t, toneAmplitude(out, 6000, 16000), 0.05*amplitude, "the 10 kHz tone must not alias into the speech band")

	speech := newMessageResampler(44100)(sinePCM(1000, 44100, 44100, amplitude))
	require.Greater(t, toneAmplitude(speech, 1000, 16000), 0.9*amplitude, "speech frequencies pass")
}

func TestSentenceDelta_KoreanGetsASpace(t *testing.T) {
	require.Equal(t, " 반갑습니다.", sentenceDelta("안녕하세요.", "반갑습니다."))
	require.Equal(t, "明天", sentenceDelta("今天。", "明天"))
	require.Equal(t, "です。", sentenceDelta("今日は", "です。"))
}

func TestRedactURL(t *testing.T) {
	require.Equal(t, "https://bucket.oss.example.com/<redacted>.mp3?<redacted>",
		RedactURL("https://user:pw@bucket.oss.example.com/calls/a.mp3?Expires=1&OSSAccessKeyId=x&Signature=s3cr3t#t"))
	require.Equal(t, "https://media.example.com/<redacted>.wav", RedactURL("https://media.example.com/a.wav"))
	require.Equal(t, "<redacted>", RedactURL("not a url"))

	// A credential in the path (a Telegram bot token, a share-link secret) is dropped as well;
	// only the extension stays, for the format (see TestRedactURL_FileNamesAreNotKept).
	require.Equal(t, "https://api.telegram.org/<redacted>.oga",
		RedactURL("https://api.telegram.org/file/bot123456:AAHsecretToken/voice/file_7.oga"))
	require.Equal(t, "https://www.dropbox.com/<redacted>.mp3?<redacted>",
		RedactURL("https://www.dropbox.com/s/k3yS3cr3t/talk.mp3?dl=1"))
	require.Equal(t, "https://media.example.com", RedactURL("https://media.example.com"))

	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	signed := "https://media.example.com/hello.wav?X-Amz-Signature=s3cr3t"
	ctx := WithAudioSource(t.Context(), NewURLSource(signed))

	httpReq := buildProviderRequest(ctx, t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, nil))
	require.NotContains(t, string(httpReq.JSONBody), "s3cr3t", "the logged flash body must not keep a signed URL")
	require.Contains(t, string(httpReq.Body), signed, "DashScope still gets the real URL")
}
