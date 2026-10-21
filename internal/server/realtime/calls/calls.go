// Package calls implements the pure-logic half of the WebRTC call signalling relay:
// protocol-variant detection, inbound call-create body parsing, upstream request encoding
// for the (variant × target) matrix, Location and call-id handling, sideband URL and header
// construction, and the in-memory call binding store.
//
// The package performs no network I/O and does not import biz or orchestrator types; the
// relay engine maps channel and credential information into Target, Credential and
// EncodeOptions before calling into it.
package calls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
)

// Variant is the signalling protocol variant spoken by the inbound client.
type Variant string

const (
	// VariantOpenAI is the OpenAI Realtime GA WebRTC flow (POST /realtime/calls).
	VariantOpenAI Variant = "openai"
	// VariantCodexV1 is the Codex quicksilver v1 flow (openai-alpha: quicksilver=v1 or
	// intent=quicksilver).
	VariantCodexV1 Variant = "codex-v1"
	// VariantCodexV3 is the Codex Frameless Bidi flow (POST /live or openai-alpha:
	// quicksilver=v2).
	VariantCodexV3 Variant = "codex-v3"
)

const (
	// DefaultLiveModel is used when a /live request names no model (the Codex v3 default).
	DefaultLiveModel = "gpt-live-1-codex"
	// DefaultRealtimeModel is used when a /realtime/calls request names no model.
	DefaultRealtimeModel = "gpt-realtime"

	// defaultMaxBytes caps the call-create body when the caller passes no limit.
	defaultMaxBytes int64 = 1 << 20

	headerOpenAIAlpha  = "Openai-Alpha"
	alphaQuicksilverV1 = "quicksilver=v1"
	alphaQuicksilverV2 = "quicksilver=v2"
	intentQuicksilver  = "quicksilver"
)

// RequestError is a client-side failure of ParseRequest. Status is the HTTP status the
// handler should answer with and Code is an OpenAI-style error code.
type RequestError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error.
func (e *RequestError) Error() string {
	return e.Message
}

// Sentinel request errors. They are returned directly or wrapped with fmt.Errorf("%w: ..."),
// so callers test them with errors.Is and read the status with StatusCode.
var (
	// ErrUnsupportedMediaType is returned for a Content-Type other than multipart/form-data,
	// application/sdp or application/json.
	ErrUnsupportedMediaType = &RequestError{
		Status:  http.StatusUnsupportedMediaType,
		Code:    "unsupported_media_type",
		Message: "unsupported content type: expected multipart/form-data, application/sdp or application/json",
	}
	// ErrMissingSDP is returned when the request carries no SDP offer.
	ErrMissingSDP = &RequestError{
		Status:  http.StatusBadRequest,
		Code:    "missing_sdp",
		Message: "missing sdp offer",
	}
	// ErrBodyTooLarge is returned when the body or one of its parts exceeds the limit.
	ErrBodyTooLarge = &RequestError{
		Status:  http.StatusRequestEntityTooLarge,
		Code:    "request_too_large",
		Message: "request body too large",
	}
	// ErrInvalidBody is returned for malformed multipart, JSON or session payloads.
	ErrInvalidBody = &RequestError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_request",
		Message: "invalid request body",
	}
)

// StatusCode returns the HTTP status for an error produced by ParseRequest, or 0 when the
// error is not a request error. An http.MaxBytesError from the caller's own body limit maps
// to 413.
func StatusCode(err error) int {
	if re, ok := errors.AsType[*RequestError](err); ok {
		return re.Status
	}

	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge
	}

	return 0
}

// Request is a parsed inbound call-create request.
type Request struct {
	// SDP is the client's SDP offer, verbatim.
	SDP string
	// Session is the client's session JSON object, or nil when absent.
	Session json.RawMessage
	// Variant is the detected protocol variant.
	Variant Variant
	// Model is session.model, else the ?model= query value, else the default for the path
	// (/live → gpt-live-1-codex, /realtime/calls → gpt-realtime).
	Model string
	// ModelFromQuery reports that Model came from the ?model= query parameter. Such a value
	// is used for channel selection only and is never forwarded upstream.
	ModelFromQuery bool
	// Prefix is the inbound route prefix ("/v1" or "/codex"), without a trailing slash.
	Prefix string
}

// DetectVariant classifies the inbound request: codex-v3 when the route path (after any /v1
// or /codex prefix) is /live or the openai-alpha header is quicksilver=v2; codex-v1 when
// openai-alpha is quicksilver=v1 or the intent query is quicksilver; openai otherwise.
// Header values are trimmed and compared case-insensitively.
func DetectVariant(path string, header http.Header, query url.Values) Variant {
	alpha := strings.TrimSpace(header.Get(headerOpenAIAlpha))

	if routePath(path) == "/live" || strings.EqualFold(alpha, alphaQuicksilverV2) {
		return VariantCodexV3
	}

	if strings.EqualFold(alpha, alphaQuicksilverV1) ||
		strings.EqualFold(strings.TrimSpace(query.Get("intent")), intentQuicksilver) {
		return VariantCodexV1
	}

	return VariantOpenAI
}

// routePath strips one /v1 or /codex route prefix and trailing slashes from a request path.
func routePath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	for _, prefix := range []string{"/v1", "/codex"} {
		if p == prefix {
			return "/"
		}

		if strings.HasPrefix(p, prefix+"/") {
			p = strings.TrimPrefix(p, prefix)

			break
		}
	}

	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}

	return p
}

// ParseRequest reads a call-create body as multipart/form-data (parts sdp and session,
// streamed with mime/multipart.Reader and no temporary files), application/sdp (the raw
// offer) or application/json ({"sdp","session"}). The body is capped at maxBytes (1 MiB when
// maxBytes <= 0) even if the caller already wrapped it with http.MaxBytesReader, and every
// multipart part is capped individually. It returns ErrUnsupportedMediaType, ErrMissingSDP,
// ErrBodyTooLarge or ErrInvalidBody (possibly wrapped) on client errors.
func ParseRequest(r *http.Request, prefix string, maxBytes int64) (*Request, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("%w: nil request", ErrInvalidBody)
	}

	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	if r.ContentLength > maxBytes {
		return nil, ErrBodyTooLarge
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedMediaType, err)
	}

	body := r.Body
	if body == nil {
		body = http.NoBody
	}

	body = http.MaxBytesReader(nil, body, maxBytes)

	var (
		sdp     string
		session json.RawMessage
	)

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		sdp, session, err = parseMultipart(body, params["boundary"], maxBytes)
	case mediaType == "application/sdp":
		var data []byte

		data, err = readLimited(body, maxBytes)
		sdp = string(data)
	case mediaType == "application/json":
		sdp, session, err = parseJSON(body, maxBytes)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedMediaType, mediaType)
	}

	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(sdp) == "" {
		return nil, ErrMissingSDP
	}

	session, err = normalizeSession(session)
	if err != nil {
		return nil, err
	}

	query := r.URL.Query()

	req := &Request{
		SDP:     sdp,
		Session: session,
		Variant: DetectVariant(r.URL.Path, r.Header, query),
		Prefix:  strings.TrimRight(strings.TrimSpace(prefix), "/"),
	}

	switch {
	case sessionModel(session) != "":
		req.Model = sessionModel(session)
	case strings.TrimSpace(query.Get("model")) != "":
		req.Model = strings.TrimSpace(query.Get("model"))
		req.ModelFromQuery = true
	case routePath(r.URL.Path) == "/live":
		req.Model = DefaultLiveModel
	default:
		req.Model = DefaultRealtimeModel
	}

	return req, nil
}

// sessionModel returns the trimmed session.model string, or "" when absent or not a string.
func sessionModel(session json.RawMessage) string {
	if len(session) == 0 {
		return ""
	}

	m := gjson.GetBytes(session, "model")
	if m.Type != gjson.String {
		return ""
	}

	return strings.TrimSpace(m.String())
}

// parseMultipart streams the multipart body and returns the sdp and session parts.
func parseMultipart(body io.Reader, boundary string, maxBytes int64) (string, json.RawMessage, error) {
	if boundary == "" {
		return "", nil, fmt.Errorf("%w: missing multipart boundary", ErrInvalidBody)
	}

	var (
		sdp     string
		session json.RawMessage
	)

	mr := multipart.NewReader(body, boundary)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return "", nil, wrapReadError(err)
		}

		switch part.FormName() {
		case "sdp":
			data, err := readLimited(part, maxBytes)
			if err != nil {
				return "", nil, err
			}

			sdp = string(data)
		case "session":
			data, err := readLimited(part, maxBytes)
			if err != nil {
				return "", nil, err
			}

			session = data
		default:
			// Unknown parts are drained by the next NextPart call.
		}
	}

	return sdp, session, nil
}

// parseJSON decodes a {"sdp","session"} JSON body.
func parseJSON(body io.Reader, maxBytes int64) (string, json.RawMessage, error) {
	data, err := readLimited(body, maxBytes)
	if err != nil {
		return "", nil, err
	}

	var payload struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session"`
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
	}

	return payload.SDP, payload.Session, nil
}

// normalizeSession trims the session payload, unwraps a JSON string that itself holds a JSON
// document, maps null/empty to nil and requires a JSON object otherwise.
func normalizeSession(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}

	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%w: invalid session: %w", ErrInvalidBody, err)
		}

		raw = bytes.TrimSpace([]byte(s))
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return nil, nil
		}
	}

	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: session is not valid JSON", ErrInvalidBody)
	}

	if !gjson.ParseBytes(raw).IsObject() {
		return nil, fmt.Errorf("%w: session must be a JSON object", ErrInvalidBody)
	}

	return raw, nil
}

// readLimited reads at most maxBytes from rd and returns ErrBodyTooLarge beyond that.
func readLimited(rd io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(rd, maxBytes+1))
	if err != nil {
		return nil, wrapReadError(err)
	}

	if int64(len(data)) > maxBytes {
		return nil, ErrBodyTooLarge
	}

	return data, nil
}

// wrapReadError maps body read failures to request errors.
func wrapReadError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return fmt.Errorf("%w: %w", ErrBodyTooLarge, err)
	}

	return fmt.Errorf("%w: %w", ErrInvalidBody, err)
}
