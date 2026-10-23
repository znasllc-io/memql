//go:build computeruse

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/png" // register the PNG decoder for image.Decode (SaveCapture writes PNG)
	"os"
	"strings"

	"github.com/go-vgo/robotgo"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// computerActionHandlers is the single source of truth for the
// computer-use-backed workerComputer actions this build supports. The
// dispatcher routes through it AND the capability descriptor
// (capabilities_computeruse.go) derives its advertised action list from it,
// so the two can never drift (memql-cockpit#162).
//
// The `capabilities` and `wait` actions are absent -- they are
// build-agnostic and routed by the dispatcher before this table is
// consulted (capabilities.go / wait.go).
//
// window_list / window_focus (memql-cockpit#167) are real on macOS
// (CGWindowList) and Linux/X11 (EWMH); on Wayland and other computeruse
// platforms their handlers return a structured
// unsupported_on_platform failure -- consumers should read the
// descriptor's displayServer field alongside the action list.
var computerActionHandlers = map[string]func(map[string]any) (*memqlv1.Success, *memqlv1.Failure){
	"screenshot":      guiScreenshot,
	"cursor_position": guiCursorPosition,
	"mouse_move":      guiMouseMove,
	"mouse_click":     guiMouseClick,
	"mouse_down":      guiMouseDown,
	"mouse_up":        guiMouseUp,
	"mouse_drag":      guiMouseDrag,
	"mouse_scroll":    guiMouseScroll,
	"key_type":        guiKeyType,
	"key_combo":       guiKeyCombo,
	"key_hold":        guiKeyHold,
	"display_info":    guiDisplayInfo,
	"window_list":     guiWindowList,
	"window_focus":    guiWindowFocus,
}

// dispatchComputer routes workerComputer.<action> to the matching
// RobotGo-backed implementation. The default-build sibling
// (computer.go, //go:build !computeruse) returns Unimplemented.
//
// Handlers run on the memql-cockpit-computeruse process; macOS TCC permissions
// (Accessibility for input, Screen Recording for screenshot) are
// required and probed at startup -- see worker.PermissionStatus
// and `memql worker setup` for the operator wizard.
func (d *Dispatcher) dispatchComputer(ctx context.Context, action string, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	_ = ctx
	if handler, ok := computerActionHandlers[action]; ok {
		return handler(args)
	}
	return nil, &memqlv1.Failure{
		ErrorCode:    "unknown_action",
		ErrorMessage: fmt.Sprintf("workerComputer.%s not implemented", action),
	}
}

// guiScreenshot captures a display (or a region of it) and returns
// base64-encoded image bytes plus dimensions.
//
// Args:
//
//	format: "png" (default) | "jpeg"
//	display: int display id from display_info (default 0 = primary)
//	region: { x, y, w, h } -- optional sub-rect (logical coords,
//	  relative to the chosen display); full display when absent
//	quality: int 1-100 (jpeg only; default 80)
//	maxLongEdge: int, downscale ceiling for the emitted image's long
//	  edge (default 1568, clamped to [512, 8000])
//
// Coordinate contract (see coords.go): the emitted image defines the
// space the model speaks, scoped to the TARGET display. width/height
// are the emitted dims, sourceWidth/sourceHeight the captured dims,
// scale the emitted/captured ratio, logicalWidth/logicalHeight the
// display's RobotGo input-space dims (the requested w/h for region
// captures), display the targeted display id.
func guiScreenshot(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	// The per-call Screen Recording TCC preflight runs in the
	// dispatcher's preflightComputerAction (preflight.go) before this
	// handler is reached, alongside the Accessibility + display-server
	// gates (memql-cockpit#164).
	format := strings.ToLower(strings.TrimSpace(argString(args, "format")))
	if format == "" {
		format = "png"
	}
	if format != "png" && format != "jpeg" && format != "jpg" {
		return nil, failure("bad_request", fmt.Sprintf("screenshot format %q not supported (use png or jpeg)", format))
	}
	region := argMap(args, "region")
	displayID := argInt(args, "display", 0)

	var img image.Image
	var logicalW, logicalH int
	if displayID == 0 {
		// Primary display: RobotGo's native capture path (physical
		// pixels on Retina/HiDPI hosts), unchanged from the
		// single-display behavior. Capture always lands as PNG; the
		// requested output format is applied at re-encode time below.
		tmp, err := os.CreateTemp("", "worker-screenshot-*.png")
		if err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		defer os.Remove(tmpPath)

		// Logical (input-space) dims for the payload: full screen uses
		// the RobotGo screen size; a region capture is relative to the
		// requested logical rect.
		logicalW, logicalH = robotgo.GetScreenSize()
		if region != nil {
			x := argInt(region, "x", 0)
			y := argInt(region, "y", 0)
			w := argInt(region, "w", 0)
			h := argInt(region, "h", 0)
			if w <= 0 || h <= 0 {
				return nil, failure("bad_request", "screenshot region requires positive w + h")
			}
			if err := robotgo.SaveCapture(tmpPath, x, y, w, h); err != nil {
				return nil, failure("screenshot_failed", err.Error())
			}
			logicalW, logicalH = w, h
		} else {
			if err := robotgo.SaveCapture(tmpPath); err != nil {
				return nil, failure("screenshot_failed", err.Error())
			}
		}

		rawCapture, err := os.ReadFile(tmpPath)
		if err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}

		// Decode to learn the true captured (physical) dimensions -- on
		// Retina/HiDPI hosts these exceed the logical size -- then apply
		// the downscale policy so the emitted image stays vision-friendly.
		decoded, _, err := image.Decode(bytes.NewReader(rawCapture))
		if err != nil {
			return nil, failure("screenshot_failed", "decode capture: "+err.Error())
		}
		img = decoded
	} else {
		// Secondary display (memql-cockpit#165): captured through
		// RobotGo's compositing path (robotgo.Capture) with the
		// display's virtual-desktop rect. That path emits logical
		// resolution -- captured == logical -- which mapperForDisplay
		// mirrors so mouse targets derived from this screenshot map
		// back correctly. A region is relative to the display and is
		// offset by its origin before capture.
		displays := enumerateDisplays()
		d, ok := findDisplay(displays, displayID)
		if !ok {
			return nil, failure("bad_request", fmt.Sprintf(
				"display %d unknown: display_info reports %d display(s)", displayID, len(displays)))
		}
		x, y, w, h := d.x, d.y, d.w, d.h
		if region != nil {
			rw := argInt(region, "w", 0)
			rh := argInt(region, "h", 0)
			if rw <= 0 || rh <= 0 {
				return nil, failure("bad_request", "screenshot region requires positive w + h")
			}
			x, y = d.x+argInt(region, "x", 0), d.y+argInt(region, "y", 0)
			w, h = rw, rh
		}
		captured, err := robotgo.Capture(x, y, w, h)
		if err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}
		img = captured
		logicalW, logicalH = w, h
	}

	capturedW, capturedH := img.Bounds().Dx(), img.Bounds().Dy()
	maxLongEdge := clampLongEdge(argInt(args, "maxLongEdge", defaultMaxLongEdge))
	emittedW, emittedH := FitWithin(capturedW, capturedH, maxLongEdge)
	if emittedW != capturedW || emittedH != capturedH {
		img = downscaleImage(img, emittedW, emittedH)
	}

	raw, err := encodeImage(img, format, argInt(args, "quality", 80))
	if err != nil {
		return nil, failure("screenshot_failed", "encode: "+err.Error())
	}

	scale := float64(emittedW) / float64(capturedW)
	encoded := base64.StdEncoding.EncodeToString(raw)
	preview := fmt.Sprintf("[%s] %dx%d (source %dx%d, scale %.3f), %d bytes",
		format, emittedW, emittedH, capturedW, capturedH, scale, len(raw))
	if displayID != 0 {
		preview += fmt.Sprintf(" display=%d", displayID)
	}
	payload := map[string]any{
		"format":        format,
		"display":       displayID,
		"width":         emittedW,
		"height":        emittedH,
		"sourceWidth":   capturedW,
		"sourceHeight":  capturedH,
		"scale":         scale,
		"logicalWidth":  logicalW,
		"logicalHeight": logicalH,
		"bytesBase64":   encoded,
		"sizeBytes":     len(raw),
	}
	if region != nil {
		payload["region"] = map[string]any{
			"x": argInt(region, "x", 0),
			"y": argInt(region, "y", 0),
			"w": argInt(region, "w", 0),
			"h": argInt(region, "h", 0),
		}
	}
	return successComputerJSON(payload, preview, len(raw)), nil
}

func guiCursorPosition(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x, y := robotgo.Location()
	return successComputerJSON(map[string]any{"x": x, "y": y}, fmt.Sprintf("(%d,%d)", x, y), 0), nil
}

// cursorLocation reports the live cursor position for the consent
// gate's strict-mode region exemption (memql-cockpit#131). The
// dispatcher calls it just before gating a workerComputer.mouse_click
// so an in-region click can skip the per-action approval modal.
// Always Known=true on the computer-use build; the headless stub returns false.
func cursorLocation() (x, y int, known bool) {
	x, y = robotgo.Location()
	return x, y, true
}

// liveMapper builds the coordinate mapper for the main display from
// live geometry plus the fixed downscale policy (defaultMaxLongEdge),
// per the contract in coords.go: model coords are in the emitted
// screenshot space, and the mapping to logical input coords is
// recomputed statelessly on every mouse action.
//
// Physical dims derive from robotgo.ScaleF() (2.0 on Retina macOS).
// Defensive: a non-positive scale is treated as 1, so on displays
// where capture dims equal logical dims the mapper is an identity
// (modulo the downscale policy).
func liveMapper() CoordinateMapper {
	logicalW, logicalH := robotgo.GetScreenSize()
	scale := robotgo.ScaleF()
	if scale <= 0 {
		scale = 1
	}
	capturedW := roundHalfAway(float64(logicalW) * scale)
	capturedH := roundHalfAway(float64(logicalH) * scale)
	emittedW, emittedH := FitWithin(capturedW, capturedH, defaultMaxLongEdge)
	return NewCoordinateMapper(logicalW, logicalH, capturedW, capturedH, emittedW, emittedH)
}

// displayGeometry describes one attached display in the
// virtual-desktop logical coordinate space: the primary display's
// top-left corner is (0,0), x grows right, y grows down, and
// secondary displays can sit at negative origins (left of / above
// the primary). scale is the pixel density a screenshot of this
// display captures at: the primary uses RobotGo's native capture
// (physical pixels, robotgo.ScaleF -- 2.0 on Retina macOS) while
// secondary displays are captured through the compositing path at
// logical resolution, so their capture scale is 1.0.
type displayGeometry struct {
	id      int
	x, y    int
	w, h    int
	scale   float64
	primary bool
}

// enumerateDisplays lists the attached displays via RobotGo
// (CGGetActiveDisplayList on macOS, Xinerama on Linux/X11). Display
// id 0 is always the primary (robotgo.GetDisplayBounds indexes the
// main display first on both platforms). Defensive: when enumeration
// fails or reports nothing usable -- e.g. Xinerama missing -- it
// falls back to a single primary display from robotgo.GetScreenSize,
// the exact pre-multi-display geometry, so callers never see an
// empty list.
func enumerateDisplays() []displayGeometry {
	primaryScale := robotgo.ScaleF()
	if primaryScale <= 0 {
		primaryScale = 1
	}
	n := robotgo.DisplaysNum()
	displays := make([]displayGeometry, 0, max(n, 1))
	for i := 0; i < n; i++ {
		x, y, w, h := robotgo.GetDisplayBounds(i)
		if w <= 0 || h <= 0 {
			// Enumeration glitch (display unplugged mid-call, Xinerama
			// gap): skip the degenerate entry rather than advertise a
			// rect no point can ever satisfy.
			continue
		}
		scale := 1.0
		if i == 0 {
			scale = primaryScale
		}
		displays = append(displays, displayGeometry{
			id: i, x: x, y: y, w: w, h: h, scale: scale, primary: i == 0,
		})
	}
	if len(displays) == 0 {
		w, h := robotgo.GetScreenSize()
		displays = append(displays, displayGeometry{
			id: 0, w: w, h: h, scale: primaryScale, primary: true,
		})
	}
	return displays
}

// displayRectsOf projects enumerated displays onto the pure
// DisplayRect form coords.go validates against.
func displayRectsOf(displays []displayGeometry) []DisplayRect {
	rects := make([]DisplayRect, 0, len(displays))
	for _, d := range displays {
		rects = append(rects, DisplayRect{ID: d.id, X: d.x, Y: d.y, W: d.w, H: d.h})
	}
	return rects
}

// findDisplay returns the enumerated display with the given id.
func findDisplay(displays []displayGeometry, id int) (displayGeometry, bool) {
	for _, d := range displays {
		if d.id == id {
			return d, true
		}
	}
	return displayGeometry{}, false
}

// mapperForDisplay builds the coordinate mapper for the display a
// mouse action targeted (the optional `display` arg; id 0 = primary,
// the default). Display 0 uses liveMapper -- the exact single-display
// geometry path -- so behavior without a display arg is unchanged.
// Secondary displays are captured at logical resolution (see
// displayGeometry.scale), so captured == logical there, and the
// mapper offsets by the display's virtual-desktop origin: emitted
// coords on display D map to D-local logical points plus D's origin.
func mapperForDisplay(displays []displayGeometry, displayID int) (CoordinateMapper, *memqlv1.Failure) {
	if displayID == 0 {
		return liveMapper(), nil
	}
	d, ok := findDisplay(displays, displayID)
	if !ok {
		return CoordinateMapper{}, failure("bad_request", fmt.Sprintf(
			"display %d unknown: display_info reports %d display(s)", displayID, len(displays)))
	}
	emittedW, emittedH := FitWithin(d.w, d.h, defaultMaxLongEdge)
	return NewCoordinateMapper(d.w, d.h, d.w, d.h, emittedW, emittedH).WithOrigin(d.x, d.y), nil
}

// guiMouseMove moves the cursor to (x, y) in the emitted-screenshot
// space of the targeted display (`display` arg, default 0 = primary
// -- see the multi-display contract in coords.go). The mapped logical
// point is re-validated against the union of display rects so a
// stale geometry can never move the cursor into a dead zone.
func guiMouseMove(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x := argInt(args, "x", -1)
	y := argInt(args, "y", -1)
	if x < 0 || y < 0 {
		return nil, failure("bad_request", "mouse_move: x and y are required and must be non-negative")
	}
	displayID := argInt(args, "display", 0)
	displays := enumerateDisplays()
	m, fail := mapperForDisplay(displays, displayID)
	if fail != nil {
		return nil, fail
	}
	if fail := validatePointInRect("mouse_move", x, y, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	lx, ly := m.ToLogical(x, y)
	if fail := validatePointInDisplays("mouse_move", lx, ly, displayRectsOf(displays)); fail != nil {
		return nil, fail
	}
	smooth := argBool(args, "smooth", false)
	if smooth {
		robotgo.MoveSmooth(lx, ly)
	} else {
		robotgo.Move(lx, ly)
	}
	return successComputerJSON(map[string]any{
		"x": x, "y": y,
		"display":  displayID,
		"logicalX": lx, "logicalY": ly,
		"smooth": smooth,
	}, "moved", 0), nil
}

// modifierKeyAliases maps the bff tool schema's modifier vocabulary
// (modifiers: [cmd|shift|alt|ctrl]) onto RobotGo key names, plus the
// common cross-platform aliases a model is likely to emit. Every value
// is a name RobotGo's KeyDown/KeyUp accept directly (see robotgo
// key.go: cmd == META, the "win" key on Windows; alt == Option on
// macOS; ctrl == control). Resolving to the OS-correct physical key is
// RobotGo's job -- holding "cmd" presses Command on macOS and the
// Super/Win key elsewhere, matching how the existing key_combo /
// key_hold handlers pass these names straight through.
var modifierKeyAliases = map[string]string{
	"cmd":     "cmd",
	"command": "cmd",
	"meta":    "cmd",
	"super":   "cmd",
	"win":     "cmd",
	"windows": "cmd",
	"shift":   "shift",
	"alt":     "alt",
	"option":  "alt",
	"opt":     "alt",
	"ctrl":    "ctrl",
	"control": "ctrl",
}

// mouseClickModifiers validates and canonicalizes the optional
// mouse_click `modifiers` arg into the RobotGo key names to hold around
// the click. The arg is a string array (e.g. ["cmd"], ["shift","alt"]);
// absent / empty yields no modifiers. An unknown name is a structured
// bad_request rather than a silent no-op -- the whole point of #184 is
// that a requested modifier must take effect or fail loudly. Order is
// preserved and duplicates are dropped so a press has exactly one
// matching release.
//
// This is the pure (pre-RobotGo) half so it can be unit-tested on any
// build; guiMouseClick performs the actual KeyDown/KeyUp.
func mouseClickModifiers(args map[string]any) ([]string, *memqlv1.Failure) {
	raw := argStringList(args, "modifiers")
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, m := range raw {
		key, ok := modifierKeyAliases[strings.ToLower(strings.TrimSpace(m))]
		if !ok {
			return nil, failure("bad_request", fmt.Sprintf(
				"mouse_click: modifier %q not supported (use cmd, shift, alt, or ctrl)", m))
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}

func guiMouseClick(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button := strings.ToLower(strings.TrimSpace(argString(args, "button")))
	if button == "" {
		button = "left"
	}
	// count drives single / double / triple; the legacy `double` flag
	// is honored as count=2 for back-compat. Clamped to [1, 3] --
	// nothing in the UI vocabulary needs more than a triple-click.
	count := clampInt(argInt(args, "count", 1), 1, 3)
	if argBool(args, "double", false) && count < 2 {
		count = 2
	}
	// modifiers (cmd/shift/alt/ctrl) are held DOWN around the click so a
	// model-requested cmd-click / shift-click composes into a real chord
	// instead of degrading to a plain click (memql-cockpit#184). The
	// bff tool schema has always advertised this arg; it was a silent
	// no-op until now. Strict-mode classification is unchanged: this is
	// still mouse_click, already in the high-risk subset (consent.go
	// isHighRiskAction), and the held-modifier-becomes-a-chord risk is
	// exactly why that subset exists.
	modifiers, fail := mouseClickModifiers(args)
	if fail != nil {
		return nil, fail
	}

	// Press modifiers in order, then ALWAYS release (LIFO) -- even if a
	// press or the click itself fails -- so a partial failure can never
	// leave a key stuck down and silently chord every later action.
	var pressed []string
	releaseModifiers := func() *memqlv1.Failure {
		var relFail *memqlv1.Failure
		for i := len(pressed) - 1; i >= 0; i-- {
			if err := robotgo.KeyUp(pressed[i]); err != nil && relFail == nil {
				relFail = failure("mouse_click_failed",
					fmt.Sprintf("release modifier %q (key may be stuck down): %v", pressed[i], err))
			}
		}
		return relFail
	}
	for _, mod := range modifiers {
		if err := robotgo.KeyDown(mod); err != nil {
			releaseModifiers()
			return nil, failure("mouse_click_failed", fmt.Sprintf("press modifier %q: %v", mod, err))
		}
		pressed = append(pressed, mod)
	}

	var err error
	switch count {
	case 2:
		err = robotgo.Click(button, true)
	case 3:
		// robotgo.Click only knows single/double. MultiClick(.., 3)
		// posts a proper clickCount=3 event sequence on macOS
		// (CGEventSetIntegerValueField under the hood) and three
		// rapid clicks elsewhere, which X11 coalesces into a triple.
		err = robotgo.MultiClick(button, 3)
	default:
		err = robotgo.Click(button, false)
	}
	releaseFail := releaseModifiers()
	if err != nil {
		return nil, failure("mouse_click_failed", err.Error())
	}
	if releaseFail != nil {
		return nil, releaseFail
	}
	payload := map[string]any{
		"button": button,
		"count":  count,
		"double": count >= 2, // legacy field, kept for payload back-compat
	}
	if len(modifiers) > 0 {
		payload["modifiers"] = modifiers
	}
	return successComputerJSON(payload, "clicked", 0), nil
}

// guiMouseDown / guiMouseUp post a bare button-state transition
// (memql-cockpit#166) for press-and-hold interactions the composite
// mouse_click / mouse_drag can't express (e.g. hold-to-reveal menus,
// custom drag handles needing intermediate moves). Deliberately
// STATELESS: mouse_up succeeds even with no preceding mouse_down --
// the OS treats a redundant button-up as a no-op, and tracking
// pairing here would just desync from reality on any missed event.
func guiMouseDown(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button, fail := mouseButtonArg(args, "mouse_down")
	if fail != nil {
		return nil, fail
	}
	if err := robotgo.MouseDown(button); err != nil {
		return nil, failure("mouse_down_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"button": button}, "mouse down", 0), nil
}

func guiMouseUp(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button, fail := mouseButtonArg(args, "mouse_up")
	if fail != nil {
		return nil, fail
	}
	if err := robotgo.MouseUp(button); err != nil {
		return nil, failure("mouse_up_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"button": button}, "mouse up", 0), nil
}

// mouseButtonArg validates the {button} argument for mouse_down /
// mouse_up. Vocabulary is left / right / middle (defaulting to
// left); "middle" maps to robotgo's "center" -- robotgo's CheckMouse
// silently falls back to LEFT_BUTTON for unknown names, so an
// unvalidated typo would click the wrong button instead of erroring.
func mouseButtonArg(args map[string]any, action string) (string, *memqlv1.Failure) {
	button := strings.ToLower(strings.TrimSpace(argString(args, "button")))
	switch button {
	case "", "left":
		return "left", nil
	case "right":
		return "right", nil
	case "middle", "center":
		return "center", nil
	}
	return "", failure("bad_request", fmt.Sprintf("%s: button %q not supported (use left, right, or middle)", action, button))
}

// guiMouseDrag drags from/to points in the emitted-screenshot space
// of the targeted display (`display` arg, default 0 = primary -- see
// the multi-display contract in coords.go). Both endpoints live on
// the SAME display: cross-display drags are not expressible and that
// is deliberate -- the drag path RobotGo takes across mismatched
// scale factors is undefined.
func guiMouseDrag(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	fromX := argInt(args, "fromX", -1)
	fromY := argInt(args, "fromY", -1)
	toX := argInt(args, "toX", -1)
	toY := argInt(args, "toY", -1)
	if fromX < 0 || fromY < 0 || toX < 0 || toY < 0 {
		return nil, failure("bad_request", "mouse_drag: fromX, fromY, toX, toY all required")
	}
	displayID := argInt(args, "display", 0)
	displays := enumerateDisplays()
	m, fail := mapperForDisplay(displays, displayID)
	if fail != nil {
		return nil, fail
	}
	if fail := validatePointInRect("mouse_drag from", fromX, fromY, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	if fail := validatePointInRect("mouse_drag to", toX, toY, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	lFromX, lFromY := m.ToLogical(fromX, fromY)
	lToX, lToY := m.ToLogical(toX, toY)
	rects := displayRectsOf(displays)
	if fail := validatePointInDisplays("mouse_drag from", lFromX, lFromY, rects); fail != nil {
		return nil, fail
	}
	if fail := validatePointInDisplays("mouse_drag to", lToX, lToY, rects); fail != nil {
		return nil, fail
	}
	robotgo.Move(lFromX, lFromY)
	robotgo.MilliSleep(50)
	robotgo.DragSmooth(lToX, lToY)
	return successComputerJSON(map[string]any{
		"fromX": fromX, "fromY": fromY,
		"toX": toX, "toY": toY,
		"display":      displayID,
		"logicalFromX": lFromX, "logicalFromY": lFromY,
		"logicalToX": lToX, "logicalToY": lToY,
	}, "dragged", 0), nil
}

// guiMouseScroll takes wheel-tick deltas (dx/dy), not screen
// coordinates, so the screenshot-space -> logical mapping that
// applies to mouse_move / mouse_drag targets does not apply here.
func guiMouseScroll(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	dx := argInt(args, "dx", 0)
	dy := argInt(args, "dy", 0)
	if dx == 0 && dy == 0 {
		return nil, failure("bad_request", "mouse_scroll: at least one of dx / dy must be non-zero")
	}
	robotgo.Scroll(dx, dy)
	return successComputerJSON(map[string]any{"dx": dx, "dy": dy}, "scrolled", 0), nil
}

func guiKeyType(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	text := argString(args, "text")
	if text == "" {
		return nil, failure("bad_request", "key_type: text required")
	}
	robotgo.TypeStr(text)
	// The typed text often includes credentials (the agent will
	// drive `key_type(text="<password>\n")` against a password
	// field). The dispatcher's audit log records the success preview
	// verbatim, which means the password would otherwise land in the
	// log shipper as cleartext. We deliberately omit the typed text
	// from both the result payload and the preview; the length is
	// preserved as a length-only indicator. If a downstream consumer
	// needs the typed value (e.g. a record-and-replay tool), it must
	// supply the value itself and not depend on read-back.
	return successComputerJSON(
		map[string]any{"chars": len(text), "text_redacted": true},
		fmt.Sprintf("typed %d chars (text redacted)", len(text)),
		len(text),
	), nil
}

func guiKeyCombo(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	keys := argStringList(args, "keys")
	if len(keys) == 0 {
		return nil, failure("bad_request", "key_combo: keys array required")
	}
	primary := keys[0]
	mods := make([]interface{}, 0, len(keys)-1)
	for _, k := range keys[1:] {
		mods = append(mods, k)
	}
	if err := robotgo.KeyTap(primary, mods...); err != nil {
		return nil, failure("key_combo_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"keys": keys}, "key combo", 0), nil
}

// keyHoldMaxMs bounds workerComputer.key_hold so a single dispatch
// can't pin a key (and the worker's call slot) indefinitely.
const keyHoldMaxMs = 10000

// guiKeyHold presses a key, holds it for durationMs, and releases it
// (memql-cockpit#166) -- the press-and-hold primitive key_press /
// key_combo can't express (games, OS-level hold gestures, key-repeat
// driven UIs). durationMs clamps into [1, keyHoldMaxMs]; absent
// clamps up from 0 to the 1ms floor (an instantaneous down/up).
//
// robotgo v1.0.2: KeyDown(key) == KeyToggle(key) (default direction
// "down") and KeyUp(key) == KeyToggle(key, "up") -- both return the
// underlying toggle error, so this is the dependable down/sleep/up
// path. On a failed release we still report key_hold_failed and name
// the release stage: a stuck key is operator-visible information.
func guiKeyHold(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	key := strings.TrimSpace(argString(args, "key"))
	if key == "" {
		return nil, failure("bad_request", "key_hold: key required")
	}
	durationMs := clampInt(argInt(args, "durationMs", 0), 1, keyHoldMaxMs)
	if err := robotgo.KeyDown(key); err != nil {
		return nil, failure("key_hold_failed", "press: "+err.Error())
	}
	robotgo.MilliSleep(durationMs)
	if err := robotgo.KeyUp(key); err != nil {
		return nil, failure("key_hold_failed", "release (key may be stuck down): "+err.Error())
	}
	return successComputerJSON(map[string]any{
		"key":        key,
		"durationMs": durationMs,
	}, fmt.Sprintf("held %q for %dms", key, durationMs), 0), nil
}

// guiDisplayInfo reports every attached display in the
// virtual-desktop logical coordinate space (memql-cockpit#165):
// {displays: [{id, x, y, width, height, scale, primary}], primary:
// <id>}. The top-level width/height remain the primary display's
// logical size for continuity with callers that predate the
// multi-display shape.
func guiDisplayInfo(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	width, height := robotgo.GetScreenSize()
	displays := enumerateDisplays()
	primaryID := 0
	list := make([]map[string]any, 0, len(displays))
	for _, d := range displays {
		if d.primary {
			primaryID = d.id
		}
		list = append(list, map[string]any{
			"id":      d.id,
			"x":       d.x,
			"y":       d.y,
			"width":   d.w,
			"height":  d.h,
			"scale":   d.scale,
			"primary": d.primary,
		})
	}
	return successComputerJSON(map[string]any{
		"width":    width,
		"height":   height,
		"displays": list,
		"primary":  primaryID,
	}, fmt.Sprintf("%dx%d, %d display(s)", width, height, len(displays)), 0), nil
}

// guiWindowList / guiWindowFocus live in window_computeruse.go with their
// platform backends in window_gui_{darwin,linux,other}.go
// (memql-cockpit#167).

// successComputerJSON moved to capabilities.go (untagged) so the
// build-agnostic capabilities handler can share it.

// argBool extracts a boolean argument with a default. The args map
// may carry the value as a real bool (parsed JSON) or a string
// ("true" / "false") depending on the caller.
func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true")
	}
	return def
}
