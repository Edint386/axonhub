package bailianasr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	// metaKind tags provider requests built here ("flash" or "async"); metaOptions carries
	// the JSON renderOptions.
	metaKind    = "bailian_asr"
	metaOptions = "bailian_asr_options"
	kindFlash   = "flash"
	kindAsync   = "async"
	kindMessage = "message"

	// jobMetadataKey holds the *job in TransformerMetadata, which is never persisted.
	jobMetadataKey = "bailian_asr_job"

	// pendingUploadURL stands in for input.file_urls[0] until the executor has uploaded the
	// audio, so the logged submit body never contains audio.
	pendingUploadURL = "axonhub-pending-upload"

	generationPath = "/api/v1/services/aigc/multimodal-generation/generation"
	submitPath     = "/api/v1/services/audio/asr/transcription"
	uploadsPath    = "/api/v1/uploads"
	tasksPath      = "/api/v1/tasks/"
)

// job is what the executor needs to run one async transcription or -message task.
type job struct {
	family family
	native string
	audio  AudioSource
	opts   renderOptions
	ext    string // file extension for the temp-storage object name, may be empty
	// message is how a -message task sends the audio (nil for async jobs).
	message *messageAudio

	// usage is the claim on the usage of the result this request got through the job cache; nil
	// when the result is this request's alone (audio given by URL). See billsUsage.
	usage     atomic.Pointer[usageClaim]
	usageOnce sync.Once
	billed    bool

	// reused is set when this request did not upload and submit the job itself: it got a cached
	// result, resumed a task the job cache held, or joined another request's submit (see
	// ReusedJob).
	reused atomic.Bool
}

// billsUsage reports whether this request records the usage of its result. DashScope bills a
// task once, but the job cache hands its result to every request that asks for the same job: the
// first of them to build its response (or its stream aggregate) from the result records the
// usage, every later one gets the transcript with zero usage. The answer is decided once per
// request, so a response and the aggregate of the same stream agree.
func (j *job) billsUsage() bool {
	j.usageOnce.Do(func() {
		claim := j.usage.Load()
		j.billed = claim == nil || claim.taken.CompareAndSwap(false, true)
	})

	return j.billed
}

func (o *Outbound) buildRequest(ctx context.Context, req *llm.Request, fam family) (*httpclient.Request, error) {
	opts, err := parseRenderOptions(req)
	if err != nil {
		return nil, err
	}

	src, err := audioSourceFor(ctx, req.Transcription)
	if err != nil {
		return nil, err
	}

	head, err := audioHead(src)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio: %w", err)
	}

	if fam == familyMessage {
		return o.buildMessageRequest(ctx, req, opts, src, head)
	}

	format, formatKnown := detectAudioFormat(head, sourceName(src))

	if fam.async() {
		return o.buildSubmitRequest(ctx, &job{family: fam, native: o.native, audio: src, opts: opts, ext: format.Name})
	}

	if !formatKnown {
		return nil, fmt.Errorf("%w: unsupported audio format for %s (send wav, mp3, m4a, flac, ogg, opus, webm, aac or amr)",
			transformer.ErrInvalidRequest, req.Model)
	}

	sampleRate, _ := wavSampleRate(head)

	if format.Name == "wav" {
		opts.AudioSeconds = wavSeconds(src)
	}

	return o.buildFlashRequest(ctx, req, opts, src, format, sampleRate)
}

// wavSeconds is the exact length of WAV audio from its header, or 0 when it cannot be read.
func wavSeconds(src AudioSource) float64 {
	r, err := src.Open()
	if err != nil {
		return 0
	}
	defer r.Close()

	info, err := readWAVHeader(r, src.Size())
	if err != nil || info.validate() != nil {
		return 0
	}

	return info.seconds()
}

// languageHints is family.languageHints with a warning when the client's language is dropped.
func languageHints(ctx context.Context, fam family, lang string) []string {
	hints := fam.languageHints(lang)
	if hints == nil && lang != "" && fam != familyParaformer8k {
		log.Warn(ctx, "bailian transcription ignores a language the model does not list; it detects the language itself",
			log.String("language", lang))
	}

	return hints
}

// audioHead returns the leading bytes for format sniffing; remote audio is identified by its
// URL extension instead of being downloaded.
func audioHead(src AudioSource) ([]byte, error) {
	if _, ok := src.(RemoteAudio); ok {
		return nil, nil
	}

	return readHead(src, sniffHeadSize)
}

func sourceName(src AudioSource) string {
	if remote, ok := src.(RemoteAudio); ok {
		return remote.URL()
	}

	return src.Name()
}

func parseRenderOptions(req *llm.Request) (renderOptions, error) {
	tr := req.Transcription

	format := strings.ToLower(strings.TrimSpace(tr.ResponseFormat))
	if format == "" {
		format = formatJSON
	}

	if !responseFormats[format] {
		return renderOptions{}, fmt.Errorf("%w: unsupported response_format %q", transformer.ErrInvalidRequest, tr.ResponseFormat)
	}

	if req.Stream != nil && *req.Stream && format != formatJSON && format != formatText {
		return renderOptions{}, fmt.Errorf("%w: stream=true supports response_format json or text only", transformer.ErrInvalidRequest)
	}

	words, segments := timestampGranularities(tr.Extra)

	return renderOptions{
		Model:    req.Model,
		Format:   format,
		Words:    words,
		Segments: segments,
		Language: normalizeLanguage(tr.Language),
	}, nil
}

// timestampGranularities reads timestamp_granularities[] (OpenAI's default is segment).
func timestampGranularities(extra map[string][]string) (bool, bool) {
	var words, segments bool

	for _, key := range []string{"timestamp_granularities[]", "timestamp_granularities"} {
		for _, value := range extra[key] {
			for part := range strings.SplitSeq(value, ",") {
				switch strings.ToLower(strings.TrimSpace(part)) {
				case "word":
					words = true
				case "segment":
					segments = true
				}
			}
		}
	}

	if !words && !segments {
		segments = true
	}

	return words, segments
}

type flashRequest struct {
	Model      string          `json:"model"`
	Input      flashInput      `json:"input"`
	Parameters flashParameters `json:"parameters"`
}

type flashInput struct {
	Messages []flashMessage `json:"messages"`
}

type flashMessage struct {
	Role    string         `json:"role"`
	Content []flashContent `json:"content"`
}

type flashContent struct {
	Type       string           `json:"type"`
	Text       string           `json:"text,omitempty"`
	InputAudio *flashInputAudio `json:"input_audio,omitempty"`
}

type flashInputAudio struct {
	Data string `json:"data"`
}

type flashParameters struct {
	Format                    string   `json:"format"`
	SampleRate                string   `json:"sample_rate,omitempty"`
	LanguageHints             []string `json:"language_hints,omitempty"`
	SpeakerDiarizationEnabled bool     `json:"speaker_diarization_enabled,omitempty"`
}

// buildFlashRequest builds the qwen-audio-3.1-asr-flash generation call. The audio goes inline
// as a data URI; the prompt becomes a user input_text context message before it. The model
// takes about twice the audio length to answer (live probe: 16.7 s for an 8 s clip), and with
// SSE all events still arrive at the end.
func (o *Outbound) buildFlashRequest(
	ctx context.Context,
	req *llm.Request,
	opts renderOptions,
	src AudioSource,
	format audioFormat,
	sampleRate string,
) (*httpclient.Request, error) {
	data, err := audioDataURI(src, format)
	if err != nil {
		return nil, err
	}

	body := flashRequest{
		Model: req.Model,
		Parameters: flashParameters{
			Format:                    format.Name,
			SampleRate:                sampleRate,
			LanguageHints:             languageHints(ctx, familyFlash, opts.Language),
			SpeakerDiarizationEnabled: opts.Format == formatDiarizedJSON,
		},
	}

	if prompt := strings.TrimSpace(req.Transcription.Prompt); prompt != "" {
		body.Input.Messages = append(body.Input.Messages, flashMessage{
			Role:    "user",
			Content: []flashContent{{Type: "input_text", Text: prompt}},
		})
	}

	body.Input.Messages = append(body.Input.Messages, flashMessage{
		Role:    "user",
		Content: []flashContent{{Type: "input_audio", InputAudio: &flashInputAudio{Data: data}}},
	})

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal qwen-audio-asr-flash request: %w", err)
	}

	// The persisted copy names the audio instead of carrying megabytes of base64.
	body.Input.Messages[len(body.Input.Messages)-1].Content[0].InputAudio.Data = audioPlaceholder(src, format.Name)

	logged, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal qwen-audio-asr-flash request: %w", err)
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")

	if req.Stream != nil && *req.Stream {
		headers.Set("Accept", "text/event-stream")
		headers.Set("X-DashScope-SSE", "enable")
	} else {
		headers.Set("Accept", "application/json")
	}

	return o.providerRequest(ctx, kindFlash, o.native+generationPath, headers, raw, logged, opts, nil)
}

// flashInlineMaxBytes bounds the audio inlined as base64 into one flash call (the model takes at
// most 5 minutes of audio). Only a large upload mapped to the flash model can exceed it, and it
// must not be read into memory.
const flashInlineMaxBytes = 26 << 20

func audioDataURI(src AudioSource, format audioFormat) (string, error) {
	if remote, ok := src.(RemoteAudio); ok {
		return remote.URL(), nil
	}

	if src.Size() > flashInlineMaxBytes {
		return "", fmt.Errorf("%w: file too large for qwen-audio-3.1-asr-flash (%d bytes, at most %d MiB are sent inline); "+
			"use qwen-audio-3.1-asr-flash-filetrans for long audio", transformer.ErrInvalidRequest, src.Size(), flashInlineMaxBytes>>20)
	}

	data, err := readAll(src)
	if err != nil {
		return "", fmt.Errorf("failed to read audio: %w", err)
	}

	return "data:" + format.MIME + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func audioPlaceholder(src AudioSource, format string) string {
	if remote, ok := src.(RemoteAudio); ok {
		return RedactURL(remote.URL())
	}

	return fmt.Sprintf("<audio bytes: %d, format: %s>", src.Size(), format)
}

type submitRequest struct {
	Model      string         `json:"model"`
	Input      submitInput    `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

type submitInput struct {
	FileURLs []string `json:"file_urls"`
}

// buildSubmitRequest builds the async task submission. The executor uploads the audio and
// swaps the real oss:// (or remote) URL into input.file_urls before sending it.
func (o *Outbound) buildSubmitRequest(ctx context.Context, j *job) (*httpclient.Request, error) {
	params := map[string]any{} // qwen filetrans fails without a parameters object
	if hints := languageHints(ctx, j.family, j.opts.Language); hints != nil {
		params["language_hints"] = hints
	}

	if j.opts.Format == formatDiarizedJSON {
		params[j.family.diarizationParam()] = true
	}

	raw, err := json.Marshal(submitRequest{
		Model:      j.opts.Model,
		Input:      submitInput{FileURLs: []string{pendingUploadURL}},
		Parameters: params,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal bailian transcription task: %w", err)
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("X-DashScope-Async", "enable")

	return o.providerRequest(ctx, kindAsync, o.native+submitPath, headers, raw, raw, j.opts, j)
}

func (o *Outbound) providerRequest(
	ctx context.Context,
	kind, url string,
	headers http.Header,
	body, logged []byte,
	opts renderOptions,
	j *job,
) (*httpclient.Request, error) {
	encodedOpts, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to encode transcription options: %w", err)
	}

	req := &httpclient.Request{
		Method:      http.MethodPost,
		URL:         url,
		Headers:     headers,
		ContentType: "application/json",
		Body:        body,
		JSONBody:    logged,
		Auth: &httpclient.AuthConfig{
			Type:   httpclient.AuthTypeBearer,
			APIKey: o.cfg.APIKeyProvider.Get(ctx),
		},
		RequestType:           string(llm.RequestTypeTranscription),
		APIFormat:             string(llm.APIFormatOpenAITranscription),
		SkipInboundQueryMerge: true,
		Metadata:              map[string]string{metaKind: kind, metaOptions: string(encodedOpts)},
	}

	// Every kind runs whole inside one attempt and is billed once Bailian has the audio: an async
	// job, a -message task, and the flash call too, which answers after about twice the audio
	// length (all SSE events come at the end). The pipeline's per-attempt response timeouts would
	// cut it off and send the audio to the next channel, paying again; only the request context
	// bounds it (see jobExecutor.doFlash for a flash call cut off once its audio was sent).
	req.Metadata[pipeline.MetadataLongRunningJob] = "true"

	if j != nil {
		req.TransformerMetadata = map[string]any{jobMetadataKey: j}
	}

	return req, nil
}
