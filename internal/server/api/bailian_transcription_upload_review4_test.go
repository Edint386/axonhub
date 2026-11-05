package api

// Regression tests for the fourth 2026-11-05 review round of the upload hook: large-upload slots
// per API key, a minimum upload rate and the API key's model list before a slot (U1); the slot is
// freed once the audio is staged and the spool disk is budgeted (U2); a file of at most 26 MiB
// always reaches upstream as sent (U3); a file whose model is unknown never takes a slot and is
// dropped above 26 MiB (U4); the model field is trimmed (U5); a spool failure does not reveal the
// spool path (U6).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

// serveAs runs CreateTranscription for the request context ctx.
func (h *bailianUploadHarness) serveAs(ctx context.Context, body io.Reader, length int64, contentType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(ctx)
	c.Request.ContentLength = length
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.CreateTranscription(c)

	return w
}

// fileAfterSkippedPart is a chunked-style multipart body: a skipped file part of skip bytes (it
// fills what the upstream handler would read), then the audio, then the model field.
func fileAfterSkippedPart(t *testing.T, skip int64, audio []byte, model string) (io.Reader, string) {
	t.Helper()

	var head bytes.Buffer

	first := multipart.NewWriter(&head)
	_, err := first.CreateFormFile("attachment", "notes.bin")
	require.NoError(t, err)

	var tail bytes.Buffer

	tail.WriteString("\r\n")

	rest := multipart.NewWriter(&tail)
	require.NoError(t, rest.SetBoundary(first.Boundary()))

	part, err := rest.CreateFormFile("file", "a.wav")
	require.NoError(t, err)
	_, err = part.Write(audio)
	require.NoError(t, err)
	require.NoError(t, rest.WriteField("model", model))
	require.NoError(t, rest.WriteField("response_format", "text"))
	require.NoError(t, rest.Close())

	return io.MultiReader(bytes.NewReader(head.Bytes()), newPatternReader(skip), bytes.NewReader(tail.Bytes())),
		first.FormDataContentType()
}

func withUploadAPIKey(ctx context.Context, id int, models ...string) context.Context {
	key := &ent.APIKey{ID: id}
	if len(models) > 0 {
		key.Profiles = &objects.APIKeyProfiles{
			ActiveProfile: "limited",
			Profiles:      []objects.APIKeyProfile{{Name: "limited", ModelIDs: models}},
		}
	}

	return contexts.WithAPIKey(ctx, key)
}

// U1: one API key cannot hold every large-upload slot: it gets at most perKey of them at once.
func TestCreateTranscription_LargeUploadSlotsPerAPIKey(t *testing.T) {
	settings := newBailianUploadSettings(1024<<20, 2, t.TempDir())
	settings.queueWait = 50 * time.Millisecond

	key7, key8 := withUploadAPIKey(t.Context(), 7), withUploadAPIKey(t.Context(), 8)

	release7, err := settings.acquire(key7, false)
	require.NoError(t, err)

	_, err = settings.acquire(key7, false)
	require.ErrorIs(t, err, errBailianUploadKeyBusy, "the key already holds its one slot")
	require.Len(t, settings.slots, 1, "a refused key gives back nothing it did not take")

	release8, err := settings.acquire(key8, false)
	require.NoError(t, err, "another key still gets the second slot")

	release7()
	release7() // safe to call again

	again, err := settings.acquire(key7, false)
	require.NoError(t, err)

	again()
	release8()
	require.Empty(t, settings.slots)
	require.Empty(t, settings.keySlots, "nothing is kept for keys without uploads")

	// Through the handler: the key's second large upload gets a 429 at once.
	h := newBailianUploadHarness(t)
	settings = newBailianUploadSettings(1024<<20, 2, h.spoolDir)
	settings.queueWait = 50 * time.Millisecond
	setBailianUploadSettingsForTest(t, settings)

	ctx := withUploadAPIKey(h.ctx, 7)

	release, err := settings.acquire(ctx, false)
	require.NoError(t, err)

	defer release()

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	w := h.serveAs(ctx, body, length, contentType)
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.Equal(t, "too_many_large_uploads_per_key", gjson.Get(w.Body.String(), "error.code").String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	h.requireSpoolEmpty(t)
}

func TestLoadBailianUploadSettings_PerKeyAndSpoolBudget(t *testing.T) {
	t.Setenv("AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY", "3")
	t.Setenv("AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB", "100")

	settings := loadBailianUploadSettings()
	require.Equal(t, 3, settings.perKey)
	require.Equal(t, int64(100<<20), settings.budget.limit)
	require.Equal(t, int64(1<<20), settings.minRateBytes)
	require.Equal(t, 60*time.Second, settings.minRateWindow)

	t.Setenv("AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY", "0")
	t.Setenv("AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB", "lots")

	settings = loadBailianUploadSettings()
	require.Equal(t, 1, settings.perKey, "the default")
	require.Equal(t, int64(4096<<20), settings.budget.limit, "the default")
}

// U1: the API key's model list is checked before a slot is taken: a key that may not use the
// model is turned down at once (the orchestrator's 422), with every slot busy and before its file
// is read.
func TestCreateTranscription_ModelAccessIsCheckedBeforeASlot(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 5 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	ctx := withUploadAPIKey(h.ctx, 9, "whisper-1")
	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	counted := &countingReader{r: body}

	started := time.Now()
	w := h.serveAs(ctx, counted, length, contentType)
	require.Less(t, time.Since(started), 3*time.Second, "never waits for a slot")
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	require.Equal(t, "invalid_model_error", gjson.Get(w.Body.String(), "error.type").String())
	require.Equal(t, "model not found: paraformer-v2", gjson.Get(w.Body.String(), "error.message").String())
	require.Less(t, counted.n.Load(), int64(1<<20), "refused before the file was read")
	h.requireSpoolEmpty(t)

	// The same key with a small file gets the same answer from the orchestrator.
	small, smallType := buildForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", apiTestWAV(1600))
	direct := h.serveAs(ctx, bytes.NewReader(small), int64(len(small)), smallType)
	require.Equal(t, direct.Code, w.Code)
	require.Equal(t, direct.Body.String(), w.Body.String())

	// A key whose list has the model is not turned down by the check.
	require.NoError(t, checkUploadModelAccess(withUploadAPIKey(t.Context(), 9, "paraformer-v2"), "paraformer-v2"))
	require.NoError(t, checkUploadModelAccess(withUploadAPIKey(t.Context(), 9), "paraformer-v2"), "no list: every model")
}

// U1: a client that keeps a large upload alive with a trickle (less than the minimum rate) is cut
// off with a 408 long before the request deadline, and its slot and spool file are freed.
func TestCreateTranscription_TricklingLargeUploadIsCutOff(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.bodyIdle = time.Hour
	settings.minRateBytes, settings.minRateWindow = 1<<20, 400*time.Millisecond
	setBailianUploadSettingsForTest(t, settings)

	type outcome struct {
		status int
		body   string
		slots  int
	}

	done := make(chan outcome, 1)

	engine := gin.New()
	engine.POST("/v1/audio/transcriptions", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(ent.NewContext(authz.WithTestBypass(c.Request.Context()), h.client), time.Hour)
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

	reader, pipeWriter := io.Pipe()
	stop := make(chan struct{})

	t.Cleanup(func() {
		close(stop)
		_ = pipeWriter.CloseWithError(io.ErrUnexpectedEOF)
	})

	go func() {
		if _, err := pipeWriter.Write(head.Bytes()); err != nil {
			return
		}

		// 30 MiB at full speed takes the only slot, then 1 KiB every 50 ms: never idle, far too slow.
		if _, err := io.Copy(pipeWriter, newPatternReader(30<<20)); err != nil {
			return
		}

		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}

			if _, err := pipeWriter.Write(bytes.Repeat([]byte{1}, 1024)); err != nil {
				return
			}
		}
	}()

	go func() {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", reader)
		if err != nil {
			return
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())

		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case got := <-done:
		require.Equal(t, http.StatusRequestTimeout, got.status, got.body)
		require.Equal(t, "request_body_too_slow", gjson.Get(got.body, "error.code").String(), got.body)
		require.Zero(t, got.slots, "the slot is free again")
	case <-time.After(15 * time.Second):
		t.Fatal("the trickling upload kept its slot")
	}

	h.requireSpoolEmpty(t)
}

// U1: only the time spent waiting for the client counts towards the minimum rate, never the
// gateway's own pauses (a slot wait, a slow disk).
func TestBodyWatch_MinimumRateCountsOnlyTheClientsTime(t *testing.T) {
	slowBytes := &slowReader{delay: 60 * time.Millisecond}
	watch := &bodyWatch{body: io.NopCloser(slowBytes), ctx: t.Context(), idle: time.Hour, minBytes: 1 << 20, window: 300 * time.Millisecond}
	watch.arm()

	time.Sleep(400 * time.Millisecond) // the gateway is busy elsewhere

	buf := make([]byte, 16)

	_, err := watch.Read(buf)
	require.NoError(t, err, "the gateway's pause is not the client's")

	for range 10 {
		if _, err = watch.Read(buf); err != nil {
			break
		}
	}

	slow, ok := err.(*errBodyTooSlow)
	require.True(t, ok, "a byte every 60 ms is too slow: %v", err)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)

	respErr := multipartReadError(fmt.Errorf("multipart: NextPart: %w", slow), settingsForTest())
	require.Contains(t, respErr.Error(), "1024 KiB")
}

func settingsForTest() *bailianUploadSettings {
	return newBailianUploadSettings(1<<20, 1, os.TempDir())
}

// slowReader returns one byte per delay.
type slowReader struct {
	delay time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)

	if len(p) == 0 {
		return 0, nil
	}

	p[0] = 'x'

	return 1, nil
}

// U2: the upload slot is freed as soon as the audio reached Bailian's temporary storage, not when
// the job ends; the spool file (a retry may upload it again) stays until the request ends.
func TestCreateTranscription_UploadSlotIsFreedOnceTheAudioIsStaged(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	setBailianUploadSettingsForTest(t, settings)

	hold, polled := make(chan struct{}), make(chan struct{}, 1)

	h.fake.mu.Lock()
	h.fake.taskHold, h.fake.taskPolled = hold, polled
	h.fake.mu.Unlock()

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}, {"response_format", "text"}}, "a.wav", 30<<20)
	answered := make(chan *httptest.ResponseRecorder, 1)

	go func() { answered <- h.serve(body, length, contentType) }()

	select {
	case <-polled:
	case <-time.After(15 * time.Second):
		t.Fatal("the job never polled its task")
	}

	require.Empty(t, settings.slots, "the job polls without holding the upload slot")
	require.Greater(t, settings.budget.inUse(), int64(30<<20), "the spool file stays for the rest of the request")

	other, err := settings.acquire(t.Context(), true)
	require.NoError(t, err, "another large upload can start meanwhile")
	other()

	close(hold)

	w := <-answered
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Zero(t, settings.budget.inUse(), "the request gave its spool bytes back")
	h.requireSpoolEmpty(t)
}

// U2: all spool files together stay within the spool budget; beyond it an upload gets a 429.
func TestCreateTranscription_SpoolBudgetBoundsTheDisk(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 2, h.spoolDir)
	settings.budget.limit = 20 << 20
	setBailianUploadSettingsForTest(t, settings)

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	w := h.serve(body, length, contentType)
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.Equal(t, "upload_storage_busy", gjson.Get(w.Body.String(), "error.code").String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	require.Zero(t, settings.budget.inUse(), "a refused upload gives its bytes back")
	require.Empty(t, settings.slots)
	h.requireSpoolEmpty(t)

	settings.budget.limit = 64 << 20

	body, length, contentType, _ = largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	w = h.serve(body, length, contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Zero(t, settings.budget.inUse())
}

// U3: a file of at most 26 MiB parsed by the hook (a chunked body, a large-upload model) reaches
// the upstream handler as sent, never as a placeholder: a channel or key mapping may send it to a
// model that is not a Bailian large-upload one.
func TestCreateTranscription_SmallFilesReachUpstreamAsSent(t *testing.T) {
	h := newBailianUploadHarness(t)

	for _, tc := range []struct {
		name string
		body *formBody
	}{
		{"model first", newFormBody(t, [][2]string{{"model", "paraformer-v2"}, {"response_format", "text"}}, 1<<20, nil)},
		{"model after the file", newFormBody(t, nil, 1<<20, [][2]string{{"model", "paraformer-v2"}, {"response_format", "text"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := h.client.Request.Query().Count(h.ctx)
			require.NoError(t, err)

			w := h.serve(tc.body.reader(), -1, tc.body.contentType)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			requests, err := h.client.Request.Query().All(h.ctx)
			require.NoError(t, err)
			require.Len(t, requests, before+1)

			stored := gjson.GetBytes(requests[len(requests)-1].RequestBody, "file").String()
			require.True(t, strings.HasPrefix(stored, fmt.Sprintf("<audio bytes: %d,", int64(1<<20)+44)), stored)

			h.fake.mu.Lock()
			digest := h.fake.uploadDigests[len(h.fake.uploadDigests)-1]
			h.fake.mu.Unlock()

			require.Equal(t, tc.body.digest, digest)
			h.requireSpoolEmpty(t)
		})
	}
}

// U4: a file whose model is still unknown never takes an upload slot. One of at most 26 MiB is
// spooled without a slot (and reaches upstream as sent); a larger one is dropped at once, without
// disk, and a Bailian large-upload model then asks for the model field first.
func TestCreateTranscription_UndecidedFileNeverTakesASlot(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
	settings.queueWait = 10 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	audio := apiTestWAV(16000)
	body, contentType := fileAfterSkippedPart(t, 70<<20, audio, "paraformer-v2")

	started := time.Now()
	w := h.serve(body, -1, contentType)
	require.Less(t, time.Since(started), 5*time.Second, "never waits for a slot")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	h.requireSpoolEmpty(t)

	t.Run("above the upstream cap, no disk at all", func(t *testing.T) {
		setBailianUploadSettingsForTest(t, newBailianUploadSettings(1024<<20, 1, filepath.Join(t.TempDir(), "missing")))

		body := newFormBody(t, nil, 70<<20, [][2]string{{"model", "paraformer-v2"}})
		w := h.serve(body.reader(), -1, body.contentType)
		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
		require.Equal(t, "model_after_large_file", gjson.Get(w.Body.String(), "error.code").String())
	})
}

// U5: the model field is matched without surrounding spaces, as the upstream handler reads it.
func TestCreateTranscription_ModelFieldIsTrimmed(t *testing.T) {
	h := newBailianUploadHarness(t)

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2\n"}, {"response_format", "text"}}, "a.wav", 30<<20)
	w := h.serve(body, length, contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "会议开始。会议结束。\n", w.Body.String())

	after := newFormBody(t, nil, 30<<20, [][2]string{{"model", " paraformer-v2 "}, {"response_format", "text"}})
	w = h.serve(after.reader(), -1, after.contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = h.form(t, [][2]string{{"model", "paraformer-v2\n"}, {"url", "https://media.example.com/a.mp3"}}, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	h.requireSpoolEmpty(t)
}

// U6: a spool failure tells the client what failed, not where the gateway spools.
func TestCreateTranscription_SpoolFailureKeepsThePathPrivate(t *testing.T) {
	h := newBailianUploadHarness(t)
	missing := filepath.Join(t.TempDir(), "spool-dir-that-is-missing")
	setBailianUploadSettingsForTest(t, newBailianUploadSettings(1024<<20, 1, missing))

	body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
	w := h.serve(body, length, contentType)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.Equal(t, "upload_spool_failed", gjson.Get(w.Body.String(), "error.code").String())
	require.Equal(t, "failed to store the uploaded audio on the gateway", gjson.Get(w.Body.String(), "error.message").String())
	require.NotContains(t, w.Body.String(), "spool-dir-that-is-missing")
	require.NotContains(t, w.Body.String(), "no such file")
}
