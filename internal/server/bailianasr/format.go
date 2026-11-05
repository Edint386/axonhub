package bailianasr

import (
	"bytes"
	"encoding/binary"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// audioFormat is one container DashScope accepts: Name is the parameters.format value (and the
// file extension), MIME the data-URI media type.
type audioFormat struct {
	Name string
	MIME string
}

var audioFormats = map[string]audioFormat{
	"aac":  {"aac", "audio/aac"},
	"amr":  {"amr", "audio/amr"},
	"avi":  {"avi", "video/x-msvideo"},
	"flac": {"flac", "audio/flac"},
	"flv":  {"flv", "video/x-flv"},
	"m4a":  {"m4a", "audio/mp4"},
	"mkv":  {"mkv", "video/x-matroska"},
	"mov":  {"mov", "video/quicktime"},
	"mp3":  {"mp3", "audio/mpeg"},
	"mp4":  {"mp4", "video/mp4"},
	"mpeg": {"mpeg", "video/mpeg"},
	"ogg":  {"ogg", "audio/ogg"},
	"opus": {"opus", "audio/ogg"},
	"wav":  {"wav", "audio/wav"},
	"webm": {"webm", "audio/webm"},
	"wma":  {"wma", "audio/x-ms-wma"},
	"wmv":  {"wmv", "video/x-ms-wmv"},
}

var extensionAliases = map[string]string{
	"mpga": "mp3", "mpg": "mpeg", "oga": "ogg", "wave": "wav", "weba": "webm",
}

// sniffHeadSize covers the WAV fmt chunk and the Ogg/Matroska codec markers.
const sniffHeadSize = 4096

// detectAudioFormat identifies the container from its magic bytes, then from the file
// extension of name (a file name or URL).
func detectAudioFormat(head []byte, name string) (audioFormat, bool) {
	if f, ok := sniffAudioFormat(head); ok {
		return audioFormats[f], true
	}

	f, ok := audioFormats[extensionFormat(name)]

	return f, ok
}

func extensionFormat(name string) string {
	if u, err := url.Parse(name); err == nil && u.Scheme != "" {
		name = u.Path
	}

	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if alias, ok := extensionAliases[ext]; ok {
		return alias
	}

	return ext
}

//nolint:cyclop // a flat magic-byte table reads best as one switch.
func sniffAudioFormat(head []byte) (string, bool) {
	switch {
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return "wav", true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("AVI ")):
		return "avi", true
	case bytes.HasPrefix(head, []byte("ID3")):
		return "mp3", true
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xF6 == 0xF0:
		return "aac", true // ADTS sync word with MPEG layer 0
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return "mp3", true
	case bytes.HasPrefix(head, []byte("OggS")):
		if bytes.Contains(head, []byte("OpusHead")) {
			return "opus", true
		}

		return "ogg", true
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "flac", true
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		return isoBrandFormat(head[8:12]), true
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		if bytes.Contains(head, []byte("matroska")) {
			return "mkv", true
		}

		return "webm", true
	case bytes.HasPrefix(head, []byte("#!AMR")):
		return "amr", true
	case bytes.HasPrefix(head, []byte("FLV")):
		return "flv", true
	case bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xBA}), bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xB3}):
		return "mpeg", true
	default:
		return "", false
	}
}

func isoBrandFormat(brand []byte) string {
	switch string(brand) {
	case "M4A ", "M4B ":
		return "m4a"
	case "qt  ":
		return "mov"
	default:
		return "mp4"
	}
}

// wavSampleRate reads the sample rate from the fmt chunk of a RIFF/WAVE header.
func wavSampleRate(head []byte) (string, bool) {
	if len(head) < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return "", false
	}

	for off := 12; off+8 <= len(head); {
		size := int(binary.LittleEndian.Uint32(head[off+4 : off+8]))
		if string(head[off:off+4]) == "fmt " {
			if off+16 > len(head) {
				return "", false
			}

			rate := binary.LittleEndian.Uint32(head[off+12 : off+16])
			if rate == 0 {
				return "", false
			}

			return strconv.FormatUint(uint64(rate), 10), true
		}

		off += 8 + size + size%2
	}

	return "", false
}
