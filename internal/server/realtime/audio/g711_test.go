package audio

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestG711ReferenceValues(t *testing.T) {
	// ITU-T G.711 reference points.
	require.EqualValues(t, 0, muLawSample(0xff), "μ-law positive zero")
	require.EqualValues(t, 0, muLawSample(0x7f), "μ-law negative zero")
	require.EqualValues(t, -32124, muLawSample(0x00))
	require.EqualValues(t, 32124, muLawSample(0x80))

	require.EqualValues(t, 8, aLawSample(0xd5), "A-law smallest positive step")
	require.EqualValues(t, -8, aLawSample(0x55), "A-law smallest negative step")
	require.EqualValues(t, 32256, aLawSample(0xaa))
	require.EqualValues(t, -32256, aLawSample(0x2a))
}

func TestG711DecodersAreMonotonicAndSymmetric(t *testing.T) {
	for b := range 256 {
		mu := muLawSample(byte(b))
		require.Equal(t, mu, -muLawSample(byte(b)^0x80), "μ-law sign bit")

		a := aLawSample(byte(b))
		require.Equal(t, a, -aLawSample(byte(b)^0x80), "A-law sign bit")
	}
}

func TestDecodeG711Lengths(t *testing.T) {
	in := []byte{0xff, 0x80, 0x00}

	mu := DecodeMuLaw(in)
	require.Len(t, mu, 6)
	require.EqualValues(t, 32124, int16(binary.LittleEndian.Uint16(mu[2:]))) //nolint:gosec // G115: PCM sample reinterpretation

	a := DecodeALaw([]byte{0xd5, 0x55})
	require.Len(t, a, 4)
	require.EqualValues(t, 8, int16(binary.LittleEndian.Uint16(a[0:])))  //nolint:gosec // G115: PCM sample reinterpretation
	require.EqualValues(t, -8, int16(binary.LittleEndian.Uint16(a[2:]))) //nolint:gosec // G115: PCM sample reinterpretation
}
