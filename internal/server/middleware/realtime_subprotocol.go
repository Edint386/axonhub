package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/samber/lo"
)

// Realtime WebSocket subprotocol authentication (custom fork code).
//
// Browsers cannot set an Authorization header on a WebSocket handshake, so the OpenAI
// Realtime API lets them offer the API key as a Sec-WebSocket-Protocol token of the form
// "openai-insecure-api-key.<key>" next to the real "realtime" subprotocol. This file lifts
// that key into Authorization: Bearer before the API-key middleware runs and strips the key
// token from the request headers so it never reaches logs, traces, stored request headers or
// the upstream dial.

const (
	// RealtimeInsecureAPIKeyPrefix is the Sec-WebSocket-Protocol token prefix browsers use to
	// carry an API key on the WebSocket handshake.
	RealtimeInsecureAPIKeyPrefix = "openai-insecure-api-key."

	// RealtimeBetaSubprotocol is the Sec-WebSocket-Protocol token clients of the removed beta
	// Realtime protocol offer.
	RealtimeBetaSubprotocol = "openai-beta.realtime-v1"
)

const (
	secWebSocketProtocolHeader = "Sec-WebSocket-Protocol"
	openAIBetaHeader           = "Openai-Beta" // Canonical form; header lookups are case-insensitive.
	realtimeBetaHeaderValue    = "realtime=v1"
)

// errMultipleRealtimeSubprotocolKeys is returned when the client offers more than one distinct
// API key token. Its text never contains a key.
var errMultipleRealtimeSubprotocolKeys = errors.New("multiple openai-insecure-api-key subprotocols")

// WithRealtimeSubprotocolAuth lifts a browser-style API key out of Sec-WebSocket-Protocol into
// Authorization: Bearer before the API-key middleware runs, and removes the key token from the
// request headers so it never reaches logs, traces, stored request headers or the upstream.
//
// Only WebSocket upgrade requests are touched. Every Sec-WebSocket-Protocol header line is
// parsed (comma-separated, trimmed). More than one distinct key token aborts the request with a
// 400 JSON error. Exactly one key token sets Authorization: Bearer <key> unless an Authorization
// header is already present, in which case the subprotocol key is ignored without comparison.
// In every case Sec-WebSocket-Protocol is rewritten to a single line holding the remaining
// tokens joined by ", ", or deleted when none remain. The key is never logged.
func WithRealtimeSubprotocolAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !websocket.IsWebSocketUpgrade(c.Request) {
			c.Next()
			return
		}

		remaining, keys := splitRealtimeSubprotocolKeys(c.Request.Header.Values(secWebSocketProtocolHeader))

		if len(keys) > 1 {
			AbortWithError(c, http.StatusBadRequest, errMultipleRealtimeSubprotocolKeys)
			return
		}

		if len(keys) == 1 && c.Request.Header.Get("Authorization") == "" {
			c.Request.Header.Set("Authorization", "Bearer "+keys[0])
		}

		if len(remaining) == 0 {
			c.Request.Header.Del(secWebSocketProtocolHeader)
		} else {
			c.Request.Header.Set(secWebSocketProtocolHeader, strings.Join(remaining, ", "))
		}

		c.Next()
	}
}

// RealtimeSubprotocols returns the client's offered subprotocol tokens (after the key token was
// removed), parsed from every Sec-WebSocket-Protocol header line, comma-separated, trimmed,
// non-empty.
func RealtimeSubprotocols(r *http.Request) []string {
	return parseHeaderTokens(r.Header.Values(secWebSocketProtocolHeader))
}

// IsRealtimeBetaRequest reports whether the client speaks the removed beta protocol: header
// OpenAI-Beta contains "realtime=v1" (case-insensitive, any position in a comma list) or the
// subprotocol list contains RealtimeBetaSubprotocol (compared case-insensitively).
func IsRealtimeBetaRequest(r *http.Request) bool {
	for _, token := range parseHeaderTokens(r.Header.Values(openAIBetaHeader)) {
		if strings.EqualFold(token, realtimeBetaHeaderValue) {
			return true
		}
	}

	for _, token := range RealtimeSubprotocols(r) {
		if strings.EqualFold(token, RealtimeBetaSubprotocol) {
			return true
		}
	}

	return false
}

// splitRealtimeSubprotocolKeys parses every Sec-WebSocket-Protocol line and separates the
// API-key tokens from the remaining subprotocol tokens. The returned keys are the distinct,
// non-empty key values with the prefix removed; key tokens with an empty key are dropped from
// the header but do not count as a key.
func splitRealtimeSubprotocolKeys(lines []string) (remaining []string, keys []string) {
	for _, token := range parseHeaderTokens(lines) {
		key, isKey := strings.CutPrefix(token, RealtimeInsecureAPIKeyPrefix)
		if !isKey {
			remaining = append(remaining, token)
			continue
		}

		if key != "" {
			keys = append(keys, key)
		}
	}

	return remaining, lo.Uniq(keys)
}

// parseHeaderTokens splits every header line on commas and returns the trimmed, non-empty
// tokens in order.
func parseHeaderTokens(lines []string) []string {
	var tokens []string

	for _, line := range lines {
		for token := range strings.SplitSeq(line, ",") {
			if token = strings.TrimSpace(token); token != "" {
				tokens = append(tokens, token)
			}
		}
	}

	return tokens
}
