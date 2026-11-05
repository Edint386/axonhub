package bailianasr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	// messageSampleRate is the only sample rate qwen-audio-3.1-asr-flash-message accepts.
	messageSampleRate = 16000
	// messageFrameBytes is one binary frame: 100 ms of 16 kHz mono PCM16 (the doc examples' size).
	messageFrameBytes    = 3200
	messageFrameDuration = 100 * time.Millisecond
	// compressedBytesPerSecond bounds compressed input by size: its duration is unknown without
	// a decoder, so the duration cap assumes 320 kbit/s.
	compressedBytesPerSecond = 40000
)

const (
	wavPCM     = 1
	wavFloat   = 3
	wavALaw    = 6
	wavMuLaw   = 7
	wavExtends = 0xFFFE
)

// messageAudio says how the -message model receives one source: WAV and headerless PCM are
// decoded to 16 kHz mono PCM16 here; mp3, Ogg Opus/Speex, ADTS AAC and AMR-NB go as they are
// with their own format name.
type messageAudio struct {
	// Format is the run-task parameters.format value.
	Format string `json:"format"`
	// WAV is set for RIFF/WAVE input.
	WAV *wavInfo `json:"wav,omitempty"`
	// PCMRate is set for headerless PCM16 mono input (.pcm files).
	PCMRate int `json:"pcm_rate,omitempty"`
	// Duration is the audio length in seconds, 0 when unknown (compressed input).
	Duration float64 `json:"duration,omitempty"`
}

type wavInfo struct {
	Encoding   uint16 `json:"encoding"`
	Channels   int    `json:"channels"`
	SampleRate int    `json:"sample_rate"`
	Bits       int    `json:"bits"`
	DataOffset int64  `json:"data_offset"`
	// DataSize is the length of the sample data in bytes.
	DataSize int64 `json:"data_size"`
}

func (w *wavInfo) blockAlign() int {
	return w.Channels * w.Bits / 8
}

var errNoDataChunk = errors.New("the WAV file has no data chunk")

// readWAVHeader reads a RIFF/WAVE header up to the start of the sample data. size is the whole
// file length, used when the data chunk does not state its own (streamed WAV).
func readWAVHeader(r io.Reader, size int64) (*wavInfo, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil || string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}

	info := &wavInfo{}
	offset := int64(len(riff))
	fmtSeen := false

	for {
		var chunk [8]byte
		if _, err := io.ReadFull(r, chunk[:]); err != nil {
			return nil, errNoDataChunk
		}

		offset += int64(len(chunk))
		id := string(chunk[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(chunk[4:8]))

		switch id {
		case "fmt ":
			if chunkSize < 16 || chunkSize > 4096 {
				return nil, fmt.Errorf("invalid WAV fmt chunk of %d bytes", chunkSize)
			}

			raw := make([]byte, chunkSize+chunkSize%2)
			if _, err := io.ReadFull(r, raw); err != nil {
				return nil, errors.New("truncated WAV fmt chunk")
			}

			offset += int64(len(raw))
			info.Encoding = binary.LittleEndian.Uint16(raw[0:2])
			info.Channels = int(binary.LittleEndian.Uint16(raw[2:4]))
			info.SampleRate = int(binary.LittleEndian.Uint32(raw[4:8]))
			info.Bits = int(binary.LittleEndian.Uint16(raw[14:16]))

			if info.Encoding == wavExtends && chunkSize >= 26 {
				info.Encoding = binary.LittleEndian.Uint16(raw[24:26])
			}

			fmtSeen = true
		case "data":
			if !fmtSeen {
				return nil, errors.New("the WAV data chunk comes before its fmt chunk")
			}

			info.DataOffset = offset
			info.DataSize = chunkSize

			if remaining := size - offset; chunkSize == 0 || chunkSize == math.MaxUint32 || (size > 0 && chunkSize > remaining) {
				info.DataSize = max(remaining, 0)
			}

			return info, nil
		default:
			skip := chunkSize + chunkSize%2
			if _, err := io.CopyN(io.Discard, r, skip); err != nil {
				return nil, errNoDataChunk
			}

			offset += skip
		}
	}
}

// validate accepts PCM (8/16/24/32 bit), IEEE float (32/64 bit) and G.711 WAV with up to 8
// channels.
func (w *wavInfo) validate() error {
	if w.Channels < 1 || w.Channels > 8 {
		return fmt.Errorf("WAV with %d channels is not supported", w.Channels)
	}

	if w.SampleRate < 1000 || w.SampleRate > 384000 {
		return fmt.Errorf("WAV sample rate %d is not supported", w.SampleRate)
	}

	ok := false

	switch w.Encoding {
	case wavPCM:
		ok = w.Bits == 8 || w.Bits == 16 || w.Bits == 24 || w.Bits == 32
	case wavFloat:
		ok = w.Bits == 32 || w.Bits == 64
	case wavALaw, wavMuLaw:
		ok = w.Bits == 8
	}

	if !ok {
		return fmt.Errorf("WAV encoding %d with %d bits per sample is not supported (send PCM, float or G.711 WAV)", w.Encoding, w.Bits)
	}

	return nil
}

func (w *wavInfo) seconds() float64 {
	return float64(w.DataSize) / float64(w.blockAlign()*w.SampleRate)
}

// detectMessageAudio decides how src reaches the -message model and enforces maxDuration.
func detectMessageAudio(src AudioSource, head []byte, extra map[string][]string, maxDuration time.Duration) (messageAudio, error) {
	name := src.Name()
	limit := maxDuration.Seconds()

	if extensionFormat(name) == "pcm" {
		rate, err := pcmSampleRate(extra)
		if err != nil {
			return messageAudio{}, err
		}

		seconds := float64(src.Size()) / float64(2*rate)

		return messageAudio{Format: "pcm", PCMRate: rate, Duration: seconds}, checkMessageDuration(seconds, limit)
	}

	sniffed, _ := sniffAudioFormat(head)

	if sniffed == "wav" {
		r, err := src.Open()
		if err != nil {
			return messageAudio{}, fmt.Errorf("failed to read audio: %w", err)
		}
		defer r.Close()

		info, err := readWAVHeader(r, src.Size())
		if err == nil {
			err = info.validate()
		}

		if err != nil {
			return messageAudio{}, fmt.Errorf("%w: %v", transformer.ErrInvalidRequest, err)
		}

		return messageAudio{Format: "pcm", WAV: info, Duration: info.seconds()}, checkMessageDuration(info.seconds(), limit)
	}

	format := messageCompressedFormat(sniffed, head, name)
	if format == "" {
		return messageAudio{}, fmt.Errorf("%w: unsupported audio format for qwen-audio-3.1-asr-flash-message "+
			"(send wav, pcm, mp3, ogg/opus, ogg/speex, aac or amr-nb)", transformer.ErrInvalidRequest)
	}

	if maxBytes := int64(limit * compressedBytesPerSecond); src.Size() > maxBytes {
		return messageAudio{}, fmt.Errorf("%w: compressed audio for qwen-audio-3.1-asr-flash-message is limited to %d bytes "+
			"(about %s at 320 kbit/s); this file has %d bytes", transformer.ErrInvalidRequest, maxBytes, maxDuration, src.Size())
	}

	return messageAudio{Format: format}, nil
}

// messageCompressedFormat names the compressed containers the -message model decodes itself.
func messageCompressedFormat(sniffed string, head []byte, name string) string {
	switch sniffed {
	case "mp3", "aac", "opus":
		return sniffed
	case "amr":
		if bytes.HasPrefix(head, []byte("#!AMR-WB")) {
			return "" // AMR-NB only
		}

		return "amr"
	case "ogg":
		if bytes.Contains(head, []byte("Speex   ")) {
			return "speex"
		}

		return "" // Ogg Vorbis
	case "":
		switch ext := extensionFormat(name); ext {
		case "mp3", "aac", "amr", "opus":
			return ext
		case "spx":
			return "speex"
		}
	}

	return ""
}

func checkMessageDuration(seconds, limit float64) error {
	if seconds <= limit {
		return nil
	}

	return fmt.Errorf("%w: audio is %s long; qwen-audio-3.1-asr-flash-message accepts at most %s "+
		"(use qwen-audio-3.1-asr-flash-filetrans for long audio)", transformer.ErrInvalidRequest,
		(time.Duration(seconds) * time.Second).String(), (time.Duration(limit) * time.Second).String())
}

// pcmSampleRate reads the optional sample_rate form field for headerless PCM (default 16000).
func pcmSampleRate(extra map[string][]string) (int, error) {
	values := extra["sample_rate"]
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return messageSampleRate, nil
	}

	rate, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || rate < 1000 || rate > 384000 {
		return 0, fmt.Errorf("%w: invalid sample_rate %q for pcm audio", transformer.ErrInvalidRequest, values[0])
	}

	return rate, nil
}

// openMessageAudio returns the bytes the -message task sends: 16 kHz mono PCM16 for WAV and
// PCM input, the file itself for compressed input.
func openMessageAudio(src AudioSource, in messageAudio) (io.ReadCloser, error) {
	r, err := src.Open()
	if err != nil {
		return nil, err
	}

	switch {
	case in.WAV != nil:
		info, err := readWAVHeader(r, src.Size())
		if err != nil {
			r.Close()
			return nil, err
		}

		return newPCMConverter(r, io.LimitReader(r, info.DataSize), info.Encoding, info.Channels, info.Bits, info.SampleRate), nil
	case in.PCMRate > 0 && in.PCMRate != messageSampleRate:
		return newPCMConverter(r, r, wavPCM, 1, 16, in.PCMRate), nil
	default:
		return r, nil
	}
}

// pcmConverter decodes interleaved PCM/float/G.711 samples to mono PCM16 and resamples them to
// 16 kHz while reading, so the audio is never held in memory as a whole.
type pcmConverter struct {
	closer    io.Closer
	src       io.Reader
	encoding  uint16
	channels  int
	bits      int
	frameSize int
	resample  func([]byte) []byte

	buf     []byte
	pending int
	out     []byte
	eof     bool
}

const pcmConverterFrames = 4096

func newPCMConverter(closer io.Closer, src io.Reader, encoding uint16, channels, bits, rate int) *pcmConverter {
	c := &pcmConverter{
		closer:    closer,
		src:       src,
		encoding:  encoding,
		channels:  channels,
		bits:      bits,
		frameSize: channels * bits / 8,
	}
	c.buf = make([]byte, c.frameSize*pcmConverterFrames)

	if rate != messageSampleRate {
		c.resample = newMessageResampler(rate)
	}

	return c
}

// newMessageResampler converts mono PCM16 at rate to 16 kHz. The windowed-sinc resampler of the
// realtime relay takes small rate ratios only (48 kHz, 24 kHz, 8 kHz). Other rates above 16 kHz
// (44.1 kHz and 22.05 kHz) are first decimated by an integer factor with that resampler, whose
// low-pass removes everything above the new Nyquist frequency, and then brought to 16 kHz by
// linear interpolation, which then only upsamples and cannot alias. Plain linear interpolation
// is left for rates neither path takes (11.025 kHz, which is upsampling anyway).
func newMessageResampler(rate int) func([]byte) []byte {
	if r, err := audio.NewResampler(rate, messageSampleRate); err == nil {
		return r.Process
	}

	for factor := (rate + messageSampleRate - 1) / messageSampleRate; factor > 1 && factor <= maxDecimationFactor; factor++ {
		if rate%factor != 0 {
			continue
		}

		decimate, err := audio.NewResampler(rate, rate/factor)
		if err != nil {
			continue
		}

		upsample := newLinearResampler(rate/factor, messageSampleRate)

		return func(pcm []byte) []byte {
			return upsample.process(decimate.Process(pcm))
		}
	}

	return newLinearResampler(rate, messageSampleRate).process
}

// maxDecimationFactor bounds the integer decimation tried before the linear fallback.
const maxDecimationFactor = 16

func (c *pcmConverter) Read(p []byte) (int, error) {
	for len(c.out) == 0 {
		if c.eof {
			return 0, io.EOF
		}

		if err := c.fill(); err != nil {
			return 0, err
		}
	}

	n := copy(p, c.out)
	c.out = c.out[n:]

	return n, nil
}

func (c *pcmConverter) fill() error {
	n, err := io.ReadAtLeast(c.src, c.buf[c.pending:], 1)

	switch {
	case errors.Is(err, io.EOF):
		c.eof = true
	case err != nil:
		return err
	}

	total := c.pending + n
	whole := total - total%c.frameSize
	mono := decodeMono(c.buf[:whole], c.encoding, c.channels, c.bits)
	c.pending = copy(c.buf, c.buf[whole:total])

	if c.resample != nil && len(mono) > 0 {
		mono = c.resample(mono)
	}

	c.out = mono

	return nil
}

func (c *pcmConverter) Close() error {
	return c.closer.Close()
}

var g711Tables = sync.OnceValue(func() [2][256]int16 {
	var tables [2][256]int16

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}

	for t, decode := range []func([]byte) []byte{audio.DecodeALaw, audio.DecodeMuLaw} {
		pcm := decode(all)
		for i := range 256 {
			tables[t][i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
		}
	}

	return tables
})

// decodeMono averages the channels of whole sample frames into mono PCM16 little-endian.
func decodeMono(data []byte, encoding uint16, channels, bits int) []byte {
	width := bits / 8
	frameSize := channels * width
	frames := len(data) / frameSize
	out := make([]byte, frames*2)

	for i := range frames {
		var sum float64

		for ch := range channels {
			sum += sampleValue(data[i*frameSize+ch*width:], encoding, bits)
		}

		binary.LittleEndian.PutUint16(out[2*i:], uint16(clampPCM16(sum/float64(channels))))
	}

	return out
}

// sampleValue returns one sample on the PCM16 scale.
func sampleValue(b []byte, encoding uint16, bits int) float64 {
	switch encoding {
	case wavFloat:
		if bits == 64 {
			return math.Float64frombits(binary.LittleEndian.Uint64(b)) * 32767
		}

		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) * 32767
	case wavALaw:
		return float64(g711Tables()[0][b[0]])
	case wavMuLaw:
		return float64(g711Tables()[1][b[0]])
	}

	switch bits {
	case 8:
		return float64(int(b[0])-128) * 256
	case 16:
		return float64(int16(binary.LittleEndian.Uint16(b)))
	case 24:
		return float64(int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24)>>8) / 256
	default:
		return float64(int32(binary.LittleEndian.Uint32(b))) / 65536
	}
}

func clampPCM16(v float64) int16 {
	switch {
	case v >= math.MaxInt16:
		return math.MaxInt16
	case v <= math.MinInt16:
		return math.MinInt16
	default:
		return int16(math.Round(v))
	}
}

// linearResampler converts mono PCM16 between rates the windowed-sinc resampler of the realtime
// relay does not take (44.1 kHz and its fractions). Output sample k sits at input position
// k*from/to, computed exactly in integers so long files do not drift.
type linearResampler struct {
	from, to int64
	next     int64 // index of the next output sample
	base     int64 // input index of the first sample of the next chunk
	prev     float64
}

func newLinearResampler(from, to int) *linearResampler {
	return &linearResampler{from: int64(from), to: int64(to)}
}

func (l *linearResampler) process(pcm []byte) []byte {
	n := int64(len(pcm) / 2)
	if n == 0 {
		return nil
	}

	sample := func(i int64) float64 {
		if i < l.base {
			return l.prev
		}

		return float64(int16(binary.LittleEndian.Uint16(pcm[2*(i-l.base):])))
	}

	last := l.base + n - 1
	out := make([]byte, 0, int(n*l.to/l.from+2)*2)

	for {
		pos := l.next * l.from
		i0, frac := pos/l.to, pos%l.to

		if i0 > last || (frac != 0 && i0+1 > last) {
			break
		}

		v := sample(i0)
		if frac != 0 {
			v += (sample(i0+1) - v) * float64(frac) / float64(l.to)
		}

		out = binary.LittleEndian.AppendUint16(out, uint16(clampPCM16(v)))
		l.next++
	}

	l.prev = sample(last)
	l.base += n

	return out
}
