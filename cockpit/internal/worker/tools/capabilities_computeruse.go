//go:build computeruse

package tools

import (
	"sort"

	"github.com/go-vgo/robotgo"
)

// buildHasComputerUse reports whether this binary was built with the computeruse
// tag. True here: the computeruse-tagged build carries the RobotGo backend.
const buildHasComputerUse = true

// supportedComputerActions derives the advertised action list from
// computerActionHandlers -- the exact routing table dispatchComputer
// uses (computer_computeruse.go) -- so the descriptor can never drift from
// what the router actually serves. Sorted for a stable wire shape.
//
// window_list / window_focus are advertised since memql-cockpit#167
// shipped real implementations (macOS CGWindowList, Linux/X11 EWMH).
// On a Wayland session they return a structured
// unsupported_on_platform failure at call time -- the descriptor's
// displayServer field is the up-front signal for that.
func supportedComputerActions() []string {
	out := make([]string, 0, len(computerActionHandlers))
	for action := range computerActionHandlers {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}

// displayCount probes the number of attached displays for the
// capability descriptor (memql-cockpit#165). Only called once
// computeCapabilities has confirmed a reachable display server
// (quartz / x11), so a non-positive probe result -- e.g. Xinerama
// missing on a single-headed X server -- is floored at 1: a display
// server implies at least one display. Mirrors the
// enumerateDisplays fallback in computer_computeruse.go.
func displayCount() int {
	n := robotgo.DisplaysNum()
	if n < 1 {
		n = 1
	}
	return n
}
