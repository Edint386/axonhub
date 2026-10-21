package biz

// Realtime (OpenAI Realtime API) channel support lives in this custom (fork) file so that the
// upstream-owned endpoint registry and outbound builder stay untouched apart from the two-line
// hook in channel_llm.go buildNonDefaultEndpointOutbound (guarded by
// channel_endpoint_realtime_fork_test.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

func init() {
	// Register the realtime api_format so ValidateEndpoints accepts explicit openai/realtime
	// endpoints. Upstream tests only assert positive membership of this map.
	SupportedAPIFormats[llm.APIFormatOpenAIRealtime.String()] = struct{}{}
}

// RealtimeMode selects how the relay reaches the upstream realtime service.
type RealtimeMode string

const (
	// RealtimeModeWebSocket is a GA WebSocket session (GET /v1/realtime).
	RealtimeModeWebSocket RealtimeMode = "websocket"
	// RealtimeModeWebRTC is WebRTC call signalling (POST /v1/realtime/calls, POST /v1/live).
	RealtimeModeWebRTC RealtimeMode = "webrtc"
)

// RealtimeDialect names the upstream wire protocol spoken by a realtime target.
type RealtimeDialect string

const (
	// RealtimeDialectGA is the OpenAI Realtime GA protocol.
	RealtimeDialectGA RealtimeDialect = "ga"
	// RealtimeDialectBailian is the Alibaba Bailian Qwen-Omni-Realtime protocol.
	RealtimeDialectBailian RealtimeDialect = "bailian"
)

const (
	realtimeDefaultPath       = "/realtime"
	realtimeBailianHost       = "dashscope.aliyuncs.com"
	realtimeBailianIntlHost   = "dashscope-intl.aliyuncs.com"
	realtimeBailianIntlMarker = "dashscope-intl"
	realtimeBailianAPIPath    = "/api-ws/v1"
	realtimeOAuthFingerprint  = "oauth:"
	realtimeKeyFingerprint    = "key:"
	realtimeCodexBackendPath  = "/backend-api"
)

// RealtimeTarget describes where and how a channel serves realtime traffic.
type RealtimeTarget struct {
	// APIBase is the https API base with no trailing slash or '#', e.g. https://api.openai.com/v1
	// or https://{ws}.cn-beijing.maas.aliyuncs.com/api-ws/v1.
	APIBase string
	// WSURL is the WebSocket session URL without query, e.g. wss://api.openai.com/v1/realtime.
	// It is empty for Codex WebRTC targets, which have no WebSocket sessions.
	WSURL string
	// Dialect is the upstream wire protocol.
	Dialect RealtimeDialect
	// Explicit reports that the target came from an explicit openai/realtime endpoint.
	Explicit bool
	// Codex reports a Codex/Fenno channel (OAuth or PAT relay); call-create uses LiveBase.
	Codex bool
	// Backend reports a Codex channel whose LiveBase contains "/backend-api" (ChatGPT backend
	// JSON shape).
	Backend bool
	// LiveBase is the Codex live base from (*codex.OutboundTransformer).LiveBaseURL(); empty for
	// non-Codex channels.
	LiveBase string
	// RecordHealth reports that dial failures may feed channel health (derived
	// openai/openai_responses/bailian targets or explicit endpoints).
	RecordHealth bool
}

// RealtimeCredential is the upstream credential used for one realtime dial.
type RealtimeCredential struct {
	// Bearer is the Authorization bearer value (API key or OAuth access token).
	Bearer string
	// AccountID is the ChatGPT account id for Codex OAuth channels, else "".
	AccountID string
	// Fingerprint is a stable id of the credential: "key:" + first 12 hex of sha256(key), or
	// "oauth:<channel id>".
	Fingerprint string
	// OAuth is true for Codex OAuth credentials (re-resolve before each dial; tokens refresh).
	OAuth bool
}

// RealtimeTarget resolves the upstream realtime target of the channel for the given mode.
//
// An explicit openai/realtime endpoint always wins. Without one, only openai,
// openai_responses and the bailian* channel types derive a target from their base URL; Codex
// and Fenno channels serve WebRTC calls through the Codex live base and never WebSocket
// sessions unless an explicit endpoint plus an API key (PAT relay) is configured. Every other
// channel type requires an explicit endpoint. WebSocket mode rejects OAuth credentials.
func (ch *Channel) RealtimeTarget(mode RealtimeMode) (RealtimeTarget, error) {
	if ch == nil || ch.Channel == nil {
		return RealtimeTarget{}, errors.New("realtime: channel is nil")
	}

	switch mode {
	case RealtimeModeWebSocket, RealtimeModeWebRTC:
	default:
		return RealtimeTarget{}, fmt.Errorf("realtime: unsupported mode %q for channel %s", mode, ch.Name)
	}

	codexChannel := isCodexRealtimeChannelType(ch.Type)

	if codexChannel && mode == RealtimeModeWebRTC {
		return ch.codexRealtimeTarget()
	}

	if mode == RealtimeModeWebSocket && ch.realtimeCredentialIsOAuth() {
		return RealtimeTarget{}, fmt.Errorf("realtime: channel %s uses OAuth credentials, which only support WebRTC calls", ch.Name)
	}

	if ep, ok := ch.realtimeEndpoint(); ok {
		if codexChannel && len(ch.GetEnabledAPIKeys()) == 0 {
			return RealtimeTarget{}, fmt.Errorf("realtime: channel %s (%s) only supports WebRTC calls; WebSocket sessions need an API key relay", ch.Name, ch.Type)
		}

		target, err := ch.explicitRealtimeTarget(ep)
		if err != nil {
			return RealtimeTarget{}, err
		}

		if codexChannel {
			ch.fillCodexRealtimeFields(&target)
		}

		return target, nil
	}

	if codexChannel {
		return RealtimeTarget{}, fmt.Errorf("realtime: channel %s (%s) only supports WebRTC calls; add an openai/realtime endpoint for WebSocket sessions", ch.Name, ch.Type)
	}

	return ch.derivedRealtimeTarget()
}

// RealtimeCredential resolves the upstream credential for one dial. It never panics on a
// channel without keys: Codex/Fenno channels read the Codex token provider (OAuth tokens or
// the PAT relay key), other channels go through the regular API key provider, which also
// records contexts.ChannelAPIKey for execution rows.
func (ch *Channel) RealtimeCredential(ctx context.Context) (RealtimeCredential, error) {
	if ch == nil || ch.Channel == nil {
		return RealtimeCredential{}, errors.New("realtime: channel is nil")
	}

	if ch.apiKeyOverride != "" {
		return realtimeKeyCredential(ch.apiKeyOverride), nil
	}

	if isCodexRealtimeChannelType(ch.Type) {
		if out, ok := ch.Outbound.(*codex.OutboundTransformer); ok && out != nil {
			return ch.codexRealtimeCredential(ctx, out)
		}
	}

	if len(ch.GetEnabledAPIKeys()) == 0 {
		return RealtimeCredential{}, fmt.Errorf("realtime: channel %s has no enabled api key", ch.Name)
	}

	key := getAPIKeyProvider(ch).Get(ctx)
	if key == "" {
		return RealtimeCredential{}, fmt.Errorf("realtime: channel %s returned an empty api key", ch.Name)
	}

	return realtimeKeyCredential(key), nil
}

// HasRealtimeCredential reports whether RealtimeCredential could resolve a credential, without
// resolving one: it neither records contexts.ChannelAPIKey nor refreshes OAuth tokens, so
// candidate selectors can call it for every channel.
func (ch *Channel) HasRealtimeCredential() bool {
	if ch == nil || ch.Channel == nil {
		return false
	}

	if ch.apiKeyOverride != "" {
		return true
	}

	if isCodexRealtimeChannelType(ch.Type) {
		if out, ok := ch.Outbound.(*codex.OutboundTransformer); ok && out != nil {
			return out.TokenProvider() != nil
		}
	}

	return len(ch.GetEnabledAPIKeys()) > 0
}

// RealtimeCredentialByFingerprint re-resolves a credential previously returned by
// RealtimeCredential. It errors when the key is no longer enabled on the channel or when the
// OAuth credential is gone.
func (ch *Channel) RealtimeCredentialByFingerprint(ctx context.Context, fingerprint string) (RealtimeCredential, error) {
	if ch == nil || ch.Channel == nil {
		return RealtimeCredential{}, errors.New("realtime: channel is nil")
	}

	if fingerprint == "" {
		return RealtimeCredential{}, fmt.Errorf("realtime: empty credential fingerprint for channel %s", ch.Name)
	}

	if strings.HasPrefix(fingerprint, realtimeOAuthFingerprint) {
		cred, err := ch.RealtimeCredential(ctx)
		if err != nil {
			return RealtimeCredential{}, err
		}

		if !cred.OAuth || cred.Fingerprint != fingerprint {
			return RealtimeCredential{}, fmt.Errorf("realtime: oauth credential %s is no longer available on channel %s", fingerprint, ch.Name)
		}

		return cred, nil
	}

	if ch.apiKeyOverride != "" && realtimeFingerprintKey(ch.apiKeyOverride) == fingerprint {
		return realtimeKeyCredential(ch.apiKeyOverride), nil
	}

	for _, key := range ch.GetEnabledAPIKeys() {
		if realtimeFingerprintKey(key) != fingerprint {
			continue
		}

		contexts.WithChannelAPIKey(ctx, key)

		return realtimeKeyCredential(key), nil
	}

	return RealtimeCredential{}, fmt.Errorf("realtime: api key %s is no longer enabled on channel %s", fingerprint, ch.Name)
}

// RealtimeHeaderOverrides applies the channel's unconditional, literal header override
// operations to a realtime handshake or signalling request. Realtime runs outside the LLM
// pipeline, so conditional and templated operations are skipped (see
// ApplyModelFetchHeaderOverrides).
func (ch *Channel) RealtimeHeaderOverrides(headers http.Header) {
	if ch == nil || ch.Channel == nil || headers == nil {
		return
	}

	ApplyModelFetchHeaderOverrides(headers, ch.GetHeaderOverrideOperations())
}

// realtimeEndpoint returns the explicit openai/realtime endpoint of the channel, if any.
func (ch *Channel) realtimeEndpoint() (objects.ChannelEndpoint, bool) {
	for _, ep := range ch.ResolveEndpoints() {
		if ep.APIFormat == llm.APIFormatOpenAIRealtime.String() {
			return ep, true
		}
	}

	return objects.ChannelEndpoint{}, false
}

// realtimeCredentialIsOAuth reports whether RealtimeCredential would hand out an OAuth token.
// Only Codex/Fenno channels exchange OAuth tokens; an apiKeyOverride (channel key test flow)
// always wins.
func (ch *Channel) realtimeCredentialIsOAuth() bool {
	if ch.apiKeyOverride != "" {
		return false
	}

	return isCodexRealtimeChannelType(ch.Type) && ch.Credentials.IsOAuth()
}

func (ch *Channel) explicitRealtimeTarget(ep objects.ChannelEndpoint) (RealtimeTarget, error) {
	base := ep.BaseURL
	if base == "" {
		base = ch.BaseURL
	}

	apiBase := realtimeAPIBase(base)
	if apiBase == "" {
		return RealtimeTarget{}, fmt.Errorf("realtime: channel %s has an openai/realtime endpoint without a base URL", ch.Name)
	}

	path := ep.Path
	if path == "" {
		path = realtimeDefaultPath
	}

	dialect := RealtimeDialectGA
	if isBailianRealtimeChannelType(ch.Type) {
		dialect = RealtimeDialectBailian
	}

	return RealtimeTarget{
		APIBase:      apiBase,
		WSURL:        realtimeWebSocketScheme(apiBase) + path,
		Dialect:      dialect,
		Explicit:     true,
		RecordHealth: true,
	}, nil
}

func (ch *Channel) derivedRealtimeTarget() (RealtimeTarget, error) {
	switch ch.Type {
	case channel.TypeOpenai, channel.TypeOpenaiResponses:
		apiBase := realtimeAPIBase(ch.BaseURL)
		if apiBase == "" {
			return RealtimeTarget{}, fmt.Errorf("realtime: channel %s has no base URL", ch.Name)
		}

		return RealtimeTarget{
			APIBase:      apiBase,
			WSURL:        realtimeWebSocketScheme(apiBase) + realtimeDefaultPath,
			Dialect:      RealtimeDialectGA,
			RecordHealth: true,
		}, nil
	case channel.TypeBailian, channel.TypeBailianResponses, channel.TypeBailianAnthropic:
		host := realtimeBailianHost
		if strings.Contains(strings.ToLower(realtimeHost(ch.BaseURL)), realtimeBailianIntlMarker) {
			host = realtimeBailianIntlHost
		}

		apiBase := "https://" + host + realtimeBailianAPIPath

		return RealtimeTarget{
			APIBase:      apiBase,
			WSURL:        "wss://" + host + realtimeBailianAPIPath + realtimeDefaultPath,
			Dialect:      RealtimeDialectBailian,
			RecordHealth: true,
		}, nil
	default:
		return RealtimeTarget{}, fmt.Errorf("realtime: channel type %s has no realtime endpoint; add an openai/realtime endpoint", ch.Type)
	}
}

func (ch *Channel) codexRealtimeTarget() (RealtimeTarget, error) {
	target := RealtimeTarget{Dialect: RealtimeDialectGA}
	if !ch.fillCodexRealtimeFields(&target) {
		return RealtimeTarget{}, fmt.Errorf("realtime: channel %s (%s) has no Codex outbound", ch.Name, ch.Type)
	}

	target.APIBase = target.LiveBase

	return target, nil
}

// fillCodexRealtimeFields sets the Codex fields from the channel's Codex outbound. It reports
// false when the channel has no Codex outbound.
func (ch *Channel) fillCodexRealtimeFields(target *RealtimeTarget) bool {
	out, ok := ch.Outbound.(*codex.OutboundTransformer)
	if !ok || out == nil {
		return false
	}

	live := out.LiveBaseURL()
	if live == "" {
		return false
	}

	target.Codex = true
	target.LiveBase = live
	target.Backend = strings.Contains(live, realtimeCodexBackendPath)

	return true
}

func (ch *Channel) codexRealtimeCredential(ctx context.Context, out *codex.OutboundTransformer) (RealtimeCredential, error) {
	tokens := out.TokenProvider()
	if tokens == nil {
		return RealtimeCredential{}, fmt.Errorf("realtime: channel %s has no Codex token provider", ch.Name)
	}

	creds, err := tokens.Get(ctx)
	if err != nil {
		return RealtimeCredential{}, fmt.Errorf("realtime: channel %s codex token: %w", ch.Name, err)
	}

	if creds == nil || creds.AccessToken == "" {
		return RealtimeCredential{}, fmt.Errorf("realtime: channel %s codex token is empty", ch.Name)
	}

	accountID := codex.ExtractChatGPTAccountIDFromJWT(creds.AccessToken)
	if accountID == "" {
		// PAT relay keys are plain API keys wrapped in the token provider.
		return realtimeKeyCredential(creds.AccessToken), nil
	}

	return RealtimeCredential{
		Bearer:      creds.AccessToken,
		AccountID:   accountID,
		Fingerprint: realtimeOAuthFingerprint + strconv.Itoa(ch.ID),
		OAuth:       true,
	}, nil
}

func realtimeKeyCredential(key string) RealtimeCredential {
	return RealtimeCredential{
		Bearer:      key,
		Fingerprint: realtimeFingerprintKey(key),
	}
}

// realtimeFingerprintKey returns "key:" + the first 12 hex characters of sha256(key).
func realtimeFingerprintKey(key string) string {
	sum := sha256.Sum256([]byte(key))

	return realtimeKeyFingerprint + hex.EncodeToString(sum[:])[:12]
}

// realtimeAPIBase normalises a channel or endpoint base URL into an http(s) API base with no
// trailing slash or marker: "##" means raw (no version), "#" means no /v1 append, otherwise
// /v1 is appended when missing (transformer.NormalizeBaseURL semantics). ws(s) schemes are
// rewritten to http(s).
func realtimeAPIBase(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}

	var normalized string
	if raw, ok := strings.CutSuffix(base, "##"); ok {
		normalized = strings.TrimRight(raw, "/")
	} else {
		normalized = transformer.NormalizeBaseURL(base, "v1")
	}

	return realtimeHTTPScheme(normalized)
}

// realtimeHTTPScheme rewrites ws(s):// to http(s)://.
func realtimeHTTPScheme(u string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(u), "wss://"):
		return "https://" + u[len("wss://"):]
	case strings.HasPrefix(strings.ToLower(u), "ws://"):
		return "http://" + u[len("ws://"):]
	default:
		return u
	}
}

// realtimeWebSocketScheme rewrites http(s):// to ws(s)://.
func realtimeWebSocketScheme(u string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(u), "https://"):
		return "wss://" + u[len("https://"):]
	case strings.HasPrefix(strings.ToLower(u), "http://"):
		return "ws://" + u[len("http://"):]
	default:
		return u
	}
}

// realtimeHost returns the host of a base URL, falling back to the raw string when it cannot be
// parsed (the caller only does a substring match).
func realtimeHost(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return base
	}

	return parsed.Hostname()
}

func isCodexRealtimeChannelType(t channel.Type) bool {
	return t == channel.TypeCodex || t == channel.TypeFenno
}

func isBailianRealtimeChannelType(t channel.Type) bool {
	return t == channel.TypeBailian || t == channel.TypeBailianResponses || t == channel.TypeBailianAnthropic
}

// errRealtimeEndpointOutbound is returned by every pipeline entry point of the realtime stub
// outbound: realtime endpoints are served by the relay, never by the transformer pipeline.
var errRealtimeEndpointOutbound = errors.New("api_format openai/realtime only serves /v1/realtime; remove it from model protocol overrides")

// realtimeEndpointOutbound is the placeholder outbound stored in Channel.Outbounds for an
// explicit openai/realtime endpoint. It exists so that buildChannelWithOutbounds accepts the
// endpoint; the realtime relay reads the endpoint through RealtimeTarget instead.
type realtimeEndpointOutbound struct{}

// newRealtimeEndpointOutbound returns the stub outbound registered for openai/realtime
// endpoints by the fork hook in channel_llm.go.
func newRealtimeEndpointOutbound() transformer.Outbound {
	return realtimeEndpointOutbound{}
}

func (realtimeEndpointOutbound) APIFormat() llm.APIFormat {
	return llm.APIFormatOpenAIRealtime
}

func (realtimeEndpointOutbound) TransformRequest(context.Context, *llm.Request) (*httpclient.Request, error) {
	return nil, errRealtimeEndpointOutbound
}

func (realtimeEndpointOutbound) TransformResponse(context.Context, *httpclient.Response) (*llm.Response, error) {
	return nil, errRealtimeEndpointOutbound
}

func (realtimeEndpointOutbound) TransformStream(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return nil, errRealtimeEndpointOutbound
}

func (realtimeEndpointOutbound) TransformError(_ context.Context, httpErr *httpclient.Error) *llm.ResponseError {
	status := http.StatusInternalServerError
	if httpErr != nil && httpErr.StatusCode > 0 {
		status = httpErr.StatusCode
	}

	return &llm.ResponseError{
		StatusCode: status,
		Detail: llm.ErrorDetail{
			Message: errRealtimeEndpointOutbound.Error(),
			Type:    "invalid_request_error",
		},
	}
}

func (realtimeEndpointOutbound) AggregateStreamChunks(context.Context, *httpclient.Request, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, errRealtimeEndpointOutbound
}
