// Package turnstate treats Codex x-codex-turn-state values as opaque tickets.
// Length and envelope checks are heuristics, not an official quality signal.
package turnstate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	Header = "X-Codex-Turn-State"

	PlanPro  = "pro"
	PlanTeam = "team"

	ProLength  = 292
	TeamLength = 332
	Signal312  = 312

	statePrefix = "gAAAAA"
)

var ErrUnavailable = errors.New("codex turn-state ticket unavailable")

type Ticket struct {
	ChannelID int
	Model     string
	Plan      string
	State     string
	Version   uint64
	Issued    time.Time
	ExpiresAt time.Time
}

type Request struct {
	ChannelID   int
	Model       string
	RequestType string
}

type Source interface {
	Acquire(ctx context.Context, req Request) (*Ticket, error)
	Observe(used *Ticket, headers http.Header, actualModel string)
}

func TargetLength(plan string) int {
	if strings.EqualFold(strings.TrimSpace(plan), PlanTeam) {
		return TeamLength
	}
	return ProLength
}

func NormalizePlan(plan string) string {
	if strings.EqualFold(strings.TrimSpace(plan), PlanTeam) {
		return PlanTeam
	}
	return PlanPro
}

func Valid(state string, wantLen int) bool {
	state = strings.TrimSpace(state)
	if wantLen > 0 && len(state) != wantLen {
		return false
	}
	if len(state) < 32 || len(state) > 2048 || !strings.HasPrefix(state, statePrefix) {
		return false
	}
	if strings.ContainsAny(state, "\r\n\t ") {
		return false
	}
	for _, c := range state {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '=') {
			return false
		}
	}
	return true
}

func Is312(state string) bool {
	state = strings.TrimSpace(state)
	return len(state) == Signal312 && Valid(state, Signal312)
}

func Fingerprint(state string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(state)))
	return hex.EncodeToString(sum[:8])
}

// ParseEnvelope inspects the Fernet-like wrapper used by community tools.
// Failure is not fatal: length+prefix is still the admission rule.
func ParseEnvelope(state string) (issued time.Time, blocks int, ok bool) {
	value := strings.TrimSpace(state)
	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return time.Time{}, 0, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return time.Time{}, 0, false
	}
	unix := binary.BigEndian.Uint64(raw[1:9])
	if unix < 1577836800 || unix >= 4102444800 {
		return time.Time{}, 0, false
	}
	return time.Unix(int64(unix), 0), (len(raw) - 57) / 16, true
}

func ExpectedBlocks(plan string) int {
	if NormalizePlan(plan) == PlanTeam {
		return 12
	}
	return 10
}

func Strip(h http.Header) {
	if h == nil {
		return
	}
	for key := range h {
		if strings.EqualFold(key, Header) {
			delete(h, key)
		}
	}
}

func Inject(h http.Header, state string) {
	if h == nil {
		return
	}
	Strip(h)
	if state != "" {
		h.Set(Header, state)
	}
}

func HeaderValue(h http.Header) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(Header))
}
