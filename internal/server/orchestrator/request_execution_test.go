package orchestrator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
)

func TestExecutionLatencyMetrics(t *testing.T) {
	startTime := time.Date(2026, time.August, 13, 10, 0, 0, 0, time.UTC)

	t.Run("uses observation time while attempt is in progress", func(t *testing.T) {
		metrics := executionLatencyMetrics(&biz.PerformanceRecord{
			StartTime: startTime,
		}, startTime.Add(1250*time.Millisecond))

		require.NotNil(t, metrics)
		require.NotNil(t, metrics.LatencyMs)
		require.EqualValues(t, 1250, *metrics.LatencyMs)
	})

	t.Run("uses recorded end time for a completed attempt", func(t *testing.T) {
		metrics := executionLatencyMetrics(&biz.PerformanceRecord{
			StartTime:        startTime,
			EndTime:          startTime.Add(750 * time.Millisecond),
			RequestCompleted: true,
		}, startTime.Add(10*time.Second))

		require.NotNil(t, metrics)
		require.NotNil(t, metrics.LatencyMs)
		require.EqualValues(t, 750, *metrics.LatencyMs)
	})

	t.Run("returns no metrics before an attempt starts", func(t *testing.T) {
		require.Nil(t, executionLatencyMetrics(nil, startTime))
		require.Nil(t, executionLatencyMetrics(&biz.PerformanceRecord{}, startTime))
	})
}
