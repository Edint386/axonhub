package bailianasr

// Regression tests for the second 2026-11-05 review round of the HTTP file transcription: a
// billed task whose result could not be read is polled again by a resend, a cached task that
// can no longer be queried is submitted again, the steps before a task exists are bounded, a
// submit whose answer was lost is never sent twice, a joined request outlives the request whose
// upload it shared, and failures that say nothing about the channel are recognised.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestAsyncJob_ResendAfterAnUnreadableResultPollsTheSameTask(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses, f.resultErrors = []string{"SUCCEEDED"}, maxResultFetchFailures })
	out := newTestOutbound(t, fake.baseURL())
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	_, err := runNonStream(t.Context(), t, out, req)
	require.Equal(t, "bailian_result_unavailable", responseError(t, err).Detail.Code)

	// The OSS outage is over: the client sends the same request again.
	resp, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err)
	require.Contains(t, string(resp.Transcription.Raw), paraformerTxt)
	require.Equal(t, 1, fake.snapshot().submitCalls, "the billed task is read again, not paid for again")
	require.Len(t, fake.snapshot().uploads, 1)

	// A result that cannot be parsed fails again on a resend, still without a new task.
	invalid := newFakeDashScope(t)
	invalid.set(func(f *fakeDashScope) { f.statuses, f.result = []string{"SUCCEEDED"}, "not a transcript" })
	out = newTestOutbound(t, invalid.baseURL())

	for range 2 {
		_, err = runNonStream(t.Context(), t, out, req)
		require.Equal(t, "bailian_result_invalid", responseError(t, err).Detail.Code)
	}

	require.Equal(t, 1, invalid.snapshot().submitCalls)
}

func TestAsyncJob_ResumedTaskWithARevokedKeyIsSubmittedAgain(t *testing.T) {
	const secondKey = "sk-second-key"

	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses, f.otherKeys = []string{"RUNNING"}, []string{secondKey} })
	out := newScopedOutbound(t, fake.baseURL(), "channel:7", &rotatingKeys{keys: []string{fakeAPIKey, secondKey}})
	req := transcriptionRequest("paraformer-v2", "", false, wavClip(800))

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := runNonStream(ctx, t, out, req)
	require.Equal(t, http.StatusGatewayTimeout, responseError(t, err).StatusCode)

	// The admin revokes the first key; the resend arrives with the second one.
	fake.set(func(f *fakeDashScope) { f.statuses, f.revokedKeys = []string{"SUCCEEDED"}, []string{fakeAPIKey} })

	resp, err := runNonStream(t.Context(), t, out, req)
	require.NoError(t, err, "a task its key can no longer query is submitted again with the current key")
	require.Contains(t, string(resp.Transcription.Raw), paraformerTxt)

	calls := fake.snapshot()
	require.Equal(t, 2, calls.submitCalls)
	require.Equal(t, "Bearer "+secondKey, calls.submitHeaders[1].Get("Authorization"))
	require.Equal(t, "Bearer "+secondKey, calls.pollAuth[len(calls.pollAuth)-1])

	// A task this request submitted itself whose query is rejected is billed: no retry, and the
	// status is not the caller's own 401.
	rejected := newFakeDashScope(t)
	rejected.set(func(f *fakeDashScope) { f.pollReject = http.StatusUnauthorized })

	_, err = runNonStream(t.Context(), t, newTestOutbound(t, rejected.baseURL()), req)
	respErr := responseError(t, err)
	require.Equal(t, http.StatusFailedDependency, respErr.StatusCode)
	require.Equal(t, "upstream_error", respErr.Detail.Type)
	require.ErrorIs(t, err, pipeline.ErrNoRetry)
}

type requestProcessor interface {
	Process(ctx context.Context, request *httpclient.Request) (*pipeline.Result, error)
}

// twoChannelPipeline runs the real pipeline over two Bailian outbounds with every retry kind on.
func twoChannelPipeline(first, second *Outbound) (requestProcessor, *channelSwitcher) {
	switcher := &channelSwitcher{channels: []*Outbound{first, second}}

	return pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(openai.NewTranscriptionInboundTransformer(), switcher,
		pipeline.WithRetry(3, 3, 0)), switcher
}

func TestAsyncJob_StalledStepsBeforeTheTaskMoveToTheNextChannel(t *testing.T) {
	for _, path := range []string{"/api/v1/uploads", "/oss"} {
		t.Run(path, func(t *testing.T) {
			stalled := newFakeDashScope(t)
			stalled.set(func(f *fakeDashScope) { f.hold = map[string]chan struct{}{path: make(chan struct{})} })
			healthy := newFakeDashScope(t)

			first := newScopedOutbound(t, stalled.baseURL(), "channel:1", nil)
			first.cfg.StartTimeout = 100 * time.Millisecond

			p, switcher := twoChannelPipeline(first, newScopedOutbound(t, healthy.baseURL(), "channel:2", nil))

			// Without the bound the stalled step waits for the request context: fail instead.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			started := time.Now()
			result, err := p.Process(ctx, multipartTranscription(t, "paraformer-v2", wavClip(800)))
			require.NoError(t, err)
			require.Contains(t, string(result.Response.Body), paraformerTxt)
			require.Less(t, time.Since(started), 5*time.Second)
			require.Equal(t, 1, switcher.switches, "the stalled channel is left for the next one")
			require.Zero(t, stalled.snapshot().submitCalls)
			require.Equal(t, 1, healthy.snapshot().submitCalls)
		})
	}

	// The bound is per step: an upload that keeps making progress is not cut off.
	slow := newFakeDashScope(t)
	out := newTestOutbound(t, slow.baseURL())
	out.cfg.StartTimeout = time.Second

	_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
	require.NoError(t, err)
}

func TestAsyncJob_SubmitWhoseAnswerIsLostIsNotSentAgain(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		lost   bool
	}{
		{"relay gateway error", http.StatusBadGateway, "<html>502 Bad Gateway</html>", true},
		{"gateway timeout", http.StatusGatewayTimeout, `{"code":"RequestTimeOut","message":"timeout"}`, true},
		{"DashScope's own server error", http.StatusInternalServerError, `{"code":"InternalError","message":"retry"}`, false},
		{"DashScope rejected the task", http.StatusBadRequest, `{"code":"InvalidParameter","message":"bad"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeDashScope(t)
			fake.set(func(f *fakeDashScope) {
				f.submitFailure.status, f.submitFailure.body = tc.status, tc.body
			})

			p, switcher := twoChannelPipeline(newScopedOutbound(t, fake.baseURL(), "channel:1", nil),
				newScopedOutbound(t, fake.baseURL(), "channel:2", nil))

			_, err := p.Process(t.Context(), multipartTranscription(t, "paraformer-v2", wavClip(800)))
			require.Error(t, err)

			if tc.lost {
				require.ErrorIs(t, err, pipeline.ErrNoRetry)
				require.Equal(t, "bailian_submit_unconfirmed", responseError(t, err).Detail.Code)
				require.Equal(t, 1, fake.snapshot().submitCalls, "a task that may exist is never submitted twice")
				require.Zero(t, switcher.switches+switcher.sameChannelRetries)
			} else {
				require.NotErrorIs(t, err, pipeline.ErrNoRetry, "no task exists, so another attempt may run")
				require.Greater(t, fake.snapshot().submitCalls, 1)
			}
		})
	}

	t.Run("no answer before the start timeout", func(t *testing.T) {
		fake := newFakeDashScope(t)
		fake.set(func(f *fakeDashScope) {
			f.hold = map[string]chan struct{}{"/api/v1/services/audio/asr/transcription": make(chan struct{})}
		})

		out := newTestOutbound(t, fake.baseURL())
		out.cfg.StartTimeout = 100 * time.Millisecond

		_, err := runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, wavClip(800)))
		require.ErrorIs(t, err, pipeline.ErrNoRetry, "the submit was sent, so its task may exist")
		require.Equal(t, http.StatusBadGateway, responseError(t, err).StatusCode)
	})
}

func TestAsyncJob_JoinedRequestOutlivesTheRequestThatStartedTheUpload(t *testing.T) {
	fake := newFakeDashScope(t)
	gate, started := make(chan struct{}), make(chan string, 4)
	fake.set(func(f *fakeDashScope) { f.hold, f.holdStarted = map[string]chan struct{}{"/oss": gate}, started })

	out := newTestOutbound(t, fake.baseURL())
	audio := wavClip(800)

	first, leave := context.WithCancel(t.Context())
	defer leave()

	var wg sync.WaitGroup

	var firstErr, secondErr error

	wg.Go(func() {
		_, firstErr = runNonStream(first, t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	<-started // the first request is uploading

	wg.Go(func() {
		_, secondErr = runNonStream(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", false, audio))
	})

	time.Sleep(200 * time.Millisecond) // the second request joins the shared upload

	leave()
	close(gate)
	wg.Wait()

	require.ErrorIs(t, firstErr, context.Canceled)
	require.NoError(t, secondErr, "the second client is still connected, so its request starts the upload again")
	require.Equal(t, 1, fake.snapshot().submitCalls)
}

func TestIsChannelNeutralFailure(t *testing.T) {
	for _, tc := range []struct {
		err     error
		neutral bool
	}{
		{taskFailedError("t", "DECODE_ERROR", "bad audio"), true},
		{taskFailedError("t", "FILE_TOO_LARGE", ""), true},
		{&httpclient.Error{StatusCode: http.StatusBadRequest}, true},
		{unfinishedTaskError("t", context.DeadlineExceeded, true), true},
		{taskFailedError("t", "InternalError.Algo", ""), false},
		{taskFailedError("t", "Arrearage", ""), false},
		{billedTaskError("t", "bailian_result_unavailable", ""), false},
		{&httpclient.Error{StatusCode: http.StatusUnauthorized}, false},
		{errors.New("connection reset"), false},
		{&llm.ResponseError{StatusCode: http.StatusBadGateway}, false},
	} {
		require.Equal(t, tc.neutral, IsChannelNeutralFailure(tc.err), "%v", tc.err)
	}
}
