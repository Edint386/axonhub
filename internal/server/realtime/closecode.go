package realtime

import (
	"errors"
	"net"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// maxCloseReasonBytes is the largest close reason that fits a control frame (125 - 2 code bytes).
const maxCloseReasonBytes = 123

// forwardableCloseCodes are the close codes a peer may legitimately send in a close frame.
// Everything else (1004-1006, 1010, 1014, 1015, unknown values) is mapped to 1011.
var forwardableCloseCodes = map[int]struct{}{
	websocket.CloseNormalClosure:           {},
	websocket.CloseGoingAway:               {},
	websocket.CloseProtocolError:           {},
	websocket.CloseUnsupportedData:         {},
	websocket.CloseInvalidFramePayloadData: {},
	websocket.ClosePolicyViolation:         {},
	websocket.CloseMessageTooBig:           {},
	websocket.CloseInternalServerErr:       {},
	websocket.CloseServiceRestart:          {},
	websocket.CloseTryAgainLater:           {},
}

// mapCloseCode converts a close code received from one peer into one that may be sent to the
// other peer. Application codes 3000-4999 are forwarded unchanged.
func mapCloseCode(code int) int {
	if code == websocket.CloseNoStatusReceived {
		return websocket.CloseNormalClosure
	}

	if _, ok := forwardableCloseCodes[code]; ok {
		return code
	}

	if code >= 3000 && code <= 4999 {
		return code
	}

	return websocket.CloseInternalServerErr
}

// truncateReason shortens a close reason to maxCloseReasonBytes without splitting a rune.
func truncateReason(reason string) string {
	if len(reason) <= maxCloseReasonBytes {
		return reason
	}

	cut := maxCloseReasonBytes
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}

	return reason[:cut]
}

// closeInfo extracts the close code and reason from a read error. ok is false when the
// error was not a close frame (network error, read limit, timeout).
func closeInfo(err error) (code int, reason string, ok bool) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code == websocket.CloseAbnormalClosure {
			return closeErr.Code, closeErr.Text, false
		}

		return closeErr.Code, closeErr.Text, true
	}

	return 0, "", false
}

// isTimeout reports whether a read error was caused by a deadline.
func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// closePayload formats a close frame payload with a truncated reason.
func closePayload(code int, reason string) []byte {
	return websocket.FormatCloseMessage(code, truncateReason(reason))
}
