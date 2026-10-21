package codex

import (
	"strings"
)

// DefaultUserAgent returns the Codex CLI fingerprint the transformer presents upstream, so the
// realtime call signalling can reuse it instead of copying the version string.
func DefaultUserAgent() string {
	return codexDefaultUserAgent
}

// LiveBaseURL returns the HTTP(S) base URL used for Codex live voice signalling
// (POST {base}/live and the related WebRTC call endpoints).
//
// The configured Codex base URL may carry the "#" or "##" raw-URL markers and may use a
// WebSocket scheme when the Responses transport is WebSocket. Live signalling is plain HTTPS,
// so the markers and any trailing slash are stripped and ws(s) schemes are rewritten to
// http(s), mirroring the catalog and image outbound conversions. An empty configured base
// resolves to the official ChatGPT backend because the constructor rewrites it.
func (t *OutboundTransformer) LiveBaseURL() string {
	if t == nil {
		return ""
	}

	base := strings.TrimRight(strings.TrimSpace(t.baseURL), "#/")

	switch {
	case strings.HasPrefix(strings.ToLower(base), "wss://"):
		return "https://" + base[len("wss://"):]
	case strings.HasPrefix(strings.ToLower(base), "ws://"):
		return "http://" + base[len("ws://"):]
	default:
		return base
	}
}
