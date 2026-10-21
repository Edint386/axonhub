package audio

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sine returns d of a sine tone as 16-bit mono PCM.
func sine(freq float64, amplitude float64, sampleRate int, d time.Duration) []byte {
	n := int(int64(sampleRate) * int64(d) / int64(time.Second))
	out := make([]byte, 0, n*bytesPerSample)

	for i := range n {
		v := amplitude * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate))
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(math.Round(v))))
	}

	return out
}

func samples(pcm []byte) []int16 {
	out := make([]int16, 0, len(pcm)/bytesPerSample)
	for i := 0; i+bytesPerSample <= len(pcm); i += bytesPerSample {
		out = append(out, int16(binary.LittleEndian.Uint16(pcm[i:])))
	}

	return out
}

func zeroCrossings(s []int16) int {
	var n int

	for i := 1; i < len(s); i++ {
		if (s[i-1] < 0) != (s[i] < 0) {
			n++
		}
	}

	return n
}

func peak(s []int16) int {
	var p int

	for _, v := range s {
		if a := int(math.Abs(float64(v))); a > p {
			p = a
		}
	}

	return p
}

func TestNewResampler(t *testing.T) {
	tests := []struct {
		name    string
		from    int
		to      int
		wantErr bool
	}{
		{name: "24k to 16k", from: 24000, to: 16000},
		{name: "16k to 24k", from: 16000, to: 24000},
		{name: "48k to 16k", from: 48000, to: 16000},
		{name: "identity", from: 24000, to: 24000, wantErr: true},
		{name: "zero", from: 0, to: 16000, wantErr: true},
		{name: "huge ratio", from: 44100, to: 16000, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewResampler(tt.from, tt.to)
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, r)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.from, r.FromRate())
			require.Equal(t, tt.to, r.ToRate())
		})
	}
}

func TestResampler_SineKeepsFrequencyAndAmplitude(t *testing.T) {
	const (
		freq      = 1000.0
		amplitude = 16000.0
	)

	in := sine(freq, amplitude, 24000, time.Second)

	r, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	out := samples(r.Process(in))

	// The filter delay eats a few samples at the start, nothing more.
	require.InDelta(t, 16000, len(out), 100)

	// Skip the start-up transient before measuring.
	steady := out[200:]
	gotFreq := float64(zeroCrossings(steady)) / 2 / (float64(len(steady)) / 16000)
	require.InDelta(t, freq, gotFreq, freq*0.05)
	require.InDelta(t, amplitude, float64(peak(steady)), amplitude*0.05)
}

func TestResampler_UpsampleSine(t *testing.T) {
	in := sine(1000, 10000, 16000, time.Second)

	r, err := NewResampler(16000, 24000)
	require.NoError(t, err)

	out := samples(r.Process(in))
	require.InDelta(t, 24000, len(out), 150)

	steady := out[300:]
	gotFreq := float64(zeroCrossings(steady)) / 2 / (float64(len(steady)) / 24000)
	require.InDelta(t, 1000, gotFreq, 50)
	require.InDelta(t, 10000, float64(peak(steady)), 500)
}

func TestResampler_ChunkedMatchesWhole(t *testing.T) {
	in := sine(440, 12000, 24000, 500*time.Millisecond)

	whole, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	want := whole.Process(in)

	tests := []struct {
		name   string
		chunks []int
	}{
		{name: "100ms", chunks: []int{4800}},
		{name: "odd sizes", chunks: []int{1, 3, 7, 101, 1023, 4799, 33}},
		{name: "single bytes then rest", chunks: []int{1, 1, 1, 1, 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewResampler(24000, 16000)
			require.NoError(t, err)

			var got []byte

			off := 0

			for i := 0; off < len(in); i++ {
				size := tt.chunks[i%len(tt.chunks)]
				end := min(off+size, len(in))
				got = append(got, r.Process(in[off:end])...)
				off = end
			}

			require.Equal(t, want, got)
		})
	}
}

func TestResampler_OddLengthCarry(t *testing.T) {
	r, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	in := sine(500, 5000, 24000, 100*time.Millisecond)

	// Feed an odd prefix: the trailing byte must be carried, never emitted as a half sample.
	first := r.Process(in[:2401])
	require.Equal(t, 0, len(first)%bytesPerSample)

	rest := r.Process(in[2401:])
	require.Equal(t, 0, len(rest)%bytesPerSample)

	whole, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	require.Equal(t, whole.Process(in), append(first, rest...))
}

func TestResampler_Reset(t *testing.T) {
	r, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	in := sine(700, 8000, 24000, 200*time.Millisecond)

	first := r.Process(in)
	r.Reset()

	again := r.Process(in)
	require.Equal(t, first, again)

	// After a reset an odd carry byte is gone too.
	r.Process([]byte{1})
	r.Reset()
	require.Nil(t, r.carry)
	require.Equal(t, int64(0), r.outIdx)
}

func TestResampler_SilenceStaysSilent(t *testing.T) {
	r, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	out := r.Process(Silence(24000, 200*time.Millisecond))
	for _, s := range samples(out) {
		require.Equal(t, int16(0), s)
	}
}

func TestResampleBase64(t *testing.T) {
	in := sine(1000, 1000, 24000, 50*time.Millisecond)
	b64 := base64.StdEncoding.EncodeToString(in)

	r, err := NewResampler(24000, 16000)
	require.NoError(t, err)

	got, err := ResampleBase64(r, b64)
	require.NoError(t, err)

	decoded, err := base64.StdEncoding.DecodeString(got)
	require.NoError(t, err)
	require.InDelta(t, len(in)*2/3, len(decoded), 200)

	t.Run("nil resampler is identity", func(t *testing.T) {
		got, err := ResampleBase64(nil, b64)
		require.NoError(t, err)
		require.Equal(t, b64, got)
	})

	t.Run("bad base64", func(t *testing.T) {
		_, err := ResampleBase64(r, "***")
		require.Error(t, err)
	})
}

func TestSplitPCM(t *testing.T) {
	tests := []struct {
		name       string
		length     int
		sampleRate int
		maxChunk   time.Duration
		wantSizes  []int
	}{
		{name: "empty", length: 0, sampleRate: 16000, maxChunk: 100 * time.Millisecond, wantSizes: nil},
		{name: "fits in one", length: 3000, sampleRate: 16000, maxChunk: 100 * time.Millisecond, wantSizes: []int{3000}},
		{name: "exact multiple", length: 9600, sampleRate: 16000, maxChunk: 100 * time.Millisecond, wantSizes: []int{3200, 3200, 3200}},
		{name: "remainder", length: 7000, sampleRate: 16000, maxChunk: 100 * time.Millisecond, wantSizes: []int{3200, 3200, 600}},
		{name: "24k 20ms", length: 2000, sampleRate: 24000, maxChunk: 20 * time.Millisecond, wantSizes: []int{960, 960, 80}},
		{name: "no max", length: 5000, sampleRate: 16000, maxChunk: 0, wantSizes: []int{5000}},
		{name: "no rate", length: 5000, sampleRate: 0, maxChunk: time.Second, wantSizes: []int{5000}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pcm := make([]byte, tt.length)
			chunks := SplitPCM(pcm, tt.sampleRate, tt.maxChunk)

			sizes := make([]int, 0, len(chunks))

			var total int

			for _, c := range chunks {
				sizes = append(sizes, len(c))
				total += len(c)
				require.Equal(t, 0, len(c)%bytesPerSample, "chunk must end on a sample boundary")
			}

			if tt.wantSizes == nil {
				require.Nil(t, chunks)
			} else {
				require.Equal(t, tt.wantSizes, sizes)
				require.Equal(t, tt.length, total)
			}
		})
	}
}

func TestSilence(t *testing.T) {
	require.Len(t, Silence(16000, 100*time.Millisecond), 3200)
	require.Len(t, Silence(24000, 100*time.Millisecond), 4800)
	require.Nil(t, Silence(24000, 0))
	require.Nil(t, Silence(0, time.Second))
}

func TestByteLenToSeconds(t *testing.T) {
	tests := []struct {
		name string
		n    int
		rate int
		want float64
	}{
		{name: "one second 24k", n: 48000, rate: 24000, want: 1},
		{name: "one second 16k", n: 32000, rate: 16000, want: 1},
		{name: "100ms 16k", n: 3200, rate: 16000, want: 0.1},
		{name: "zero", n: 0, rate: 24000, want: 0},
		{name: "no rate", n: 100, rate: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.InDelta(t, tt.want, ByteLenToSeconds(tt.n, tt.rate), 1e-9)
		})
	}
}
