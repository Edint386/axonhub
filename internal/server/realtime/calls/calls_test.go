package calls

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSDP = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"

func TestDetectVariant(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		header map[string]string
		query  string
		want   Variant
	}{
		{name: "v1 live path", path: "/v1/live", want: VariantCodexV3},
		{name: "codex live path", path: "/codex/live", want: VariantCodexV3},
		{name: "bare live path", path: "/live", want: VariantCodexV3},
		{name: "live path trailing slash", path: "/v1/live/", want: VariantCodexV3},
		{name: "alpha v2 on calls path", path: "/v1/realtime/calls", header: map[string]string{"openai-alpha": "quicksilver=v2"}, want: VariantCodexV3},
		{name: "alpha v2 case and spaces", path: "/v1/realtime/calls", header: map[string]string{"OpenAI-Alpha": "  QuickSilver=V2 "}, want: VariantCodexV3},
		{name: "alpha v1", path: "/v1/realtime/calls", header: map[string]string{"openai-alpha": "quicksilver=v1"}, want: VariantCodexV1},
		{name: "intent quicksilver", path: "/v1/realtime/calls", query: "intent=quicksilver", want: VariantCodexV1},
		{name: "live path wins over alpha v1", path: "/v1/live", header: map[string]string{"openai-alpha": "quicksilver=v1"}, want: VariantCodexV3},
		{name: "plain calls", path: "/v1/realtime/calls", want: VariantOpenAI},
		{name: "codex calls", path: "/codex/realtime/calls", want: VariantOpenAI},
		{name: "unknown alpha", path: "/v1/realtime/calls", header: map[string]string{"openai-alpha": "something=else"}, want: VariantOpenAI},
		{name: "other intent", path: "/v1/realtime/calls", query: "intent=transcription", want: VariantOpenAI},
		{name: "live-like prefix is not live", path: "/v1/livestream", want: VariantOpenAI},
		{name: "nil header and query", path: "/v1/realtime/calls", want: VariantOpenAI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h http.Header
			if tt.header != nil {
				h = http.Header{}
				for k, v := range tt.header {
					h.Set(k, v)
				}
			}

			var q url.Values
			if tt.query != "" {
				var err error

				q, err = url.ParseQuery(tt.query)
				require.NoError(t, err)
			}

			require.Equal(t, tt.want, DetectVariant(tt.path, h, q))
		})
	}
}

// multipartBodyWith builds a multipart/form-data body with the given parts (name → content).
func multipartBodyWith(t *testing.T, boundary string, parts [][2]string) (string, []byte) {
	t.Helper()

	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)
	require.NoError(t, w.SetBoundary(boundary))

	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p[0]+`"`)

		pw, err := w.CreatePart(h)
		require.NoError(t, err)

		_, err = io.WriteString(pw, p[1])
		require.NoError(t, err)
	}

	require.NoError(t, w.Close())

	return w.FormDataContentType(), buf.Bytes()
}

// unsizedReader hides the body type from httptest.NewRequest so ContentLength stays -1 and
// the streaming limit is exercised.
type unsizedReader struct {
	r io.Reader
}

func (u unsizedReader) Read(p []byte) (int, error) {
	return u.r.Read(p)
}

func newCallRequest(t *testing.T, path, contentType string, body []byte, sized bool) *http.Request {
	t.Helper()

	var rd io.Reader = bytes.NewReader(body)
	if !sized {
		rd = unsizedReader{r: bytes.NewReader(body)}
	}

	r := httptest.NewRequest(http.MethodPost, path, rd)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}

	return r
}

func TestParseRequest(t *testing.T) {
	t.Run("multipart with sdp and session", func(t *testing.T) {
		ct, body := multipartBodyWith(t, "test-boundary", [][2]string{
			{"sdp", testSDP},
			{"session", `{"model":"gpt-live-1-codex","delegation":{"type":"client"}}`},
			{"extra", "ignored"},
		})

		req, err := ParseRequest(newCallRequest(t, "/v1/live", ct, body, true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
		require.JSONEq(t, `{"model":"gpt-live-1-codex","delegation":{"type":"client"}}`, string(req.Session))
		require.Equal(t, VariantCodexV3, req.Variant)
		require.Equal(t, "gpt-live-1-codex", req.Model)
		require.False(t, req.ModelFromQuery)
		require.Equal(t, "/v1", req.Prefix)
	})

	t.Run("multipart without session defaults model by path", func(t *testing.T) {
		ct, body := multipartBodyWith(t, "test-boundary", [][2]string{{"sdp", testSDP}})

		req, err := ParseRequest(newCallRequest(t, "/codex/live", ct, body, false), "/codex/", 1<<20)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
		require.Nil(t, req.Session)
		require.Equal(t, VariantCodexV3, req.Variant)
		require.Equal(t, DefaultLiveModel, req.Model)
		require.False(t, req.ModelFromQuery)
		require.Equal(t, "/codex", req.Prefix)
	})

	t.Run("raw sdp on realtime calls", func(t *testing.T) {
		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp", []byte(testSDP), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
		require.Nil(t, req.Session)
		require.Equal(t, VariantOpenAI, req.Variant)
		require.Equal(t, DefaultRealtimeModel, req.Model)
	})

	t.Run("raw sdp with charset parameter", func(t *testing.T) {
		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp; charset=utf-8", []byte(testSDP), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
	})

	t.Run("raw sdp uses query model for selection only", func(t *testing.T) {
		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls?model=gpt-realtime-2.1", "application/sdp", []byte(testSDP), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, "gpt-realtime-2.1", req.Model)
		require.True(t, req.ModelFromQuery)
	})

	t.Run("session model wins over query model", func(t *testing.T) {
		body := `{"sdp":` + jsonString(testSDP) + `,"session":{"type":"realtime","model":"gpt-realtime"}}`

		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls?model=other", "application/json", []byte(body), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, "gpt-realtime", req.Model)
		require.False(t, req.ModelFromQuery)
	})

	t.Run("json body", func(t *testing.T) {
		body := `{"sdp":` + jsonString(testSDP) + `,"session":{"type":"quicksilver","model":"gpt-realtime-1.5"}}`

		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls?intent=quicksilver", "application/json", []byte(body), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
		require.JSONEq(t, `{"type":"quicksilver","model":"gpt-realtime-1.5"}`, string(req.Session))
		require.Equal(t, VariantCodexV1, req.Variant)
		require.Equal(t, "gpt-realtime-1.5", req.Model)
	})

	t.Run("json body with string-encoded session", func(t *testing.T) {
		body := `{"sdp":` + jsonString(testSDP) + `,"session":"{\"model\":\"m1\"}"}`

		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/json", []byte(body), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"m1"}`, string(req.Session))
		require.Equal(t, "m1", req.Model)
	})

	t.Run("json body with null session", func(t *testing.T) {
		body := `{"sdp":` + jsonString(testSDP) + `,"session":null}`

		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/json", []byte(body), true), "/v1", 1<<20)
		require.NoError(t, err)
		require.Nil(t, req.Session)
	})

	t.Run("unsupported content type", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "text/plain", []byte(testSDP), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrUnsupportedMediaType)
		require.Equal(t, http.StatusUnsupportedMediaType, StatusCode(err))
	})

	t.Run("missing content type", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "", []byte(testSDP), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrUnsupportedMediaType)
	})

	t.Run("oversize by content length", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp", []byte(strings.Repeat("a", 65)), true), "/v1", 64)
		require.ErrorIs(t, err, ErrBodyTooLarge)
		require.Equal(t, http.StatusRequestEntityTooLarge, StatusCode(err))
	})

	t.Run("oversize streamed raw body", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp", []byte(strings.Repeat("a", 65)), false), "/v1", 64)
		require.ErrorIs(t, err, ErrBodyTooLarge)
		require.Equal(t, http.StatusRequestEntityTooLarge, StatusCode(err))
	})

	t.Run("oversize streamed multipart part", func(t *testing.T) {
		ct, body := multipartBodyWith(t, "test-boundary", [][2]string{{"sdp", strings.Repeat("a", 300)}})

		_, err := ParseRequest(newCallRequest(t, "/v1/live", ct, body, false), "/v1", 256)
		require.ErrorIs(t, err, ErrBodyTooLarge)
		require.Equal(t, http.StatusRequestEntityTooLarge, StatusCode(err))
	})

	t.Run("caller max bytes error maps to 413", func(t *testing.T) {
		r := newCallRequest(t, "/v1/realtime/calls", "application/sdp", []byte(strings.Repeat("a", 65)), false)
		r.Body = http.MaxBytesReader(nil, r.Body, 32)

		_, err := ParseRequest(r, "/v1", 1<<20)
		require.ErrorIs(t, err, ErrBodyTooLarge)
		require.Equal(t, http.StatusRequestEntityTooLarge, StatusCode(err))
	})

	t.Run("missing sdp in multipart", func(t *testing.T) {
		ct, body := multipartBodyWith(t, "test-boundary", [][2]string{{"session", `{"model":"m"}`}})

		_, err := ParseRequest(newCallRequest(t, "/v1/live", ct, body, true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrMissingSDP)
		require.Equal(t, http.StatusBadRequest, StatusCode(err))
	})

	t.Run("blank sdp in json", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/json", []byte(`{"sdp":"  \n"}`), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrMissingSDP)
	})

	t.Run("empty raw body", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp", nil, true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrMissingSDP)
	})

	t.Run("nil body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", nil)
		r.Header.Set("Content-Type", "application/sdp")
		r.Body = nil

		_, err := ParseRequest(r, "/v1", 1<<20)
		require.ErrorIs(t, err, ErrMissingSDP)
	})

	t.Run("malformed json", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/json", []byte(`{"sdp":`), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrInvalidBody)
		require.Equal(t, http.StatusBadRequest, StatusCode(err))
	})

	t.Run("invalid session json in multipart", func(t *testing.T) {
		ct, body := multipartBodyWith(t, "test-boundary", [][2]string{{"sdp", testSDP}, {"session", `{"model":`}})

		_, err := ParseRequest(newCallRequest(t, "/v1/live", ct, body, true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrInvalidBody)
	})

	t.Run("session must be an object", func(t *testing.T) {
		body := `{"sdp":` + jsonString(testSDP) + `,"session":[1,2]}`

		_, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/json", []byte(body), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrInvalidBody)
	})

	t.Run("multipart without boundary", func(t *testing.T) {
		_, err := ParseRequest(newCallRequest(t, "/v1/live", "multipart/form-data", []byte("x"), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrInvalidBody)
	})

	t.Run("truncated multipart", func(t *testing.T) {
		body := "--test-boundary\r\nContent-Disposition: form-data; name=\"sdp\"\r\n\r\n" + testSDP

		_, err := ParseRequest(newCallRequest(t, "/v1/live", "multipart/form-data; boundary=test-boundary", []byte(body), true), "/v1", 1<<20)
		require.ErrorIs(t, err, ErrInvalidBody)
	})

	t.Run("zero max bytes uses the default cap", func(t *testing.T) {
		req, err := ParseRequest(newCallRequest(t, "/v1/realtime/calls", "application/sdp", []byte(testSDP), true), "/v1", 0)
		require.NoError(t, err)
		require.Equal(t, testSDP, req.SDP)
	})
}

func TestStatusCode(t *testing.T) {
	require.Equal(t, 0, StatusCode(nil))
	require.Equal(t, 0, StatusCode(errors.New("other")))
	require.Equal(t, http.StatusRequestEntityTooLarge, StatusCode(&http.MaxBytesError{Limit: 1}))
	require.Equal(t, http.StatusBadRequest, StatusCode(ErrInvalidBody))
	require.Equal(t, http.StatusUnsupportedMediaType, StatusCode(ErrUnsupportedMediaType))
}

// jsonString quotes s as a JSON string literal.
func jsonString(s string) string {
	var buf bytes.Buffer

	buf.WriteByte('"')

	for _, c := range s {
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			buf.WriteRune(c)
		}
	}

	buf.WriteByte('"')

	return buf.String()
}
