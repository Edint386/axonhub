package api

// Regression tests for the third 2026-11-05 review round of the upload hook: a multipart
// preamble cannot grow the recording past its cap, a client that stops sending loses its slot
// at the idle timeout or the request deadline, a deadline is answered, a slot wait never holds
// the recording, models that cannot take large audio never wait for a slot, spool files have no
// name on disk, and the file name of an audio URL is not stored.

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
)

// preambleBody is a multipart body (boundary B) whose first part comes after size bytes of
// 1 KiB preamble lines, which mime/multipart skips inside NextPart.
func preambleBody(size int64, model string) io.Reader {
	line := strings.Repeat("x", 1022) + "\r\n"

	return io.MultiReader(
		&repeatReader{chunk: []byte(line), left: int(size / int64(len(line)))},
		strings.NewReader("--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n"+model+"\r\n"+
			"--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nRIFF\r\n--B--\r\n"),
	)
}

func TestLargeUploadParser_PreambleCannotGrowTheRecordingPastItsCap(t *testing.T) {
	settings := newBailianUploadSettings(1024<<20, 2, t.TempDir())
	counted := &countingReader{r: preambleBody(300<<20, "paraformer-v2")}
	raw := newBodyRecorder(counted)

	_, verdict, err := newLargeUploadParser(raw, "B", settings, &uploadSlot{settings: settings, ctx: t.Context()}, -1).parse()
	require.NoError(t, err)
	require.Equal(t, verdictTooLarge, verdict, "a body that long is too large for the upstream handler")
	require.LessOrEqual(t, raw.size, int64(recordingHardCap))
	require.Nil(t, raw.chunks, "the recording is dropped")
	require.Less(t, counted.n.Load(), int64(recordingHardCap+1<<20), "the parse stops at the cap, not at the end of the body")

	// Through the handler the answer is the upstream one, byte for byte.
	h := newBailianUploadHarness(t)
	hooked := h.serve(preambleBody(100<<20, "whisper-1"), -1, "multipart/form-data; boundary=B")
	direct := h.upstream(preambleBody(100<<20, "whisper-1"), -1, "multipart/form-data; boundary=B")
	require.Equal(t, direct.Code, hooked.Code)
	require.Equal(t, direct.Body.String(), hooked.Body.String())
}

// stallingUpload sends head and then size pattern bytes of a chunked body, and then nothing
// until the test ends.
func stallingUpload(t *testing.T, url string, head []byte, size int64, contentType string) {
	t.Helper()

	reader, writer := io.Pipe()
	release := make(chan struct{})

	t.Cleanup(func() {
		close(release)
		_ = writer.CloseWithError(io.ErrUnexpectedEOF)
	})

	go func() {
		if _, err := writer.Write(head); err != nil {
			return
		}

		if _, err := io.Copy(writer, newPatternReader(size)); err != nil {
			return
		}

		<-release
	}()

	go func() {
		req, err := http.NewRequest(http.MethodPost, url, reader)
		if err != nil {
			return
		}

		req.Header.Set("Content-Type", contentType)

		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
}

func TestCreateTranscription_StalledLargeUploadReleasesItsSlot(t *testing.T) {
	h := newBailianUploadHarness(t)

	for _, tc := range []struct {
		name    string
		idle    time.Duration
		timeout time.Duration
	}{
		{"the client sends nothing for the idle timeout", 300 * time.Millisecond, time.Hour},
		{"the request deadline passes", time.Hour, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
			settings.bodyIdle = tc.idle
			setBailianUploadSettingsForTest(t, settings)

			type outcome struct {
				status int
				body   string
				slots  int
			}

			done := make(chan outcome, 1)

			engine := gin.New()
			engine.POST("/v1/audio/transcriptions", func(c *gin.Context) {
				ctx, cancel := context.WithTimeout(ent.NewContext(authz.WithTestBypass(c.Request.Context()), h.client), tc.timeout)
				defer cancel()

				recorded := &recordingWriter{ResponseWriter: c.Writer}
				c.Writer = recorded
				c.Request = c.Request.WithContext(ctx)

				h.handlers.CreateTranscription(c)
				done <- outcome{status: c.Writer.Status(), body: recorded.body.String(), slots: len(settings.slots)}
			})

			server := httptest.NewServer(engine)
			t.Cleanup(server.Close)

			var head bytes.Buffer

			writer := multipart.NewWriter(&head)
			require.NoError(t, writer.WriteField("model", "paraformer-v2"))
			_, err := writer.CreateFormFile("file", "a.wav")
			require.NoError(t, err)
			head.Write(apiTestWAV(0))

			// 30 MiB takes the only upload slot, then the client stops sending.
			stallingUpload(t, server.URL+"/v1/audio/transcriptions", head.Bytes(), 30<<20, writer.FormDataContentType())

			select {
			case got := <-done:
				require.Zero(t, got.slots, "the slot is free again")
				require.NotEqual(t, http.StatusOK, got.status, got.body)
				require.NotEmpty(t, got.body, "the client gets an answer, not an empty 200")

				if tc.idle < time.Minute {
					require.Equal(t, http.StatusRequestTimeout, got.status, got.body)
					require.Equal(t, "request_body_timeout", gjson.Get(got.body, "error.code").String(), got.body)
				} else {
					require.Contains(t, got.body, "deadline exceeded")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the stalled upload kept its slot")
			}

			h.requireSpoolEmpty(t)
		})
	}
}

// recordingWriter keeps a copy of the response body.
type recordingWriter struct {
	gin.ResponseWriter

	body bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.body.Write(p)
	return w.ResponseWriter.Write(p)
}

func (w *recordingWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// Unwrap lets http.ResponseController reach the connection, as the production middleware
// writer (middleware.realtimeResponseHeadersWriter) does.
func (w *recordingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// serveWithin runs CreateTranscription with a request deadline of timeout.
func (h *bailianUploadHarness) serveWithin(timeout time.Duration, body io.Reader, length int64, contentType string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(ctx)
	c.Request.ContentLength = length
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.CreateTranscription(c)

	return w
}

func TestCreateTranscription_DeadlineDuringASlotWaitIsAnswered(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 10 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)

	started := time.Now()
	w := h.serveWithin(300*time.Millisecond, body, length, contentType)
	require.Less(t, time.Since(started), 5*time.Second)
	require.NotEqual(t, http.StatusOK, w.Code, "a deadline is not an empty success")
	require.Contains(t, w.Body.String(), "deadline exceeded")
	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_SlotWaitDoesNotHoldTheRecording(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 3 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	// The Node SDK order: a 40 MiB file, then its model. The file is only held in the recording
	// until the model is known; the slot wait that follows must not keep it.
	body := newFormBody(t, nil, 40<<20, [][2]string{{"model", "paraformer-v2"}})

	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	answered := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()

	go func() { answered <- h.serve(body.reader(), -1, body.contentType) }()

	time.Sleep(1500 * time.Millisecond) // the body is read and the request waits for the slot

	runtime.GC()

	var during runtime.MemStats
	runtime.ReadMemStats(&during)

	w := <-answered
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	require.GreaterOrEqual(t, time.Since(started), 2*time.Second, "the memory was measured during the slot wait")
	require.Less(t, int64(during.HeapAlloc)-int64(before.HeapAlloc), int64(16<<20), "the 40 MiB recording is dropped before the wait")
	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_SmallFileModelsNeverWaitForASlot(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 10 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	tooLarge := func(t *testing.T, w *httptest.ResponseRecorder, started time.Time) {
		t.Helper()

		require.Less(t, time.Since(started), 5*time.Second, "never waits for a large-upload slot")
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "invalid request: file too large", gjson.Get(w.Body.String(), "error.message").String())
		require.Empty(t, w.Header().Get("Retry-After"), "a retry can never succeed")
	}

	for _, model := range []string{"qwen-audio-3.1-asr-flash", "qwen-audio-3.1-asr-flash-message"} {
		t.Run(model, func(t *testing.T) {
			body, length, contentType, _ := largeForm(t, [][2]string{{"model", model}}, "a.wav", 27<<20)

			started := time.Now()
			tooLarge(t, h.serve(body, length, contentType), started)

			// The model after a file too large for the upstream handler (the spool met the busy
			// slot before the model was known).
			after := newFormBody(t, nil, 70<<20, [][2]string{{"model", model}})

			started = time.Now()
			tooLarge(t, h.serve(after.reader(), -1, after.contentType), started)
			h.requireSpoolEmpty(t)
		})
	}
}

func TestSpoolWriter_SpoolFileHasNoNameOnDisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot unlink an open file; the cleanup and sweepSpool remove it there")
	}

	dir := t.TempDir()

	w, err := newSpoolWriter(dir, "a.wav", io.Discard)
	require.NoError(t, err)
	require.NoError(t, w.finish(strings.NewReader("RIFF audio"), 1<<20, nil, newBailianUploadSettings(1<<20, 1, dir)))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a restart or kill mid-request leaves nothing behind")

	data, err := io.ReadAll(w.file.reader())
	require.NoError(t, err)
	require.Equal(t, "RIFF audio", string(data))

	form := &spooledForm{file: w.file, memoryFile: -1}
	form.cleanup()
	form.cleanup()

	_, err = w.file.f.Stat()
	require.Error(t, err, "the cleanup closed the file, which frees its space")
}

func TestCreateTranscription_AudioURLFileNamesAreNotStored(t *testing.T) {
	h := newBailianUploadHarness(t)

	w := h.form(t, [][2]string{{"model", "paraformer-v2"}, {"url", "https://utfs.io/f/K3yS3cr3tCapabilityToken"}}, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	h.fake.mu.Lock()
	require.Contains(t, h.fake.submitBodies[len(h.fake.submitBodies)-1], "K3yS3cr3tCapabilityToken", "Bailian still gets the real URL")
	h.fake.mu.Unlock()

	requests, err := h.client.Request.Query().All(h.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, requests)

	for _, request := range requests {
		require.NotContains(t, string(request.RequestBody), "K3yS3cr3t", string(request.RequestBody))
	}

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)

	for _, execution := range executions {
		require.NotContains(t, string(execution.RequestBody), "K3yS3cr3t")
	}
}
