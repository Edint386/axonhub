package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// fork: plugin onAttempt hooks (upstream #2926) skip Bailian ASR provider requests.
//
// Upstream's own openai/audio_transcriptions bodies are multipart, which the attempt hook
// skips, so upstream never shows a transcription to onAttempt. internal/server/bailianasr
// sends DashScope-native JSON under that label instead. Without this skip, a plugin scoped
// to the format would get a DashScope payload with the audio or signed URL inside, and any
// edit it returned would also replace the redacted log copy (JSONBody), so the stored
// request execution would hold the audio or the signed URL. Skipping keeps upstream's
// effective behaviour; onRequest hooks still run on the transcription's llm.Request.
func skipAttemptHooksForBailianASR(stage []pipeline.Middleware) []pipeline.Middleware {
	for i, middleware := range stage {
		stage[i] = &bailianASRAttemptHookSkip{Middleware: middleware}
	}

	return stage
}

// bailianASRAttemptHookSkip forwards every callback, including OnProcessEnd, to the
// wrapped attempt-stage middleware, and passes Bailian ASR provider requests through
// OnOutboundRawRequest untouched.
type bailianASRAttemptHookSkip struct {
	pipeline.Middleware
}

func (m *bailianASRAttemptHookSkip) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if bailianasr.IsTranscriptionRequest(request) {
		return request, nil
	}

	return m.Middleware.OnOutboundRawRequest(ctx, request)
}
