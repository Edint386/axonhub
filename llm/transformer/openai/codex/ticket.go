package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex/turnstate"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func (e *codexExecutor) applyTicket(ctx context.Context, request *httpclient.Request) (*turnstate.Ticket, error) {
	if request == nil {
		return nil, nil
	}
	if request.Headers == nil {
		request.Headers = make(http.Header)
	}
	if e.transformer == nil || e.transformer.tickets == nil || !e.transformer.isOfficialCodex() {
		return nil, nil
	}
	if e.transformer.transport == responses.TransportWebSocket {
		return nil, nil
	}
	if request.RequestType == string(llm.RequestTypeCompact) ||
		request.RequestType == llm.RequestTypeAlphaSearch.String() {
		return nil, nil
	}
	model := strings.TrimSpace(gjson.GetBytes(request.Body, "model").String())
	ticket, err := e.transformer.tickets.Acquire(ctx, turnstate.Request{
		ChannelID:   e.transformer.channelID,
		Model:       model,
		RequestType: request.RequestType,
	})
	if err != nil {
		if errors.Is(err, turnstate.ErrUnavailable) {
			return nil, &httpclient.Error{
				Method:     request.Method,
				URL:        request.URL,
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Body:       []byte(`{"error":{"message":"codex turn-state ticket unavailable","type":"state_unavailable","code":"state_unavailable"}}`),
			}
		}
		return nil, err
	}
	if ticket == nil {
		return nil, nil
	}
	turnstate.Inject(request.Headers, ticket.State)
	return ticket, nil
}

func (e *codexExecutor) observeResponse(ticket *turnstate.Ticket, resp *httpclient.Response, err error) {
	if ticket == nil || e.transformer == nil || e.transformer.tickets == nil || err != nil || resp == nil {
		return
	}
	actual := ""
	if complete, matches, model := inspectCompletedModel(ticket.Model, resp.Body); complete && !matches {
		actual = model
	}
	e.transformer.tickets.Observe(ticket, resp.Headers, actual)
}

func (e *codexExecutor) observeCollected(ticket *turnstate.Ticket, headers http.Header, actualModel string) {
	if ticket == nil || e.transformer == nil || e.transformer.tickets == nil {
		return
	}
	e.transformer.tickets.Observe(ticket, headers, actualModel)
}

func (e *codexExecutor) observeStream(ticket *turnstate.Ticket, request *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
	if ticket == nil || stream == nil || e.transformer == nil || e.transformer.tickets == nil {
		return stream
	}
	model := ticket.Model
	if model == "" && request != nil {
		model = strings.TrimSpace(gjson.GetBytes(request.Body, "model").String())
	}
	return &observingStream{
		Stream:   stream,
		source:   e.transformer.tickets,
		ticket:   ticket,
		headers:  httpclient.StreamResponseHeaders(stream),
		observer: turnstate.NewCompletionObserver(model),
	}
}

func inspectCompletedModel(expected string, body []byte) (complete bool, matches bool, actual string) {
	observer := turnstate.NewCompletionObserver(expected)
	observer.WriteJSON(body)
	return observer.Result()
}

type observingStream struct {
	streams.Stream[*httpclient.StreamEvent]
	source   turnstate.Source
	ticket   *turnstate.Ticket
	headers  http.Header
	observer *turnstate.CompletionObserver
	once     sync.Once
}

func (s *observingStream) Next() bool {
	ok := s.Stream.Next()
	if ok {
		if ev := s.Stream.Current(); ev != nil && len(ev.Data) > 0 {
			s.observer.WriteJSON(ev.Data)
		}
		return true
	}
	s.finish()
	return false
}

func (s *observingStream) Close() error {
	s.finish()
	return s.Stream.Close()
}

func (s *observingStream) finish() {
	s.once.Do(func() {
		s.observer.Finish()
		actual := ""
		if complete, matches, model := s.observer.Result(); complete && !matches {
			actual = model
		}
		s.source.Observe(s.ticket, s.headers, actual)
	})
}
