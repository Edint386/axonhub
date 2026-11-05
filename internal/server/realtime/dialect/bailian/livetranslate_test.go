package bailian

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// liveTranslateFixture holds the upstream frames of a two-sentence qwen3.8-livetranslate session
// shaped like the live probe of 2026-11-05 (8 s Chinese clip, target en): session.created,
// session.updated, two full response cycles with per-response usage (154 and 269 total tokens),
// session.finished.
func liveTranslateFixture(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "livetranslate_two_sentences.jsonl"))
	require.NoError(t, err)

	var frames []string

	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		frames = append(frames, string(line))
	}

	require.Equal(t, "session.created", gjson.Get(frames[0], "type").String())
	require.Equal(t, "session.updated", gjson.Get(frames[1], "type").String())
	require.Equal(t, "session.finished", gjson.Get(frames[len(frames)-1], "type").String())

	return frames
}

// liveTranslateProbeUpdate is the session.update the live probe found working; only the
// adapter's event_id is added.
const liveTranslateProbeUpdate = `{"event_id":"axh_lt_init","type":"session.update","session":{"output_modalities":["text","audio"],"voice":"Tina","audio":{"input":{"format":{"type":"pcm","sample_rate":16000}}},"translation":{"language":"en"}}}`

const liveTranslateCreated = `{"event_id":"event_lt_01","type":"session.created","session":{"id":"sess_lt_1","object":"realtime.session","model":"qwen3.8-livetranslate-flash-realtime","output_modalities":["text","audio"],"audio":{"output":{"voice":"Chelsie"}},"translation":{"language":"en"}}}`

func newTestLiveTranslate(t *testing.T) *LiveTranslateDialect {
	t.Helper()

	d, err := NewLiveTranslate(LiveTranslateOptions{
		RequestedModel:  "gpt-realtime-translate",
		ActualModel:     "qwen3.8-livetranslate-flash-realtime",
		SessionDuration: time.Hour,
	})
	require.NoError(t, err)

	return d
}

func translateAppend(id string, ms int) string {
	return `{"event_id":"` + id + `","type":"session.input_audio_buffer.append","audio":"` +
		base64.StdEncoding.EncodeToString(make([]byte, clientRate*2*ms/1000)) + `"}`
}

// startLiveTranslate runs session.created and returns the OpenAI session.created.
func startLiveTranslate(t *testing.T, d *LiveTranslateDialect) gjson.Result {
	t.Helper()

	toUpstream, gated := d.Start()
	require.Nil(t, toUpstream)
	require.True(t, gated, "client frames wait for the upstream session.created")

	out := upstream(t, d, liveTranslateCreated)
	require.True(t, out.OpenGate)
	require.Empty(t, out.ToUpstream)

	return single(t, out.ToClient)
}

// sendFirstAudio sends the first append and acknowledges the single upstream update.
func sendFirstAudio(t *testing.T, d *LiveTranslateDialect) gjson.Result {
	t.Helper()

	out := client(t, d, translateAppend("a1", 100))
	require.True(t, out.CloseGate)
	init := single(t, out.ToUpstream)

	ack := upstream(t, d, `{"event_id":"event_lt_02","type":"session.updated","session":{}}`)
	require.True(t, ack.OpenGate)
	require.Len(t, ack.ToUpstream, 1)

	return init
}

func TestSupportedLiveTranslate(t *testing.T) {
	for _, model := range []string{
		"qwen3.8-livetranslate-flash-realtime",
		"qwen3.8-livetranslate-flash-realtime-2026-11-01",
		" Qwen3.8-LiveTranslate-Flash-Realtime ",
	} {
		require.True(t, SupportedLiveTranslate(model), model)
		require.False(t, Supported(model), "%s must not be admitted as an omni model", model)
		require.False(t, SupportedBailianTranscription(model), model)
	}

	// qwen3.5 / qwen3 livetranslate speak an older wire format the dialect does not implement.
	for _, model := range []string{
		"qwen3.5-livetranslate-flash-realtime", "qwen3-livetranslate-flash-realtime",
		"qwen3-livetranslate-flash", "qwen3.8-omni-flash-realtime", "qwen3-asr-flash-realtime", "gpt-realtime-translate",
	} {
		require.False(t, SupportedLiveTranslate(model), model)
	}

	_, err := NewLiveTranslate(LiveTranslateOptions{ActualModel: "qwen3.8-omni-flash-realtime"})
	require.Error(t, err)
}

func TestTranslationLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"es": "es", "EN-us": "en", "pt_BR": "pt", "zh-CN": "zh", "zh-Hans": "zh", "zh-TW": "zh", "cmn": "zh",
		"zh-HK": "yue", "yue": "yue", "no": "nb", "nb-NO": "nb", "tl": "fil", "fil": "fil", "iw": "he", "in": "id", "uk": "uk",
	} {
		got, ok := translationLanguage(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in)
	}

	for _, in := range []string{"", "xx", "eo", "la-VA", "english", "sr-Latn-RS"} {
		_, ok := translationLanguage(in)
		require.False(t, ok, in)
	}
}

// Every spelling of one locale picks the same target, and so the same output modalities: the
// script subtag never decides (zh-Hant-HK is zh-HK, Cantonese, text only).
func TestTranslationLanguage_EverySpellingOfALocaleAgrees(t *testing.T) {
	for want, spellings := range map[string][]string{
		"yue": {"zh-HK", "zh-Hant-HK", "ZH_hant_hk", "zh-MO", "zh-Hant-MO", "zh-yue", "zh-yue-HK", "yue-HK", "yue-Hant-HK"},
		"zh":  {"zh-TW", "zh-Hant-TW", "zh-Hant", "zh-Hans", "zh-Hans-CN", "zh-CN", "zh-Hans-SG", "cmn-Hans-CN"},
		"pt":  {"pt-BR", "pt-Latn-BR"},
		"sv":  {"sv", "sv-Latn-SE"},
	} {
		for _, in := range spellings {
			got, ok := translationLanguage(in)
			require.True(t, ok, in)
			require.Equal(t, want, got, in)
		}
	}

	modalities := func(language string) string {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		client(t, d, `{"type":"session.update","session":{"audio":{"output":{"language":"`+language+`"}}}}`)

		return sendFirstAudio(t, d).Get("session.output_modalities").Raw
	}

	require.Equal(t, `["text"]`, modalities("zh-HK"))
	require.Equal(t, modalities("zh-HK"), modalities("zh-Hant-HK"))
}

func TestLiveTranslate_TwoSentenceSession(t *testing.T) {
	frames := liveTranslateFixture(t)
	d := newTestLiveTranslate(t)

	toUpstream, gated := d.Start()
	require.Nil(t, toUpstream)
	require.True(t, gated)

	// The upstream session.created (default voice Chelsie) becomes the OpenAI session.created.
	out := upstream(t, d, frames[0])
	require.True(t, out.OpenGate)

	created := single(t, out.ToClient)
	require.Equal(t, "session.created", created.Get("type").String())
	require.Equal(t, "sess_lt_1", created.Get("session.id").String())
	require.Equal(t, "translation", created.Get("session.type").String())
	require.Equal(t, "gpt-realtime-translate", created.Get("session.model").String(), "the requested model is echoed")
	require.Equal(t, "en", created.Get("session.audio.output.language").String())
	require.Equal(t, gjson.Null, created.Get("session.audio.input.transcription").Type)
	require.Equal(t, gjson.Null, created.Get("session.audio.input.noise_reduction").Type)
	require.Greater(t, created.Get("session.expires_at").Int(), time.Now().Unix())

	// Client updates are answered locally; nothing reaches the upstream, which allows one update.
	out = client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"input":{"transcription":{"model":"gpt-realtime-whisper"},"noise_reduction":{"type":"near_field"}},"output":{"language":"en-US"}}}}`)
	require.Empty(t, out.ToUpstream)

	updated := single(t, out.ToClient)
	require.Equal(t, "session.updated", updated.Get("type").String())
	require.Equal(t, "en-US", updated.Get("session.audio.output.language").String(), "the client's spelling is echoed")
	require.Equal(t, "gpt-realtime-whisper", updated.Get("session.audio.input.transcription.model").String())
	require.Equal(t, "near_field", updated.Get("session.audio.input.noise_reduction.type").String())

	// The first audio sends exactly the update the live probe found working and holds the audio.
	out = client(t, d, translateAppend("a1", 100))
	require.True(t, out.CloseGate)
	require.Len(t, out.ToUpstream, 1)
	require.JSONEq(t, liveTranslateProbeUpdate, string(out.ToUpstream[0]))

	// The acknowledgement releases the held audio: 100 ms resampled to 16 kHz, client event id kept.
	out = upstream(t, d, frames[1])
	require.True(t, out.OpenGate)
	require.Empty(t, out.ToClient)

	held := single(t, out.ToUpstream)
	require.Equal(t, "input_audio_buffer.append", held.Get("type").String())
	require.Equal(t, "a1", held.Get("event_id").String())

	pcm, err := base64.StdEncoding.DecodeString(held.Get("audio").String())
	require.NoError(t, err)
	require.InDelta(t, 3200, len(pcm), 64)

	// Later audio goes straight upstream, split into 100 ms chunks.
	out = client(t, d, translateAppend("a2", 250))
	require.Len(t, out.ToUpstream, 3)
	require.Equal(t, "a2", gjson.GetBytes(out.ToUpstream[0], "event_id").String())
	require.False(t, gjson.GetBytes(out.ToUpstream[1], "event_id").Exists())

	var (
		input, output strings.Builder
		audioDeltas   []gjson.Result
		usage         []dialect.UsageRecord
	)

	for _, frame := range frames[2 : len(frames)-1] {
		out := upstream(t, d, frame)
		require.Nil(t, out.ResponseInProgress, "translation responses are never cancelled at teardown")
		require.Nil(t, out.Close)
		require.Empty(t, out.ToUpstream)

		usage = append(usage, out.Usage...)

		for _, raw := range out.ToClient {
			ev := gjson.ParseBytes(raw)
			require.NotEmpty(t, ev.Get("event_id").String())
			require.True(t, ev.Get("elapsed_ms").Exists())

			switch ev.Get("type").String() {
			case "session.input_transcript.delta":
				input.WriteString(ev.Get("delta").String())
			case "session.output_transcript.delta":
				output.WriteString(ev.Get("delta").String())
			case "session.output_audio.delta":
				audioDeltas = append(audioDeltas, ev)
			default:
				t.Fatalf("unexpected client event %s", ev.Raw)
			}
		}
	}

	require.Equal(t, "Today, the weather is nice. Let's go for a walk in the park. Tomorrow at 3 p.m. we have a meeting, remember to bring your computer. ", output.String())
	require.Equal(t, "今天 天气不错 我们去公园散步 明天 下午三点开会 记得带电脑 ", input.String())

	require.Len(t, audioDeltas, 4)
	require.Equal(t, "AAABAAIAAwA=", audioDeltas[0].Get("delta").String(), "24 kHz speech passes through")
	require.EqualValues(t, 24000, audioDeltas[0].Get("sample_rate").Int())
	require.Equal(t, "pcm16", audioDeltas[0].Get("format").String())
	require.EqualValues(t, 1, audioDeltas[0].Get("channels").Int())
	require.EqualValues(t, 200, audioDeltas[0].Get("elapsed_ms").Int(), "350 ms of client audio, in 200 ms steps")

	// One usage record per response.done; the probe showed per-response (not cumulative) usage.
	require.Len(t, usage, 2)

	for i, want := range []struct {
		id                    string
		total, input, output  int64
		inputAudio, outputAud int64
	}{
		{"resp_lt_1", 154, 75, 79, 75, 62},
		{"resp_lt_2", 269, 110, 159, 110, 139},
	} {
		rec := usage[i]
		require.Equal(t, "response", rec.Kind)
		require.Equal(t, want.id, rec.ResponseID)
		require.Empty(t, rec.Model, "billed under the session's actual model")
		require.EqualValues(t, want.total, rec.Usage.TotalTokens)
		require.EqualValues(t, want.input, rec.Usage.PromptTokens)
		require.EqualValues(t, want.output, rec.Usage.CompletionTokens)
		require.EqualValues(t, want.inputAudio, rec.Usage.PromptTokensDetails.AudioTokens)
		require.EqualValues(t, want.outputAud, rec.Usage.CompletionTokensDetails.AudioTokens)
		require.Equal(t, "translation", gjson.GetBytes(rec.Summary, "kind").String())
		require.Equal(t, "en", gjson.GetBytes(rec.Summary, "language").String())
	}

	require.Contains(t, gjson.GetBytes(usage[1].Summary, "transcript").String(), "remember to bring your computer")

	// session.close ends the session with 1000; teardown sends one session.finish and waits for
	// session.finished, which becomes session.closed.
	out = client(t, d, `{"event_id":"c1","type":"session.close"}`)
	require.Equal(t, &dialect.CloseRequest{Code: 1000, Reason: "session closed by client"}, out.Close)
	require.Empty(t, out.ToClient)
	require.Empty(t, out.ToUpstream)

	goodbye := d.CloseUpstream()
	require.Len(t, goodbye, 1)
	require.Equal(t, "session.finish", gjson.GetBytes(goodbye[0], "type").String())
	require.True(t, d.FinishDrain())
	require.Equal(t, 15*time.Second, d.FinishDrainTimeout())

	out = upstream(t, d, frames[len(frames)-1])
	require.True(t, out.UpstreamFinished)
	require.Nil(t, out.Close, "the session is already closing")
	require.Equal(t, "session.closed", single(t, out.ToClient).Get("type").String())

	// Audio after the close is refused.
	out = client(t, d, translateAppend("late", 20))
	require.Empty(t, out.ToUpstream)
	require.Equal(t, "session_closing", single(t, out.ToClient).Get("error.code").String())
}

func TestLiveTranslate_SessionUpdateValidation(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)

	for _, tc := range []struct {
		update, param string
	}{
		{`{"event_id":"bad1","type":"session.update","session":{"audio":{"output":{"language":"eo"}}}}`, "session.audio.output.language"},
		{`{"event_id":"bad2","type":"session.update","session":{"audio":{"output":{"language":3}}}}`, "session.audio.output.language"},
		{`{"event_id":"bad3","type":"session.update","session":{"type":"realtime"}}`, "session.type"},
		{`{"event_id":"bad4","type":"session.update","session":{"model":"gpt-realtime"}}`, "session.model"},
		{`{"event_id":"bad5","type":"session.update","session":{"audio":{"input":{"transcription":"on"}}}}`, "session.audio.input.transcription"},
	} {
		out := client(t, d, tc.update)
		require.Empty(t, out.ToUpstream)
		require.Nil(t, out.Close, "a rejected update keeps the session open")

		ev := single(t, out.ToClient)
		require.Equal(t, "error", ev.Get("type").String())
		require.Equal(t, "invalid_request_error", ev.Get("error.type").String())
		require.Equal(t, "invalid_value", ev.Get("error.code").String())
		require.Equal(t, tc.param, ev.Get("error.param").String())
		require.Equal(t, gjson.Get(tc.update, "event_id").String(), ev.Get("error.event_id").String())
	}

	// Nothing of the rejected updates was applied; the model itself may be repeated.
	out := client(t, d, `{"type":"session.update","session":{"type":"translation","model":"gpt-realtime-translate","audio":{"output":{"language":"de"}}}}`)
	updated := single(t, out.ToClient)
	require.Equal(t, "de", updated.Get("session.audio.output.language").String())
	require.Equal(t, gjson.Null, updated.Get("session.audio.input.transcription").Type)
}

func TestLiveTranslate_LanguageIsFixedOnceAudioStarted(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)

	client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"output":{"language":"ja"}}}}`)
	init := sendFirstAudio(t, d)
	require.Equal(t, "ja", init.Get("session.translation.language").String())

	out := client(t, d, `{"event_id":"u2","type":"session.update","session":{"audio":{"output":{"language":"fr"}}}}`)
	require.Empty(t, out.ToUpstream, "the upstream closes on a second session.update")

	ev := single(t, out.ToClient)
	require.Equal(t, "unsupported_on_upstream", ev.Get("error.code").String())
	require.Equal(t, "u2", ev.Get("error.event_id").String())

	// The same language in another spelling and the local-only settings still apply.
	out = client(t, d, `{"event_id":"u3","type":"session.update","session":{"audio":{"output":{"language":"ja-JP"},"input":{"transcription":{"model":"gpt-realtime-whisper"}}}}}`)
	require.Empty(t, out.ToUpstream)

	updated := single(t, out.ToClient)
	require.Equal(t, "session.updated", updated.Get("type").String())
	require.Equal(t, "ja-JP", updated.Get("session.audio.output.language").String())
}

func TestLiveTranslate_TextOnlyLanguage(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)

	client(t, d, `{"type":"session.update","session":{"audio":{"output":{"language":"uk"}}}}`)
	init := sendFirstAudio(t, d)
	require.Equal(t, "uk", init.Get("session.translation.language").String())
	require.Equal(t, `["text"]`, init.Get("session.output_modalities").Raw, "Ukrainian is a text-only target")

	out := upstream(t, d, `{"type":"response.text.delta","response_id":"r1","item_id":"i1","delta":"Привіт"}`)
	require.Equal(t, "Привіт", single(t, out.ToClient).Get("delta").String())

	out = upstream(t, d, `{"type":"response.text.done","response_id":"r1","item_id":"i1","text":"Привіт."}`)
	require.Equal(t, ".", single(t, out.ToClient).Get("delta").String())
}

func TestLiveTranslate_InputTranscriptOnlyWhenRequested(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)
	sendFirstAudio(t, d)

	delta := `{"type":"conversation.item.input_audio_transcription.delta","item_id":"s1","content_index":0,"delta":"今天 "}`
	require.Empty(t, upstream(t, d, delta).ToClient, "transcription is null by default")

	client(t, d, `{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"gpt-realtime-whisper"}}}}}`)
	require.Equal(t, "session.input_transcript.delta", single(t, upstream(t, d, delta).ToClient).Get("type").String())

	failed := single(t, upstream(t, d, `{"type":"conversation.item.input_audio_transcription.failed","item_id":"s1","error":{"code":"asr_failed","message":"boom"}}`).ToClient)
	require.Equal(t, "server_error", failed.Get("error.type").String())
	require.Equal(t, "asr_failed", failed.Get("error.code").String())
}

func TestLiveTranslate_UpstreamErrors(t *testing.T) {
	t.Run("a rejected configuration points at the client's update and closes", func(t *testing.T) {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		client(t, d, `{"event_id":"u1","type":"session.update","session":{"audio":{"output":{"language":"es"}}}}`)

		out := client(t, d, translateAppend("a1", 100))
		require.True(t, out.CloseGate)

		out = upstream(t, d, `{"event_id":"e1","type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Session update error: session already started or finished or failed.","event_id":"axh_lt_init"}}`)
		require.Equal(t, 1011, out.Close.Code)
		require.Empty(t, out.ToUpstream, "the held audio is not released")

		ev := single(t, out.ToClient)
		require.Equal(t, "server_error", ev.Get("error.type").String())
		require.Equal(t, "upstream_config_rejected", ev.Get("error.code").String())
		require.Equal(t, "u1", ev.Get("error.event_id").String())
		require.Contains(t, ev.Get("error.message").String(), "session already started")

		require.Nil(t, d.CloseUpstream(), "no goodbye to a failed upstream")
		require.False(t, d.FinishDrain())
	})

	t.Run("a response failure is relayed and closes", func(t *testing.T) {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		sendFirstAudio(t, d)

		out := upstream(t, d, `{"event_id":"e2","type":"error","error":{"code":"COMMON_ERROR","message":"<400> InternalError.Algo.InvalidParameter: Voice 'Chelsie' is not supported."}}`)
		require.Equal(t, 1011, out.Close.Code)

		ev := single(t, out.ToClient)
		require.Equal(t, "server_error", ev.Get("error.type").String())
		require.Equal(t, "COMMON_ERROR", ev.Get("error.code").String())
		require.Contains(t, ev.Get("error.message").String(), "Chelsie")
	})

	t.Run("a failed response is billed and reported", func(t *testing.T) {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		sendFirstAudio(t, d)

		out := upstream(t, d, `{"type":"response.done","response":{"id":"r9","status":"failed","usage":{"total_tokens":10,"input_tokens":10,"output_tokens":0}}}`)
		require.Len(t, out.Usage, 1)
		require.Nil(t, out.Close)
		require.Equal(t, "response_failed", single(t, out.ToClient).Get("error.code").String())
	})
}

func TestLiveTranslate_ClientEvents(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)

	for _, tc := range []struct{ frame, code, param string }{
		{`{"event_id":"x1","type":"session.input_audio_buffer.append","audio":"!!"}`, "invalid_value", "audio"},
		{`{"event_id":"x2","type":"session.input_audio_buffer.append","audio":""}`, "invalid_value", "audio"},
		{`{"event_id":"x3","type":"input_audio_buffer.append","audio":"AAAA"}`, "invalid_value", "type"},
		{`{"event_id":"x4","type":"response.create"}`, "invalid_value", "type"},
	} {
		out := client(t, d, tc.frame)
		require.Empty(t, out.ToUpstream, tc.frame)

		ev := single(t, out.ToClient)
		require.Equal(t, tc.code, ev.Get("error.code").String(), tc.frame)
		require.Equal(t, tc.param, ev.Get("error.param").String(), tc.frame)
	}

	// Closing before any audio: nothing to flush upstream, session.closed at once.
	out := client(t, d, `{"type":"session.close"}`)
	require.Equal(t, 1000, out.Close.Code)
	require.Equal(t, "session.closed", single(t, out.ToClient).Get("type").String())
	require.Nil(t, d.CloseUpstream())
	require.False(t, d.FinishDrain())

	require.Empty(t, client(t, d, `{"type":"session.close"}`).ToClient, "a second close is ignored")
}

func TestLiveTranslate_UpstreamFinishWithoutClientClose(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)
	sendFirstAudio(t, d)

	out := upstream(t, d, `{"type":"session.finished"}`)
	require.True(t, out.UpstreamFinished)
	require.Equal(t, 1000, out.Close.Code)
	require.Equal(t, "session.closed", single(t, out.ToClient).Get("type").String())

	require.Empty(t, upstream(t, d, `{"type":"session.finished"}`).ToClient, "session.closed is sent once")
}

// A client session.close is always answered with session.closed, and a goodbye the upstream did
// not complete is reported as failed rather than as a completed session.
func TestLiveTranslate_ClientGoodbyeTheUpstreamDoesNotComplete(t *testing.T) {
	closing := func(t *testing.T) *LiveTranslateDialect {
		t.Helper()

		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		sendFirstAudio(t, d)

		out := client(t, d, `{"type":"session.close"}`)
		require.Equal(t, 1000, out.Close.Code)
		require.Empty(t, out.ToClient, "answered by the upstream's session.finished")
		require.Len(t, d.CloseUpstream(), 1)
		require.True(t, d.FinishDrain())

		return d
	}

	t.Run("an upstream error during the flush", func(t *testing.T) {
		d := closing(t)

		out := upstream(t, d, `{"type":"error","error":{"code":"COMMON_ERROR","message":"boom"}}`)
		require.Equal(t, "error", single(t, out.ToClient).Get("type").String())

		require.Equal(t, "session.closed", single(t, d.PendingGoodbye()).Get("type").String())
		require.Empty(t, d.PendingGoodbye(), "session.closed is sent once")
		require.ErrorContains(t, d.GoodbyeFailed(), "upstream error")
	})

	t.Run("no session.finished: the upstream closed or the drain timed out", func(t *testing.T) {
		d := closing(t)

		require.Equal(t, "session.closed", single(t, d.PendingGoodbye()).Get("type").String())
		require.ErrorContains(t, d.GoodbyeFailed(), "did not finish")

		// A session.finished that still arrives answers nothing twice.
		require.Empty(t, upstream(t, d, `{"type":"session.finished"}`).ToClient)
	})

	t.Run("an acknowledged goodbye completes", func(t *testing.T) {
		d := closing(t)

		require.Equal(t, "session.closed", single(t, upstream(t, d, `{"type":"session.finished"}`).ToClient).Get("type").String())
		require.Empty(t, d.PendingGoodbye())
		require.NoError(t, d.GoodbyeFailed())
	})

	t.Run("a goodbye without audio completes at once", func(t *testing.T) {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)

		require.Equal(t, "session.closed", single(t, client(t, d, `{"type":"session.close"}`).ToClient).Get("type").String())
		require.Nil(t, d.CloseUpstream())
		require.Empty(t, d.PendingGoodbye())
		require.NoError(t, d.GoodbyeFailed())
	})

	t.Run("an upstream failure without a client goodbye is no goodbye", func(t *testing.T) {
		d := newTestLiveTranslate(t)
		startLiveTranslate(t, d)
		sendFirstAudio(t, d)

		require.Equal(t, 1011, upstream(t, d, `{"type":"error","error":{"code":"COMMON_ERROR","message":"boom"}}`).Close.Code)
		require.Empty(t, d.PendingGoodbye())
		require.NoError(t, d.GoodbyeFailed())
	})
}

// While the single update is unacknowledged no audio reached the upstream: there is nothing to
// flush, so no session.finish and no finish drain against an upstream that already stalled.
func TestLiveTranslate_NoGoodbyeWhileTheUpdateIsUnacknowledged(t *testing.T) {
	d := newTestLiveTranslate(t)
	startLiveTranslate(t, d)

	out := client(t, d, translateAppend("a1", 100))
	require.True(t, out.CloseGate)

	require.Nil(t, d.CloseUpstream())
	require.False(t, d.FinishDrain())

	// An acknowledgement arriving between teardown's CloseUpstream and FinishDrain changes
	// nothing: no session.finish was sent, so none is waited for.
	_ = upstream(t, d, `{"type":"session.updated","session":{}}`)
	require.False(t, d.FinishDrain())
}

func TestTranscriptStitcher(t *testing.T) {
	t.Run("spaced scripts get one separator between segments", func(t *testing.T) {
		var s transcriptStitcher

		require.Equal(t, "Hello", s.delta("a", "Hello"))
		require.Equal(t, ".", s.delta("a", "."))
		require.Equal(t, "", s.done("a", "Hello."))
		require.Equal(t, " Bye", s.delta("b", "Bye"))
		require.Equal(t, " ¿Qué", s.delta("c", "¿Qué"), "opening punctuation starts a new sentence")
		require.Equal(t, ",", s.delta("d", ","), "closing punctuation never gets a space")
	})

	t.Run("unspaced scripts are joined directly", func(t *testing.T) {
		var s transcriptStitcher

		require.Equal(t, "今天天气不错。", s.delta("a", "今天天气不错。"))
		require.Equal(t, "明天开会", s.delta("b", "明天开会"))
		require.Equal(t, "Zoom", s.delta("c", "Zoom"), "no space after Chinese text")
	})

	t.Run("final text fills in what the deltas left out", func(t *testing.T) {
		var s transcriptStitcher

		require.Equal(t, "Hi", s.delta("a", "Hi"))
		require.Equal(t, " there. ", s.done("a", "Hi there. "))
		require.Equal(t, "Next", s.delta("b", "Next"), "the final text's trailing space already separates")
		require.Equal(t, "", s.done("b", "Different"), "a diverged final text cannot be appended")
		require.Equal(t, " Whole.", s.done("c", "Whole."), "a segment without deltas is emitted whole")
		require.Equal(t, "", s.done("d", ""))
	})

	t.Run("an older segment's final text is not appended after a newer one", func(t *testing.T) {
		var s transcriptStitcher

		s.delta("a", "One")
		s.delta("b", "Two")
		require.Equal(t, "", s.done("a", "One more"))
		require.Equal(t, " three", s.done("b", "Two three"))
	})

	t.Run("segments without ids are split by their final text", func(t *testing.T) {
		var s transcriptStitcher

		s.delta("", "First.")
		require.Equal(t, "", s.done("", "First."))
		require.Equal(t, " Second", s.delta("", "Second"))
	})
}
