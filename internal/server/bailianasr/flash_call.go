package bailianasr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// isFlash reports a qwen-audio-3.1-asr-flash generation call built by this package.
func isFlash(req *httpclient.Request) bool {
	return req != nil && req.Metadata[metaKind] == kindFlash
}

// doFlash runs one flash call. The call is synchronous but long (about twice the audio length),
// and Bailian works on, and bills, the audio once it received the request. The pipeline's
// per-attempt response timeouts no longer cut it (it is a long-running job, see
// providerRequest), but a broken connection or the request deadline still can: once the request
// body was sent, such a failure carries pipeline.ErrNoRetry (see flashCallError), so no other
// attempt sends, and pays for, the same audio again. A failure before the body was sent, and an
// answer Bailian gave (an HTTP error), keep their usual handling.
func (e *jobExecutor) doFlash(ctx context.Context, req *httpclient.Request) (*httpclient.Response, error) {
	callCtx, sent := traceRequestSent(ctx)

	resp, err := e.inner.Do(callCtx, req)
	if err != nil {
		return nil, flashCallError(ctx, err, sent.Load())
	}

	return resp, nil
}

// doFlashStream is doFlash for stream=true. All events of the call come at its end, so the stream
// can still break before any of them: that failure also carries pipeline.ErrNoRetry.
func (e *jobExecutor) doFlashStream(ctx context.Context, req *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	callCtx, sent := traceRequestSent(ctx)

	stream, err := e.inner.DoStream(callCtx, req)
	if err != nil {
		return nil, flashCallError(ctx, err, sent.Load())
	}

	return httpclient.WithResponseHeaders(&flashStream{Stream: stream, ctx: ctx}, httpclient.GetResponseHeaders(stream)), nil
}

// traceRequestSent reports, through the returned flag, whether the HTTP request was written.
func traceRequestSent(ctx context.Context) (context.Context, *atomic.Bool) {
	var sent atomic.Bool

	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				sent.Store(true)
			}
		},
	}), &sent
}

// flashCallError sorts a failed flash call. A failure before the request was sent reached nobody
// and is returned as it is. After the request was sent, an HTTP error Bailian answered itself is
// returned as it is too, but a 504, or a 5xx whose body is not DashScope's own error (a relay or
// proxy in front of it gave up; the same rule as submitError), does not tell whether Bailian ran
// the call: like any other failure after the request was sent, it lost the answer of a call
// Bailian may have run and billed, and carries pipeline.ErrNoRetry. When the request context
// ended (the client left, or the request deadline passed) such a failure reads like a job the
// deadline cut off (bailian_task_unfinished, which says nothing about the channel); otherwise it
// is bailian_flash_answer_lost, a failure of the channel. The cause never holds an
// *httpclient.Error, which the pipeline would replace (losing the marker).
func flashCallError(ctx context.Context, err error, sent bool) error {
	if !sent {
		return err
	}

	if httpErr, answered := errors.AsType[*httpclient.Error](err); answered {
		if !answerHidesOutcome(httpErr) {
			return err
		}

		return flashAnswerLostError(fmt.Sprintf("HTTP %d from %s", httpErr.StatusCode, redactedTarget(httpErr.URL)), pipeline.ErrNoRetry)
	}

	if errors.Is(err, pipeline.ErrNoRetry) {
		return err
	}

	cause := errors.Join(err, pipeline.ErrNoRetry)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return newResponseError(http.StatusGatewayTimeout, "bailian_task_unfinished",
			fmt.Sprintf("Bailian was still transcribing when the request ended (%v); a qwen-audio-3.1-asr-flash call is "+
				"not resumed and Bailian may bill it, so the gateway does not send the audio again", ctxErr), "", cause)
	}

	return flashAnswerLostError(err.Error(), cause)
}

// flashAnswerLostError reports a flash call whose audio was sent but whose answer was lost.
func flashAnswerLostError(detail string, cause error) error {
	return newResponseError(http.StatusBadGateway, "bailian_flash_answer_lost",
		fmt.Sprintf("the audio was sent to Bailian but the answer was lost (%s); Bailian may have transcribed and billed it, "+
			"so the gateway does not send it again", detail), "", cause)
}

// flashStream marks a broken flash stream (a transport or decoding failure, not an error event
// Bailian sent) with pipeline.ErrNoRetry: the request was sent long before.
type flashStream struct {
	streams.Stream[*httpclient.StreamEvent]

	ctx context.Context
}

func (s *flashStream) Err() error {
	err := s.Stream.Err()
	if err == nil {
		return nil
	}

	return flashCallError(s.ctx, err, true)
}
