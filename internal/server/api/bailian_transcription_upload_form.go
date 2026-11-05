package api

// custom: the streaming multipart parser behind serveLargeBailianTranscription (see
// bailian_transcription_upload.go). A large or chunked body is only known to be a Bailian
// request once its model field has been read, and that field may come after the file (the Node
// SDK's order). Until then the parser records the raw body in memory, at most as much as the
// upstream handler would have read (64 MiB), so a request that turns out not to be a Bailian
// one is handed to the upstream handler byte for byte: no upload slot, no spool file and the
// same errors as without this hook. Only a body above that size whose model is still unknown
// touches the disk (it is too large for the upstream handler either way), and only for a file of
// at most the upstream cap (26 MiB), without an upload slot: a larger file whose model is still
// unknown is dropped (only its size is kept), and if its model then turns out to be a Bailian
// large-upload model the client is asked to send the model field before the file (413). A disk
// or size problem such a spool meets is only the answer once the model turns out to be a Bailian
// one (any other model gets the upstream error).
//
// Part names are copied out of their header (a substring would keep the whole header line
// alive) and every part header is charged against bailianUploadFieldsCap, so the heap a form
// can hold stays bounded whatever the client sends. The recording itself stops at
// recordingHardCap even where the parser has no chance to look at it (a preamble before the
// first part, which mime/multipart skips line by line inside NextPart).
//
// A spool file is unlinked as soon as it is created (where the OS allows it), so the disk space
// comes back when the request closes it, or when the process exits for any reason.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"sync"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	// upstreamRequestBodyCap is how much of a request body the upstream handler reads before it
	// refuses it (llm/httpclient/utils.go maxRequestBodySize).
	upstreamRequestBodyCap = 64 << 20

	// maxMultipartParts bounds the parts of a transcription form, as multipart.ReadForm does by
	// default; a model name and a few options need far fewer.
	maxMultipartParts = 1000

	// multipartPartOverhead is charged per part against bailianUploadFieldsCap on top of its
	// name, so many empty parts cannot grow the heap past the cap either.
	multipartPartOverhead = 256

	recordChunkSize = 256 << 10

	// recordingHardCap is where the recording stops by itself. The parser checks it against
	// upstreamRequestBodyCap after every field and file chunk; in between it can grow by at most
	// a part header (mime/multipart allows 10 MiB) and a field (1 MiB), which the margin covers.
	recordingHardCap = upstreamRequestBodyCap + 16<<20
)

// errRecordingCap fails the read that would grow the recording past recordingHardCap: a body
// that long without a checkpoint (a huge preamble) is too large for the upstream handler, and
// the parse ends as such (verdictTooLarge).
var errRecordingCap = errors.New("multipart body too large before its first part")

// errUpstreamBodyTooLarge is what the upstream handler answers for a body above
// upstreamRequestBodyCap (httpclient.ReadHTTPRequest).
var errUpstreamBodyTooLarge = fmt.Errorf("failed to read request body: %w", httpclient.ErrRequestBodyTooLarge)

// bodyRecorder keeps a copy of everything read through it while recording, in fixed-size chunks
// so growing it never copies what it already holds.
type bodyRecorder struct {
	r         io.Reader
	chunks    [][]byte
	size      int64
	recording bool
	// overflowed reports that recording stopped because the body outgrew upstreamRequestBodyCap.
	overflowed bool
	// err is the first error (io.EOF included) reading the body returned.
	err error
}

func newBodyRecorder(r io.Reader) *bodyRecorder {
	return &bodyRecorder{r: r, recording: true}
}

func (b *bodyRecorder) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 && b.recording {
		if b.size+int64(n) > recordingHardCap {
			b.overflow()
			return n, errRecordingCap
		}

		b.record(p[:n])
	}

	if err != nil && b.err == nil {
		b.err = err
	}

	return n, err
}

func (b *bodyRecorder) record(p []byte) {
	b.size += int64(len(p))

	for len(p) > 0 {
		if len(b.chunks) == 0 || len(b.chunks[len(b.chunks)-1]) == recordChunkSize {
			b.chunks = append(b.chunks, make([]byte, 0, recordChunkSize))
		}

		last := &b.chunks[len(b.chunks)-1]
		k := min(len(p), recordChunkSize-len(*last))
		*last = append(*last, p[:k]...)
		p = p[k:]
	}
}

// full reports a recording larger than the upstream handler would read.
func (b *bodyRecorder) full() bool {
	return b.recording && b.size > upstreamRequestBodyCap
}

// stop drops the recording.
func (b *bodyRecorder) stop() {
	b.recording = false
	b.chunks = nil
}

func (b *bodyRecorder) overflow() {
	b.stop()
	b.overflowed = true
}

// snapshot reads the recording without consuming it.
func (b *bodyRecorder) snapshot() io.Reader {
	readers := make([]io.Reader, 0, len(b.chunks))
	for _, chunk := range b.chunks {
		readers = append(readers, bytes.NewReader(chunk))
	}

	return io.MultiReader(readers...)
}

// replay hands the recording over: the reader releases every chunk once it was read.
func (b *bodyRecorder) replay() io.Reader {
	r := &chunkReader{chunks: b.chunks}
	b.chunks = nil
	b.recording = false

	return r
}

// handOver is the body the upstream handler reads instead of original: the recording, then
// what the client sent after it, or the error reading it met there. A net/http body reports a
// truncated upload once and reads as a clean EOF afterwards, so reading original again would
// hand upstream a truncated body that looks complete.
func (b *bodyRecorder) handOver(original io.ReadCloser) io.ReadCloser {
	var rest io.Reader = original
	if b.err != nil {
		rest = errReader{err: b.err}
	}

	return readCloser{io.MultiReader(b.replay(), rest), original}
}

// errReader returns err on every read.
type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && len(r.chunks[0]) == 0 {
		r.chunks[0] = nil
		r.chunks = r.chunks[1:]
	}

	if len(r.chunks) == 0 {
		r.chunks = nil
		return 0, io.EOF
	}

	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]

	return n, nil
}

// releasingBody serves a buffered request body once and drops it at EOF, so the copy is not
// kept alive for the rest of the request after the upstream handler read it.
type releasingBody struct {
	buf []byte
}

func (b *releasingBody) Read(p []byte) (int, error) {
	if len(b.buf) == 0 {
		b.buf = nil
		return 0, io.EOF
	}

	n := copy(p, b.buf)

	b.buf = b.buf[n:]
	if len(b.buf) == 0 {
		b.buf = nil
	}

	return n, nil
}

func (b *releasingBody) Close() error {
	b.buf = nil
	return nil
}

// largeVerdict is what a parsed large body turned out to be.
type largeVerdict int

const (
	// verdictBailian: the model is a Bailian one; the form holds what bailianasr needs.
	verdictBailian largeVerdict = iota + 1
	// verdictUpstream: not a Bailian request (or not one this parser can tell); replay the
	// recording to the upstream handler.
	verdictUpstream
	// verdictTooLarge: not known to be a Bailian request and larger than the upstream handler
	// reads, so the upstream answer is errUpstreamBodyTooLarge.
	verdictTooLarge
)

type spooledForm struct {
	fields formFields
	// file is the "file" part on disk.
	file *spooledFile
	// memoryFile is the size of a "file" part still only held in the recording (its model was
	// unknown while it was read); -1 when there is none.
	memoryFile     int64
	memoryFileName string
	once           sync.Once
}

type spooledFile struct {
	// f is the open spool file, read through ReadAt; path is its name, empty once it was unlinked
	// (see newSpoolWriter).
	f      *os.File
	path   string
	name   string
	size   int64
	digest string

	// reserved is what the file took of budget (see spoolBudget and spoolWriter.charge); budget
	// is nil for a spool that is never charged.
	budget   *spoolBudget
	reserved int64
}

// reader reads the whole spooled file from the start.
func (f *spooledFile) reader() io.Reader {
	return io.NewSectionReader(f.f, 0, f.size)
}

// release closes the spool file (which frees an unlinked one), removes it if it still has a
// name and gives its bytes back to the spool budget; it is safe to call more than once.
func (f *spooledFile) release() {
	if f.f != nil {
		_ = f.f.Close()
	}

	if f.path != "" {
		_ = os.Remove(f.path)
		f.path = ""
	}

	f.budget.release(f.reserved)
	f.reserved = 0
}

func (f *spooledForm) first(name string) string { return f.fields.first(name) }

func (f *spooledForm) model() string { return f.fields.model() }

func (f *spooledForm) audioURL() string { return f.fields.audioURL() }

func (f *spooledForm) stream() bool { return f.fields.stream() }

func (f *spooledForm) hasFile() bool { return f.file != nil || f.memoryFile >= 0 }

// cleanup releases the spooled file; it is safe to call more than once.
func (f *spooledForm) cleanup() {
	f.once.Do(func() {
		if f.file != nil {
			f.file.release()
		}
	})
}

// largeUploadParser parses a multipart body as a stream (see the file comment).
type largeUploadParser struct {
	settings      *bailianUploadSettings
	slot          *uploadSlot
	raw           *bodyRecorder
	boundary      string
	reader        *multipart.Reader
	contentLength int64
	form          *spooledForm
	// watch (when set) is armed once a file is spooled; see bodyWatch.
	watch *bodyWatch

	// decided is set once the model field was read; bailian tells whether it names a model of
	// bailianasr.
	decided bool
	bailian bool

	parts      int
	fieldBytes int

	// deferred is why a file whose model was still unknown was dropped: it outgrew the upstream
	// cap (errModelAfterLargeFile), or a disk or budget problem met while spooling it. It is the
	// answer only if the model turns out to be a Bailian one (see dropUndecidedFile).
	deferred error
	// droppedFile reports a "file" part dropped that way, and droppedSize its size.
	droppedFile bool
	droppedSize int64
}

func newLargeUploadParser(raw *bodyRecorder, boundary string, settings *bailianUploadSettings, slot *uploadSlot, contentLength int64) *largeUploadParser {
	return &largeUploadParser{
		settings:      settings,
		slot:          slot,
		raw:           raw,
		boundary:      boundary,
		reader:        multipart.NewReader(raw, boundary),
		contentLength: contentLength,
		form:          &spooledForm{memoryFile: -1},
	}
}

// parse reads the whole form (or as much as it takes to know it is not a Bailian request). The
// returned error only matters with verdictBailian. The form is never nil, so its cleanup can
// always be deferred.
func (p *largeUploadParser) parse() (*spooledForm, largeVerdict, error) {
	verdict, err := p.run()
	return p.form, verdict, err
}

func (p *largeUploadParser) run() (largeVerdict, error) {
	for {
		part, err := p.reader.NextPart()
		if errors.Is(err, io.EOF) {
			return p.atEOF()
		}

		if err != nil {
			return p.fail(multipartReadError(err, p.settings))
		}

		// Copies: the parsed values are substrings of the part header, which can be megabytes.
		name, filename := strings.Clone(part.FormName()), strings.Clone(part.FileName())

		p.parts++
		p.fieldBytes += headerBytes(part.Header) + multipartPartOverhead

		if p.parts > maxMultipartParts {
			return p.fail(fmt.Errorf("%w: too many multipart parts", transformer.ErrInvalidRequest))
		}

		if p.fieldBytes > bailianUploadFieldsCap {
			return p.fail(fmt.Errorf("%w: multipart field too large", transformer.ErrInvalidRequest))
		}

		var verdict largeVerdict

		switch {
		case filename == "":
			verdict, err = p.readField(part, name)
		case name == "file":
			verdict, err = p.readFile(part, filename)
		default:
			verdict, err = p.skip(part)
		}

		if verdict != 0 || err != nil {
			return verdict, err
		}
	}
}

// headerBytes is the size of a part header as sent.
func headerBytes(header textproto.MIMEHeader) int {
	n := 0

	for key, values := range header {
		for _, value := range values {
			n += len(key) + len(value) + len(": \r\n")
		}
	}

	return n
}

// fail ends the parse on err: a Bailian request gets err, any other request goes to the upstream
// handler, which then reports the problem itself.
func (p *largeUploadParser) fail(err error) (largeVerdict, error) {
	if p.decided && p.bailian {
		return verdictBailian, err
	}

	return p.handOff()
}

// handOff passes a request that is not (known to be) a Bailian one to the upstream handler.
func (p *largeUploadParser) handOff() (largeVerdict, error) {
	if p.raw.overflowed {
		return verdictTooLarge, nil
	}

	return verdictUpstream, nil
}

// checkSize reacts to a recording that outgrew what the upstream handler reads.
func (p *largeUploadParser) checkSize() (largeVerdict, error) {
	if !p.raw.full() {
		return 0, nil
	}

	// Too large for the upstream handler and, by its length, for Bailian as well.
	if p.contentLength > maxLargeUploadBody(p.settings) {
		return verdictTooLarge, nil
	}

	if p.form.memoryFile >= 0 {
		size := p.form.memoryFile

		switch {
		case !p.decided && size > upstreamAudioFileCap:
			// Too large to keep while its model is unknown (see dropUndecidedFile).
			if verdict, err := p.dropUndecidedFile(nil, size, nil, nil); verdict != 0 || err != nil {
				return verdict, err
			}
		case size > upstreamAudioFileCap && !bailianasr.SupportsLargeUpload(p.form.model()):
			// A Bailian model without large uploads cannot take such a file at all: answer as the
			// upstream handler would, without spooling it (it would bypass the spool budget).
			return verdictBailian, fmt.Errorf("%w: file too large", transformer.ErrInvalidRequest)
		default:
			// A finished "file" part only held in the recording goes to disk first.
			if err := p.spoolMemoryFile(); err != nil {
				if p.decided {
					return verdictBailian, err
				}

				if verdict, err := p.dropUndecidedFile(nil, size, nil, err); verdict != 0 || err != nil {
					return verdict, err
				}
			}
		}
	}

	p.raw.overflow()

	return 0, nil
}

func (p *largeUploadParser) readField(part io.Reader, name string) (largeVerdict, error) {
	value, err := io.ReadAll(io.LimitReader(part, bailianUploadFieldCap+1))
	if err != nil {
		return p.fail(multipartReadError(err, p.settings))
	}

	p.fieldBytes += len(value)
	if len(value) > bailianUploadFieldCap || p.fieldBytes > bailianUploadFieldsCap {
		return p.fail(fmt.Errorf("%w: multipart field too large", transformer.ErrInvalidRequest))
	}

	p.form.fields = append(p.form.fields, formField{name: name, value: string(value)})

	if name == "model" && !p.decided {
		model := strings.TrimSpace(string(value))
		p.decided = true
		p.bailian = bailianasr.SupportsModel(model)

		if !p.bailian {
			return p.handOff()
		}

		// The API key's model list is checked before any slot or disk is spent on the request.
		if err := checkUploadModelAccess(p.slot.ctx, model); err != nil {
			return verdictBailian, err
		}

		if p.deferred != nil {
			return verdictBailian, p.droppedFileError(model)
		}

		// The recording is only needed for a file read before the model was known.
		if p.form.memoryFile < 0 {
			p.raw.stop()
		}
	}

	return p.checkSize()
}

// skip drains a file part other than "file", as the upstream parser does.
func (p *largeUploadParser) skip(part io.Reader) (largeVerdict, error) {
	buf := make([]byte, 32<<10)

	for {
		_, err := part.Read(buf)

		if verdict, sizeErr := p.checkSize(); verdict != 0 || sizeErr != nil {
			return verdict, sizeErr
		}

		if errors.Is(err, io.EOF) {
			return 0, nil
		}

		if err != nil {
			return p.fail(multipartReadError(err, p.settings))
		}
	}
}

func (p *largeUploadParser) readFile(part io.Reader, filename string) (largeVerdict, error) {
	if p.form.hasFile() || p.droppedFile {
		return p.fail(fmt.Errorf("%w: multiple file parts are not allowed", transformer.ErrInvalidRequest))
	}

	if !p.decided && !p.raw.overflowed {
		return p.readUndecidedFile(part, filename)
	}

	if !p.decided {
		// The body already outgrew what the upstream handler reads. A file of at most the
		// upstream cap goes to disk without an upload slot (a Bailian model may still take it);
		// a larger one is dropped, since nothing is known yet that would justify its slot and
		// disk.
		tracked := &readTracker{r: part}

		file, err := p.spoolPart(tracked, filename, upstreamAudioFileCap, false)
		p.form.file = file

		switch {
		case errors.Is(err, errSpoolLimit):
			return p.dropUndecidedFile(part, tracked.n, nil, nil)
		case err != nil:
			return p.dropUndecidedFile(part, tracked.n, tracked.err, err)
		}

		return 0, nil
	}

	// A Bailian model named before the file. One that cannot take large audio keeps the
	// upstream cap, so such a request is turned down without spooling a whole oversized file;
	// its file (at most that cap) needs no upload slot, so a busy slot never answers it.
	model := p.form.model()
	limit, tooLarge, gated := p.settings.maxBytes, bailianFileTooLarge(p.settings), true

	if !bailianasr.SupportsLargeUpload(model) {
		limit, tooLarge, gated = upstreamAudioFileCap, fmt.Errorf("%w: file too large", transformer.ErrInvalidRequest), false
	} else if p.contentLength > maxLargeUploadBody(p.settings) {
		return verdictBailian, tooLarge
	}

	file, err := p.spoolPart(part, filename, limit, gated)
	p.form.file = file

	switch {
	case errors.Is(err, errSpoolLimit):
		return verdictBailian, tooLarge
	case err != nil:
		return verdictBailian, multipartReadError(err, p.settings)
	}

	return 0, nil
}

// errModelAfterLargeFile answers a Bailian large-upload request whose file came before its model
// field and was larger than the upstream cap: the hook does not keep (or take a slot and disk
// for) such a file before it knows the model.
var errModelAfterLargeFile = &llm.ResponseError{
	StatusCode: http.StatusRequestEntityTooLarge,
	Detail: llm.ErrorDetail{
		Code: "model_after_large_file",
		Type: "invalid_request_error",
		Message: "the file is larger than 26 MiB and came before the model field; send the model field before the file " +
			"(the OpenAI Python SDK does) to upload a large file for this model",
	},
}

// dropUndecidedFile drops a "file" part whose model is not known yet: it outgrew the upstream
// cap (cause nil), or spooling it met a disk or budget problem (cause). Whatever was spooled or
// recorded of it is released and the rest of the part (part may be nil when it was read to its
// end) is read only to count its size, so the parse goes on to the model field and a request for
// another model still gets the upstream error, never one about Bailian uploads. A failure
// reading the client's body (readErr, or one met while counting) ends the parse as for any other
// request.
func (p *largeUploadParser) dropUndecidedFile(part io.Reader, read int64, readErr, cause error) (largeVerdict, error) {
	if p.form.file != nil {
		p.form.file.release()
		p.form.file = nil
	}

	p.form.memoryFile = -1
	p.droppedFile = true
	p.raw.overflow()

	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return p.fail(multipartReadError(readErr, p.settings))
	}

	if part != nil {
		rest, err := io.Copy(io.Discard, part)
		read += rest

		if err != nil {
			return p.fail(multipartReadError(err, p.settings))
		}
	}

	p.droppedSize = read

	if cause == nil || read > upstreamAudioFileCap {
		cause = errModelAfterLargeFile
	}

	p.deferred = cause

	return 0, nil
}

// droppedFileError is the answer for a Bailian model whose file was dropped before the model was
// known (see dropUndecidedFile).
func (p *largeUploadParser) droppedFileError(model string) error {
	switch {
	case p.droppedSize > upstreamAudioFileCap && !bailianasr.SupportsLargeUpload(model):
		// The upstream answer: the model cannot take such a file at all.
		return fmt.Errorf("%w: file too large", transformer.ErrInvalidRequest)
	case p.droppedSize > p.settings.maxBytes:
		return bailianFileTooLarge(p.settings)
	default:
		return p.deferred
	}
}

// readTracker remembers how much was read through it and the first error reading r returned.
type readTracker struct {
	r   io.Reader
	n   int64
	err error
}

func (t *readTracker) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	t.n += int64(n)

	if err != nil && t.err == nil {
		t.err = err
	}

	return n, err
}

// readUndecidedFile reads a "file" part whose model is not known yet. Its bytes stay in the
// recording; only when the body outgrows what the upstream handler reads does the file move
// to disk and continue there.
func (p *largeUploadParser) readUndecidedFile(part io.Reader, filename string) (largeVerdict, error) {
	buf := make([]byte, 32<<10)

	var size int64

	for {
		n, err := part.Read(buf)
		size += int64(n)

		if p.raw.full() {
			if p.contentLength > maxLargeUploadBody(p.settings) {
				return verdictTooLarge, nil
			}

			if size > upstreamAudioFileCap {
				return p.dropUndecidedFile(part, size, nil, nil)
			}

			tracked := &readTracker{r: part}
			if err := p.spoolUndecidedFile(tracked, filename, size); err != nil {
				if errors.Is(err, errSpoolLimit) {
					return p.dropUndecidedFile(part, size+tracked.n, nil, nil)
				}

				return p.dropUndecidedFile(part, size+tracked.n, tracked.err, err)
			}

			return 0, nil
		}

		if errors.Is(err, io.EOF) {
			p.form.memoryFile, p.form.memoryFileName = size, filename
			return 0, nil
		}

		if err != nil {
			return p.fail(multipartReadError(err, p.settings))
		}
	}
}

// spoolUndecidedFile writes the read bytes of the "file" part from the recording to disk, drops
// the recording and spools the rest of the part, without an upload slot and up to the upstream
// cap (errSpoolLimit beyond it).
func (p *largeUploadParser) spoolUndecidedFile(part io.Reader, filename string, read int64) error {
	w, err := p.newSpoolWriter(filename, false, false)
	if err != nil {
		return err
	}

	p.form.file = w.file

	if err := p.copyRecordedFile(w, read); err != nil {
		w.abort()
		return err
	}

	p.raw.overflow()

	return w.finish(part, upstreamAudioFileCap, errSpoolLimit, p.settings)
}

// spoolMemoryFile moves a finished "file" part from the recording to disk and drops the
// recording. Only then does it take the upload slot the file needs (above the upstream cap, for a
// large-upload model): waiting for a slot must not hold up to 64 MiB of recording, and the file
// on disk is no larger than that. A file whose model is not known yet never gets here above the
// upstream cap (see checkSize).
func (p *largeUploadParser) spoolMemoryFile() error {
	// Only a large upload is charged against the spool budget (see spoolWriter.charge), so a file
	// a model without large uploads gets is never refused for it. Such a model never gets here with
	// a file above the upstream cap (checkSize answers "file too large" first).
	large := p.decided && bailianasr.SupportsLargeUpload(p.form.model())

	w, err := p.newSpoolWriter(p.form.memoryFileName, false, large)
	if err != nil {
		return err
	}

	p.form.file = w.file
	p.form.memoryFile = -1

	if err := p.copyRecordedFile(w, -1); err != nil {
		w.abort()
		return err
	}

	p.raw.stop()

	if err := w.finish(eofReader{}, p.settings.maxBytes, bailianFileTooLarge(p.settings), p.settings); err != nil {
		return err
	}

	if large && w.file.size > upstreamAudioFileCap {
		return p.slot.acquire()
	}

	return nil
}

// copyRecordedFile parses the recording again and copies the first n bytes (all of them for a
// negative n) of its "file" part to w. The recording holds every byte the live parser read, so
// this parse delivers at least what the live one did.
func (p *largeUploadParser) copyRecordedFile(w *spoolWriter, n int64) error {
	reader := multipart.NewReader(p.raw.snapshot(), p.boundary)

	for {
		part, err := reader.NextPart()
		if err != nil {
			return fmt.Errorf("failed to re-read the recorded upload: %w", err)
		}

		if part.FileName() == "" || part.FormName() != "file" {
			continue
		}

		var src io.Reader = part
		if n >= 0 {
			src = io.LimitReader(part, n)
		}

		copied, err := io.Copy(w, src)
		if err != nil {
			// A failure of the gateway's disk names the spool file: the client never sees it.
			if _, ok := errors.AsType[*os.PathError](err); ok {
				return spoolError(err)
			}

			return err
		}

		if n >= 0 && copied != n {
			return fmt.Errorf("re-read %d of %d recorded upload bytes", copied, n)
		}

		return nil
	}
}

// gate takes the upload slot once a spooled file of a large-upload model grows past the upstream
// cap (a file whose model is still unknown is never gated: it never gets that large).
func (p *largeUploadParser) gate() io.Writer {
	return &slotGate{slot: p.slot, threshold: upstreamAudioFileCap}
}

// newSpoolWriter starts a spool file (sweeping stale ones first, see sweepSpool). Gated writes go
// through the upload-slot gate; a charged spool (a large upload, which may grow past the
// upstream cap) takes its bytes from the spool budget once it does (see spoolWriter.charge).
// From here on the body reads are bounded (see bodyWatch).
func (p *largeUploadParser) newSpoolWriter(filename string, gated, charged bool) (*spoolWriter, error) {
	p.watch.arm()
	p.settings.sweepSpool(p.slot.ctx)

	gate := io.Discard
	if gated {
		gate = p.gate()
	}

	w, err := newSpoolWriter(p.settings.spoolDir, filename, gate)
	if err != nil {
		return nil, err
	}

	if charged {
		w.file.budget = p.settings.budget
	}

	return w, nil
}

// spoolPart writes one whole file part to a new spool file (at most limit bytes). A gated part
// (a large upload) is also charged against the spool budget.
func (p *largeUploadParser) spoolPart(part io.Reader, filename string, limit int64, gated bool) (*spooledFile, error) {
	w, err := p.newSpoolWriter(filename, gated, gated)
	if err != nil {
		return nil, err
	}

	return spoolPart(part, limit, w)
}

func (p *largeUploadParser) atEOF() (largeVerdict, error) {
	if p.decided && p.bailian {
		return verdictBailian, nil
	}

	// No model field, or one after everything: decide now.
	return p.handOff()
}

// spoolForLargeUpload moves a file held in the recording to disk when a large-upload model needs
// it as a file source (the recording is dropped before the slot wait).
func (p *largeUploadParser) spoolForLargeUpload() error {
	if p.form.memoryFile > p.settings.maxBytes {
		return bailianFileTooLarge(p.settings)
	}

	err := p.spoolMemoryFile()
	p.raw.stop()

	return err
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// maxLargeUploadBody is the largest body a Bailian large upload can have.
func maxLargeUploadBody(settings *bailianUploadSettings) int64 {
	return settings.maxBytes + bailianUploadFieldsCap + 1<<20
}

var errSpoolLimit = errors.New("spool limit exceeded")

// spoolWriter writes one file part to a temp file while hashing it; writes also go through the
// upload-slot gate.
type spoolWriter struct {
	file *spooledFile
	tmp  *os.File
	hash hash.Hash
	w    io.Writer
}

// newSpoolWriter creates the spool file and unlinks it at once where the OS allows it (not on
// Windows, which keeps the name until the cleanup removes it): the open file keeps its data for
// the request, and the space comes back when it is closed, also when a restart, crash or kill
// ends the process before its cleanup ran.
func newSpoolWriter(dir, filename string, gate io.Writer) (*spoolWriter, error) {
	tmp, err := os.CreateTemp(dir, spoolFilePrefix+"*")
	if err != nil {
		return nil, spoolError(err)
	}

	file := &spooledFile{f: tmp, path: tmp.Name(), name: filename}
	if os.Remove(tmp.Name()) == nil {
		file.path = ""
	}

	w := &spoolWriter{file: file, tmp: tmp, hash: sha256.New()}
	w.w = io.MultiWriter(gate, tmp, w.hash)

	return w, nil
}

func (w *spoolWriter) Write(p []byte) (int, error) {
	if err := w.charge(int64(len(p))); err != nil {
		return 0, err
	}

	n, err := w.w.Write(p)
	w.file.size += int64(n)

	return n, err
}

// charge takes a write of n bytes from the spool budget. Only a charged spool (a large upload)
// counts, and only once it grows past the upstream cap: then with all its bytes. A file of at
// most that cap is rebuilt in memory for the upstream handler anyway (see rebuildMultipart), so a
// small request, chunked or not, is never refused because large uploads fill the budget.
func (w *spoolWriter) charge(n int64) error {
	if w.file.budget == nil {
		return nil
	}

	total := w.file.size + n
	if total <= upstreamAudioFileCap {
		return nil
	}

	need := total - w.file.reserved
	if need <= 0 {
		return nil
	}

	if !w.file.budget.reserve(need) {
		return errSpoolBudget
	}

	w.file.reserved += need

	return nil
}

// errSpoolBudget refuses a write that would take the spool files past
// AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB in all.
var errSpoolBudget = &llm.ResponseError{
	StatusCode: http.StatusTooManyRequests,
	Detail: llm.ErrorDetail{
		Code:    "upload_storage_busy",
		Type:    "rate_limit_error",
		Message: "the gateway is holding too many large transcription uploads on disk right now; retry later",
	},
}

func (w *spoolWriter) abort() {
	w.file.release()
}

// finish copies the rest of src (the file may hold at most limit bytes in all) and records the
// digest. The file stays open for the request (see spooledFile.release).
func (w *spoolWriter) finish(src io.Reader, limit int64, tooLarge error, settings *bailianUploadSettings) error {
	_, copyErr := io.Copy(w, io.LimitReader(src, limit+1-w.file.size))

	switch {
	case copyErr != nil:
		if _, ok := errors.AsType[*os.PathError](copyErr); ok {
			return spoolError(copyErr)
		}

		return multipartReadError(copyErr, settings)
	case w.file.size > limit:
		return tooLarge
	}

	w.file.digest = hex.EncodeToString(w.hash.Sum(nil))

	return nil
}

func spoolPart(part io.Reader, limit int64, w *spoolWriter) (*spooledFile, error) {
	_, copyErr := io.Copy(w, io.LimitReader(part, limit+1))

	switch {
	case copyErr != nil:
		if _, ok := errors.AsType[*os.PathError](copyErr); ok {
			return w.file, spoolError(copyErr)
		}

		return w.file, copyErr
	case w.file.size > limit:
		return w.file, errSpoolLimit
	}

	w.file.digest = hex.EncodeToString(w.hash.Sum(nil))

	return w.file, nil
}

// spoolError reports a failure of the gateway's own disk. The client gets a fixed message: the
// cause names the spool directory and the state of the disk, which stay in the server log.
func spoolError(err error) error {
	log.Error(context.Background(), "bailian transcription upload: failed to store the uploaded audio", log.Cause(err))

	return &llm.ResponseError{
		StatusCode: http.StatusInternalServerError,
		Detail: llm.ErrorDetail{
			Code:    "upload_spool_failed",
			Type:    "api_error",
			Message: "failed to store the uploaded audio on the gateway",
		},
	}
}

func multipartReadError(err error, settings *bailianUploadSettings) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return bailianFileTooLarge(settings)
	}

	if slow, ok := errors.AsType[*errBodyTooSlow](err); ok {
		return &llm.ResponseError{
			StatusCode: http.StatusRequestTimeout,
			Detail: llm.ErrorDetail{
				Code: "request_body_too_slow",
				Type: "invalid_request_error",
				Message: fmt.Sprintf("the client sent less than %d KiB of request body within %s; the upload was abandoned",
					slow.minBytes>>10, slow.window),
			},
			Cause: os.ErrDeadlineExceeded,
		}
	}

	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The read deadline of bodyWatch: no body data for the idle time (or the request deadline,
		// which writeUploadError reports instead).
		return &llm.ResponseError{
			StatusCode: http.StatusRequestTimeout,
			Detail: llm.ErrorDetail{
				Code:    "request_body_timeout",
				Type:    "invalid_request_error",
				Message: fmt.Sprintf("the client sent no request body data for %s; the upload was abandoned", settings.bodyIdle),
			},
			Cause: os.ErrDeadlineExceeded,
		}
	}

	if _, ok := errors.AsType[*llm.ResponseError](err); ok {
		return err
	}

	return fmt.Errorf("%w: failed to read multipart", transformer.ErrInvalidRequest)
}

type scannedForm struct {
	fields  formFields
	hasFile bool
}

func (f *scannedForm) first(name string) string { return f.fields.first(name) }

func (f *scannedForm) model() string { return f.fields.model() }

func (f *scannedForm) audioURL() string { return f.fields.audioURL() }

func (f *scannedForm) stream() bool { return f.fields.stream() }

var errTooManyParts = errors.New("too many multipart parts")

// scanMultipartForm reads the fields of an in-memory multipart body and notes whether it has a
// file part named "file", without copying the file. A form with more than maxMultipartParts
// parts is left to the upstream handler.
func scanMultipartForm(body io.Reader, boundary string) (*scannedForm, error) {
	reader := multipart.NewReader(body, boundary)
	form := &scannedForm{}

	for parts := 1; ; parts++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}

		if err != nil {
			return nil, err
		}

		if parts > maxMultipartParts {
			return nil, errTooManyParts
		}

		if part.FileName() != "" {
			form.hasFile = form.hasFile || part.FormName() == "file"
			continue
		}

		value, err := io.ReadAll(io.LimitReader(part, upstreamAudioFileCap+1))
		if err != nil {
			return nil, err
		}

		form.fields = append(form.fields, formField{name: strings.Clone(part.FormName()), value: string(value)})
	}
}
