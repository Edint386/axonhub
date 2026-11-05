package api

// Regression tests for the 2026-11-05 review of the upload hook: requests for other models keep
// the upstream behaviour byte for byte, multipart forms cannot grow the heap, a buffered small
// body is released, the model may follow a large file, and signed audio URLs are not stored.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// upstream runs the upstream handler alone, as if the fork hook did not exist.
func (h *bailianUploadHarness) upstream(body io.Reader, length int64, contentType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(h.ctx)
	c.Request.ContentLength = length
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.TranscriptionHandlers.ChatCompletion(c)

	return w
}

// formBody builds a multipart body as a reusable stream: fields before the file, a WAV file of
// size pattern bytes after its header (no file when size < 0), then fields after the file.
type formBody struct {
	head, tail  []byte
	size        int64
	contentType string
	digest      string
}

func newFormBody(t *testing.T, before [][2]string, size int64, after [][2]string) *formBody {
	t.Helper()

	var head bytes.Buffer

	writer := multipart.NewWriter(&head)
	for _, field := range before {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}

	b := &formBody{size: size, contentType: writer.FormDataContentType()}

	var tail bytes.Buffer

	if size >= 0 {
		_, err := writer.CreateFormFile("file", "a.wav")
		require.NoError(t, err)

		head.Write(apiTestWAV(0))

		hash := sha256.New()
		hash.Write(apiTestWAV(0))
		_, err = io.Copy(hash, newPatternReader(size))
		require.NoError(t, err)
		b.digest = hex.EncodeToString(hash.Sum(nil))

		// The rest goes on after the file content: the fields after it, then the closing line.
		if len(after) == 0 {
			tail.WriteString("\r\n--" + writer.Boundary() + "--\r\n")
		} else {
			tail.WriteString("\r\n")

			// A fresh writer starts its first part with "--B\r\n" and closes with "\r\n--B--\r\n".
			tailWriter := multipart.NewWriter(&tail)
			require.NoError(t, tailWriter.SetBoundary(writer.Boundary()))

			for _, field := range after {
				require.NoError(t, tailWriter.WriteField(field[0], field[1]))
			}

			require.NoError(t, tailWriter.Close())
		}

		b.tail = tail.Bytes()
	} else {
		for _, field := range after {
			require.NoError(t, writer.WriteField(field[0], field[1]))
		}

		require.NoError(t, writer.Close())
	}

	b.head = head.Bytes()

	return b
}

func (b *formBody) reader() io.Reader {
	if b.size < 0 {
		return bytes.NewReader(b.head)
	}

	return io.MultiReader(bytes.NewReader(b.head), newPatternReader(b.size), bytes.NewReader(b.tail))
}

func (b *formBody) length() int64 {
	if b.size < 0 {
		return int64(len(b.head))
	}

	return int64(len(b.head)+len(b.tail)) + b.size
}

// rawParts is a multipart body with a model field and n header-less empty parts.
func rawParts(model string, n int) (string, string) {
	var b strings.Builder

	if model != "" {
		b.WriteString("--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\n" + model + "\r\n")
	}

	for range n {
		b.WriteString("--B\r\n\r\n\r\n")
	}

	b.WriteString("--B--\r\n")

	return b.String(), "multipart/form-data; boundary=B"
}

// TestCreateTranscription_OtherModelsBehaveExactlyAsUpstream holds every large-upload slot and
// points the spool at a missing directory: a request for another model must neither wait for a
// slot nor touch the spool, and must get exactly the upstream answer.
func TestCreateTranscription_OtherModelsBehaveExactlyAsUpstream(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 1, filepath.Join(t.TempDir(), "missing"))
	settings.queueWait = 5 * time.Second
	setBailianUploadSettingsForTest(t, settings)

	release, err := settings.acquire(t.Context(), false)
	require.NoError(t, err)

	defer release()

	model := [][2]string{{"model", "whisper-1"}}
	bigField := [][2]string{{"model", "whisper-1"}, {"prompt", strings.Repeat("p", 2<<20)}}

	cases := []struct {
		name    string
		body    *formBody
		chunked bool
	}{
		{"model first, 30 MiB file, known length", newFormBody(t, model, 30<<20, nil), false},
		{"model after a 30 MiB file, chunked", newFormBody(t, nil, 30<<20, model), true},
		{"chunked small file with a 2 MiB field", newFormBody(t, bigField, 1024, nil), true},
		{"2 MiB field before the model, chunked", newFormBody(t, [][2]string{{"prompt", strings.Repeat("p", 2<<20)}}, 1024, model), true},
		{"unknown model before a 70 MiB file, chunked", newFormBody(t, [][2]string{{"model", "whisper-large-v3"}}, 70<<20, nil), true},
		{"no file and no model, chunked", newFormBody(t, [][2]string{{"language", "zh"}}, -1, nil), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			length := tc.body.length()
			if tc.chunked {
				length = -1
			}

			started := time.Now()
			hooked := h.serve(tc.body.reader(), length, tc.body.contentType)
			require.Less(t, time.Since(started), 3*time.Second, "never waits for a large-upload slot")

			direct := h.upstream(tc.body.reader(), length, tc.body.contentType)
			require.Equal(t, direct.Code, hooked.Code, hooked.Body.String())
			require.Equal(t, direct.Body.String(), hooked.Body.String())
			require.NotEqual(t, http.StatusTooManyRequests, hooked.Code)
			require.NotContains(t, hooked.Body.String(), "upload_spool_failed")
		})
	}

	parts, contentType := rawParts("", 5000)
	hooked := h.serve(strings.NewReader(parts), -1, contentType)
	direct := h.upstream(strings.NewReader(parts), -1, contentType)
	require.Equal(t, direct.Code, hooked.Code)
	require.Equal(t, direct.Body.String(), hooked.Body.String(), "a form with many parts and no model goes to upstream unchanged")
}

// TestCreateTranscription_UnknownModelAboveTheUpstreamCapGetsTheUpstreamError covers the one case
// that has to touch the disk: a body above 64 MiB whose model is still unknown when it gets there.
func TestCreateTranscription_UnknownModelAboveTheUpstreamCapGetsTheUpstreamError(t *testing.T) {
	h := newBailianUploadHarness(t)
	body := newFormBody(t, nil, 70<<20, [][2]string{{"model", "whisper-1"}})

	hooked := h.serve(body.reader(), -1, body.contentType)
	direct := h.upstream(body.reader(), -1, body.contentType)
	require.Equal(t, direct.Code, hooked.Code)
	require.Equal(t, direct.Body.String(), hooked.Body.String())

	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_MultipartFormsAreBounded(t *testing.T) {
	h := newBailianUploadHarness(t)

	parts, contentType := rawParts("paraformer-v2", 5000)
	w := h.serve(strings.NewReader(parts), -1, contentType)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "too many multipart parts")

	longName := "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nparaformer-v2\r\n" +
		"--B\r\nContent-Disposition: form-data; name=\"" + strings.Repeat("n", 5<<20) + "\"\r\n\r\nx\r\n--B--\r\n"
	w = h.serve(strings.NewReader(longName), -1, contentType)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "multipart field too large")

	// Without a model the parser stops after maxMultipartParts parts and keeps the rest unread.
	parts, _ = rawParts("", 200000)
	counted := &countingReader{r: strings.NewReader(parts)}
	parser := newLargeUploadParser(newBodyRecorder(counted), "B", currentBailianUploadSettings(), &uploadSlot{}, -1)

	form, verdict, err := parser.parse()
	require.NoError(t, err)
	require.Equal(t, verdictUpstream, verdict)
	require.LessOrEqual(t, len(form.fields), maxMultipartParts)
	require.Less(t, counted.n.Load(), int64(len(parts)/10), "the form is handed over long before its end")

	// The small path leaves such a form to upstream as well.
	small, _ := rawParts("paraformer-v2", 2000)
	_, err = scanMultipartForm(strings.NewReader(small), "B")
	require.ErrorIs(t, err, errTooManyParts)
}

func TestCreateTranscription_SmallBodyBufferIsReleasedOnceRead(t *testing.T) {
	h := newBailianUploadHarness(t)
	body, contentType := buildForm(t, [][2]string{{"model", "whisper-1"}}, "a.wav", apiTestWAV(1600))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body)).WithContext(h.ctx)
	c.Request.Header.Set("Content-Type", contentType)

	h.handlers.CreateTranscription(c)

	released, ok := c.Request.Body.(*releasingBody)
	require.True(t, ok, "the hook hands the buffered body over, got %T", c.Request.Body)
	require.Nil(t, released.buf, "the upstream handler read it, so the hook's copy is gone")
}

func TestCreateTranscription_ModelAfterALargeFile(t *testing.T) {
	h := newBailianUploadHarness(t)
	model := [][2]string{{"model", "paraformer-v2"}, {"response_format", "text"}}

	// The whole body fits in what the upstream handler would read (64 MiB): the file read before
	// its model is still in the recording and goes to disk once the model is known.
	body := newFormBody(t, nil, 30<<20, model)

	w := h.serve(body.reader(), -1, body.contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "会议开始。会议结束。\n", w.Body.String())

	h.fake.mu.Lock()
	uploaded := h.fake.uploadSizes[len(h.fake.uploadSizes)-1]
	digest := h.fake.uploadDigests[len(h.fake.uploadDigests)-1]
	h.fake.mu.Unlock()

	require.Equal(t, int64(30<<20)+44, uploaded)
	require.Equal(t, body.digest, digest, "the file read before its model reaches Bailian intact")
	h.requireSpoolEmpty(t)

	// A larger body is not kept while its model is unknown (round 4: no slot or disk for a
	// request that may not be a Bailian one); the client is asked to send the model first.
	body = newFormBody(t, nil, 70<<20, model)

	w = h.serve(body.reader(), -1, body.contentType)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	require.Equal(t, "model_after_large_file", gjson.Get(w.Body.String(), "error.code").String())
	require.Contains(t, gjson.Get(w.Body.String(), "error.message").String(), "send the model field before the file")
	h.requireSpoolEmpty(t)

	// A small file before a Bailian model keeps the in-memory upstream path.
	small := newFormBody(t, nil, 3200, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}, {"response_format", "text"}})
	w = h.serve(small.reader(), -1, small.contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "你好。\n", w.Body.String())

	// A model that cannot take large audio still gets the upstream error for a file read first.
	large := newFormBody(t, nil, 30<<20, [][2]string{{"model", "qwen-audio-3.1-asr-flash"}})
	w = h.serve(large.reader(), -1, large.contentType)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, "invalid request: file too large", gjson.Get(w.Body.String(), "error.message").String())

	h.requireSpoolEmpty(t)
}

func TestCreateTranscription_SignedAudioURLsAreNotStored(t *testing.T) {
	h := newBailianUploadHarness(t)

	for _, tc := range []struct{ model, url string }{
		{"paraformer-v2", "https://bucket.oss.example.com/calls/a.mp3?Expires=1&OSSAccessKeyId=k&Signature=s3cr3t"},
		{"qwen-audio-3.1-asr-flash", "https://media.example.com/hello.wav?X-Amz-Signature=s3cr3t"},
	} {
		w := h.form(t, [][2]string{{"model", tc.model}, {"url", tc.url}}, "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	h.fake.mu.Lock()
	require.Contains(t, h.fake.submitBodies[0], "Signature=s3cr3t", "Bailian still gets the real URL")
	require.Contains(t, h.fake.flashBodies[0], "X-Amz-Signature=s3cr3t")
	h.fake.mu.Unlock()

	requests, err := h.client.Request.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, requests, 2)

	for _, request := range requests {
		require.NotContains(t, string(request.RequestBody), "s3cr3t")
		require.True(t, strings.HasSuffix(gjson.GetBytes(request.RequestBody, "url").String(), "?<redacted>"), string(request.RequestBody))
	}

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)

	for _, execution := range executions {
		require.NotContains(t, string(execution.RequestBody), "s3cr3t")
	}
}

// TestCreateTranscription_FileAfterAnOversizedSkippedPart reads a 70 MiB part the upstream parser
// skips (a file part not named "file") before the audio and the model: the recording has
// overflowed by then, so the audio goes to disk and still reaches Bailian intact.
func TestCreateTranscription_FileAfterAnOversizedSkippedPart(t *testing.T) {
	h := newBailianUploadHarness(t)
	audio := apiTestWAV(16000)

	build := func(model string) (io.Reader, string) {
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

		return io.MultiReader(bytes.NewReader(head.Bytes()), newPatternReader(70<<20), bytes.NewReader(tail.Bytes())),
			first.FormDataContentType()
	}

	body, contentType := build("paraformer-v2")
	w := h.serve(body, -1, contentType)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	sum := sha256.Sum256(audio)

	h.fake.mu.Lock()
	require.Equal(t, []string{hex.EncodeToString(sum[:])}, h.fake.uploadDigests)
	h.fake.mu.Unlock()

	body, contentType = build("whisper-1")
	hooked := h.serve(body, -1, contentType)
	body, contentType = build("whisper-1")
	direct := h.upstream(body, -1, contentType)
	require.Equal(t, direct.Code, hooked.Code)
	require.Equal(t, direct.Body.String(), hooked.Body.String())

	h.requireSpoolEmpty(t)
}
