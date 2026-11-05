package bailian

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	qaCommit     = `{"type":"input_audio_buffer.commit"}`
	qaTaskFailed = `{"header":{"task_id":"0123456789abcdef0123456789abcdef","event":"task-failed","error_code":"ResponseTimeout","error_message":"Response timeout!","attributes":{}},"payload":{}}`
)

// -streaming bills the audio the gateway forwarded upstream, never audio it received once
// teardown began (the session drops that audio instead of sending it).
func TestQwenAudioStreamingBillsOnlyForwardedAudio(t *testing.T) {
	t.Run("audio after the goodbye is neither forwarded nor billed", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1000)
		require.Equal(t, "finish-task", single(t, d.CloseUpstream()).Get("header.action").String())

		// The client keeps streaming during the finish drain of a policy, cap or shutdown end.
		for range 5 {
			out := client(t, d, audioMs(1000))
			require.Empty(t, out.ToUpstream)
			require.Empty(t, out.ToClient)
		}

		require.Empty(t, client(t, d, qaCommit).ToUpstream)

		done := upstream(t, d, taskFinishedLive)
		require.True(t, done.UpstreamFinished)
		require.InDelta(t, 1, single1(t, done.Usage).DurationSeconds, 1e-9, "1 s forwarded, not 6 s received")
		require.Empty(t, d.FinalUsage(), "billed once")
	})

	t.Run("audio held for task-started is dropped when the goodbye went first", func(t *testing.T) {
		// A 30 s file sent as appends plus a commit, and a close within the run-task round
		// trip: the first append sends run-task and closes the gate, the rest is queued.
		d := newTestParaformer(t, qaStreaming)
		run := single(t, client(t, d, audioMs(1000)).ToUpstream)
		require.Equal(t, "run-task", run.Get("header.action").String())
		require.Equal(t, "finish-task", single(t, d.CloseUpstream()).Get("header.action").String())

		started := upstream(t, d, taskStarted)
		require.True(t, started.OpenGate)
		require.Empty(t, started.ToUpstream, "the held audio is never sent")

		// The session replays the queued frames when the gate opens.
		for range 30 {
			require.Empty(t, client(t, d, audioMs(1000)).ToUpstream)
		}

		require.Empty(t, client(t, d, qaCommit).ToUpstream)

		done := upstream(t, d, taskFinishedLive)
		require.True(t, done.UpstreamFinished)
		require.Empty(t, done.ToClient, "the replayed commit was never sent, so nothing answers it")
		require.Empty(t, done.Usage, "no audio reached the upstream")
		require.Empty(t, d.FinalUsage())
	})

	t.Run("an append the session drops is handed back", func(t *testing.T) {
		// The session started ending just before teardown's goodbye: it drops this append.
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1000)

		out := client(t, d, audioMs(2000))
		require.Len(t, out.ToUpstream, 1)
		require.NotNil(t, out.Discarded)
		out.Discarded(0)

		_ = d.CloseUpstream()
		require.InDelta(t, 1, single1(t, upstream(t, d, taskFinishedLive).Usage).DurationSeconds, 1e-9)
	})

	t.Run("held audio the upstream writer only partly took bills the part it took", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = client(t, d, audioMs(1000)) // run-task; the audio is held
		_ = client(t, d, audioMs(1000))
		_ = client(t, d, audioMs(1000))

		started := upstream(t, d, taskStarted)
		require.Len(t, started.ToUpstream, 3)
		require.NotNil(t, started.Discarded)
		started.Discarded(1)

		require.InDelta(t, 1, single1(t, d.FinalUsage()).DurationSeconds, 1e-9)
	})

	t.Run("audio forwarded before the goodbye is still billed in full", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1000)
		require.Len(t, client(t, d, audioMs(1500)).ToUpstream, 1)

		_ = d.CloseUpstream()
		require.InDelta(t, 3, single1(t, upstream(t, d, taskFinishedLive).Usage).DurationSeconds, 1e-9)
	})
}

// The -streaming remainder is billed exactly once on every ending, also when no task-finished
// follows (task-failed, a lost upstream, a commit or goodbye whose task-finished never came).
func TestQwenAudioStreamingBillsTheRemainderOnEveryEnding(t *testing.T) {
	t.Run("task-failed without any final", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1000)
		_ = client(t, d, audioMs(59000))

		out := upstream(t, d, qaTaskFailed)
		require.Equal(t, 1011, out.Close.Code)
		require.Empty(t, out.Usage)

		// Teardown still sends finish-task, but no task-finished follows a failed task.
		_ = d.CloseUpstream()

		rec := single1(t, d.FinalUsage())
		require.InDelta(t, 60, rec.DurationSeconds, 1e-9)
		require.True(t, strings.HasPrefix(rec.ItemID, "task_"), rec.ItemID)
		require.Equal(t, "qwen_audio_task_tail", gjson.GetBytes(rec.Summary, "type").String())
		require.Empty(t, d.FinalUsage(), "billed once")
	})

	t.Run("task-failed after a final bills only the rest", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 8400)
		require.InDelta(t, 8, single1(t, upstream(t, d, qaResult(t, 1, 160, 7400, "好。", true, false, "")).Usage).DurationSeconds, 1e-9)

		_ = client(t, d, audioMs(30000))
		_ = upstream(t, d, qaTaskFailed)
		_ = d.CloseUpstream()

		rec := single1(t, d.FinalUsage())
		require.Equal(t, "item_axh_1", rec.ItemID)
		require.InDelta(t, 31, rec.DurationSeconds, 1e-9, "38.4 s forwarded: 39 s in all")
	})

	t.Run("a commit whose task-finished never came", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 2500)
		require.Equal(t, "finish-task", single(t, client(t, d, qaCommit).ToUpstream).Get("header.action").String())

		// The gate timeout ends the session; teardown waits for the commit's task-finished in vain.
		require.Empty(t, d.CloseUpstream())
		require.True(t, d.FinishDrain())

		require.InDelta(t, 3, single1(t, d.FinalUsage()).DurationSeconds, 1e-9)
	})

	t.Run("a lost upstream", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 4200)
		require.InDelta(t, 5, single1(t, d.FinalUsage()).DurationSeconds, 1e-9)
	})

	t.Run("nothing after a task tail that billed everything", func(t *testing.T) {
		d := newTestParaformer(t, qaStreaming)
		_ = startTask(t, d, 1500)
		_ = d.CloseUpstream()
		require.InDelta(t, 2, single1(t, upstream(t, d, taskFinishedLive).Usage).DurationSeconds, 1e-9)
		require.Empty(t, d.FinalUsage())
	})

	t.Run("nothing without audio", func(t *testing.T) {
		require.Empty(t, newTestParaformer(t, qaStreaming).FinalUsage())
	})

	t.Run("message and paraformer bill what the upstream reports", func(t *testing.T) {
		for _, model := range []string{qaMessage, paraformer} {
			d := newTestParaformer(t, model)
			_ = startTask(t, d, 5000)
			_ = upstream(t, d, qaTaskFailed)
			require.Empty(t, d.FinalUsage(), model)
		}
	})
}

// The qwen-audio profiles send heartbeat:true so an open mic's silence does not time the task
// out; the heartbeat results that keeps coming are dropped: no event, no usage, no commit point.
func TestQwenAudioHeartbeatResultsAreDropped(t *testing.T) {
	heartbeats := []string{
		`{"header":{"event":"result-generated","attributes":{}},"payload":{"output":{"sentence":{"sentence_id":1,"begin_time":0,"end_time":null,"text":"","heartbeat":true,"sentence_end":false}}}}`,
		`{"header":{"event":"result-generated","attributes":{}},"payload":{"output":{"sentence":{"sentence_id":1,"begin_time":0,"end_time":2950,"text":"","heartbeat":true,"sentence_end":true,"words":[]}},"usage":{"duration":3,"input_tokens":40,"output_tokens":0,"total_tokens":40}}}`,
	}

	for _, model := range []string{qaStreaming, qaMessage} {
		t.Run(model, func(t *testing.T) {
			d := newTestParaformer(t, model)
			run := startTask(t, d, 3000)
			require.True(t, run.Get("payload.parameters.heartbeat").Bool())

			for _, hb := range heartbeats {
				out := upstream(t, d, hb)
				require.Empty(t, out.ToClient)
				require.Empty(t, out.ToUpstream)
				require.Empty(t, out.Usage)
				require.Nil(t, out.Close)
			}

			// A heartbeat is no final: the whole buffer is still uncommitted.
			require.Equal(t, "finish-task", single(t, client(t, d, qaCommit).ToUpstream).Get("header.action").String())
		})
	}

	// Paraformer's run-task stays as it was (TestParaformerRunTaskIsByteIdentical pins its bytes).
	require.False(t, startTask(t, newTestParaformer(t, paraformer), 100).Get("payload.parameters.heartbeat").Exists())
}
