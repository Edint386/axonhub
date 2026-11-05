package bailianasr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// Outbound puts Bailian file transcription in front of a channel outbound. Transcription
// requests for the allowlisted models become native DashScope requests; every other request,
// response, stream and error goes to the wrapped outbound unchanged.
type Outbound struct {
	inner  transformer.Outbound
	cfg    Config
	native string
	cache  *jobCache
}

var (
	_ transformer.Outbound                  = (*Outbound)(nil)
	_ pipeline.ChannelCustomizedExecutor    = (*Outbound)(nil)
	_ transformer.PassThroughBodyPolicy     = (*Outbound)(nil)
	_ transformer.PassThroughResponsePolicy = (*Outbound)(nil)
)

// Wrap returns inner with Bailian file transcription in front of it.
func Wrap(inner transformer.Outbound, cfg Config) *Outbound {
	return &Outbound{inner: inner, cfg: cfg, native: NativeBaseURL(cfg.BaseURL), cache: sharedJobCache}
}

// Inner returns the wrapped outbound.
func (o *Outbound) Inner() transformer.Outbound {
	return o.inner
}

// NativeBaseURL returns the DashScope API origin this outbound calls.
func (o *Outbound) NativeBaseURL() string {
	return o.native
}

func (o *Outbound) APIFormat() llm.APIFormat {
	return o.inner.APIFormat()
}

func (o *Outbound) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	if req == nil || req.RequestType != llm.RequestTypeTranscription || req.Transcription == nil {
		return o.inner.TransformRequest(ctx, req)
	}

	fam, ok := familyOf(req.Model)
	if !ok {
		return o.inner.TransformRequest(ctx, req)
	}

	return o.buildRequest(ctx, req, fam)
}

func (o *Outbound) TransformResponse(ctx context.Context, resp *httpclient.Response) (*llm.Response, error) {
	kind, opts, ok := requestKind(responseRequest(resp))
	if !ok {
		return o.inner.TransformResponse(ctx, resp)
	}

	t, err := decodeResponse(kind, resp.Body)
	if err != nil {
		return nil, err
	}

	if opts.AudioSeconds > 0 {
		t.Duration = opts.AudioSeconds
	}

	llmResp, err := toLLMResponse(t, opts)
	if err != nil {
		return nil, err
	}

	if !billsUsage(responseRequest(resp)) {
		llmResp.Usage = &llm.Usage{}
	}

	return llmResp, nil
}

// billsUsage reports whether the response to req records its usage: false only for an async job
// whose result another request already billed (see job.billsUsage). The transcript the client
// gets keeps its usage either way; only the gateway's usage record is zero.
func billsUsage(req *httpclient.Request) bool {
	j, ok := jobOf(req, kindAsync)
	return !ok || j.billsUsage()
}

func (o *Outbound) TransformStream(
	ctx context.Context,
	req *httpclient.Request,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*llm.Response], error) {
	kind, opts, ok := requestKind(req)
	if !ok {
		return o.inner.TransformStream(ctx, req, stream)
	}

	s := newTranscriptStream(stream, newStreamFolder(kind), opts)
	s.billsUsage = func() bool { return billsUsage(req) }

	return s, nil
}

func (o *Outbound) TransformError(ctx context.Context, err *httpclient.Error) *llm.ResponseError {
	if respErr, ok := nativeHTTPError(err); ok {
		return respErr
	}

	return o.inner.TransformError(ctx, err)
}

func (o *Outbound) AggregateStreamChunks(
	ctx context.Context,
	req *httpclient.Request,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	kind, _, ok := requestKind(req)
	if !ok {
		return o.inner.AggregateStreamChunks(ctx, req, chunks)
	}

	body, meta, err := aggregateChunks(newStreamFolder(kind), chunks)
	if err == nil && meta.Usage != nil && !billsUsage(req) {
		meta.Usage = &llm.Usage{}
	}

	return body, meta, err
}

// CustomizeExecutor keeps whatever executor the wrapped outbound customizes and puts the
// async-job runner in front of it.
func (o *Outbound) CustomizeExecutor(executor pipeline.Executor) pipeline.Executor {
	if custom, ok := o.inner.(pipeline.ChannelCustomizedExecutor); ok {
		executor = custom.CustomizeExecutor(executor)
	}

	return &jobExecutor{inner: executor, cfg: o.cfg, cache: o.cache}
}

// AllowPassThroughBody vetoes raw pass-through for DashScope requests: the client's multipart
// body is never what DashScope expects.
func (o *Outbound) AllowPassThroughBody(ctx context.Context, llmReq *llm.Request, providerReq *httpclient.Request) bool {
	if _, _, ok := requestKind(providerReq); ok {
		return false
	}

	if policy, ok := o.inner.(transformer.PassThroughBodyPolicy); ok {
		return policy.AllowPassThroughBody(ctx, llmReq, providerReq)
	}

	return true
}

func (o *Outbound) AllowPassThroughResponse(ctx context.Context, llmReq *llm.Request, providerReq *httpclient.Request) bool {
	if _, _, ok := requestKind(providerReq); ok {
		return false
	}

	if policy, ok := o.inner.(transformer.PassThroughResponsePolicy); ok {
		return policy.AllowPassThroughResponse(ctx, llmReq, providerReq)
	}

	return true
}

func responseRequest(resp *httpclient.Response) *httpclient.Request {
	if resp == nil {
		return nil
	}

	return resp.Request
}

// IsTranscriptionRequest reports whether req is a provider request this package built (a flash
// call, an async job or a -message task). Such an attempt runs for about as long as the audio
// (or much longer), so its duration says nothing about the channel's latency.
func IsTranscriptionRequest(req *httpclient.Request) bool {
	_, _, ok := requestKind(req)
	return ok
}

// ReusedJob reports whether req (a provider request this package built) was answered without
// uploading and submitting its audio itself: from a cached result, a task the job cache held, or
// another request's submit it joined. Such a success says nothing about the channel or key the
// attempt was given, so channel performance records count it as neither a success nor a failure.
func ReusedJob(req *httpclient.Request) bool {
	j, ok := jobOf(req, kindAsync)
	return ok && j.reused.Load()
}

// requestKind reports whether req was built by this package and with which render options.
func requestKind(req *httpclient.Request) (string, renderOptions, bool) {
	if req == nil || req.Metadata == nil {
		return "", renderOptions{}, false
	}

	kind := req.Metadata[metaKind]
	if kind != kindFlash && kind != kindAsync && kind != kindMessage {
		return "", renderOptions{}, false
	}

	var opts renderOptions
	if raw := req.Metadata[metaOptions]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			return "", renderOptions{}, false
		}
	}

	return kind, opts, true
}

// decodeResponse reads the executor's job envelope (async, message) or a generation body (flash).
func decodeResponse(kind string, body []byte) (*transcript, error) {
	if kind == kindAsync || kind == kindMessage {
		var t transcript
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, fmt.Errorf("failed to decode bailian transcription result: %w", err)
		}

		return &t, nil
	}

	t, err := parseFlashBody(body)
	if err != nil {
		// A 2xx body can still carry a DashScope error object.
		if code, message, requestID, ok := dashScopeError(body); ok {
			return nil, newResponseError(statusForCode(code, http.StatusBadGateway), code, message, requestID, err)
		}

		return nil, err
	}

	return t, nil
}
