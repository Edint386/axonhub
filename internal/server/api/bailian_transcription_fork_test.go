package api

// End-to-end tests for the custom Bailian file transcription (internal/server/bailianasr):
// POST /v1/audio/transcriptions through CreateTranscription, the real orchestrator, a channel
// built by ChannelService (so the channel_llm.go fork hook is exercised) and a fake DashScope.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
	entrequestexecution "github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const bailianASRTaskID = "0f7c2a7e-api-test-task"

// bailianASRFake answers the native DashScope calls with frames shaped like the 2026-11-05 live probe.
type bailianASRFake struct {
	server  *httptest.Server
	mu      sync.Mutex
	submits int
	uploads int
}

func newBailianASRFake(t *testing.T) *bailianASRFake {
	t.Helper()

	f := &bailianASRFake{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)

	return f
}

func (f *bailianASRFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	const text = "今天天气不错，我们去公园散步。明天下午三点开会，记得带电脑。"

	switch {
	case r.URL.Path == "/api/v1/uploads":
		_, _ = fmt.Fprintf(w, `{"request_id":"p","data":{"policy":"cG9saWN5","signature":"sig","upload_dir":"dashscope-instant/t",`+
			`"upload_host":"%s/oss","expire_in_seconds":300,"max_file_size_mb":1024,"oss_access_key_id":"LTAI",`+
			`"x_oss_object_acl":"private","x_oss_forbid_overwrite":"true"}}`, f.server.URL)
	case r.URL.Path == "/oss":
		_, _ = io.Copy(io.Discard, r.Body)
		f.uploads++
	case r.URL.Path == "/api/v1/services/audio/asr/transcription":
		f.submits++
		_, _ = fmt.Fprintf(w, `{"output":{"task_status":"PENDING","task_id":"%s"},"request_id":"s"}`, bailianASRTaskID)
	case r.URL.Path == "/api/v1/tasks/"+bailianASRTaskID:
		_, _ = fmt.Fprintf(w, `{"request_id":"q","output":{"task_id":"%s","task_status":"SUCCEEDED","results":[{"file_url":"oss://x",`+
			`"transcription_url":"%s/result.json","subtask_status":"SUCCEEDED"}]},"usage":{"duration":8}}`, bailianASRTaskID, f.server.URL)
	case r.URL.Path == "/result.json":
		_, _ = io.WriteString(w, `{"properties":{"original_duration_in_milliseconds":8000},"transcripts":[{"channel_id":0,"text":"`+text+
			`","sentences":[{"begin_time":160,"end_time":3200,"text":"今天天气不错，我们去公园散步。","sentence_id":1},`+
			`{"begin_time":4400,"end_time":8016,"text":"明天下午三点开会，记得带电脑。","sentence_id":2}]}]}`)
	case r.URL.Path == "/api/v1/services/aigc/multimodal-generation/generation":
		w.Header().Set("Content-Type", "text/event-stream;charset=UTF-8")
		_, _ = io.WriteString(w, "id:1\nevent:result\n:HTTP_STATUS/200\ndata:"+
			`{"output":{"sentence":{"sentence_id":1,"text":"今天天气不错，我们去公园散步。","sentence_end":true}},"usage":{"duration":4},"request_id":"f"}`+"\n\n"+
			"id:2\nevent:result\n:HTTP_STATUS/200\ndata:"+
			`{"output":{"sentence":{"sentence_id":2,"text":"`+text+`","sentence_end":true}},`+
			`"usage":{"duration":8,"input_tokens":101,"output_tokens":17,"total_tokens":118},"request_id":"f"}`+"\n\n")
	default:
		http.Error(w, `{"code":"NotFound","message":"no route"}`, http.StatusNotFound)
	}
}

type bailianASRHarness struct {
	ctx      context.Context
	client   *ent.Client
	handlers *OpenAIHandlers
	fake     *bailianASRFake
}

func newBailianASRHarness(t *testing.T) *bailianASRHarness {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	fake := newBailianASRFake(t)

	models := []string{"paraformer-v2", "qwen-audio-3.1-asr-flash"}
	row, err := client.Channel.Create().
		SetType(channel.TypeBailian).
		SetName("Bailian ASR").
		SetBaseURL(fake.server.URL + "/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-api-test"}).
		SetSupportedModels(models).
		SetDefaultTestModel(models[0]).
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.ChannelModelPrice.Create().
		SetChannelID(row.ID).
		SetModelID("paraformer-v2").
		SetReferenceID("paraformer-v2-per-second").
		SetPrice(objects.ModelPrice{Items: []objects.ModelPriceItem{{
			ItemCode: objects.PriceItemCodeDurationSeconds,
			Pricing: objects.Pricing{
				Mode:         objects.PricingModeUsagePerUnit,
				UsagePerUnit: lo.ToPtr(decimal.RequireFromString("0.00008")),
			},
		}}}).
		Save(ctx)
	require.NoError(t, err)

	channelService, requestService, systemService, usageLogService := setupSpeechAPITestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreChunks: true, StoreRequestBody: true, StoreResponseBody: true}))

	// GetChannel builds the channel like the enabled-channel cache does, fork hook included.
	bizChannel, err := channelService.GetChannel(ctx, row.ID)
	require.NoError(t, err)
	channelService.PreloadModelPricesForTest(ctx, bizChannel)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{bizChannel})

	candidates := make([]*orchestrator.ChannelModelsCandidate, 0, len(models))
	for _, model := range models {
		candidates = append(candidates, &orchestrator.ChannelModelsCandidate{
			Channel: bizChannel,
			Models:  []biz.ChannelModelEntry{{RequestModel: model, ActualModel: model, Source: "direct"}},
		})
	}

	inbound := openai.NewTranscriptionInboundTransformer()
	promptProtection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	t.Cleanup(promptProtection.Stop)

	orch := orchestrator.NewChatCompletionOrchestrator(
		channelService, nil, requestService, httpclient.NewHttpClient(), inbound, systemService, usageLogService,
		nil, nil, promptProtection, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil,
	).WithChannelSelector(&bailianASRSelector{candidates: candidates})
	orch.PipelineFactory = pipeline.NewFactory(httpclient.NewHttpClient())

	return &bailianASRHarness{
		ctx:      ctx,
		client:   client,
		handlers: &OpenAIHandlers{TranscriptionHandlers: NewChatCompletionHandlers(orch)},
		fake:     fake,
	}
}

// bailianASRSelector returns the candidate whose model matches the request.
type bailianASRSelector struct {
	candidates []*orchestrator.ChannelModelsCandidate
}

func (s *bailianASRSelector) Select(_ context.Context, req *llm.Request) ([]*orchestrator.ChannelModelsCandidate, error) {
	for _, candidate := range s.candidates {
		if candidate.Models[0].RequestModel == req.Model {
			return []*orchestrator.ChannelModelsCandidate{candidate}, nil
		}
	}

	return nil, nil
}

func (h *bailianASRHarness) transcribe(t *testing.T, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		require.NoError(t, writer.WriteField(name, value))
	}

	file, err := writer.CreateFormFile("file", "clip.wav")
	require.NoError(t, err)
	_, err = file.Write(apiTestWAV(1600))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body).WithContext(h.ctx)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())

	h.handlers.CreateTranscription(c)

	return w
}

func apiTestWAV(samples int) []byte {
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+samples*2))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(samples*2))

	return data
}

func TestCreateTranscription_BailianParaformerJobIsBilledPerSecond(t *testing.T) {
	h := newBailianASRHarness(t)

	w := h.transcribe(t, map[string]string{"model": "paraformer-v2", "response_format": "srt", "language": "zh"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
	require.Equal(t, "1\n00:00:00,160 --> 00:00:03,200\n今天天气不错，我们去公园散步。\n\n"+
		"2\n00:00:04,400 --> 00:00:08,016\n明天下午三点开会，记得带电脑。\n\n", w.Body.String())
	require.Equal(t, 1, h.fake.submits)
	require.Equal(t, 1, h.fake.uploads)

	requests, err := h.client.Request.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, entrequest.StatusCompleted, requests[0].Status)

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, executions, 1)
	require.Equal(t, entrequestexecution.StatusCompleted, executions[0].Status)
	require.Equal(t, string(llm.APIFormatOpenAITranscription), executions[0].Format)
	require.Contains(t, string(executions[0].RequestBody), "axonhub-pending-upload", "the stored submit body carries no audio")
	require.NotContains(t, string(executions[0].RequestBody), "RIFF")

	usageLogs, err := h.client.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, usageLogs, 1)
	require.Equal(t, "paraformer-v2", usageLogs[0].ModelID)
	require.NotNil(t, usageLogs[0].TotalCost)
	require.InDelta(t, 8*0.00008, *usageLogs[0].TotalCost, 1e-9, "8 billed seconds at the duration_seconds price")
	require.Len(t, usageLogs[0].CostItems, 1)
	require.Equal(t, objects.PriceItemCodeDurationSeconds, usageLogs[0].CostItems[0].ItemCode)
	require.EqualValues(t, 8, usageLogs[0].CostItems[0].Quantity)
}

func TestCreateTranscription_BailianFlashStreamRecordsTokens(t *testing.T) {
	h := newBailianASRHarness(t)

	w := h.transcribe(t, map[string]string{"model": "qwen-audio-3.1-asr-flash", "stream": "true"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var deltas []string

	done := gjson.Result{}

	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}

		event := gjson.Parse(data)
		switch event.Get("type").String() {
		case "transcript.text.delta":
			deltas = append(deltas, event.Get("delta").String())
		case "transcript.text.done":
			done = event
		}
	}

	require.Equal(t, []string{"今天天气不错，我们去公园散步。", "明天下午三点开会，记得带电脑。"}, deltas)
	require.Equal(t, "今天天气不错，我们去公园散步。明天下午三点开会，记得带电脑。", done.Get("text").String())
	require.EqualValues(t, 101, done.Get("usage.prompt_tokens").Int())

	usageLogs, err := h.client.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, usageLogs, 1)
	require.EqualValues(t, 101, usageLogs[0].PromptTokens)
	require.EqualValues(t, 17, usageLogs[0].CompletionTokens)
	require.EqualValues(t, 118, usageLogs[0].TotalTokens)
	require.Equal(t, string(llm.APIFormatOpenAITranscription), usageLogs[0].Format)
}
