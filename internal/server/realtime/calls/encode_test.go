package calls

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var (
	openAITarget  = Target{APIBase: "https://api.openai.com/v1"}
	relayTarget   = Target{APIBase: "https://relay.example.com/v1", Codex: true}
	backendTarget = Target{Codex: true, Backend: true, OAuth: true, LiveBase: "https://chatgpt.com/backend-api/codex#"}
	// backendRelayTarget is a Codex PAT relay whose base mimics the ChatGPT backend: the
	// request shape follows the URL, the sideband base follows the (API key) credential.
	backendRelayTarget = Target{Codex: true, Backend: true, LiveBase: "https://relay.example.com/backend-api/codex"}
)

// decodeMultipart parses an encoded multipart body into name → (content type, content).
func decodeMultipart(t *testing.T, contentType string, body []byte) map[string][2]string {
	t.Helper()

	mediaType, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	require.Equal(t, MultipartBoundary, params["boundary"])

	parts := map[string][2]string{}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		data, err := io.ReadAll(part)
		require.NoError(t, err)

		require.Empty(t, part.FileName(), "parts must not carry a filename")

		parts[part.FormName()] = [2]string{part.Header.Get("Content-Type"), string(data)}
	}

	return parts
}

func codexHeaders() http.Header {
	h := http.Header{}
	h.Set("Openai-Alpha", "quicksilver=v2")
	h.Set("X-Session-Id", "sess-1")
	h.Set("Voice-Session-Id", "voice-1")
	h.Set("Session-Id", "codex-sess")
	h.Set("Thread-Id", "thread-1")
	h.Set("X-Codex-Turn-Metadata", `{"thread_source":"desktop"}`)
	h.Set("Originator", "Codex Desktop")
	h.Set("Openai-Safety-Identifier", "user-hash-1")
	h.Set("X-Oai-Attestation", "attest-token")
	h.Set("User-Agent", "codex_desktop/1.0")
	h.Set("Authorization", "Bearer client-key")
	h.Set("Openai-Beta", "realtime=v1")
	h.Set("Cookie", "secret=1")
	h.Set("Sec-WebSocket-Key", "abc")

	return h
}

func TestEncodeUpstream_Matrix(t *testing.T) {
	codexSession := `{"id":"sess_x","model":"gpt-live-1-codex","audio":{"output":{"voice":"cove"}},"delegation":{"type":"client"}}`
	v1Session := `{"type":"quicksilver","model":"gpt-realtime-1.5","audio":{"output":{"voice":"cove"}}}`
	gaSession := `{"type":"realtime","model":"gpt-realtime","audio":{"output":{"voice":"marin"}}}`

	tests := []struct {
		name        string
		variant     Variant
		session     string
		header      http.Header
		query       url.Values
		target      Target
		wantURL     string
		wantJSON    bool
		wantSession string
	}{
		{
			name: "openai variant to openai family", variant: VariantOpenAI, session: gaSession, target: openAITarget,
			query:       url.Values{"dcid": {"4"}, "model": {"gpt-realtime"}},
			wantURL:     "https://api.openai.com/v1/realtime/calls?dcid=4",
			wantSession: `{"type":"realtime","model":"actual-model","audio":{"output":{"voice":"marin"}}}`,
		},
		{
			name: "openai variant to openai family without dcid", variant: VariantOpenAI, session: gaSession, target: openAITarget,
			wantURL:     "https://api.openai.com/v1/realtime/calls",
			wantSession: `{"type":"realtime","model":"actual-model","audio":{"output":{"voice":"marin"}}}`,
		},
		{
			name: "codex-v1 to openai family", variant: VariantCodexV1, session: v1Session, target: openAITarget,
			query:       url.Values{"intent": {"quicksilver"}, "dcid": {"4"}},
			wantURL:     "https://api.openai.com/v1/realtime/calls?intent=quicksilver&architecture=avas",
			wantSession: `{"type":"quicksilver","model":"actual-model","audio":{"output":{"voice":"cove"}}}`,
		},
		{
			name: "codex-v3 to openai family", variant: VariantCodexV3, session: codexSession, header: codexHeaders(), target: openAITarget,
			wantURL:     "https://api.openai.com/v1/live",
			wantSession: `{"model":"actual-model","audio":{"output":{"voice":"cove"}},"delegation":{"type":"client"}}`,
		},
		{
			name: "openai variant to codex relay", variant: VariantOpenAI, session: gaSession, target: relayTarget,
			wantURL:     "https://relay.example.com/v1/realtime/calls",
			wantSession: `{"type":"realtime","model":"actual-model","audio":{"output":{"voice":"marin"}}}`,
		},
		{
			name: "codex-v1 to codex relay", variant: VariantCodexV1, session: v1Session, target: relayTarget,
			wantURL:     "https://relay.example.com/v1/realtime/calls?intent=quicksilver&architecture=avas",
			wantSession: `{"type":"quicksilver","model":"actual-model","audio":{"output":{"voice":"cove"}}}`,
		},
		{
			name: "codex-v3 to codex relay", variant: VariantCodexV3, session: codexSession, header: codexHeaders(), target: relayTarget,
			wantURL:     "https://relay.example.com/v1/live",
			wantSession: `{"model":"actual-model","audio":{"output":{"voice":"cove"}},"delegation":{"type":"client"}}`,
		},
		{
			// The backend only accepts intent=quicksilver together with session.type
			// "quicksilver", so the GA client's "realtime" is overwritten.
			name: "openai variant to codex backend", variant: VariantOpenAI, session: gaSession, target: backendTarget,
			wantURL:     "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas",
			wantJSON:    true,
			wantSession: `{"type":"quicksilver","model":"actual-model","audio":{"output":{"voice":"marin"}}}`,
		},
		{
			name: "openai variant to backend-shaped codex relay", variant: VariantOpenAI, session: gaSession, target: backendRelayTarget,
			wantURL:     "https://relay.example.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas",
			wantJSON:    true,
			wantSession: `{"type":"quicksilver","model":"actual-model","audio":{"output":{"voice":"marin"}}}`,
		},
		{
			name: "codex-v1 to codex backend", variant: VariantCodexV1, session: v1Session, target: backendTarget,
			wantURL:     "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas",
			wantJSON:    true,
			wantSession: `{"type":"quicksilver","model":"actual-model","audio":{"output":{"voice":"cove"}}}`,
		},
		{
			name: "codex-v3 to codex backend", variant: VariantCodexV3, session: codexSession, header: codexHeaders(), target: backendTarget,
			wantURL:     "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas",
			wantJSON:    true,
			wantSession: `{"model":"actual-model","audio":{"output":{"voice":"cove"}},"delegation":{"type":"client"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbound := &Request{SDP: testSDP, Session: json.RawMessage(tt.session), Variant: tt.variant, Model: "requested"}

			up, err := EncodeUpstream(inbound, tt.header, tt.query, tt.target, Credential{Bearer: "tok"}, EncodeOptions{ActualModel: "actual-model"})
			require.NoError(t, err)
			require.Equal(t, http.MethodPost, up.Method)
			require.Equal(t, tt.wantURL, up.URL)
			require.Equal(t, "application/sdp", up.Header.Get("Accept"))
			require.Equal(t, "Bearer tok", up.Header.Get("Authorization"))
			require.Empty(t, up.Header.Get("Openai-Beta"))
			require.Empty(t, up.Header.Get("Cookie"))
			require.Empty(t, up.Header.Get("Sec-WebSocket-Key"))
			require.Empty(t, up.Header.Get("Chatgpt-Account-Id"))

			if tt.wantJSON {
				require.Equal(t, "application/json", up.Header.Get("Content-Type"))
				require.Equal(t, testSDP, gjson.GetBytes(up.Body, "sdp").String())
				require.JSONEq(t, tt.wantSession, gjson.GetBytes(up.Body, "session").Raw)

				return
			}

			parts := decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)
			require.Len(t, parts, 2)
			require.Equal(t, "application/sdp", parts["sdp"][0])
			require.Equal(t, testSDP, parts["sdp"][1])
			require.Equal(t, "application/json", parts["session"][0])
			require.JSONEq(t, tt.wantSession, parts["session"][1])
			require.True(t, bytes.HasPrefix(up.Body, []byte("--"+MultipartBoundary+"\r\n")))
		})
	}
}

func TestEncodeUpstream_SessionRules(t *testing.T) {
	t.Run("openai variant synthesizes session without inbound session", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI, Model: "gpt-realtime"}, nil, nil, openAITarget, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "gpt-realtime-2.1"})
		require.NoError(t, err)

		parts := decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)
		require.JSONEq(t, `{"type":"realtime","model":"gpt-realtime-2.1"}`, parts["session"][1])
	})

	t.Run("openai variant keeps an explicit session type", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI, Session: json.RawMessage(`{"type":"transcription"}`)}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)

		parts := decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)
		require.JSONEq(t, `{"type":"transcription","model":"m"}`, parts["session"][1])
	})

	t.Run("codex variants do not inject a type on api shape", func(t *testing.T) {
		for _, v := range []Variant{VariantCodexV1, VariantCodexV3} {
			up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: v}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
			require.NoError(t, err)

			parts := decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)
			require.JSONEq(t, `{"model":"m"}`, parts["session"][1], "variant %s", v)
		}
	})

	t.Run("backend forces quicksilver type unless openai-alpha is present", func(t *testing.T) {
		tests := []struct {
			name    string
			alpha   string
			session string
			want    string
		}{
			{name: "no alpha no type", session: `{"model":"x"}`, want: `{"model":"actual","type":"quicksilver"}`},
			{name: "no alpha no session", want: `{"model":"actual","type":"quicksilver"}`},
			{name: "no alpha realtime type is overwritten", session: `{"type":"realtime"}`, want: `{"type":"quicksilver","model":"actual"}`},
			{name: "no alpha quicksilver type kept", session: `{"type":"quicksilver"}`, want: `{"type":"quicksilver","model":"actual"}`},
			{name: "alpha present no type", alpha: "quicksilver=v2", session: `{"model":"x"}`, want: `{"model":"actual"}`},
			{name: "alpha and type", alpha: "quicksilver=v1", session: `{"type":"quicksilver"}`, want: `{"type":"quicksilver","model":"actual"}`},
			{name: "alpha keeps a foreign type untouched", alpha: "quicksilver=v2", session: `{"type":"realtime"}`, want: `{"type":"realtime","model":"actual"}`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				h := http.Header{}
				if tt.alpha != "" {
					h.Set("Openai-Alpha", tt.alpha)
				}

				var session json.RawMessage
				if tt.session != "" {
					session = json.RawMessage(tt.session)
				}

				up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3, Session: session}, h, nil, backendTarget, Credential{Bearer: "t"}, EncodeOptions{ActualModel: "actual"})
				require.NoError(t, err)
				require.JSONEq(t, tt.want, gjson.GetBytes(up.Body, "session").Raw)
			})
		}
	})

	t.Run("session id is removed and model overwritten everywhere", func(t *testing.T) {
		for _, target := range []Target{openAITarget, relayTarget, backendTarget} {
			up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3, Session: json.RawMessage(`{"id":"sess_1","model":"client-model","type":"realtime"}`)}, nil, nil, target, Credential{}, EncodeOptions{ActualModel: "actual"})
			require.NoError(t, err)

			var session string
			if target.Backend {
				session = gjson.GetBytes(up.Body, "session").Raw
			} else {
				session = decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)["session"][1]
			}

			require.False(t, gjson.Get(session, "id").Exists())
			require.Equal(t, "actual", gjson.Get(session, "model").String())

			// Without openai-alpha the backend forces quicksilver; API-shape targets keep the type.
			if target.Backend {
				require.Equal(t, "quicksilver", gjson.Get(session, "type").String())
			} else {
				require.Equal(t, "realtime", gjson.Get(session, "type").String())
			}
		}
	})

	t.Run("actual model falls back to inbound model", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI, Model: "gpt-realtime"}, nil, nil, openAITarget, Credential{}, EncodeOptions{})
		require.NoError(t, err)

		parts := decodeMultipart(t, up.Header.Get("Content-Type"), up.Body)
		require.JSONEq(t, `{"type":"realtime","model":"gpt-realtime"}`, parts["session"][1])
	})

	t.Run("json body does not html-escape the sdp", func(t *testing.T) {
		sdp := "v=0\r\na=fingerprint:sha-256 AB<CD>&EF\r\n"

		up, err := EncodeUpstream(&Request{SDP: sdp, Variant: VariantCodexV3}, nil, nil, backendTarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Contains(t, string(up.Body), `AB<CD>&EF`)
		require.Equal(t, sdp, gjson.GetBytes(up.Body, "sdp").String())
	})
}

func TestEncodeUpstream_Headers(t *testing.T) {
	t.Run("forwarded allowlist, account id and attestation on backend", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, codexHeaders(), nil, backendTarget, Credential{Bearer: "oauth", AccountID: "acct-1"}, EncodeOptions{ActualModel: "m", ForwardAttestation: true, UserAgent: "codex_cli_rs/0.1"})
		require.NoError(t, err)

		require.Equal(t, "Bearer oauth", up.Header.Get("Authorization"))
		require.Equal(t, "acct-1", up.Header.Get("Chatgpt-Account-Id"))
		require.Equal(t, "quicksilver=v2", up.Header.Get("Openai-Alpha"))
		require.Equal(t, "sess-1", up.Header.Get("X-Session-Id"))
		require.Equal(t, "voice-1", up.Header.Get("Voice-Session-Id"))
		require.Equal(t, "codex-sess", up.Header.Get("Session-Id"))
		require.Equal(t, "thread-1", up.Header.Get("Thread-Id"))
		require.Equal(t, `{"thread_source":"desktop"}`, up.Header.Get("X-Codex-Turn-Metadata"))
		require.Equal(t, "Codex Desktop", up.Header.Get("Originator"))
		require.Equal(t, "user-hash-1", up.Header.Get("Openai-Safety-Identifier"))
		require.Equal(t, "attest-token", up.Header.Get("X-Oai-Attestation"))
		require.Equal(t, "codex_cli_rs/0.1", up.Header.Get("User-Agent"))
		require.Empty(t, up.Header.Get("Openai-Beta"))
		require.Empty(t, up.Header.Get("Cookie"))
		require.Empty(t, up.Header.Get("Sec-WebSocket-Key"))
	})

	t.Run("attestation dropped when flag off", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, codexHeaders(), nil, backendTarget, Credential{Bearer: "oauth"}, EncodeOptions{ActualModel: "m", ForwardAttestation: false})
		require.NoError(t, err)
		require.Empty(t, up.Header.Get("X-Oai-Attestation"))
		require.Equal(t, "quicksilver=v2", up.Header.Get("Openai-Alpha"))
	})

	t.Run("attestation only reaches the codex backend", func(t *testing.T) {
		// The device token is meant for the ChatGPT backend only: api.openai.com, PAT relays
		// and even a backend-shaped relay never see it unless they are the backend itself.
		for _, target := range []Target{openAITarget, relayTarget} {
			up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, codexHeaders(), nil, target, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "m", ForwardAttestation: true})
			require.NoError(t, err)
			require.Empty(t, up.Header.Get("X-Oai-Attestation"), "target %+v", target)
			require.Equal(t, "user-hash-1", up.Header.Get("Openai-Safety-Identifier"))
		}
	})

	t.Run("safety identifier forwarded on the openai flow", func(t *testing.T) {
		h := http.Header{}
		h.Set("Openai-Safety-Identifier", "hashed-user")

		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, h, nil, openAITarget, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Equal(t, "hashed-user", up.Header.Get("Openai-Safety-Identifier"))
	})

	t.Run("user agent pass-through uses client UA", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, codexHeaders(), nil, openAITarget, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "m", UserAgent: "default-ua", UserAgentPassThrough: true})
		require.NoError(t, err)
		require.Equal(t, "codex_desktop/1.0", up.Header.Get("User-Agent"))
	})

	t.Run("user agent pass-through without client UA falls back", func(t *testing.T) {
		h := codexHeaders()
		h.Del("User-Agent")

		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, h, nil, openAITarget, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "m", UserAgent: "default-ua", UserAgentPassThrough: true})
		require.NoError(t, err)
		require.Equal(t, "default-ua", up.Header.Get("User-Agent"))
	})

	t.Run("no user agent when none configured", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, codexHeaders(), nil, openAITarget, Credential{Bearer: "k"}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Empty(t, up.Header.Get("User-Agent"))
	})

	t.Run("no authorization without bearer", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Empty(t, up.Header.Get("Authorization"))
	})
}

func TestEncodeUpstream_Bases(t *testing.T) {
	t.Run("backend base trims hash and maps wss", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, nil, nil, Target{Codex: true, Backend: true, LiveBase: "wss://chatgpt.com/backend-api/codex##"}, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Equal(t, "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas", up.URL)
	})

	t.Run("api base trims trailing slash and drops query", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantCodexV3}, nil, nil, Target{APIBase: "https://api.openai.com/v1/?x=1"}, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Equal(t, "https://api.openai.com/v1/live", up.URL)
	})

	t.Run("backend falls back to api base", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, nil, nil, Target{Codex: true, Backend: true, APIBase: "https://chatgpt.com/backend-api/codex"}, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Equal(t, "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas", up.URL)
	})

	t.Run("errors", func(t *testing.T) {
		_, err := EncodeUpstream(nil, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.Error(t, err)

		_, err = EncodeUpstream(&Request{Variant: VariantOpenAI}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.ErrorIs(t, err, ErrMissingSDP)

		_, err = EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, nil, nil, openAITarget, Credential{}, EncodeOptions{})
		require.ErrorContains(t, err, "no model")

		_, err = EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, nil, nil, Target{}, Credential{}, EncodeOptions{ActualModel: "m"})
		require.ErrorContains(t, err, "empty base url")

		_, err = EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI}, nil, nil, Target{APIBase: "ftp://x/v1"}, Credential{}, EncodeOptions{ActualModel: "m"})
		require.ErrorContains(t, err, "unsupported scheme")

		_, err = EncodeUpstream(&Request{SDP: testSDP, Variant: Variant("bogus")}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.ErrorContains(t, err, "unknown variant")

		_, err = EncodeUpstream(&Request{SDP: testSDP, Variant: VariantOpenAI, Session: json.RawMessage(`[1]`)}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.ErrorIs(t, err, ErrInvalidBody)
	})

	t.Run("empty variant defaults to openai", func(t *testing.T) {
		up, err := EncodeUpstream(&Request{SDP: testSDP}, nil, nil, openAITarget, Credential{}, EncodeOptions{ActualModel: "m"})
		require.NoError(t, err)
		require.Equal(t, "https://api.openai.com/v1/realtime/calls", up.URL)
	})
}

func TestParseCallID(t *testing.T) {
	upstream, err := url.Parse("https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas")
	require.NoError(t, err)

	tests := []struct {
		name     string
		location string
		upstream *url.URL
		wantID   string
		wantOK   bool
	}{
		{name: "relative live", location: "/v1/live/rtc_e2e", upstream: upstream, wantID: "rtc_e2e", wantOK: true},
		{name: "relative calls", location: "/v1/realtime/calls/rtc_u1_9c6574da8b8a41a18da9308f4ad974ce", upstream: upstream, wantID: "rtc_u1_9c6574da8b8a41a18da9308f4ad974ce", wantOK: true},
		{name: "absolute", location: "https://api.openai.com/v1/realtime/calls/rtc_abc", upstream: upstream, wantID: "rtc_abc", wantOK: true},
		{name: "double calls", location: "/v1/realtime/calls/calls/rtc_x", upstream: upstream, wantID: "rtc_x", wantOK: true},
		{name: "query and fragment ignored", location: "/v1/live/rtc_q?x=1&call_id=rtc_other#frag", upstream: upstream, wantID: "rtc_q", wantOK: true},
		{name: "trailing slash", location: "/v1/live/rtc_t/", upstream: upstream, wantID: "rtc_t", wantOK: true},
		{name: "bare relative segment", location: "rtc_bare", upstream: upstream, wantID: "rtc_bare", wantOK: true},
		{name: "uuid", location: "/v1/live/123e4567-e89b-12d3-a456-426614174000", upstream: upstream, wantID: "123e4567-e89b-12d3-a456-426614174000", wantOK: true},
		{name: "uuid uppercase", location: "/v1/live/123E4567-E89B-12D3-A456-426614174000", upstream: nil, wantID: "123E4567-E89B-12D3-A456-426614174000", wantOK: true},
		{name: "query form", location: "/v1/realtime?call_id=rtc_query", upstream: upstream, wantID: "rtc_query", wantOK: true},
		{name: "query only", location: "?call_id=rtc_only", upstream: upstream, wantID: "rtc_only", wantOK: true},
		{name: "nil upstream relative", location: "/v1/live/rtc_nil", upstream: nil, wantID: "rtc_nil", wantOK: true},
		{name: "garbage segment", location: "/v1/realtime/calls/", upstream: upstream, wantOK: false},
		{name: "garbage words", location: "/v1/realtime/calls/notacall", upstream: upstream, wantOK: false},
		{name: "bad uuid", location: "/v1/live/123e4567-e89b-12d3-a456-42661417400g", upstream: upstream, wantOK: false},
		{name: "rtc prefix only", location: "/v1/live/rtc_", upstream: upstream, wantOK: false},
		{name: "empty", location: "   ", upstream: upstream, wantOK: false},
		{name: "unparseable", location: "http://[::1", upstream: upstream, wantOK: false},
		{name: "empty call_id query", location: "/v1/realtime?call_id=", upstream: upstream, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := ParseCallID(tt.location, tt.upstream)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantID, id)
		})
	}
}

func TestRewriteLocation(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		variant Variant
		id      string
		want    string
	}{
		{name: "v3 under v1", prefix: "/v1", variant: VariantCodexV3, id: "rtc_a", want: "/v1/live/rtc_a"},
		{name: "v3 under codex", prefix: "/codex", variant: VariantCodexV3, id: "rtc_a", want: "/codex/live/rtc_a"},
		{name: "v1 under v1", prefix: "/v1", variant: VariantCodexV1, id: "rtc_b", want: "/v1/realtime/calls/rtc_b"},
		{name: "openai under codex", prefix: "/codex/", variant: VariantOpenAI, id: "rtc_c", want: "/codex/realtime/calls/rtc_c"},
		{name: "empty prefix", prefix: "", variant: VariantOpenAI, id: "rtc_d", want: "/realtime/calls/rtc_d"},
		{name: "prefix without slash", prefix: "v1", variant: VariantOpenAI, id: "rtc_e", want: "/v1/realtime/calls/rtc_e"},
		{name: "id is path-escaped", prefix: "/v1", variant: VariantCodexV3, id: "rtc_a/b", want: "/v1/live/rtc_a%2Fb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, RewriteLocation(tt.prefix, tt.variant, tt.id))
		})
	}
}

func TestForwardedResponseHeaders(t *testing.T) {
	upstream := http.Header{}
	upstream.Set("Content-Type", "application/sdp")
	upstream.Set("Location", "/v1/realtime/calls/rtc_1")
	upstream.Set("Retry-After", "3")
	upstream.Set("X-Request-ID", "req-1")
	upstream.Set("Openai-Request-Id", "oai-1")
	upstream.Set("Set-Cookie", "a=b")
	upstream.Set("Cf-Ray", "ray")
	upstream.Set("Content-Length", "123")

	got := ForwardedResponseHeaders(upstream)
	require.Equal(t, "application/sdp", got.Get("Content-Type"))
	require.Equal(t, "/v1/realtime/calls/rtc_1", got.Get("Location"))
	require.Equal(t, "3", got.Get("Retry-After"))
	require.Equal(t, "req-1", got.Get("X-Request-ID"))
	require.Equal(t, "oai-1", got.Get("Openai-Request-Id"))
	require.Equal(t, "Location", got.Get("Access-Control-Expose-Headers"))
	require.Empty(t, got.Get("Set-Cookie"))
	require.Empty(t, got.Get("Cf-Ray"))
	require.Empty(t, got.Get("Content-Length"))
	require.Len(t, got, 6)

	t.Run("without location", func(t *testing.T) {
		got := ForwardedResponseHeaders(http.Header{"Content-Type": {"application/json"}})
		require.Equal(t, "application/json", got.Get("Content-Type"))
		require.Empty(t, got.Get("Access-Control-Expose-Headers"))
		require.Len(t, got, 1)
	})

	t.Run("nil upstream", func(t *testing.T) {
		require.Empty(t, ForwardedResponseHeaders(nil))
	})
}

func TestSidebandURL(t *testing.T) {
	tests := []struct {
		name    string
		variant Variant
		target  Target
		id      string
		want    string
		wantErr string
	}{
		{name: "v3 backend", variant: VariantCodexV3, target: backendTarget, id: "rtc_1", want: "wss://api.openai.com/v1/live/rtc_1"},
		{name: "v1 backend", variant: VariantCodexV1, target: backendTarget, id: "rtc_1", want: "wss://api.openai.com/v1/realtime?intent=quicksilver&call_id=rtc_1"},
		// Backend calls are created with intent=quicksilver, so their sideband carries it too.
		{name: "openai backend", variant: VariantOpenAI, target: backendTarget, id: "rtc_1", want: "wss://api.openai.com/v1/realtime?intent=quicksilver&call_id=rtc_1"},
		// An API-key relay keeps its sideband on the channel base even when the base looks
		// like the ChatGPT backend: the relay key must never be sent to api.openai.com.
		{name: "v3 backend-shaped relay", variant: VariantCodexV3, target: backendRelayTarget, id: "rtc_1", want: "wss://relay.example.com/backend-api/codex/live/rtc_1"},
		{name: "v1 backend-shaped relay", variant: VariantCodexV1, target: backendRelayTarget, id: "rtc_1", want: "wss://relay.example.com/backend-api/codex/realtime?intent=quicksilver&call_id=rtc_1"},
		{name: "openai backend-shaped relay", variant: VariantOpenAI, target: backendRelayTarget, id: "rtc_1", want: "wss://relay.example.com/backend-api/codex/realtime?intent=quicksilver&call_id=rtc_1"},
		// OAuth decides the base, not the URL shape: an OAuth credential on a custom base
		// still dials api.openai.com like Codex does.
		{name: "v3 oauth custom base", variant: VariantCodexV3, target: Target{Codex: true, OAuth: true, APIBase: "https://relay.example.com/v1"}, id: "rtc_1", want: "wss://api.openai.com/v1/live/rtc_1"},
		{name: "oauth without any base", variant: VariantOpenAI, target: Target{Codex: true, OAuth: true}, id: "rtc_1", want: "wss://api.openai.com/v1/realtime?call_id=rtc_1"},
		{name: "v3 openai family", variant: VariantCodexV3, target: openAITarget, id: "rtc_2", want: "wss://api.openai.com/v1/live/rtc_2"},
		{name: "v1 openai family", variant: VariantCodexV1, target: openAITarget, id: "rtc_2", want: "wss://api.openai.com/v1/realtime?intent=quicksilver&call_id=rtc_2"},
		{name: "openai openai family", variant: VariantOpenAI, target: openAITarget, id: "rtc_2", want: "wss://api.openai.com/v1/realtime?call_id=rtc_2"},
		{name: "v3 relay", variant: VariantCodexV3, target: relayTarget, id: "rtc_3", want: "wss://relay.example.com/v1/live/rtc_3"},
		{name: "v1 relay", variant: VariantCodexV1, target: relayTarget, id: "rtc_3", want: "wss://relay.example.com/v1/realtime?intent=quicksilver&call_id=rtc_3"},
		{name: "openai relay", variant: VariantOpenAI, target: relayTarget, id: "rtc_3", want: "wss://relay.example.com/v1/realtime?call_id=rtc_3"},
		{name: "http base maps to ws", variant: VariantOpenAI, target: Target{APIBase: "http://localhost:8090/v1/"}, id: "rtc_4", want: "ws://localhost:8090/v1/realtime?call_id=rtc_4"},
		{name: "wss base kept and hash trimmed", variant: VariantCodexV3, target: Target{APIBase: "wss://relay.example.com/v1#", Codex: true}, id: "rtc_5", want: "wss://relay.example.com/v1/live/rtc_5"},
		{name: "id escaped", variant: VariantOpenAI, target: openAITarget, id: "rtc a&b", want: "wss://api.openai.com/v1/realtime?call_id=rtc+a%26b"},
		{name: "empty id", variant: VariantOpenAI, target: openAITarget, id: "", wantErr: "empty call id"},
		{name: "missing base", variant: VariantOpenAI, target: Target{}, id: "rtc_6", wantErr: "empty base url"},
		{name: "unknown variant", variant: Variant("bogus"), target: openAITarget, id: "rtc_7", wantErr: "unknown variant"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SidebandURL(tt.variant, tt.target, tt.id)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSidebandHeaders(t *testing.T) {
	t.Run("credential and allowlist", func(t *testing.T) {
		h := SidebandHeaders(codexHeaders(), Credential{Bearer: "oauth", AccountID: "acct"}, EncodeOptions{UserAgent: "codex_cli_rs/0.1"})
		require.Equal(t, "Bearer oauth", h.Get("Authorization"))
		require.Equal(t, "acct", h.Get("Chatgpt-Account-Id"))
		require.Equal(t, "quicksilver=v2", h.Get("Openai-Alpha"))
		require.Equal(t, "sess-1", h.Get("X-Session-Id"))
		require.Equal(t, "voice-1", h.Get("Voice-Session-Id"))
		require.Equal(t, "codex-sess", h.Get("Session-Id"))
		require.Equal(t, "thread-1", h.Get("Thread-Id"))
		require.Equal(t, `{"thread_source":"desktop"}`, h.Get("X-Codex-Turn-Metadata"))
		require.Equal(t, "Codex Desktop", h.Get("Originator"))
		require.Equal(t, "user-hash-1", h.Get("Openai-Safety-Identifier"))
		require.Equal(t, "codex_cli_rs/0.1", h.Get("User-Agent"))
		require.Empty(t, h.Get("X-Oai-Attestation"))
		require.Empty(t, h.Get("Sec-WebSocket-Key"))
		require.Empty(t, h.Get("Cookie"))
		require.Empty(t, h.Get("Openai-Beta"))
	})

	t.Run("user agent pass-through", func(t *testing.T) {
		h := SidebandHeaders(codexHeaders(), Credential{Bearer: "k"}, EncodeOptions{UserAgent: "default", UserAgentPassThrough: true})
		require.Equal(t, "codex_desktop/1.0", h.Get("User-Agent"))
		require.Empty(t, h.Get("Chatgpt-Account-Id"))
	})

	t.Run("nil inbound", func(t *testing.T) {
		h := SidebandHeaders(nil, Credential{Bearer: "k"}, EncodeOptions{})
		require.Equal(t, "Bearer k", h.Get("Authorization"))
		require.Len(t, h, 1)
	})

	t.Run("multi-valued headers are all copied", func(t *testing.T) {
		in := http.Header{}
		in.Add("X-Session-Id", "a")
		in.Add("X-Session-Id", "b")

		h := SidebandHeaders(in, Credential{}, EncodeOptions{})
		require.Equal(t, []string{"a", "b"}, h.Values("X-Session-Id"))
	})
}

func TestBaseNormalization(t *testing.T) {
	tests := []struct {
		in      string
		wantH   string
		wantWS  string
		wantErr bool
	}{
		{in: "https://api.openai.com/v1", wantH: "https://api.openai.com/v1", wantWS: "wss://api.openai.com/v1"},
		{in: "https://chatgpt.com/backend-api/codex#", wantH: "https://chatgpt.com/backend-api/codex", wantWS: "wss://chatgpt.com/backend-api/codex"},
		{in: "https://chatgpt.com/backend-api/codex##", wantH: "https://chatgpt.com/backend-api/codex", wantWS: "wss://chatgpt.com/backend-api/codex"},
		{in: "wss://host/v1/", wantH: "https://host/v1", wantWS: "wss://host/v1"},
		{in: "ws://host:8090/v1", wantH: "http://host:8090/v1", wantWS: "ws://host:8090/v1"},
		{in: "HTTPS://Host/v1", wantH: "https://Host/v1", wantWS: "wss://Host/v1"},
		{in: "  https://host/v1?x=1#f  ", wantH: "https://host/v1", wantWS: "wss://host/v1"},
		{in: "", wantErr: true},
		{in: "/v1", wantErr: true},
		{in: "ftp://host/v1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.in), func(t *testing.T) {
			h, errH := httpBase(tt.in)
			ws, errWS := wsBase(tt.in)

			if tt.wantErr {
				require.Error(t, errH)
				require.Error(t, errWS)

				return
			}

			require.NoError(t, errH)
			require.NoError(t, errWS)
			require.Equal(t, tt.wantH, h)
			require.Equal(t, tt.wantWS, ws)
		})
	}
}
