package calls

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// MultipartBoundary is the fixed boundary of API-shape call-create bodies (the value
	// Codex itself uses).
	MultipartBoundary = "codex-realtime-call-boundary"
	// OpenAISidebandBase is the sideband WebSocket base for calls created with a Codex OAuth
	// credential; Codex >= 0.146 dials it regardless of the provider base URL.
	OpenAISidebandBase = "wss://api.openai.com/v1"

	// quicksilverQuery is the AVAS call-create query Codex appends for v1 and backend calls.
	quicksilverQuery = "intent=quicksilver&architecture=avas"

	headerAuthorization    = "Authorization"
	headerAccountID        = "Chatgpt-Account-Id"
	headerUserAgent        = "User-Agent"
	headerAttestation      = "X-Oai-Attestation"
	headerLocation         = "Location"
	headerExposeHeaders    = "Access-Control-Expose-Headers"
	contentTypeSDP         = "application/sdp"
	contentTypeJSON        = "application/json"
	sessionTypeRealtime    = "realtime"
	sessionTypeQuicksilver = "quicksilver"
)

// forwardedHeaders is the allowlist of inbound protocol headers copied to the upstream
// call-create and sideband requests. Authorization, Chatgpt-Account-Id, User-Agent and
// x-oai-attestation are handled separately. OpenAI-Safety-Identifier is an end-user hash,
// not a credential, and keeps per-user attribution upstream.
var forwardedHeaders = []string{
	headerOpenAIAlpha,
	"X-Session-Id",
	"Voice-Session-Id",
	"Session-Id",
	"Thread-Id",
	"X-Codex-Turn-Metadata",
	"Originator",
	"Openai-Safety-Identifier",
}

// forwardedResponseHeaders is the allowlist of upstream response headers returned to the
// client. Location must be overwritten by the caller with RewriteLocation.
var forwardedResponseHeaders = []string{
	"Content-Type",
	headerLocation,
	"Retry-After",
	"X-Request-Id",
	"Openai-Request-Id",
}

// Target describes where the upstream call is created.
type Target struct {
	// APIBase is the https base for OpenAI-family and Codex PAT relay channels, without a
	// trailing slash (for example https://api.openai.com/v1).
	APIBase string
	// Codex marks a Codex channel.
	Codex bool
	// Backend marks the Codex ChatGPT backend (JSON request shape); LiveBase then contains
	// /backend-api.
	Backend bool
	// LiveBase is the Codex live base URL (the transformer's LiveBaseURL()).
	LiveBase string
	// OAuth marks a call created with a Codex OAuth credential; its sideband lives at
	// OpenAISidebandBase. API-key credentials (OpenAI keys and Codex PAT relays) keep the
	// sideband on the channel base, whatever its URL shape.
	OAuth bool
}

// Credential is the upstream identity pinned to a call.
type Credential struct {
	// Bearer is the Authorization bearer token (API key, PAT or OAuth access token).
	Bearer string
	// AccountID is the ChatGPT account id for Codex OAuth channels; empty otherwise.
	AccountID string
}

// EncodeOptions tunes EncodeUpstream and SidebandHeaders.
type EncodeOptions struct {
	// ActualModel replaces session.model; it falls back to the inbound Request.Model.
	ActualModel string
	// ForwardAttestation forwards the client's x-oai-attestation header on call-create. It
	// only applies to the Codex ChatGPT backend; the device token is never sent elsewhere.
	ForwardAttestation bool
	// UserAgent is the User-Agent sent upstream unless UserAgentPassThrough applies.
	UserAgent string
	// UserAgentPassThrough keeps the client's User-Agent when it sent one.
	UserAgentPassThrough bool
}

// UpstreamRequest is an encoded upstream HTTP request.
type UpstreamRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// EncodeUpstream builds the upstream call-create request for the (variant × target) matrix.
//
// Codex backend targets receive POST {LiveBase}/realtime/calls?intent=quicksilver&architecture=avas
// with a JSON {"sdp","session"} body. Because intent=quicksilver is always sent there, the
// backend requires session.type="quicksilver": it is forced (overwriting "realtime") when the
// inbound request carried no openai-alpha header, and left untouched when it did (Codex
// clients send the right type themselves). Every other target receives a multipart/form-data
// body (boundary MultipartBoundary, parts sdp and session) at {APIBase}/live (codex-v3),
// {APIBase}/realtime/calls?intent=quicksilver&architecture=avas (codex-v1) or
// {APIBase}/realtime/calls with only the dcid query forwarded (openai); for the openai
// variant session.type="realtime" is added when absent. In every case session.model is set
// to the actual model, session.id is removed, a missing session is synthesized, Accept is
// application/sdp and OpenAI-Beta is never sent. The x-oai-attestation header is forwarded
// only to the Codex backend, and only when opts.ForwardAttestation is set.
func EncodeUpstream(
	inbound *Request,
	inboundHeader http.Header,
	inboundQuery url.Values,
	target Target,
	cred Credential,
	opts EncodeOptions,
) (*UpstreamRequest, error) {
	if inbound == nil {
		return nil, fmt.Errorf("calls: encode upstream: nil inbound request")
	}

	if strings.TrimSpace(inbound.SDP) == "" {
		return nil, fmt.Errorf("calls: encode upstream: %w", ErrMissingSDP)
	}

	actualModel := strings.TrimSpace(opts.ActualModel)
	if actualModel == "" {
		actualModel = strings.TrimSpace(inbound.Model)
	}

	if actualModel == "" {
		return nil, fmt.Errorf("calls: encode upstream: no model")
	}

	variant := inbound.Variant
	if variant == "" {
		variant = VariantOpenAI
	}

	backend := target.Codex && target.Backend

	session, err := prepareSession(inbound.Session, variant, backend, actualModel, inboundHeader.Get(headerOpenAIAlpha) != "")
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Accept", contentTypeSDP)
	setCredential(header, cred)
	copyHeaders(header, inboundHeader, forwardedHeaders)

	if opts.ForwardAttestation && backend {
		copyHeaders(header, inboundHeader, []string{headerAttestation})
	}

	applyUserAgent(header, inboundHeader, opts)

	if backend {
		base, err := httpBase(firstNonEmpty(target.LiveBase, target.APIBase))
		if err != nil {
			return nil, fmt.Errorf("calls: encode upstream: codex backend base: %w", err)
		}

		body, err := jsonBody(inbound.SDP, session)
		if err != nil {
			return nil, err
		}

		header.Set("Content-Type", contentTypeJSON)

		return &UpstreamRequest{
			Method: http.MethodPost,
			URL:    base + "/realtime/calls?" + quicksilverQuery,
			Header: header,
			Body:   body,
		}, nil
	}

	base, err := httpBase(firstNonEmpty(target.APIBase, target.LiveBase))
	if err != nil {
		return nil, fmt.Errorf("calls: encode upstream: api base: %w", err)
	}

	var endpoint string

	switch variant {
	case VariantCodexV3:
		endpoint = base + "/live"
	case VariantCodexV1:
		endpoint = base + "/realtime/calls?" + quicksilverQuery
	case VariantOpenAI:
		endpoint = base + "/realtime/calls"

		if dcid := inboundQuery["dcid"]; len(dcid) > 0 {
			endpoint += "?" + url.Values{"dcid": dcid}.Encode()
		}
	default:
		return nil, fmt.Errorf("calls: encode upstream: unknown variant %q", variant)
	}

	body, contentType, err := multipartBody(inbound.SDP, session)
	if err != nil {
		return nil, err
	}

	header.Set("Content-Type", contentType)

	return &UpstreamRequest{
		Method: http.MethodPost,
		URL:    endpoint,
		Header: header,
		Body:   body,
	}, nil
}

// prepareSession applies the session rewrite rules: model, id removal and type injection.
func prepareSession(raw json.RawMessage, variant Variant, backend bool, actualModel string, hadAlpha bool) (json.RawMessage, error) {
	session := []byte("{}")

	if len(raw) > 0 {
		if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
			return nil, fmt.Errorf("calls: encode upstream: %w: session must be a JSON object", ErrInvalidBody)
		}

		session = append([]byte(nil), raw...)
	}

	session, err := sjson.SetBytes(session, "model", actualModel)
	if err != nil {
		return nil, fmt.Errorf("calls: encode upstream: set session.model: %w", err)
	}

	session, err = sjson.DeleteBytes(session, "id")
	if err != nil {
		return nil, fmt.Errorf("calls: encode upstream: delete session.id: %w", err)
	}

	hasType := gjson.GetBytes(session, "type").Exists()

	var injectType string

	switch {
	case backend:
		// The backend request always carries intent=quicksilver, which the ChatGPT backend
		// only accepts together with session.type="quicksilver"; GA clients send "realtime".
		if !hadAlpha {
			injectType = sessionTypeQuicksilver
		}
	case variant == VariantOpenAI:
		if !hasType {
			injectType = sessionTypeRealtime
		}
	}

	if injectType != "" {
		session, err = sjson.SetBytes(session, "type", injectType)
		if err != nil {
			return nil, fmt.Errorf("calls: encode upstream: set session.type: %w", err)
		}
	}

	return session, nil
}

// jsonBody encodes the Codex backend {"sdp","session"} body without HTML escaping.
func jsonBody(sdp string, session json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	payload := struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session"`
	}{SDP: sdp, Session: session}

	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("calls: encode upstream: marshal json body: %w", err)
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// multipartBody encodes the API-shape multipart body with the fixed boundary.
func multipartBody(sdp string, session json.RawMessage) ([]byte, string, error) {
	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(MultipartBoundary); err != nil {
		return nil, "", fmt.Errorf("calls: encode upstream: set boundary: %w", err)
	}

	if err := writePart(w, "sdp", contentTypeSDP, []byte(sdp)); err != nil {
		return nil, "", err
	}

	if err := writePart(w, "session", contentTypeJSON, session); err != nil {
		return nil, "", err
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("calls: encode upstream: close multipart: %w", err)
	}

	return buf.Bytes(), w.FormDataContentType(), nil
}

// writePart writes one form-data part with an explicit Content-Type.
func writePart(w *multipart.Writer, name, contentType string, data []byte) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, name))
	h.Set("Content-Type", contentType)

	pw, err := w.CreatePart(h)
	if err != nil {
		return fmt.Errorf("calls: encode upstream: create part %s: %w", name, err)
	}

	if _, err := pw.Write(data); err != nil {
		return fmt.Errorf("calls: encode upstream: write part %s: %w", name, err)
	}

	return nil
}

// ParseCallID resolves the upstream Location header (relative or absolute) against the
// upstream request URL and returns the last path segment when it starts with "rtc_" or is a
// 36-character UUID (query and fragment ignored); otherwise it returns the call_id query
// value; otherwise ok is false.
func ParseCallID(location string, upstream *url.URL) (string, bool) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", false
	}

	ref, err := url.Parse(location)
	if err != nil {
		return "", false
	}

	resolved := ref
	if upstream != nil {
		resolved = upstream.ResolveReference(ref)
	}

	if seg := path.Base(strings.TrimRight(resolved.Path, "/")); isCallID(seg) {
		return seg, true
	}

	if id := strings.TrimSpace(resolved.Query().Get("call_id")); id != "" {
		return id, true
	}

	return "", false
}

// isCallID reports whether seg looks like a realtime call id.
func isCallID(seg string) bool {
	if strings.HasPrefix(seg, "rtc_") && len(seg) > len("rtc_") {
		return true
	}

	return isUUID(seg)
}

// isUUID reports whether s is a 36-character hyphenated hexadecimal UUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}

	for i := range len(s) {
		c := s[i]

		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}

	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// RewriteLocation returns the gateway-relative Location for a call: {prefix}/live/{id} for
// codex-v3 and {prefix}/realtime/calls/{id} otherwise.
func RewriteLocation(prefix string, variant Variant, id string) string {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	escaped := url.PathEscape(id)

	if variant == VariantCodexV3 {
		return prefix + "/live/" + escaped
	}

	return prefix + "/realtime/calls/" + escaped
}

// ForwardedResponseHeaders copies the allowlisted upstream response headers (Content-Type,
// Location, Retry-After, X-Request-Id, OpenAI-Request-Id). The caller must overwrite
// Location with RewriteLocation; when a Location is present the result also carries
// Access-Control-Expose-Headers: Location so browsers can read it.
func ForwardedResponseHeaders(upstream http.Header) http.Header {
	h := http.Header{}
	copyHeaders(h, upstream, forwardedResponseHeaders)

	if h.Get(headerLocation) != "" {
		h.Set(headerExposeHeaders, headerLocation)
	}

	return h
}

// SidebandURL builds the upstream sideband WebSocket URL: {base}/live/{id} for codex-v3,
// {base}/realtime?intent=quicksilver&call_id={id} for codex-v1 and for openai calls created
// through the Codex backend (those were created with intent=quicksilver), and
// {base}/realtime?call_id={id} for every other openai call. The base is OpenAISidebandBase
// for OAuth credentials and otherwise the target base (APIBase, else LiveBase) with http(s)
// mapped to ws(s); the URL shape of the base plays no part in the choice.
func SidebandURL(variant Variant, target Target, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("calls: sideband url: empty call id")
	}

	base := OpenAISidebandBase

	if !target.OAuth {
		var err error

		base, err = wsBase(firstNonEmpty(target.APIBase, target.LiveBase))
		if err != nil {
			return "", fmt.Errorf("calls: sideband url: %w", err)
		}
	}

	switch variant {
	case VariantCodexV3:
		return base + "/live/" + url.PathEscape(id), nil
	case VariantCodexV1:
		return base + "/realtime?intent=quicksilver&call_id=" + url.QueryEscape(id), nil
	case VariantOpenAI:
		if target.Codex && target.Backend {
			return base + "/realtime?intent=quicksilver&call_id=" + url.QueryEscape(id), nil
		}

		return base + "/realtime?call_id=" + url.QueryEscape(id), nil
	default:
		return "", fmt.Errorf("calls: sideband url: unknown variant %q", variant)
	}
}

// SidebandHeaders returns the headers for the upstream sideband dial: the pinned credential
// (Authorization bearer and Chatgpt-Account-Id when set), the forwarded protocol headers
// (openai-alpha, x-session-id, voice-session-id, session-id, thread-id, x-codex-turn-metadata,
// originator) and User-Agent per the EncodeOptions rule. WebSocket handshake headers are
// never copied.
func SidebandHeaders(inbound http.Header, cred Credential, opts EncodeOptions) http.Header {
	h := http.Header{}
	setCredential(h, cred)
	copyHeaders(h, inbound, forwardedHeaders)
	applyUserAgent(h, inbound, opts)

	return h
}

// setCredential writes the bearer and account headers when they are set.
func setCredential(dst http.Header, cred Credential) {
	if bearer := strings.TrimSpace(cred.Bearer); bearer != "" {
		dst.Set(headerAuthorization, "Bearer "+bearer)
	}

	if account := strings.TrimSpace(cred.AccountID); account != "" {
		dst.Set(headerAccountID, account)
	}
}

// copyHeaders copies every value of the named headers from src to dst.
func copyHeaders(dst, src http.Header, names []string) {
	for _, name := range names {
		for _, v := range src.Values(name) {
			dst.Add(name, v)
		}
	}
}

// applyUserAgent sets User-Agent: the client's when pass-through is on and it sent one,
// else opts.UserAgent when non-empty.
func applyUserAgent(dst, inbound http.Header, opts EncodeOptions) {
	if opts.UserAgentPassThrough {
		if ua := strings.TrimSpace(inbound.Get(headerUserAgent)); ua != "" {
			dst.Set(headerUserAgent, ua)

			return
		}
	}

	if ua := strings.TrimSpace(opts.UserAgent); ua != "" {
		dst.Set(headerUserAgent, ua)
	}
}

// httpBase normalizes an upstream base URL for HTTP use: trims whitespace, '#' and '/',
// maps ws(s) to http(s) and drops any query or fragment.
func httpBase(base string) (string, error) {
	u, err := parseBase(base)
	if err != nil {
		return "", err
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		u.Scheme = strings.ToLower(u.Scheme)
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported scheme %q in base %q", u.Scheme, base)
	}

	return strings.TrimRight(u.String(), "/"), nil
}

// wsBase normalizes an upstream base URL for WebSocket use: like httpBase but with http(s)
// mapped to ws(s).
func wsBase(base string) (string, error) {
	u, err := parseBase(base)
	if err != nil {
		return "", err
	}

	switch strings.ToLower(u.Scheme) {
	case "ws", "wss":
		u.Scheme = strings.ToLower(u.Scheme)
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q in base %q", u.Scheme, base)
	}

	return strings.TrimRight(u.String(), "/"), nil
}

// parseBase trims and parses a base URL, dropping its query and fragment.
func parseBase(base string) (*url.URL, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "#/")
	if base == "" {
		return nil, fmt.Errorf("empty base url")
	}

	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse base %q: %w", base, err)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("base %q has no host", base)
	}

	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""

	return u, nil
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
