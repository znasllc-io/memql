// Screenshot image helpers (memql-cockpit#163). No build tag: pure
// image-processing code extracted from the computeruse-tagged handler so CI
// can compile and test it headlessly.

package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

// downscaleImage resamples src to w x h with CatmullRom (high
// quality, fine for the at-most-once-per-screenshot cost). Callers
// decide the target via FitWithin so this never upscales in practice.
func downscaleImage(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

// encodeImage serializes img in the requested screenshot format.
// quality applies to jpeg only and is clamped to [1,100].
func encodeImage(img image.Image, format string, quality int) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case "jpeg", "jpg":
		if quality < 1 {
			quality = 1
		} else if quality > 100 {
			quality = 100
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	case "png", "":
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported image format %q (use png or jpeg)", format)
	}
	return buf.Bytes(), nil
}
