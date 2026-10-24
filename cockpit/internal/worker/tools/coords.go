// Coordinate model for workerComputer (memql-cockpit#163).
//
// The contract: model/tool coordinates are ALWAYS in the coordinate
// space of the most recently emitted screenshot image, i.e.
// (0,0)..(emittedWidth-1, emittedHeight-1). Three spaces exist on a
// HiDPI host and they differ:
//
//   - logical (input) space: the points RobotGo moves/clicks in.
//     robotgo.GetScreenSize() reports this size.
//   - captured (physical) space: the pixel dimensions of the raw
//     capture. On a 2x Retina display this is 2x the logical size.
//   - emitted space: the captured image after the downscale policy
//     (long edge clamped to defaultMaxLongEdge) -- this is what the
//     model actually sees and therefore the space it speaks.
//
// The worker maps emitted-space coordinates to logical input
// coordinates deterministically from the current display geometry
// plus the fixed downscale policy, so the mapping is stateless and
// reproducible per call: no per-screenshot state is retained between
// a screenshot and the mouse action that follows it. Screenshot
// payloads report width/height (emitted), sourceWidth/sourceHeight
// (captured), scale (emitted/captured) and logicalWidth/logicalHeight
// so the mapping is reversible by the caller.
//
// Note: workerComputer.screenshot accepts a maxLongEdge override for
// inspection/zoom use; mouse-action mapping always assumes the
// default policy, so click targets should be derived from
// default-policy screenshots.
//
// Multi-display (memql-cockpit#165): screenshot, mouse_move and
// mouse_drag accept an optional `display` arg (an id from
// display_info; default 0 = primary). Coordinates are ALWAYS
// interpreted in the emitted-screenshot space OF THAT DISPLAY -- the
// space a screenshot with the same `display` value emits -- never in
// a cross-display virtual space, so the mapping stays stateless: the
// worker never remembers which display was screenshotted last. The
// mapper for display D maps emitted coords into D-local logical
// coords and then offsets by D's origin in the virtual desktop
// (OriginX/OriginY, negative for displays left of / above the
// primary). When `display` is absent the primary-display mapping is
// exactly the single-display behavior. Mapped logical points must
// land inside SOME display rect (validatePointInDisplays): the
// virtual desktop is a union of per-display rects, and a point in a
// gap between monitors is out_of_bounds, not a valid target.
//
// Everything in this file is pure (no build tag, no display needed)
// so CI can test it headlessly.

package tools

import (
	"fmt"
	"math"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// defaultMaxLongEdge is the downscale policy's default ceiling for a
// screenshot's long edge, in pixels. 1568 is the Anthropic vision
// sweet spot: larger images are downscaled by the API anyway, so
// shipping more pixels only bloats the payload.
const defaultMaxLongEdge = 1568

// maxLongEdge override bounds for workerComputer.screenshot.
const (
	minAllowedLongEdge = 512
	maxAllowedLongEdge = 8000
)

// CoordinateMapper converts between the emitted screenshot image
// space (what the model sees) and the logical input space (what
// RobotGo acts in), given the captured (physical) dimensions that
// link the two. Construct one per call from live display geometry;
// it holds no cross-call state.
type CoordinateMapper struct {
	LogicalW, LogicalH   int // logical input space (RobotGo points)
	CapturedW, CapturedH int // physical capture dimensions
	EmittedW, EmittedH   int // emitted image dimensions (post-downscale)
	// OriginX, OriginY locate the mapped display's top-left corner in
	// the virtual-desktop logical space (memql-cockpit#165). Zero for
	// the primary display -- the pre-multi-display behavior -- and
	// possibly negative for displays left of / above the primary.
	OriginX, OriginY int
}

// NewCoordinateMapper builds a mapper. Non-positive dimensions are
// normalized to 1 so the mapper never divides by zero; callers are
// expected to bounds-validate points separately via
// validatePointInRect.
func NewCoordinateMapper(logicalW, logicalH, capturedW, capturedH, emittedW, emittedH int) CoordinateMapper {
	return CoordinateMapper{
		LogicalW: atLeast1(logicalW), LogicalH: atLeast1(logicalH),
		CapturedW: atLeast1(capturedW), CapturedH: atLeast1(capturedH),
		EmittedW: atLeast1(emittedW), EmittedH: atLeast1(emittedH),
	}
}

// WithOrigin returns a copy of the mapper whose mapped display sits
// at (x, y) in the virtual-desktop logical space (memql-cockpit#165).
// The zero origin is the primary display, so a mapper without
// WithOrigin behaves exactly as before multi-display support.
func (m CoordinateMapper) WithOrigin(x, y int) CoordinateMapper {
	m.OriginX, m.OriginY = x, y
	return m
}

// ToLogical maps a point in emitted-image space to logical input
// space. Per axis: scale by logical/emitted, round half away from
// zero, clamp into the display-local logical rect so edge pixels can
// never map past the display, then offset by the display origin into
// the virtual-desktop space.
func (m CoordinateMapper) ToLogical(x, y int) (int, int) {
	lx := roundHalfAway(float64(x) * float64(m.LogicalW) / float64(m.EmittedW))
	ly := roundHalfAway(float64(y) * float64(m.LogicalH) / float64(m.EmittedH))
	return m.OriginX + clampInt(lx, 0, m.LogicalW-1), m.OriginY + clampInt(ly, 0, m.LogicalH-1)
}

// ToEmitted maps a point in (virtual-desktop) logical input space
// back to emitted-image space (the reverse of ToLogical): the display
// origin is subtracted first, then the same rounding and clamping
// rules apply.
func (m CoordinateMapper) ToEmitted(x, y int) (int, int) {
	x -= m.OriginX
	y -= m.OriginY
	ex := roundHalfAway(float64(x) * float64(m.EmittedW) / float64(m.LogicalW))
	ey := roundHalfAway(float64(y) * float64(m.EmittedH) / float64(m.LogicalH))
	return clampInt(ex, 0, m.EmittedW-1), clampInt(ey, 0, m.EmittedH-1)
}

// FitWithin returns the proportional downscale target for a w x h
// image so its long edge does not exceed maxLongEdge. It never
// upscales: images already within the limit are returned unchanged.
// The short edge is rounded half away from zero and floored at 1.
func FitWithin(w, h, maxLongEdge int) (int, int) {
	if w <= 0 || h <= 0 || maxLongEdge <= 0 {
		return w, h
	}
	long := w
	if h > long {
		long = h
	}
	if long <= maxLongEdge {
		return w, h
	}
	ratio := float64(maxLongEdge) / float64(long)
	return atLeast1(roundHalfAway(float64(w) * ratio)), atLeast1(roundHalfAway(float64(h) * ratio))
}

// clampLongEdge applies the [minAllowedLongEdge, maxAllowedLongEdge]
// bounds to a caller-supplied maxLongEdge override.
func clampLongEdge(v int) int {
	return clampInt(v, minAllowedLongEdge, maxAllowedLongEdge)
}

// validatePointInRect bounds-checks a point against the rect
// (0,0)..(w-1,h-1). Returns nil when inside; otherwise a structured
// out_of_bounds failure naming the valid rect, for mouse handlers to
// return INSTEAD of acting on an undefined target.
func validatePointInRect(label string, x, y, w, h int) *memqlv1.Failure {
	if w <= 0 || h <= 0 {
		return failure("out_of_bounds", fmt.Sprintf(
			"%s: point (%d,%d) cannot be validated against an empty rect %dx%d", label, x, y, w, h))
	}
	if x < 0 || y < 0 || x >= w || y >= h {
		return failure("out_of_bounds", fmt.Sprintf(
			"%s: point (%d,%d) is outside the valid rect (0,0)..(%d,%d); coordinates must be in the most recent screenshot's image space",
			label, x, y, w-1, h-1))
	}
	return nil
}

// DisplayRect is one display's rect in the virtual-desktop logical
// coordinate space (memql-cockpit#165). X/Y are the top-left origin
// relative to the primary display's (0,0) -- negative for displays
// left of / above the primary -- and W/H the logical size.
type DisplayRect struct {
	ID         int
	X, Y, W, H int
}

// contains reports whether the (virtual-desktop logical) point lies
// inside this display's rect. Degenerate rects contain nothing.
func (d DisplayRect) contains(x, y int) bool {
	return d.W > 0 && d.H > 0 &&
		x >= d.X && y >= d.Y && x < d.X+d.W && y < d.Y+d.H
}

// validatePointInDisplays bounds-checks a virtual-desktop logical
// point against the union of per-display rects: valid iff SOME
// display contains it. This is deliberately NOT a bounding-box check
// -- in an L-shaped or gapped layout the bounding box includes dead
// zones no display covers, and a point there has no target, so it
// must fail with a structured out_of_bounds naming every display
// rect instead of silently landing somewhere undefined.
func validatePointInDisplays(label string, x, y int, displays []DisplayRect) *memqlv1.Failure {
	for _, d := range displays {
		if d.contains(x, y) {
			return nil
		}
	}
	if len(displays) == 0 {
		return failure("out_of_bounds", fmt.Sprintf(
			"%s: logical point (%d,%d) cannot be validated: no displays enumerated", label, x, y))
	}
	rects := make([]string, 0, len(displays))
	for _, d := range displays {
		rects = append(rects, fmt.Sprintf("display %d (%d,%d)..(%d,%d)", d.ID, d.X, d.Y, d.X+d.W-1, d.Y+d.H-1))
	}
	return failure("out_of_bounds", fmt.Sprintf(
		"%s: logical point (%d,%d) is outside every display rect [%s]; gaps between displays are not valid targets",
		label, x, y, strings.Join(rects, ", ")))
}

// roundHalfAway rounds to the nearest integer with halves away from
// zero (math.Round semantics), the deterministic rule the coordinate
// contract specifies.
func roundHalfAway(f float64) int {
	return int(math.Round(f))
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func atLeast1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
