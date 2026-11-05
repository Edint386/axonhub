package pipeline

// Guard tests for the two "// fork:" hooks in pipeline.go that call no_retry_fork.go (listed in
// CUSTOM_BRANCH_RULES.md section 5). A merge that drops either hook still compiles; these tests
// fail instead.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// noRetryOutbound offers every kind of retry: another alias target, same-channel retry and
// another channel.
func noRetryOutbound(t *testing.T) (*aliasMockOutbound, *int, *int) {
	t.Helper()

	canRetryCalls, nextChannelCalls := 0, 0

	return &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			canRetry: func(error) bool {
				canRetryCalls++
				return true
			},
			hasMoreChannels: func() bool { return true },
			nextChannel: func(context.Context) error {
				nextChannelCalls++
				return nil
			},
		},
		hasNextAliasTarget: func() bool { return true },
		nextAliasTarget:    func(context.Context, error) (bool, bool) { return true, false },
	}, &canRetryCalls, &nextChannelCalls
}

// billedJobError is what bailianasr returns once a job was submitted: a non-retryable status
// whose cause carries ErrNoRetry.
func billedJobError() error {
	return &llm.ResponseError{
		StatusCode: http.StatusFailedDependency,
		Detail:     llm.ErrorDetail{Code: "bailian_connection_lost", Message: "the task ended early"},
		Cause:      fmt.Errorf("bailian transcription task failed: %w", ErrNoRetry),
	}
}

// TestProcess_NoRetryErrorEndsThePipeline guards
//
//	if isNoRetryError(lastErr) { // fork: see no_retry_fork.go
//
// in Process: an attempt that committed billed work is never retried on the same channel, on
// another alias target or on another channel, even when all three are available.
func TestProcess_NoRetryErrorEndsThePipeline(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		outbound, canRetryCalls, nextChannelCalls := noRetryOutbound(t)
		executor, execCalls := aliasFailingExecutor(10, billedJobError())

		p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(3, 3, 0))

		_, err := p.Process(context.Background(), &httpclient.Request{})
		require.ErrorIs(t, err, ErrNoRetry)
		require.Equal(t, 1, *execCalls, "the billed job must run exactly once")
		require.Zero(t, outbound.nextAliasCalls, "no alias switch")
		require.Zero(t, *canRetryCalls, "no same-channel retry")
		require.Zero(t, *nextChannelCalls, "no channel switch")
	})

	t.Run("stream before the first event", func(t *testing.T) {
		outbound, canRetryCalls, nextChannelCalls := noRetryOutbound(t)
		streamCalls := 0
		executor := &mockExecutor{
			doStream: func(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
				streamCalls++
				return nil, billedJobError()
			},
		}

		p := NewFactory(executor).Pipeline(&mockInbound{transformRequest: streamingRequest(true)}, outbound, WithRetry(3, 3, 0))

		_, err := p.Process(context.Background(), &httpclient.Request{})
		require.ErrorIs(t, err, ErrNoRetry)
		require.Equal(t, 1, streamCalls)
		require.Zero(t, outbound.nextAliasCalls)
		require.Zero(t, *canRetryCalls)
		require.Zero(t, *nextChannelCalls)
	})

	t.Run("other errors still retry", func(t *testing.T) {
		outbound, _, nextChannelCalls := noRetryOutbound(t)
		outbound.hasNextAliasTarget = nil
		outbound.nextAliasTarget = nil
		outbound.canRetry = func(error) bool { return false }
		executor, execCalls := aliasFailingExecutor(1, &llm.ResponseError{StatusCode: http.StatusFailedDependency})

		p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(3, 3, 0))

		_, err := p.Process(context.Background(), &httpclient.Request{})
		require.NoError(t, err)
		require.Equal(t, 2, *execCalls)
		require.Equal(t, 1, *nextChannelCalls, "an error without ErrNoRetry keeps the upstream channel switch")
	})
}

// slowJobOutbound builds provider requests that are (or are not) marked as long-running jobs.
func slowJobOutbound(longRunning bool) *mockOutbound {
	return &mockOutbound{
		apiFormat: llm.APIFormatOpenAITranscription,
		transformRequest: func(context.Context, *llm.Request) (*httpclient.Request, error) {
			req := &httpclient.Request{Metadata: map[string]string{}}
			if longRunning {
				req.Metadata[MetadataLongRunningJob] = "true"
			}

			return req, nil
		},
		transformStream: func(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
			return streams.SliceStream([]*llm.Response{
				{Transcription: &llm.TranscriptionResponse{Raw: []byte("{}")}},
				llm.DoneResponse,
			}), nil
		},
	}
}

// slowJobExecutor takes 150 ms before it answers, longer than the 30 ms timeouts below.
func slowJobExecutor() *mockExecutor {
	wait := func(ctx context.Context) error {
		select {
		case <-time.After(150 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return &mockExecutor{
		do: func(ctx context.Context, _ *httpclient.Request) (*httpclient.Response, error) {
			if err := wait(ctx); err != nil {
				return nil, err
			}

			return &httpclient.Response{StatusCode: http.StatusOK}, nil
		},
		doStream: func(ctx context.Context, _ *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			if err := wait(ctx); err != nil {
				return nil, err
			}

			return streams.SliceStream([]*httpclient.StreamEvent{{Type: "committed"}}), nil
		},
	}
}

func streamingRequest(stream bool) func(context.Context, *httpclient.Request) (*llm.Request, error) {
	return func(context.Context, *httpclient.Request) (*llm.Request, error) {
		return &llm.Request{Stream: &stream}, nil
	}
}

// TestProcess_LongRunningJobIgnoresResponseTimeouts guards
//
//	streamFirstEventTimeout, nonStreamTimeout = jobResponseTimeouts(...) // fork: see no_retry_fork.go
//
// in processRequest: a provider request marked MetadataLongRunningJob is not cut off by the
// per-attempt response timeouts, while every other request still is.
func TestProcess_LongRunningJobIgnoresResponseTimeouts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			inbound := &mockInbound{transformRequest: streamingRequest(stream)}

			p := NewFactory(slowJobExecutor()).Pipeline(inbound, slowJobOutbound(true),
				WithResponseTimeouts(30*time.Millisecond, 30*time.Millisecond))

			result, err := p.Process(context.Background(), &httpclient.Request{})
			require.NoError(t, err, "a long-running job must not hit the per-attempt response timeouts")

			if result.EventStream != nil {
				require.NoError(t, result.EventStream.Close())
			}

			p = NewFactory(slowJobExecutor()).Pipeline(inbound, slowJobOutbound(false),
				WithResponseTimeouts(30*time.Millisecond, 30*time.Millisecond))

			_, err = p.Process(context.Background(), &httpclient.Request{})
			require.Error(t, err)
			require.True(t, isResponseTimeoutError(err), "unmarked requests keep the timeouts: %v", err)
		})
	}
}
