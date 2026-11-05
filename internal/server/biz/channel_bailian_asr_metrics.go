package biz

// custom: PerformanceRecord.LatencyExcluded (two "// fork:" lines in channel_metrics.go: the field
// and the early return in recordSuccess, guarded by channel_bailian_asr_metrics_fork_test.go and
// listed in CUSTOM_BRANCH_RULES.md section 5).
//
// A Bailian file transcription (internal/server/bailianasr) runs on the same channel as Bailian
// chat but takes seconds to many minutes: a flash call about twice the audio length, an async job
// the upload plus the queue, a -message task the paced audio. The orchestrator marks such an
// attempt (internal/server/orchestrator/performance_bailian_asr.go), and its success then counts
// as a success (it resets consecutive failures and auto-disable counts) without moving the
// latency averages the load balancer ranks the channel by for chat. Its failures still count,
// except those that say nothing about the channel (rejected audio, a deadline that cut off a
// running job), which the orchestrator records as neither (markTranscriptionFailure); so does a
// success served by another request's job, which used none of this attempt's credentials
// (markTranscriptionSuccess).
