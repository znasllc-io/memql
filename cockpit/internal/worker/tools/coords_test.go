package tools

import (
	"strings"
	"testing"
)

// Pure coordinate-model tests (memql-cockpit#163). No display, no
// build tag: these run headlessly in CI and pin the contract that
// model coordinates live in the emitted screenshot's image space.

func TestFitWithin(t *testing.T) {
	cases := []struct {
		name         string
		w, h, max    int
		wantW, wantH int
	}{
		{name: "no upscale below limit", w: 800, h: 600, max: 1568, wantW: 800, wantH: 600},
		{name: "exactly at limit unchanged", w: 1568, h: 980, max: 1568, wantW: 1568, wantH: 980},
		{name: "retina 2x landscape", w: 3456, h: 2234, max: 1568, wantW: 1568, wantH: 1014},
		{name: "retina 2x portrait", w: 2234, h: 3456, max: 1568, wantW: 1014, wantH: 1568},
		{name: "1.25 scale capture", w: 1920, h: 1080, max: 1568, wantW: 1568, wantH: 882},
		{name: "square", w: 4000, h: 4000, max: 1000, wantW: 1000, wantH: 1000},
		{name: "extreme aspect floors short edge at 1", w: 10000, h: 2, max: 1568, wantW: 1568, wantH: 1},
		{name: "degenerate zero width returned as-is", w: 0, h: 500, max: 1568, wantW: 0, wantH: 500},
		{name: "degenerate max returned as-is", w: 640, h: 480, max: 0, wantW: 640, wantH: 480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotW, gotH := FitWithin(tc.w, tc.h, tc.max)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Fatalf("FitWithin(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tc.w, tc.h, tc.max, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestCoordinateMapperToLogical(t *testing.T) {
	// Geometries used across the table.
	retina := NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014)  // 2x Retina + downscale
	scale150 := NewCoordinateMapper(1280, 800, 1920, 1200, 1568, 980)  // 1.5x HiDPI + downscale
	scale125 := NewCoordinateMapper(1536, 864, 1920, 1080, 1568, 882)  // 1.25x fractional + downscale
	identity := NewCoordinateMapper(1024, 768, 1024, 768, 1024, 768)   // 1x, no downscale
	retinaRaw := NewCoordinateMapper(700, 500, 1400, 1000, 1400, 1000) // 2x, capture small enough to skip downscale

	cases := []struct {
		name         string
		m            CoordinateMapper
		x, y         int
		wantX, wantY int
	}{
		{name: "retina origin", m: retina, x: 0, y: 0, wantX: 0, wantY: 0},
		{name: "retina center-ish", m: retina, x: 784, y: 507, wantX: 864, wantY: 559},
		{name: "retina far corner stays on screen", m: retina, x: 1567, y: 1013, wantX: 1727, wantY: 1116},
		{name: "1.5x mapped point", m: scale150, x: 784, y: 490, wantX: 640, wantY: 400},
		{name: "1.5x far corner", m: scale150, x: 1567, y: 979, wantX: 1279, wantY: 799},
		{name: "1.25x mapped point", m: scale125, x: 1567, y: 881, wantX: 1535, wantY: 863},
		{name: "identity passes through", m: identity, x: 512, y: 384, wantX: 512, wantY: 384},
		{name: "identity far corner", m: identity, x: 1023, y: 767, wantX: 1023, wantY: 767},
		// 2x with no downscale: edge pixel maps to logicalW-0.5 which
		// rounds half away from zero to logicalW; the clamp must pull
		// it back inside the logical rect.
		{name: "2x raw edge rounds then clamps", m: retinaRaw, x: 1399, y: 999, wantX: 699, wantY: 499},
		{name: "2x raw even pixel", m: retinaRaw, x: 700, y: 500, wantX: 350, wantY: 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotX, gotY := tc.m.ToLogical(tc.x, tc.y)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("ToLogical(%d,%d) = (%d,%d), want (%d,%d)",
					tc.x, tc.y, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

func TestCoordinateMapperToEmitted(t *testing.T) {
	retina := NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014)
	cases := []struct {
		name         string
		x, y         int
		wantX, wantY int
	}{
		{name: "origin", x: 0, y: 0, wantX: 0, wantY: 0},
		{name: "logical corner maps inside emitted", x: 1727, y: 1116, wantX: 1567, wantY: 1013},
		{name: "mid point", x: 864, y: 559, wantX: 784, wantY: 507},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotX, gotY := retina.ToEmitted(tc.x, tc.y)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("ToEmitted(%d,%d) = (%d,%d), want (%d,%d)",
					tc.x, tc.y, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

// TestCoordinateMapperRoundTrip pins that emitted -> logical ->
// emitted lands within one pixel of where it started across the
// supported scale factors, so repeated mapping cannot drift.
func TestCoordinateMapperRoundTrip(t *testing.T) {
	mappers := map[string]CoordinateMapper{
		"retina2x": NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014),
		"hidpi1.5": NewCoordinateMapper(1280, 800, 1920, 1200, 1568, 980),
		"frac1.25": NewCoordinateMapper(1536, 864, 1920, 1080, 1568, 882),
		"identity": NewCoordinateMapper(1024, 768, 1024, 768, 1024, 768),
	}
	for name, m := range mappers {
		t.Run(name, func(t *testing.T) {
			points := [][2]int{
				{0, 0},
				{1, 1},
				{m.EmittedW / 2, m.EmittedH / 2},
				{m.EmittedW - 1, m.EmittedH - 1},
				{m.EmittedW / 3, (m.EmittedH * 2) / 3},
			}
			for _, p := range points {
				lx, ly := m.ToLogical(p[0], p[1])
				if lx < 0 || ly < 0 || lx >= m.LogicalW || ly >= m.LogicalH {
					t.Fatalf("ToLogical(%d,%d) = (%d,%d) escaped logical rect %dx%d",
						p[0], p[1], lx, ly, m.LogicalW, m.LogicalH)
				}
				ex, ey := m.ToEmitted(lx, ly)
				if abs(ex-p[0]) > 1 || abs(ey-p[1]) > 1 {
					t.Fatalf("round trip (%d,%d) -> (%d,%d) -> (%d,%d) drifted more than 1px",
						p[0], p[1], lx, ly, ex, ey)
				}
			}
		})
	}
}

func TestValidatePointInRect(t *testing.T) {
	cases := []struct {
		name       string
		x, y, w, h int
		wantOK     bool
	}{
		{name: "origin inside", x: 0, y: 0, w: 1568, h: 1014, wantOK: true},
		{name: "far corner inside", x: 1567, y: 1013, w: 1568, h: 1014, wantOK: true},
		{name: "x at width is outside", x: 1568, y: 0, w: 1568, h: 1014, wantOK: false},
		{name: "y at height is outside", x: 0, y: 1014, w: 1568, h: 1014, wantOK: false},
		{name: "negative x", x: -1, y: 10, w: 1568, h: 1014, wantOK: false},
		{name: "negative y", x: 10, y: -1, w: 1568, h: 1014, wantOK: false},
		{name: "empty rect always rejects", x: 0, y: 0, w: 0, h: 0, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail := validatePointInRect("mouse_move", tc.x, tc.y, tc.w, tc.h)
			if tc.wantOK {
				if fail != nil {
					t.Fatalf("expected (%d,%d) inside %dx%d, got failure: %s",
						tc.x, tc.y, tc.w, tc.h, fail.GetErrorMessage())
				}
				return
			}
			if fail == nil {
				t.Fatalf("expected (%d,%d) outside %dx%d to fail, got nil", tc.x, tc.y, tc.w, tc.h)
			}
			if fail.GetErrorCode() != "out_of_bounds" {
				t.Fatalf("error code = %q, want out_of_bounds", fail.GetErrorCode())
			}
			if tc.w > 0 && tc.h > 0 && !strings.Contains(fail.GetErrorMessage(), "(0,0)..(1567,1013)") {
				t.Fatalf("message should name the valid rect, got: %s", fail.GetErrorMessage())
			}
			if !strings.Contains(fail.GetErrorMessage(), "mouse_move") {
				t.Fatalf("message should carry the action label, got: %s", fail.GetErrorMessage())
			}
		})
	}
}

func TestClampLongEdge(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{in: 100, want: 512},
		{in: 512, want: 512},
		{in: 1568, want: 1568},
		{in: 8000, want: 8000},
		{in: 99999, want: 8000},
	}
	for _, tc := range cases {
		if got := clampLongEdge(tc.in); got != tc.want {
			t.Fatalf("clampLongEdge(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Multi-display geometry tests (memql-cockpit#165). Pure: no
// display, no build tag.

// TestCoordinateMapperWithOrigin pins the per-display mapping
// contract: emitted coords on display D map to D-local logical
// points clamped into D's rect, then offset by D's virtual-desktop
// origin (negative for displays left of / above the primary).
func TestCoordinateMapperWithOrigin(t *testing.T) {
	// Secondary 1920x1080 at logical capture (captured == logical),
	// downscaled to 1568x882.
	right := NewCoordinateMapper(1920, 1080, 1920, 1080, 1568, 882).WithOrigin(1728, 0)
	left := NewCoordinateMapper(1920, 1080, 1920, 1080, 1568, 882).WithOrigin(-1920, 0)
	above := NewCoordinateMapper(1920, 1080, 1920, 1080, 1568, 882).WithOrigin(0, -1080)
	// Secondary small enough to skip the downscale policy entirely.
	rightRaw := NewCoordinateMapper(1280, 720, 1280, 720, 1280, 720).WithOrigin(2560, 200)

	cases := []struct {
		name         string
		m            CoordinateMapper
		x, y         int
		wantX, wantY int
	}{
		{name: "right monitor origin maps to its corner", m: right, x: 0, y: 0, wantX: 1728, wantY: 0},
		{name: "right monitor far corner stays inside", m: right, x: 1567, y: 881, wantX: 1728 + 1919, wantY: 1079},
		{name: "right monitor mid point", m: right, x: 784, y: 441, wantX: 1728 + 960, wantY: 540},
		{name: "left monitor origin is negative", m: left, x: 0, y: 0, wantX: -1920, wantY: 0},
		{name: "left monitor mid point", m: left, x: 784, y: 441, wantX: -960, wantY: 540},
		{name: "left monitor far corner stays left of primary", m: left, x: 1567, y: 881, wantX: -1, wantY: 1079},
		{name: "above monitor maps to negative y", m: above, x: 0, y: 0, wantX: 0, wantY: -1080},
		{name: "no-downscale identity plus offset", m: rightRaw, x: 100, y: 50, wantX: 2660, wantY: 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotX, gotY := tc.m.ToLogical(tc.x, tc.y)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("ToLogical(%d,%d) = (%d,%d), want (%d,%d)",
					tc.x, tc.y, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

// TestCoordinateMapperWithOriginZeroIsIdentity pins that a zero
// origin -- the default for every mapper built before multi-display
// support -- changes nothing, so the single-display behavior is
// byte-identical.
func TestCoordinateMapperWithOriginZeroIsIdentity(t *testing.T) {
	base := NewCoordinateMapper(1728, 1117, 3456, 2234, 1568, 1014)
	offset := base.WithOrigin(0, 0)
	points := [][2]int{{0, 0}, {784, 507}, {1567, 1013}}
	for _, p := range points {
		bx, by := base.ToLogical(p[0], p[1])
		ox, oy := offset.ToLogical(p[0], p[1])
		if bx != ox || by != oy {
			t.Fatalf("WithOrigin(0,0) diverged at (%d,%d): base (%d,%d) vs origin (%d,%d)",
				p[0], p[1], bx, by, ox, oy)
		}
	}
}

// TestCoordinateMapperWithOriginRoundTrip pins emitted -> logical ->
// emitted within one pixel for offset displays, including negative
// origins, so repeated mapping cannot drift across displays.
func TestCoordinateMapperWithOriginRoundTrip(t *testing.T) {
	mappers := map[string]CoordinateMapper{
		"right":     NewCoordinateMapper(1920, 1080, 1920, 1080, 1568, 882).WithOrigin(1728, 0),
		"left":      NewCoordinateMapper(1920, 1080, 1920, 1080, 1568, 882).WithOrigin(-1920, 0),
		"above":     NewCoordinateMapper(2560, 1440, 2560, 1440, 1568, 882).WithOrigin(0, -1440),
		"identity+": NewCoordinateMapper(1280, 720, 1280, 720, 1280, 720).WithOrigin(2560, 200),
	}
	for name, m := range mappers {
		t.Run(name, func(t *testing.T) {
			points := [][2]int{
				{0, 0},
				{1, 1},
				{m.EmittedW / 2, m.EmittedH / 2},
				{m.EmittedW - 1, m.EmittedH - 1},
			}
			for _, p := range points {
				lx, ly := m.ToLogical(p[0], p[1])
				if lx < m.OriginX || ly < m.OriginY ||
					lx >= m.OriginX+m.LogicalW || ly >= m.OriginY+m.LogicalH {
					t.Fatalf("ToLogical(%d,%d) = (%d,%d) escaped display rect origin (%d,%d) size %dx%d",
						p[0], p[1], lx, ly, m.OriginX, m.OriginY, m.LogicalW, m.LogicalH)
				}
				ex, ey := m.ToEmitted(lx, ly)
				if abs(ex-p[0]) > 1 || abs(ey-p[1]) > 1 {
					t.Fatalf("round trip (%d,%d) -> (%d,%d) -> (%d,%d) drifted more than 1px",
						p[0], p[1], lx, ly, ex, ey)
				}
			}
		})
	}
}

func TestValidatePointInDisplays(t *testing.T) {
	sideBySide := []DisplayRect{
		{ID: 0, X: 0, Y: 0, W: 1920, H: 1080},
		{ID: 1, X: 1920, Y: 0, W: 1920, H: 1080},
	}
	// L-shaped: a wide primary with a smaller display below-left. The
	// bounding box would cover (1920..2559, 1440..2519) but no
	// display does -- that dead zone must reject.
	lShape := []DisplayRect{
		{ID: 0, X: 0, Y: 0, W: 2560, H: 1440},
		{ID: 1, X: 0, Y: 1440, W: 1920, H: 1080},
	}
	leftMonitor := []DisplayRect{
		{ID: 0, X: 0, Y: 0, W: 1728, H: 1117},
		{ID: 1, X: -1920, Y: 0, W: 1920, H: 1080},
	}
	gapped := []DisplayRect{
		{ID: 0, X: 0, Y: 0, W: 1000, H: 1000},
		{ID: 1, X: 1100, Y: 0, W: 1000, H: 1000},
	}
	withDegenerate := []DisplayRect{
		{ID: 0, X: 0, Y: 0, W: 0, H: 0},
		{ID: 1, X: 10, Y: 10, W: 100, H: 100},
	}

	cases := []struct {
		name     string
		displays []DisplayRect
		x, y     int
		wantOK   bool
	}{
		{name: "primary origin", displays: sideBySide, x: 0, y: 0, wantOK: true},
		{name: "primary far corner", displays: sideBySide, x: 1919, y: 1079, wantOK: true},
		{name: "secondary first column", displays: sideBySide, x: 1920, y: 0, wantOK: true},
		{name: "secondary far corner", displays: sideBySide, x: 3839, y: 1079, wantOK: true},
		{name: "past the union right edge", displays: sideBySide, x: 3840, y: 0, wantOK: false},
		{name: "below both displays", displays: sideBySide, x: 0, y: 1080, wantOK: false},
		{name: "negative x with no left display", displays: sideBySide, x: -1, y: 0, wantOK: false},
		{name: "L-shape inside lower display", displays: lShape, x: 1900, y: 1500, wantOK: true},
		{name: "L-shape dead zone rejects", displays: lShape, x: 2000, y: 1500, wantOK: false},
		{name: "L-shape primary far corner", displays: lShape, x: 2559, y: 1439, wantOK: true},
		{name: "L-shape right of primary", displays: lShape, x: 2560, y: 0, wantOK: false},
		{name: "left monitor negative coords valid", displays: leftMonitor, x: -1, y: 0, wantOK: true},
		{name: "left monitor far origin valid", displays: leftMonitor, x: -1920, y: 0, wantOK: true},
		{name: "past the left monitor", displays: leftMonitor, x: -1921, y: 0, wantOK: false},
		{name: "below left monitor but left of primary", displays: leftMonitor, x: -1, y: 1085, wantOK: false},
		{name: "gap between monitors rejects", displays: gapped, x: 1050, y: 500, wantOK: false},
		{name: "right edge of gap is valid", displays: gapped, x: 1100, y: 500, wantOK: true},
		{name: "no displays rejects", displays: nil, x: 0, y: 0, wantOK: false},
		{name: "degenerate rect contains nothing", displays: withDegenerate, x: 5, y: 5, wantOK: false},
		{name: "degenerate rect does not mask real ones", displays: withDegenerate, x: 10, y: 10, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail := validatePointInDisplays("mouse_move", tc.x, tc.y, tc.displays)
			if tc.wantOK {
				if fail != nil {
					t.Fatalf("expected (%d,%d) inside the display union, got failure: %s",
						tc.x, tc.y, fail.GetErrorMessage())
				}
				return
			}
			if fail == nil {
				t.Fatalf("expected (%d,%d) outside the display union to fail, got nil", tc.x, tc.y)
			}
			if fail.GetErrorCode() != "out_of_bounds" {
				t.Fatalf("error code = %q, want out_of_bounds", fail.GetErrorCode())
			}
			if !strings.Contains(fail.GetErrorMessage(), "mouse_move") {
				t.Fatalf("message should carry the action label, got: %s", fail.GetErrorMessage())
			}
			if len(tc.displays) > 0 && !strings.Contains(fail.GetErrorMessage(), "display 0") {
				t.Fatalf("message should name the display rects, got: %s", fail.GetErrorMessage())
			}
		})
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
