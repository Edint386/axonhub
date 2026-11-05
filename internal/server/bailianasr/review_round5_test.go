package bailianasr

// Regression tests for the round-4 regression check of the HTTP file transcription: a flash call
// answered by a relay's 504 (or a 5xx that is not DashScope's) after its audio was sent is never
// sent again; a request that joined another request's submit restarts only after a failure that
// may be the leader's own (its credentials, or its context), and every shared failure, billed ones
// included, is reported as not its own.

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

// A 504, or a 5xx whose body is not DashScope's own error, answered after the flash audio was
// sent does not tell whether Bailian transcribed (and billed) it: no attempt sends it again. An
// answer DashScope gave itself keeps its usual handling.
func TestFlash_RelayErrorAfterTheAudioWasSentIsNotSentAgain(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		lost   bool
	}{
		{"relay timeout", http.StatusGatewayTimeout, "upstream request timeout", true},
		{"DashScope-shaped timeout", http.StatusGatewayTimeout, `{"code":"RequestTimeOut","message":"timeout"}`, true},
		{"relay gateway error", http.StatusBadGateway, "<html>502 Bad Gateway</html>", true},
		{"relay unavailable", http.StatusServiceUnavailable, "no healthy upstream", true},
		{"DashScope's own server error", http.StatusInternalServerError, `{"code":"InternalError","message":"retry"}`, false},
		{"DashScope rejected the audio", http.StatusBadRequest, `{"code":"InvalidParameter","message":"bad"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				calls.Add(1)

				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			p, switcher := twoChannelPipeline(newScopedOutbound(t, server.URL+"/compatible-mode/v1", "", nil),
				newScopedOutbound(t, server.URL+"/compatible-mode/v1", "", nil))

			_, err := p.Process(t.Context(), multipartTranscription(t, "qwen-audio-3.1-asr-flash", wavClip(800)))
			require.Error(t, err)

			if !tc.lost {
				require.NotErrorIs(t, err, pipeline.ErrNoRetry, "Bailian answered, so its usual handling applies")
				require.Equal(t, tc.status, responseError(t, err).StatusCode)

				return
			}

			require.ErrorIs(t, err, pipeline.ErrNoRetry)
			require.Equal(t, "bailian_flash_answer_lost", responseError(t, err).Detail.Code)
			require.Equal(t, http.StatusBadGateway, responseError(t, err).StatusCode)
			require.Contains(t, responseError(t, err).Detail.Message, "HTTP")
			require.False(t, IsChannelNeutralFailure(err), "a lost answer counts against the channel")

			_, holdsHTTPError := errors.AsType[*httpclient.Error](err)
			require.False(t, holdsHTTPError, "the pipeline would replace an HTTP error and lose the marker")
			require.EqualValues(t, 1, calls.Load(), "the audio was sent once")
			require.Zero(t, switcher.switches+switcher.sameChannelRetries)
		})
	}

	t.Run("stream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			http.Error(w, "upstream request timeout", http.StatusGatewayTimeout)
		}))
		t.Cleanup(server.Close)

		out := newTestOutbound(t, server.URL+"/compatible-mode/v1")
		httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("qwen-audio-3.1-asr-flash", "", true, wavClip(800)))

		_, err := executor(out).DoStream(t.Context(), httpReq)
		require.ErrorIs(t, err, pipeline.ErrNoRetry)
		require.Equal(t, "bailian_flash_answer_lost", responseError(t, err).Detail.Code)
	})

	t.Run("an answer before the request was sent is kept", func(t *testing.T) {
		answered := &httpclient.Error{StatusCode: http.StatusGatewayTimeout, Body: []byte("upstream request timeout")}
		require.Same(t, answered, flashCallError(t.Context(), answered, false))
	})
}

// joinedRun is one request that may join another request's shared upload and submit; it keeps the
// executor's own error, which the orchestrator's channel metrics read.
type joinedRun struct {
	httpReq *httpclient.Request
	err     error
	elapsed time.Duration
}

func newJoinedRun(t *testing.T, out *Outbound, audio []byte) *joinedRun {
	t.Helper()

	return &joinedRun{httpReq: buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))}
}

func (r *joinedRun) run(ctx context.Context, out *Outbound) {
	started := time.Now()
	_, r.err = executor(out).Do(ctx, r.httpReq)
	r.elapsed = time.Since(started)
}

// A failure that would fail the same way for every request (the endpoint stalled, the audio was
// rejected) is shared with the requests that joined the submit: they neither run it again nor
// wait for the same stalled endpoint once more, and the failure is not theirs.
func TestAsyncJob_JoinedRequestsShareFailuresThatAreNotTheLeaders(t *testing.T) {
	const submitPath = "/api/v1/services/audio/asr/transcription"

	t.Run("start timeout", func(t *testing.T) {
		fake := newFakeDashScope(t)
		started := make(chan string, 16)
		fake.set(func(f *fakeDashScope) {
			f.hold, f.holdStarted = map[string]chan struct{}{"/api/v1/uploads": make(chan struct{})}, started
		})

		out := newTestOutbound(t, fake.baseURL())
		out.cfg.StartTimeout = 400 * time.Millisecond
		audio := wavClip(800)

		leader := newJoinedRun(t, out, audio)
		joiners := []*joinedRun{newJoinedRun(t, out, audio), newJoinedRun(t, out, audio), newJoinedRun(t, out, audio)}

		var wg sync.WaitGroup

		wg.Go(func() { leader.run(t.Context(), out) })

		<-started // the leader's getPolicy is held

		for _, joiner := range joiners {
			wg.Go(func() { joiner.run(t.Context(), out) })
		}

		wg.Wait()

		require.Equal(t, "bailian_start_timeout", responseError(t, leader.err).Detail.Code)
		require.False(t, IsChannelNeutralFailure(leader.err), "the stall is the leader's channel's")

		for _, joiner := range joiners {
			require.Equal(t, "bailian_start_timeout", responseError(t, joiner.err).Detail.Code)
			require.ErrorIs(t, joiner.err, pipeline.ErrNonStreamResponseTimeout, "the pipeline still moves to the next channel")
			require.NotErrorIs(t, joiner.err, pipeline.ErrNoRetry)
			require.True(t, IsChannelNeutralFailure(joiner.err), "the joined request's key was never used")
			require.Less(t, joiner.elapsed, 2*out.cfg.StartTimeout, "no second wait for the same stalled endpoint")
		}

		require.Empty(t, started, "only the leader asked for an upload policy")
	})

	t.Run("audio rejected", func(t *testing.T) {
		fake := newFakeDashScope(t)
		gate, started := make(chan struct{}), make(chan string, 16)
		fake.set(func(f *fakeDashScope) {
			f.submitFailure.status, f.submitFailure.body = http.StatusBadRequest, `{"code":"InvalidFile.DecodeFailed","message":"bad audio"}`
			f.hold, f.holdStarted = map[string]chan struct{}{submitPath: gate}, started
		})

		out := newTestOutbound(t, fake.baseURL())
		audio := wavClip(800)
		leader, joiner := newJoinedRun(t, out, audio), newJoinedRun(t, out, audio)

		var wg sync.WaitGroup

		wg.Go(func() { leader.run(t.Context(), out) })

		<-started // the leader's submit is held

		wg.Go(func() { joiner.run(t.Context(), out) })

		time.Sleep(200 * time.Millisecond) // the second request joins the shared submit
		close(gate)
		wg.Wait()

		httpErr, ok := errors.AsType[*httpclient.Error](joiner.err)
		require.True(t, ok, "the pipeline still transforms DashScope's answer")
		require.Equal(t, http.StatusBadRequest, httpErr.StatusCode)
		require.ErrorIs(t, joiner.err, errJoinedSubmit)

		calls := fake.snapshot()
		require.Equal(t, 1, calls.submitCalls, "the rejected audio is not submitted again")
		require.Len(t, calls.uploads, 1, "nor uploaded again")
		require.Equal(t, 1, calls.policyCalls)
	})
}

// A billed failure, or a submit whose answer was lost, is shared with the requests that joined the
// submit (another submit would pay again), and for them it is another request's failure: it says
// nothing about their channel or key, so the channel metrics treat it as neutral.
func TestAsyncJob_JoinedRequestReportsASharedBilledFailureAsNotItsOwn(t *testing.T) {
	const submitPath = "/api/v1/services/audio/asr/transcription"

	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) {
		f.submitFailure.status, f.submitFailure.body = http.StatusGatewayTimeout, "upstream request timeout"
		f.hold, f.holdStarted = map[string]chan struct{}{submitPath: gate}, started
	})

	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)
	leader, joiner := newJoinedRun(t, out, audio), newJoinedRun(t, out, audio)

	var wg sync.WaitGroup

	wg.Go(func() { leader.run(t.Context(), out) })

	<-started // the leader's submit is held

	wg.Go(func() { joiner.run(t.Context(), out) })

	time.Sleep(200 * time.Millisecond) // the second request joins the shared submit
	close(gate)
	wg.Wait()

	require.Equal(t, codeSubmitUnconfirmed, responseError(t, leader.err).Detail.Code)
	require.False(t, IsChannelNeutralFailure(leader.err), "the leader's own channel lost the answer")

	require.ErrorIs(t, joiner.err, pipeline.ErrNoRetry, "the pipeline still stops")
	require.Equal(t, codeSubmitUnconfirmed, responseError(t, joiner.err).Detail.Code)
	require.Equal(t, http.StatusBadGateway, responseError(t, joiner.err).StatusCode)
	require.True(t, IsChannelNeutralFailure(joiner.err), "the joined request's key was never used")

	_, holdsHTTPError := errors.AsType[*httpclient.Error](joiner.err)
	require.False(t, holdsHTTPError)
	require.Equal(t, 1, fake.snapshot().submitCalls, "only one submit was sent")
}

func TestFailedForTheLeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		leaderGone bool
		restart    bool
	}{
		{"revoked key", &httpclient.Error{StatusCode: http.StatusUnauthorized}, false, true},
		{"key without access", &httpclient.Error{StatusCode: http.StatusForbidden}, false, true},
		{"throttled key", &httpclient.Error{StatusCode: http.StatusTooManyRequests}, false, true},
		{"quota of the key", &llm.ResponseError{StatusCode: http.StatusForbidden}, false, true},
		// DashScope answers these credential failures with HTTP 400; the code decides.
		{"account in arrears", &httpclient.Error{StatusCode: http.StatusBadRequest, Body: []byte(`{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing."}`)}, false, true},
		{"access denied", &httpclient.Error{StatusCode: http.StatusBadRequest, Body: []byte(`{"code":"AccessDenied.Unpurchased","message":"denied"}`)}, false, true},
		{"arrears as a response error", newResponseError(http.StatusBadRequest, "Arrearage", "denied", "", nil), false, true},
		{"audio rejected with a DashScope code", &httpclient.Error{StatusCode: http.StatusBadRequest, Body: []byte(`{"code":"InvalidParameter","message":"bad audio"}`)}, false, false},
		{"the leader's client left", context.Canceled, true, true},
		{"the leader's client left mid-upload", errors.New("bailian temporary storage upload failed: EOF"), true, true},
		{"start timeout", startTimeoutError("upload policy request", time.Second), false, false},
		{"audio rejected", &httpclient.Error{StatusCode: http.StatusBadRequest}, false, false},
		{"audio too large", newResponseError(http.StatusRequestEntityTooLarge, "file_too_large", "", "", nil), false, false},
		{"upload failed", newResponseError(http.StatusBadGateway, "bailian_upload_failed", "", "", nil), false, false},
		{"transport failure", errors.New("connection refused"), false, false},
		{"lost submit answer", unconfirmedSubmitError("HTTP 504", nil), false, false},
		{"lost submit answer after the leader left", unconfirmedSubmitError("HTTP 504", nil), true, false},
		{"pending submit", pendingSubmitError(), false, false},
	} {
		require.Equal(t, tc.restart, failedForTheLeaderOnly(tc.err, tc.leaderGone), tc.name)
	}
}
