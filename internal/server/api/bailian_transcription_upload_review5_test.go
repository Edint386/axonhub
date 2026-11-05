package api

// Regression tests for the round-4 regression check of the upload hook: a disk failure while a
// recorded file moves to the spool never reveals the spool path, and only large uploads (spools
// past the 26 MiB upstream cap) are charged against the spool budget, so a small request, chunked
// or not, is never refused with upload_storage_busy.

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
)

// copyRecordedFile moves a file held in the recording to disk (spoolMemoryFile, spoolUndecidedFile):
// when the gateway's disk fails under it, the client gets the fixed upload_spool_failed answer,
// never the spool file's path.
func TestCopyRecordedFile_DiskFailureKeepsThePathPrivate(t *testing.T) {
	h := newBailianUploadHarness(t)
	spoolDir := filepath.Join(t.TempDir(), "private-spool-dir")
	require.NoError(t, os.Mkdir(spoolDir, 0o700))

	settings := newBailianUploadSettings(1024<<20, 1, spoolDir)

	body, contentType := buildForm(t, [][2]string{{"response_format", "text"}}, "a.wav", apiTestWAV(16000))
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)

	raw := newBodyRecorder(bytes.NewReader(body))
	_, err = io.Copy(io.Discard, raw)
	require.NoError(t, err)

	p := newLargeUploadParser(raw, params["boundary"], settings, &uploadSlot{settings: settings, ctx: t.Context()}, int64(len(body)))

	// -1: the whole recorded file (spoolMemoryFile); 100: its first bytes (spoolUndecidedFile).
	for _, n := range []int64{-1, 100} {
		w, err := newSpoolWriter(spoolDir, "a.wav", io.Discard)
		require.NoError(t, err)
		require.NoError(t, w.tmp.Close()) // every write to the spool file now fails

		err = p.copyRecordedFile(w, n)
		w.abort()

		respErr, ok := err.(*llm.ResponseError)
		require.True(t, ok, "a disk failure is the gateway's: %T %v", err, err)
		require.Equal(t, http.StatusInternalServerError, respErr.StatusCode)
		require.Equal(t, "upload_spool_failed", respErr.Detail.Code)
		require.NotContains(t, err.Error(), spoolDir)

		// What the client receives.
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil).WithContext(h.ctx)
		h.handlers.writeUploadError(c, settings, err)

		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		require.Equal(t, "upload_spool_failed", gjson.Get(rec.Body.String(), "error.code").String())
		require.Equal(t, "failed to store the uploaded audio on the gateway", gjson.Get(rec.Body.String(), "error.message").String())
		require.NotContains(t, rec.Body.String(), "private-spool-dir")
		require.NotContains(t, rec.Body.String(), spoolFilePrefix)
		require.NotContains(t, rec.Body.String(), "already closed")
	}

	entries, err := os.ReadDir(spoolDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// Only a large upload is charged against the spool budget: with the budget full, a small request
// sent without a Content-Length (spooled to disk on the way) still goes through, while a large
// upload is still refused.
func TestCreateTranscription_SpoolBudgetOnlyChargesLargeUploads(t *testing.T) {
	h := newBailianUploadHarness(t)
	settings := newBailianUploadSettings(1024<<20, 2, h.spoolDir)
	settings.budget.limit = 64 << 20
	setBailianUploadSettingsForTest(t, settings)

	require.True(t, settings.budget.reserve(settings.budget.limit), "large jobs hold the whole budget")

	for _, model := range []string{"paraformer-v2", "qwen-audio-3.1-asr-flash"} {
		t.Run(model, func(t *testing.T) {
			small := newFormBody(t, [][2]string{{"model", model}, {"response_format", "text"}}, 32<<10, nil)

			w := h.serve(small.reader(), -1, small.contentType)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "upload_storage_busy")
			require.Equal(t, settings.budget.limit, settings.budget.inUse(), "the small spool took nothing of the budget")
			h.requireSpoolEmpty(t)
		})
	}

	// A spool of at most the upstream cap, however it is sent, is never charged.
	t.Run("at the upstream cap", func(t *testing.T) {
		atCap := newFormBody(t, [][2]string{{"model", "paraformer-v2"}, {"response_format", "text"}}, upstreamAudioFileCap-int64(len(apiTestWAV(0))), nil)

		w := h.serve(atCap.reader(), -1, atCap.contentType)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		h.requireSpoolEmpty(t)
	})

	t.Run("a large upload is still refused", func(t *testing.T) {
		body, length, contentType, _ := largeForm(t, [][2]string{{"model", "paraformer-v2"}}, "a.wav", 30<<20)

		w := h.serve(body, length, contentType)
		require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
		require.Equal(t, "upload_storage_busy", gjson.Get(w.Body.String(), "error.code").String())
		require.Equal(t, settings.budget.limit, settings.budget.inUse(), "the refused upload gave its bytes back")
		require.Empty(t, settings.slots)
		h.requireSpoolEmpty(t)
	})

	// A file read before its model, for a Bailian model without large uploads, followed by more
	// body: once the recording overflows it is answered "file too large" without being spooled
	// (spooling it would bypass the budget).
	t.Run("oversized file of a model without large uploads", func(t *testing.T) {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)

		go func() {
			part, err := mw.CreateFormFile("file", "a.wav")
			if err == nil {
				_, _ = part.Write(apiTestWAV(0))
				_, err = io.CopyN(part, zeroReader{}, 40<<20)
			}

			if err == nil {
				err = mw.WriteField("model", "qwen-audio-3.1-asr-flash")
			}

			if err == nil {
				var extra io.Writer

				extra, err = mw.CreateFormFile("extra", "b.bin")
				if err == nil {
					_, err = io.CopyN(extra, zeroReader{}, 30<<20)
				}
			}

			if err == nil {
				err = mw.Close()
			}

			_ = pw.CloseWithError(err)
		}()

		// The answer must not touch the disk at all: with no spool directory, spooling the file
		// would fail with upload_spool_failed instead.
		spoolDir := settings.spoolDir
		settings.spoolDir = filepath.Join(t.TempDir(), "missing")

		defer func() { settings.spoolDir = spoolDir }()

		w := h.serve(pr, -1, mw.FormDataContentType())
		_ = pr.Close()

		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, gjson.Get(w.Body.String(), "error.message").String(), "file too large")
		require.Equal(t, settings.budget.limit, settings.budget.inUse(), "nothing was charged or given back")
		require.Empty(t, settings.slots)
		h.requireSpoolEmpty(t)
	})

	settings.budget.release(settings.budget.limit)
	require.Zero(t, settings.budget.inUse())
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// The spool writer charges a large upload once it grows past the upstream cap, with all its
// bytes, and gives them back when the file is released.
func TestSpoolWriter_ChargesOnlyPastTheUpstreamCap(t *testing.T) {
	budget := &spoolBudget{limit: 64 << 20}

	w, err := newSpoolWriter(t.TempDir(), "a.wav", io.Discard)
	require.NoError(t, err)

	w.file.budget = budget

	chunk := bytes.Repeat([]byte{1}, 1<<20)
	for range upstreamAudioFileCap >> 20 {
		_, err = w.Write(chunk)
		require.NoError(t, err)
	}

	require.Zero(t, budget.inUse(), "nothing is charged up to the cap")

	_, err = w.Write(chunk)
	require.NoError(t, err)
	require.Equal(t, int64(upstreamAudioFileCap+1<<20), budget.inUse(), "past the cap the whole file counts")

	_, err = w.Write(chunk)
	require.NoError(t, err)
	require.Equal(t, w.file.size, budget.inUse())

	w.file.release()
	require.Zero(t, budget.inUse())

	// A spool without a budget (one that cannot grow past the cap) is never refused.
	uncharged, err := newSpoolWriter(t.TempDir(), "b.wav", io.Discard)
	require.NoError(t, err)

	defer uncharged.abort()

	_, err = uncharged.Write(chunk)
	require.NoError(t, err)
}
