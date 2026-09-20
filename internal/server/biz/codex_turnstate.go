package biz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/openai/codex/turnstate"
)

const (
	codexTurnStateProbeURL  = "https://chatgpt.com/backend-api/codex/responses"
	codexTurnStateUserAgent = "codex_cli_rs/0.153.4"
	codexTurnStateVersion   = "0.153.4"
)

var sidPattern = regexp.MustCompile(`(?i)(-sid-)[^-]+`)

type storedTurnTicket struct {
	Ticket      turnstate.Ticket
	Fingerprint string
	Identity    string
}

type turnJobRecord struct {
	Attempts      int
	LastError     string
	CooldownUntil time.Time
}

// CodexTurnStateManager harvests and injects official Codex turn-state tickets.
type CodexTurnStateManager struct {
	channels *ChannelService
	probeURL string
	tick     time.Duration

	mu      sync.Mutex
	tickets map[string]*storedTurnTicket
	records map[string]*turnJobRecord
	jobs    map[string]struct{}
	revoked map[string]uint64
	events  map[int][]CodexTurnStateEvent
	version uint64
	sem     chan struct{}
	cancel  context.CancelFunc
}

func NewCodexTurnStateManager(channels *ChannelService) *CodexTurnStateManager {
	return &CodexTurnStateManager{
		channels: channels,
		probeURL: codexTurnStateProbeURL,
		tick:     5 * time.Second,
		tickets:  map[string]*storedTurnTicket{},
		records:  map[string]*turnJobRecord{},
		jobs:     map[string]struct{}{},
		revoked:  map[string]uint64{},
		events:   map[int][]CodexTurnStateEvent{},
		sem:      make(chan struct{}, 4),
	}
}

func (m *CodexTurnStateManager) Start() {
	if m == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(context.Background(), "codex turn-state harvest panic", log.Any("panic", recovered))
			}
		}()
		m.loop(ctx)
	}()
}

func (m *CodexTurnStateManager) Stop() {
	if m != nil && m.cancel != nil {
		m.cancel()
	}
}

func turnStateKey(channelID int, model string) string {
	return fmt.Sprintf("%d\x00%s", channelID, strings.TrimSpace(model))
}

func (m *CodexTurnStateManager) Acquire(ctx context.Context, req turnstate.Request) (*turnstate.Ticket, error) {
	if m == nil || req.ChannelID <= 0 {
		return nil, nil
	}
	ch := m.channels.GetEnabledChannel(req.ChannelID)
	if ch == nil {
		return nil, nil
	}
	cfg, ok := turnStateConfigOf(ch)
	if !ok || !cfg.CoversModel(req.Model) {
		return nil, nil
	}
	now := time.Now()
	key := turnStateKey(ch.ID, req.Model)
	fp := turnStateFingerprint(ch, cfg)
	identity := turnStateIdentity(ch)

	m.mu.Lock()
	stored := m.tickets[key]
	revoked := m.revoked[key]
	m.mu.Unlock()

	if stored != nil && stored.Fingerprint == fp && stored.Identity == identity &&
		stored.Ticket.Version != revoked && stored.Ticket.ExpiresAt.After(now) &&
		turnstate.Valid(stored.Ticket.State, turnstate.TargetLength(cfg.Plan)) {
		ticket := stored.Ticket
		return &ticket, nil
	}
	m.notify(ch, cfg, req.Model)
	if cfg.IsStrict() {
		return nil, turnstate.ErrUnavailable
	}
	return nil, nil
}

func (m *CodexTurnStateManager) Observe(used *turnstate.Ticket, headers http.Header, actualModel string) {
	if m == nil || used == nil {
		return
	}
	reason := ""
	if turnstate.Is312(turnstate.HeaderValue(headers)) {
		reason = "state_312"
	} else if strings.TrimSpace(actualModel) != "" && strings.TrimSpace(actualModel) != used.Model {
		reason = "model_mismatch"
	}
	if reason == "" {
		return
	}
	key := turnStateKey(used.ChannelID, used.Model)
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.tickets[key]
	if current == nil || current.Ticket.Version != used.Version {
		return
	}
	delete(m.tickets, key)
	m.revoked[key] = used.Version
	rec := m.records[key]
	if rec == nil {
		rec = &turnJobRecord{}
		m.records[key] = rec
	}
	rec.LastError = reason
	m.appendEventLocked(used.ChannelID, CodexTurnStateEvent{
		At:         time.Now(),
		Model:      used.Model,
		Kind:       "invalidated",
		Reason:     reason,
		StateBytes: len(used.State),
	})
	log.Warn(context.Background(), "codex turn-state ticket invalidated",
		log.Int("channel_id", used.ChannelID),
		log.String("model", used.Model),
		log.String("reason", reason),
	)
}

func (m *CodexTurnStateManager) notify(ch *Channel, cfg objects.CodexTurnStateSettings, model string) {
	m.startCollect(ch, cfg, model)
}

func (m *CodexTurnStateManager) startCollect(ch *Channel, cfg objects.CodexTurnStateSettings, model string) {
	if ch == nil {
		return
	}
	key := turnStateKey(ch.ID, model)
	m.mu.Lock()
	_, running := m.jobs[key]
	rec := m.records[key]
	cooling := rec != nil && rec.CooldownUntil.After(time.Now())
	if running || cooling {
		m.mu.Unlock()
		return
	}
	m.jobs[key] = struct{}{}
	m.mu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(context.Background(), "codex turn-state collect panic", log.Any("panic", recovered))
			}
			m.mu.Lock()
			delete(m.jobs, key)
			m.mu.Unlock()
		}()
		m.collect(context.Background(), ch, cfg, model)
	}()
}

func (m *CodexTurnStateManager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.schedule()
		}
	}
}

func (m *CodexTurnStateManager) schedule() {
	if m.channels == nil {
		return
	}
	for _, ch := range m.channels.GetEnabledChannels() {
		cfg, ok := turnStateConfigOf(ch)
		if !ok {
			continue
		}
		for _, model := range cfg.Models {
			key := turnStateKey(ch.ID, model)
			m.mu.Lock()
			stored := m.tickets[key]
			rec := m.records[key]
			_, running := m.jobs[key]
			now := time.Now()
			refreshAt := time.Time{}
			if stored != nil {
				refreshAt = stored.Ticket.ExpiresAt.Add(-time.Duration(cfg.RefreshBeforeMinutes) * time.Minute)
			}
			need := !running && (stored == nil || now.After(refreshAt) || stored.Fingerprint != turnStateFingerprint(ch, cfg))
			if rec != nil && rec.CooldownUntil.After(now) {
				need = false
			}
			m.mu.Unlock()
			if need {
				m.startCollect(ch, cfg, model)
			}
		}
	}
}

func (m *CodexTurnStateManager) collect(ctx context.Context, ch *Channel, cfg objects.CodexTurnStateSettings, model string) {
	key := turnStateKey(ch.ID, model)

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return
	}

	reason := "attempts_exhausted"
	success := false
	defer func() {
		m.mu.Lock()
		rec := m.records[key]
		if rec == nil {
			rec = &turnJobRecord{}
			m.records[key] = rec
		}
		if success {
			rec.LastError = ""
			rec.CooldownUntil = time.Time{}
		} else {
			rec.LastError = reason
			rec.CooldownUntil = time.Now().Add(time.Duration(cfg.CooldownSeconds) * time.Second)
		}
		m.mu.Unlock()
	}()

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			reason = "cancelled"
			return
		}
		m.mu.Lock()
		rec := m.records[key]
		if rec == nil {
			rec = &turnJobRecord{}
			m.records[key] = rec
		}
		rec.Attempts = attempt
		m.mu.Unlock()
		if attempt > 1 {
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				reason = "cancelled"
				return
			}
		}
		harvestURL, err := rotateHarvestProxy(cfg.HarvestProxyURL)
		if err != nil {
			reason = "invalid_harvest_proxy"
			m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "harvest", Reason: reason})
			return
		}
		started := time.Now()
		candidate, status, err := m.probe(ctx, ch, model, harvestURL, "")
		harvestMs := int(time.Since(started).Milliseconds())
		if err != nil {
			reason = probeReason(status, err)
			m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "harvest", Reason: reason, StatusCode: status, DurationMs: harvestMs})
			if isStopStatus(status) {
				return
			}
			continue
		}
		if !turnstate.Valid(candidate, turnstate.TargetLength(cfg.Plan)) {
			reason = "unexpected_state_length"
			m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "harvest", Reason: reason, StatusCode: status, DurationMs: harvestMs, StateBytes: len(candidate)})
			continue
		}
		m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "harvest", Reason: "ok", StatusCode: status, DurationMs: harvestMs, StateBytes: len(candidate)})
		started = time.Now()
		returned, status, err := m.probe(ctx, ch, model, "", candidate)
		validateMs := int(time.Since(started).Milliseconds())
		if err != nil || turnstate.Is312(returned) {
			reason = "fixed_proxy_validation_failed"
			m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "validate", Reason: reason, StatusCode: status, DurationMs: validateMs, StateBytes: len(returned)})
			if isStopStatus(status) {
				return
			}
			continue
		}
		m.recordEvent(ch.ID, CodexTurnStateEvent{At: time.Now(), Model: model, Kind: "validate", Reason: "ok", StatusCode: status, DurationMs: validateMs, StateBytes: len(returned)})
		now := time.Now()
		m.mu.Lock()
		m.version++
		stored := &storedTurnTicket{
			Ticket: turnstate.Ticket{
				ChannelID: ch.ID,
				Model:     model,
				Plan:      turnstate.NormalizePlan(cfg.Plan),
				State:     candidate,
				Version:   m.version,
				Issued:    now,
				ExpiresAt: now.Add(time.Duration(cfg.TTLMinutes) * time.Minute),
			},
			Fingerprint: turnStateFingerprint(ch, cfg),
			Identity:    turnStateIdentity(ch),
		}
		m.tickets[key] = stored
		delete(m.revoked, key)
		m.appendEventLocked(ch.ID, CodexTurnStateEvent{
			At:         now,
			Model:      model,
			Kind:       "ready",
			Reason:     "ok",
			StateBytes: len(candidate),
		})
		m.mu.Unlock()
		success = true
		log.Info(context.Background(), "codex turn-state ticket ready",
			log.Int("channel_id", ch.ID),
			log.String("model", model),
			log.Int("state_bytes", len(candidate)),
		)
		return
	}
}

func (m *CodexTurnStateManager) probe(ctx context.Context, ch *Channel, model, harvestURL, state string) (string, int, error) {
	ot, ok := ch.Outbound.(*codex.OutboundTransformer)
	if !ok || ot.TokenProvider() == nil {
		return "", 0, errors.New("codex oauth token provider unavailable")
	}
	creds, err := ot.TokenProvider().Get(ctx)
	if err != nil {
		return "", 0, err
	}
	payload := map[string]any{
		"model":        model,
		"store":        false,
		"stream":       true,
		"instructions": "Reply with exactly: pong",
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}},
		},
	}
	body, _ := json.Marshal(payload)
	headers := make(http.Header)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Content-Type", "application/json")
	headers.Set("Openai-Beta", "responses=experimental")
	headers.Set("User-Agent", codexTurnStateUserAgent)
	headers.Set("Originator", "codex_cli_rs")
	headers.Set("Version", codexTurnStateVersion)
	headers.Set(codex.SessionHeader, uuid.NewString())
	if accountID := codex.ExtractChatGPTAccountIDFromJWT(creds.AccessToken); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	if state != "" {
		turnstate.Inject(headers, state)
	} else {
		turnstate.Strip(headers)
	}
	client := ch.HTTPClient
	if client == nil {
		client = httpclient.NewHttpClient()
	}
	if harvestURL != "" {
		proxyCfg, err := proxyConfigFromHarvestURL(harvestURL)
		if err != nil {
			return "", 0, err
		}
		client = client.WithProxy(proxyCfg)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := client.Do(probeCtx, &httpclient.Request{
		Method:  http.MethodPost,
		URL:     m.probeURL,
		Headers: headers,
		Body:    body,
		Auth:    &httpclient.AuthConfig{Type: httpclient.AuthTypeBearer, APIKey: creds.AccessToken},
	})
	if err != nil {
		if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
			return "", httpErr.StatusCode, err
		}
		return "", 0, err
	}
	observer := turnstate.NewCompletionObserver(model)
	observer.Write(resp.Body)
	observer.Finish()
	complete, matches, _ := observer.Result()
	if !complete || !matches {
		return strings.TrimSpace(turnstate.HeaderValue(resp.Headers)), resp.StatusCode, errors.New("probe did not complete with requested model")
	}
	return strings.TrimSpace(turnstate.HeaderValue(resp.Headers)), resp.StatusCode, nil
}

func turnStateConfigOf(ch *Channel) (objects.CodexTurnStateSettings, bool) {
	if ch == nil || ch.Settings == nil || !ch.Settings.CodexTurnState.IsEnabled() {
		return objects.CodexTurnStateSettings{}, false
	}
	if ch.Type != channel.TypeCodex && ch.Type != channel.TypeFenno {
		return objects.CodexTurnStateSettings{}, false
	}
	if !ch.Credentials.IsOAuth() {
		return objects.CodexTurnStateSettings{}, false
	}
	if !strings.Contains(strings.ToLower(ch.BaseURL), "chatgpt.com") &&
		ch.BaseURL != "" && ch.BaseURL != "https://api.openai.com/v1" {
		return objects.CodexTurnStateSettings{}, false
	}
	cfg := ch.Settings.CodexTurnState.Normalized()
	return cfg, true
}

func turnStateFingerprint(ch *Channel, cfg objects.CodexTurnStateSettings) string {
	business := ""
	if ch != nil && ch.Settings != nil && ch.Settings.Proxy != nil {
		business = string(ch.Settings.Proxy.Type) + "\x00" + ch.Settings.Proxy.URL + "\x00" + ch.Settings.Proxy.Username
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cfg.Plan,
		cfg.HarvestProxyURL,
		fmt.Sprint(cfg.TTLMinutes),
		business,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func turnStateIdentity(ch *Channel) string {
	if ch == nil || !ch.Credentials.IsOAuth() {
		return ""
	}
	creds, err := ch.Credentials.ResolveOAuthCredentials()
	if err != nil || creds == nil {
		return ""
	}
	return codex.ExtractChatGPTAccountIDFromJWT(creds.AccessToken)
}

func rotateHarvestProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	sid := fmt.Sprint(n.Int64() + 100000)
	expanded := strings.NewReplacer("{sid}", sid, "{random}", sid).Replace(raw)
	u, err := url.Parse(expanded)
	if err != nil {
		return "", err
	}
	if (strings.HasSuffix(strings.ToLower(u.Hostname()), ".1024proxy.io") || strings.EqualFold(u.Hostname(), "1024proxy.io")) && u.User != nil {
		name := u.User.Username()
		if sidPattern.MatchString(name) {
			name = sidPattern.ReplaceAllString(name, "${1}"+sid)
		} else {
			name += "-sid-" + sid
		}
		if pass, ok := u.User.Password(); ok {
			u.User = url.UserPassword(name, pass)
		} else {
			u.User = url.User(name)
		}
	}
	return u.String(), nil
}

func proxyConfigFromHarvestURL(raw string) (*httpclient.ProxyConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("invalid harvest proxy URL")
	}
	username, password := "", ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
	}
	return &httpclient.ProxyConfig{
		Type:                   httpclient.ProxyTypeURL,
		URL:                    u.String(),
		Username:               username,
		Password:               password,
		DisableConnectionReuse: true,
	}, nil
}

func isStopStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests
}

func probeReason(status int, err error) string {
	switch status {
	case http.StatusUnauthorized:
		return "upstream_unauthorized"
	case http.StatusForbidden:
		return "upstream_forbidden"
	case http.StatusTooManyRequests:
		return "upstream_rate_limited"
	}
	if err != nil {
		return "harvest_failed"
	}
	return "harvest_failed"
}
