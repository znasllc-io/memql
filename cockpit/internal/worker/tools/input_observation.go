package tools

import "time"

// Activity is content-free. Private synthesized events must not enter the HID
// counters. A missing monitor never enables the transition path.
type inputActivity struct {
	counters   [16]uint32
	activation uint64
}

type inputActivityBackend interface {
	inputActivity() (inputActivity, bool)
}

type InputInterruption struct {
	At     time.Time             `json:"at"`
	Action string                `json:"action"`
	Phase  string                `json:"phase"`
	Reason string                `json:"reason"`
	Focus  *InputFocusDiagnostic `json:"focus,omitempty"`
}

type InputFocusDiagnostic struct {
	Stage           string `json:"stage"`
	ErrorCode       int    `json:"error_code"`
	MatchingWindows int    `json:"matching_windows"`
}

func (g *inputTargetGuard) stopFocus(action, phase string, err error) {
	first := g.firstInterruption == nil
	g.stop(action, phase, "focus_unavailable")
	if failure, ok := err.(interface{ inputFocusDiagnostic() InputFocusDiagnostic }); first && ok {
		diagnostic := failure.inputFocusDiagnostic()
		g.firstInterruption.Focus = &diagnostic
	}
}

func (g *inputTargetGuard) stop(action, phase, reason string) {
	g.halted = true
	if g.firstInterruption == nil {
		g.firstInterruption = &InputInterruption{At: g.now().UTC(), Action: action, Phase: phase, Reason: reason}
	}
}

func (g *inputTargetGuard) activity() *inputActivity {
	if backend, ok := g.backend.(inputActivityBackend); ok {
		if value, available := backend.inputActivity(); available {
			return &value
		}
	}
	return nil
}

func (g *inputTargetGuard) undisturbed(expected *inputActivity) bool {
	if expected == nil {
		return true
	} // old backends retain the strict field anchor
	current := g.activity()
	return current != nil && *current == *expected
}

// A completed discrete gesture may replace its field asynchronously. It has
// already spent its input receipt. Only a new stable screenshot can grant input
// again, in the identical retained app/window/geometry, with no HID activity or
// intervening activation. This is not recovery from a latched interruption.
func (g *inputTargetGuard) canObserveTransition(owner inputScope) bool {
	return g.pendingObservation && owner == g.pendingScope && g.now().Before(g.pendingUntil) && g.anchorActivity != nil && g.undisturbed(g.anchorActivity)
}

func focusDifference(a, b inputFocus) string {
	if !a.same(b, false) {
		return "window_identity_or_geometry_changed"
	}
	return "focused_field_changed"
}
