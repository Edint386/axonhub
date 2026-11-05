package bailianasr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
)

// AudioSource is the audio one transcription reads. Requests carry the multipart file in memory
// today; a large-upload or URL input can supply its own source through WithAudioSource without
// touching the request builders or the executor.
type AudioSource interface {
	// Name is the client file name; it may be empty.
	Name() string
	// Size is the audio length in bytes.
	Size() int64
	// Open returns a new reader positioned at the start of the audio.
	Open() (io.ReadCloser, error)
	// Digest is a stable content hash used as part of the job-cache key; remote audio, which
	// the gateway never reads, hashes its URL instead (see urlSource.Digest).
	Digest() (string, error)
}

// RemoteAudio is an AudioSource DashScope downloads itself (a public http(s) URL): async jobs
// skip the temp-storage upload and submit the URL, and the flash model receives the URL in
// place of a base64 data URI.
type RemoteAudio interface {
	AudioSource
	URL() string
}

// StagedAudio is an AudioSource that wants to know when this request no longer needs anything it
// reserved to read the audio (an upload slot, for example): its audio reached Bailian's temporary
// storage, or the job cache already had the job. Staged may be called more than once. The source
// must stay readable until the request ends: a pipeline retry may upload it again.
type StagedAudio interface {
	AudioSource
	Staged()
}

func notifyStaged(src AudioSource) {
	if staged, ok := src.(StagedAudio); ok {
		staged.Staged()
	}
}

type audioSourceKey struct{}

// WithAudioSource makes the transcription of this request read src instead of
// llm.TranscriptionRequest.File.
func WithAudioSource(ctx context.Context, src AudioSource) context.Context {
	return context.WithValue(ctx, audioSourceKey{}, src)
}

func audioSourceFor(ctx context.Context, tr *llm.TranscriptionRequest) (AudioSource, error) {
	if src, ok := ctx.Value(audioSourceKey{}).(AudioSource); ok && src != nil {
		return src, nil
	}

	if len(tr.File) == 0 {
		return nil, fmt.Errorf("%w: file is required for transcription", transformer.ErrInvalidRequest)
	}

	return NewBytesSource(tr.FileName, tr.File), nil
}

// NewBytesSource wraps in-memory audio.
func NewBytesSource(name string, data []byte) AudioSource {
	return &bytesSource{name: name, data: data}
}

type bytesSource struct {
	name   string
	data   []byte
	once   sync.Once
	digest string
}

func (s *bytesSource) Name() string { return s.name }

func (s *bytesSource) Size() int64 { return int64(len(s.data)) }

func (s *bytesSource) Open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func (s *bytesSource) Digest() (string, error) {
	s.once.Do(func() {
		sum := sha256.Sum256(s.data)
		s.digest = hex.EncodeToString(sum[:])
	})

	return s.digest, nil
}

// readHead returns up to n leading bytes of src, for format sniffing.
func readHead(src AudioSource, n int) ([]byte, error) {
	r, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	head := make([]byte, n)

	read, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	return head[:read], nil
}

func readAll(src AudioSource) ([]byte, error) {
	r, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return io.ReadAll(r)
}
