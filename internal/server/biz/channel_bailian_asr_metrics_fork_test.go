package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
)

// TestRecordPerformance_LatencyExcludedSuccessKeepsTheLatencyScores guards the two fork lines
// in channel_metrics.go: the PerformanceRecord.LatencyExcluded field and
//
//	if perf.LatencyExcluded { // fork: see channel_bailian_asr_metrics.go
//		return
//	}
//
// in recordSuccess. A three-minute Bailian transcription job on a channel that also serves chat
// must count as a success (it resets the failure streak) without moving the latency average the
// load balancer ranks the channel by. Listed in CUSTOM_BRANCH_RULES.md section 5.
func TestRecordPerformance_LatencyExcludedSuccessKeepsTheLatencyScores(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:bailian-asr-latency?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)

	const channelID = 9

	now := time.Now()
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: channelID, Source: entrequest.SourceAPI, StartTime: now.Add(-time.Second), EndTime: now,
		Success: true, RequestCompleted: true,
	})
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: channelID, Source: entrequest.SourceAPI, StartTime: now.Add(-time.Second), EndTime: now,
		RequestCompleted: true, ResponseStatusCode: 500,
	})

	before, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Positive(t, before.NonStreamingLatencyEWMA)
	require.Equal(t, int64(1), before.ConsecutiveFailures)

	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID: channelID, Source: entrequest.SourceAPI, StartTime: now.Add(-3 * time.Minute), EndTime: now,
		Success: true, RequestCompleted: true, LatencyExcluded: true,
	})

	after, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Equal(t, before.NonStreamingLatencyEWMA, after.NonStreamingLatencyEWMA, "the job's duration must not move the latency average")
	require.Equal(t, before.NonStreamingSampleCount, after.NonStreamingSampleCount)
	require.Equal(t, before.SuccessCount+1, after.SuccessCount, "it still counts as a success")
	require.Zero(t, after.ConsecutiveFailures, "and still ends a failure streak")
}
