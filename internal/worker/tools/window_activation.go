package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Activation is an initial selection, never recovery from interrupted input.
// The platform retains exact window/process identities and interruption state;
// the guard additionally binds its one-shot observation to the calling run.
type windowActivation interface {
	activate(context.Context) (inputFocus, error)
	close()
}

type windowActivationBackend interface {
	observeWindow(context.Context, WindowInfo) (windowActivation, error)
}

type windowSelection struct {
	id      string
	scope   inputScope
	window  WindowInfo
	expires time.Time
	target  windowActivation
}

func (g *inputTargetGuard) clearSelection() {
	if g.selection != nil {
		g.selection.target.close()
		g.selection = nil
	}
}

func sameWindowRecord(a, b WindowInfo) bool {
	return a.ID == b.ID && a.PID == b.PID && a.X == b.X && a.Y == b.Y && a.Width == b.Width && a.Height == b.Height
}

// An ordinary list remains read-only. Supplying windowId AND pid asks to retain
// exactly that freshly enumerated window for one initial activation attempt.
func (g *inputTargetGuard) windowList(ctx context.Context, args map[string]any, list func() ([]WindowInfo, *memqlv1.Failure)) (*memqlv1.Success, *memqlv1.Failure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clearSelection()
	if err := ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	windows, fail := list()
	if fail != nil {
		return nil, fail
	}
	payload := windowListPayload(windows)
	if _, requested := args["windowId"]; !requested {
		return successComputerJSON(payload, fmt.Sprintf("%d windows", len(windows)), 0), nil
	}
	if g.halted {
		return nil, g.interrupted()
	}
	if g.anchor != nil {
		return nil, g.refuseBoundActivation()
	}
	owner, owned := inputOwner(ctx)
	backend, supported := g.backend.(windowActivationBackend)
	if !owned || !supported {
		return nil, failure("window_focus_unavailable", "Initial window selection needs an owned run and an exact-window activation backend.")
	}
	id, fail := parseWindowID(args)
	if fail != nil {
		return nil, fail
	}
	pid, pidFail := parseWindowID(map[string]any{"windowId": args["pid"]})
	if pidFail != nil || pid == 0 || pid > 1<<31-1 {
		return nil, failure("bad_request", "window_list selection requires the positive pid from its window record.")
	}
	var selected WindowInfo
	matches := 0
	for _, w := range windows {
		if w.ID == id && uint64(w.PID) == pid {
			selected = w
			matches++
		}
	}
	if matches != 1 {
		return nil, failure("window_not_found", "The exact window ID and PID no longer identify one on-screen window.")
	}
	target, err := backend.observeWindow(ctx, selected)
	if err != nil {
		return nil, failure("window_focus_unavailable", err.Error())
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		target.close()
		return nil, failure("window_focus_unavailable", "Cannot create a selection receipt.")
	}
	g.selection = &windowSelection{id: hex.EncodeToString(nonce[:]), scope: owner, window: selected, expires: g.now().Add(inputTargetLifetime), target: target}
	payload["focusTarget"] = map[string]any{"id": g.selection.id, "windowId": id, "pid": pid, "expiresAt": g.selection.expires.UTC(), "singleUse": true}
	payload["focusInstructions"] = "For initial selection only, pass focusTarget.id as focusTarget together with windowId and pid to window_focus. Any detected interruption stops input. Activation creates no input receipt; take a fresh screenshot with windowId before input."
	return successComputerJSON(payload, "Retained exact window for one initial selection", 0), nil
}

// Called under the same lock as screenshot/input. Even a refused activation
// invalidates an older input receipt. An established anchor is never replaced.
func (g *inputTargetGuard) selectWindow(ctx context.Context, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	g.clearFrame()
	if g.anchor != nil {
		return nil, g.refuseBoundActivation()
	}
	s := g.selection
	owner, owned := inputOwner(ctx)
	if s == nil || !owned || owner != s.scope || argString(args, "focusTarget") != s.id {
		return nil, failure("window_focus_receipt_required", "Use window_list with the intended windowId and pid to obtain a fresh, single-use focusTarget for this run.")
	}
	defer g.clearSelection() // all attempts with this receipt consume it
	id, fail := parseWindowID(args)
	pid, pidFail := parseWindowID(map[string]any{"windowId": args["pid"]})
	if fail != nil || pidFail != nil || id != s.window.ID || pid != uint64(s.window.PID) || !g.now().Before(s.expires) {
		return nil, failure("window_focus_receipt_required", "Selection receipt expired or does not match the requested window ID and PID.")
	}
	if err := ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	// A valid receipt can expire while waiting for the main thread or an AX
	// response. Carry its remaining lifetime into every activation stage.
	activationCtx, cancel := context.WithDeadline(ctx, s.expires)
	defer cancel()
	focused, err := s.target.activate(activationCtx)
	if err != nil || focused == nil {
		if focused != nil {
			focused.close()
		}
		g.stop("window_focus", "activation", "activation_unverified")
		message := "Exact-window activation could not be verified."
		if err != nil {
			message += " " + err.Error()
		}
		return nil, failure("input_interrupted", message+" "+g.interrupted().GetErrorMessage())
	}
	if activationCtx.Err() != nil || !g.now().Before(s.expires) || !sameWindowRecord(s.window, focused.window()) {
		focused.close()
		g.stop("window_focus", "activation", "expired_or_window_changed")
		return nil, g.interrupted()
	}
	g.anchor = focused
	return successComputerJSON(map[string]any{"focused": true, "id": s.window.ID, "pid": s.window.PID, "method": "verified_initial_ax_window", "screenshotRequired": true}, "Exact window selected; fresh screenshot required before input", 0), nil
}

func (g *inputTargetGuard) refuseBoundActivation() *memqlv1.Failure {
	if g.backend == nil {
		g.stop("window_focus", "already_bound", "backend_unavailable")
		return g.interrupted()
	}
	current, err := g.backend.focus()
	if err != nil {
		g.stopFocus("window_focus", "already_bound", err)
		return g.interrupted()
	}
	defer current.close()
	if !g.anchor.same(current, true) {
		g.stop("window_focus", "already_bound", focusDifference(g.anchor, current))
		return g.interrupted()
	}
	return failure("input_focus_already_bound", "This worker already has a desktop target. Initial window selection cannot replace it; request a fresh screenshot of that target.")
}
