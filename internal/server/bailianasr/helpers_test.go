package bailianasr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// recordingOutbound stands in for the wrapped Bailian outbound.
type recordingOutbound struct {
	requests  int
	responses int
	errors    int
}

var errFromInner = errors.New("handled by the wrapped outbound")

func (r *recordingOutbound) APIFormat() llm.APIFormat { return llm.APIFormatOpenAIChatCompletion }

func (r *recordingOutbound) TransformRequest(context.Context, *llm.Request) (*httpclient.Request, error) {
	r.requests++
	return &httpclient.Request{URL: "https://inner.example/compatible-mode/v1/chat/completions"}, nil
}

func (r *recordingOutbound) TransformResponse(context.Context, *httpclient.Response) (*llm.Response, error) {
	r.responses++
	return &llm.Response{ID: "inner"}, nil
}

func (r *recordingOutbound) TransformStream(
	context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*llm.Response], error) {
	return nil, errFromInner
}

func (r *recordingOutbound) TransformError(context.Context, *httpclient.Error) *llm.ResponseError {
	r.errors++
	return &llm.ResponseError{StatusCode: 418, Detail: llm.ErrorDetail{Message: "inner"}}
}

func (r *recordingOutbound) AggregateStreamChunks(
	context.Context, *httpclient.Request, []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, errFromInner
}

func newTestOutbound(t *testing.T, baseURL string) *Outbound {
	t.Helper()

	out := Wrap(&recordingOutbound{}, Config{
		BaseURL:         baseURL,
		APIKeyProvider:  auth.NewStaticKeyProvider(fakeAPIKey),
		CacheScope:      t.Name(),
		PollInterval:    time.Millisecond,
		MaxPollInterval: 3 * time.Millisecond,
	})
	out.cache = newJobCache(jobCacheTTL, 16)

	return out
}

func transcriptionRequest(model, format string, stream bool, audio []byte) *llm.Request {
	return &llm.Request{
		Model:       model,
		Stream:      lo.ToPtr(stream),
		RequestType: llm.RequestTypeTranscription,
		APIFormat:   llm.APIFormatOpenAITranscription,
		Transcription: &llm.TranscriptionRequest{
			File:           audio,
			FileName:       "clip.wav",
			Language:       "zh",
			ResponseFormat: format,
		},
	}
}

// buildProviderRequest runs TransformRequest and the auth finalization the pipeline applies.
func buildProviderRequest(ctx context.Context, t *testing.T, out *Outbound, req *llm.Request) *httpclient.Request {
	t.Helper()

	httpReq, err := out.TransformRequest(ctx, req)
	require.NoError(t, err)

	httpReq, err = httpclient.FinalizeAuthHeaders(httpReq)
	require.NoError(t, err)

	return httpReq
}

func executor(out *Outbound) pipeline.Executor {
	return out.CustomizeExecutor(httpclient.NewHttpClient())
}

// runNonStream drives one non-stream attempt the way pipeline.notStream does. It does not
// fail the test itself, so goroutines may call it.
func runNonStream(ctx context.Context, _ *testing.T, out *Outbound, req *llm.Request) (*llm.Response, error) {
	httpReq, err := out.TransformRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	httpReq, err = httpclient.FinalizeAuthHeaders(httpReq)
	if err != nil {
		return nil, err
	}

	resp, err := executor(out).Do(ctx, httpReq)
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return nil, out.TransformError(ctx, httpErr)
	}

	if err != nil {
		return nil, err
	}

	return out.TransformResponse(ctx, resp)
}

// runStream drives one stream attempt and returns the unified responses plus the raw events.
func runStream(ctx context.Context, t *testing.T, out *Outbound, req *llm.Request) ([]*llm.Response, []*httpclient.StreamEvent, *httpclient.Request) {
	t.Helper()

	httpReq := buildProviderRequest(ctx, t, out, req)

	raw, err := executor(out).DoStream(ctx, httpReq)
	require.NoError(t, err)

	var events []*httpclient.StreamEvent

	recorded := streams.Map(raw, func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
		events = append(events, event)
		return event
	})

	stream, err := out.TransformStream(ctx, httpReq, recorded)
	require.NoError(t, err)

	responses, err := streams.All(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())

	return responses, events, httpReq
}

func transcriptionEvents(responses []*llm.Response) []*llm.TranscriptionStreamEvent {
	var events []*llm.TranscriptionStreamEvent

	for _, resp := range responses {
		if resp != nil && resp.TranscriptionStreamEvent != nil {
			events = append(events, resp.TranscriptionStreamEvent)
		}
	}

	return events
}

func responseError(t *testing.T, err error) *llm.ResponseError {
	t.Helper()

	respErr, ok := errors.AsType[*llm.ResponseError](err)
	require.True(t, ok, "expected *llm.ResponseError, got %T: %v", err, err)

	return respErr
}
