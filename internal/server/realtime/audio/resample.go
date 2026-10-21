// Package audio holds the small PCM helpers used by the realtime relay dialects: a stateful
// sample-rate converter for 16-bit little-endian mono PCM, chunking and silence generation.
//
// Everything in this package is custom (fork) code.
package audio

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const (
	// bytesPerSample is the width of one 16-bit PCM sample.
	bytesPerSample = 2

	// maxRatioTerm bounds the reduced up/down factors so that the polyphase filter stays small.
	maxRatioTerm = 16

	// halfTapsPerPhase is the number of input samples the low-pass looks at on each side of
	// the output position. 32 gives a transition band of roughly 2 kHz at a 48 kHz
	// intermediate rate, which is plenty for speech.
	halfTapsPerPhase = 32
)

// Resampler converts 16-bit little-endian mono PCM between sample rates.
//
// It is a polyphase windowed-sinc converter: the input is conceptually upsampled by L,
// low-passed at the lower of the two Nyquist frequencies with a Blackman-windowed sinc, and
// decimated by M, where L/M is the reduced rate ratio. It is stateful: a trailing odd byte,
// the filter history and the fractional phase are carried across Process calls so arbitrary
// chunk boundaries produce exactly the same bytes as a single whole-buffer call.
type Resampler struct {
	from int
	to   int
	up   int // L.
	down int // M.

	// phases[p][i] multiplies the input sample base-i for output phase p.
	phases [][]float64
	// histLen is the number of past input samples every output needs (max taps per phase - 1).
	histLen int

	buf      []int16 // Input samples still needed; buf[0] has global index bufStart.
	bufStart int64
	outIdx   int64  // Global index of the next output sample.
	carry    []byte // Trailing odd byte from the previous Process call.
}

// NewResampler returns a converter from fromRate to toRate. The pair must be distinct
// positive rates whose reduced ratio has both terms at most 16 (24000→16000 is 2/3).
func NewResampler(fromRate, toRate int) (*Resampler, error) {
	if fromRate <= 0 || toRate <= 0 {
		return nil, fmt.Errorf("audio: invalid sample rates %d→%d", fromRate, toRate)
	}

	if fromRate == toRate {
		return nil, fmt.Errorf("audio: resampling %d→%d is an identity; no resampler is needed", fromRate, toRate)
	}

	g := gcd(fromRate, toRate)
	up := toRate / g
	down := fromRate / g

	if up > maxRatioTerm || down > maxRatioTerm {
		return nil, fmt.Errorf("audio: unsupported sample rate pair %d→%d (ratio %d/%d)", fromRate, toRate, up, down)
	}

	r := &Resampler{
		from: fromRate,
		to:   toRate,
		up:   up,
		down: down,
	}
	r.buildFilter()

	return r, nil
}

// buildFilter computes the polyphase branches of the anti-aliasing low-pass.
func (r *Resampler) buildFilter() {
	// Cut-off at the lower Nyquist frequency, normalised to the upsampled rate from*L.
	fc := float64(min(r.from, r.to)) / 2 / float64(r.from*r.up)
	n := 2*r.up*halfTapsPerPhase + 1
	center := float64(n-1) / 2

	taps := make([]float64, n)
	for i := range taps {
		x := float64(i) - center
		// Windowed sinc with gain L to compensate for the zero insertion.
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}

		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) + 0.08*math.Cos(4*math.Pi*float64(i)/float64(n-1))
		taps[i] = s * w * float64(r.up)
	}

	r.phases = make([][]float64, r.up)
	for p := range r.up {
		var branch []float64

		var sum float64

		for i := p; i < n; i += r.up {
			branch = append(branch, taps[i])
			sum += taps[i]
		}

		// Normalise each branch to unity DC gain so low frequencies pass unchanged.
		if sum != 0 {
			for i := range branch {
				branch[i] /= sum
			}
		}

		r.phases[p] = branch

		if len(branch)-1 > r.histLen {
			r.histLen = len(branch) - 1
		}
	}
}

// Process converts the next chunk of input PCM and returns the output PCM produced so far.
// Input chunks may have any length, including odd byte counts; output is always whole
// samples. The returned slice is owned by the caller.
func (r *Resampler) Process(pcm []byte) []byte {
	data := pcm
	if len(r.carry) > 0 {
		data = make([]byte, 0, len(r.carry)+len(pcm))
		data = append(data, r.carry...)
		data = append(data, pcm...)
		r.carry = nil
	}

	if len(data)%bytesPerSample != 0 {
		r.carry = []byte{data[len(data)-1]}
		data = data[:len(data)-1]
	}

	for i := 0; i+bytesPerSample <= len(data); i += bytesPerSample {
		r.buf = append(r.buf, int16(binary.LittleEndian.Uint16(data[i:]))) //nolint:gosec // G115: two's-complement PCM sample reinterpretation.
	}

	var out []byte

	for {
		pos := r.outIdx * int64(r.down)
		base := pos / int64(r.up)
		phase := int(pos % int64(r.up))

		last := r.bufStart + int64(len(r.buf)) - 1
		if base > last {
			break
		}

		branch := r.phases[phase]

		var acc float64

		for i, c := range branch {
			idx := base - int64(i)
			if idx < 0 {
				break
			}

			if idx < r.bufStart {
				// Older than the retained history; treated as silence. This only happens for
				// the first output samples after a Reset, where the history is genuinely empty.
				break
			}

			acc += c * float64(r.buf[idx-r.bufStart])
		}

		out = binary.LittleEndian.AppendUint16(out, uint16(clampSample(acc))) //nolint:gosec // G115: two's-complement PCM sample reinterpretation.
		r.outIdx++
	}

	// Drop the input samples no future output can reference.
	nextBase := (r.outIdx * int64(r.down)) / int64(r.up)

	keepFrom := nextBase - int64(r.histLen)
	if keepFrom > r.bufStart {
		drop := keepFrom - r.bufStart
		if drop > int64(len(r.buf)) {
			drop = int64(len(r.buf))
		}

		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.bufStart += drop
	}

	return out
}

// Reset discards all carried state so the next Process call starts a fresh stream.
func (r *Resampler) Reset() {
	r.buf = r.buf[:0]
	r.bufStart = 0
	r.outIdx = 0
	r.carry = nil
}

// ProcessedOutput returns the number of output samples produced since the last Reset.
func (r *Resampler) ProcessedOutput() int64 {
	return r.outIdx
}

// FromRate returns the input sample rate.
func (r *Resampler) FromRate() int {
	return r.from
}

// ToRate returns the output sample rate.
func (r *Resampler) ToRate() int {
	return r.to
}

// ResampleBase64 decodes base64 PCM, runs it through r and re-encodes the result. A nil
// resampler is an identity.
func ResampleBase64(r *Resampler, b64 string) (string, error) {
	if r == nil {
		return b64, nil
	}

	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("audio: decode base64 pcm: %w", err)
	}

	return base64.StdEncoding.EncodeToString(r.Process(pcm)), nil
}

// SplitPCM splits 16-bit mono PCM into chunks of at most maxChunk duration at sampleRate.
// Chunk boundaries fall on sample boundaries; the slices alias pcm. A non-positive maxChunk
// or sampleRate returns the whole buffer as one chunk, and an empty buffer returns nil.
func SplitPCM(pcm []byte, sampleRate int, maxChunk time.Duration) [][]byte {
	if len(pcm) == 0 {
		return nil
	}

	chunkBytes := bytesForDuration(sampleRate, maxChunk)
	if chunkBytes <= 0 || chunkBytes >= len(pcm) {
		return [][]byte{pcm}
	}

	chunks := make([][]byte, 0, (len(pcm)+chunkBytes-1)/chunkBytes)
	for off := 0; off < len(pcm); off += chunkBytes {
		end := min(off+chunkBytes, len(pcm))
		chunks = append(chunks, pcm[off:end])
	}

	return chunks
}

// Silence returns d of digital silence as 16-bit mono PCM at sampleRate.
func Silence(sampleRate int, d time.Duration) []byte {
	n := bytesForDuration(sampleRate, d)
	if n <= 0 {
		return nil
	}

	return make([]byte, n)
}

// ByteLenToSeconds converts a 16-bit mono PCM byte length at sampleRate to seconds.
func ByteLenToSeconds(n int, sampleRate int) float64 {
	if n <= 0 || sampleRate <= 0 {
		return 0
	}

	return float64(n) / float64(bytesPerSample*sampleRate)
}

// bytesForDuration returns the even byte count covering d at sampleRate.
func bytesForDuration(sampleRate int, d time.Duration) int {
	if sampleRate <= 0 || d <= 0 {
		return 0
	}

	samples := int64(sampleRate) * int64(d) / int64(time.Second)

	return int(samples) * bytesPerSample
}

// clampSample rounds a filtered value to the nearest int16.
func clampSample(v float64) int16 {
	v = math.Round(v)
	if v > math.MaxInt16 {
		return math.MaxInt16
	}

	if v < math.MinInt16 {
		return math.MinInt16
	}

	return int16(v)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}

	return a
}
