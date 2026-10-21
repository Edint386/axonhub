package bailian

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"

	"github.com/tidwall/gjson"

	// Register the stdlib decoders for input_image data URIs.
	_ "image/gif"
	_ "image/png"
)

const (
	// maxImageBase64 is Bailian's limit for one input_image_buffer.append payload.
	maxImageBase64 = 256 * 1024
	// maxImageRaw caps the encoded image accepted for transcoding.
	maxImageRaw = 8 << 20
	// maxImageSide and maxImagePixels bound the decoded size before image.Decode allocates
	// (Bailian's own maximum is 1080p); they stop decompression bombs.
	maxImageSide   = 8192
	maxImagePixels = 1920 * 1080 * 4
	// jpegQuality is the transcoding quality for non-JPEG inputs.
	jpegQuality = 85
	// minImageSide stops downscaling once an image is too small to be useful.
	minImageSide = 64
)

// imageResult is a pre-converted input_image content part (see prepareImages).
type imageResult struct {
	b64 string
	err error
}

// prepareImages converts every input_image part of a conversation.item.create user message
// before the dialect mutex is taken, so image decoding never stalls the upstream pump. The
// map is keyed by content index; it is nil when the frame carries no images.
func prepareImages(frame []byte) map[int]imageResult {
	ev := gjson.ParseBytes(frame)
	if ev.Get("type").String() != "conversation.item.create" {
		return nil
	}

	item := ev.Get("item")
	if item.Get("type").String() != "message" || item.Get("role").String() == "assistant" || item.Get("role").String() == "system" {
		return nil
	}

	var out map[int]imageResult

	for i, p := range item.Get("content").Array() {
		if p.Get("type").String() != "input_image" {
			continue
		}

		if out == nil {
			out = map[int]imageResult{}
		}

		b64, err := convertImage(p.Get("image_url").String())
		out[i] = imageResult{b64: b64, err: err}
	}

	return out
}

// convertImage turns a GA input_image data URI into the raw base64 JPEG Bailian expects.
// PNG and GIF are transcoded with the stdlib; oversized images are box-downscaled by halves
// until they fit the 256 KB base64 limit.
func convertImage(dataURI string) (string, error) {
	payload := dataURI

	if strings.HasPrefix(dataURI, "data:") {
		header, rest, found := strings.Cut(dataURI, ",")
		if !found {
			return "", fmt.Errorf("bailian: malformed image data URI")
		}

		if !strings.Contains(header, ";base64") {
			return "", fmt.Errorf("bailian: image data URI must be base64 encoded")
		}

		payload = rest
	}

	if base64.StdEncoding.DecodedLen(len(payload)) > maxImageRaw {
		return "", fmt.Errorf("bailian: image exceeds %d bytes", maxImageRaw)
	}

	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("bailian: decode image base64: %w", err)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("bailian: decode image: %w", err)
	}

	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageSide || cfg.Height > maxImageSide || cfg.Width*cfg.Height > maxImagePixels {
		return "", fmt.Errorf("bailian: image dimensions %dx%d exceed the supported size", cfg.Width, cfg.Height)
	}

	if format == "jpeg" && len(payload) <= maxImageBase64 {
		return payload, nil
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("bailian: decode image: %w", err)
	}

	for {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return "", fmt.Errorf("bailian: encode jpeg: %w", err)
		}

		if base64.StdEncoding.EncodedLen(buf.Len()) <= maxImageBase64 {
			return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
		}

		b := img.Bounds()
		if b.Dx()/2 < minImageSide || b.Dy()/2 < minImageSide {
			return "", fmt.Errorf("bailian: image exceeds the 256 KB upstream limit even after downscaling")
		}

		img = halve(img)
	}
}

// halve returns img downscaled by two with a 2x2 box filter.
func halve(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	out := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		for x := range w {
			var r, g, bl, a uint32

			for dy := range 2 {
				for dx := range 2 {
					pr, pg, pb, pa := img.At(b.Min.X+2*x+dx, b.Min.Y+2*y+dy).RGBA()
					r += pr
					g += pg
					bl += pb
					a += pa
				}
			}

			out.SetRGBA(x, y, color.RGBA{
				R: uint8(r / 4 >> 8),  //nolint:gosec // G115: 16-bit channel reduced to 8 bits.
				G: uint8(g / 4 >> 8),  //nolint:gosec // G115: 16-bit channel reduced to 8 bits.
				B: uint8(bl / 4 >> 8), //nolint:gosec // G115: 16-bit channel reduced to 8 bits.
				A: uint8(a / 4 >> 8),  //nolint:gosec // G115: 16-bit channel reduced to 8 bits.
			})
		}
	}

	return out
}
