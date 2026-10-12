package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// aliasMockOutbound adds alias failover to mockOutbound.
type aliasMockOutbound struct {
	*mockOutbound

	hasNextAliasTarget func() bool
	nextAliasTarget    func(context.Context, error) (bool, bool)
	nextAliasCalls     int
}

func (m *aliasMockOutbound) HasNextAliasTarget() bool {
	if m.hasNextAliasTarget != nil {
		return m.hasNextAliasTarget()
	}

	return false
}

func (m *aliasMockOutbound) NextAliasTarget(ctx context.Context, err error) (bool, bool) {
	m.nextAliasCalls++
	if m.nextAliasTarget != nil {
		return m.nextAliasTarget(ctx, err)
	}

	return false, false
}

// aliasFailingExecutor fails the first failures calls with err and succeeds afterwards.
func aliasFailingExecutor(failures int, err error) (*mockExecutor, *int) {
	calls := 0

	return &mockExecutor{
		do: func(context.Context, *httpclient.Request) (*httpclient.Response, error) {
			calls++
			if calls <= failures {
				return nil, err
			}

			return &httpclient.Response{}, nil
		},
	}, &calls
}

func TestPipeline_Process_AliasFailoverDoesNotSpendSameChannelBudget(t *testing.T) {
	rateLimited := &httpclient.Error{StatusCode: http.StatusTooManyRequests}
	executor, execCalls := aliasFailingExecutor(2, rateLimited)

	canRetryCalls := 0
	targetsLeft := 2
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			canRetry: func(error) bool {
				canRetryCalls++
				return true
			},
		},
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			if targetsLeft == 0 {
				return false, false
			}
			targetsLeft--

			return true, false
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(0, 0, 0))

	res, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 3, *execCalls)
	require.Equal(t, 2, outbound.nextAliasCalls)
	require.Zero(t, canRetryCalls, "alias switches must not go through the same-channel budget")
}

func TestPipeline_Process_AliasFailoverSkipsRetryDelay(t *testing.T) {
	executor, execCalls := aliasFailingExecutor(1, &httpclient.Error{StatusCode: http.StatusTooManyRequests})

	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{},
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			return true, false
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(0, 0, 5*time.Second))

	start := time.Now()
	_, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.Equal(t, 2, *execCalls)
	require.Less(t, time.Since(start), 2*time.Second, "switching to another upstream model must not wait for the retry delay")
}

func TestPipeline_Process_AliasFailoverThenSameChannelRetry(t *testing.T) {
	executor, execCalls := aliasFailingExecutor(10, &httpclient.Error{StatusCode: http.StatusServiceUnavailable})

	prepareCalls := 0
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			canRetry: func(error) bool { return true },
			prepareForRetry: func(context.Context) error {
				prepareCalls++
				return nil
			},
		},
	}
	outbound.nextAliasTarget = func(context.Context, error) (bool, bool) {
		return outbound.nextAliasCalls == 1, false
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(0, 1, 0))

	_, err := p.Process(context.Background(), &httpclient.Request{})
	require.Error(t, err)
	// first target, alias switch, then the one same-channel retry on the last target.
	require.Equal(t, 3, *execCalls)
	require.Equal(t, 1, prepareCalls)
}

func TestPipeline_Process_AliasFailoverSkipsTimeouts(t *testing.T) {
	executor, execCalls := aliasFailingExecutor(1, fmt.Errorf("upstream: %w", ErrNonStreamResponseTimeout))

	switchCalls := 0
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			canRetry:        func(error) bool { return true },
			hasMoreChannels: func() bool { return true },
			nextChannel: func(context.Context) error {
				switchCalls++
				return nil
			},
		},
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			return true, false
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(1, 1, 0))

	_, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.Equal(t, 2, *execCalls)
	require.Zero(t, outbound.nextAliasCalls, "timeouts keep upstream semantics and leave the channel")
	require.Equal(t, 1, switchCalls)
}

func TestPipeline_Process_AliasFailoverAbandonedChannelSkipsSameChannelRetry(t *testing.T) {
	executor, execCalls := aliasFailingExecutor(1, &httpclient.Error{StatusCode: http.StatusTooManyRequests})

	canRetryCalls := 0
	switchCalls := 0
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			canRetry: func(error) bool {
				canRetryCalls++
				return true
			},
			hasMoreChannels: func() bool { return true },
			nextChannel: func(context.Context) error {
				switchCalls++
				return nil
			},
		},
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			return false, true
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(1, 2, 0))

	_, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.Equal(t, 2, *execCalls)
	require.Zero(t, canRetryCalls, "a channel that left service must not be retried")
	require.Equal(t, 1, switchCalls)
}

func TestPipeline_Process_AliasFailoverCapPerChannel(t *testing.T) {
	executor, execCalls := aliasFailingExecutor(1000, &httpclient.Error{StatusCode: http.StatusTooManyRequests})

	switchCalls := 0
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			hasMoreChannels: func() bool { return switchCalls == 0 },
			nextChannel: func(context.Context) error {
				switchCalls++
				return nil
			},
		},
		// A misbehaving outbound that always claims a switch.
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			return true, false
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(1, 0, 0))

	_, err := p.Process(context.Background(), &httpclient.Request{})
	require.Error(t, err)
	require.Equal(t, 1, switchCalls)
	// Two channels, each with its first attempt plus the capped alias switches.
	require.Equal(t, 2*(1+maxAliasSwitchesPerChannel), *execCalls)
}

func TestPipeline_Process_AliasFailoverStopsOnHardErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{},
		nextAliasTarget: func(context.Context, error) (bool, bool) {
			return true, false
		},
	}

	executor := &mockExecutor{
		do: func(context.Context, *httpclient.Request) (*httpclient.Response, error) {
			cancel()
			return nil, errors.New("client went away")
		},
	}

	p := NewFactory(executor).Pipeline(&mockInbound{}, outbound, WithRetry(0, 0, 0))

	_, err := p.Process(ctx, &httpclient.Request{})
	require.Error(t, err)
	require.Zero(t, outbound.nextAliasCalls, "a canceled request must not switch alias targets")
}

func TestPipeline_HasStreamRetryBudgetCountsAliasTargets(t *testing.T) {
	hasNext := true
	outbound := &aliasMockOutbound{
		mockOutbound:       &mockOutbound{},
		hasNextAliasTarget: func() bool { return hasNext },
	}

	p := NewFactory(&mockExecutor{}).Pipeline(&mockInbound{}, outbound)
	require.True(t, p.hasStreamRetryBudget(), "a remaining alias target needs pre-commit buffering")

	hasNext = false
	require.False(t, p.hasStreamRetryBudget())

	plain := NewFactory(&mockExecutor{}).Pipeline(&mockInbound{}, &mockOutbound{})
	require.False(t, plain.hasStreamRetryBudget())
}

func TestPipeline_Process_AliasFailoverRetriesStreamErrorBeforeContent(t *testing.T) {
	streamFlag := true
	upstreamErr := &llm.ResponseError{
		StatusCode: http.StatusTooManyRequests,
		Detail:     llm.ErrorDetail{Message: "rate limit reached for model", Type: "tokens"},
	}

	inbound := &mockInbound{
		transformRequest: func(context.Context, *httpclient.Request) (*llm.Request, error) {
			return &llm.Request{Stream: &streamFlag}, nil
		},
		transformStream: transformLlmContentToEvents,
	}

	attempts := 0
	executor := &mockExecutor{
		doStream: func(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			attempts++
			return streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("raw")}}), nil
		},
	}

	hasNext := true
	outbound := &aliasMockOutbound{
		mockOutbound: &mockOutbound{
			transformStream: func(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
				if attempts == 1 {
					return &llmErrorAfterStream{
						items: []*llm.Response{{
							Object:  "chat.completion.chunk",
							Choices: []llm.Choice{{Delta: &llm.Message{}}},
						}},
						err: upstreamErr,
					}, nil
				}

				return streams.SliceStream([]*llm.Response{llmContentChunk("ok"), llm.DoneResponse}), nil
			},
		},
		hasNextAliasTarget: func() bool { return hasNext },
		nextAliasTarget: func(_ context.Context, err error) (bool, bool) {
			if !hasNext || !errors.Is(err, upstreamErr) {
				return false, false
			}
			hasNext = false

			return true, false
		},
	}

	// No retry budget at all: only the remaining alias target keeps the
	// pre-content stream error inside the pipeline.
	p := NewFactory(executor).Pipeline(inbound, outbound, WithRetry(0, 0, 0))

	res, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.True(t, res.Stream)
	require.Equal(t, 2, attempts)
	require.Equal(t, 1, outbound.nextAliasCalls)

	events, err := streams.All(res.EventStream)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "ok", string(events[0].Data))
}
