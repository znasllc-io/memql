package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func testGradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}

func TestDownscaleImageDims(t *testing.T) {
	src := testGradient(100, 60)
	dst := downscaleImage(src, 50, 30)
	if got := dst.Bounds(); got.Dx() != 50 || got.Dy() != 30 {
		t.Fatalf("downscaled dims = %dx%d, want 50x30", got.Dx(), got.Dy())
	}
}

func TestEncodeImagePNG(t *testing.T) {
	raw, err := encodeImage(testGradient(40, 20), "png", 0)
	if err != nil {
		t.Fatalf("encode png: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png round trip: %v", err)
	}
	if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 20 {
		t.Fatalf("png round trip dims = %dx%d, want 40x20", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestEncodeImageJPEGClampsQuality(t *testing.T) {
	// Out-of-range qualities must clamp rather than error.
	for _, q := range []int{-5, 0, 50, 100, 500} {
		raw, err := encodeImage(testGradient(40, 20), "jpeg", q)
		if err != nil {
			t.Fatalf("encode jpeg quality=%d: %v", q, err)
		}
		if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
			t.Fatalf("decode jpeg quality=%d: %v", q, err)
		}
	}
}

func TestEncodeImageRejectsUnknownFormat(t *testing.T) {
	if _, err := encodeImage(testGradient(4, 4), "webp", 80); err == nil {
		t.Fatal("expected unsupported-format error for webp, got nil")
	}
}
