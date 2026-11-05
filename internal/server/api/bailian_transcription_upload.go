package api

// custom: the request side of Bailian file transcription (internal/server/bailianasr) on
// POST /v1/audio/transcriptions. CreateTranscription calls serveBailianTranscription first
// through one "// fork:" hook (guarded by bailian_transcription_upload_test.go and listed in
// CUSTOM_BRANCH_RULES.md section 5). Everything that is not a Bailian request goes on to the
// upstream handler exactly as the client sent it.
//
// It adds three things the upstream handler cannot do:
//   - Large files for the async Bailian models (qwen-audio-3.1-asr-flash-filetrans, paraformer-v2,
//     paraformer-8k-v2). Upstream reads every request into memory and caps a file at 26 MiB, a
//     multipart body at 50 MiB and any body at 64 MiB. Here a body above 26 MiB is parsed as a
//     stream, the file is spooled to disk while it is hashed, and the request continues with a
//     small multipart body (same fields, a placeholder file) while bailianasr reads the audio
//     from the spooled file and streams it to Bailian's temporary storage. At most
//     AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY (default 2) such uploads run at once, and at most
//     AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY (default 1) per API key; an upload holds its slot from the
//     moment its file passes 26 MiB until the audio reached Bailian's temporary storage (not
//     while the job runs). Each file may have up to AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB (default
//     1024, Bailian's per-file cap), spooled under AXONHUB_BAILIAN_ASR_SPOOL_DIR (default the
//     system temp dir; it must be on disk, not tmpfs); all spool files together hold at most
//     AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB (default 4096), beyond which an upload gets a 429. The
//     API key's model list (its active profile) is checked before a slot is taken. A spool file is
//     unlinked as soon as it is created, so its space comes back when the request ends, and also
//     when the process is restarted or killed mid-job; where the OS cannot unlink an open file
//     (Windows) it is removed when the request ends, and files a killed process left behind are
//     swept by the next large upload (see sweepSpool). Once a file is being spooled, a client that
//     stops sending is cut off after AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS (default 120) without
//     data, one that sends less than 1 MiB within 60 s of waiting is cut off as too slow, and the
//     request deadline cuts it off as well (see bodyWatch).
//   - A "url" (Groq style) or "file_url" form field instead of the file, for the Bailian models
//     that take a URL: Bailian downloads the http(s) URL itself.
//   - stream=true for the async models and the -message model: their stream commits as soon as
//     Bailian has accepted the work, so SSE keep-alive comments are forced on for the wait even
//     when server.sse_keep_alive is off.
//
// Models are matched by the name the client sends (the bailianasr allowlist, surrounding spaces
// ignored as upstream does); a model alias is not resolved here, so an alias only gets the
// upstream limits. A file of at most 26 MiB always reaches the upstream handler as sent (rebuilt
// when its body was parsed here), so a request that a channel or key mapping sends to another
// model gets its real audio; only a file above that cap travels out of band (a placeholder in the
// body), and such a request can only be served by a Bailian large-upload model.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	// upstreamAudioFileCap is the upstream per-file (and per-field) cap of the transcription
	// multipart parser (llm/transformer/openai/audio_inbound.go maxAudioFileSize).
	upstreamAudioFileCap = 26 << 20

	// bailianUploadFieldCap and bailianUploadFieldsCap bound the form fields of a large upload,
	// which are kept in memory (they are a model name and a few options).
	bailianUploadFieldCap  = 1 << 20
	bailianUploadFieldsCap = 4 << 20

	defaultBailianUploadMaxMB       = 1024
	defaultBailianUploadConcurrency = 2
	defaultBailianUploadPerKey      = 1
	defaultBailianSpoolBudgetMB     = 4096

	// bailianUploadMinRateBytes and bailianUploadMinRateWindow are the slowest a spooling upload
	// may arrive: every MiB within 60 s of waiting for the client (see bodyWatch).
	bailianUploadMinRateBytes  = 1 << 20
	bailianUploadMinRateWindow = 60 * time.Second

	// defaultBailianUploadQueueWait is how long a large upload waits for a free slot before a
	// 429. It stays below the 60 s a reverse proxy usually allows between two body writes.
	defaultBailianUploadQueueWait = 30 * time.Second

	defaultBailianKeepAliveInterval = 15 * time.Second

	// defaultBailianUploadBodyIdle is how long a large upload that holds a slot or a spool file
	// waits for the next body bytes. It stays above the 60 s a reverse proxy usually allows
	// between two body writes, and above queueWait, during which the body is not read.
	defaultBailianUploadBodyIdle = 2 * time.Minute

	// bailianPlaceholderAudio is the content of the file part the upstream handler sees when the
	// real audio travels out of band. A channel other than Bailian would receive these bytes and
	// reject them as audio.
	bailianPlaceholderAudio = "axonhub: the audio of this request is read by the Bailian adapter, not from this part\n"
)

type bailianUploadSettings struct {
	maxBytes  int64
	spoolDir  string
	slots     chan struct{}
	queueWait time.Duration
	bodyIdle  time.Duration

	// perKey caps the slots one API key holds at once (0: no cap).
	perKey   int
	keyMu    sync.Mutex
	keySlots map[int]*keySlots

	// budget bounds the bytes of all large-upload spool files (those past the upstream cap)
	// together; see spoolWriter.charge.
	budget *spoolBudget

	// minRateBytes must arrive within minRateWindow of waiting for the client once a request
	// spools (minRateWindow 0: no such rule).
	minRateBytes  int64
	minRateWindow time.Duration

	sweepMu   sync.Mutex
	lastSweep time.Time
}

// keySlots are the large-upload slots of one API key; refs counts the requests holding or
// waiting for one, so the entry goes once nobody needs it.
type keySlots struct {
	ch   chan struct{}
	refs int
}

// spoolBudget bounds the disk space the large-upload spool files take together (see
// spoolWriter.charge).
type spoolBudget struct {
	limit int64

	mu   sync.Mutex
	used int64
}

// reserve takes n bytes of the budget; false when they do not fit.
func (b *spoolBudget) reserve(n int64) bool {
	if b == nil {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.used+n > b.limit {
		return false
	}

	b.used += n

	return true
}

func (b *spoolBudget) release(n int64) {
	if b == nil || n <= 0 {
		return
	}

	b.mu.Lock()
	b.used = max(b.used-n, 0)
	b.mu.Unlock()
}

func (b *spoolBudget) inUse() int64 {
	if b == nil {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.used
}

const (
	// spoolFilePrefix names every spool file, so a sweep only ever removes files of this hook.
	spoolFilePrefix = "axonhub-bailian-asr-"

	// spoolStaleAfter is how long after its last write a spool file is taken as left behind
	// (when the current request's deadline is further away, that is used instead). A spool
	// file only lives as long as its request.
	spoolStaleAfter = 2 * time.Hour

	// spoolSweepInterval spaces the sweeps of one process.
	spoolSweepInterval = time.Hour
)

// sweepSpool removes spool files a previous process left behind: a restart or crash ends a
// request without its deferred cleanup, and nothing else would ever remove its file. It runs
// at the first spool of the process and then at most once per spoolSweepInterval, and removes
// only regular files named spoolFilePrefix* that were last written longer ago than any request
// can live.
func (s *bailianUploadSettings) sweepSpool(ctx context.Context) {
	now := time.Now()

	s.sweepMu.Lock()
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < spoolSweepInterval {
		s.sweepMu.Unlock()
		return
	}

	s.lastSweep = now
	s.sweepMu.Unlock()

	staleAfter := spoolStaleAfter
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			staleAfter = max(staleAfter, time.Until(deadline)+10*time.Minute)
		}
	}

	entries, err := os.ReadDir(s.spoolDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), spoolFilePrefix) {
			continue
		}

		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > staleAfter {
			_ = os.Remove(filepath.Join(s.spoolDir, entry.Name()))
		}
	}
}

var (
	bailianUploadMu      sync.Mutex
	bailianUploadCurrent *bailianUploadSettings
)

// currentBailianUploadSettings loads the env settings on first use; the slots are process-wide.
func currentBailianUploadSettings() *bailianUploadSettings {
	bailianUploadMu.Lock()
	defer bailianUploadMu.Unlock()

	if bailianUploadCurrent == nil {
		bailianUploadCurrent = loadBailianUploadSettings()
	}

	return bailianUploadCurrent
}

func loadBailianUploadSettings() *bailianUploadSettings {
	maxMB := envPositiveInt("AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB", defaultBailianUploadMaxMB)
	concurrency := envPositiveInt("AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY", defaultBailianUploadConcurrency)

	settings := newBailianUploadSettings(int64(maxMB)<<20, concurrency, strings.TrimSpace(os.Getenv("AXONHUB_BAILIAN_ASR_SPOOL_DIR")))
	settings.bodyIdle = time.Duration(envPositiveInt("AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS",
		int(defaultBailianUploadBodyIdle/time.Second))) * time.Second
	settings.perKey = envPositiveInt("AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY", defaultBailianUploadPerKey)
	settings.budget = &spoolBudget{limit: int64(envPositiveInt("AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB", defaultBailianSpoolBudgetMB)) << 20}

	return settings
}

func newBailianUploadSettings(maxBytes int64, concurrency int, spoolDir string) *bailianUploadSettings {
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}

	return &bailianUploadSettings{
		maxBytes:      maxBytes,
		spoolDir:      spoolDir,
		slots:         make(chan struct{}, concurrency),
		queueWait:     defaultBailianUploadQueueWait,
		bodyIdle:      defaultBailianUploadBodyIdle,
		perKey:        defaultBailianUploadPerKey,
		budget:        &spoolBudget{limit: defaultBailianSpoolBudgetMB << 20},
		minRateBytes:  bailianUploadMinRateBytes,
		minRateWindow: bailianUploadMinRateWindow,
	}
}

func envPositiveInt(name string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return v
	}

	return fallback
}

var (
	errBailianUploadBusy    = errors.New("too many large uploads in progress")
	errBailianUploadKeyBusy = errors.New("too many large uploads in progress for this API key")
)

// acquire takes one large-upload slot: one of the calling API key's perKey slots first (a request
// without an API key has no such cap), then one of the process-wide slots, waiting up to
// queueWait in all (not at all with noWait). The returned release frees both and is safe to call
// more than once, also concurrently.
func (s *bailianUploadSettings) acquire(ctx context.Context, noWait bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var deadline <-chan time.Time

	if !noWait {
		timer := time.NewTimer(s.queueWait)
		defer timer.Stop()

		deadline = timer.C
	}

	releaseKey := func() {}

	if id, ok := uploadAPIKeyID(ctx); ok && s.perKey > 0 {
		key := s.keySlotsFor(id)

		if err := takeSlot(ctx, key.ch, noWait, deadline); err != nil {
			s.dropKeySlots(id, key)

			if errors.Is(err, errBailianUploadBusy) {
				err = errBailianUploadKeyBusy
			}

			return nil, err
		}

		releaseKey = func() {
			<-key.ch
			s.dropKeySlots(id, key)
		}
	}

	if err := takeSlot(ctx, s.slots, noWait, deadline); err != nil {
		releaseKey()
		return nil, err
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			<-s.slots
			releaseKey()
		})
	}, nil
}

func takeSlot(ctx context.Context, slots chan struct{}, noWait bool, deadline <-chan time.Time) error {
	if noWait {
		select {
		case slots <- struct{}{}:
			return nil
		default:
			return errBailianUploadBusy
		}
	}

	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline:
		return errBailianUploadBusy
	}
}

func (s *bailianUploadSettings) keySlotsFor(id int) *keySlots {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	if s.keySlots == nil {
		s.keySlots = map[int]*keySlots{}
	}

	key := s.keySlots[id]
	if key == nil {
		key = &keySlots{ch: make(chan struct{}, s.perKey)}
		s.keySlots[id] = key
	}

	key.refs++

	return key
}

func (s *bailianUploadSettings) dropKeySlots(id int, key *keySlots) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	if key.refs--; key.refs <= 0 && s.keySlots[id] == key {
		delete(s.keySlots, id)
	}
}

// uploadAPIKeyID is the API key the request authenticated with (every /v1 route requires one).
func uploadAPIKeyID(ctx context.Context) (int, bool) {
	key, ok := contexts.GetAPIKey(ctx)
	if !ok || key == nil {
		return 0, false
	}

	return key.ID, true
}

// checkUploadModelAccess applies the API key's model list (its active profile) before the hook
// spends an upload slot or disk on a request the orchestrator would refuse anyway; the error is
// the one the orchestrator gives (checkApiKeyModelAccess), which the client gets as a 422.
func checkUploadModelAccess(ctx context.Context, model string) error {
	if ctx == nil {
		return nil
	}

	key, ok := contexts.GetAPIKey(ctx)
	if !ok || key == nil {
		return nil
	}

	profile := key.GetActiveProfile()
	if profile == nil || len(profile.ModelIDs) == 0 || slices.Contains(profile.ModelIDs, model) {
		return nil
	}

	return fmt.Errorf("%w: %s", transformer.ErrInvalidModel, model)
}

// serveBailianTranscription handles the request and returns true when it answered it; otherwise
// it returns false and the request body reads exactly as the client sent it.
func (handlers *OpenAIHandlers) serveBailianTranscription(c *gin.Context) bool {
	if handlers == nil || handlers.TranscriptionHandlers == nil || c.Request == nil || c.Request.Body == nil {
		return false
	}

	boundary, ok := multipartBoundary(c.Request.Header.Get("Content-Type"))
	if !ok {
		return false
	}

	// Compressed bodies keep the upstream path (and its limits): their size is not the file size.
	if enc := strings.TrimSpace(c.Request.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return false
	}

	if c.Request.ContentLength >= 0 && c.Request.ContentLength <= upstreamAudioFileCap {
		return handlers.serveSmallBailianTranscription(c, boundary)
	}

	return handlers.serveLargeBailianTranscription(c, boundary)
}

func multipartBoundary(contentType string) (string, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") || params["boundary"] == "" {
		return "", false
	}

	return params["boundary"], true
}

// serveSmallBailianTranscription looks at a body the upstream handler can take (at most 26 MiB).
func (handlers *OpenAIHandlers) serveSmallBailianTranscription(c *gin.Context, boundary string) bool {
	original := c.Request.Body

	body, err := readDeclaredBody(original, c.Request.ContentLength)
	if int64(len(body)) < c.Request.ContentLength {
		// Let upstream see the same bytes and then the same read error, on every read: a net/http
		// body reports a cut-off upload once and reads as a clean EOF afterwards.
		c.Request.Body = readCloser{io.MultiReader(bytes.NewReader(body), errReader{err: err}), original}
		return false
	}

	// The upstream handler reads the same bytes; the buffer is released once it has read them,
	// so this copy does not live on next to the upstream one for the whole request.
	c.Request.Body = &releasingBody{buf: body}

	form, err := scanMultipartForm(bytes.NewReader(body), boundary)
	if err != nil {
		return false
	}

	model := form.model()
	if !bailianasr.SupportsModel(model) {
		return false
	}

	if !form.hasFile {
		if rawURL := form.audioURL(); rawURL != "" {
			handlers.serveBailianURLTranscription(c, form.fields, model, rawURL)
			return true
		}

		return false
	}

	if form.stream() && bailianasr.StreamsWithKeepAlive(model) {
		bailianKeepAliveHandlers(handlers.TranscriptionHandlers).ChatCompletion(c)
		return true
	}

	return false
}

type readCloser struct {
	io.Reader
	io.Closer
}

// smallBodyFirstRead is the first buffer size of readDeclaredBody.
const smallBodyFirstRead = 64 << 10

// readDeclaredBody reads a body of the declared length n into a buffer that grows with what
// actually arrives, so a client that declares 26 MiB and then sends nothing pins no more than
// it sent. It stops at n bytes or at the first error, which it returns (io.EOF included).
func readDeclaredBody(r io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, 0, min(n, smallBodyFirstRead))

	for int64(len(buf)) < n {
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(n, int64(2*cap(buf))))
			copy(grown, buf)
			buf = grown
		}

		read, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+read]

		if err != nil {
			return buf, err
		}
	}

	return buf, nil
}

// serveLargeBailianTranscription takes a body above the upstream caps (or of unknown length).
// It returns false when the request is not a Bailian one: the body then reads exactly as the
// client sent it, and no upload slot or spool file was used for it (see
// bailian_transcription_upload_form.go).
func (handlers *OpenAIHandlers) serveLargeBailianTranscription(c *gin.Context, boundary string) bool {
	settings := currentBailianUploadSettings()
	original := c.Request.Body

	// A known body a Bailian upload cannot take is still read up to its model field: another
	// model may take it, or turn it down with the upstream error.
	limit := max(maxLargeUploadBody(settings), upstreamRequestBodyCap+1<<20)
	watch := newBodyWatch(c, original, settings.bodyIdle)
	watch.minBytes, watch.window = settings.minRateBytes, settings.minRateWindow
	raw := newBodyRecorder(http.MaxBytesReader(c.Writer, watch, limit))

	// A slot is taken only once a file of a large-upload model grows past the upstream cap, so
	// small chunked uploads and other models never wait behind large Bailian uploads. It is
	// freed as soon as bailianasr staged the audio at Bailian (see bailianasr.StagedAudio), and
	// at the latest when the request ends.
	slot := &uploadSlot{settings: settings, ctx: c.Request.Context()}
	defer slot.free()

	parser := newLargeUploadParser(raw, boundary, settings, slot, c.Request.ContentLength)
	parser.watch = watch
	form, verdict, err := parser.parse()

	// A body read to its end goes on to a job that may run for many minutes, which must not
	// inherit the watch's read deadline (see bodyWatch.finish). After a failure the deadline
	// stays, so net/http's discard of the unread rest of the body ends with it too.
	if err == nil && verdict == verdictBailian {
		watch.finish()
	}

	defer form.cleanup()

	switch {
	case err != nil:
		handlers.writeUploadError(c, settings, err)
		return true
	case verdict == verdictTooLarge:
		handlers.writeTranscriptionError(c, errUpstreamBodyTooLarge)
		return true
	case verdict == verdictUpstream:
		c.Request.Body = raw.handOver(original)
		return false
	}

	model := form.model()

	switch {
	case form.memoryFile > upstreamAudioFileCap && bailianasr.SupportsLargeUpload(model):
		// A large file read before its model: move it to disk now.
		if err := parser.spoolForLargeUpload(); err != nil {
			handlers.writeUploadError(c, settings, err)
			return true
		}

		file := form.file
		handlers.serveBailianWithSource(c, form.fields, model, file.name,
			bailianasr.NewFileSource(file.f, file.name, file.size, file.digest, slot.free))
	case form.memoryFile >= 0:
		// The whole body is still recorded: the upstream handler reads it as the client sent it.
		c.Request.Body = raw.handOver(original)
		form.cleanup()
		slot.free()
		handlers.transcriptionHandlersFor(form.fields, model).ChatCompletion(c)
	case form.file != nil && form.file.size > upstreamAudioFileCap && bailianasr.SupportsLargeUpload(model):
		// Only a file above the upstream cap travels out of band; a smaller one is rebuilt below,
		// so whichever model a mapping sends it to gets the real audio.
		file := form.file
		handlers.serveBailianWithSource(c, form.fields, model, file.name,
			bailianasr.NewFileSource(file.f, file.name, file.size, file.digest, slot.free))
	case form.file == nil && form.audioURL() != "":
		handlers.serveBailianURLTranscription(c, form.fields, model, form.audioURL())
	case form.file == nil || form.file.size <= upstreamAudioFileCap:
		// Upstream takes it after all (a chunked body, or large fields): rebuild what it would
		// have read, free the slot and the spool, and hand over.
		body, contentType, err := rebuildMultipart(form)
		if err != nil {
			handlers.writeTranscriptionError(c, err)
			return true
		}

		form.cleanup()
		slot.free()
		replaceRequestBody(c, body, contentType)
		handlers.transcriptionHandlersFor(form.fields, model).ChatCompletion(c)
	default:
		// The message upstream gives a file above its cap.
		handlers.writeTranscriptionError(c, fmt.Errorf("%w: file too large", transformer.ErrInvalidRequest))
	}

	return true
}

// transcriptionHandlersFor returns the handlers for a Bailian request: forced keep-alive for a
// stream that waits for a long job.
func (handlers *OpenAIHandlers) transcriptionHandlersFor(fields formFields, model string) *ChatCompletionHandlers {
	if fields.stream() && bailianasr.StreamsWithKeepAlive(model) {
		return bailianKeepAliveHandlers(handlers.TranscriptionHandlers)
	}

	return handlers.TranscriptionHandlers
}

// uploadSlot holds at most one large-upload slot for a request.
type uploadSlot struct {
	settings *bailianUploadSettings
	ctx      context.Context
	release  func()
}

// acquire takes the slot, waiting up to queueWait; an API key over its perKey cap or a gateway
// without a free slot gets a 429.
func (s *uploadSlot) acquire() error {
	if s.release != nil {
		return nil
	}

	release, err := s.settings.acquire(s.ctx, false)
	if err != nil {
		switch {
		case errors.Is(err, errBailianUploadKeyBusy):
			return &llm.ResponseError{
				StatusCode: http.StatusTooManyRequests,
				Detail: llm.ErrorDetail{
					Code: "too_many_large_uploads_per_key",
					Type: "rate_limit_error",
					Message: fmt.Sprintf("this API key already has %d large transcription upload(s) in progress; retry later",
						s.settings.perKey),
				},
			}
		case errors.Is(err, errBailianUploadBusy):
			return &llm.ResponseError{
				StatusCode: http.StatusTooManyRequests,
				Detail: llm.ErrorDetail{
					Code:    "too_many_large_uploads",
					Type:    "rate_limit_error",
					Message: fmt.Sprintf("%d large transcription uploads are already in progress; retry later", cap(s.settings.slots)),
				},
			}
		}

		return err
	}

	s.release = release

	return nil
}

// free releases the slot; it is safe to call more than once, also from the goroutine of the job
// (see bailianasr.StagedAudio) while the handler's deferred call runs.
func (s *uploadSlot) free() {
	if s.release != nil {
		s.release()
	}
}

// slotGate takes the upload slot once more than threshold bytes went through it.
type slotGate struct {
	slot      *uploadSlot
	threshold int64
	written   int64
}

func (g *slotGate) Write(p []byte) (int, error) {
	g.written += int64(len(p))
	if g.written > g.threshold {
		if err := g.slot.acquire(); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// bodyWatch bounds the body reads of a large upload once it spools a file, and with it holds an
// upload slot or disk space (the parser arms it). A read then fails once the request deadline
// passed, and the connection's read deadline makes a read that waits fail once the client sent
// nothing for idle, once it sent less than minBytes within window of waiting (a client trickling
// a byte now and then would otherwise keep its slot and spool file until the request deadline),
// or at the request deadline: the request timeout only cancels a context, which a blocked body
// read never sees. Only the time spent waiting in Read counts towards window, so the gateway's
// own pauses (a slot wait, a slow disk) never count against the client. Before it is armed it
// reads exactly like the body (a request that is not a Bailian one never arms it). A server with
// its own read_timeout already bounds the whole body, and its deadline is left alone; the
// minimum rate is still checked between reads there.
type bodyWatch struct {
	body     io.ReadCloser
	ctx      context.Context
	control  *http.ResponseController
	idle     time.Duration
	deadline time.Time // the request deadline; zero when there is none
	armed    bool
	finished bool

	// minBytes must arrive within window of waiting (window 0: no minimum rate). waited and
	// arrived count towards the current window, which restarts once minBytes arrived.
	minBytes int64
	window   time.Duration
	waited   time.Duration
	arrived  int64

	// readDeadline is the connection read deadline last set; rateBound reports that the minimum
	// rate (not idle or the request deadline) set it.
	readDeadline time.Time
	rateBound    bool
}

func newBodyWatch(c *gin.Context, body io.ReadCloser, idle time.Duration) *bodyWatch {
	w := &bodyWatch{body: body, ctx: c.Request.Context(), idle: idle}
	w.deadline, _ = w.ctx.Deadline()

	if srv, ok := w.ctx.Value(http.ServerContextKey).(*http.Server); !ok || srv.ReadTimeout <= 0 {
		w.control = http.NewResponseController(c.Writer)
	}

	return w
}

// errBodyTooSlow is the cause of a read the minimum rate of bodyWatch cut off. It is also an
// os.ErrDeadlineExceeded, the error a read deadline gives.
type errBodyTooSlow struct {
	minBytes int64
	window   time.Duration
}

func (e *errBodyTooSlow) Error() string {
	return fmt.Sprintf("the request body arrived slower than %d bytes per %s", e.minBytes, e.window)
}

func (e *errBodyTooSlow) Unwrap() error { return os.ErrDeadlineExceeded }

func (w *bodyWatch) Read(p []byte) (int, error) {
	if !w.armed {
		return w.body.Read(p)
	}

	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	if w.window > 0 && w.waited >= w.window {
		return 0, w.tooSlow()
	}

	w.extend(false)

	started := time.Now()
	n, err := w.body.Read(p)

	if w.window > 0 {
		w.waited += time.Since(started)
		w.arrived += int64(n)

		if w.arrived >= w.minBytes {
			w.waited, w.arrived = 0, 0
		}
	}

	if err != nil && w.rateBound && errors.Is(err, os.ErrDeadlineExceeded) {
		return n, w.tooSlow()
	}

	return n, err
}

func (w *bodyWatch) tooSlow() error {
	return &errBodyTooSlow{minBytes: w.minBytes, window: w.window}
}

func (w *bodyWatch) Close() error { return w.body.Close() }

// arm starts bounding the reads; it does nothing once the body was finished.
func (w *bodyWatch) arm() {
	if w == nil || w.armed || w.finished {
		return
	}

	w.armed = true
	w.waited, w.arrived = 0, 0
	w.extend(true)
}

// bodyWatchDeadlineGrace puts the read deadline just after the request deadline: a body read
// that fails makes net/http cancel the request, and the request context must have ended with its
// own deadline first, so the client gets the deadline error rather than nothing.
const bodyWatchDeadlineGrace = time.Second

// extend moves the connection's read deadline to the earliest of idle from now, the end of the
// minimum-rate window (what is left of it from now) and the request deadline (plus
// bodyWatchDeadlineGrace). That time never moves backwards between two calls, so a deadline set
// earlier is at most a small tolerance stricter than the current one, and it is only moved when
// it lags by more than that.
func (w *bodyWatch) extend(force bool) {
	if w.control == nil {
		return
	}

	now := time.Now()
	deadline, rateBound := now.Add(w.idle), false

	if w.window > 0 {
		if end := now.Add(w.window - w.waited); end.Before(deadline) {
			deadline, rateBound = end, true
		}
	}

	if !w.deadline.IsZero() && w.deadline.Add(bodyWatchDeadlineGrace).Before(deadline) {
		deadline, rateBound = w.deadline.Add(bodyWatchDeadlineGrace), false
	}

	if !force && deadline.Sub(w.readDeadline) < w.tolerance() {
		return
	}

	w.readDeadline, w.rateBound = deadline, rateBound
	_ = w.control.SetReadDeadline(deadline)
}

// tolerance is how far the read deadline may lag before extend moves it.
func (w *bodyWatch) tolerance() time.Duration {
	bound := w.idle
	if w.window > 0 {
		bound = min(bound, w.window)
	}

	return min(bound/16, time.Second)
}

// finish ends the watch once the body has been read: the connection gets back the server's
// default (no read deadline), so the background read net/http starts at the end of the body
// does not cancel the request while its long job runs.
func (w *bodyWatch) finish() {
	if w == nil || w.finished {
		return
	}

	w.finished = true

	if w.armed && w.control != nil {
		_ = w.control.SetReadDeadline(time.Time{})
	}

	w.armed = false
}

// writeUploadError answers a failed large upload. A client that went away gets nothing; a
// request whose deadline passed gets the deadline error (as the orchestrator answers it),
// whatever read or slot wait the deadline cut short.
func (handlers *OpenAIHandlers) writeUploadError(c *gin.Context, settings *bailianUploadSettings, err error) {
	if ctxErr := c.Request.Context().Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			err = ctxErr
		case errors.Is(err, os.ErrDeadlineExceeded):
			// net/http cancels the request when a body read fails, also when bodyWatch's idle
			// read deadline made it fail: that client is still connected and gets the 408.
		default:
			return
		}
	}

	if respErr, ok := errors.AsType[*llm.ResponseError](err); ok && respErr.StatusCode == http.StatusTooManyRequests {
		c.Header("Retry-After", strconv.Itoa(max(int(settings.queueWait.Seconds()), 1)))
	}

	handlers.writeTranscriptionError(c, err)
}

func bailianFileTooLarge(settings *bailianUploadSettings) error {
	return fmt.Errorf("%w: file too large (at most %d MiB for qwen-audio-3.1-asr-flash-filetrans, paraformer-v2 "+
		"and paraformer-8k-v2, 26 MiB for other models)", transformer.ErrInvalidRequest, settings.maxBytes>>20)
}

func (handlers *OpenAIHandlers) serveBailianURLTranscription(c *gin.Context, fields formFields, model, rawURL string) {
	if !bailianasr.SupportsURLInput(model) {
		handlers.writeTranscriptionError(c, fmt.Errorf("%w: url input is not supported for %s; send the audio in the file field",
			transformer.ErrInvalidRequest, model))

		return
	}

	rawURL = strings.TrimSpace(rawURL)
	if !bailianasr.IsHTTPURL(rawURL) {
		handlers.writeTranscriptionError(c, fmt.Errorf("%w: url must be an absolute http(s) URL", transformer.ErrInvalidRequest))
		return
	}

	src := bailianasr.NewURLSource(rawURL)

	name := src.Name()
	if name == "" {
		name = "audio"
	}

	// The real URL only travels in the request context; the body the upstream handler parses
	// (and stores) carries it without its query string, which may be a download credential.
	handlers.serveBailianWithSource(c, redactURLFields(fields), model, name, src)
}

// redactURLFields copies fields with the url and file_url values redacted.
func redactURLFields(fields formFields) formFields {
	redacted := make(formFields, len(fields))

	for i, field := range fields {
		if field.name == "url" || field.name == "file_url" {
			field.value = bailianasr.RedactURL(field.value)
		}

		redacted[i] = field
	}

	return redacted
}

// serveBailianWithSource continues on the upstream handler with a small placeholder body while
// bailianasr reads src.
func (handlers *OpenAIHandlers) serveBailianWithSource(
	c *gin.Context,
	fields formFields,
	model, filename string,
	src bailianasr.AudioSource,
) {
	body, contentType, err := placeholderMultipart(fields, filename)
	if err != nil {
		handlers.writeTranscriptionError(c, err)
		return
	}

	replaceRequestBody(c, body, contentType)
	c.Request = c.Request.WithContext(bailianasr.WithAudioSource(c.Request.Context(), src))

	handlers.transcriptionHandlersFor(fields, model).ChatCompletion(c)
}

// bailianKeepAliveHandlers returns handlers whose SSE responses always carry keep-alive
// comments: a long Bailian job sends nothing else until its transcript is ready.
func bailianKeepAliveHandlers(h *ChatCompletionHandlers) *ChatCompletionHandlers {
	copied := *h

	if !copied.sseKeepAlive.Enabled || copied.sseKeepAlive.Interval <= 0 {
		interval := copied.sseKeepAlive.Interval
		if interval <= 0 {
			interval = defaultBailianKeepAliveInterval
		}

		copied.sseKeepAlive = SSEKeepAliveConfig{Enabled: true, Interval: interval}
	}

	if copied.sseHeartbeatFormat == sseHeartbeatNone {
		copied.sseHeartbeatFormat = sseHeartbeatOpenAI
	}

	return &copied
}

// replaceRequestBody hands the upstream handler a body built here; the buffer is released once
// it has been read, so it does not live on next to the upstream copy for the whole request.
func replaceRequestBody(c *gin.Context, body []byte, contentType string) {
	c.Request.Body = &releasingBody{buf: body}
	c.Request.ContentLength = int64(len(body))
	c.Request.TransferEncoding = nil
	c.Request.Header.Set("Content-Type", contentType)
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func (handlers *OpenAIHandlers) writeTranscriptionError(c *gin.Context, err error) {
	if orch := handlers.TranscriptionHandlers.ChatCompletionOrchestrator; orch != nil && orch.Inbound != nil {
		httpErr := orch.Inbound.TransformError(c.Request.Context(), err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))

		return
	}

	JSONError(c, http.StatusBadRequest, err)
}

type formField struct {
	name  string
	value string
}

type formFields []formField

func (f formFields) first(name string) string {
	for _, field := range f {
		if field.name == name {
			return field.value
		}
	}

	return ""
}

// model is the model field without surrounding spaces, as the upstream handler reads it.
func (f formFields) model() string {
	return strings.TrimSpace(f.first("model"))
}

// stream mirrors the upstream stream field parsing; invalid values count as false (upstream
// rejects them later).
func (f formFields) stream() bool {
	switch strings.ToLower(strings.TrimSpace(f.first("stream"))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// audioURL is the url (Groq style) or file_url field.
func (f formFields) audioURL() string {
	if v := strings.TrimSpace(f.first("url")); v != "" {
		return v
	}

	return strings.TrimSpace(f.first("file_url"))
}

// placeholderMultipart is the small body the upstream handler parses when the audio travels out
// of band: the same fields in the same order and a placeholder file under the client's name.
func placeholderMultipart(fields formFields, filename string) ([]byte, string, error) {
	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)
	if err := writeFormFields(writer, fields); err != nil {
		return nil, "", err
	}

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", fmt.Errorf("failed to build the transcription form: %w", err)
	}

	if _, err := io.WriteString(part, bailianPlaceholderAudio); err != nil {
		return nil, "", fmt.Errorf("failed to build the transcription form: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to build the transcription form: %w", err)
	}

	return buf.Bytes(), writer.FormDataContentType(), nil
}

// rebuildMultipart restores a body the upstream handler can take (file at most 26 MiB).
func rebuildMultipart(form *spooledForm) ([]byte, string, error) {
	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)
	if err := writeFormFields(writer, form.fields); err != nil {
		return nil, "", err
	}

	if form.file != nil {
		part, err := writer.CreateFormFile("file", form.file.name)
		if err != nil {
			return nil, "", fmt.Errorf("failed to rebuild the transcription form: %w", err)
		}

		if _, err := io.Copy(part, form.file.reader()); err != nil {
			return nil, "", spoolError(err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to rebuild the transcription form: %w", err)
	}

	return buf.Bytes(), writer.FormDataContentType(), nil
}

func writeFormFields(writer *multipart.Writer, fields formFields) error {
	for _, field := range fields {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return fmt.Errorf("failed to build the transcription form: %w", err)
		}
	}

	return nil
}
