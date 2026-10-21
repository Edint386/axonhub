package bailian

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// maxCachedItems bounds the per-session item cache used to answer conversation.item.retrieve.
const maxCachedItems = 512

// itemCache keeps GA-normalised conversation items keyed by id, evicting the oldest first.
type itemCache struct {
	items map[string]json.RawMessage
	order []string
}

func newItemCache() *itemCache {
	return &itemCache{items: map[string]json.RawMessage{}}
}

// put stores an item under its id; an existing entry keeps its position.
func (c *itemCache) put(id string, item json.RawMessage) {
	if id == "" {
		return
	}

	if _, ok := c.items[id]; !ok {
		c.order = append(c.order, id)

		for len(c.order) > maxCachedItems {
			delete(c.items, c.order[0])
			c.order = c.order[1:]
		}
	}

	c.items[id] = item
}

// get returns the cached item, if any.
func (c *itemCache) get(id string) (json.RawMessage, bool) {
	item, ok := c.items[id]

	return item, ok
}

// remove forgets an item.
func (c *itemCache) remove(id string) {
	if _, ok := c.items[id]; !ok {
		return
	}

	delete(c.items, id)

	for i, o := range c.order {
		if o == id {
			c.order = append(c.order[:i], c.order[i+1:]...)

			break
		}
	}
}

// setTranscript writes a transcript onto the input_audio content part at index of a cached item.
func (c *itemCache) setTranscript(id string, index int, transcript string) {
	item, ok := c.items[id]
	if !ok {
		return
	}

	path := "content." + itoa(index) + ".transcript"
	if updated, err := sjson.SetBytes(item, path, transcript); err == nil {
		c.items[id] = updated
	}
}

// normalizeItem converts a Bailian conversation item to its GA shape:
//   - audio parts carrying the transcript in "text" move it to "transcript";
//   - assistant message parts are renamed text→output_text and audio→output_audio;
//   - messages whose content is all input_* are forced to role user;
//   - audio payload bytes are stripped (GA item events never carry them).
func normalizeItem(item []byte) []byte {
	parsed := gjson.ParseBytes(item)
	if parsed.Get("type").String() != "message" {
		return item
	}

	content := parsed.Get("content")
	if !content.IsArray() {
		return item
	}

	parts := content.Array()
	role := parsed.Get("role").String()

	allInput := len(parts) > 0

	for _, p := range parts {
		if !strings.HasPrefix(p.Get("type").String(), "input_") {
			allInput = false

			break
		}
	}

	if allInput {
		role = "user"
	}

	out := item

	for i, p := range parts {
		np := normalizePartRaw([]byte(p.Raw), role == "assistant")
		if np != nil {
			out, _ = sjson.SetRawBytes(out, "content."+itoa(i), np)
		}
	}

	if role != "" && role != parsed.Get("role").String() {
		out, _ = sjson.SetBytes(out, "role", role)
	}

	return out
}

// normalizePartRaw normalises one content part; it returns nil when nothing changed.
func normalizePartRaw(part []byte, assistant bool) []byte {
	p := gjson.ParseBytes(part)
	typ := p.Get("type").String()

	out := part
	changed := false

	if typ == "audio" || typ == "output_audio" || typ == "input_audio" {
		if !p.Get("transcript").Exists() {
			if text := p.Get("text"); text.Type == gjson.String {
				out, _ = sjson.SetBytes(out, "transcript", text.String())
				out, _ = sjson.DeleteBytes(out, "text")
				changed = true
			}
		}

		if p.Get("audio").Exists() {
			out, _ = sjson.DeleteBytes(out, "audio")
			changed = true
		}
	}

	if assistant {
		switch typ {
		case "text":
			out, _ = sjson.SetBytes(out, "type", "output_text")
			changed = true
		case "audio":
			out, _ = sjson.SetBytes(out, "type", "output_audio")
			changed = true
		}
	}

	if !changed {
		return nil
	}

	return out
}

// normalizeContentPart normalises the "part" of response.content_part.* events: the type
// stays text/audio, only the transcript field moves.
func normalizeContentPart(frame []byte) []byte {
	part := gjson.GetBytes(frame, "part")
	if !part.IsObject() {
		return frame
	}

	np := normalizePartRaw([]byte(part.Raw), false)
	if np == nil {
		return frame
	}

	out, err := sjson.SetRawBytes(frame, "part", np)
	if err != nil {
		return frame
	}

	return out
}

// isAllInputContent reports whether a message item carries only input_* parts.
func isAllInputContent(item gjson.Result) bool {
	content := item.Get("content")
	if !content.IsArray() {
		return false
	}

	parts := content.Array()
	if len(parts) == 0 {
		return false
	}

	for _, p := range parts {
		if !strings.HasPrefix(p.Get("type").String(), "input_") {
			return false
		}
	}

	return true
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
