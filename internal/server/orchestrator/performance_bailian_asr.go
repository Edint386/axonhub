package orchestrator

// custom: three "// fork:" lines in performance.go and one in outbound.go (guarded by
// performance_bailian_asr_test.go and listed in CUSTOM_BRANCH_RULES.md section 5):
//   - OnOutboundRawRequest sets PerformanceRecord.LatencyExcluded from excludesLatency (see
//     internal/server/biz/channel_bailian_asr_metrics.go for what the flag does);
//   - OnOutboundRawError lets markTranscriptionFailure mark a failed Bailian transcription
//     attempt before the generic code does;
//   - right after MarkSuccess in OnOutboundLlmResponse (non-stream) and in
//     OutboundPersistentStream.markPerformanceTerminal (stream), markTranscriptionSuccess turns
//     the success of an attempt that reused another request's job into a neutral record.

import (
	"errors"

	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// excludesLatency reports attempts whose duration must not move the channel's latency averages:
// Bailian file transcriptions (flash calls, async jobs and -message tasks), which share a
// channel with Bailian chat but last as long as their audio or longer.
func excludesLatency(request *httpclient.Request) bool {
	return bailianasr.IsTranscriptionRequest(request)
}

// markTranscriptionFailure marks the record of a failed Bailian file transcription attempt (one
// excludesLatency marked) and reports true; for any other attempt it reports false and the
// upstream code marks the record as before.
//
// The upstream code records every error that is not an *httpclient.Error as a 500 failure of
// the channel, and most bailianasr errors are *llm.ResponseError: a user's undecodable file
// would raise the consecutive failures of the channel it shares with Bailian chat (which moves
// chat off it) and feed auto-disable rules. Here a failure that says nothing about the channel
// (the audio or its parameters were rejected, the request deadline cut off a job that was still
// running, or a task another request submitted with another key could no longer be queried with
// that key; see bailianasr.IsChannelNeutralFailure) counts as neither a success nor a failure,
// like a cancellation. Any other failure still counts, with the status the client got
// (424 for a task that failed at Bailian, 504 for a stalled start, 502 for a lost submit answer,
// 500 for a transport error) instead of a blanket 500.
func markTranscriptionFailure(perf *biz.PerformanceRecord, err error) bool {
	if perf == nil || !perf.LatencyExcluded {
		return false
	}

	if bailianasr.IsChannelNeutralFailure(err) {
		perf.MarkCanceled()
		return true
	}

	status := ExtractErrorCode(err)
	if _, isHTTP := errors.AsType[*httpclient.Error](err); !isHTTP {
		if respErr, ok := errors.AsType[*llm.ResponseError](err); ok && respErr.StatusCode > 0 {
			status = respErr.StatusCode
		}
	}

	perf.MarkFailedWithMessage(status, extractErrorMessageForMatching(err))

	return true
}

// markTranscriptionSuccess runs right after MarkSuccess. A Bailian file transcription served
// without uploading and submitting its own audio (a cached result, a task the job cache held, or
// another request's submit it joined; see bailianasr.ReusedJob) used other credentials, or none
// of this channel's, for the work: its success says nothing about the channel or key the attempt
// was given, so it counts as neither a success nor a failure, like a cancellation (it neither
// clears that key's failures nor moves a round-robin cursor). Every other record stays a success.
func markTranscriptionSuccess(perf *biz.PerformanceRecord, state *PersistenceState) {
	if perf == nil || !perf.LatencyExcluded || state == nil || !reusedTranscriptionJob(state.RawProviderRequest) {
		return
	}

	perf.MarkCanceled()
}

// reusedTranscriptionJob is bailianasr.ReusedJob (a variable for the guard tests).
var reusedTranscriptionJob = bailianasr.ReusedJob
