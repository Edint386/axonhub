package api

// Tests for bailian_transcription_upload.go: the fork hook in CreateTranscription, large uploads
// spooled to disk, URL input, forced keep-alive for long Bailian streams and the -message model,
// all through the real orchestrator and a channel built by ChannelService against a fake DashScope.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// TestCreateTranscription_ForkHookServesBailianFirst guards the fork hook in openai.go:
//
//	if handlers.serveBailianTranscription(c) { // fork: see bailian_transcription_upload.go
//		return
//	}
//
// A merge that drops it still compiles; without it Bailian transcription loses large uploads,
// URL input and keep-alive for long jobs (the behaviour tests below would fail as well).
func TestCreateTranscription_ForkHookServesBailianFirst(t *testing.T) {
	src, err := os.ReadFile("openai.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "openai.go", src, parser.ParseComments)
	require.NoError(t, err)

	var fn *ast.FuncDecl

	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "CreateTranscription" && d.Recv != nil {
			fn = d
		}
	}

	require.NotNil(t, fn, "CreateTranscription not found in openai.go")
	require.NotEmpty(t, fn.Body.List)

	hook, ok := fn.Body.List[0].(*ast.IfStmt)
	require.True(t, ok, "the first statement of CreateTranscription must be the Bailian fork hook")

	call, ok := hook.Cond.(*ast.CallExpr)
	require.True(t, ok)
	sel, ok := call.Fun.(*ast.SelectorExpr)
	require.True(t, ok)
	require.Equal(t, "serveBailianTranscription", sel.Sel.Name)

	_, returns := hook.Body.List[0].(*ast.ReturnStmt)
	require.True(t, returns, "the hook must return when it served the request")

	line := strings.Split(string(src), "\n")[fset.Position(hook.Pos()).Line-1]
	require.Contains(t, line, "// fork:")
}

// bailianUploadFake answers getPolicy, the OSS form upload (hashing the file part as a stream),
// submit, poll, the result file, the flash call and the -message inference WebSocket.
type bailianUploadFake struct {
	server *httptest.Server

	mu            sync.Mutex
	uploadSizes   []int64
	uploadDigests []string
	submitBodies  []string
	flashBodies   []string
	wsAudioBytes  int
	wsRunTasks    []string

	// taskHold (when set) holds every task poll until it is closed; taskPolled gets a value per
	// held poll when there is room.
	taskHold   chan struct{}
	taskPolled chan struct{}
}

const bailianUploadTaskID = "5b1f0c4e-upload-test-task"

func newBailianUploadFake(t *testing.T) *bailianUploadFake {
	t.Helper()

	f := &bailianUploadFake{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)

	return f
}

func (f *bailianUploadFake) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/uploads":
		_, _ = fmt.Fprintf(w, `{"request_id":"p","data":{"policy":"cG9saWN5","signature":"sig","upload_dir":"dashscope-instant/u",`+
			`"upload_host":"%s/oss","expire_in_seconds":300,"max_file_size_mb":1024,"oss_access_key_id":"LTAI",`+
			`"x_oss_object_acl":"private","x_oss_forbid_overwrite":"true"}}`, f.server.URL)
	case "/oss":
		f.serveUpload(w, r)
	case "/api/v1/services/audio/asr/transcription":
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.submitBodies = append(f.submitBodies, string(body))
		f.mu.Unlock()

		_, _ = fmt.Fprintf(w, `{"output":{"task_status":"PENDING","task_id":"%s"},"request_id":"s"}`, bailianUploadTaskID)
	case "/api/v1/tasks/" + bailianUploadTaskID:
		f.mu.Lock()
		hold, polled := f.taskHold, f.taskPolled
		f.mu.Unlock()

		if hold != nil {
			select {
			case polled <- struct{}{}:
			default:
			}

			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}

		_, _ = fmt.Fprintf(w, `{"request_id":"q","output":{"task_id":"%s","task_status":"SUCCEEDED","results":[{"file_url":"oss://x",`+
			`"transcription_url":"%s/result.json","subtask_status":"SUCCEEDED"}]},"usage":{"duration":7200}}`, bailianUploadTaskID, f.server.URL)
	case "/result.json":
		_, _ = io.WriteString(w, `{"properties":{"original_duration_in_milliseconds":7200000},"transcripts":[{"channel_id":0,`+
			`"text":"会议开始。会议结束。","sentences":[{"begin_time":0,"end_time":2000,"text":"会议开始。","sentence_id":1},`+
			`{"begin_time":7190000,"end_time":7199000,"text":"会议结束。","sentence_id":2}]}]}`)
	case "/api/v1/services/aigc/multimodal-generation/generation":
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.flashBodies = append(f.flashBodies, string(body))
		f.mu.Unlock()

		_, _ = io.WriteString(w, `{"output":{"sentence":{"sentence_id":1,"begin_time":0,"end_time":900,"text":"你好。","sentence_end":true}},`+
			`"usage":{"duration":1,"input_tokens":30,"output_tokens":2,"total_tokens":32},"request_id":"f"}`)
	case "/api-ws/v1/inference":
		f.serveInference(w, r)
	default:
		http.Error(w, `{"code":"NotFound","message":"no route"}`, http.StatusNotFound)
	}
}

func (f *bailianUploadFake) serveUpload(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "bad content type", http.StatusBadRequest)
		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if part.FormName() != "file" {
			continue
		}

		hash := sha256.New()
		n, _ := io.Copy(hash, part)

		f.mu.Lock()
		f.uploadSizes = append(f.uploadSizes, n)
		f.uploadDigests = append(f.uploadDigests, hex.EncodeToString(hash.Sum(nil)))
		f.mu.Unlock()
	}

	w.WriteHeader(http.StatusOK)
}

func (f *bailianUploadFake) serveInference(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	taskID := ""

	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if kind == websocket.BinaryMessage {
			f.mu.Lock()
			f.wsAudioBytes += len(data)
			f.mu.Unlock()

			continue
		}

		switch gjson.GetBytes(data, "header.action").String() {
		case "run-task":
			taskID = gjson.GetBytes(data, "header.task_id").String()

			f.mu.Lock()
			f.wsRunTasks = append(f.wsRunTasks, string(data))
			f.mu.Unlock()

			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"header":{"task_id":"`+taskID+`","event":"task-started"},"payload":{}}`))
		case "finish-task":
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"header":{"task_id":"`+taskID+`","event":"result-generated"},`+
				`"payload":{"output":{"sentence":{"sentence_id":1,"begin_time":0,"end_time":900,"text":"收到。","sentence_end":true}},`+
				`"usage":{"duration":1,"input_tokens":20,"output_tokens":3,"total_tokens":23}}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"header":{"task_id":"`+taskID+`","event":"task-finished"},"payload":{"output":{}}}`))

			return
		}
	}
}

type bailianUploadHarness struct {
	ctx      context.Context
	client   *ent.Client
	handlers *OpenAIHandlers
	fake     *bailianUploadFake
	spoolDir string
}

var bailianUploadModels = []string{
	"paraformer-v2", "qwen-audio-3.1-asr-flash", "qwen-audio-3.1-asr-flash-filetrans", "qwen-audio-3.1-asr-flash-message",
}

func newBailianUploadHarness(t *testing.T) *bailianUploadHarness {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	fake := newBailianUploadFake(t)

	row, err := client.Channel.Create().
		SetType(channel.TypeBailian).
		SetName("Bailian ASR uploads").
		SetBaseURL(fake.server.URL + "/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-upload-test"}).
		SetSupportedModels(bailianUploadModels).
		SetDefaultTestModel(bailianUploadModels[0]).
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	channelService, requestService, systemService, usageLogService := setupSpeechAPITestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreChunks: true, StoreRequestBody: true, StoreResponseBody: true}))

	bizChannel, err := channelService.GetChannel(ctx, row.ID)
	require.NoError(t, err)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{bizChannel})

	candidates := make([]*orchestrator.ChannelModelsCandidate, 0, len(bailianUploadModels))
	for _, model := range bailianUploadModels {
		candidates = append(candidates, &orchestrator.ChannelModelsCandidate{
			Channel: bizChannel,
			Models:  []biz.ChannelModelEntry{{RequestModel: model, ActualModel: model, Source: "direct"}},
		})
	}

	promptProtection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	t.Cleanup(promptProtection.Stop)

	orch := orchestrator.NewChatCompletionOrchestrator(
		channelService, nil, requestService, httpclient.NewHttpClient(), openai.NewTranscriptionInboundTransformer(), systemService,
		usageLogService, nil, nil, promptProtection, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil,
	).WithChannelSelector(&bailianASRSelector{candidates: candidates})
	orch.PipelineFactory = pipeline.NewFactory(httpclient.NewHttpClient())

	spoolDir := t.TempDir()
	setBailianUploadSettingsForTest(t, newBailianUploadSettings(1024<<20, 2, spoolDir))

	return &bailianUploadHarness{
		ctx:    ctx,
		client: client,
		// Production handlers: no StreamWriter override, keep-alive off in the server config.
		handlers: &OpenAIHandlers{TranscriptionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orch,
			sseKeepAlive:               SSEKeepAliveConfig{Interval: 50 * time.Millisecond},
			sseHeartbeatFormat:         sseHeartbeatOpenAI,
			inputUsageCache:            newInputUsageCache(),
		}},
		fake:     fake,
		spoolDir: spoolDir,
	}
}

func setBailianUploadSettingsForTest(t *testing.T, settings *bailianUploadSettings) {
	t.Helper()

	bailianUploadMu.Lock()
	previous := bailianUploadCurrent
	bailianUploadCurrent = settings
	bailianUploadMu.Unlock()

	t.Cleanup(func() {
		bailianUploadMu.Lock()
		bailianUploadCurrent = previous
		bailianUploadMu.Unlock()
	})
}

// serve runs CreateTranscription on a raw body; length -1 sends it without a Content-Length.
func (h *bailianUploadHarness) serve(body io.Reader, length int64, contentType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(h.ctx)
	c.Request.ContentLength = length
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.CreateTranscription(c)

	return w
}

func (h *bailianUploadHarness) form(t *testing.T, fields [][2]string, filename string, file []byte) *httptest.ResponseRecorder {
	t.Helper()

	body, contentType := buildForm(t, fields, filename, file)

	return h.serve(bytes.NewReader(body), int64(len(body)), contentType)
}

func buildForm(t *testing.T, fields [][2]string, filename string, file []byte) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)
	for _, field := range fields {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}

	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = part.Write(file)
		require.NoError(t, err)
	}

	require.NoError(t, writer.Close())

	return body.Bytes(), writer.FormDataContentType()
}

func (h *bailianUploadHarness) requireSpoolEmpty(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(h.spoolDir)
	require.NoError(t, err)
	require.Empty(t, entries, "the spooled upload must be removed")
}

// patternReader yields size deterministic bytes without allocating them.
type patternReader struct {
	block     []byte
	offset    int
	remaining int64
}

func newPatternReader(size int64) *patternReader {
	block := make([]byte, 4093)
	rng := rand.New(rand.NewPCG(1, 2))

	for i := range block {
		block[i] = byte(rng.UintN(256))
	}

	return &patternReader{block: block, remaining: size}
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), r.remaining))
	for i := 0; i < n; {
		c := copy(p[i:n], r.block[r.offset:])
		r.offset = (r.offset + c) % len(r.block)
		i += c
	}

	r.remaining -= int64(n)

	return n, nil
}

// largeForm streams a multipart body whose file is a WAV header followed by size pattern bytes.
func largeForm(t *testing.T, fields [][2]string, filename string, size int64) (io.Reader, int64, string, string) {
	t.Helper()

	var head bytes.Buffer

	writer := multipart.NewWriter(&head)
	for _, field := range fields {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}

	_, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)

	wav := apiTestWAV(0)
	tail := "\r\n--" + writer.Boundary() + "--\r\n"

	hash := sha256.New()
	hash.Write(wav)
	_, err = io.Copy(hash, newPatternReader(size))
	require.NoError(t, err)

	body := io.MultiReader(bytes.NewReader(head.Bytes()), bytes.NewReader(wav), newPatternReader(size), strings.NewReader(tail))
	length := int64(head.Len()+len(wav)+len(tail)) + size

	return body, length, writer.FormDataContentType(), hex.EncodeToString(hash.Sum(nil))
}

// TestCreateTranscription_BailianLargeUploadStreamsFromDisk pushes a 70 MiB file (above every
// upstream cap, including the 64 MiB request cap) through CreateTranscription: the file is
// spooled to disk and streamed to the fake OSS, memory stays far below the file size, and the
// spooled copy is gone afterwards.
func TestCreateTranscription_BailianLargeUploadStreamsFromDisk(t *testing.T) {
	h := newBailianUploadHarness(t)

	const size = 70 << 20

	body, length, contentType, digest := largeForm(t, [][2]string{
		{"model", "paraformer-v2"}, {"response_format", "srt"}, {"language", "zh"},
	}, "meeting.wav", size)

	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var peak atomic.Uint64

	stop := make(chan struct{})
	sampled := make(chan struct{})

	go func() {
		defer close(sampled)

		var stats runtime.MemStats

		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				runtime.ReadMemStats(&stats)

				if stats.HeapAlloc > peak.Load() {
					peak.Store(stats.HeapAlloc)
				}
			}
		}
	}()

	w := h.serve(body, length, contentType)

	close(stop)
	<-sampled

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "1\n00:00:00,000 --> 00:00:02,000\n会议开始。\n\n2\n01:59:50,000 --> 01:59:59,000\n会议结束。\n\n", w.Body.String())

	h.fake.mu.Lock()
	require.Equal(t, []int64{size + 44}, h.fake.uploadSizes, "the whole file reached the temporary storage")
	require.Equal(t, []string{digest}, h.fake.uploadDigests)
	require.Len(t, h.fake.submitBodies, 1)
	h.fake.mu.Unlock()

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated %d MiB in total, heap peak %d MiB above the start", allocated>>20, (peak.Load()-min(peak.Load(), before.HeapAlloc))>>20)
	require.Less(t, allocated, uint64(size/4), "the file must never be read into memory")
	require.Less(t, peak.Load()-min(peak.Load(), before.HeapAlloc), uint64(24<<20))

	h.requireSpoolEmpty(t)

	requests, err := h.client.Request.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Less(t, len(requests[0].RequestBody), 1024, "the stored request names the audio instead of carrying it")
	require.Equal(t, "paraformer-v2", gjson.GetBytes(requests[0].RequestBody, "model").String())

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, executions, 1)
	require.Contains(t, string(executions[0].RequestBody), "axonhub-pending-upload")
	require.Less(t, len(executions[0].RequestBody), 1024)

	usageLogs, err := h.client.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, usageLogs, 1)
}

func TestCreateTranscription_LargeUploadForOtherModelsKeepsTheUpstreamError(t *testing.T) {
	h := newBailianUploadHarness(t)

	for _, model := range []string{"qwen-audio-3.1-asr-flash", "whisper-1"} {
		body, length, contentType, _ := largeForm(t, [][2]string{{"model", model}}, "a.wav", 30<<20)

		w := h.serve(body, length, contentType)
		require.Equal(t, http.StatusBadRequest, w.Code, model)
		require.Equal(t, "invalid_request_error", gjson.Get(w.Body.String(), "error.type").String())
		require.Equal(t, "invalid request: file too large", gjson.Get(w.Body.String(), "error.message").String())
	}

	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_LargeUploadAboveTheConfiguredMax(t *testing.T) {
	h := newBailianUploadHarness(t)
	setBailianUploadSettingsForTest(t, newBailianUploadSettings(1<<20, 2, h.spoolDir))

	// The Content-Length is enough to refuse it once the model is known, before the file is read.
	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 40<<20)
	counted := &countingReader{r: body}

	w := h.serve(counted, length, contentType)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "file too large (at most 1 MiB")
	require.Less(t, counted.n.Load(), int64(64<<10), "refused before reading the file")

	// Without a Content-Length the limit applies while spooling.
	body, _, contentType, _ = largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 3<<20)
	w = h.serve(body, -1, contentType)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "file too large (at most 1 MiB")

	h.requireSpoolEmpty(t)
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))

	return n, err
}

func TestCreateTranscription_LargeUploadsShareBoundedSlots(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 50 * time.Millisecond
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)

	w := h.serve(body, length, contentType)
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.Equal(t, "rate_limit_error", gjson.Get(w.Body.String(), "error.type").String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))

	// Without a Content-Length the slot is only needed once the file outgrows the upstream cap.
	body, _, contentType, _ = largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	w = h.serve(body, -1, contentType)
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())

	small, smallType := buildForm(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}, {"response_format", "text"}}, "hi.wav", apiTestWAV(1600))
	w = h.serve(bytes.NewReader(small), -1, smallType)
	require.Equal(t, http.StatusOK, w.Code, "a small chunked upload never waits for a large-upload slot: %s", w.Body.String())

	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_ChunkedSmallBodyStillReachesUpstream(t *testing.T) {
	h := newBailianUploadHarness(t)

	body, contentType := buildForm(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}, {"response_format", "text"}}, "hi.wav", apiTestWAV(1600))

	w := h.serve(bytes.NewReader(body), -1, contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "你好。\n", w.Body.String())

	h.fake.mu.Lock()
	require.Len(t, h.fake.flashBodies, 1)
	require.True(t, strings.HasPrefix(gjson.Get(h.fake.flashBodies[0], "input.messages.0.content.0.input_audio.data").String(),
		"data:audio/wav;base64,UklGR"), "the rebuilt body carries the real audio")
	h.fake.mu.Unlock()

	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_URLInput(t *testing.T) {
	h := newBailianUploadHarness(t)

	w := h.form(t, [][2]string{{"model", "paraformer-v2"}, {"url", "https://media.example.com/calls/2026-11-05.mp3?sig=abc"}}, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "会议开始。会议结束。", gjson.Get(w.Body.String(), "text").String())

	w = h.form(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}, {"file_url", "https://media.example.com/hello.wav"}}, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	h.fake.mu.Lock()
	require.Len(t, h.fake.uploadSizes, 0, "Bailian downloads the URL itself")
	require.Equal(t, "https://media.example.com/calls/2026-11-05.mp3?sig=abc", gjson.Get(h.fake.submitBodies[0], "input.file_urls.0").String())
	require.Equal(t, "https://media.example.com/hello.wav", gjson.Get(h.fake.flashBodies[0], "input.messages.0.content.0.input_audio.data").String())
	require.Equal(t, "wav", gjson.Get(h.fake.flashBodies[0], "parameters.format").String())
	h.fake.mu.Unlock()

	w = h.form(t, [][2]string{{"model", "paraformer-v2"}, {"url", "ftp://media.example.com/a.mp3"}}, "", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "absolute http(s) URL")

	w = h.form(t, [][2]string{{"model", "paraformer-v2"}, {"url", "/etc/passwd"}}, "", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = h.form(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash-message"}, {"url", "https://media.example.com/a.wav"}}, "", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "url input is not supported for qwen-audio-3.1-asr-flash-message")

	// Other models keep the upstream behaviour.
	w = h.form(t, [][2]string{{"model", "whisper-1"}, {"url", "https://media.example.com/a.wav"}}, "", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "file is required")

	requests, err := h.client.Request.Query().All(h.ctx)
	require.NoError(t, err)

	for _, request := range requests {
		require.NotContains(t, string(request.RequestBody), "axonhub: the audio", "the placeholder is logged by size, not content")
	}
}

// TestCreateTranscription_BailianAsyncStreamCommitsBeforeTheResult checks that a long job's
// stream starts right after submit and carries keep-alive comments until the transcript (the
// server config here has keep-alive off). It also guards the two upstream couplings the
// committed event relies on: the pipeline counts it as content, and the client never sees it.
func TestCreateTranscription_BailianAsyncStreamCommitsBeforeTheResult(t *testing.T) {
	h := newBailianUploadHarness(t)

	w := h.form(t, [][2]string{{"model", "paraformer-v2"}, {"stream", "true"}}, "a.wav", apiTestWAV(1600))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")

	out := w.Body.String()
	firstDelta := strings.Index(out, "transcript.text.delta")
	require.Positive(t, firstDelta, out)

	keepAlive := strings.Index(out, ": keep-alive")
	require.GreaterOrEqual(t, keepAlive, 0, "keep-alive comments are sent while the job runs")
	require.Less(t, keepAlive, firstDelta)

	var types []string

	for line := range strings.SplitSeq(out, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			types = append(types, gjson.Get(data, "type").String())
		}
	}

	require.Equal(t, []string{"transcript.text.delta", "transcript.text.delta", "transcript.text.done"}, types,
		"the committed event never reaches the client")
}

func TestCreateTranscription_BailianMessageModel(t *testing.T) {
	h := newBailianUploadHarness(t)

	w := h.form(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash-message"}, {"language", "zh"}}, "note.wav", toneWAVForAPI(48000, 2, 0.5))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "收到。", gjson.Get(w.Body.String(), "text").String())
	require.EqualValues(t, 23, gjson.Get(w.Body.String(), "usage.total_tokens").Int())

	h.fake.mu.Lock()
	require.Equal(t, 16000, h.fake.wsAudioBytes, "0.5 s of 48 kHz stereo becomes 16 kHz mono PCM16")
	require.Equal(t, "pcm", gjson.Get(h.fake.wsRunTasks[0], "payload.parameters.format").String())
	h.fake.mu.Unlock()

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, executions, 1)
	require.Equal(t, "run-task", gjson.GetBytes(executions[0].RequestBody, "header.action").String())

	usageLogs, err := h.client.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, usageLogs, 1)
	require.EqualValues(t, 20, usageLogs[0].PromptTokens)
	require.EqualValues(t, 3, usageLogs[0].CompletionTokens)
}

func toneWAVForAPI(rate, channels int, seconds float64) []byte {
	frames := int(float64(rate) * seconds)
	data := apiTestWAV(frames * channels)

	binary.LittleEndian.PutUint16(data[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(data[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(data[32:34], uint16(channels*2))

	return data
}
