package bailianasr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm/httpclient"
)

// maxResultFetchFailures bounds result downloads that fail after the task succeeded.
const maxResultFetchFailures = 5

// unknownStatusGrace treats UNKNOWN as "not visible yet" right after a submit (a variable so
// tests can shorten it).
var unknownStatusGrace = 10 * time.Second

// noSpeechCodes are subtask "failures" that mean the audio holds no speech: an empty transcript.
var noSpeechCodes = map[string]bool{
	"SUCCESS_WITH_NO_VALID_FRAGMENT": true,
	"ASR_RESPONSE_HAVE_NO_WORDS":     true,
}

// errStartCallTimeout is the cause of a cancelled call before a task exists.
var errStartCallTimeout = errors.New("bailian start call timed out")

// submit sends the task with the uploaded file URL. A failure before the request was sent, or a
// rejection DashScope answered itself, comes before any task exists, so it keeps its normal
// (possibly retryable) status. A failure after the request was sent whose answer is lost may
// hide a task DashScope created: that one is reported with pipeline.ErrNoRetry (see
// unconfirmedSubmitError), so nothing submits the audio again.
//
// Once started, the call runs to DashScope's answer (at most the start timeout) even when the
// client goes away meanwhile: only the answer carries the task ID, and with it the job cache
// lets a resend, and the requests that joined this submit, wait for the task instead of paying
// for it again. A request whose client already left does not submit at all.
func (e *jobExecutor) submit(ctx context.Context, req *httpclient.Request, fileURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	body, err := sjson.SetBytes(req.Body, "input.file_urls", []string{fileURL})
	if err != nil {
		return "", fmt.Errorf("failed to set the task file URL: %w", err)
	}

	submitReq := *req
	submitReq.Body = body
	submitReq.Headers = req.Headers.Clone()
	if submitReq.Headers == nil {
		submitReq.Headers = http.Header{}
	}

	submitReq.Headers.Set("X-DashScope-Async", "enable")
	if strings.HasPrefix(fileURL, "oss://") {
		submitReq.Headers.Set("X-DashScope-OssResourceResolve", "enable")
	}

	timeout := e.cfg.startTimeout()
	callCtx, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), timeout, errStartCallTimeout)
	defer cancel()

	var sent atomic.Bool

	callCtx = httptrace.WithClientTrace(callCtx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				sent.Store(true)
			}
		},
	})

	resp, err := e.inner.Do(callCtx, &submitReq)
	if err != nil {
		return "", submitError(ctx, callCtx, err, sent.Load(), timeout)
	}

	root := gjson.ParseBytes(resp.Body)

	taskID := firstString(root.Get("output.task_id"), root.Get("task_id"))
	if taskID == "" {
		return "", newResponseError(http.StatusBadGateway, "bailian_submit", "Bailian accepted the transcription task but returned no task_id",
			root.Get("request_id").String(), nil)
	}

	return taskID, nil
}

// submitError sorts a failed submit into one that may be retried (nothing reached DashScope, or
// DashScope rejected it) and one whose task may exist.
func submitError(ctx, callCtx context.Context, err error, sent bool, timeout time.Duration) error {
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		// An answer came back. A 504, or a 5xx that is not DashScope's own error body (a relay
		// or proxy in front of it gave up), does not tell whether the task was created.
		if answerHidesOutcome(httpErr) {
			return unconfirmedSubmitError(fmt.Sprintf("HTTP %d from %s", httpErr.StatusCode, redactedTarget(httpErr.URL)), nil)
		}

		return err
	}

	timedOut := ctx.Err() == nil && errors.Is(context.Cause(callCtx), errStartCallTimeout)

	switch {
	case sent && timedOut:
		return unconfirmedSubmitError(fmt.Sprintf("no answer within %s", timeout), nil)
	case sent:
		return unconfirmedSubmitError(err.Error(), err)
	case timedOut:
		return startTimeoutError("task submit", timeout)
	default:
		return err
	}
}

// redactedTarget is a URL without its query string, for error messages.
func redactedTarget(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		return u.String()
	}

	return "Bailian"
}

// taskState is the useful part of the (messy, model-dependent) GET /api/v1/tasks/{id} JSON.
type taskState struct {
	status    string
	subStatus string
	resultURL string
	code      string
	message   string
	usage     dsUsage
}

func parseTaskState(body []byte) taskState {
	root := gjson.ParseBytes(body)
	output := root.Get("output")
	first := output.Get("results.0")

	usage := root.Get("usage")
	if !usage.Exists() {
		usage = output.Get("usage")
	}

	return taskState{
		status:    strings.ToUpper(firstString(output.Get("task_status"), root.Get("task_status"))),
		subStatus: strings.ToUpper(first.Get("subtask_status").String()),
		resultURL: firstString(first.Get("transcription_url"), output.Get("transcription_url"), root.Get("transcription_url")),
		code:      firstString(first.Get("code"), output.Get("code"), root.Get("code")),
		message:   firstString(first.Get("message"), output.Get("message"), root.Get("message")),
		usage:     parseUsage(usage),
	}
}

// await polls the task until it ends or ctx is done. Poll and download failures that may be
// temporary are retried here, never by the pipeline: once a task exists, every error returned
// is non-retryable (4xx) or comes with ctx already done, and errors for a task that was or will
// be billed carry pipeline.ErrNoRetry, so no other channel runs the job again. alive is called
// after every poll that found the task.
func (e *jobExecutor) await(ctx context.Context, j *job, task taskRef, alive func()) (*transcript, error) {
	taskID := task.id
	unfinished := func(err error) error { return unfinishedTaskError(taskID, err, task.resumable) }
	interval, maxInterval := e.cfg.pollIntervals()
	step := interval
	submitted := time.Now()
	downloadFailures := 0

	for {
		if err := sleepContext(ctx, interval); err != nil {
			return nil, unfinished(err)
		}

		interval = min(interval+step, maxInterval)

		state, err := e.queryTask(ctx, j, task)
		if err != nil {
			if ctx.Err() != nil {
				return nil, unfinished(ctx.Err())
			}

			if !isTransient(err) {
				return nil, pollError(taskID, err, task.foreign)
			}

			if isRateLimited(err) {
				interval = maxInterval
			}

			log.Debug(ctx, "bailian transcription poll failed, retrying", log.String("task_id", taskID), log.Cause(err))

			continue
		}

		if alive != nil {
			alive()
		}

		switch state.status {
		case "SUCCEEDED":
			if err := finishedTaskError(taskID, state); err != nil {
				return nil, err
			}

			if state.subStatus == "FAILED" { // no speech: an empty transcript, not an error
				return &transcript{ID: taskID, Usage: state.usage}, nil
			}

			body, err := e.download(ctx, state.resultURL)
			if err == nil {
				return resultTranscript(taskID, state, body)
			}

			if ctx.Err() != nil {
				return nil, unfinished(ctx.Err())
			}

			// The next poll returns a freshly signed URL, which also fixes an expired one.
			if downloadFailures++; downloadFailures >= maxResultFetchFailures {
				return nil, billedTaskError(taskID, "bailian_result_unavailable", err.Error())
			}

			log.Warn(ctx, "bailian transcription result download failed, polling again",
				log.String("task_id", taskID), log.Cause(err))
		case "FAILED", "CANCELED":
			return nil, taskFailedError(taskID, state.code, state.message)
		case "UNKNOWN":
			if time.Since(submitted) > unknownStatusGrace {
				return nil, unknownTaskError(task, state)
			}
		}
	}
}

func (e *jobExecutor) queryTask(ctx context.Context, j *job, task taskRef) (taskState, error) {
	resp, err := e.inner.Do(ctx, &httpclient.Request{
		Method:  http.MethodGet,
		URL:     j.native + tasksPath + url.PathEscape(task.id),
		Headers: task.headers.Clone(),
	})
	if err != nil {
		return taskState{}, err
	}

	return parseTaskState(resp.Body), nil
}

// finishedTaskError checks a SUCCEEDED task: a single-file task reports SUCCEEDED even when its
// only subtask failed, so the subtask status decides. A succeeded subtask without a result was
// billed all the same.
func finishedTaskError(taskID string, state taskState) error {
	switch {
	case state.subStatus == "FAILED" && !noSpeechCodes[state.code]:
		return taskFailedError(taskID, state.code, state.message)
	case state.subStatus != "FAILED" && state.resultURL == "":
		return billedTaskError(taskID, "bailian_result_missing", "the finished task has no transcription_url")
	default:
		return nil
	}
}

// download fetches the result file. transcription_url is a pre-signed OSS link, so no
// DashScope credentials go with it, and its query string (a 24 h credential) is kept out of
// the returned errors, which end up in logs and client messages.
func (e *jobExecutor) download(ctx context.Context, resultURL string) ([]byte, error) {
	resp, err := e.inner.Do(ctx, &httpclient.Request{Method: http.MethodGet, URL: resultURL})
	if err == nil {
		return resp.Body, nil
	}

	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return nil, fmt.Errorf("transcription result download returned HTTP %d", httpErr.StatusCode)
	}

	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return nil, fmt.Errorf("transcription result download failed: %w", urlErr.Err)
	}

	return nil, errors.New("transcription result download failed")
}

func resultTranscript(taskID string, state taskState, body []byte) (*transcript, error) {
	t, err := parseFileResult(body)
	if err != nil {
		return nil, newResponseError(http.StatusFailedDependency, "bailian_result_invalid", err.Error(), "", errBilledJobFailed)
	}

	t.ID = taskID
	t.Usage = state.usage

	return t, nil
}

// unknownTaskError reports a task DashScope still calls UNKNOWN after unknownStatusGrace. Every
// task here was accepted with a task_id, so within DashScope's retention (dashScopeTaskRetention
// from its submit) it runs and is billed although the query cannot see it (a relay in front of
// DashScope that queries with another account, or DashScope showing it late): it is reported as
// billed, nothing submits it again, and the job cache keeps it, so a resend polls it again. Only
// an older task (or a query DashScope rejects for good, see jobExecutor.resumable) may be gone,
// failed unbilled, and be submitted again.
func unknownTaskError(task taskRef, state taskState) error {
	code := firstNonEmpty(state.code, "UNKNOWN")
	if !task.fresh && !withinRetention(task.submitted, time.Now()) {
		return taskFailedError(task.id, code, state.message)
	}

	message := "Bailian accepted the task but its status query does not find it; it may still run and be billed, " +
		"so the gateway does not submit it again"
	if task.resumable {
		message += " (sending the same request again polls this task instead of starting a new one)"
	}

	if state.message != "" {
		message += " (" + state.message + ")"
	}

	return billedTaskError(task.id, code, message)
}

// withinRetention reports whether a task submitted at submitted can still be queried at now. A
// task whose submit time is unknown is taken as within it: treating a live task as gone would
// pay for it twice.
func withinRetention(submitted, now time.Time) bool {
	return submitted.IsZero() || now.Sub(submitted) < dashScopeTaskRetention
}

// errForeignTaskQuery marks a rejected query of a task another request submitted with other
// credentials (see taskRef.foreign): IsChannelNeutralFailure keeps it off this request's
// channel and key.
var errForeignTaskQuery = errors.New("the task was queried with the credentials of the request that submitted it")

// pollError reports a task query rejected for good (for example 401 or 404). The task itself
// may still run and be billed, so nothing retries it. The status is always 424: the rejected
// call is the gateway's query with the credentials the task was submitted with, not the
// caller's own request, so it must not read as the caller's key failing. When those credentials
// are not this request's own (foreign), the failure is not this channel's either.
func pollError(taskID string, err error, foreign bool) error {
	code, message := "", err.Error()

	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		code, message, _, _ = dashScopeError(httpErr.Body)
		if message == "" {
			message = fmt.Sprintf("task query returned HTTP %d", httpErr.StatusCode)
		}
	}

	respErr := billedTaskError(taskID, code, "the task query was rejected: "+message)
	respErr.StatusCode = http.StatusFailedDependency
	respErr.Detail.Type = errorType(http.StatusFailedDependency)

	if foreign {
		respErr.Cause = errors.Join(respErr.Cause, errForeignTaskQuery)
	}

	return respErr
}

// isJobFailure reports errors after which the task can never succeed (and was not billed), so
// the job cache must not hand it to the next attempt.
func isJobFailure(err error) bool {
	return errors.Is(err, errJobFailed)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
