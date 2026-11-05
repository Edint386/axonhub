package bailianasr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"
)

// NewFileSource reads size bytes of audio from an open file, for example a large upload the
// HTTP handler spooled to disk (and unlinked at once). digest is the hex SHA-256 of the content
// (computed while spooling); the caller owns the file and closes it once the request has
// finished. Every Open reads from the start through ReadAt, so readers never share an offset.
// onStaged (when set) runs once, the first time the audio is no longer needed for the job (see
// StagedAudio); the file must stay open until the request ends all the same.
func NewFileSource(file io.ReaderAt, name string, size int64, digest string, onStaged func()) AudioSource {
	return &fileSource{file: file, name: name, size: size, digest: digest, onStaged: onStaged}
}

type fileSource struct {
	file   io.ReaderAt
	name   string
	size   int64
	digest string

	onStaged func()
	staged   sync.Once
}

func (s *fileSource) Staged() {
	s.staged.Do(func() {
		if s.onStaged != nil {
			s.onStaged()
		}
	})
}

func (s *fileSource) Name() string { return s.name }

func (s *fileSource) Size() int64 { return s.size }

func (s *fileSource) Open() (io.ReadCloser, error) {
	return io.NopCloser(io.NewSectionReader(s.file, 0, s.size)), nil
}

func (s *fileSource) Digest() (string, error) {
	if s.digest == "" {
		return "", errors.New("file source has no digest")
	}

	return s.digest, nil
}

// NewURLSource is audio DashScope downloads itself from a public http(s) URL; the gateway never
// reads it. The caller validates the scheme.
func NewURLSource(rawURL string) RemoteAudio {
	return &urlSource{url: rawURL}
}

type urlSource struct {
	url string
}

// Name is "audio" with the extension of the URL's file name ("audio.mp3"), or "" when it has
// none. It becomes the file name of the stored request body, so the file name itself (which may
// be a capability key, as in https://utfs.io/f/<key>) is never part of it.
func (s *urlSource) Name() string {
	if u, err := url.Parse(s.url); err == nil {
		if ext := urlExtension(u.Path); ext != "" {
			return "audio" + ext
		}
	}

	return ""
}

func (s *urlSource) Size() int64 { return 0 }

func (s *urlSource) Open() (io.ReadCloser, error) {
	return nil, errors.New("remote audio is downloaded by DashScope, not by the gateway")
}

// Digest hashes the URL string, not the content behind it (the gateway never downloads it). The
// job cache therefore never takes remote audio (see jobExecutor.start): neither a result nor a
// running task is shared with a later request for the same URL, which submits again and reads
// the current content.
func (s *urlSource) Digest() (string, error) {
	sum := sha256.Sum256([]byte(s.url))
	return hex.EncodeToString(sum[:]), nil
}

func (s *urlSource) URL() string { return s.url }

// redactedPart replaces the parts of a logged audio URL that may be credentials.
const redactedPart = "<redacted>"

// RedactURL is rawURL reduced to its scheme, host and the extension of its file name, for request
// bodies that are stored or logged: a signed or unguessable URL is a working download credential
// for the audio, whether the secret is in its query string (X-Amz-Signature, an OSS Signature),
// in its path (a Telegram bot token, a share-link secret), in its file name itself (UploadThing's
// /f/<fileKey>, a random blob name) or in a path parameter (;jsessionid=). Only the extension is
// kept, for the format; everything else becomes "<redacted>". The real URL only travels in the
// request context and in the body sent to DashScope.
func RedactURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return redactedPart
	}

	out := u.Scheme + "://" + u.Host
	if strings.Trim(u.Path, "/") != "" {
		out += "/" + redactedPart + urlExtension(u.Path)
	}

	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		out += "?" + redactedPart
	}

	return out
}

// maxURLExtension bounds the extension urlExtension keeps (".webm" is the longest audio one).
const maxURLExtension = 5

// urlExtension is the extension of the file name of a URL path (".mp3"), without path
// parameters, when it is short and alphanumeric enough to name a format rather than carry part
// of a secret; "" otherwise.
func urlExtension(p string) string {
	name := path.Base(p)
	if i := strings.IndexByte(name, ';'); i >= 0 {
		name = name[:i]
	}

	ext := path.Ext(name)
	if len(ext) < 2 || len(ext) > maxURLExtension+1 {
		return ""
	}

	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}

	return ext
}

// IsHTTPURL reports whether raw is an absolute http(s) URL with a host, the only kind of audio
// URL passed on to DashScope.
func IsHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}

	return u.Scheme == "http" || u.Scheme == "https"
}
