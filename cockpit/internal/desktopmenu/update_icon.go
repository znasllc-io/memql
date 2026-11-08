package desktopmenu

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// updateIcon keeps the familiar MemQL mark and overlays a high-contrast up
// arrow. Shape, plus the tray's title/tooltip, conveys availability without
// depending on a color difference or an animation.
func updateIcon() []byte {
	source, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		return icon
	}
	canvas := image.NewNRGBA(source.Bounds())
	draw.Draw(canvas, canvas.Bounds(), source, source.Bounds().Min, draw.Src)
	// The embedded app icon is 64 px. Draw the badge at that resolution so
	// desktop panels can scale the complete image as a single icon.
	for y := 36; y < 64; y++ {
		for x := 36; x < 64; x++ {
			dx, dy := x-50, y-50
			if dx*dx+dy*dy <= 13*13 {
				canvas.Set(x, y, color.RGBA{255, 255, 255, 255})
			}
			if dx*dx+dy*dy <= 11*11 {
				canvas.Set(x, y, color.RGBA{0, 90, 66, 255})
			}
		}
	}
	for y := 43; y <= 56; y++ {
		for x := 43; x <= 57; x++ {
			dx := x - 50
			if dx < 0 {
				dx = -dx
			}
			shaft := x >= 48 && x <= 52 && y >= 47
			arrow := y <= 50 && dx <= y-43 && dx >= y-46
			if shaft || arrow {
				canvas.Set(x, y, color.White)
			}
		}
	}
	var result bytes.Buffer
	if png.Encode(&result, canvas) != nil {
		return icon
	}
	return result.Bytes()
}
