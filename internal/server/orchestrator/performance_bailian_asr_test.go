package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
)

// TestPerformanceRecording_BailianTranscriptionsAreExcludedFromLatency guards the fork line in
// performance.go OnOutboundRawRequest:
//
//	perf.LatencyExcluded = excludesLatency(request) // fork: see performance_bailian_asr.go
//
// Without it a minutes-long Bailian transcription job pushes the latency average of the channel
// it shares with Bailian chat far up, and chat moves to other channels. Listed in
// CUSTOM_BRANCH_RULES.md section 5; biz TestRecordPerformance_LatencyExcludedSuccessKeepsTheLatencyScores
// guards the other half.
func TestPerformanceRecording_BailianTranscriptionsAreExcludedFromLatency(t *testing.T) {
	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: &ent.Channel{ID: 3, Name: "bailian"}, Outbound: &mockTransformer{}},
		},
	}
	middleware := &performanceRecording{outbound: &PersistentOutboundTransformer{state: state}}

	asr := bailianasr.Wrap(nil, bailianasr.Config{
		BaseURL:        "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyProvider: auth.NewStaticKeyProvider("sk-test"),
	})

	for _, model := range []string{"paraformer-v2", "qwen-audio-3.1-asr-flash", "qwen-audio-3.1-asr-flash-filetrans"} {
		providerReq, err := asr.TransformRequest(context.Background(), &llm.Request{
			Model:         model,
			Stream:        lo.ToPtr(false),
			RequestType:   llm.RequestTypeTranscription,
			APIFormat:     llm.APIFormatOpenAITranscription,
			Transcription: &llm.TranscriptionRequest{File: wavForPerformanceTest(), FileName: "a.wav"},
		})
		require.NoError(t, err, model)

		_, err = middleware.OnOutboundRawRequest(context.Background(), providerReq)
		require.NoError(t, err)
		require.True(t, state.Perf.LatencyExcluded, "%s must not move the channel's latency scores", model)
	}

	_, err := middleware.OnOutboundRawRequest(context.Background(), &httpclient.Request{
		Method: "POST",
		URL:    "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
	})
	require.NoError(t, err)
	require.False(t, state.Perf.LatencyExcluded, "chat keeps feeding the latency scores")
}

// TestPerformanceRecording_BailianTranscriptionFailuresKeepTheirMeaning guards the fork line in
// performance.go OnOutboundRawError:
//
//	} else if !markTranscriptionFailure(perf, err) { // fork: see performance_bailian_asr.go
//
// Without it every bailianasr error (an *llm.ResponseError) is recorded as a 500 failure of the
// channel: a user's undecodable file raises the consecutive failures of the channel Bailian chat
// shares and feeds auto-disable rules. Listed in CUSTOM_BRANCH_RULES.md section 5.
func TestPerformanceRecording_BailianTranscriptionFailuresKeepTheirMeaning(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	state := &PersistenceState{
		ChannelService: biz.NewChannelServiceForTest(client),
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: &ent.Channel{ID: 3, Name: "bailian"}, Outbound: &mockTransformer{}},
		},
	}
	middleware := &performanceRecording{outbound: &PersistentOutboundTransformer{state: state}}

	asr := bailianasr.Wrap(nil, bailianasr.Config{
		BaseURL:        "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyProvider: auth.NewStaticKeyProvider("sk-test"),
	})

	transcription, err := asr.TransformRequest(context.Background(), &llm.Request{
		Model:         "paraformer-v2",
		Stream:        lo.ToPtr(false),
		RequestType:   llm.RequestTypeTranscription,
		APIFormat:     llm.APIFormatOpenAITranscription,
		Transcription: &llm.TranscriptionRequest{File: wavForPerformanceTest(), FileName: "a.wav"},
	})
	require.NoError(t, err)

	chat := &httpclient.Request{Method: "POST", URL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"}

	record := func(request *httpclient.Request, err error) *biz.PerformanceRecord {
		t.Helper()

		_, reqErr := middleware.OnOutboundRawRequest(context.Background(), request)
		require.NoError(t, reqErr)
		middleware.OnOutboundRawError(context.Background(), err)

		return state.Perf
	}

	// Failures that say nothing about the channel count as neither a success nor a failure.
	for _, neutral := range []error{
		&llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Code: "DECODE_ERROR", Message: "bad audio"}},
		&llm.ResponseError{StatusCode: http.StatusRequestEntityTooLarge, Detail: llm.ErrorDetail{Code: "FILE_TOO_LARGE"}},
		&httpclient.Error{StatusCode: http.StatusBadRequest, Body: []byte(`{"code":"InvalidParameter","message":"bad format"}`)},
		&llm.ResponseError{
			StatusCode: http.StatusGatewayTimeout,
			Detail:     llm.ErrorDetail{Code: "bailian_task_unfinished"},
			Cause:      errors.Join(context.DeadlineExceeded, errors.New("no retry")),
		},
	} {
		perf := record(transcription, neutral)
		require.True(t, perf.Canceled, "%v must not count against the channel", neutral)
		require.False(t, perf.Success)
		require.True(t, perf.RequestCompleted)
	}

	// Real failures still count, with the status the client got instead of a blanket 500.
	for _, tc := range []struct {
		err    error
		status int
	}{
		{&llm.ResponseError{StatusCode: http.StatusFailedDependency, Detail: llm.ErrorDetail{Code: "InternalError.Algo", Message: "try again"}}, http.StatusFailedDependency},
		{&llm.ResponseError{StatusCode: http.StatusForbidden, Detail: llm.ErrorDetail{Code: "Arrearage", Message: "overdue"}}, http.StatusForbidden},
		{&llm.ResponseError{StatusCode: http.StatusGatewayTimeout, Detail: llm.ErrorDetail{Code: "bailian_start_timeout"}}, http.StatusGatewayTimeout},
		{&httpclient.Error{StatusCode: http.StatusUnauthorized, Body: []byte(`{"code":"InvalidApiKey","message":"Invalid API-key provided."}`)}, http.StatusUnauthorized},
		{errors.New("connection reset by peer"), http.StatusInternalServerError},
	} {
		perf := record(transcription, tc.err)
		require.False(t, perf.Canceled, "%v is a failure of the channel", tc.err)
		require.False(t, perf.Success)
		require.Equal(t, tc.status, perf.ResponseStatusCode, "%v", tc.err)
		require.NotEmpty(t, perf.ErrorMessage, "keyword auto-disable rules still see the message")
	}

	// Every other request keeps the upstream recording exactly.
	perf := record(chat, &llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Message: "bad"}})
	require.False(t, perf.Canceled)
	require.Equal(t, http.StatusInternalServerError, perf.ResponseStatusCode)

	perf = record(transcription, context.Canceled)
	require.True(t, perf.Canceled, "a cancellation stays a cancellation")
}

// TestPerformanceRecording_ReusedTranscriptionSuccessIsNeutral guards the two fork lines right
// after MarkSuccess, in performance.go OnOutboundLlmResponse (non-stream) and in outbound.go
// OutboundPersistentStream.markPerformanceTerminal (stream):
//
//	markTranscriptionSuccess(..., ...) // fork: see performance_bailian_asr.go
//
// Without them a Bailian transcription answered from the job cache, or by another request's
// submit it joined, counts as a success of the channel and key the attempt was given although
// none of their credentials did the work: it clears that key's consecutive failures and
// auto-disable counts even when the key is revoked. Listed in CUSTOM_BRANCH_RULES.md section 5.
func TestPerformanceRecording_ReusedTranscriptionSuccessIsNeutral(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	reused := false
	previous := reusedTranscriptionJob
	reusedTranscriptionJob = func(*httpclient.Request) bool { return reused }

	t.Cleanup(func() { reusedTranscriptionJob = previous })

	state := &PersistenceState{
		ChannelService: biz.NewChannelServiceForTest(client),
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: &ent.Channel{ID: 3, Name: "bailian"}, Outbound: &mockTransformer{}},
		},
	}
	middleware := &performanceRecording{outbound: &PersistentOutboundTransformer{state: state}}

	asr := bailianasr.Wrap(nil, bailianasr.Config{
		BaseURL:        "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyProvider: auth.NewStaticKeyProvider("sk-test"),
	})

	transcription, err := asr.TransformRequest(context.Background(), &llm.Request{
		Model:         "paraformer-v2",
		Stream:        lo.ToPtr(false),
		RequestType:   llm.RequestTypeTranscription,
		APIFormat:     llm.APIFormatOpenAITranscription,
		Transcription: &llm.TranscriptionRequest{File: wavForPerformanceTest(), FileName: "a.wav"},
	})
	require.NoError(t, err)

	chat := &httpclient.Request{Method: "POST", URL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"}

	nonStream := func(request *httpclient.Request) *biz.PerformanceRecord {
		t.Helper()

		_, err := middleware.OnOutboundRawRequest(context.Background(), request)
		require.NoError(t, err)

		state.RawProviderRequest = request
		_, err = middleware.OnOutboundLlmResponse(context.Background(), &llm.Response{})
		require.NoError(t, err)

		return state.Perf
	}

	stream := func(request *httpclient.Request) *biz.PerformanceRecord {
		t.Helper()

		_, err := middleware.OnOutboundRawRequest(context.Background(), request)
		require.NoError(t, err)

		state.RawProviderRequest = request
		ts := &OutboundPersistentStream{perf: state.Perf, state: state}
		ts.markPerformanceTerminal(streamTerminalCompleted, "")

		return state.Perf
	}

	for name, record := range map[string]func(*httpclient.Request) *biz.PerformanceRecord{"non-stream": nonStream, "stream": stream} {
		reused = true

		perf := record(transcription)
		require.True(t, perf.Canceled, "%s: a reused job's success is not the channel's", name)
		require.False(t, perf.Success, name)
		require.True(t, perf.RequestCompleted, name)

		perf = record(chat)
		require.True(t, perf.Success, "%s: every other request keeps the upstream recording", name)
		require.False(t, perf.Canceled, name)

		reused = false

		perf = record(transcription)
		require.True(t, perf.Success, "%s: a job the attempt submitted itself is a success", name)
		require.False(t, perf.Canceled, name)
	}
}

// wavForPerformanceTest is a 16 kHz mono PCM16 WAV of 0.1 s silence.
func wavForPerformanceTest() []byte {
	const samples = 1600

	data := make([]byte, 44+samples*2)
	copy(data[0:], "RIFF")
	putLE32(data[4:], 36+samples*2)
	copy(data[8:], "WAVEfmt ")
	putLE32(data[16:], 16)
	data[20], data[22] = 1, 1
	putLE32(data[24:], 16000)
	putLE32(data[28:], 32000)
	data[32], data[34] = 2, 16
	copy(data[36:], "data")
	putLE32(data[40:], samples*2)

	return data
}

func putLE32(b []byte, v int) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
