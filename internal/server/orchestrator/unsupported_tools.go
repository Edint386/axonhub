package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

func applyUnsupportedToolsPolicy(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnRawRequest("unsupported-tools-policy", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		llmReq := outbound.state.LlmRequest
		if llmReq == nil || len(llmReq.UnsupportedTools) == 0 || len(request.Body) == 0 {
			return request, nil
		}

		preserve, forwardAcrossFormats := outbound.effectiveUnsupportedToolsPolicy(ctx, systemService)
		if !preserve {
			return request, nil
		}

		if outbound.isPassThroughEnabled(ctx, systemService) {
			return request, nil
		}

		inboundFormat := llmReq.APIFormat
		outboundFormat := llm.APIFormat(request.APIFormat)
		if inboundFormat != outboundFormat && !forwardAcrossFormats {
			return request, nil
		}

		body, err := mergeUnsupportedToolsIntoBody(request.Body, llmReq.UnsupportedTools)
		if err != nil {
			log.Warn(ctx, "failed to merge unsupported tools into outbound request",
				log.String("api_format", request.APIFormat),
				log.Cause(err),
			)

			return request, nil
		}

		request.Body = body

		return request, nil
	})
}

func (p *PersistentOutboundTransformer) effectiveUnsupportedToolsPolicy(
	ctx context.Context,
	systemService *biz.SystemService,
) (bool, bool) {
	channel := p.GetCurrentChannel()

	preserve, forwardAcrossFormats := false, false
	if systemService != nil {
		if enabled, err := systemService.PreserveUnsupportedTools(ctx); err == nil {
			preserve = enabled
		} else {
			log.Warn(ctx, "failed to get preserve unsupported tools setting", log.Cause(err))
		}

		if enabled, err := systemService.ForwardUnsupportedToolsAcrossFormats(ctx); err == nil {
			forwardAcrossFormats = enabled
		} else {
			log.Warn(ctx, "failed to get forward unsupported tools across formats setting", log.Cause(err))
		}
	}

	if channel != nil && channel.Settings != nil {
		if channel.Settings.PreserveUnsupportedTools != nil {
			preserve = *channel.Settings.PreserveUnsupportedTools
		}

		if channel.Settings.ForwardUnsupportedToolsAcrossFormats != nil {
			forwardAcrossFormats = *channel.Settings.ForwardUnsupportedToolsAcrossFormats
		}
	}

	return preserve, forwardAcrossFormats
}

func mergeUnsupportedToolsIntoBody(body []byte, unsupportedTools []llm.UnsupportedTool) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("outbound request body is not valid JSON")
	}

	if len(unsupportedTools) == 0 {
		return body, nil
	}

	toolsValue := gjson.GetBytes(body, "tools")
	if toolsValue.Exists() && !toolsValue.IsArray() {
		return body, nil
	}

	var tools []json.RawMessage
	if toolsValue.Exists() {
		if err := json.Unmarshal([]byte(toolsValue.Raw), &tools); err != nil {
			return nil, fmt.Errorf("unmarshal outbound tools: %w", err)
		}
	}

	filteredUnsupportedTools := make([]llm.UnsupportedTool, 0, len(unsupportedTools))
	for _, unsupportedTool := range unsupportedTools {
		if len(unsupportedTool.Raw) == 0 {
			continue
		}

		filteredUnsupportedTools = append(filteredUnsupportedTools, unsupportedTool)
	}

	if len(filteredUnsupportedTools) == 0 {
		return body, nil
	}

	sort.SliceStable(filteredUnsupportedTools, func(i, j int) bool {
		return filteredUnsupportedTools[i].Index < filteredUnsupportedTools[j].Index
	})

	result := make([]json.RawMessage, 0, len(tools)+len(filteredUnsupportedTools))
	nextKnown := 0
	unsupportedBefore := 0

	for _, unsupportedTool := range filteredUnsupportedTools {
		knownTargetBefore := unsupportedTool.Index - unsupportedBefore
		if knownTargetBefore < nextKnown {
			knownTargetBefore = nextKnown
		}
		if knownTargetBefore > len(tools) {
			knownTargetBefore = len(tools)
		}

		for nextKnown < knownTargetBefore {
			result = append(result, tools[nextKnown])
			nextKnown++
		}

		result = append(result, append(json.RawMessage(nil), unsupportedTool.Raw...))
		unsupportedBefore++
	}

	result = append(result, tools[nextKnown:]...)

	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal merged tools: %w", err)
	}

	nextBody, err := sjson.SetRawBytes(body, "tools", data)
	if err != nil {
		return nil, fmt.Errorf("set merged tools: %w", err)
	}

	return nextBody, nil
}
