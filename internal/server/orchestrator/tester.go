package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xjson"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/decisions"
	"github.com/looplj/axonhub/llm/transformer/typesafe"
)

const testChannelAPIKeysMaxConcurrency = 8

const responsesWebSocketTestPrompt = "ping"

// TestChannelOrchestrator handles channel testing functionality.
// It is stateless and can be reused across multiple test requests.
type TestChannelOrchestrator struct {
	channelService              *biz.ChannelService
	requestService              *biz.RequestService
	systemService               *biz.SystemService
	usageLogService             *biz.UsageLogService
	promptProtectionRuleService *biz.PromptProtectionRuleService
	httpClient                  *httpclient.HttpClient
	modelCircuitBreaker         *biz.ModelCircuitBreaker
	modelMapper                 *ModelMapper
	loadBalancer                *LoadBalancer
	channelLimiterManager       *ChannelLimiterManager
}

// NewTestChannelOrchestrator creates a new TestChannelOrchestrator.
func NewTestChannelOrchestrator(
	channelService *biz.ChannelService,
	requestService *biz.RequestService,
	systemService *biz.SystemService,
	usageLogService *biz.UsageLogService,
	promptProtectionRuleService *biz.PromptProtectionRuleService,
	httpClient *httpclient.HttpClient,
) *TestChannelOrchestrator {
	return &TestChannelOrchestrator{
		channelService:              channelService,
		requestService:              requestService,
		systemService:               systemService,
		usageLogService:             usageLogService,
		promptProtectionRuleService: promptProtectionRuleService,
		httpClient:                  httpClient,
		modelCircuitBreaker:         biz.NewModelCircuitBreaker(),
		modelMapper:                 NewModelMapper(),
		loadBalancer:                NewLoadBalancer(systemService, channelService, NewWeightStrategy()),
		channelLimiterManager:       NewChannelLimiterManager(),
	}
}

// TestChannelRequest represents a channel test request.
type TestChannelRequest struct {
	ChannelID objects.GUID
	ModelID   *string
}

// TestChannelOptions controls request behavior that callers may need to force.
// A nil Stream preserves the channel policy used by the existing test API.
type TestChannelOptions struct {
	Stream *bool
}

// buildChannelTestRequest creates the request used by channel tests.
func buildChannelTestRequest(model string, useStream bool, systemPrompt string, userPrompt string, responsesWebSocket bool, apiFormat llm.APIFormat) *llm.Request {
	if apiFormat == llm.APIFormatOpenAIDecisions {
		body := xjson.MustMarshal(map[string]any{
			"model": model,
			"input": userPrompt,
			"questions": []map[string]any{{
				"name":      "answer",
				"type":      "predicate",
				"predicate": "Is this a channel connectivity test?",
			}},
		})
		return &llm.Request{
			Model:       model,
			RequestType: llm.RequestTypeDecisions,
			APIFormat:   llm.APIFormatOpenAIDecisions,
			Stream:      lo.ToPtr(false),
			Decisions: &llm.DecisionsRequest{
				Body: body,
			},
		}
	}

	req := &llm.Request{
		Model: model,
		Messages: []llm.Message{
			{
				Role:    "system",
				Content: llm.MessageContent{Content: lo.ToPtr(systemPrompt)},
			},
			{
				Role:    "user",
				Content: llm.MessageContent{Content: lo.ToPtr(userPrompt)},
			},
		},
		MaxCompletionTokens: lo.ToPtr(int64(256)),
		Stream:              lo.ToPtr(useStream),
	}

	if responsesWebSocket {
		req.Messages = []llm.Message{{
			Role:    "user",
			Content: llm.MessageContent{Content: lo.ToPtr(responsesWebSocketTestPrompt)},
		}}
		req.MaxCompletionTokens = nil
		req.Stream = lo.ToPtr(true)
	}

	return req
}

func channelTestAPIFormat(channel *biz.Channel, model string) llm.APIFormat {
	if channel != nil {
		endpoints := channel.ResolveEndpoints()
		entry, ok := channel.GetModelEntries()[model]
		if !ok {
			entry = channel.GetDirectModelEntries()[model]
		}
		forced := forcedAPIFormatsForCandidate(channel, []biz.ChannelModelEntry{entry}, model)
		if filtered := FilterEndpointsByAPIFormats(endpoints, forced); len(filtered) > 0 {
			endpoints = filtered[:1]
		}
		for _, endpoint := range endpoints {
			if endpoint.APIFormat == llm.APIFormatOpenAIDecisions.String() {
				return llm.APIFormatOpenAIDecisions
			}
			if endpoint.APIFormat == llm.APIFormatTypeSafeSystemOne.String() {
				return llm.APIFormatTypeSafeSystemOne
			}
		}
	}
	return llm.APIFormatOpenAIChatCompletion
}

func channelTestInbound(apiFormat llm.APIFormat) transformer.Inbound {
	if apiFormat == llm.APIFormatOpenAIDecisions {
		return decisions.NewInboundTransformer()
	}
	return openai.NewInboundTransformer()
}

func parseDecisionsTestResponse(body []byte) (string, error) {
	var response llm.DecisionsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("invalid Decisions response: %w", err)
	}
	if len(response.Answers) == 0 {
		return "", fmt.Errorf("no answers in Decisions response")
	}
	return string(body), nil
}

// usesResponsesWebSocket reports whether a channel routes Responses requests over WebSocket.
func usesResponsesWebSocket(channel *biz.Channel) bool {
	if channel == nil {
		return false
	}

	for _, endpoint := range channel.ResolveEndpoints() {
		if endpoint.APIFormat != llm.APIFormatOpenAIResponse.String() && endpoint.APIFormat != llm.APIFormatOpenAIResponseCompact.String() {
			continue
		}

		transport := strings.ToLower(strings.TrimSpace(endpoint.Transport))
		if transport == objects.ChannelEndpointTransportWebSocket {
			return true
		}
		if transport != "" {
			continue
		}

		baseURL := endpoint.BaseURL
		if baseURL == "" {
			baseURL = channel.BaseURL
		}
		baseURL = strings.ToLower(strings.TrimSpace(baseURL))
		if strings.HasPrefix(baseURL, "ws://") || strings.HasPrefix(baseURL, "wss://") {
			return true
		}
	}

	return false
}

func (processor *TestChannelOrchestrator) buildChannelTestInput(ctx context.Context, model string, useStream bool, systemPrompt, userPrompt string, responsesWebSocket bool, apiFormat llm.APIFormat) (transformer.Inbound, []byte, error) {
	if apiFormat == llm.APIFormatTypeSafeSystemOne {
		if useStream {
			return nil, nil, fmt.Errorf("systemone does not support streaming")
		}
		// Protect role-scoped prompts before System One combines them into State.
		prompts := buildChannelTestRequest(model, false, systemPrompt, userPrompt, false, llm.APIFormatOpenAIChatCompletion)
		protected, err := processor.promptProtectionRuleService.Protect(ctx, prompts)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(struct {
			llm.SystemOneRequest

			Model string `json:"model"`
		}{
			Model: model,
			SystemOneRequest: llm.SystemOneRequest{
				State: lo.FromPtr(protected.Messages[0].Content.Content) + "\n\n" + lo.FromPtr(protected.Messages[1].Content.Content),
				Questions: map[string]llm.SystemOneQuestion{
					"connection": {Type: "noul", Instructions: "Does the state contain a test prompt?"},
				},
			},
		})
		return typesafe.NewSystemOneInboundTransformer(), body, err
	}
	request := buildChannelTestRequest(model, useStream, systemPrompt, userPrompt, responsesWebSocket, apiFormat)
	body, err := json.Marshal(request)
	return channelTestInbound(apiFormat), body, err
}

func channelTestResponseMessage(body []byte, apiFormat llm.APIFormat) (*string, error) {
	if apiFormat == llm.APIFormatTypeSafeSystemOne {
		response, err := xjson.To[llm.SystemOneResponse](body)
		if err != nil {
			return nil, err
		}
		if len(response.Answers) == 0 {
			return nil, fmt.Errorf("no answers in System One response")
		}
		if _, ok := response.Answers["connection"]; !ok {
			return nil, fmt.Errorf("no connection answer in System One response")
		}
		answers, err := json.Marshal(response.Answers)
		if err != nil {
			return nil, err
		}
		return lo.ToPtr(string(answers)), nil
	}
	response, err := xjson.To[llm.Response](body)
	if err != nil {
		return nil, err
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		return nil, fmt.Errorf("No message in response")
	}
	return response.Choices[0].Message.Content.Content, nil
}

// TestChannelResult represents the result of a channel test.
type TestChannelResult struct {
	// Latency is kept in seconds for compatibility with the existing API.
	Latency float64
	// TTFBMs is the elapsed time until the first decoded upstream stream event.
	// For non-streaming responses it is a documented fallback to TotalMs because
	// the provider response is delivered as one complete body by the pipeline.
	TTFBMs *float64
	// TTFTMs is the elapsed time until the first response chunk containing
	// deliverable output. Role, usage-only, and empty delta chunks do not count.
	TTFTMs *float64
	// TotalMs is the elapsed time until the response has been fully consumed.
	TotalMs float64
	Stream  bool
	Success bool
	Message *string
	Error   *string
}

// newTestChannelResult creates a result with a consistent timing snapshot.
// Timing fields are intentionally measured at the point a result is returned,
// so failures before an upstream response still report their total duration.
func newTestChannelResult(startTime time.Time, useStream bool) *TestChannelResult {
	result := &TestChannelResult{Stream: useStream}
	finalizeTestChannelResult(result, startTime)

	return result
}

func finalizeTestChannelResult(result *TestChannelResult, startTime time.Time) {
	if result == nil {
		return
	}

	elapsed := time.Since(startTime)
	result.Latency = elapsed.Seconds()
	result.TotalMs = float64(elapsed) / float64(time.Millisecond)
}

func markTestChannelFirstByte(result *TestChannelResult, startTime time.Time) {
	if result == nil || result.TTFBMs != nil {
		return
	}

	value := float64(time.Since(startTime)) / float64(time.Millisecond)
	result.TTFBMs = &value
}

func markTestChannelFirstToken(result *TestChannelResult, startTime time.Time) {
	if result == nil || result.TTFTMs != nil {
		return
	}

	value := float64(time.Since(startTime)) / float64(time.Millisecond)
	result.TTFTMs = &value
}

func setNonStreamingFirstByte(result *TestChannelResult) {
	if result == nil || result.TTFBMs != nil {
		return
	}

	value := result.TotalMs
	result.TTFBMs = &value
}

func testChannelErrorResult(startTime time.Time, useStream bool, message string) *TestChannelResult {
	result := newTestChannelResult(startTime, useStream)
	result.Success = false
	result.Message = new("")
	result.Error = new(message)

	return result
}

// TestChannel tests a specific channel with a simple request.
func (processor *TestChannelOrchestrator) TestChannel(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (*TestChannelResult, error) {
	return processor.TestChannelWithOptions(ctx, channelID, modelID, proxy, TestChannelOptions{})
}

// TestChannelWithOptions tests a channel while allowing active health probes
// to choose streaming independently for each configured model.
func (processor *TestChannelOrchestrator) TestChannelWithOptions(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
	options TestChannelOptions,
) (*TestChannelResult, error) {
	channel, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}
	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = channel.DefaultTestModel
	}
	systemPrompt, userPrompt, err := processor.systemService.ChannelTestPrompts(ctx)
	if err != nil {
		return nil, err
	}
	useStream := channel.Policies.Stream == objects.CapabilityPolicyRequire
	if options.Stream != nil {
		useStream = *options.Stream
	}
	apiFormat := channelTestAPIFormat(channel, testModel)
	inbound, body, err := processor.buildChannelTestInput(ctx, testModel, useStream, systemPrompt, userPrompt, usesResponsesWebSocket(channel), apiFormat)
	if err != nil {
		return nil, err
	}
	// Create ChatCompletionOrchestrator for this test request
	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: NewSpecifiedChannelSelector(processor.channelService, channelID),
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: processor.promptProtectionRuleService,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
		Inbound:                    inbound,
		SystemService:              processor.systemService,
		UsageLogService:            processor.usageLogService,
		proxy:                      proxy,
		ModelMapper:                processor.modelMapper,
		adaptiveLoadBalancer:       processor.loadBalancer,
		failoverLoadBalancer:       processor.loadBalancer,
		circuitBreakerLoadBalancer: processor.loadBalancer,
		channelLimiterManager:      processor.channelLimiterManager,
		modelCircuitBreaker:        processor.modelCircuitBreaker,
	}

	// Measure latency
	startTime := time.Now()
	rawResponse, err := chatProcessor.Process(ctx, &httpclient.Request{
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: body,
	})

	if err != nil {
		rawErr := inbound.TransformError(ctx, err)
		message := gjson.GetBytes(rawErr.Body, "error.message").String()

		return testChannelErrorResult(startTime, useStream, message), nil
	}

	// Handle streaming response
	if rawResponse.ChatCompletionStream != nil {
		return processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime)
	}

	return processor.handleNonStreamingResponse(rawResponse, startTime, useStream, apiFormat), nil
}

func (processor *TestChannelOrchestrator) handleNonStreamingResponse(
	rawResponse ChatCompletionResult,
	startTime time.Time,
	useStream bool,
	apiFormat llm.APIFormat,
) *TestChannelResult {
	result := newTestChannelResult(startTime, useStream)
	setNonStreamingFirstByte(result)

	if rawResponse.ChatCompletion == nil {
		result.Success = false
		result.Message = new("")
		result.Error = new("No response body")

		return result
	}

	var responseMessage *string
	if apiFormat == llm.APIFormatOpenAIDecisions {
		message, err := parseDecisionsTestResponse(rawResponse.ChatCompletion.Body)
		if err != nil {
			result.Success = false
			result.Message = new("")
			result.Error = new(err.Error())

			return result
		}
		responseMessage = &message
	} else {
		message, err := channelTestResponseMessage(rawResponse.ChatCompletion.Body, apiFormat)
		if err != nil {
			result.Success = false
			result.Message = new("")
			result.Error = new(err.Error())

			return result
		}
		responseMessage = message
	}

	result.Success = true
	result.Message = responseMessage

	return result
}

// handleStreamResponse processes a streaming response and accumulates the content.
func (processor *TestChannelOrchestrator) handleStreamResponse(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	startTime time.Time,
) (*TestChannelResult, error) {
	defer func() {
		_ = stream.Close()
	}()

	result := &TestChannelResult{Stream: true}
	finalizeTestChannelResult(result, startTime)

	// Accumulate stream chunks
	var accumulatedContent string

	for stream.Next() {
		select {
		case <-ctx.Done():
			finalizeTestChannelResult(result, startTime)
			result.Success = false
			result.Message = lo.ToPtr(accumulatedContent)
			result.Error = lo.ToPtr(ctx.Err().Error())

			return result, nil
		default:
		}

		event := stream.Current()
		if event == nil {
			continue
		}

		// Receiving an event is the closest observable approximation of the
		// upstream first-byte boundary available after transformation.
		markTestChannelFirstByte(result, startTime)

		// The stream may end with a "[DONE]" message which is not valid JSON.
		if string(event.Data) == "[DONE]" {
			continue
		}

		// Parse the stream event data
		var chunk llm.Response
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			log.Warn(ctx, "failed to unmarshal stream event data", log.Cause(err), log.ByteString("data", event.Data))
			continue
		}

		if hasMeaningfulStreamOutput(&chunk) {
			markTestChannelFirstToken(result, startTime)
		}

		// Accumulate content from the first choice
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil && chunk.Choices[0].Delta.Content.Content != nil {
			accumulatedContent += *chunk.Choices[0].Delta.Content.Content
		}
	}

	// Calculate latency after processing all stream events.
	finalizeTestChannelResult(result, startTime)

	if err := ctx.Err(); err != nil {
		result.Success = false
		result.Message = lo.ToPtr(accumulatedContent)
		result.Error = lo.ToPtr(err.Error())

		return result, nil
	}

	if stream.Err() != nil {
		result.Success = false
		result.Message = lo.ToPtr("")
		result.Error = lo.ToPtr(stream.Err().Error())

		return result, nil
	}

	if accumulatedContent == "" {
		result.Success = false
		result.Message = lo.ToPtr("")
		result.Error = lo.ToPtr("No content in stream response")

		return result, nil
	}

	result.Success = true
	result.Message = lo.ToPtr(accumulatedContent)
	result.Error = nil

	return result, nil
}

// TestAPIKeyResult represents the result of testing a single API key.
type TestAPIKeyResult struct {
	KeyPrefix string
	Success   bool
	Latency   float64
	TTFBMs    *float64
	TTFTMs    *float64
	TotalMs   float64
	Stream    bool
	Error     *string
	Disabled  bool
}

func testAPIKeyResultFromChannelResult(keyPrefix string, result *TestChannelResult) *TestAPIKeyResult {
	if result == nil {
		return &TestAPIKeyResult{KeyPrefix: keyPrefix}
	}

	return &TestAPIKeyResult{
		KeyPrefix: keyPrefix,
		Success:   result.Success,
		Latency:   result.Latency,
		TTFBMs:    result.TTFBMs,
		TTFTMs:    result.TTFTMs,
		TotalMs:   result.TotalMs,
		Stream:    result.Stream,
		Error:     result.Error,
	}
}

// TestChannelAPIKeysResult represents the aggregated result of testing all API keys.
type TestChannelAPIKeysResult struct {
	ChannelID    objects.GUID
	Total        int
	SuccessCount int
	FailedCount  int
	Results      []*TestAPIKeyResult
}

// TestChannelAPIKeys tests all API keys for a specific channel individually.
func (processor *TestChannelOrchestrator) TestChannelAPIKeys(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (*TestChannelAPIKeysResult, error) {
	ch, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	allKeys := ch.Credentials.GetAllAPIKeys()
	if len(allKeys) == 0 {
		return nil, fmt.Errorf("no API keys configured for channel")
	}

	// Build disabled set
	disabledSet := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		disabledSet[dk.Key] = struct{}{}
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = ch.DefaultTestModel
	}

	useStream := ch.Policies.Stream == objects.CapabilityPolicyRequire
	apiFormat := channelTestAPIFormat(ch, testModel)
	responsesWebSocket := usesResponsesWebSocket(ch)
	systemPrompt, userPrompt, err := processor.systemService.ChannelTestPrompts(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]*TestAPIKeyResult, len(allKeys))

	var (
		successCount int32
		failedCount  int32
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(testChannelAPIKeysMaxConcurrency, len(allKeys)))

	for i, key := range allKeys {
		index := i
		apiKey := key

		group.Go(func() error {
			select {
			case <-groupCtx.Done():
				errMsg := groupCtx.Err().Error()
				results[index] = &TestAPIKeyResult{
					KeyPrefix: maskAPIKey(apiKey),
					Success:   false,
					Error:     &errMsg,
				}

				atomic.AddInt32(&failedCount, 1)

				return nil
			default:
			}

			result := processor.testSingleKey(groupCtx, channelID, apiKey, testModel, useStream, responsesWebSocket, proxy, systemPrompt, userPrompt, apiFormat)
			_, isDisabled := disabledSet[apiKey]
			result.Disabled = isDisabled
			results[index] = result

			if result.Success {
				atomic.AddInt32(&successCount, 1)
				return nil
			}

			atomic.AddInt32(&failedCount, 1)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return &TestChannelAPIKeysResult{
		ChannelID:    channelID,
		Total:        len(allKeys),
		SuccessCount: int(successCount),
		FailedCount:  int(failedCount),
		Results:      results,
	}, nil
}

// TestSingleAPIKey tests a single API key for a channel.
// It verifies that the provided key belongs to the channel before testing.
func (processor *TestChannelOrchestrator) TestSingleAPIKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (*TestAPIKeyResult, error) {
	ch, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	// Verify the provided key is actually configured for this channel.
	channelKeys := ch.Credentials.GetAllAPIKeys()
	if len(channelKeys) == 0 {
		return nil, fmt.Errorf("no API keys configured for channel")
	}

	keyBelongsToChannel := lo.Contains(channelKeys, key)
	if !keyBelongsToChannel {
		return nil, fmt.Errorf("the provided API key is not configured for this channel")
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = ch.DefaultTestModel
	}

	useStream := ch.Policies.Stream == objects.CapabilityPolicyRequire
	apiFormat := channelTestAPIFormat(ch, testModel)
	responsesWebSocket := usesResponsesWebSocket(ch)
	systemPrompt, userPrompt, err := processor.systemService.ChannelTestPrompts(ctx)
	if err != nil {
		return nil, err
	}

	disabledSet := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		disabledSet[dk.Key] = struct{}{}
	}

	result := processor.testSingleKey(ctx, channelID, key, testModel, useStream, responsesWebSocket, proxy, systemPrompt, userPrompt, apiFormat)
	_, isDisabled := disabledSet[key]
	result.Disabled = isDisabled

	return result, nil
}

// testSingleKey tests a single API key by forcing the use of a specific key via SetAPIKey.
func (processor *TestChannelOrchestrator) testSingleKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	testModel string,
	useStream bool,
	responsesWebSocket bool,
	proxy *httpclient.ProxyConfig,
	systemPrompt string,
	userPrompt string,
	apiFormat llm.APIFormat,
) *TestAPIKeyResult {
	keyPrefix := maskAPIKey(key)

	inbound, body, err := processor.buildChannelTestInput(ctx, testModel, useStream, systemPrompt, userPrompt, responsesWebSocket, apiFormat)
	if err != nil {
		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Error:     lo.ToPtr(err.Error()),
		}
	}

	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: &SpecifiedChannelSelector{
			ChannelService: processor.channelService,
			ChannelID:      channelID,
			SelectedAPIKey: key,
		},
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: processor.promptProtectionRuleService,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
		Inbound:                    inbound,
		SystemService:              processor.systemService,
		UsageLogService:            processor.usageLogService,
		proxy:                      proxy,
		ModelMapper:                processor.modelMapper,
		adaptiveLoadBalancer:       processor.loadBalancer,
		failoverLoadBalancer:       processor.loadBalancer,
		circuitBreakerLoadBalancer: processor.loadBalancer,
		channelLimiterManager:      processor.channelLimiterManager,
		modelCircuitBreaker:        processor.modelCircuitBreaker,
	}

	startTime := time.Now()

	rawResponse, err := chatProcessor.Process(ctx, &httpclient.Request{
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: body,
	})
	if err != nil {
		rawErr := inbound.TransformError(ctx, err)
		message := gjson.GetBytes(rawErr.Body, "error.message").String()

		result := testChannelErrorResult(startTime, useStream, message)

		return testAPIKeyResultFromChannelResult(keyPrefix, result)
	}

	// Handle streaming response
	if rawResponse.ChatCompletionStream != nil {
		streamResult, _ := processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime)

		return testAPIKeyResultFromChannelResult(keyPrefix, streamResult)
	}
	return testAPIKeyResultFromChannelResult(
		keyPrefix,
		processor.handleNonStreamingResponse(rawResponse, startTime, useStream, apiFormat),
	)
}

// maskAPIKey returns a masked version of the API key for display.
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}

	return key[:4] + "****" + key[len(key)-4:]
}
