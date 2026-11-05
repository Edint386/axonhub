package bailianasr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// streamFolder turns raw upstream events into transcript text deltas and keeps what the final
// transcript.text.done event and the usage record need.
type streamFolder interface {
	// fold consumes one raw event and returns the text deltas it adds.
	fold(event *httpclient.StreamEvent) ([]string, error)
	// finish returns any text still owed once upstream has ended.
	finish() []string
	result() streamResult
}

type streamResult struct {
	id       string
	text     string
	usage    dsUsage
	duration float64
	// final is set once the upstream reported the outcome of the whole call: the flash event
	// with the call's token usage, task-finished, or the async result.
	final bool
}

func newStreamFolder(kind string) streamFolder {
	switch kind {
	case kindAsync:
		return &asyncFolder{}
	case kindMessage:
		return &messageFolder{}
	default:
		return &flashFolder{}
	}
}

// flashFolder reads the qwen-audio-3.1-asr-flash SSE events. One event arrives per finished
// sentence and its sentence.text is cumulative across sentences (live probe 2026-11-05), so
// only the new suffix becomes a delta. Usage on the last event covers the whole call; earlier
// finals carry a partial usage (duration only), so only usage with token counts marks the call
// as complete.
type flashFolder struct {
	emitted string // the deltas sent so far, concatenated
	latest  string
	// cumulative is latched once a final extended the text sent so far; from then on full is
	// the latest cumulative text and sentRunes how much of it the deltas covered.
	cumulative bool
	full       string
	sentRunes  int
	res        streamResult
}

func (f *flashFolder) fold(event *httpclient.StreamEvent) ([]string, error) {
	if event == nil || len(event.Data) == 0 {
		return nil, nil
	}

	root := gjson.ParseBytes(event.Data)
	if !root.IsObject() {
		return nil, nil
	}

	if !root.Get("output").Exists() {
		if code, message, requestID, ok := dashScopeError(event.Data); ok {
			return nil, newResponseError(statusForCode(code, http.StatusBadGateway), code, message, requestID, nil)
		}

		return nil, nil
	}

	if id := root.Get("request_id").String(); id != "" {
		f.res.id = id
	}

	if usage := root.Get("usage"); usage.Exists() {
		f.res.usage = parseUsage(usage)
		f.res.final = f.res.usage.hasTokens()
	}

	sentence := root.Get("output.sentence")
	if full := firstString(sentence.Get("text"), root.Get("output.text")); full != "" {
		f.latest = full
	}

	if !sentence.Get("sentence_end").Bool() {
		return nil, nil
	}

	return f.advance(f.latest), nil
}

func (f *flashFolder) finish() []string {
	return f.advance(f.latest)
}

// advance emits what full adds to the text sent so far. Until a final has extended the text
// sent so far, a text that does not extend it is taken as a new, non-cumulative sentence rather
// than dropped.
func (f *flashFolder) advance(full string) []string {
	if full == "" {
		return nil
	}

	if f.cumulative {
		return f.advanceCumulative(full)
	}

	switch {
	case strings.HasPrefix(f.emitted, full):
		return nil
	case strings.HasPrefix(full, f.emitted):
		delta := full[len(f.emitted):]
		f.cumulative = f.emitted != ""
		f.emitted, f.full, f.sentRunes = full, full, utf8.RuneCountInString(full)

		return []string{delta}
	case revisesCumulative(f.emitted, full):
		f.cumulative = true
		f.full, f.sentRunes = f.emitted, utf8.RuneCountInString(f.emitted)

		return f.advanceCumulative(full)
	default:
		f.emitted += full
		return []string{full}
	}
}

// revisesCumulative reports a cumulative text that changed a few characters of the text sent so
// far (an ITN or punctuation revision of an earlier sentence) and goes on after it, as opposed
// to the next sentence on its own: it is longer, and position by position its start matches all
// but a fifth (at least one) of the runes sent. Short texts are never taken as revised.
func revisesCumulative(sent, full string) bool {
	before, after := []rune(sent), []rune(full)
	if len(before) < 4 || len(after) <= len(before) {
		return false
	}

	changed := 0

	for i, r := range before {
		if after[i] != r {
			changed++
		}
	}

	return changed <= max(1, len(before)/5)
}

// advanceCumulative handles a cumulative text, which may revise characters already sent (ITN or
// punctuation of an earlier sentence): only the runes past those already covered are new, so a
// revision never repeats the earlier text in the deltas. The final text is full itself.
func (f *flashFolder) advanceCumulative(full string) []string {
	if strings.HasPrefix(f.full, full) {
		return nil
	}

	f.full = full

	runes := []rune(full)
	if len(runes) <= f.sentRunes {
		return nil
	}

	delta := string(runes[f.sentRunes:])
	f.sentRunes = len(runes)
	f.emitted += delta

	return []string{delta}
}

func (f *flashFolder) result() streamResult {
	res := f.res

	res.text = f.emitted
	if f.cumulative {
		res.text = f.full
	}

	res.duration = res.usage.Duration

	return res
}

// asyncFolder reads the single result event of an async job and replays it sentence by sentence.
type asyncFolder struct {
	res streamResult
}

func (f *asyncFolder) fold(event *httpclient.StreamEvent) ([]string, error) {
	if event == nil || event.Type != resultEventType {
		return nil, nil
	}

	t, err := decodeResponse(kindAsync, event.Data)
	if err != nil {
		return nil, err
	}

	f.res = streamResult{id: t.ID, text: t.Text, usage: t.Usage, duration: t.Duration, final: true}

	return segmentDeltas(t), nil
}

func (f *asyncFolder) finish() []string { return nil }

func (f *asyncFolder) result() streamResult { return f.res }

// segmentDeltas cuts t.Text into one delta per segment; the deltas always concatenate to t.Text.
func segmentDeltas(t *transcript) []string {
	if t.Text == "" {
		return nil
	}

	var deltas []string

	cursor := 0
	for _, seg := range t.Segments {
		if seg.Text == "" {
			continue
		}

		i := strings.Index(t.Text[cursor:], seg.Text)
		if i < 0 {
			continue
		}

		end := cursor + i + len(seg.Text)
		deltas = append(deltas, t.Text[cursor:end])
		cursor = end
	}

	switch {
	case len(deltas) == 0:
		return []string{t.Text}
	case cursor < len(t.Text):
		deltas[len(deltas)-1] += t.Text[cursor:]
	}

	return deltas
}

// transcriptStream maps raw upstream events to OpenAI transcript.text.delta events and ends
// with transcript.text.done (carrying usage) once upstream has ended cleanly.
type transcriptStream struct {
	src    streams.Stream[*httpclient.StreamEvent]
	folder streamFolder
	opts   renderOptions
	// billsUsage (when set) tells whether a complete result is billed by this request; see
	// job.billsUsage.
	billsUsage func() bool
	queue      []*llm.Response
	cur        *llm.Response
	err        error
	ended      bool
}

func newTranscriptStream(src streams.Stream[*httpclient.StreamEvent], folder streamFolder, opts renderOptions) *transcriptStream {
	return &transcriptStream{src: src, folder: folder, opts: opts}
}

func (s *transcriptStream) Next() bool {
	for len(s.queue) == 0 {
		if s.ended || s.err != nil {
			return false
		}

		if s.src.Next() {
			ev := s.src.Current()
			if ev != nil && ev.Type == committedEventType {
				s.queue = append(s.queue, s.committed())
				continue
			}

			deltas, err := s.folder.fold(ev)
			if err != nil {
				s.err = err
				return false
			}

			s.enqueueDeltas(deltas)

			continue
		}

		if err := s.src.Err(); err != nil {
			s.err = err
			return false
		}

		s.enqueueDeltas(s.folder.finish())
		s.queue = append(s.queue, s.done(), llm.DoneResponse)
		s.ended = true
	}

	s.cur, s.queue = s.queue[0], s.queue[1:]

	return true
}

func (s *transcriptStream) Current() *llm.Response { return s.cur }

func (s *transcriptStream) Err() error { return s.err }

func (s *transcriptStream) Close() error { return s.src.Close() }

func (s *transcriptStream) enqueueDeltas(deltas []string) {
	for _, delta := range deltas {
		if delta == "" {
			continue
		}

		s.queue = append(s.queue, s.event(&llm.TranscriptionStreamEvent{Type: "transcript.text.delta", Delta: delta}, nil))
	}
}

// done carries the usage both on the response (billing) and on the client event. The client
// sees it in the gateway's usage shape, as for every other transcription stream. A result
// another request already billed keeps its usage on the client event and bills zero.
func (s *transcriptStream) done() *llm.Response {
	res := s.folder.result()
	usage := res.usage.llmUsage(res.duration)

	billed := usage
	if res.final && s.billsUsage != nil && !s.billsUsage() {
		billed = &llm.Usage{}
	}

	resp := s.event(&llm.TranscriptionStreamEvent{Type: "transcript.text.done", Text: res.text, Usage: usage}, billed)
	resp.ID = res.id

	return resp
}

// committed is the response for the committed event: a non-empty Transcription.Raw makes the
// pipeline treat it as content (llm/pipeline/empty_response.go hasResponseContent), so the
// attempt commits and the SSE response starts; the OpenAI inbound stream forwards only
// TranscriptionStreamEvent, so the client never receives it. The api package test
// TestCreateTranscription_BailianAsyncStreamCommitsBeforeTheResult guards both couplings.
func (s *transcriptStream) committed() *llm.Response {
	return &llm.Response{
		Model:         s.opts.Model,
		RequestType:   llm.RequestTypeTranscription,
		APIFormat:     llm.APIFormatOpenAITranscription,
		Transcription: &llm.TranscriptionResponse{Raw: []byte("{}"), RawContentType: contentTypeJSON},
	}
}

func (s *transcriptStream) event(ev *llm.TranscriptionStreamEvent, usage *llm.Usage) *llm.Response {
	return &llm.Response{
		Model:                    s.opts.Model,
		RequestType:              llm.RequestTypeTranscription,
		APIFormat:                llm.APIFormatOpenAITranscription,
		TranscriptionStreamEvent: ev,
		Usage:                    usage,
	}
}

// aggregateChunks rebuilds the stream outcome from the raw events for persistence and billing.
// A stream that failed (an in-band error event) is never complete and carries no usage, so a
// failed attempt is not billed next to its retry.
func aggregateChunks(folder streamFolder, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	failed := false

	for _, chunk := range chunks {
		if _, err := folder.fold(chunk); err != nil {
			failed = true
			break
		}
	}

	folder.finish()
	res := folder.result()
	res.final = res.final && !failed

	body, err := json.Marshal(map[string]string{"text": res.text})
	if err != nil {
		return nil, llm.ResponseMeta{}, fmt.Errorf("failed to marshal transcription stream aggregate: %w", err)
	}

	meta := llm.ResponseMeta{ID: res.id, Completed: res.final}
	if res.final {
		meta.Usage = res.usage.llmUsage(res.duration)
		meta.AccountingEvidence = llm.AccountingEvidenceExplicitFinal
	}

	return body, meta, nil
}
