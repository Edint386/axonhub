package bailianasr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// An async stream commits right after submit (so the handler can flush SSE headers and send
// keep-alive comments) and only then waits for the task.
func TestAsyncJob_StreamCommitsRightAfterSubmit(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
	out := newTestOutbound(t, fake.baseURL())

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", true, wavClip(800)))

	raw, err := executor(out).DoStream(t.Context(), httpReq)
	require.NoError(t, err)

	defer raw.Close()

	var events []*httpclient.StreamEvent

	recorded := streams.Map(raw, func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
		events = append(events, event)
		return event
	})

	stream, err := out.TransformStream(t.Context(), httpReq, recorded)
	require.NoError(t, err)

	require.True(t, stream.Next())
	first := stream.Current()
	require.NotNil(t, first.Transcription)
	require.Equal(t, "{}", string(first.Transcription.Raw))
	require.Nil(t, first.TranscriptionStreamEvent)
	require.Equal(t, committedEventType, events[0].Type)
	require.Equal(t, fakeTaskID, gjson.GetBytes(events[0].Data, "task_id").String())

	calls := fake.snapshot()
	require.Equal(t, 1, calls.submitCalls)
	require.Zero(t, calls.resultCalls, "the task is still running")

	fake.set(func(f *fakeDashScope) { f.statuses = []string{"SUCCEEDED"} })

	rest, err := streams.All(stream)
	require.NoError(t, err)

	events2 := transcriptionEvents(rest)
	require.Len(t, events2, 2)
	require.Equal(t, paraformerTxt, events2[0].Delta)
	require.Equal(t, "transcript.text.done", events2[1].Type)
	require.InDelta(t, 8, rest[len(rest)-2].Usage.DurationSeconds, 0.001)

	_, meta, err := out.AggregateStreamChunks(t.Context(), httpReq, events)
	require.NoError(t, err)
	require.True(t, meta.Completed)
	require.InDelta(t, 8, meta.Usage.DurationSeconds, 0.001)
}

func TestAsyncJob_ClosingTheStreamStopsPolling(t *testing.T) {
	fake := newFakeDashScope(t)
	fake.set(func(f *fakeDashScope) { f.statuses = []string{"RUNNING"} })
	out := newTestOutbound(t, fake.baseURL())

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", true, wavClip(800)))

	raw, err := executor(out).DoStream(t.Context(), httpReq)
	require.NoError(t, err)
	require.True(t, raw.Next())

	require.Eventually(t, func() bool { return fake.snapshot().pollCalls > 0 }, 2*time.Second, time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = raw.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the poll loop")
	}

	polls := fake.snapshot().pollCalls
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, polls, fake.snapshot().pollCalls, "no polling after Close")
}

// A pre-submit failure is still a DoStream error, so the pipeline may retry it.
func TestAsyncJob_StreamPreSubmitErrorsComeFromDoStream(t *testing.T) {
	fake := newFakeDashScope(t)
	out := newTestOutbound(t, fake.baseURL())
	out.cfg.APIKeyProvider = auth.NewStaticKeyProvider("sk-wrong")

	httpReq := buildProviderRequest(t.Context(), t, out, transcriptionRequest("paraformer-v2", "", true, wavClip(800)))

	_, err := executor(out).DoStream(t.Context(), httpReq)
	require.Error(t, err)
	require.Zero(t, fake.snapshot().submitCalls)
}
