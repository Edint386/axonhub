package llm

import "context"

type requestContextKey struct{}

// WithRequest attaches the transformed llm.Request to ctx.
//
// Inbound transformers only receive the provider response when converting it
// back to the client format, but some conversions need to consult the original
// client request. Response-side TransformerMetadata cannot be used for this:
// it is produced by the channel-specific outbound transformer, so request data
// stashed by one API format never reaches a response produced by another.
func WithRequest(ctx context.Context, req *Request) context.Context {
	if ctx == nil || req == nil {
		return ctx
	}

	return context.WithValue(ctx, requestContextKey{}, req)
}

// RequestFromContext returns the llm.Request attached by WithRequest, or nil.
func RequestFromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}

	req, _ := ctx.Value(requestContextKey{}).(*Request)

	return req
}
