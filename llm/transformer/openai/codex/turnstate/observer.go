package turnstate

import (
	"bytes"
	"encoding/json"
	"strings"
)

const maxObservedFrame = 1 << 20

type CompletionObserver struct {
	expected                                  string
	actual                                    string
	line, event                               []byte
	lineOverflow, eventOverflow               bool
	complete, matches                         bool
}

func NewCompletionObserver(expected string) *CompletionObserver {
	return &CompletionObserver{expected: strings.TrimSpace(expected), matches: true}
}

func (o *CompletionObserver) Write(p []byte) {
	for _, b := range p {
		if b == '\n' {
			o.consumeLine()
			continue
		}
		if o.lineOverflow {
			continue
		}
		if len(o.line) >= maxObservedFrame {
			o.line, o.lineOverflow = nil, true
			continue
		}
		o.line = append(o.line, b)
	}
}

func (o *CompletionObserver) WriteJSON(data []byte) {
	o.inspect(data)
}

func (o *CompletionObserver) Finish() {
	if len(o.line) > 0 || o.lineOverflow {
		o.consumeLine()
	}
	o.consumeEvent()
}

func (o *CompletionObserver) Result() (complete bool, matches bool, actual string) {
	return o.complete, o.complete && o.matches, o.actual
}

func (o *CompletionObserver) consumeLine() {
	if o.lineOverflow {
		o.eventOverflow = true
		o.lineOverflow = false
		o.line = o.line[:0]
		return
	}
	line := bytes.TrimSuffix(o.line, []byte{'\r'})
	if len(line) == 0 {
		o.consumeEvent()
	} else if bytes.HasPrefix(line, []byte("data:")) && !o.eventOverflow {
		data := line[5:]
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		if len(o.event)+len(data)+1 > maxObservedFrame {
			o.event, o.eventOverflow = nil, true
		} else {
			o.event = append(o.event, data...)
			o.event = append(o.event, '\n')
		}
	}
	o.line = o.line[:0]
}

func (o *CompletionObserver) consumeEvent() {
	if !o.eventOverflow && len(o.event) > 0 {
		o.inspect(o.event)
	}
	o.event = o.event[:0]
	o.eventOverflow = false
}

func (o *CompletionObserver) inspect(data []byte) {
	if len(data) == 0 || len(data) > maxObservedFrame {
		return
	}
	var value struct {
		Type     string          `json:"type"`
		Object   string          `json:"object"`
		Status   string          `json:"status"`
		Model    string          `json:"model"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Status string          `json:"status"`
			Model  string          `json:"model"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &value) != nil {
		return
	}
	model := ""
	if value.Type == "response.completed" && value.Response != nil && noResponseError(value.Error) && noResponseError(value.Response.Error) &&
		(value.Response.Status == "completed" || value.Response.Status == "") {
		model = value.Response.Model
	} else if value.Object == "response" && value.Status == "completed" && noResponseError(value.Error) {
		model = value.Model
	}
	if strings.TrimSpace(model) == "" {
		return
	}
	o.actual = strings.TrimSpace(model)
	o.complete = true
	o.matches = o.matches && o.actual == o.expected
}

func noResponseError(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
