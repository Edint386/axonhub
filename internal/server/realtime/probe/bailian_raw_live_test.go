package probe

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestBailianLive_RawFrames sends one session.update per variant on a fresh connection and
// logs the raw frames that come back, including close frames whose reason exceeds the 125-byte
// control-frame limit (gorilla reports those only as "len > 125 for control", hiding the
// reason). Use it to find out which field of an update Bailian rejects.
func TestBailianLive_RawFrames(t *testing.T) {
	model := os.Getenv("TEST_BAILIAN_MODEL")
	if model == "" {
		model = "qwen3.5-omni-plus-realtime"
	}

	variants := []struct {
		name   string
		update string
	}{
		{"modalities_only", `{"event_id":"raw_1","type":"session.update","session":{"modalities":["text","audio"]}}`},
		{"voice_instructions", `{"event_id":"raw_2","type":"session.update","session":{"modalities":["text","audio"],"voice":"Tina","instructions":"Reply briefly."}}`},
		{"transcription", `{"event_id":"raw_3","type":"session.update","session":{"modalities":["text","audio"],"input_audio_transcription":{"model":"qwen3-asr-flash-realtime"}}}`},
		{"turn_detection_full", `{"event_id":"raw_4","type":"session.update","session":{"turn_detection":{"type":"server_vad","threshold":0.3,"silence_duration_ms":500,"prefix_padding_ms":300,"create_response":true,"interrupt_response":true}}}`},
		{"nested_audio_24k", `{"event_id":"raw_5","type":"session.update","session":{"audio":{"input":{"format":{"type":"pcm","sample_rate":24000}},"output":{"voice":"Tina","format":{"type":"pcm","sample_rate":24000}}}}}`},
		{"nested_audio_16k_input_only", `{"event_id":"raw_6","type":"session.update","session":{"audio":{"input":{"format":{"type":"pcm","sample_rate":16000}}}}}`},
		{"flat_formats", `{"event_id":"raw_7","type":"session.update","session":{"input_audio_format":"pcm","output_audio_format":"pcm"}}`},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			p := dialBailian(t, model)

			created := p.recv(10*time.Second, "session.created", "error")
			require.Contains(t, created, "session.created")

			p.send(v.update)

			frames := readRawFrames(t, p.conn.UnderlyingConn(), 4*time.Second)
			t.Logf("%s -> %s\n%s", v.name, truncate(v.update), strings.Join(frames, "\n"))
		})
	}
}

// readRawFrames reads unmasked server frames straight from the connection until timeout or a
// close frame and renders each as text (JSON frames are compacted, close frames show code and
// reason). The gorilla reader must not have buffered beyond the last message read through it.
func readRawFrames(t *testing.T, conn net.Conn, timeout time.Duration) []string {
	t.Helper()

	var out []string

	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(conn, header); err != nil {
			return append(out, "<- read end: "+err.Error())
		}

		opcode := header[0] & 0x0f
		length := uint64(header[1] & 0x7f)

		switch length {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(conn, ext); err != nil {
				return append(out, "<- read end: "+err.Error())
			}

			length = uint64(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(conn, ext); err != nil {
				return append(out, "<- read end: "+err.Error())
			}

			length = binary.BigEndian.Uint64(ext)
		}

		if header[1]&0x80 != 0 {
			// Servers never mask; skip the key if one shows up anyway.
			if _, err := io.ReadFull(conn, make([]byte, 4)); err != nil {
				return append(out, "<- read end: "+err.Error())
			}
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return append(out, "<- read end: "+err.Error())
		}

		switch opcode {
		case 0x8:
			code := 0
			reason := ""

			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
				reason = string(payload[2:])
			}

			return append(out, "<- CLOSE code="+itoaInt(code)+" reason_len="+itoaInt(len(reason))+" reason="+reason)
		case 0x9:
			out = append(out, "<- PING")
		case 0xa:
			out = append(out, "<- PONG")
		default:
			text := string(payload)
			if json.Valid(payload) {
				text = gjson.ParseBytes(payload).Get("@this").Raw
			}

			out = append(out, "<- "+truncate(text))
		}
	}
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

var _ = websocket.TextMessage
