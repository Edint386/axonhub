package bailianasr

// Regression tests for the fourth 2026-11-05 review round of the HTTP file transcription: a
// resumed task that reads UNKNOWN within DashScope's retention stays billed and forgetting only
// drops its own task (B1), the flash call is a long-running job and a cut after its audio was
// sent is never sent again (B3), a request that joined another request's submit retries a
// non-billed failure with its own credentials and a reused job's success is not the channel's
// (B4), and a lost submit answer blocks an identical resend for a while (B5).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// runJob is runNonStream that also returns the provider request; it does not fail the test
// itself, so goroutines may call it.
func runJob(ctx context.Context, out *Outbound, req *llm.Request) (*httpclient.Request, *llm.Response, error) {
	httpReq, err := out.TransformRequest(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	httpReq, err = httpclient.FinalizeAuthHeaders(httpReq)
	if err != nil {
		return nil, nil, err
	}

	resp, err := executor(out).Do(ctx, httpReq)
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return httpReq, nil, out.TransformError(ctx, httpErr)
	}

	if err != nil {
		return httpReq, nil, err
	}

	llmResp, err := out.TransformResponse(ctx, resp)

	return httpReq, llmResp, err
}

// B1: a task the job cache hands to a resend was accepted with a task_id; within DashScope's 24 h
// retention a status that does not find it never makes it "gone", so the resend never submits
// (and pays for) the audio again. Only beyond the retention may it be submitted again.
func TestAsyncJob_ResumedTaskThatReadsUnknownIsStillBilled(t *testing.T) {
	previous := unknownStatusGrace
	unknownStatusGrace = 20 * time.Millisecond

	t.Cleanup(func() { unknownStatusGrace = previous })

	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"UNKNOWN"} })
	first, second := sharedCacheOutbounds(t, fake.baseURL(), fakeAPIKey, fakeAPIKey)

	// A relay that queries with another account: the fresh task reads UNKNOWN and is billed.
	p, _ := twoChannelPipeline(first, second)
	_, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, 1, fake.snapshot().submitCalls)

	// The client sends the same request again: it resumes the task, which still reads UNKNOWN.
	p, switcher := twoChannelPipeline(first, second)
	_, err = p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry, "the resumed task is billed, not gone")
	require.Equal(t, http.StatusFailedDependency, responseError(t, err).StatusCode)
	require.Contains(t, responseError(t, err).Detail.Message, "polls this task")
	require.Equal(t, 1, fake.snapshot().submitCalls, "no second paid task")
	require.Zero(t, switcher.switches+switcher.sameChannelRetries)

	// Past DashScope's retention the task may really be gone: then it is submitted again.
	first.cache.mu.Lock()
	for _, entry := range first.cache.entries {
		entry.submitted = entry.submitted.Add(-dashScopeTaskRetention - time.Hour)
	}
	first.cache.mu.Unlock()

	p, switcher = twoChannelPipeline(first, second)
	_, err = p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry, "the new task reads UNKNOWN as well and is billed")
	require.Equal(t, 2, fake.snapshot().submitCalls)
	require.Equal(t, 1, switcher.sameChannelRetries)

	require.True(t, withinRetention(time.Time{}, time.Now()), "an unknown submit time is never taken as gone")
}

func TestJobCache_ForgetTaskKeepsAnotherTasksEntry(t *testing.T) {
	cache := newJobCache(time.Minute, 16)

	cache.putTask("k", "T2", nil)
	cache.forgetTask("k", "T1") // a request that waited for the replaced task T1 ends

	entry, ok := cache.lookup("k")
	require.True(t, ok, "the newer task survives")
	require.Equal(t, "T2", entry.taskID)
	require.False(t, entry.submitted.IsZero(), "the submit time is kept for the retention check")

	cache.putResult("k", "T2", &transcript{ID: "T2"})
	entry, _ = cache.lookup("k")
	require.False(t, entry.submitted.IsZero(), "a stored result keeps the submit time")

	cache.forgetTask("k", "T2")
	_, ok = cache.lookup("k")
	require.False(t, ok)
}

// B3: a flash call is cut once its audio was sent (the connection broke, or the client left):
// Bailian may have run and billed it, so neither the same channel nor another one sends it again.
// A failure before anything was sent is still retried.
func TestFlash_CallCutAfterItsAudioWasSentIsNotSentAgain(t *testing.T) {
	var calls atomic.Int32

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)

		panic(http.ErrAbortHandler) // the connection breaks after the request arrived
	}))
	t.Cleanup(cut.Close)

	p, switcher := twoChannelPipeline(newScopedOutbound(t, cut.URL+"/compatible-mode/v1", "", nil),
		newScopedOutbound(t, cut.URL+"/compatible-mode/v1", "", nil))

	_, err := p.Process(t.Context(), multipartTranscription(t, "qwen-audio-3.1-asr-flash", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, "bailian_flash_answer_lost", responseError(t, err).Detail.Code)
	require.Equal(t, http.StatusBadGateway, responseError(t, err).StatusCode)
	require.False(t, IsChannelNeutralFailure(err), "a broken connection counts against the channel")
	require.EqualValues(t, 1, calls.Load(), "the audio was sent once")
	require.Zero(t, switcher.switches+switcher.sameChannelRetries)

	t.Run("nothing sent yet", func(t *testing.T) {
		refused := httptest.NewServer(http.NotFoundHandler())
		refusedURL := refused.URL
		refused.Close()

		healthy := newFakeDashScope(t)
		p, _ := twoChannelPipeline(newScopedOutbound(t, refusedURL+"/compatible-mode/v1", "", nil),
			newScopedOutbound(t, healthy.baseURL(), "", nil))

		result, err := p.Process(t.Context(), multipartTranscription(t, "qwen-audio-3.1-asr-flash", wavClip(800)))
		require.NoError(t, err, "a call that never reached Bailian moves on")
		require.Contains(t, string(result.Response.Body), clipText)
		require.Equal(t, 1, healthy.snapshot().flashCalls)
	})

	t.Run("stream broken before its events", func(t *testing.T) {
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()

			panic(http.ErrAbortHandler)
		}))
		t.Cleanup(broken.Close)

		out := newTestOutbound(t, broken.URL+"/compatible-mode/v1")
		httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", true, wavClip(800)))

		stream, err := executor(out).DoStream(t.Context(), httpReq)
		require.NoError(t, err)

		for stream.Next() {
		}

		require.ErrorIs(t, stream.Err(), pipeline.ErrNoRetry)
		require.Equal(t, "bailian_flash_answer_lost", responseError(t, stream.Err()).Detail.Code)
		require.NoError(t, stream.Close())
	})

	t.Run("the request ends while Bailian transcribes", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			<-r.Context().Done()
		}))
		t.Cleanup(slow.Close)

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		out := newTestOutbound(t, slow.URL+"/compatible-mode/v1")
		_, err := runNonStream(ctx, t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, wavClip(800)))
		require.ErrorIs(t, err, pipeline.ErrNoRetry)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, "bailian_task_unfinished", responseError(t, err).Detail.Code)
		require.True(t, IsChannelNeutralFailure(err), "the deadline says nothing about the channel")
	})
}

// B4: a request that joined another request's upload and submit gets back that request's
// failure (here its revoked key): nothing was billed, so it runs the upload again with its own
// credentials instead of taking the other key's 401 as its own.
func TestAsyncJob_JoinedRequestRetriesTheLeadersFailureWithItsOwnKey(t *testing.T) {
	const revoked = "sk-revoked"

	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) {
		f.revokedKeys = []string{revoked}
		f.hold, f.holdStarted = map[string]chan struct{}{"/api/v1/uploads": gate}, started
	})

	leader, joiner := sharedCacheOutbounds(t, fake.baseURL(), revoked, fakeAPIKey)
	audio := wavClip(800)

	var (
		wg                   sync.WaitGroup
		leaderErr, joinerErr error
		joinerReq            *httpclient.Request
		joinerResp           *llm.Response
	)

	wg.Go(func() {
		_, _, leaderErr = runJob(t.Context(), leader, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	<-started // the leader's getPolicy (with the revoked key) is in flight

	wg.Go(func() {
		joinerReq, joinerResp, joinerErr = runJob(t.Context(), joiner, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	time.Sleep(200 * time.Millisecond) // the second request joins the shared upload
	close(gate)
	wg.Wait()

	require.Equal(t, http.StatusUnauthorized, responseError(t, leaderErr).StatusCode, "the leader's own key failed")
	require.NoError(t, joinerErr, "the joined request uploads again with its own key")
	require.Contains(t, string(joinerResp.Transcription.Raw), paraformerTxt)
	require.False(t, ReusedJob(joinerReq), "it submitted the audio itself")

	calls := fake.snapshot()
	require.Equal(t, 2, calls.policyCalls)
	require.Equal(t, 1, calls.submitCalls)

	// A failure that never became the joined request's own reads as the other request's failure
	// and is neutral for the channel.
	shared := &joinedSubmitError{err: &httpclient.Error{StatusCode: http.StatusUnauthorized}}
	require.True(t, IsChannelNeutralFailure(shared))

	httpErr, ok := errors.AsType[*httpclient.Error](shared)
	require.True(t, ok, "the pipeline still finds the HTTP error to transform")
	require.Equal(t, http.StatusUnauthorized, httpErr.StatusCode)
}

// B4: a job served from the cache, from a task the cache held, or from another request's submit
// is reused: its success says nothing about the channel or key of the attempt (see ReusedJob and
// the orchestrator's markTranscriptionSuccess).
func TestAsyncJob_ReusedJobsAreMarked(t *testing.T) {
	const submitPath = "/api/v1/services/audio/asr/transcription"

	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) {
		f.statuses = []string{"SUCCEEDED"}
		f.hold, f.holdStarted = map[string]chan struct{}{submitPath: gate}, started
	})

	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	var (
		wg                   sync.WaitGroup
		leaderReq, joinerReq *httpclient.Request
		leaderErr, joinerErr error
	)

	wg.Go(func() {
		leaderReq, _, leaderErr = runJob(t.Context(), out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	<-started

	wg.Go(func() {
		joinerReq, _, joinerErr = runJob(t.Context(), out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()

	require.NoError(t, leaderErr)
	require.NoError(t, joinerErr)
	require.False(t, ReusedJob(leaderReq), "the request that submitted the audio")
	require.True(t, ReusedJob(joinerReq), "the request that joined its submit")

	cachedReq, _, err := runJob(t.Context(), out, transcriptionRequest("paraformer-v2", "", false, audio))
	require.NoError(t, err)
	require.True(t, ReusedJob(cachedReq), "a cached result")
	require.Equal(t, 1, fake.snapshot().submitCalls)

	flashReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", false, audio))
	require.False(t, ReusedJob(flashReq))
	require.False(t, ReusedJob(nil))
}

// B5: a submit whose answer was lost leaves a pending entry: an identical resend within 10
// minutes gets a 409 (nothing uploaded or submitted, nothing retried, not the channel's fault)
// instead of paying for a second task while the first may still run.
func TestAsyncJob_LostSubmitAnswerBlocksAnIdenticalResend(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) {
		f.submitFailure.status, f.submitFailure.body = http.StatusGatewayTimeout, `{"code":"RequestTimeOut","message":"timeout"}`
	})

	first, second := sharedCacheOutbounds(t, fake.baseURL(), fakeAPIKey, fakeAPIKey)
	now := time.Now()
	first.cache.now = func() time.Time { return now }

	p, _ := twoChannelPipeline(first, second)
	_, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, "bailian_submit_unconfirmed", responseError(t, err).Detail.Code)
	require.Contains(t, responseError(t, err).Detail.Message, "refused (409)")
	require.NotContains(t, responseError(t, err).Detail.Message, "does not send it again", "the old text promised too much")

	p, switcher := twoChannelPipeline(first, second)
	_, err = p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
	require.Equal(t, http.StatusConflict, responseError(t, err).StatusCode)
	require.Equal(t, "bailian_submit_pending", responseError(t, err).Detail.Code)
	require.Contains(t, responseError(t, err).Detail.Message, "may still be running")
	require.True(t, IsChannelNeutralFailure(err))
	require.Zero(t, switcher.switches+switcher.sameChannelRetries)

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls, "no second paid task")
	require.Len(t, calls.uploads, 1, "not even uploaded again")

	// After the pending window the resend starts (and pays for) a new task.
	now = now.Add(pendingSubmitTTL + time.Minute)
	fake.set(func(f *fakeDashScope) { f.submitFailure.status = 0 })

	p, _ = twoChannelPipeline(first, second)
	result, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
	require.NoError(t, err)
	require.Contains(t, string(result.Response.Body), paraformerTxt)
	require.Equal(t, 2, fake.snapshot().submitCalls)
}
