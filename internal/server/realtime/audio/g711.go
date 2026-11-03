package audio

import "encoding/binary"

// G711Rate is the sample rate of G.711 telephone audio (GA "audio/pcmu" and "audio/pcma").
const G711Rate = 8000

// DecodeMuLaw expands G.711 μ-law bytes (one byte per sample) to 16-bit little-endian PCM.
func DecodeMuLaw(in []byte) []byte {
	out := make([]byte, len(in)*2)

	for i, b := range in {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(muLawSample(b))) //nolint:gosec // G115: two's-complement PCM sample reinterpretation
	}

	return out
}

// DecodeALaw expands G.711 A-law bytes (one byte per sample) to 16-bit little-endian PCM.
func DecodeALaw(in []byte) []byte {
	out := make([]byte, len(in)*2)

	for i, b := range in {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(aLawSample(b))) //nolint:gosec // G115: two's-complement PCM sample reinterpretation
	}

	return out
}

// muLawSample decodes one μ-law byte (ITU-T G.711).
func muLawSample(b byte) int16 {
	u := ^b
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0f

	magnitude := ((int32(mantissa) << 3) + 0x84) << exponent
	magnitude -= 0x84

	if sign != 0 {
		return int16(-magnitude) //nolint:gosec // G115: bounded by the G.711 range (±32124)
	}

	return int16(magnitude) //nolint:gosec // G115: bounded by the G.711 range (±32124)
}

// aLawSample decodes one A-law byte (ITU-T G.711).
func aLawSample(b byte) int16 {
	a := b ^ 0x55
	sign := a & 0x80
	exponent := (a >> 4) & 0x07
	mantissa := int32(a & 0x0f)

	var magnitude int32
	if exponent == 0 {
		magnitude = (mantissa << 4) + 8
	} else {
		magnitude = ((mantissa << 4) + 0x108) << (exponent - 1)
	}

	if sign == 0 {
		return int16(-magnitude) //nolint:gosec // G115: bounded by the G.711 range (±32256)
	}

	return int16(magnitude) //nolint:gosec // G115: bounded by the G.711 range (±32256)
}
