package bailianasr

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// dashScopeError reads the flat {"code","message","request_id"} body of the native API.
func dashScopeError(body []byte) (code, message, requestID string, ok bool) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() || root.Get("error").Exists() {
		return "", "", "", false
	}

	code = root.Get("code").String()
	message = root.Get("message").String()

	return code, message, root.Get("request_id").String(), code != "" || message != ""
}

// isNativeAPIURL tells native DashScope calls (/api/v1/...) from compatible-mode ones, whose
// OpenAI-shaped errors the wrapped outbound handles.
func isNativeAPIURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.Contains(u.Path, "/api/v1/")
}

// answerHidesOutcome reports an HTTP answer to a request that reached the far side but that does
// not tell whether DashScope took it (created the task, ran and billed the call): a 504, or a 5xx
// whose body is not DashScope's own error (a relay or proxy in front of it gave up).
func answerHidesOutcome(httpErr *httpclient.Error) bool {
	if httpErr.StatusCode == http.StatusGatewayTimeout {
		return true
	}

	_, _, _, fromDashScope := dashScopeError(httpErr.Body)

	return httpErr.StatusCode >= http.StatusInternalServerError && !fromDashScope
}

func nativeHTTPError(httpErr *httpclient.Error) (*llm.ResponseError, bool) {
	if httpErr == nil || !isNativeAPIURL(httpErr.URL) {
		return nil, false
	}

	code, message, requestID, ok := dashScopeError(httpErr.Body)
	if !ok {
		return nil, false
	}

	status := httpErr.StatusCode
	if status < http.StatusBadRequest {
		status = statusForCode(code, http.StatusBadGateway)
	}

	return newResponseError(status, code, message, requestID, httpErr), true
}

// statusForCode maps a DashScope error or subtask code to an HTTP status.
func statusForCode(code string, fallback int) int {
	upper := strings.ToUpper(code)

	switch {
	case code == "FILE_TOO_LARGE":
		return http.StatusRequestEntityTooLarge
	case code == "FILE_SERVER_ERROR", code == "FILE_TRANS_TASK_EXPIRED":
		return fallback
	case strings.HasPrefix(upper, "INVALIDPARAMETER"), strings.HasPrefix(upper, "INVALID_PARAMETER"),
		strings.HasPrefix(upper, "INVALIDFILE"), strings.HasPrefix(upper, "DATAINSPECTION"),
		strings.HasPrefix(upper, "BADREQUEST"), strings.HasPrefix(upper, "FILE_"),
		strings.HasPrefix(upper, "AUDIO_"), strings.HasPrefix(upper, "DECODE"),
		strings.HasPrefix(upper, "NO_VALID_AUDIO"), strings.HasPrefix(upper, "MKV_"),
		strings.HasPrefix(upper, "REQUEST_INVALID"), strings.HasPrefix(upper, "CONTENT_LENGTH"):
		return http.StatusBadRequest
	case code == "InvalidApiKey":
		return http.StatusUnauthorized
	case strings.HasPrefix(upper, "ARREARAGE"), strings.HasPrefix(upper, "ACCESSDENIED"),
		strings.HasPrefix(upper, "ALLOCATIONQUOTA"):
		return http.StatusForbidden
	case strings.HasPrefix(upper, "MODELNOTFOUND"):
		return http.StatusNotFound
	case strings.HasPrefix(upper, "THROTTLING"):
		return http.StatusTooManyRequests
	default:
		return fallback
	}
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusGatewayTimeout:
		return "timeout_error"
	case status == http.StatusFailedDependency:
		return "upstream_error"
	case status >= http.StatusInternalServerError:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func newResponseError(status int, code, message, requestID string, cause error) *llm.ResponseError {
	return &llm.ResponseError{
		StatusCode: status,
		Detail: llm.ErrorDetail{
			Code:      code,
			Message:   message,
			Type:      errorType(status),
			RequestID: requestID,
		},
		Cause: cause,
	}
}

// errJobFailed marks a task that ended for good without a usable result and without being
// billed (FAILED, CANCELED, UNKNOWN, a failed subtask): the job cache forgets it, so a repeated
// request submits the audio again.
var errJobFailed = errors.New("bailian transcription task failed")

// errBilledJobFailed marks a task that was billed (or will be) although this request has no
// transcript for it. It carries pipeline.ErrNoRetry, so the pipeline neither retries the channel
// nor switches to another channel, alias target or model, any of which would run and bill the job
// again. It is not errJobFailed: the job cache keeps the task, so a repeated request polls the
// same task again (a result download that failed may work by then) instead of paying for a new
// one.
var errBilledJobFailed = fmt.Errorf("bailian transcription task has no usable result: %w", pipeline.ErrNoRetry)

// taskFailedError reports a task that ended without a usable result and without being billed
// (FAILED, CANCELED, UNKNOWN, a failed subtask). The gateway returns only non-retryable statuses
// after a submit, so codes that would map to 429 or 5xx become 424; another channel may still
// take the request.
func taskFailedError(taskID, code, message string) *llm.ResponseError {
	return jobError(taskID, code, message, errJobFailed)
}

// billedTaskError reports a task that was billed (or still runs and will be) but has no usable
// result for this request: the result could not be downloaded or parsed, the task could no
// longer be queried, or a -message task broke off after its audio started. Nothing retries it.
func billedTaskError(taskID, code, message string) *llm.ResponseError {
	return jobError(taskID, code, message, errBilledJobFailed)
}

func jobError(taskID, code, message string, cause error) *llm.ResponseError {
	status := statusForCode(code, http.StatusFailedDependency)
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		status = http.StatusFailedDependency
	}

	if code == "" {
		code = "bailian_task_failed"
	}

	text := fmt.Sprintf("Bailian transcription task %s failed: %s", taskID, code)
	if message != "" {
		text += ": " + message
	}

	return newResponseError(status, code, text, "", cause)
}

// unfinishedTaskError reports a task the request deadline (or a client disconnect) cut off. Its
// cause keeps the context error and pipeline.ErrNoRetry: the task still runs and is billed, so
// nothing retries it. A resumable task stays in the job cache, so a repeated request resumes it;
// a task for audio given by URL is not kept (see urlSource.Digest).
func unfinishedTaskError(taskID string, cause error, resumable bool) *llm.ResponseError {
	next := "sending the same request again with the same API key within 30 minutes resumes this task instead of " +
		"starting a new one (unless the gateway restarts meanwhile)"
	if !resumable {
		next = "it keeps running and is billed; a request with an audio URL is not resumed, so sending it again starts a new task"
	}

	message := fmt.Sprintf("Bailian transcription task %s was still running when the request ended (%v); %s", taskID, cause, next)

	return newResponseError(http.StatusGatewayTimeout, "bailian_task_unfinished", message, "", errors.Join(cause, pipeline.ErrNoRetry))
}

// startTimeoutError reports a step before any task exists (getPolicy, the temporary-storage
// upload, a submit that was never sent) that got no answer, or made no progress, for d. Nothing
// was billed, so it stays retryable; it wraps pipeline.ErrNonStreamResponseTimeout, the error the
// response timeout this long-running attempt is exempt from would have raised, so the pipeline
// moves on to the next channel as it did before the exemption.
func startTimeoutError(step string, d time.Duration) *llm.ResponseError {
	return newResponseError(http.StatusGatewayTimeout, "bailian_start_timeout",
		fmt.Sprintf("Bailian did not answer the %s within %s", step, d), "", pipeline.ErrNonStreamResponseTimeout)
}

// unconfirmedSubmitError reports a submit whose request reached the far side but whose answer
// was lost (a broken connection, a timeout, or a gateway error from a relay in front of
// DashScope): DashScope may have created, and will bill, the task. It carries
// pipeline.ErrNoRetry, so no other attempt submits the audio again, and the job cache keeps a
// pending entry for pendingSubmitTTL (see pendingSubmitError). The cause never holds an
// *httpclient.Error, which the pipeline would replace (and lose the marker with).
func unconfirmedSubmitError(detail string, cause error) *llm.ResponseError {
	return newResponseError(http.StatusBadGateway, codeSubmitUnconfirmed,
		fmt.Sprintf("the transcription task was sent to Bailian but its answer was lost (%s); Bailian may have "+
			"started the task and will bill it, so the gateway does not retry it. The same uploaded file sent again "+
			"with the same API key within %d minutes is refused (409) while that task may still be running; after "+
			"that, or for audio given by URL, sending it again starts, and is billed as, a new task",
			detail, int(pendingSubmitTTL/time.Minute)),
		"", errors.Join(cause, pipeline.ErrNoRetry))
}

const (
	codeSubmitUnconfirmed = "bailian_submit_unconfirmed"
	codeSubmitPending     = "bailian_submit_pending"
)

func isUnconfirmedSubmit(err error) bool {
	respErr, ok := errors.AsType[*llm.ResponseError](err)
	return ok && respErr.Detail.Code == codeSubmitUnconfirmed
}

// pendingSubmitError refuses a request whose identical predecessor's submit was sent less than
// pendingSubmitTTL ago and got no answer (see unconfirmedSubmitError): that task may still run
// and be billed, and submitting the audio again would pay for it twice. It carries
// pipeline.ErrNoRetry (every other attempt would meet the same pending entry) and says nothing
// about the channel.
func pendingSubmitError() *llm.ResponseError {
	return newResponseError(http.StatusConflict, codeSubmitPending,
		fmt.Sprintf("the previous submit of this transcription (the same file, parameters and API key) may still be "+
			"running at Bailian: it was sent less than %d minutes ago and its answer was lost, so the gateway does not "+
			"submit the audio again yet; retry later (a retry after that starts, and is billed as, a new task)",
			int(pendingSubmitTTL/time.Minute)),
		"", pipeline.ErrNoRetry)
}

// errJoinedSubmit marks a failure of the shared upload and submit another request led with its own
// credentials (see jobExecutor.submitOnce): it says nothing about the channel or key of the
// request that joined it.
var errJoinedSubmit = errors.New("the shared submit of another request failed")

// joinedSubmitError is another request's submit failure handed to a request that joined it. It
// reads as that failure (the pipeline still finds an *httpclient.Error in it) and carries
// errJoinedSubmit for IsChannelNeutralFailure.
type joinedSubmitError struct {
	err error
}

func (e *joinedSubmitError) Error() string { return e.err.Error() }

func (e *joinedSubmitError) Unwrap() []error { return []error{e.err, errJoinedSubmit} }

// IsChannelNeutralFailure reports a transcription failure that says nothing about the health of
// the channel: the client's audio or parameters were rejected (400, 413), the request deadline
// cut off a job that was still running, a task another request submitted with other credentials
// could no longer be queried with them, the shared submit another request led failed, or an
// identical request's submit is still pending. Channel performance records treat it as neither a
// success nor a failure.
func IsChannelNeutralFailure(err error) bool {
	if respErr, ok := errors.AsType[*llm.ResponseError](err); ok &&
		(respErr.Detail.Code == "bailian_task_unfinished" || respErr.Detail.Code == codeSubmitPending) {
		return true
	}

	if errors.Is(err, errForeignTaskQuery) || errors.Is(err, errJoinedSubmit) {
		return true
	}

	var status int

	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		status = httpErr.StatusCode
	} else if respErr, ok := errors.AsType[*llm.ResponseError](err); ok {
		status = respErr.StatusCode
	}

	return status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge
}

// isTransient reports errors worth retrying inside the executor: transport failures, 429 and 5xx.
func isTransient(err error) bool {
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}

	if _, ok := errors.AsType[*llm.ResponseError](err); ok {
		return false
	}

	return true
}

func isRateLimited(err error) bool {
	httpErr, ok := errors.AsType[*httpclient.Error](err)
	return ok && httpErr.StatusCode == http.StatusTooManyRequests
}
