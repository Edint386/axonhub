package pipeline

// fork: custom hooks for executors that run a whole paid job inside one attempt (the Bailian
// file transcription of internal/server/bailianasr). Two one-line "// fork:" hooks in
// pipeline.go call into this file; both are guarded by no_retry_fork_test.go and listed in
// CUSTOM_BRANCH_RULES.md section 5.

import (
	"errors"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
)

// ErrNoRetry marks an attempt that already committed paid upstream work (a submitted job that
// was billed or will be). Process returns such an error at once: no same-channel retry, no
// channel switch and no alias or model switch, because any of them would run and bill the job
// again. Wrap it into the error (for example as the cause of an llm.ResponseError); errors.Is
// finds it through the pipeline's own wrapping.
var ErrNoRetry = errors.New("the attempt committed billed upstream work and must not be retried")

// MetadataLongRunningJob is the httpclient.Request.Metadata key an outbound sets ("true") when
// its executor runs a whole long job inside one attempt (upload, submit and polling, a streamed
// audio task, or a synchronous call that answers only after minutes, such as a transcription
// that takes about twice the audio length). Such an attempt is not bounded by the per-attempt
// response timeouts (stream_first_event_timeout, non_stream_response_timeout): cutting it off
// would abandon a running, billed job and retry it elsewhere. The request context still bounds
// it.
const MetadataLongRunningJob = "axonhub_long_running_job"

// isNoRetryError reports an error carrying ErrNoRetry.
func isNoRetryError(err error) bool {
	return errors.Is(err, ErrNoRetry)
}

// jobResponseTimeouts drops both response timeouts for a long-running job attempt and keeps
// them for every other request.
func jobResponseTimeouts(req *httpclient.Request, streamFirstEventTimeout, nonStreamTimeout time.Duration) (time.Duration, time.Duration) {
	if req != nil && req.Metadata[MetadataLongRunningJob] == "true" {
		return 0, 0
	}

	return streamFirstEventTimeout, nonStreamTimeout
}
