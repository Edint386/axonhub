package bailian

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

func TestSupported(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"qwen3.8-omni-flash-realtime", true},
		{"qwen3.5-omni-plus-realtime", true},
		{"qwen3.5-omni-flash-realtime", true},
		{"qwen3-omni-flash-realtime-2025-09-15", true},
		{"qwen-omni-turbo-realtime", true},
		{"qwen-omni-turbo-realtime-2025-05-08", true},
		{"Qwen3.8-Omni-Flash-Realtime", true},
		{"qwen3-livetranslate-flash-realtime", false},
		{"qwen3.8-livetranslate-flash-realtime", false},
		{"qwen-audio-asr-realtime", false},
		{"qwen3-asr-flash-realtime", false},
		{"gpt-realtime", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, Supported(tt.model))
		})
	}
}

func TestCapabilitiesFor(t *testing.T) {
	tests := []struct {
		model string
		want  Capabilities
	}{
		{"qwen3.5-omni-plus-realtime", Capabilities{NestedAudio: true, InputRate: 24000, Resample: false, SemanticVAD: true, IdleTimeout: true, TranscriptionModel: "qwen3-asr-flash-realtime", OutputFormatSettable: true, VoiceFamily: "3.5", DefaultVoice: "Tina", SessionFinish: true}},
		{"qwen3.5-omni-flash-realtime", Capabilities{NestedAudio: true, InputRate: 24000, Resample: false, SemanticVAD: true, IdleTimeout: true, TranscriptionModel: "qwen3-asr-flash-realtime", OutputFormatSettable: true, VoiceFamily: "3.5", DefaultVoice: "Tina", SessionFinish: true}},
		// 3.8 takes 24 kHz both ways and honours idle_timeout_ms, but rejects session.finish
		// and cannot speak its own default voice (live probe 2026-10-22).
		{"qwen3.8-omni-flash-realtime", Capabilities{NestedAudio: true, InputRate: 24000, Resample: false, SemanticVAD: true, IdleTimeout: true, TranscriptionModel: "qwen3-asr-flash-realtime", OutputFormatSettable: true, VoiceFamily: "3.8", DefaultVoice: "Tina", SessionFinish: false}},
		{"qwen3-omni-flash-realtime-2025-09-15", Capabilities{NestedAudio: false, InputRate: 16000, Resample: true, SemanticVAD: false, IdleTimeout: false, TranscriptionModel: "qwen3-asr-flash-realtime", OutputFormatSettable: false, VoiceFamily: "legacy", DefaultVoice: "Cherry", SessionFinish: true}},
		{"qwen-omni-turbo-realtime", Capabilities{NestedAudio: false, InputRate: 16000, Resample: true, SemanticVAD: false, IdleTimeout: false, TranscriptionModel: "qwen3-asr-flash-realtime", OutputFormatSettable: false, VoiceFamily: "legacy", DefaultVoice: "Cherry", SessionFinish: true}},
	}

	// Every family default must be in its own allow-list, or the init update could never
	// carry a usable voice.
	for _, model := range []string{"qwen3.5-omni-plus-realtime", "qwen3.8-omni-flash-realtime", "qwen-omni-turbo-realtime"} {
		caps := CapabilitiesFor(model)
		_, ok := canonicalVoice(caps.DefaultVoice, caps)
		require.True(t, ok, "%s: default voice %q is not in the %s allow-list", model, caps.DefaultVoice, caps.VoiceFamily)
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, CapabilitiesFor(tt.model))
		})
	}
}

func TestCanonicalVoice(t *testing.T) {
	tests := []struct {
		in     string
		family string
		want   string
		ok     bool
	}{
		{"Tina", "3.8", "Tina", true},
		{"tina", "3.5", "Tina", true},
		{" CINDY ", "3.8", "Cindy", true},
		{"Cherry", "legacy", "Cherry", true},
		{"Ethan", "3.5", "Ethan", true},
		// Family-specific voices are rejected elsewhere so the whole update is not refused.
		{"Zane", "3.8", "Zane", true},
		{"Zane", "3.5", "", false},
		{"Ethan", "3.8", "", false},
		{"Cherry", "3.8", "", false},
		{"ash", "3.8", "", false},
		{"marin", "3.5", "", false},
		{"", "3.8", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.family+"/"+tt.in, func(t *testing.T) {
			got, ok := canonicalVoice(tt.in, Capabilities{VoiceFamily: tt.family})
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEffectiveVoice(t *testing.T) {
	tests := []struct {
		in    string
		model string
		want  string
	}{
		// The upstream default of 3.8 is not usable there; it is on legacy models.
		{"Chelsie", "qwen3.8-omni-flash-realtime", "Tina"},
		{"Chelsie", "qwen3.5-omni-plus-realtime", "Tina"},
		{"Chelsie", "qwen-omni-turbo-realtime", "Chelsie"},
		{"", "qwen3.8-omni-flash-realtime", "Tina"},
		{"", "qwen3-omni-flash-realtime", "Cherry"},
		{"alloy", "qwen3.8-omni-flash-realtime", "Tina"},
		{"zane", "qwen3.8-omni-flash-realtime", "Zane"},
		{"Zane", "qwen3.5-omni-plus-realtime", "Tina"},
		{"cindy", "qwen3.5-omni-plus-realtime", "Cindy"},
		{"Tina", "qwen-omni-turbo-realtime", "Cherry"},
	}

	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.in, func(t *testing.T) {
			require.Equal(t, tt.want, effectiveVoice(tt.in, CapabilitiesFor(tt.model)))
		})
	}
}

func TestConvertImage_RejectsOversizedDimensions(t *testing.T) {
	// A tiny PNG that declares a huge canvas must be refused before image.Decode allocates.
	var buf bytes.Buffer

	img := image.NewGray(image.Rect(0, 0, 16, 16))
	require.NoError(t, png.Encode(&buf, img))

	// Patch the IHDR width/height to 30000x30000 (bytes 16-23) and refresh the IHDR CRC
	// (over the chunk type + data, bytes 12-29): DecodeConfig only reads the header, which is
	// all convertImage may touch before rejecting.
	data := buf.Bytes()
	binary.BigEndian.PutUint32(data[16:20], 30000)
	binary.BigEndian.PutUint32(data[20:24], 30000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))

	_, err := convertImage("data:image/png;base64," + base64.StdEncoding.EncodeToString(data))
	require.Error(t, err)
	require.Contains(t, err.Error(), "dimensions")
}

func TestNew(t *testing.T) {
	t.Run("rejects unsupported model", func(t *testing.T) {
		_, err := New(Options{ActualModel: "qwen3-livetranslate-flash-realtime"})
		require.Error(t, err)
	})

	t.Run("defaults requested model and ids", func(t *testing.T) {
		d, err := New(Options{ActualModel: "qwen-omni-turbo-realtime"})
		require.NoError(t, err)
		require.Equal(t, "bailian", d.Name())

		bd, ok := d.(*Dialect)
		require.True(t, ok)
		require.Equal(t, "qwen-omni-turbo-realtime", bd.opts.RequestedModel)
		require.NotNil(t, bd.ids)
		require.NotNil(t, bd.resampler, "legacy models take 16 kHz and need the resampler")
		require.True(t, bd.Capabilities().Resample)
	})

	t.Run("24 kHz families need no resampler", func(t *testing.T) {
		for _, model := range []string{"qwen3.5-omni-plus-realtime", "qwen3.8-omni-flash-realtime"} {
			d := newTestDialect(t, model, "", false)
			require.Nil(t, d.resampler, model)
			require.False(t, d.Capabilities().Resample, model)
		}
	})

	t.Run("start is gated and sends nothing", func(t *testing.T) {
		d := newTestDialect(t, "qwen3.5-omni-plus-realtime", "", false)
		frames, gated := d.Start()
		require.Nil(t, frames)
		require.True(t, gated)
	})

	t.Run("close upstream sends session.finish where the family accepts it", func(t *testing.T) {
		for _, model := range []string{"qwen3.5-omni-plus-realtime", "qwen-omni-turbo-realtime"} {
			d := newTestDialect(t, model, "", false)
			frames := d.CloseUpstream()
			require.Len(t, frames, 1, model)
			require.Equal(t, "session.finish", dialect.EventType(frames[0]), model)
		}
	})

	t.Run("close upstream sends nothing on 3.8, which rejects session.finish", func(t *testing.T) {
		d := newTestDialect(t, "qwen3.8-omni-flash-realtime", "", false)
		require.Empty(t, d.CloseUpstream())
	})
}

// appendFrame builds an input_audio_buffer.append carrying pcm.
func appendFrame(t *testing.T, pcm []byte, eventID string) []byte {
	t.Helper()

	ev := map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm)}
	if eventID != "" {
		ev["event_id"] = eventID
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	return b
}

// decodedUpstreamAudio sums the decoded audio bytes of upstream append frames.
func decodedUpstreamAudio(t *testing.T, frames [][]byte) int {
	t.Helper()

	var total int

	for _, f := range frames {
		require.Equal(t, "input_audio_buffer.append", dialect.EventType(f))

		pcm, err := base64.StdEncoding.DecodeString(gjson.GetBytes(f, "audio").String())
		require.NoError(t, err)

		total += len(pcm)
	}

	return total
}

func TestClientAppend_ResampleOnLegacy(t *testing.T) {
	d := newTestDialect(t, "qwen3-omni-flash-realtime-2025-09-15", "", false)
	runInit(t, d, "qwen3-omni-flash-realtime-2025-09-15")

	// 500 ms at 24 kHz = 24000 bytes → 16000 bytes at 16 kHz, split into ≤100 ms (3200 B) chunks.
	in := audio.Silence(24000, 500*time.Millisecond)
	for i := range in {
		in[i] = byte(i * 7)
	}

	out, err := d.ClientFrame(appendFrame(t, in, "a1"))
	require.NoError(t, err)
	require.Empty(t, out.ToClient)
	require.GreaterOrEqual(t, len(out.ToUpstream), 5)

	for _, f := range out.ToUpstream {
		pcm, err := base64.StdEncoding.DecodeString(gjson.GetBytes(f, "audio").String())
		require.NoError(t, err)
		require.LessOrEqual(t, len(pcm), 3200)
		require.Equal(t, 0, len(pcm)%2)
	}

	require.Equal(t, "a1", gjson.GetBytes(out.ToUpstream[0], "event_id").String())
	require.False(t, gjson.GetBytes(out.ToUpstream[1], "event_id").Exists())

	total := decodedUpstreamAudio(t, out.ToUpstream)
	require.InDelta(t, 16000, total, 200, "24k→16k keeps two thirds of the bytes minus filter latency")
	require.Equal(t, int64(24000), d.appendedBytes, "duration accounting uses client-rate bytes")

	// A second append continues the stream; totals converge on 2/3.
	out, err = d.ClientFrame(appendFrame(t, in, ""))
	require.NoError(t, err)
	total += decodedUpstreamAudio(t, out.ToUpstream)
	require.InDelta(t, 32000, total, 200)

	// Clearing resets the resampler and the counter.
	out, err = d.ClientFrame([]byte(`{"type":"input_audio_buffer.clear"}`))
	require.NoError(t, err)
	require.Len(t, out.ToUpstream, 1)
	require.Equal(t, int64(0), d.appendedBytes)
	require.Equal(t, int64(0), d.resampler.ProcessedOutput())
}

// TestClientAppend_Passthrough24k covers the 24 kHz families (3.5 and 3.8): appends are
// forwarded unresampled, with the same ≤100 ms split rule.
func TestClientAppend_Passthrough24k(t *testing.T) {
	for _, model := range []string{"qwen3.5-omni-plus-realtime", "qwen3.8-omni-flash-realtime"} {
		t.Run(model, func(t *testing.T) {
			d := newTestDialect(t, model, "", false)
			runInit(t, d, model)

			in := audio.Silence(24000, 100*time.Millisecond)
			frame := appendFrame(t, in, "a1")

			out, err := d.ClientFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 1)
			require.Equal(t, frame, out.ToUpstream[0], "a ≤100 ms frame is forwarded untouched")
			require.Equal(t, int64(len(in)), d.appendedBytes)
			require.True(t, d.audioStarted)

			// Longer appends are split into ≤100 ms upstream chunks without resampling.
			long := audio.Silence(24000, 300*time.Millisecond)

			out, err = d.ClientFrame(appendFrame(t, long, "a2"))
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 3)

			for _, f := range out.ToUpstream {
				chunk, err := base64.StdEncoding.DecodeString(gjson.GetBytes(f, "audio").String())
				require.NoError(t, err)
				require.Len(t, chunk, 4800)
			}

			require.Equal(t, int64(len(in)+len(long)), d.appendedBytes)

			// Once audio has started, later session updates no longer carry the audio formats.
			out, err = d.ClientFrame([]byte(`{"event_id":"c1","type":"session.update","session":{"instructions":"later"}}`))
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 1)
			require.False(t, gjson.GetBytes(out.ToUpstream[0], "session.audio.input").Exists())
			require.False(t, gjson.GetBytes(out.ToUpstream[0], "session.audio.output.format").Exists())
			require.Equal(t, "Tina", gjson.GetBytes(out.ToUpstream[0], "session.audio.output.voice").String())
		})
	}
}

func TestClientAppend_InvalidBase64(t *testing.T) {
	// Both the pass-through path (3.8) and the resampling path (legacy) must refuse it.
	for _, model := range []string{"qwen3.8-omni-flash-realtime", "qwen-omni-turbo-realtime"} {
		t.Run(model, func(t *testing.T) {
			d := newTestDialect(t, model, "", false)
			runInit(t, d, model)

			out, err := d.ClientFrame([]byte(`{"event_id":"a1","type":"input_audio_buffer.append","audio":"***"}`))
			require.NoError(t, err)
			require.Empty(t, out.ToUpstream)
			require.Len(t, out.ToClient, 1)
			require.Equal(t, "invalid_value", gjson.GetBytes(out.ToClient[0], "error.code").String())
			require.Equal(t, "a1", gjson.GetBytes(out.ToClient[0], "error.event_id").String())
		})
	}
}

func TestClientItem_InputAudioInManualMode(t *testing.T) {
	tests := []struct {
		model     string
		wantBytes int
		note      string
	}{
		{"qwen3.8-omni-flash-realtime", 12000, "250 ms forwarded at 24 kHz"},
		{"qwen-omni-turbo-realtime", 8000, "250 ms resampled to 16 kHz"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			d := newTestDialect(t, tt.model, "", false)
			runInit(t, d, tt.model)

			// Switch to manual mode and ack it.
			out, err := d.ClientFrame([]byte(`{"type":"session.update","session":{"audio":{"input":{"turn_detection":null}}}}`))
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 1)

			_, err = d.UpstreamFrame([]byte(`{"type":"session.updated","session":{}}`))
			require.NoError(t, err)

			pcm := audio.Silence(24000, 250*time.Millisecond)
			item := map[string]any{
				"event_id": "c1",
				"type":     "conversation.item.create",
				"item": map[string]any{
					"type": "message",
					"role": "user",
					"content": []map[string]any{
						{"type": "input_audio", "audio": base64.StdEncoding.EncodeToString(pcm)},
						{"type": "input_text", "text": "and this"},
					},
				},
			}

			frame, err := json.Marshal(item)
			require.NoError(t, err)

			out, err = d.ClientFrame(frame)
			require.NoError(t, err)
			require.Empty(t, out.ToClient)

			n := len(out.ToUpstream)
			require.GreaterOrEqual(t, n, 4)

			appends := out.ToUpstream[:n-2]
			require.InDelta(t, tt.wantBytes, decodedUpstreamAudio(t, appends), 200, tt.note)
			require.Equal(t, "input_audio_buffer.commit", dialect.EventType(out.ToUpstream[n-2]))

			msg := out.ToUpstream[n-1]
			require.Equal(t, "conversation.item.create", dialect.EventType(msg))
			require.Len(t, gjson.GetBytes(msg, "item.content").Array(), 1)
			require.Equal(t, "input_text", gjson.GetBytes(msg, "item.content.0.type").String())
			require.Equal(t, int64(len(pcm)), d.appendedBytes)
		})
	}
}

// pngDataURI encodes a solid-colour PNG of the given size as a GA data URI.
func pngDataURI(t *testing.T, w, h int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestConvertImage(t *testing.T) {
	t.Run("png is transcoded to jpeg", func(t *testing.T) {
		got, err := convertImage(pngDataURI(t, 32, 24))
		require.NoError(t, err)

		raw, err := base64.StdEncoding.DecodeString(got)
		require.NoError(t, err)

		cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
		require.NoError(t, err)
		require.Equal(t, "jpeg", format)
		require.Equal(t, 32, cfg.Width)
		require.Equal(t, 24, cfg.Height)
	})

	t.Run("small jpeg passes through verbatim", func(t *testing.T) {
		img := image.NewGray(image.Rect(0, 0, 8, 8))

		var buf bytes.Buffer
		require.NoError(t, jpeg.Encode(&buf, img, nil))

		payload := base64.StdEncoding.EncodeToString(buf.Bytes())

		got, err := convertImage("data:image/jpeg;base64," + payload)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("oversized image is downscaled to fit", func(t *testing.T) {
		// Noise compresses badly: 1200x1200 noise exceeds 256 KB as JPEG.
		img := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
		seed := uint32(1)

		for i := range img.Pix {
			seed = seed*1664525 + 1013904223
			img.Pix[i] = byte(seed >> 24)
		}

		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))

		got, err := convertImage("data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()))
		require.NoError(t, err)
		require.LessOrEqual(t, len(got), maxImageBase64)

		raw, err := base64.StdEncoding.DecodeString(got)
		require.NoError(t, err)

		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		require.NoError(t, err)
		require.Less(t, cfg.Width, 1200)
	})

	t.Run("errors", func(t *testing.T) {
		tests := []struct {
			name string
			in   string
		}{
			{"missing comma", "data:image/png;base64"},
			{"not base64 encoded uri", "data:image/png,abc"},
			{"bad base64", "data:image/png;base64,***"},
			{"not an image", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("hello"))},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := convertImage(tt.in)
				require.Error(t, err)
			})
		}
	})
}

func TestClientItem_InputImage(t *testing.T) {
	d := newTestDialect(t, "qwen3.8-omni-flash-realtime", "", false)
	runInit(t, d, "qwen3.8-omni-flash-realtime")

	frame := []byte(`{"event_id":"c1","type":"conversation.item.create","item":{"id":"item_img","type":"message","role":"user","content":[{"type":"input_image","image_url":"` + pngDataURI(t, 16, 16) + `","detail":"auto"}]}}`)

	out, err := d.ClientFrame(frame)
	require.NoError(t, err)

	// Images bind to the audio buffer they are committed with: the image goes first, then
	// (no audio yet) 100 ms of silence at the upstream rate (24 kHz on 3.8), then the commit.
	require.Len(t, out.ToUpstream, 3)
	require.Equal(t, "input_image_buffer.append", dialect.EventType(out.ToUpstream[0]))
	require.Equal(t, "input_audio_buffer.append", dialect.EventType(out.ToUpstream[1]))
	require.Equal(t, "input_audio_buffer.commit", dialect.EventType(out.ToUpstream[2]))

	silence, err := base64.StdEncoding.DecodeString(gjson.GetBytes(out.ToUpstream[1], "audio").String())
	require.NoError(t, err)
	require.Len(t, silence, 4800)
	require.Equal(t, bytes.Repeat([]byte{0}, 4800), silence)

	raw, err := base64.StdEncoding.DecodeString(gjson.GetBytes(out.ToUpstream[0], "image").String())
	require.NoError(t, err)

	_, format, err := image.DecodeConfig(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "jpeg", format)

	// A pure image item is acknowledged locally and cached.
	require.Len(t, out.ToClient, 2)
	require.Equal(t, "conversation.item.added", dialect.EventType(out.ToClient[0]))
	require.Equal(t, "conversation.item.done", dialect.EventType(out.ToClient[1]))
	require.Equal(t, "item_img", gjson.GetBytes(out.ToClient[1], "item.id").String())
	require.Equal(t, "user", gjson.GetBytes(out.ToClient[1], "item.role").String())
	require.Equal(t, "input_image", gjson.GetBytes(out.ToClient[1], "item.content.0.type").String())
	require.False(t, gjson.GetBytes(out.ToClient[1], "item.content.0.image_url").Exists())

	out, err = d.ClientFrame([]byte(`{"type":"conversation.item.retrieve","item_id":"item_img"}`))
	require.NoError(t, err)
	require.Len(t, out.ToClient, 1)
	require.Equal(t, "conversation.item.retrieved", dialect.EventType(out.ToClient[0]))

	// Image plus text: the previous commit emptied the buffer, so silence is padded again,
	// the image is committed, and the text is forwarded as the message afterwards.
	frame = []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + pngDataURI(t, 8, 8) + `"},{"type":"input_text","text":"what is this"}]}}`)

	out, err = d.ClientFrame(frame)
	require.NoError(t, err)
	require.Empty(t, out.ToClient)
	require.Len(t, out.ToUpstream, 4)
	require.Equal(t, "input_image_buffer.append", dialect.EventType(out.ToUpstream[0]))
	require.Equal(t, "input_audio_buffer.append", dialect.EventType(out.ToUpstream[1]))
	require.Equal(t, "input_audio_buffer.commit", dialect.EventType(out.ToUpstream[2]))
	require.Equal(t, "conversation.item.create", dialect.EventType(out.ToUpstream[3]))
	require.Len(t, gjson.GetBytes(out.ToUpstream[3], "item.content").Array(), 1)

	// An empty message is refused rather than silently dropped.
	out, err = d.ClientFrame([]byte(`{"event_id":"c9","type":"conversation.item.create","item":{"type":"message","role":"user","content":[]}}`))
	require.NoError(t, err)
	require.Empty(t, out.ToUpstream)
	require.Len(t, out.ToClient, 1)
	require.Equal(t, "item.content", gjson.GetBytes(out.ToClient[0], "error.param").String())

	// A broken image yields a GA error naming the part.
	out, err = d.ClientFrame([]byte(`{"event_id":"c3","type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}`))
	require.NoError(t, err)
	require.Empty(t, out.ToUpstream)
	require.Len(t, out.ToClient, 1)
	require.Equal(t, "item.content[0].image_url", gjson.GetBytes(out.ToClient[0], "error.param").String())
	require.Equal(t, "c3", gjson.GetBytes(out.ToClient[0], "error.event_id").String())
}

func TestTranscriptTracker(t *testing.T) {
	t.Run("per sentence", func(t *testing.T) {
		tr := &transcriptTracker{}
		require.Equal(t, "", tr.delta(""))
		require.Equal(t, "今天", tr.delta("今天"))
		require.Equal(t, "天气不错，", tr.delta("今天天气不错，"))
		require.Equal(t, "", tr.delta("今天天气不错，"))
		require.Equal(t, "", tr.delta(""))
		require.Equal(t, "阳光", tr.delta("阳光"))
		require.Equal(t, "明媚。", tr.complete("今天天气不错，阳光明媚。"))
	})

	t.Run("text that keeps growing across the boundary is a plain extension", func(t *testing.T) {
		tr := &transcriptTracker{}
		require.Equal(t, "a.", tr.delta("a."))
		require.Equal(t, " b", tr.delta("a. b"))
		require.Equal(t, ".", tr.complete("a. b."))
	})

	t.Run("a shorter text that is not a prefix extension starts a new sentence", func(t *testing.T) {
		tr := &transcriptTracker{}
		require.Equal(t, "hello.", tr.delta("hello."))
		require.Equal(t, "bye", tr.delta("bye"))
		require.Equal(t, ".", tr.complete("hello.bye."))
	})

	t.Run("completed that does not extend emits nothing", func(t *testing.T) {
		tr := &transcriptTracker{}
		require.Equal(t, "hello", tr.delta("hello"))
		require.Equal(t, "", tr.complete("hi"))
		require.Equal(t, "", tr.complete("hello"))
	})
}

func TestItemCache(t *testing.T) {
	c := newItemCache()

	for i := range maxCachedItems + 5 {
		c.put("item_"+strconv.Itoa(i), json.RawMessage(`{"id":"x"}`))
	}

	_, ok := c.get("item_0")
	require.False(t, ok, "oldest entries are evicted")

	_, ok = c.get("item_" + strconv.Itoa(maxCachedItems+4))
	require.True(t, ok)
	require.Len(t, c.order, maxCachedItems)

	c.put("", json.RawMessage(`{}`))
	_, ok = c.get("")
	require.False(t, ok)

	c.put("t", json.RawMessage(`{"id":"t","content":[{"type":"input_audio"}]}`))
	c.setTranscript("t", 0, "hi")
	item, ok := c.get("t")
	require.True(t, ok)
	require.Equal(t, "hi", gjson.GetBytes(item, "content.0.transcript").String())

	c.remove("t")
	_, ok = c.get("t")
	require.False(t, ok)
	c.remove("t")
}

func TestNormalizeItem(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "assistant audio text to transcript and renamed",
			in:   `{"type":"message","role":"assistant","content":[{"type":"audio","text":"hi","audio":"AAAA"},{"type":"text","text":"t"}]}`,
			want: `{"type":"message","role":"assistant","content":[{"type":"output_audio","transcript":"hi"},{"type":"output_text","text":"t"}]}`,
		},
		{
			name: "existing transcript wins",
			in:   `{"type":"message","role":"assistant","content":[{"type":"audio","text":"x","transcript":"y"}]}`,
			want: `{"type":"message","role":"assistant","content":[{"type":"output_audio","text":"x","transcript":"y"}]}`,
		},
		{
			name: "all input content forces user",
			in:   `{"type":"message","role":"assistant","content":[{"type":"input_audio"},{"type":"input_text","text":"a"}]}`,
			want: `{"type":"message","role":"user","content":[{"type":"input_audio"},{"type":"input_text","text":"a"}]}`,
		},
		{
			name: "non message untouched",
			in:   `{"type":"function_call","name":"f","arguments":""}`,
			want: `{"type":"function_call","name":"f","arguments":""}`,
		},
		{
			name: "empty content keeps role",
			in:   `{"type":"message","role":"assistant","content":[]}`,
			want: `{"type":"message","role":"assistant","content":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.JSONEq(t, tt.want, string(normalizeItem([]byte(tt.in))))
		})
	}
}

func TestMapRequireApproval(t *testing.T) {
	tests := []struct {
		name    string
		obj     string
		allowed []string
		known   bool
		want    string
	}{
		{"never covers all allowed", `{"never":{"tool_names":["a","b"]}}`, []string{"a", "b"}, true, "never"},
		{"never misses one", `{"never":{"tool_names":["a"]}}`, []string{"a", "b"}, true, "always"},
		{"never without names", `{"never":{}}`, nil, false, "never"},
		{"always with names", `{"always":{"tool_names":["a"]},"never":{}}`, nil, false, "always"},
		{"always read only", `{"always":{"read_only":true},"never":{}}`, nil, false, "always"},
		{"no never", `{"always":{}}`, nil, false, "always"},
		{"allowed unknown with named never", `{"never":{"tool_names":["a"]}}`, nil, false, "always"},
		// "Only read-only tools skip approval" cannot be expressed upstream: approve everything.
		{"never read only", `{"never":{"read_only":true}}`, []string{"a"}, true, "always"},
		{"never read only with names", `{"never":{"read_only":true,"tool_names":["a"]}}`, []string{"a"}, true, "always"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mapRequireApproval(gjson.Parse(tt.obj), tt.allowed, tt.known))
		})
	}
}

func TestTranslateMCPTool_ReadOnlyAllowedToolsRejected(t *testing.T) {
	tool := gjson.Parse(`{"type":"mcp","server_label":"amap","server_url":"https://mcp.example/sse","allowed_tools":{"read_only":true}}`)

	_, rej := translateMCPTool(tool, 0)
	require.NotNil(t, rej)
	require.Equal(t, "unsupported_on_upstream", rej.code)
	require.Equal(t, "session.tools[0].allowed_tools.read_only", rej.param)

	// With explicit tool names the read_only flag is simply dropped.
	tool = gjson.Parse(`{"type":"mcp","server_label":"amap","server_url":"https://mcp.example/sse","allowed_tools":{"read_only":true,"tool_names":["a"]}}`)

	entry, rej := translateMCPTool(tool, 0)
	require.Nil(t, rej)
	require.Equal(t, []any{"a"}, gjson.GetBytes(entry, "allowed_tools").Value())
}

func TestBase64DecodedLen(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 4, 5, 100, 4799} {
		b := make([]byte, n)
		require.Equal(t, int64(n), base64DecodedLen(base64.StdEncoding.EncodeToString(b)), "n=%d", n)
	}
}

func TestConcurrentFrames(t *testing.T) {
	d := newTestDialect(t, "qwen3.8-omni-flash-realtime", "", true)
	runInit(t, d, "qwen3.8-omni-flash-realtime")

	const rounds = 200

	pcm := audio.Silence(24000, 20*time.Millisecond)
	appendFrame := appendFrame(t, pcm, "")

	var wg sync.WaitGroup

	wg.Add(3)

	go func() {
		defer wg.Done()

		for i := range rounds {
			_, err := d.ClientFrame(appendFrame)
			require.NoError(t, err)

			if i%20 == 0 {
				_, err = d.ClientFrame([]byte(`{"type":"session.update","session":{"instructions":"n` + strconv.Itoa(i) + `"}}`))
				require.NoError(t, err)

				_, err = d.ClientFrame([]byte(`{"type":"conversation.item.retrieve","item_id":"item_a"}`))
				require.NoError(t, err)
			}
		}
	}()

	go func() {
		defer wg.Done()

		for i := range rounds {
			_, err := d.UpstreamFrame([]byte(`{"event_id":"e","type":"response.audio.delta","response_id":"r","item_id":"item_a","output_index":0,"content_index":0,"delta":"AAAA"}`))
			require.NoError(t, err)

			if i%20 == 0 {
				_, err = d.UpstreamFrame([]byte(`{"type":"session.updated","session":{}}`))
				require.NoError(t, err)
			}
		}
	}()

	go func() {
		defer wg.Done()

		for i := range rounds {
			id := "item_" + strconv.Itoa(i)

			_, err := d.UpstreamFrame([]byte(`{"type":"conversation.item.created","item":{"id":"` + id + `","type":"message","status":"in_progress","role":"user","content":[{"type":"input_audio"}]}}`))
			require.NoError(t, err)

			_, err = d.UpstreamFrame([]byte(`{"type":"conversation.item.input_audio_transcription.delta","item_id":"` + id + `","text":"hi","stash":""}`))
			require.NoError(t, err)

			_, err = d.UpstreamFrame([]byte(`{"type":"input_audio_buffer.committed","item_id":"` + id + `"}`))
			require.NoError(t, err)

			_, err = d.UpstreamFrame([]byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"` + id + `","content_index":0,"transcript":"hi there"}`))
			require.NoError(t, err)
		}
	}()

	wg.Wait()
}
