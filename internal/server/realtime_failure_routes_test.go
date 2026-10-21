package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// The tests in this file drive the realtime Prepare failure paths through the real routes:
// upstream rejection and failover, busy channels, unreachable upstreams, malformed client
// handshakes and the upstream error policy. They use the fakes of realtime_routes_test.go.

// rejectingRealtimeUpstream answers every request (websocket handshakes and call creates
// alike) with a fixed status, headers and body, counting the attempts.
type rejectingRealtimeUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	attempts int
}

func newRejectingRealtimeUpstream(t *testing.T, status int, contentType, body string, headers http.Header) *rejectingRealtimeUpstream {
	t.Helper()

	f := &rejectingRealtimeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.attempts++
		f.mu.Unlock()

		for name, values := range headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}

		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.server.Close)

	return f
}

func (f *rejectingRealtimeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.attempts
}

// realtimeFailureChannel describes one openai channel of the failure harness, all serving
// gpt-realtime.
type realtimeFailureChannel struct {
	name          string
	baseURL       string
	apiKey        string
	weight        int
	maxConcurrent int64
	rules         []objects.APIKeyAutoDisableRule
}

type realtimeFailureHarness struct {
	*codexRouteHarness

	channelService *biz.ChannelService
	gateway        *httptest.Server
	ids            map[string]int
}

// newRealtimeFailureHarness mounts the realtime routes over the given channels with a short
// connect budget, failover load balancing (highest ordering weight first) and every candidate
// kept, so the attempt order of the tests is deterministic.
func newRealtimeFailureHarness(t *testing.T, specs ...realtimeFailureChannel) *realtimeFailureHarness {
	t.Helper()

	// LoadConfig reads these when the routes are mounted.
	t.Setenv("AXONHUB_REALTIME_CONNECT_BUDGET", "1s")
	t.Setenv("AXONHUB_REALTIME_HANDSHAKE_TIMEOUT", "1s")

	h := newCodexRouteHarness(t)

	policy := h.system.RetryPolicyOrDefault(h.ctx)
	policy.Enabled = true
	policy.MaxChannelRetries = 2
	policy.LoadBalancerStrategy = biz.LoadBalancerStrategyFailover
	require.NoError(t, h.system.SetRetryPolicy(h.ctx, policy))

	ids := map[string]int{}

	for _, spec := range specs {
		create := h.db.Channel.Create().
			SetName(spec.name).
			SetType(channel.TypeOpenai).
			SetStatus(channel.StatusEnabled).
			SetBaseURL(spec.baseURL).
			SetCredentials(objects.ChannelCredentials{APIKey: spec.apiKey}).
			SetSupportedModels([]string{"gpt-realtime"}).
			SetDefaultTestModel("gpt-realtime").
			SetOrderingWeight(spec.weight)

		if spec.maxConcurrent > 0 {
			create.SetSettings(&objects.ChannelSettings{RateLimit: &objects.ChannelRateLimit{MaxConcurrent: lo.ToPtr(spec.maxConcurrent)}})
		}

		if len(spec.rules) > 0 {
			create.SetPolicies(objects.ChannelPolicies{
				APIKeyAutoDisableMode:  objects.APIKeyAutoDisableModeCustom,
				APIKeyAutoDisableRules: spec.rules,
			})
		}

		ids[spec.name] = create.SaveX(h.ctx).ID
	}

	channels := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           h.db,
		SystemService: h.system,
		HttpClient:    httpclient.NewHttpClient(),
	})
	t.Cleanup(channels.Stop)

	models := biz.NewModelService(biz.ModelServiceParams{Ent: h.db, SystemService: h.system, ChannelService: channels})
	usage := biz.NewUsageLogService(h.db, h.system, channels)
	selector := orchestrator.NewDefaultSelector(channels, models, h.system)
	orch := orchestrator.NewChatCompletionOrchestrator(channels, selector, h.requests, httpclient.NewHttpClient(), openai.NewInboundTransformer(), h.system, usage, nil, nil, nil, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil)

	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 5 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	return &realtimeFailureHarness{codexRouteHarness: h, channelService: channels, gateway: gateway, ids: ids}
}

func (h *realtimeFailureHarness) dial(t *testing.T, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.key.Key)

	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.gateway.URL, "http")+path, header)
}

// waitFailureCount polls the channel's in-memory health until the asynchronous performance
// consumer has recorded n failures.
func (h *realtimeFailureHarness) waitFailureCount(t *testing.T, channelID int, n int64) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		metrics, err := h.channelService.GetChannelMetrics(h.ctx, channelID)
		require.NoError(t, err)

		if metrics.FailureCount >= n {
			require.Equal(t, n, metrics.FailureCount, "exactly %d failures expected", n)
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("channel %d never reached %d recorded failures", channelID, n)
}

func (h *realtimeFailureHarness) disabledKeys(t *testing.T, channelID int) []objects.DisabledAPIKey {
	t.Helper()

	row, err := h.db.Channel.Get(h.ctx, channelID)
	require.NoError(t, err)

	return row.DisabledAPIKeys
}

func (h *realtimeFailureHarness) waitRequestRows(t *testing.T, n int) []*ent.Request {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := h.db.Request.Query().All(h.ctx)
		require.NoError(t, err)

		if len(rows) >= n {
			return rows
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("expected at least %d request rows", n)

	return nil
}

func readErrorBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	if resp == nil || resp.Body == nil {
		return ""
	}

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	return string(body)
}

const rejectingUpstreamBody = `{"error":{"message":"Incorrect API key provided: sk-proj-secret","type":"invalid_request_error","code":"invalid_api_key"}}`

func TestRealtimeFailureRoutes_UpstreamRejectionFailsOverAndCountsOnce(t *testing.T) {
	rejecting := newRejectingRealtimeUpstream(t, http.StatusUnauthorized, "application/json", rejectingUpstreamBody, nil)
	healthy := newFakeRealtimeUpstream(t)

	// A rule that disables the key after two 401s: a single rejection must leave the key
	// enabled, which fails if one handshake failure is counted twice.
	rules := []objects.APIKeyAutoDisableRule{{StatusCodes: []int{http.StatusUnauthorized}, Times: 2, Action: objects.APIKeyAutoDisableActionPermanent}}

	h := newRealtimeFailureHarness(t,
		realtimeFailureChannel{name: "rejecting", baseURL: rejecting.server.URL + "/v1", apiKey: "sk-bad", weight: 100, rules: rules},
		realtimeFailureChannel{name: "healthy", baseURL: healthy.server.URL + "/v1", apiKey: "sk-good", weight: 50},
	)

	conn, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	defer conn.Close()

	require.Equal(t, "session.created", gjson.Get(readWS(t, conn), "type").String())
	require.Equal(t, 1, rejecting.count(), "the rejecting channel is tried first and exactly once")
	require.Equal(t, "Bearer sk-good", healthy.lastSessionRequest(t).Header.Get("Authorization"))

	h.waitFailureCount(t, h.ids["rejecting"], 1)
	require.Empty(t, h.disabledKeys(t, h.ids["rejecting"]), "one failure must count once against a times=2 rule")

	healthyMetrics, err := h.channelService.GetChannelMetrics(h.ctx, h.ids["healthy"])
	require.NoError(t, err)
	require.Zero(t, healthyMetrics.FailureCount)

	// The second rejection reaches the threshold and the rule disables the key.
	conn2, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NoError(t, conn2.Close())

	h.waitFailureCount(t, h.ids["rejecting"], 2)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(h.disabledKeys(t, h.ids["rejecting"])) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	disabled := h.disabledKeys(t, h.ids["rejecting"])
	require.Len(t, disabled, 1)
	require.Equal(t, "sk-bad", disabled[0].Key)
	require.Equal(t, http.StatusUnauthorized, disabled[0].ErrorCode)
}

func TestRealtimeFailureRoutes_BusyChannelAnswers429(t *testing.T) {
	healthy := newFakeRealtimeUpstream(t)

	h := newRealtimeFailureHarness(t,
		realtimeFailureChannel{name: "single", baseURL: healthy.server.URL + "/v1", apiKey: "sk-good", weight: 100, maxConcurrent: 1},
	)

	conn, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	defer conn.Close()

	require.Equal(t, "session.created", gjson.Get(readWS(t, conn), "type").String())

	// The slot is held by the live session: the second attempt waits out the connect budget
	// and is answered 429 instead of dialing.
	_, resp, err = h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	body := readErrorBody(t, resp)
	require.Contains(t, body, "concurrency")

	// The failed attempt leaves a failed request row without a channel and no channel failure.
	rows := h.waitRequestRows(t, 1)
	require.Len(t, rows, 1)
	require.Equal(t, request.StatusFailed, rows[0].Status)
	require.Zero(t, rows[0].ChannelID)

	metrics, err := h.channelService.GetChannelMetrics(h.ctx, h.ids["single"])
	require.NoError(t, err)
	require.Zero(t, metrics.FailureCount, "a busy channel is not a failed channel")

	h.upstreamSessionCount(t, healthy, 1)
}

// upstreamSessionCount asserts how many handshakes the fake upstream received.
func (h *realtimeFailureHarness) upstreamSessionCount(t *testing.T, upstream *fakeRealtimeUpstream, n int) {
	t.Helper()

	upstream.mu.Lock()
	defer upstream.mu.Unlock()

	require.Len(t, upstream.sessionReqs, n)
}

func TestRealtimeFailureRoutes_UnreachableUpstreamAnswers502(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.URL
	closed.Close()

	h := newRealtimeFailureHarness(t,
		realtimeFailureChannel{name: "unreachable", baseURL: addr + "/v1", apiKey: "sk-gone", weight: 100},
	)

	_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	body := readErrorBody(t, resp)
	require.Equal(t, "upstream_error", gjson.Get(body, "error.type").String())
	require.NotEmpty(t, gjson.Get(body, "error.message").String())

	// One failed summary row with no channel, and one channel failure recorded as a transport
	// error (500), not as the 502 the client saw.
	rows := h.waitRequestRows(t, 1)
	require.Len(t, rows, 1)
	require.Equal(t, request.StatusFailed, rows[0].Status)
	require.Zero(t, rows[0].ChannelID)

	h.waitFailureCount(t, h.ids["unreachable"], 1)

	executions, err := h.db.RequestExecution.Query().Count(h.ctx)
	require.NoError(t, err)
	require.Zero(t, executions, "a session that never connected has no execution row")
}

func TestRealtimeFailureRoutes_MalformedHandshakeNeverDialsUpstream(t *testing.T) {
	healthy := newFakeRealtimeUpstream(t)

	h := newRealtimeFailureHarness(t,
		realtimeFailureChannel{name: "healthy", baseURL: healthy.server.URL + "/v1", apiKey: "sk-good", weight: 100},
	)

	validKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))

	tests := []struct {
		name    string
		version string
		key     string
	}{
		{name: "wrong version", version: "12", key: validKey},
		{name: "missing version", version: "", key: validKey},
		{name: "key is not base64", version: "13", key: "not-base64!"},
		{name: "key decodes to the wrong length", version: "13", key: base64.StdEncoding.EncodeToString([]byte("short"))},
		{name: "missing key", version: "13", key: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.gateway.URL+"/v1/realtime?model=gpt-realtime", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+h.key.Key)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")

			if tt.version != "" {
				req.Header.Set("Sec-Websocket-Version", tt.version)
			}

			if tt.key != "" {
				req.Header.Set("Sec-Websocket-Key", tt.key)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)

			body := readErrorBody(t, resp)
			require.Contains(t, body, "Sec-WebSocket")
		})
	}

	h.upstreamSessionCount(t, healthy, 0)

	count, err := h.db.Request.Query().Count(h.ctx)
	require.NoError(t, err)
	require.Zero(t, count, "a rejected handshake writes no request row")

	// A well-formed handshake still works.
	conn, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NoError(t, conn.Close())
}

func TestRealtimeFailureRoutes_UpstreamErrorPolicy(t *testing.T) {
	t.Run("passthrough keeps the upstream body and forwards retry and request id headers", func(t *testing.T) {
		rejecting := newRejectingRealtimeUpstream(t, http.StatusTooManyRequests, "application/json", `{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			http.Header{"Retry-After": []string{"7"}, "X-Request-Id": []string{"req_upstream_1"}})

		h := newRealtimeFailureHarness(t,
			realtimeFailureChannel{name: "rejecting", baseURL: rejecting.server.URL + "/v1", apiKey: "sk-bad", weight: 100},
		)

		_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
		require.Error(t, err)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		require.Equal(t, "7", resp.Header.Get("Retry-After"))
		require.Equal(t, "req_upstream_1", resp.Header.Get("X-Request-Id"))

		body := readErrorBody(t, resp)
		require.Equal(t, "rate_limit_exceeded", gjson.Get(body, "error.code").String())
	})

	t.Run("a non-json upstream body is wrapped in an openai error", func(t *testing.T) {
		rejecting := newRejectingRealtimeUpstream(t, http.StatusInternalServerError, "text/plain", "upstream exploded", nil)

		h := newRealtimeFailureHarness(t,
			realtimeFailureChannel{name: "rejecting", baseURL: rejecting.server.URL + "/v1", apiKey: "sk-bad", weight: 100},
		)

		_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

		body := readErrorBody(t, resp)
		require.True(t, gjson.Valid(body), body)
		require.Equal(t, "upstream exploded", gjson.Get(body, "error.message").String())
		require.Equal(t, "upstream_error", gjson.Get(body, "error.type").String())
	})

	t.Run("hidden mode masks rejected handshakes and call creates", func(t *testing.T) {
		rejecting := newRejectingRealtimeUpstream(t, http.StatusUnauthorized, "application/json", rejectingUpstreamBody, nil)

		h := newRealtimeFailureHarness(t,
			realtimeFailureChannel{name: "rejecting", baseURL: rejecting.server.URL + "/v1", apiKey: "sk-bad", weight: 100},
		)

		policy := h.system.RetryPolicyOrDefault(h.ctx)
		policy.UpstreamErrorPolicy.Mode = biz.UpstreamErrorModeHidden
		require.NoError(t, h.system.SetRetryPolicy(h.ctx, policy))

		_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
		require.Error(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		body := readErrorBody(t, resp)
		require.NotContains(t, body, "sk-proj-secret")
		require.Equal(t, biz.DefaultUpstreamErrorMessage, gjson.Get(body, "error.message").String())

		// Call creates go through the same policy even though the relay's call path marks
		// nothing: writePrepareError wraps the raw upstream error itself.
		call, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.gateway.URL+"/v1/realtime/calls?model=gpt-realtime", strings.NewReader("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"))
		require.NoError(t, err)
		call.Header.Set("Authorization", "Bearer "+h.key.Key)
		call.Header.Set("Content-Type", "application/sdp")

		callResp, err := http.DefaultClient.Do(call)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, callResp.StatusCode)

		callBody := readErrorBody(t, callResp)
		require.NotContains(t, callBody, "sk-proj-secret")
		require.Equal(t, biz.DefaultUpstreamErrorMessage, gjson.Get(callBody, "error.message").String())
	})

	t.Run("hidden mode masks transport failures but keeps gateway answers", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		addr := closed.URL
		closed.Close()

		h := newRealtimeFailureHarness(t,
			realtimeFailureChannel{name: "unreachable", baseURL: addr + "/v1", apiKey: "sk-gone", weight: 100},
		)

		policy := h.system.RetryPolicyOrDefault(h.ctx)
		policy.UpstreamErrorPolicy.Mode = biz.UpstreamErrorModeHidden
		require.NoError(t, h.system.SetRetryPolicy(h.ctx, policy))

		_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime")
		require.Error(t, err)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)

		body := readErrorBody(t, resp)
		require.NotContains(t, body, addr, "the upstream address must not leak")
		require.Equal(t, biz.DefaultUpstreamErrorMessage, gjson.Get(body, "error.message").String())

		// The call_id_not_found answer is gateway generated and stays verbatim.
		_, resp, err = h.dial(t, "/v1/realtime?call_id=rtc_unknown")
		require.Error(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.Equal(t, "call_id_not_found", gjson.Get(readErrorBody(t, resp), "error.code").String())
	})
}
