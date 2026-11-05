// Package bailianasr serves OpenAI POST /v1/audio/transcriptions on Bailian (DashScope)
// channels for the file-transcription models that compatible-mode does not expose:
//
//   - qwen-audio-3.1-asr-flash: one synchronous multimodal-generation call (SSE for stream=true);
//   - qwen-audio-3.1-asr-flash-filetrans, paraformer-v2, paraformer-8k-v2: asynchronous file jobs
//     (temp-storage upload, submit, poll, download the result file);
//   - qwen-audio-3.1-asr-flash-message: one task on the inference WebSocket (run-task, the audio
//     as 16 kHz PCM or the compressed file in 100 ms frames, finish-task), run in the executor.
//
// Wrap puts the package in front of a channel outbound. It builds the DashScope requests for
// those models only and hands every other request to the wrapped outbound unchanged. The async
// jobs run inside the executor returned by CustomizeExecutor, so one pipeline attempt is one
// whole job, and a job cache keeps pipeline retries from submitting the same audio twice.
//
// Audio normally comes from the multipart file; WithAudioSource lets the HTTP handler supply a
// file spooled to disk (uploads above the upstream caps) or a URL DashScope downloads itself
// (internal/server/api/bailian_transcription_upload.go).
//
// custom: fork-only package, wired into upstream code by two "// fork:" hooks in
// internal/server/biz/channel_llm.go (see internal/server/biz/channel_bailian_asr.go) and one in
// internal/server/api/openai.go CreateTranscription (see CUSTOM_BRANCH_RULES.md section 5).
package bailianasr
