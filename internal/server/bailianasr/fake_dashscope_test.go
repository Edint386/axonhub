package bailianasr

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The frames below follow live-probe-findings.md (2026-11-05): an 8 s two-sentence Chinese clip.
const (
	clipText      = "今天天气不错，我们去公园散步。明天下午三点开会，记得带电脑。"
	paraformerTxt = "今天天气不错，我们去公园散步，明天下午三点开会，记得带电脑。"
	fakeAPIKey    = "sk-test-bailian"
	fakeTaskID    = "8d4c6c1b-6f2e-4c55-9a43-1b2f3c4d5e6f"
)

// flashWords is the word list of the single aggregated sentence the flash model returns.
const flashWords = `[
 {"begin_time":160,"end_time":1200,"text":"今天天气不错","punctuation":"，","fixed":true,"speaker_id":null},
 {"begin_time":1200,"end_time":3200,"text":"我们去公园散步","punctuation":"。","fixed":true,"speaker_id":null},
 {"begin_time":4400,"end_time":6400,"text":"明天下午三点开会","punctuation":"，","fixed":true,"speaker_id":null},
 {"begin_time":6400,"end_time":8016,"text":"记得带电脑","punctuation":"。","fixed":true,"speaker_id":null}]`

// flashBody is a non-SSE generation response: ONE aggregated sentence plus a nested duplicate.
var flashBody = `{"output":{"sentence":{"sentence_id":2,"begin_time":160,"end_time":8016,"text":"` + clipText +
	`","channel_id":0,"speaker_id":null,"sentence_end":true,"words":` + flashWords + `},"text":"` + clipText +
	`","output":{"sentence":{"sentence_id":2,"text":"` + clipText + `"},"text":"` + clipText + `"}},` +
	`"usage":{"duration":8,"input_tokens":101,"output_tokens":17,"total_tokens":118},"request_id":"req-flash-1"}`

// flashSSE: one event per finished sentence with CUMULATIVE sentence.text, an intermediate
// non-final event, and the full usage only on the last event.
var flashSSE = strings.Join([]string{
	sseEvent(1, `{"output":{"sentence":{"sentence_id":1,"begin_time":160,"end_time":3200,"text":"今天天气不错，我们去公园散步。","sentence_end":true},"text":"今天天气不错，我们去公园散步。"},"usage":{"duration":4},"request_id":"req-flash-sse"}`),
	sseEvent(2, `{"output":{"sentence":{"sentence_id":2,"begin_time":4400,"end_time":null,"text":"今天天气不错，我们去公园散步。明天下午","sentence_end":false},"text":"今天天气不错，我们去公园散步。明天下午"},"request_id":"req-flash-sse"}`),
	sseEvent(3, `{"output":{"sentence":{"sentence_id":2,"begin_time":4400,"end_time":8016,"text":"`+clipText+`","sentence_end":true},"text":"`+clipText+`"},"usage":{"duration":8,"input_tokens":101,"output_tokens":17,"total_tokens":118},"request_id":"req-flash-sse"}`),
}, "")

func sseEvent(id int, data string) string {
	return fmt.Sprintf("id:%d\nevent:result\n:HTTP_STATUS/200\ndata:%s\n\n", id, data)
}

// paraformerResult is a transcription_url body: one comma-joined sentence, per-character words.
func paraformerResult(speaker string) string {
	var words []string

	begin := 160
	for _, r := range "今天天气不错" {
		words = append(words, fmt.Sprintf(`{"begin_time":%d,"end_time":%d,"text":"%c","punctuation":""}`, begin, begin+200, r))
		begin += 200
	}

	return `{"file_url":"oss://dashscope-instant/x/a.wav","properties":{"audio_format":"pcm_s16le","channels":[0],` +
		`"original_sampling_rate":16000,"original_duration_in_milliseconds":8000},"transcripts":[{"channel_id":0,` +
		`"content_duration_in_milliseconds":7200,"text":"` + paraformerTxt + `","sentences":[{"begin_time":160,"end_time":7900,` +
		`"text":"` + paraformerTxt + `","sentence_id":1` + speaker + `,"words":[` + strings.Join(words, ",") + `]}]}]}`
}

// qwenFiletransResult has two proper sentences with confidence, one of them for a second speaker.
const qwenFiletransResult = `{"file_url":"oss://dashscope-instant/x/b.wav","properties":{"audio_format":"pcm_s16le",` +
	`"channels":[0],"original_sampling_rate":16000,"original_duration_in_milliseconds":8000},"transcripts":[{"channel_id":0,` +
	`"content_duration_in_milliseconds":6000,"text":"` + clipText + `","sentences":[` +
	`{"begin_time":160,"end_time":3200,"text":"今天天气不错，我们去公园散步。","sentence_id":1,"speaker_id":0,` +
	`"words":[{"begin_time":160,"end_time":1200,"text":"今天天气不错","punctuation":"，","confidence":0.98},` +
	`{"begin_time":1200,"end_time":3200,"text":"我们去公园散步","punctuation":"。","confidence":0.97}]},` +
	`{"begin_time":4400,"end_time":8016,"text":"明天下午三点开会，记得带电脑。","sentence_id":2,"speaker_id":1,` +
	`"words":[{"begin_time":4400,"end_time":6400,"text":"明天下午三点开会","punctuation":"，","confidence":0.99},` +
	`{"begin_time":6400,"end_time":8016,"text":"记得带电脑","punctuation":"。","confidence":0.96}]}]}]}`

// wavClip is a 16 kHz mono PCM16 WAV header followed by silence.
func wavClip(samples int) []byte {
	data := make([]byte, 44+samples*2)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+samples*2))
	copy(data[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 16000)
	binary.LittleEndian.PutUint32(data[28:32], 32000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(samples*2))

	return data
}

type uploadRecord struct {
	fields        []string
	values        map[string]string
	file          []byte
	contentLength int64
	authorization string
}

// fakeDashScope serves getPolicy, the OSS form upload, task submit/poll, the result file and the
// flash generation endpoint on one httptest server.
type fakeDashScope struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// statuses is the task_status sequence the poll returns; the last one repeats.
	statuses []string
	// pollErrors makes the first polls fail with HTTP 500.
	pollErrors int
	// pollReject makes every poll fail with this status and a DashScope error body.
	pollReject int
	// expiredPolicies makes the first OSS uploads fail with "Policy expired".
	expiredPolicies int
	// resultErrors makes the first result downloads fail with HTTP 500.
	resultErrors int
	// taskFailure is returned as output.code/message on FAILED.
	taskFailure [2]string
	// subtask overrides results[0] for SUCCEEDED (e.g. a failed subtask).
	subtask string
	result  string
	usage   string

	// otherKeys are accepted by getPolicy besides fakeAPIKey; revokedKeys are refused (401) by
	// getPolicy and the task query.
	otherKeys   []string
	revokedKeys []string
	// submitFailure makes every submit fail with this status and body once it was received
	// (the task is counted as submitted).
	submitFailure struct {
		status int
		body   string
	}
	// hold makes the requests to these paths wait until the channel is closed or the client
	// gives up; holdStarted (if set) gets one value per held request.
	hold        map[string]chan struct{}
	holdStarted chan string
	// stop releases every held request when the test ends, so the server can close.
	stop chan struct{}

	calls fakeCalls
}

// fakeCalls records what the fake DashScope received.
type fakeCalls struct {
	policyCalls   int
	submitCalls   int
	pollCalls     int
	resultCalls   int
	flashCalls    int
	uploads       []uploadRecord
	submitBodies  []string
	submitHeaders []http.Header
	resultAuth    []string
	pollAuth      []string
	flashBodies   []string
	flashHeaders  []http.Header
	// policyRelay and pollRelay record the X-Relay-Token header (a channel header override).
	policyRelay []string
	pollRelay   []string
}

func newFakeDashScope(t *testing.T) *fakeDashScope {
	t.Helper()

	f := &fakeDashScope{
		t:        t,
		statuses: []string{"PENDING", "RUNNING", "SUCCEEDED"},
		result:   paraformerResult(""),
		usage:    `{"duration":8}`,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	f.stop = make(chan struct{})
	t.Cleanup(f.server.Close)
	t.Cleanup(func() { close(f.stop) }) // runs first

	return f
}

func (f *fakeDashScope) baseURL() string {
	return f.server.URL + "/compatible-mode/v1"
}

func (f *fakeDashScope) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	gate, started := f.hold[r.URL.Path], f.holdStarted
	f.mu.Unlock()

	if gate != nil {
		// Read the body first: the server only notices a client that gives up once it did.
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		if started != nil {
			started <- r.URL.Path
		}

		select {
		case <-gate:
		case <-r.Context().Done():
			return
		case <-f.stop:
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/uploads":
		f.servePolicy(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/oss":
		f.serveUpload(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/services/audio/asr/transcription":
		f.serveSubmit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fakeTaskID:
		f.servePoll(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/result/"+fakeTaskID+".json":
		f.calls.resultCalls++
		f.calls.resultAuth = append(f.calls.resultAuth, r.Header.Get("Authorization"))

		if f.resultErrors > 0 {
			f.resultErrors--
			http.Error(w, "oss unavailable", http.StatusInternalServerError)

			return
		}

		_, _ = io.WriteString(w, f.result)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/services/aigc/multimodal-generation/generation":
		f.serveFlash(w, r)
	default:
		http.Error(w, `{"code":"NotFound","message":"no route"}`, http.StatusNotFound)
	}
}

func (f *fakeDashScope) servePolicy(w http.ResponseWriter, r *http.Request) {
	f.calls.policyCalls++
	f.calls.policyRelay = append(f.calls.policyRelay, r.Header.Get("X-Relay-Token"))
	if !f.keyAccepted(r) || r.URL.Query().Get("action") != "getPolicy" {
		http.Error(w, `{"code":"InvalidApiKey","message":"bad key"}`, http.StatusUnauthorized)
		return
	}

	_, _ = fmt.Fprintf(w, `{"request_id":"req-policy","data":{"policy":"eyJwb2xpY3kiOjF9","signature":"sig==",`+
		`"upload_dir":"dashscope-instant/abc/2026-11-05/%s","upload_host":"%s/oss","expire_in_seconds":300,`+
		`"max_file_size_mb":1024,"capacity_limit_mb":999999999,"oss_access_key_id":"LTAI-test",`+
		`"x_oss_object_acl":"private","x_oss_forbid_overwrite":"true"}}`, r.URL.Query().Get("model"), f.server.URL)
}

func (f *fakeDashScope) keyAccepted(r *http.Request) bool {
	authorization := r.Header.Get("Authorization")
	for _, key := range f.revokedKeys {
		if authorization == "Bearer "+key {
			return false
		}
	}

	if authorization == "Bearer "+fakeAPIKey {
		return true
	}

	for _, key := range f.otherKeys {
		if authorization == "Bearer "+key {
			return true
		}
	}

	return false
}

func (f *fakeDashScope) serveUpload(w http.ResponseWriter, r *http.Request) {
	record := uploadRecord{values: map[string]string{}, contentLength: r.ContentLength, authorization: r.Header.Get("Authorization")}

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "bad content type", http.StatusBadRequest)
		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		data, _ := io.ReadAll(part)
		record.fields = append(record.fields, part.FormName())
		if part.FormName() == "file" {
			record.file = data
		} else {
			record.values[part.FormName()] = string(data)
		}
	}

	if f.expiredPolicies > 0 {
		f.expiredPolicies--
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Invalid according to Policy: Policy expired.</Message></Error>`)

		return
	}

	f.calls.uploads = append(f.calls.uploads, record)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeDashScope) serveSubmit(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.calls.submitCalls++
	f.calls.submitBodies = append(f.calls.submitBodies, string(body))
	f.calls.submitHeaders = append(f.calls.submitHeaders, r.Header.Clone())

	if f.submitFailure.status != 0 {
		w.WriteHeader(f.submitFailure.status)
		_, _ = io.WriteString(w, f.submitFailure.body)

		return
	}

	_, _ = fmt.Fprintf(w, `{"output":{"task_status":"PENDING","task_id":"%s"},"request_id":"req-submit"}`, fakeTaskID)
}

func (f *fakeDashScope) servePoll(w http.ResponseWriter, r *http.Request) {
	f.calls.pollCalls++
	f.calls.pollAuth = append(f.calls.pollAuth, r.Header.Get("Authorization"))
	f.calls.pollRelay = append(f.calls.pollRelay, r.Header.Get("X-Relay-Token"))

	if f.pollReject != 0 {
		http.Error(w, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`, f.pollReject)
		return
	}

	if !f.keyAccepted(r) {
		http.Error(w, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`, http.StatusUnauthorized)
		return
	}

	if f.pollErrors > 0 {
		f.pollErrors--
		http.Error(w, `{"code":"InternalError","message":"try later"}`, http.StatusInternalServerError)

		return
	}

	status := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}

	_, _ = io.WriteString(w, f.taskJSON(status))
}

// taskJSON mimics the messy task-query JSON: duplicated top-level fields, a nested output copy.
func (f *fakeDashScope) taskJSON(status string) string {
	common := fmt.Sprintf(`"task_id":"%s","task_status":"%s","submit_time":"2026-11-05 10:00:00.000"`, fakeTaskID, status)

	switch status {
	case "SUCCEEDED":
		subtask := f.subtask
		if subtask == "" {
			subtask = fmt.Sprintf(`{"file_url":"oss://x","transcription_url":"%s/result/%s.json?Expires=1&Signature=s3cr3t","subtask_status":"SUCCEEDED"}`,
				f.server.URL, fakeTaskID)
		}

		return fmt.Sprintf(`{"request_id":"req-poll","task_status":"%s","output":{%s,"end_time":"2026-11-05 10:00:03.000",`+
			`"results":[%s],"task_metrics":{"TOTAL":1,"SUCCEEDED":1,"FAILED":0}},"usage":%s}`, status, common, subtask, f.usage)
	case "FAILED":
		return fmt.Sprintf(`{"request_id":"req-poll","output":{%s,"code":"%s","message":"%s"}}`,
			common, f.taskFailure[0], f.taskFailure[1])
	default:
		return fmt.Sprintf(`{"request_id":"req-poll","output":{%s}}`, common)
	}
}

func (f *fakeDashScope) serveFlash(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.calls.flashCalls++
	f.calls.flashBodies = append(f.calls.flashBodies, string(body))
	f.calls.flashHeaders = append(f.calls.flashHeaders, r.Header.Clone())

	if r.Header.Get("X-DashScope-SSE") == "enable" {
		w.Header().Set("Content-Type", "text/event-stream;charset=UTF-8")
		_, _ = io.WriteString(w, flashSSE)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, flashBody)
}

func (f *fakeDashScope) snapshot() fakeCalls {
	f.mu.Lock()
	defer f.mu.Unlock()

	calls := f.calls
	calls.uploads = append([]uploadRecord(nil), f.calls.uploads...)
	calls.submitBodies = append([]string(nil), f.calls.submitBodies...)
	calls.submitHeaders = append([]http.Header(nil), f.calls.submitHeaders...)
	calls.resultAuth = append([]string(nil), f.calls.resultAuth...)
	calls.pollAuth = append([]string(nil), f.calls.pollAuth...)
	calls.flashBodies = append([]string(nil), f.calls.flashBodies...)
	calls.flashHeaders = append([]http.Header(nil), f.calls.flashHeaders...)
	calls.policyRelay = append([]string(nil), f.calls.policyRelay...)
	calls.pollRelay = append([]string(nil), f.calls.pollRelay...)

	return calls
}

func (f *fakeDashScope) set(fn func(*fakeDashScope)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}
