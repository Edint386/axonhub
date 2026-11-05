package bailianasr

// Regression tests for the third 2026-11-05 review round of the HTTP file transcription: a
// resend that reaches another Bailian channel resumes its task, the job cache is per caller and
// per DashScope workspace, a cached result bills its usage once, a task this request submitted
// that reads UNKNOWN is never submitted again, a submit outlives the client that sent it, a
// rejected query with another request's key is not this channel's failure, touch keeps another
// task's entry, and stored audio URLs keep no file name.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/pipeline"
)

// sharedCacheOutbounds are two Bailian channels of one gateway: the same DashScope endpoint,
// their own keys, and the process-wide job cache (here one cache for the test).
func sharedCacheOutbounds(t *testing.T, baseURL string, keys ...string) (*Outbound, *Outbound) {
	t.Helper()

	cache := newJobCache(jobCacheTTL, 16)
	outs := make([]*Outbound, 2)

	for i := range outs {
		outs[i] = Wrap(&recordingOutbound{}, Config{
			BaseURL:         baseURL,
			APIKeyProvider:  auth.NewStaticKeyProvider(keys[i]),
			PollInterval:    time.Millisecond,
			MaxPollInterval: 3 * time.Millisecond,
		})
		outs[i].cache = cache
	}

	return outs[0], outs[1]
}

func withCaller(ctx context.Context, apiKeyID int) context.Context {
	return contexts.WithAPIKey(ctx, &ent.APIKey{ID: apiKeyID})
}

func TestAsyncJob_ResendOnAnotherChannelResumesTheTask(t *testing.T) {
	const secondKey = "sk-second-account"

	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses, f.otherKeys = []string{"RUNNING"}, []string{secondKey} })
	first, second := sharedCacheOutbounds(t, fake.baseURL(), fakeAPIKey, secondKey)
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	cut, cancel := context.WithTimeout(withCaller(t.Context(), 1), 100*time.Millisecond)
	defer cancel()

	_, err := runNonStream(cut, t, first, req)
	respErr := responseError(t, err)
	require.Equal(t, "bailian_task_unfinished", respErr.Detail.Code)
	require.Contains(t, respErr.Detail.Message, "with the same API key within 30 minutes")
	require.NotContains(t, respErr.Detail.Message, "same Bailian channel", "the client cannot pick the channel")

	// The load balancer gives the resend to the other channel.
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })

	resp, err := runNonStream(withCaller(t.Context(), 1), t, second, req)
	require.NoError(t, err)
	require.Contains(t, string(resp.Transcription.Raw), paraformerTxt)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls, "the resend resumes the task the first channel submitted")
	require.Equal(t, "Bearer "+fakeAPIKey, calls.pollAuth[len(calls.pollAuth)-1], "polled with the submitting account")
}

func TestAsyncJob_JobCacheIsPerCallerAndWorkspace(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })
	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	run := func(ctx context.Context, workspace string) {
		t.Helper()

		httpReq := buildProviderRequest(ctx, t, out, transcriptionRequest("paraformer-v2", "", false, audio))
		if workspace != "" {
			httpReq.Headers.Set("X-DashScope-WorkSpace", workspace) // an inbound or channel-override header
		}

		// Per-attempt headers a client sends never make a resend a new job.
		httpReq.Headers.Set("X-Stainless-Retry-Count", time.Now().String())

		resp, err := executor(out).Do(ctx, httpReq)
		require.NoError(t, err)

		_, err = out.TransformResponse(ctx, resp)
		require.NoError(t, err)
	}

	run(withCaller(t.Context(), 1), "")
	run(withCaller(t.Context(), 1), "")
	require.Equal(t, 1, fake.snapshot().submitCalls, "the same caller's resend reuses the job")

	run(withCaller(t.Context(), 2), "")
	require.Equal(t, 2, fake.snapshot().submitCalls, "another caller never receives (or learns about) the first caller's job")

	run(withCaller(t.Context(), 1), "ws-other")
	require.Equal(t, 3, fake.snapshot().submitCalls, "another DashScope workspace is another job")
}

func TestAsyncJob_CachedResultBillsItsUsageOnce(t *testing.T) {
	t.Run("a resend after the response was lost", func(t *testing.T) {
		fake := newFakeDashScope(t)
		out := newTestOutbound(t, fake.baseURL())
		req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

		first, err := runNonStream(t.Context(), t, out, req)
		require.NoError(t, err)
		require.InDelta(t, 8, first.Usage.DurationSeconds, 0.001)

		again, err := runNonStream(t.Context(), t, out, req)
		require.NoError(t, err)
		require.Equal(t, 1, fake.snapshot().submitCalls)
		require.Contains(t, string(again.Transcription.Raw), paraformerTxt, "the client still gets the transcript")
		require.NotNil(t, again.Usage)
		require.Zero(t, *again.Usage, "DashScope billed the task once, so the gateway does too")
	})

	t.Run("concurrent identical requests", func(t *testing.T) {
		fake := newFakeDashScope(t)
		out := newTestOutbound(t, fake.baseURL())
		audio := wavClip(800)

		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			billed float64
		)

		for range 3 {
			wg.Go(func() {
				resp, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
				if err != nil || resp == nil {
					t.Errorf("request failed: %v", err)
					return
				}

				mu.Lock()
				billed += resp.Usage.DurationSeconds
				mu.Unlock()
			})
		}

		wg.Wait()
		require.Equal(t, 1, fake.snapshot().submitCalls)
		require.InDelta(t, 8, billed, 0.001, "three requests share one task and one bill")
	})

	t.Run("streams", func(t *testing.T) {
		fake := newFakeDashScope(t)
		out := newTestOutbound(t, fake.baseURL())
		req := transcriptionRequest("paraformer-v2", "", true, wavClip(800))

		_, raw, httpReq := runStream(t.Context(), t, out, req)
		_, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, raw)
		require.NoError(t, err)
		require.InDelta(t, 8, meta.Usage.DurationSeconds, 0.001)

		responses, raw, httpReq := runStream(t.Context(), t, out, req)
		done := responses[len(responses)-2]
		require.Equal(t, "transcript.text.done", done.TranscriptionStreamEvent.Type)
		require.InDelta(t, 8, done.TranscriptionStreamEvent.Usage.DurationSeconds, 0.001, "the client event keeps the transcript's usage")
		require.Zero(t, *done.Usage)

		_, meta, err = out.AggregateStreamChunks(t.Context(), httpReq, raw)
		require.NoError(t, err)
		require.True(t, meta.Completed)
		require.Zero(t, *meta.Usage, "the cached result is not billed again")
		require.Equal(t, 1, fake.snapshot().submitCalls)
	})

	t.Run("audio given by URL is its own task every time", func(t *testing.T) {
		fake := newFakeDashScope(t)
		out := newTestOutbound(t, fake.baseURL())
		ctx := WithAudioSource(t.Context(), NewURLSource("https://media.example.com/a.wav"))

		for range 2 {
			resp, err := runNonStream(ctx, t, out, transcriptionRequest("paraformer-v2", "", false, nil))
			require.NoError(t, err)
			require.InDelta(t, 8, resp.Usage.DurationSeconds, 0.001)
		}

		require.Equal(t, 2, fake.snapshot().submitCalls)
	})
}

func TestAsyncJob_UnknownStatusOfAFreshTaskIsBilled(t *testing.T) {
	previous := unknownStatusGrace
	unknownStatusGrace = 20 * time.Millisecond

	t.Cleanup(func() { unknownStatusGrace = previous })

	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"UNKNOWN"} })

	// A relay that queries with another account sees the task this request just submitted as
	// UNKNOWN: it runs and is billed, so no other channel may submit it again.
	p, switcher := twoChannelPipeline(newScopedOutbound(t, fake.baseURL(), "", nil), newScopedOutbound(t, fake.baseURL(), "", nil))

	_, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, http.StatusFailedDependency, responseError(t, err).StatusCode)
	require.Equal(t, 1, fake.snapshot().submitCalls)
	require.Zero(t, switcher.switches+switcher.sameChannelRetries)

	// The task stays in the job cache, and a resend that still finds it UNKNOWN keeps it as billed
	// too (round 4: it was accepted less than DashScope's 24 h retention ago, see
	// TestAsyncJob_ResumedTaskThatReadsUnknownIsStillBilled).
	out := newTestOutbound(t, fake.baseURL())
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	_, err = runNonStream(t.Context(), t, out, req)
	require.ErrorIs(t, err, pipeline.ErrNoRetry)

	_, err = runNonStream(t.Context(), t, out, req)
	require.False(t, isJobFailure(err), "a cached task that reads UNKNOWN within the retention is not gone: %v", err)
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, 2, fake.snapshot().submitCalls, "the resend resumed the task instead of submitting")
}

func TestAsyncJob_SubmitOutlivesTheClientThatSentIt(t *testing.T) {
	const submitPath = "/api/v1/services/audio/asr/transcription"

	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) {
		f.statuses = []string{"SUCCEEDED"}
		f.hold, f.holdStarted = map[string]chan struct{}{submitPath: gate}, started
	})

	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	first, leave := context.WithCancel(t.Context())
	defer leave()

	var (
		wg                  sync.WaitGroup
		firstErr, secondErr error
	)

	wg.Go(func() {
		_, firstErr = runNonStream(first, t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	<-started // the first request's submit reached DashScope

	wg.Go(func() {
		_, secondErr = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	time.Sleep(200 * time.Millisecond) // the second request joins the submit

	leave() // the first client goes away while the submit's answer is on its way
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	respErr := responseError(t, firstErr)
	require.Equal(t, "bailian_task_unfinished", respErr.Detail.Code, "the task ID was read, so the task is resumable")
	require.ErrorIs(t, firstErr, pipeline.ErrNoRetry)
	require.NoError(t, secondErr, "the joined request waits for the task the first one submitted")

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	require.NoError(t, err)
	require.Equal(t, 1, fake.snapshot().submitCalls, "neither the joined request nor a resend submits again")

	// A request whose client is already gone does not submit at all.
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(900)))
	_, err = executor(out).(*jobExecutor).submit(gone, httpReq, "oss://x")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, fake.snapshot().submitCalls)
}

func TestAsyncJob_RejectedQueryWithAnotherRequestsKeyIsChannelNeutral(t *testing.T) {
	const submitPath = "/api/v1/services/audio/asr/transcription"

	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) {
		f.otherKeys = []string{"sk-second-key"}
		f.hold, f.holdStarted = map[string]chan struct{}{submitPath: gate}, started
	})

	out := newScopedOutbound(t, fake.baseURL(), "", &rotatingKeys{keys: []string{fakeAPIKey, "sk-second-key"}})
	audio := wavClip(800)

	var (
		wg                  sync.WaitGroup
		firstErr, secondErr error
	)

	wg.Go(func() {
		_, firstErr = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	<-started

	wg.Go(func() {
		_, secondErr = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	time.Sleep(200 * time.Millisecond) // the second request (the channel's second key) joins the submit

	// The submitting key is revoked while the task runs.
	fake.set(func(f *fakeDashScope) { f.pollReject = http.StatusUnauthorized })
	close(gate)
	wg.Wait()

	for _, err := range []error{firstErr, secondErr} {
		require.ErrorIs(t, err, pipeline.ErrNoRetry)
		require.Equal(t, http.StatusFailedDependency, responseError(t, err).StatusCode)
	}

	require.False(t, IsChannelNeutralFailure(firstErr), "the submitting request's own key failed: that counts")
	require.True(t, IsChannelNeutralFailure(secondErr), "the joined request's key was never used for the rejected query")
	require.Equal(t, 1, fake.snapshot().submitCalls)
}

func TestJobCache_TouchLeavesAnotherTasksEntryAlone(t *testing.T) {
	cache := newJobCache(time.Minute, 16)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	result := &transcript{ID: "T2", Text: "好。"}
	cache.putResult("k", "T2", result)

	// The request that waited for the replaced task T1 ends.
	cache.touch("k", taskRef{id: "T1", headers: http.Header{"Authorization": []string{"Bearer sk-revoked"}}})

	entry, ok := cache.lookup("k")
	require.True(t, ok)
	require.Equal(t, "T2", entry.taskID)
	require.Same(t, result, entry.result, "the finished task's result survives")

	// Once the other entry expired, the running task is stored again.
	now = now.Add(2 * time.Minute)
	cache.touch("k", taskRef{id: "T1"})

	entry, ok = cache.lookup("k")
	require.True(t, ok)
	require.Equal(t, "T1", entry.taskID)

	// Requests that waited for the same task share one usage claim.
	claim := cache.putResult("shared", "T3", &transcript{ID: "T3"})
	require.Same(t, claim, cache.putResult("shared", "T3", &transcript{ID: "T3"}))
	require.NotSame(t, claim, cache.putResult("shared", "T4", &transcript{ID: "T4"}))
}

func TestRedactURL_FileNamesAreNotKept(t *testing.T) {
	for raw, want := range map[string]string{
		"https://utfs.io/f/K3yS3cr3tCapabilityToken":                   "https://utfs.io/<redacted>",
		"https://dl.example.com/files/audio.mp3;jsessionid=S3SS10N":    "https://dl.example.com/<redacted>.mp3",
		"https://blob.example.com/Xk2sd9F0aQ-random-name.webm":         "https://blob.example.com/<redacted>.webm",
		"https://blob.example.com/name.K3yS3cr3tWithADot":              "https://blob.example.com/<redacted>",
		"https://media.example.com/":                                   "https://media.example.com",
		"https://media.example.com/a%20b.ogg?sig=1":                    "https://media.example.com/<redacted>.ogg?<redacted>",
		"https://api.telegram.org/file/bot123:AAHsecret/voice/f_7.oga": "https://api.telegram.org/<redacted>.oga",
	} {
		require.Equal(t, want, RedactURL(raw), raw)
	}

	require.Equal(t, "audio.m4a", NewURLSource("https://media.example.com/room-3/latest.m4a").Name())
	require.Empty(t, NewURLSource("https://utfs.io/f/K3yS3cr3tCapabilityToken").Name())
	require.Equal(t, "audio.mp3", NewURLSource("https://dl.example.com/a.mp3;jsessionid=S3SS10N").Name())

	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	ctx := WithAudioSource(t.Context(), NewURLSource("https://utfs.io/f/K3yS3cr3tCapabilityToken.wav"))

	httpReq := buildProviderRequest(ctx, t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, nil))
	require.NotContains(t, string(httpReq.JSONBody), "K3yS3cr3t", "the logged flash body keeps no capability file name")
	require.Contains(t, string(httpReq.Body), "K3yS3cr3tCapabilityToken.wav", "DashScope still gets the real URL")
}
