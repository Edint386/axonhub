package bailianasr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm/httpclient"
)

// uploadPolicy is the data object of GET /api/v1/uploads?action=getPolicy. The numeric fields
// are documented as strings but sent as numbers; gjson reads both.
type uploadPolicy struct {
	Policy          string
	Signature       string
	UploadDir       string
	UploadHost      string
	AccessKeyID     string
	ObjectACL       string
	ForbidOverwrite string
	MaxFileSizeMB   float64
}

var errPolicyExpired = errors.New("bailian upload policy expired")

// maxBufferedUploadBytes bounds the fallback that buffers an upload in memory when the channel
// executor is not the HTTP client (it always is in the gateway).
const maxBufferedUploadBytes = 32 << 20

// upload puts the audio into DashScope temporary storage (getPolicy, then an OSS form POST)
// and returns its oss:// URL. The stored file is bound to the model and the account of the key.
func (e *jobExecutor) upload(ctx context.Context, req *httpclient.Request, j *job) (string, error) {
	for attempt := 0; ; attempt++ {
		policy, err := e.getPolicy(ctx, req, j)
		if err != nil {
			return "", err
		}

		if limit := int64(policy.MaxFileSizeMB * 1024 * 1024); limit > 0 && j.audio.Size() > limit {
			return "", newResponseError(http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("audio is %d bytes; Bailian temporary storage accepts at most %.0f MB for %s",
					j.audio.Size(), policy.MaxFileSizeMB, j.opts.Model), "", nil)
		}

		key := policy.UploadDir + "/" + objectName(j.ext)

		err = e.postObject(ctx, policy, key, j.audio)
		if errors.Is(err, errPolicyExpired) && attempt == 0 {
			continue
		}

		if err != nil {
			return "", err
		}

		return "oss://" + key, nil
	}
}

// objectName is unique (x-oss-forbid-overwrite is on) and ASCII-only (DashScope requires
// percent-encoding otherwise).
func objectName(ext string) string {
	if ext == "" {
		return uuid.NewString()
	}

	return uuid.NewString() + "." + ext
}

func (e *jobExecutor) getPolicy(ctx context.Context, req *httpclient.Request, j *job) (uploadPolicy, error) {
	query := url.Values{"action": {"getPolicy"}, "model": {j.opts.Model}}

	timeout := e.cfg.startTimeout()
	callCtx, cancel := context.WithTimeoutCause(ctx, timeout, errStartCallTimeout)
	defer cancel()

	resp, err := e.inner.Do(callCtx, &httpclient.Request{
		Method:  http.MethodGet,
		URL:     j.native + uploadsPath + "?" + query.Encode(),
		Headers: dashScopeHeaders(req, "Content-Type", "application/json"),
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(context.Cause(callCtx), errStartCallTimeout) {
			return uploadPolicy{}, startTimeoutError("upload policy request", timeout)
		}

		return uploadPolicy{}, err
	}

	data := gjson.GetBytes(resp.Body, "data")
	policy := uploadPolicy{
		Policy:          data.Get("policy").String(),
		Signature:       data.Get("signature").String(),
		UploadDir:       strings.TrimRight(data.Get("upload_dir").String(), "/"),
		UploadHost:      data.Get("upload_host").String(),
		AccessKeyID:     data.Get("oss_access_key_id").String(),
		ObjectACL:       data.Get("x_oss_object_acl").String(),
		ForbidOverwrite: data.Get("x_oss_forbid_overwrite").String(),
		MaxFileSizeMB:   data.Get("max_file_size_mb").Float(),
	}

	if policy.UploadHost == "" || policy.Policy == "" {
		return uploadPolicy{}, newResponseError(http.StatusBadGateway, "bailian_upload_policy",
			"Bailian getPolicy returned no upload policy", gjson.GetBytes(resp.Body, "request_id").String(), nil)
	}

	return policy, nil
}

// postObject sends the OSS PostObject form. The file field must come last; the body is
// streamed from the audio source with an exact Content-Length.
func (e *jobExecutor) postObject(ctx context.Context, policy uploadPolicy, key string, src AudioSource) error {
	head, tail, contentType, err := ossFormEnvelope(policy, key)
	if err != nil {
		return err
	}

	audio, err := src.Open()
	if err != nil {
		return fmt.Errorf("failed to read audio: %w", err)
	}
	defer audio.Close()

	status, respBody, err := e.sendForm(ctx, policy.UploadHost, contentType,
		io.MultiReader(bytes.NewReader(head), audio, strings.NewReader(tail)),
		int64(len(head))+src.Size()+int64(len(tail)))
	if err != nil {
		return err
	}

	if status/100 == 2 {
		return nil
	}

	if status == http.StatusForbidden && bytes.Contains(respBody, []byte("Policy expired")) {
		return newResponseError(http.StatusBadGateway, "bailian_upload_policy_expired",
			"Bailian temporary storage rejected an expired upload policy", "", errPolicyExpired)
	}

	return newResponseError(http.StatusBadGateway, "bailian_upload_failed",
		fmt.Sprintf("Bailian temporary storage upload failed with HTTP %d %s", status, ossErrorCode(respBody)), "", nil)
}

func ossFormEnvelope(policy uploadPolicy, key string) ([]byte, string, string, error) {
	var head bytes.Buffer

	writer := multipart.NewWriter(&head)
	fields := [][2]string{
		{"OSSAccessKeyId", policy.AccessKeyID},
		{"Signature", policy.Signature},
		{"policy", policy.Policy},
		{"x-oss-object-acl", policy.ObjectACL},
		{"x-oss-forbid-overwrite", policy.ForbidOverwrite},
		{"key", key},
		{"success_action_status", "200"},
	}

	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return nil, "", "", fmt.Errorf("failed to build upload form: %w", err)
		}
	}

	part := textproto.MIMEHeader{}
	part.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, key[strings.LastIndex(key, "/")+1:]))
	part.Set("Content-Type", "application/octet-stream")

	if _, err := writer.CreatePart(part); err != nil {
		return nil, "", "", fmt.Errorf("failed to build upload form: %w", err)
	}

	// What multipart.Writer.Close would write after the file content.
	tail := "\r\n--" + writer.Boundary() + "--\r\n"

	return head.Bytes(), tail, writer.FormDataContentType(), nil
}

// errUploadStalled is the cause of an upload cancelled for making no progress.
var errUploadStalled = errors.New("bailian temporary storage upload stalled")

// sendForm posts to the OSS upload host without any DashScope credentials. It streams through
// the channel's native HTTP client (proxy settings included) when the inner executor is an
// HttpClient, and otherwise falls back to a buffered request through the executor. A streamed
// upload fails once it has sent nothing (or, after the last byte, received no answer) for the
// start timeout: a large file may take many minutes, but never that long without progress.
func (e *jobExecutor) sendForm(ctx context.Context, target, contentType string, body io.Reader, length int64) (int, []byte, error) {
	if client, ok := e.inner.(*httpclient.HttpClient); ok {
		stall := e.cfg.startTimeout()

		uploadCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)

		watchdog := time.AfterFunc(stall, func() { cancel(errUploadStalled) })
		defer watchdog.Stop()

		httpReq, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, target, &progressReader{r: body, progress: watchdog, stall: stall})
		if err != nil {
			return 0, nil, fmt.Errorf("failed to build upload request: %w", err)
		}

		httpReq.ContentLength = length
		httpReq.Header.Set("Content-Type", contentType)

		resp, err := client.GetNativeClient().Do(httpReq)
		if err != nil {
			if ctx.Err() == nil && errors.Is(context.Cause(uploadCtx), errUploadStalled) {
				return 0, nil, startTimeoutError("temporary storage upload (no progress)", stall)
			}

			return 0, nil, fmt.Errorf("bailian temporary storage upload failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

		return resp.StatusCode, respBody, nil
	}

	// Buffering is only acceptable for small files: a large upload must stream from disk.
	if length > maxBufferedUploadBytes {
		return 0, nil, newResponseError(http.StatusInternalServerError, "bailian_upload_unstreamable",
			fmt.Sprintf("cannot stream a %d-byte upload through this channel's executor", length), "", nil)
	}

	buffered, err := io.ReadAll(body)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read audio: %w", err)
	}

	resp, err := e.inner.Do(ctx, &httpclient.Request{
		Method:      http.MethodPost,
		URL:         target,
		Headers:     http.Header{"Content-Type": []string{contentType}},
		ContentType: contentType,
		Body:        buffered,
	})
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return httpErr.StatusCode, httpErr.Body, nil
	}

	if err != nil {
		return 0, nil, fmt.Errorf("bailian temporary storage upload failed: %w", err)
	}

	return resp.StatusCode, resp.Body, nil
}

// progressReader restarts the upload watchdog whenever the transport takes more of the body.
type progressReader struct {
	r        io.Reader
	progress *time.Timer
	stall    time.Duration
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress.Reset(p.stall)
	}

	return n, err
}

func ossErrorCode(body []byte) string {
	start := bytes.Index(body, []byte("<Code>"))
	end := bytes.Index(body, []byte("</Code>"))

	if start < 0 || end <= start {
		return ""
	}

	return string(body[start+len("<Code>") : end])
}

// requestOnlyHeaders describe the provider request's own body or mode; the extra DashScope
// calls of a job (getPolicy, task polls, the -message handshake) must not inherit them.
var requestOnlyHeaders = []string{
	"Content-Type", "Content-Length", "Content-Encoding", "Accept", "Transfer-Encoding",
	"X-DashScope-Async", "X-DashScope-SSE", "X-DashScope-OssResourceResolve",
}

// handshakeOnlyHeaders are set by the WebSocket dialer itself; gorilla/websocket refuses a
// handshake whose caller supplies them.
var handshakeOnlyHeaders = []string{
	"Host", "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions",
	"Sec-Websocket-Protocol",
}

// dashScopeHeaders carries the request's headers to the extra DashScope API calls of a job:
// the credentials, the workspace header, channel header overrides (a relay in front of
// DashScope may need them on every call) and the given header pairs, without the headers that
// describe the original request body.
func dashScopeHeaders(req *httpclient.Request, pairs ...string) http.Header {
	headers := req.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}

	for _, name := range requestOnlyHeaders {
		headers.Del(name)
	}

	if headers.Get("Authorization") == "" && req.Auth != nil && req.Auth.Type == httpclient.AuthTypeBearer {
		headers.Set("Authorization", "Bearer "+req.Auth.APIKey)
	}

	for i := 0; i+1 < len(pairs); i += 2 {
		headers.Set(pairs[i], pairs[i+1])
	}

	return headers
}

// handshakeHeaders is dashScopeHeaders for the -message WebSocket handshake.
func handshakeHeaders(req *httpclient.Request) http.Header {
	headers := dashScopeHeaders(req)
	for _, name := range handshakeOnlyHeaders {
		headers.Del(name)
	}

	return headers
}
