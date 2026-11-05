package api

// Regression tests for the second 2026-11-05 review round of the upload hook: part headers are
// charged and not retained, spool files a dead process left behind are swept, a spool whose
// model is still unknown never waits for a slot nor becomes the answer for another model, built
// bodies are released once read, a declared length is not allocated up front, and a body cut
// short keeps its read error.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// repeatReader yields chunk n times without allocating the whole body.
type repeatReader struct {
	chunk []byte
	left  int
	off   int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}

	n := copy(p, r.chunk[r.off:])

	r.off += n
	if r.off == len(r.chunk) {
		r.off, r.left = 0, r.left-1
	}

	return n, nil
}

// paddedHeaderParts is a multipart body (boundary B): an optional model field, then n fields
// whose unquoted name sits in a Content-Disposition header padded to pad bytes.
func paddedHeaderParts(model string, n, pad int) io.Reader {
	head := ""
	if model != "" {
		head = "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n" + model + "\r\n"
	}

	part := "--B\r\nContent-Disposition: form-data; name=f; pad=" + strings.Repeat("A", pad) + "\r\n\r\nx\r\n"

	return io.MultiReader(strings.NewReader(head), &repeatReader{chunk: []byte(part), left: n}, strings.NewReader("--B--\r\n"))
}

func TestLargeUploadParser_PartHeadersAreChargedAndNotRetained(t *testing.T) {
	settings := newBailianUploadSettings(1024<<20, 2, t.TempDir())

	// 300 parts with 1 MiB headers: the header charge trips the fields cap after a few of them,
	// for a Bailian request and for one whose model is unknown alike.
	for _, model := range []string{"paraformer-v2", ""} {
		counted := &countingReader{r: paddedHeaderParts(model, 300, 1<<20)}
		parser := newLargeUploadParser(newBodyRecorder(counted), "B", settings, &uploadSlot{settings: settings}, -1)

		_, verdict, err := parser.parse()
		if model == "" {
			require.NoError(t, err)
			require.Equal(t, verdictUpstream, verdict, "a request that is not known to be Bailian goes to upstream")
		} else {
			require.Equal(t, verdictBailian, verdict)
			require.ErrorContains(t, err, "multipart field too large")
		}

		require.Less(t, counted.n.Load(), int64(8<<20), "the parse stops long before the 300 MiB body ends")
	}

	// Below the cap, a stored name keeps only its own bytes, not the header line it came from.
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	form := parsePaddedForm(t, settings, 3, 1280<<10)

	runtime.GC()
	runtime.ReadMemStats(&after)

	require.Equal(t, formFields{{"model", "paraformer-v2"}, {"f", "x"}, {"f", "x"}, {"f", "x"}}, form.fields)
	require.Less(t, int64(after.HeapAlloc)-int64(before.HeapAlloc), int64(1<<20),
		"three 1.25 MiB part headers must not stay alive through the field names")
	runtime.KeepAlive(form)
}

// parsePaddedForm parses a Bailian form with n padded-header fields; nothing but the form
// outlives it.
func parsePaddedForm(t *testing.T, settings *bailianUploadSettings, n, pad int) *spooledForm {
	t.Helper()

	var body bytes.Buffer

	_, err := io.Copy(&body, paddedHeaderParts("paraformer-v2", n, pad))
	require.NoError(t, err)

	parser := newLargeUploadParser(newBodyRecorder(&body), "B", settings, &uploadSlot{settings: settings}, -1)
	form, verdict, err := parser.parse()
	require.NoError(t, err)
	require.Equal(t, verdictBailian, verdict)

	return form
}

func TestCreateTranscription_StaleSpoolFilesAreSwept(t *testing.T) {
	h := newBailianUploadHarness(t)
	old := time.Now().Add(-3 * time.Hour)

	write := func(name string, mtime time.Time) string {
		path := filepath.Join(h.spoolDir, name)
		require.NoError(t, os.WriteFile(path, []byte("audio"), 0o600))
		require.NoError(t, os.Chtimes(path, mtime, mtime))

		return path
	}

	leftBehind := write(spoolFilePrefix+"left-behind", old)
	inFlight := write(spoolFilePrefix+"in-flight", time.Now())
	foreign := write("someone-else.tmp", old)
	dir := filepath.Join(h.spoolDir, spoolFilePrefix+"dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chtimes(dir, old, old))

	upload := func() {
		body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)
		w := h.serve(body, length, contentType)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	upload()
	require.NoFileExists(t, leftBehind, "a spool file a restarted or killed process left behind is removed")
	require.FileExists(t, inFlight, "a recent spool file may belong to a live request")
	require.FileExists(t, foreign, "only this hook's files are swept")
	require.DirExists(t, dir)

	// One sweep per interval: a file left behind meanwhile waits for the next one.
	later := write(spoolFilePrefix+"later", old)

	upload()
	require.FileExists(t, later)
}

func TestCreateTranscription_UndecidedSpoolNeverAnswersForOtherModels(t *testing.T) {
	h := newBailianUploadHarness(t)

	// fileThenModel is a chunked body whose model field comes after a file of size bytes.
	fileThenModel := func(model string, size int64) *formBody {
		return newFormBody(t, nil, size, [][2]string{{"model", model}})
	}

	sameAsUpstream := func(t *testing.T, body *formBody) {
		t.Helper()

		started := time.Now()
		hooked := h.serve(body.reader(), -1, body.contentType)
		require.Less(t, time.Since(started), 3*time.Second, "never waits for a large-upload slot")

		direct := h.upstream(body.reader(), -1, body.contentType)
		require.Equal(t, direct.Code, hooked.Code, hooked.Body.String())
		require.Equal(t, direct.Body.String(), hooked.Body.String())
		require.Empty(t, hooked.Header().Get("Retry-After"))
	}

	t.Run("every slot busy", func(t *testing.T) {
		settings := newBailianUploadSettings(1024<<20, 1, h.spoolDir)
		settings.queueWait = 5 * time.Second
		setBailianUploadSettingsForTest(t, settings)

		release, err := settings.acquire(t.Context(), false)
		require.NoError(t, err)

		defer release()

		sameAsUpstream(t, fileThenModel("whisper-1", 70<<20))
		h.requireSpoolEmpty(t)

		// The same body for a Bailian large-upload model is answered at once: a file above the
		// upstream cap whose model was unknown was dropped (round 4) without a slot or disk, so
		// the client is asked to send the model first.
		body := fileThenModel("paraformer-v2", 70<<20)
		started := time.Now()
		w := h.serve(body.reader(), -1, body.contentType)
		require.Less(t, time.Since(started), 3*time.Second)
		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
		require.Equal(t, "model_after_large_file", gjson.Get(w.Body.String(), "error.code").String())
		require.Empty(t, w.Header().Get("Retry-After"))
		h.requireSpoolEmpty(t)
	})

	t.Run("spool directory missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		setBailianUploadSettingsForTest(t, newBailianUploadSettings(1024<<20, 2, missing))

		sameAsUpstream(t, fileThenModel("whisper-1", 70<<20))

		// Only a file of at most the upstream cap is spooled before its model is known: one after
		// a part that filled the recording.
		body, contentType := fileAfterSkippedPart(t, 70<<20, apiTestWAV(16000), "paraformer-v2")
		w := h.serve(body, -1, contentType)
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "upload_spool_failed")
		require.NotContains(t, w.Body.String(), missing, "the spool path stays in the server log")
	})

	t.Run("file above the Bailian cap", func(t *testing.T) {
		setBailianUploadSettingsForTest(t, newBailianUploadSettings(66<<20, 2, h.spoolDir))

		sameAsUpstream(t, fileThenModel("whisper-1", 67<<20))
		h.requireSpoolEmpty(t)

		body := fileThenModel("paraformer-v2", 67<<20)
		w := h.serve(body.reader(), -1, body.contentType)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, gjson.Get(w.Body.String(), "error.message").String(), "at most 66 MiB")
		h.requireSpoolEmpty(t)
	})
}

// serveForRequest runs CreateTranscription and returns the request the handler ended with.
func (h *bailianUploadHarness) serveForRequest(body io.Reader, length int64, contentType string) (*httptest.ResponseRecorder, *http.Request) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(h.ctx)
	c.Request.ContentLength = length
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.CreateTranscription(c)

	return w, c.Request
}

func TestCreateTranscription_BuiltBodiesAreReleasedOnceRead(t *testing.T) {
	h := newBailianUploadHarness(t)

	requireReleased := func(t *testing.T, req *http.Request) {
		t.Helper()

		released, ok := req.Body.(*releasingBody)
		require.True(t, ok, "the hook hands its built body over, got %T", req.Body)
		require.Nil(t, released.buf, "the upstream handler read it, so the hook's copy is gone")
	}

	// A chunked flash upload is rebuilt for the upstream handler.
	body, contentType := buildForm(t, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}, {"response_format", "text"}}, "hi.wav", apiTestWAV(1600))
	w, req := h.serveForRequest(bytes.NewReader(body), -1, contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	requireReleased(t, req)

	// URL input continues with a placeholder body.
	body, contentType = buildForm(t, [][2]string{{"model", "paraformer-v2"}, {"url", "https://media.example.com/a.mp3"}}, "", nil)
	w, req = h.serveForRequest(bytes.NewReader(body), int64(len(body)), contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	requireReleased(t, req)
}

// cutReader returns data and then err.
type cutReader struct {
	data []byte
	err  error
}

func (r *cutReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}

	n := copy(p, r.data)
	r.data = r.data[n:]

	return n, nil
}

func TestReadDeclaredBody_GrowsWithWhatArrives(t *testing.T) {
	// A client that declares 26 MiB and sends ten bytes pins ten bytes, not 26 MiB.
	body, err := readDeclaredBody(&cutReader{data: []byte("0123456789"), err: io.ErrUnexpectedEOF}, upstreamAudioFileCap)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, "0123456789", string(body))
	require.LessOrEqual(t, cap(body), smallBodyFirstRead)

	full := bytes.Repeat([]byte("audio"), 60<<10)
	body, err = readDeclaredBody(bytes.NewReader(full), int64(len(full)))
	require.NoError(t, err)
	require.Equal(t, full, body)
	require.Equal(t, len(full), cap(body), "a complete body ends in a buffer of exactly its size")
}

// cutOffBody reads like a net/http request body whose client stopped before its declared end:
// the bytes it sent, then io.ErrUnexpectedEOF once, then a clean io.EOF on every later read.
type cutOffBody struct {
	r        io.Reader
	reported bool
}

func (b *cutOffBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) && !b.reported {
		b.reported = true
		return n, io.ErrUnexpectedEOF
	}

	return n, err
}

func TestCreateTranscription_CutOffBodyKeepsItsReadError(t *testing.T) {
	h := newBailianUploadHarness(t)

	full, contentType := buildForm(t, [][2]string{{"model", "whisper-1"}}, "a.wav", apiTestWAV(1<<20))
	fileFirst := newFormBody(t, nil, 2<<20, [][2]string{{"model", "whisper-1"}})

	var fileFirstBody bytes.Buffer

	_, err := io.Copy(&fileFirstBody, fileFirst.reader())
	require.NoError(t, err)

	cases := []struct {
		name        string
		sent        []byte
		length      int64
		contentType string
	}{
		{"known length, cut in the file", full[:len(full)/2], int64(len(full)), contentType},
		{"known length, cut after the closing boundary", full[:len(full)-2], int64(len(full)), contentType},
		{"chunked, cut in a file read before its model", fileFirstBody.Bytes()[:1<<20], -1, fileFirst.contentType},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hooked := h.serve(&cutOffBody{r: bytes.NewReader(tc.sent)}, tc.length, tc.contentType)
			direct := h.upstream(&cutOffBody{r: bytes.NewReader(tc.sent)}, tc.length, tc.contentType)

			require.Equal(t, direct.Code, hooked.Code, hooked.Body.String())
			require.Equal(t, direct.Body.String(), hooked.Body.String())
			require.Contains(t, hooked.Body.String(), "unexpected EOF", fmt.Sprintf("upstream reports the cut-off upload: %s", hooked.Body.String()))
		})
	}
}
