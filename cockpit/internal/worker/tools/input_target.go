package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A screenshot is an observation, not permission. These one-action receipts
// additionally bind existing consent to the observed application, AX window,
// focused element and exact image geometry. The backend must address input to
// that process: a check followed by a global event has a cross-application race.
const inputTargetLifetime = time.Minute

type inputFocus interface {
	window() WindowInfo
	same(inputFocus, bool) bool
	close()
}

type inputTargetBackend interface {
	focus() (inputFocus, error)
	emit(inputFocus, targetInputEvent) error
}

type targetInputEvent struct {
	kind, text, key, button string
	modifiers               []string
	x, y, dx, dy            int
	down, release           bool
}

type inputScope struct {
	dispatcher *Dispatcher
	run        string
}
type inputScopeContextKey struct{}

type inputFrame struct {
	id       string
	scope    inputScope
	focus    inputFocus
	mapper   CoordinateMapper
	expires  time.Time
	used     bool
	activity *inputActivity
}

// One process-wide lock serializes desktop transactions across enrollments.
// A detected focus change latches the desktop off until the operator restarts
// the worker. New screenshots, tokens, runs and window_focus cannot clear it.
// Read-only screenshots remain available. No application name is special-cased.
type inputTargetGuard struct {
	mu                 sync.Mutex
	frame              *inputFrame
	selection          *windowSelection
	anchor             inputFocus
	halted             bool
	backend            inputTargetBackend
	now                func() time.Time
	anchorActivity     *inputActivity
	pendingObservation bool
	pendingScope       inputScope
	pendingUntil       time.Time
	firstInterruption  *InputInterruption
}

var desktopInputGuard = &inputTargetGuard{now: time.Now}

var desktopRuntimeDisabled atomic.Bool

// DisableDesktopRuntime isolates a failed GUI startup from inference and host
// work. It is one-way for this process; no later call can re-enable input.
func DisableDesktopRuntime() {
	desktopRuntimeDisabled.Store(true)
	desktopInputGuard.mu.Lock()
	desktopInputGuard.stop("runtime", "startup", "desktop_runtime_disabled")
	desktopInputGuard.mu.Unlock()
}

// InputTargetStatus is an explicit local diagnostic, not an input receipt or
// permission. It contains no element contents, application titles or tokens.
type InputTargetStatus struct {
	Available                  bool               `json:"available"`
	Bound                      bool               `json:"bound"`
	Halted                     bool               `json:"halted"`
	PostEventAccess            *bool              `json:"post_event_access,omitempty"`
	PointerCoordinateAccessors *bool              `json:"pointer_coordinate_accessors,omitempty"`
	WindowID                   uint64             `json:"observed_window_id,omitempty"`
	PID                        int                `json:"observed_pid,omitempty"`
	Error                      string             `json:"error,omitempty"`
	AcquisitionMilliseconds    int64              `json:"acquisition_milliseconds"`
	DiagnosticProbes           []InputTargetProbe `json:"diagnostic_probes,omitempty"`
	PendingObservation         bool               `json:"pending_observation"`
	FirstInterruption          *InputInterruption `json:"first_interruption,omitempty"`
}

// InputTargetProbe is observation only; no diagnostic path grants input.
type InputTargetProbe struct {
	Method              string `json:"method"`
	Available           bool   `json:"available"`
	PID                 int    `json:"observed_pid,omitempty"`
	WindowID            uint64 `json:"observed_window_id,omitempty"`
	ElapsedMilliseconds int64  `json:"elapsed_milliseconds"`
	Error               string `json:"error,omitempty"`
	MainLoopRunning     bool   `json:"main_loop_running"`
	MainLoopTurns       uint64 `json:"main_loop_turns"`
}

// DesktopInputTargetStatus reads from the actual worker process so its TCC
// identity is used. It never activates an app, posts input or changes the guard.
func DesktopInputTargetStatus() InputTargetStatus { return desktopInputGuard.targetStatus() }

func (g *inputTargetGuard) targetStatus() InputTargetStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	status := InputTargetStatus{Bound: g.anchor != nil, Halted: g.halted, PostEventAccess: g.postingAccess(), PointerCoordinateAccessors: g.pointerCoordinateAccessors()}
	status.PendingObservation, status.FirstInterruption = g.pendingObservation, g.firstInterruption
	if g.backend == nil {
		status.Error = "No target-addressed input backend is available."
		return status
	}
	started := time.Now()
	focus, err := g.backend.focus()
	status.AcquisitionMilliseconds = time.Since(started).Milliseconds()
	if diagnostic, ok := g.backend.(interface{ diagnosticProbes() []InputTargetProbe }); ok {
		status.DiagnosticProbes = diagnostic.diagnosticProbes()
	}
	if err != nil {
		status.Error = err.Error()
		return status
	}
	defer focus.close()
	w := focus.window()
	status.Available, status.WindowID, status.PID = true, w.ID, w.PID
	return status
}

// Passive and content-free. AX inspection permission is distinct from permission
// to synthesize events. Nil means this backend cannot report the latter.
func (g *inputTargetGuard) postingAccess() *bool {
	if b, ok := g.backend.(interface{ canPostEvents() bool }); ok {
		allowed := b.canPostEvents()
		return &allowed
	}
	return nil
}

// Reports accessor availability only. Each pointer packet must independently
// round-trip its exact window identity and global/window-local coordinates.
func (g *inputTargetGuard) pointerCoordinateAccessors() *bool {
	if b, ok := g.backend.(interface{ pointerCoordinateAccessors() bool }); ok {
		available := b.pointerCoordinateAccessors()
		return &available
	}
	return nil
}

func (g *inputTargetGuard) clearFrame() {
	if g.frame != nil {
		g.frame.focus.close()
		g.frame = nil
	}
}

func (g *inputTargetGuard) interrupted() *memqlv1.Failure {
	return failure("input_interrupted", "Desktop target can no longer be verified. No automatic retry or focus activation is allowed. Desktop input stays stopped until the operator reviews the interruption and restarts the worker.")
}

func inputOwner(ctx context.Context) (inputScope, bool) {
	s, ok := ctx.Value(inputScopeContextKey{}).(inputScope)
	return s, ok && s.dispatcher != nil && s.run != ""
}

func (g *inputTargetGuard) screenshot(ctx context.Context, args map[string]any, capture func(map[string]any) (*memqlv1.Success, *memqlv1.Failure)) (*memqlv1.Success, *memqlv1.Failure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	g.clearFrame() // Every new observation invalidates any earlier receipt, but not the focus anchor.
	owner, _ := inputOwner(ctx)
	transition := g.canObserveTransition(owner)
	if _, requested := args["windowId"]; !requested {
		// Read-only screenshots still observe interruptions when a target was
		// established. They never erase its anchor or silently re-acquire it.
		observe := func() {
			if g.anchor == nil || g.halted {
				return
			}
			if g.backend == nil {
				g.stop("screenshot", "read_only", "backend_unavailable")
				return
			}
			live, err := g.backend.focus()
			if err != nil {
				g.stopFocus("screenshot", "read_only", err)
				return
			}
			defer live.close()
			if !g.undisturbed(g.anchorActivity) {
				g.stop("screenshot", "read_only", "human_activity_or_activation")
			} else if !g.anchor.same(live, !transition) {
				g.stop("screenshot", "read_only", focusDifference(g.anchor, live))
			}
		}
		observe()
		shot, fail := capture(args)
		observe()
		if shot != nil && g.halted {
			var payload map[string]any
			if json.Unmarshal(shot.GetResultJson(), &payload) == nil && payload != nil {
				payload["desktopInputStopped"] = true
				payload["inputInstructions"] = g.interrupted().GetErrorMessage()
				shot.ResultJson, _ = json.Marshal(payload)
			}
		}
		return shot, fail
	}
	if g.halted {
		return nil, g.interrupted()
	}
	owner, ok := inputOwner(ctx)
	if !ok {
		return nil, failure("input_target_required", "A screenshot-bound input target requires an owned run.")
	}
	id, refusal := parseWindowID(args)
	if refusal != nil {
		return nil, refusal
	}
	if g.backend == nil {
		return nil, failure("targeted_input_unavailable", "This platform has no verified target-addressed input backend. Read-only screenshots are available without windowId.")
	}
	activity := g.activity()
	if _, supported := g.backend.(inputActivityBackend); supported && activity == nil {
		if g.anchor != nil {
			g.stop("screenshot", "before_capture", "activity_monitor_unavailable_or_input_held")
			return nil, g.interrupted()
		}
		return nil, failure("input_activity_unavailable", "Cannot establish an undisturbed input baseline. No receipt was granted.")
	}
	if g.anchorActivity != nil && !g.undisturbed(g.anchorActivity) {
		g.stop("screenshot", "before_capture", "human_activity_or_activation")
		return nil, g.interrupted()
	}
	before, err := g.backend.focus()
	if err != nil {
		if g.anchor != nil {
			g.stopFocus("screenshot", "before_capture", err)
			return nil, g.interrupted()
		}
		return nil, failure("input_focus_unavailable", err.Error())
	}
	keep := false
	defer func() {
		if !keep {
			before.close()
		}
	}()
	if before.window().ID != id {
		g.stop("screenshot", "before_capture", "requested_window_changed")
		return nil, g.interrupted()
	}
	if g.anchor != nil && !g.anchor.same(before, !transition) {
		g.stop("screenshot", "before_capture", focusDifference(g.anchor, before))
		return nil, g.interrupted()
	}
	shot, refusal := capture(args)
	if refusal != nil || shot == nil {
		return shot, refusal
	}
	after, err := g.backend.focus()
	if err != nil {
		g.stopFocus("screenshot", "after_capture", err)
		return nil, g.interrupted()
	}
	same := before.same(after, true)
	if !g.undisturbed(activity) {
		after.close()
		g.stop("screenshot", "after_capture", "human_activity_or_activation")
		return nil, g.interrupted()
	}
	if transition && !g.canObserveTransition(owner) {
		after.close()
		g.stop("screenshot", "after_capture", "transition_observation_expired_or_interrupted")
		return nil, g.interrupted()
	}
	if same {
		if g.anchor != nil {
			g.anchor.close()
		}
		g.anchor = after
		g.anchorActivity = activity
		g.pendingObservation = false
	} else {
		after.close()
	}
	if !same {
		g.stop("screenshot", "after_capture", "focus_changed_during_capture")
		return nil, g.interrupted()
	}
	var payload map[string]any
	if err := json.Unmarshal(shot.GetResultJson(), &payload); err != nil {
		return nil, failure("input_target_unavailable", "Screenshot geometry is unavailable.")
	}
	mapper := NewCoordinateMapper(argInt(payload, "logicalWidth", 0), argInt(payload, "logicalHeight", 0), argInt(payload, "sourceWidth", 0), argInt(payload, "sourceHeight", 0), argInt(payload, "width", 0), argInt(payload, "height", 0)).WithOrigin(argInt(payload, "logicalOriginX", 0), argInt(payload, "logicalOriginY", 0))
	for _, field := range []string{"logicalWidth", "logicalHeight", "width", "height", "sourceWidth", "sourceHeight"} {
		if argInt(payload, field, 0) <= 0 {
			return nil, failure("input_target_unavailable", "Screenshot geometry is incomplete.")
		}
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, failure("input_target_unavailable", "Cannot create an input receipt.")
	}
	g.clearFrame()
	g.frame = &inputFrame{id: hex.EncodeToString(nonce[:]), scope: owner, focus: before, mapper: mapper, expires: g.now().Add(inputTargetLifetime), activity: activity}
	keep = true
	w := before.window()
	payload["inputTarget"] = map[string]any{"id": g.frame.id, "windowId": w.ID, "pid": w.PID, "expiresAt": g.frame.expires.UTC(), "singleUse": true}
	payload["inputInstructions"] = "Pass inputTarget.id as inputTarget on exactly one input action. Mouse actions require x and y from this image. Take a new screenshot with the same intended windowId before the next action. A focus interruption cannot be retried automatically."
	shot.ResultJson, _ = json.Marshal(payload)
	return shot, nil
}

func (g *inputTargetGuard) input(ctx context.Context, action string, args map[string]any) (success *memqlv1.Success, fail *memqlv1.Failure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.halted {
		return nil, g.interrupted()
	}
	if action == "window_focus" {
		return g.selectWindow(ctx, args)
	}
	owner, ok := inputOwner(ctx)
	f := g.frame
	if !ok || f == nil || f.scope != owner || f.used || argString(args, "inputTarget") != f.id || !g.now().Before(f.expires) {
		return nil, failure("input_target_required", "Input requires a fresh, unused screenshot inputTarget.id for this run. Request screenshot with the intended windowId; do not retry this input blindly.")
	}
	f.used = true // failures consume the receipt too; no blind retries
	if access := g.postingAccess(); access != nil && !*access {
		return nil, failure("input_posting_unavailable", "The operating system's passive event-posting preflight refused input. AX focus access alone does not authorize posting. No input or permission prompt was sent.")
	}
	if err := ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	events, refusal := planTargetInput(action, args, f.mapper, f.focus.window())
	if refusal != nil {
		return nil, refusal
	}
	check := func(element bool) bool {
		if ctx.Err() != nil || !g.now().Before(f.expires) {
			g.stop(action, "transaction", "cancelled_or_expired")
			return false
		}
		if !g.undisturbed(f.activity) {
			g.stop(action, "transaction", "human_activity_or_activation")
			return false
		}
		live, err := g.backend.focus()
		if err != nil {
			g.stopFocus(action, "transaction", err)
			return false
		}
		defer live.close()
		if !f.focus.same(live, element) {
			g.stop(action, "transaction", focusDifference(f.focus, live))
			return false
		}
		if ctx.Err() != nil || !g.now().Before(f.expires) {
			g.stop(action, "transaction", "cancelled_or_expired")
			return false
		}
		return true
	}
	if !check(true) {
		g.stop(action, "before_transaction", "target_unverified")
		return nil, g.interrupted()
	}
	// Attempt release only to the original PID after interruption. A failed
	// cleanup must be reported; no release is sent to the new foreground.
	var release *targetInputEvent
	defer func() {
		if release != nil {
			if err := g.backend.emit(f.focus, *release); err != nil {
				g.stop(action, "release", "release_failed")
				prior := ""
				if fail != nil {
					prior = fail.GetErrorMessage() + " "
				}
				success = nil
				fail = failure("input_interrupted", prior+"Original-target release failed; a key/button may remain down. Operator review required: "+err.Error())
			}
		}
	}()
	emitted := 0
	for _, event := range events {
		if !check((action == "key_type" || action == "key_combo") && !event.release) {
			g.stop(action, "transaction", "target_unverified")
			return nil, inputInterruptedAfter(emitted)
		}
		if event.down {
			up := event
			up.down, up.release = false, true
			release = &up
		}
		if err := g.backend.emit(f.focus, event); err != nil {
			g.stop(action, "emit", "delivery_failed")
			return nil, failure("input_interrupted", fmt.Sprintf("Target-addressed input failed after %d events; no retry: %v", emitted, err))
		}
		emitted++
		if event.release {
			release = nil
		}
	}
	// Observe the effect of this transaction. A field change caused by a click
	// or Tab is allowed here; a different window is an interruption. This is
	// sampled enforcement, not attribution of who caused a focus change.
	after, err := g.backend.focus()
	if err != nil {
		g.stopFocus(action, "after_transaction", err)
		return nil, inputInterruptedAfter(emitted)
	}
	if !g.undisturbed(f.activity) {
		after.close()
		g.stop(action, "after_transaction", "human_activity_or_activation")
		return nil, inputInterruptedAfter(emitted)
	}
	transition := f.activity != nil && (action == "key_combo" || action == "mouse_click")
	if !f.focus.same(after, !(transition || inputMayChangeField(action, args))) {
		reason := focusDifference(f.focus, after)
		after.close()
		g.stop(action, "after_transaction", reason)
		return nil, inputInterruptedAfter(emitted)
	}
	if g.anchor != nil {
		g.anchor.close()
	}
	g.anchor = after
	g.anchorActivity = f.activity
	// This observation-only lifetime starts after the gesture. The spent input
	// receipt is never renewed, including when the gesture finished near expiry.
	g.pendingObservation, g.pendingScope, g.pendingUntil = transition, f.scope, g.now().Add(inputTargetLifetime)
	return successComputerJSON(map[string]any{"targetWindowId": f.focus.window().ID, "targetPid": f.focus.window().PID, "eventsSent": emitted, "text_redacted": true, "requiresFreshScreenshot": true}, "input posted to target process; inspect a fresh screenshot before continuing", 0), nil
}

func planTargetInput(action string, args map[string]any, mapper CoordinateMapper, w WindowInfo) ([]targetInputEvent, *memqlv1.Failure) {
	allowed := map[string]bool{"inputTarget": true}
	for _, key := range map[string][]string{"key_type": {"text"}, "key_combo": {"keys"}, "mouse_move": {"x", "y"}, "mouse_click": {"x", "y", "button", "modifiers"}, "mouse_scroll": {"x", "y", "dx", "dy"}}[action] {
		allowed[key] = true
	}
	for key := range args {
		if !allowed[key] {
			return nil, failure("bad_request", "Unsupported argument for target-addressed "+action+": "+key)
		}
	}
	switch action {
	case "key_type":
		text := argString(args, "text")
		if text == "" || len(text) > 4096 || !utf8.ValidString(text) {
			return nil, failure("bad_request", "key_type requires 1..4096 UTF-8 bytes; inspect a fresh screenshot between longer chunks.")
		}
		events := make([]targetInputEvent, 0, len(text))
		for _, r := range text {
			if !unicode.IsPrint(r) {
				return nil, failure("bad_request", "key_type accepts printable text only; submit or navigate with a separate key_combo after a fresh screenshot.")
			}
			events = append(events, targetInputEvent{kind: "text", text: string(r), down: true}, targetInputEvent{kind: "text", text: string(r), release: true})
		}
		return events, nil
	case "key_combo":
		keys, listOK := targetStringList(args, "keys")
		if !listOK {
			return nil, failure("bad_request", "keys must be a string array.")
		}
		if len(keys) == 0 {
			return nil, failure("bad_request", "key_combo requires keys, primary key first.")
		}
		if _, ok := targetKeyCode(keys[0]); !ok {
			return nil, failure("bad_request", "Unsupported primary key. key_combo requires the primary key first, then modifiers: for example keys=[\"r\",\"cmd\"] or keys=[\"enter\"].")
		}
		mods, refusal := targetModifiers(keys[1:])
		if refusal != nil {
			return nil, refusal
		}
		return []targetInputEvent{{kind: "key", key: keys[0], modifiers: mods, down: true}, {kind: "key", key: keys[0], modifiers: mods, release: true}}, nil
	case "mouse_move", "mouse_click", "mouse_scroll":
		x, xOK := args["x"]
		y, yOK := args["y"]
		if !xOK || !yOK {
			return nil, failure("bad_request", action+" requires explicit x and y in the bound screenshot; the live global cursor is not a target.")
		}
		px, pxOK := strictInputInt(x)
		py, pyOK := strictInputInt(y)
		if !pxOK || !pyOK {
			return nil, failure("bad_request", "Pointer coordinates must be exact integers.")
		}
		if refusal := validatePointInRect(action, px, py, mapper.EmittedW, mapper.EmittedH); refusal != nil {
			return nil, refusal
		}
		lx, ly := mapper.ToLogical(px, py)
		if !(DisplayRect{X: w.X, Y: w.Y, W: w.Width, H: w.Height}).contains(lx, ly) {
			return nil, failure("input_target_bounds", "The point is outside the observed target window.")
		}
		if action == "mouse_move" {
			return []targetInputEvent{{kind: "move", x: lx, y: ly}}, nil
		}
		if action == "mouse_scroll" {
			dx, dy := 0, 0
			var valid bool
			if v, ok := args["dx"]; ok {
				dx, valid = strictInputInt(v)
				if !valid {
					return nil, failure("bad_request", "dx must be an integer.")
				}
			}
			if v, ok := args["dy"]; ok {
				dy, valid = strictInputInt(v)
				if !valid {
					return nil, failure("bad_request", "dy must be an integer.")
				}
			}
			if dx < -1000 || dx > 1000 || dy < -1000 || dy > 1000 {
				return nil, failure("bad_request", "Scroll deltas must be in [-1000,1000].")
			}
			if dx == 0 && dy == 0 {
				return nil, failure("bad_request", "mouse_scroll requires dx or dy.")
			}
			return []targetInputEvent{{kind: "scroll", x: lx, y: ly, dx: dx, dy: dy}}, nil
		}
		button := argString(args, "button")
		if button == "" {
			button = "left"
		}
		if button != "left" && button != "right" && button != "middle" {
			return nil, failure("bad_request", "mouse_click button must be left, right or middle.")
		}
		values, valid := targetStringList(args, "modifiers")
		if !valid {
			return nil, failure("bad_request", "modifiers must be a string array.")
		}
		mods, refusal := targetModifiers(values)
		if refusal != nil {
			return nil, refusal
		}
		return []targetInputEvent{{kind: "mouse", button: button, modifiers: mods, x: lx, y: ly, down: true}, {kind: "mouse", button: button, modifiers: mods, x: lx, y: ly, release: true}}, nil
	default:
		return nil, failure("targeted_input_unavailable", action+" has no safe target-addressed transaction in this build. No global-input fallback is permitted.")
	}
}

func targetModifiers(values []string) ([]string, *memqlv1.Failure) {
	mods := make([]string, 0, len(values))
	for _, value := range values {
		switch value {
		case "cmd", "command":
			mods = append(mods, "cmd")
		case "ctrl", "control":
			mods = append(mods, "ctrl")
		case "alt", "option":
			mods = append(mods, "alt")
		case "shift":
			mods = append(mods, "shift")
		default:
			return nil, failure("bad_request", "Unknown input modifier; expected cmd, ctrl, alt or shift.")
		}
	}
	return mods, nil
}

func strictInputInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= -2147483648 && n <= 2147483647 && n == math.Trunc(n) {
			return int(n), true
		}
	}
	return 0, false
}

// macOS virtual key codes. Character shortcuts use the ANSI physical key
// positions; printable text uses Unicode events instead, independent of layout.
func targetKeyCode(key string) (uint16, bool) {
	codes := map[string]uint16{
		"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9, "b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17,
		"1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26, "-": 27, "8": 28, "0": 29,
		"]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35, "enter": 36, "return": 36, "l": 37, "j": 38, "'": 39, "k": 40, ";": 41, "\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47,
		"tab": 48, "space": 49, "`": 50, "backspace": 51, "escape": 53, "esc": 53,
		"home": 115, "pageup": 116, "delete": 117, "end": 119, "pagedown": 121, "left": 123, "right": 124, "down": 125, "up": 126,
		"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
	}
	code, ok := codes[key]
	return code, ok
}

func targetStringList(args map[string]any, name string) ([]string, bool) {
	value, exists := args[name]
	if !exists {
		return nil, true
	}
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		result = append(result, s)
	}
	return result, true
}

func inputInterruptedAfter(emitted int) *memqlv1.Failure {
	return failure("input_interrupted", fmt.Sprintf("Desktop target or transaction lifetime changed after %d target-addressed events. The sequence stopped; do not retry or assume it had no effect. Operator review and worker restart are required.", emitted))
}

// Only these explicit navigation transactions may adopt an observed new field.
// Ordinary text, pointer movement and arbitrary shortcuts preserve the anchor.
func inputMayChangeField(action string, args map[string]any) bool {
	if action == "mouse_click" {
		return true
	}
	if action != "key_combo" {
		return false
	}
	keys, ok := targetStringList(args, "keys")
	if !ok || len(keys) == 0 {
		return false
	}
	if keys[0] == "tab" || keys[0] == "escape" || keys[0] == "esc" {
		return true
	}
	return len(keys) == 2 && (keys[0] == "l" || keys[0] == "f") && (keys[1] == "cmd" || keys[1] == "command")
}
