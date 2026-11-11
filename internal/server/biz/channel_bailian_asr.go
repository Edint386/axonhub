package biz

// Bailian file transcription (POST /v1/audio/transcriptions for qwen-audio-3.1-asr-flash,
// qwen-audio-3.1-asr-flash-filetrans, qwen-audio-3.1-asr-flash-message, paraformer-v2 and
// paraformer-8k-v2) lives in the custom
// package internal/server/bailianasr. These helpers are its only entry points from the upstream
// channel builder: two "// fork:" hooks in channel_llm.go, guarded by
// channel_bailian_asr_fork_test.go and listed in CUSTOM_BRANCH_RULES.md section 5.

import (
	"fmt"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// bailianASRConfig leaves bailianasr.Config.CacheScope empty: the job cache is shared by every
// Bailian channel, so a client resending a request whose job is still running (or done) resumes
// it on whichever channel the load balancer picks, as long as that channel calls the same
// DashScope endpoint; per-channel scopes made such a resend pay for the audio again.
// It fails like upstream's getAPIKeyProvider when the channel has no selectable API key.
func bailianASRConfig(_ *ent.Channel, ch *Channel, baseURL string) (bailianasr.Config, error) {
	provider, err := getAPIKeyProvider(ch)
	if err != nil {
		return bailianasr.Config{}, err
	}

	return bailianasr.Config{
		BaseURL:        baseURL,
		APIKeyProvider: provider,
	}, nil
}

// wrapBailianASROutbounds puts bailianasr in front of the Outbounds entries a transcription
// request on a TypeBailian channel can reach, so the file-transcription models work without any
// endpoint setup: every entry bound to the primary outbound, and the user-configured endpoint of
// the primary format (openai/chat_completions, for example to use another region), which a
// transcription request falls back to (SelectAPIFormat) when the channel has no
// openai/audio_transcriptions endpoint. ch.Outbound itself stays the upstream Bailian
// transformer; requests are routed through Outbounds (orchestrator selectOutboundForCandidate),
// and every request other than those transcriptions reaches the wrapped outbound unchanged.
func wrapBailianASROutbounds(c *ent.Channel, ch *Channel, outbounds map[string]transformer.Outbound) {
	if c.Type != channel.TypeBailian || ch.Outbound == nil {
		return
	}

	type target struct {
		out     transformer.Outbound
		baseURL string
		wrapped transformer.Outbound
	}

	targets := []*target{{out: ch.Outbound, baseURL: c.BaseURL}}

	primaryFormat := ch.Outbound.APIFormat().String()
	if user, ok := outbounds[primaryFormat]; ok && user != ch.Outbound {
		if _, already := user.(*bailianasr.Outbound); !already {
			targets = append(targets, &target{out: user, baseURL: userEndpointBaseURL(c, primaryFormat)})
		}
	}

	for format, out := range outbounds {
		for _, t := range targets {
			if out != t.out {
				continue
			}

			if t.wrapped == nil {
				cfg, err := bailianASRConfig(c, ch, t.baseURL)
				if err != nil {
					// No selectable API key. The error depends only on the channel, so this
					// returns before any entry is wrapped; the Bailian builder already refuses
					// such a channel before reaching this hook.
					return
				}

				t.wrapped = bailianasr.Wrap(t.out, cfg)
			}

			outbounds[format] = t.wrapped
		}
	}
}

// userEndpointBaseURL is the base URL buildNonDefaultEndpointOutbound used for the user endpoint
// of format (the last one wins, as in buildChannelWithOutbounds).
func userEndpointBaseURL(c *ent.Channel, format string) string {
	baseURL := c.BaseURL

	for _, ep := range NormalizeEndpoints(c.Endpoints, c.BaseURL) {
		if ep.APIFormat != format {
			continue
		}

		baseURL = c.BaseURL
		if ep.BaseURL != "" {
			baseURL = ep.BaseURL
		}
	}

	return baseURL
}

// bailianASREndpointOutbound serves an explicit openai/audio_transcriptions endpoint on a Bailian
// channel (chat, Responses or Anthropic type). Upstream would build a plain OpenAI outbound,
// which posts to compatible-mode /audio/transcriptions and gets a 404; that outbound is kept
// behind the wrapper for every model bailianasr does not serve.
func bailianASREndpointOutbound(
	c *ent.Channel,
	ch *Channel,
	ep objects.ChannelEndpoint,
	baseURL string,
) (transformer.Outbound, bool, error) {
	if !isBailianRealtimeChannelType(c.Type) || ep.APIFormat != llm.APIFormatOpenAITranscription.String() {
		return nil, false, nil
	}

	// Same contract as upstream's withAPIKey in buildNonDefaultEndpointOutbound: an endpoint
	// that needs an API key fails closed when the channel has none.
	provider, err := getAPIKeyProvider(ch)
	if err != nil {
		return nil, true, fmt.Errorf("%w for api_format %q", err, ep.APIFormat)
	}

	inner, err := openai.NewOutboundTransformerWithConfig(&openai.Config{
		PlatformType:   openai.PlatformOpenAI,
		BaseURL:        baseURL,
		APIKeyProvider: provider,
		EndpointPath:   ep.Path,
	})
	if err != nil {
		return nil, true, err
	}

	cfg, err := bailianASRConfig(c, ch, baseURL)
	if err != nil {
		return nil, true, fmt.Errorf("%w for api_format %q", err, ep.APIFormat)
	}

	return bailianasr.Wrap(inner, cfg), true, nil
}
