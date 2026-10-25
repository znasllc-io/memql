package tools

import (
	"encoding/json"
	"fmt"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// capabilitySchemaVersion is the wire-shape version of the
// capability descriptor. Bump when the JSON shape changes in a way
// consumers must branch on; additive fields don't need a bump.
const capabilitySchemaVersion = 1

// computerCapabilitiesAction is the one workerComputer action that
// works on EVERY build variant. The dispatcher routes it before the
// per-build dispatchComputer so a headless binary answers honestly
// (computerUseAvailable=false, actions=[]) instead of returning
// computeruse_unavailable. It's the agent runtime's introspection path:
// "what can this worker actually do?" must never itself fail.
const computerCapabilitiesAction = "capabilities"

// computerWaitAction is the build-agnostic pacing primitive
// (memql-cockpit#166): a bounded sleep that needs no computer-use backend.
// Like `capabilities` it is routed by the dispatcher BEFORE the
// per-build router so it works identically on headless and computeruse
// builds -- see wait.go.
const computerWaitAction = "wait"

// buildAgnosticComputerActions lists workerComputer actions that are
// dispatchable on EVERY build variant and therefore belong in the
// descriptor's Actions list on both builds. Decision
// (memql-cockpit#166): Actions means "dispatchable actions", so
// `wait` is advertised even when computerUseAvailable=false -- a headless
// worker really can serve it. The meta `capabilities` action stays
// out of the list (it is the introspection channel itself and is
// unconditionally available; advertising it would be circular).
var buildAgnosticComputerActions = []string{computerWaitAction}

// CapabilityDescriptor is the worker's computer-use capability
// self-description (memql-cockpit#162). Computed from the build tag
// (computeruse vs headless) plus the host environment at call time.
//
// Actions lists supported workerComputer actions, filtering legacy refused
// inputs and platforms without a target-addressed backend, plus the
// build-agnostic set (`wait`, memql-cockpit#166) that works on every
// variant -- so a headless build advertises ["wait"], not []. The
// `capabilities` action itself is deliberately NOT listed -- it is
// available on every build unconditionally.
//
// ComputerUseAvailable reports whether the binary was built with the computeruse
// tag. DisplayServer reports what the environment looks like at
// call time ("none" when no display is reachable); consumers that
// want "can I actually screenshot right now" should check both.
//
// Displays is the number of attached displays at call time
// (memql-cockpit#165): 0 on headless builds and when no display
// server is reachable, at least 1 otherwise. An additive field --
// schemaVersion stays 1.
type CapabilityDescriptor struct {
	Architecture  string `json:"architecture"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
	// ActionContracts comes from the implementation, never operator labels.
	// Version 2 requires explicit execution/platform and executes image digests.
	ActionContracts      map[string]int      `json:"actionContracts,omitempty"`
	RepositoryScopes     map[string][]string `json:"repositoryScopes,omitempty"`
	Platform             string              `json:"platform"`
	DisplayServer        string              `json:"displayServer"`
	ComputerUseAvailable bool                `json:"computerUseAvailable"`
	Actions              []string            `json:"actions"`
	Displays             int                 `json:"displays"`

	InputTargetPolicy map[string]any `json:"inputTargetPolicy,omitempty"`

	SchemaVersion int `json:"schemaVersion"`
}

// ComputeCapabilities builds the descriptor for the running binary
// and host. The build-variant bits (buildHasComputerUse +
// supportedComputerActions) come from capabilities_headless.go /
// capabilities_computeruse.go.
func ComputeCapabilities() CapabilityDescriptor {
	desc := computeCapabilitiesForRuntime(runtime.GOOS, os.Getenv, !desktopRuntimeDisabled.Load())
	// Scoped reads refuse symlink components. Advertise the physical home path
	// used by this worker account, without granting access or listing its files.
	if home, err := filepath.EvalSymlinks(homeDir()); err == nil && permission.CleanAbsolute(home) {
		desc.HomeDirectory = home
	}
	return desc
}

// computeCapabilities is the env-injectable core of
// ComputeCapabilities so tests can table-drive the display-server
// detection without mutating the process environment.
func computeCapabilities(goos string, getenv func(string) string) CapabilityDescriptor {
	return computeCapabilitiesForRuntime(goos, getenv, true)
}

func computeCapabilitiesForRuntime(goos string, getenv func(string) string, desktopReady bool) CapabilityDescriptor {
	hasDesktop := buildHasComputerUse && desktopReady
	// Dispatchable actions = the per-build computer-use router table plus the
	// build-agnostic set (`wait`). Merged + sorted for a stable wire
	// shape; the slices never overlap (the router table is computeruse-only,
	// the agnostic set is routed before the router).
	actions := append(availableComputerActions(supportedComputerActions(), goos), buildAgnosticComputerActions...)
	if !hasDesktop {
		actions = append([]string{}, buildAgnosticComputerActions...)
	}
	sort.Strings(actions)
	if actions == nil {
		// Keep the JSON shape stable: "actions": [] -- never null.
		actions = []string{}
	}
	displayServer := "none"
	if hasDesktop {
		displayServer = detectDisplayServer(goos, getenv)
	}
	// Probe the display count only when a RobotGo-drivable display
	// server is actually reachable: the probe touches the display
	// server, and capabilities must never fail -- not even on a computeruse
	// build running headless or under Wayland.
	displays := 0
	if displayServer == "quartz" || displayServer == "x11" {
		displays = displayCount()
	}
	contracts := map[string]int{"workerHost.pipeline_step": 4, "workerHost.fs_read": 2}
	if goos == "darwin" || goos == "linux" {
		contracts["workerHost.computer_permission"] = 1
	}
	var targetPolicy map[string]any
	if hasDesktop {
		contracts["workerComputer.input_target"] = 1
		supported := []string{}
		if goos == "darwin" {
			supported = []string{"key_type", "key_combo", "mouse_move", "mouse_click", "mouse_scroll"}
			contracts["workerComputer.window_selection"] = 1
		}
		targetPolicy = map[string]any{
			"supportedInputActions":      supported,
			"acquisition":                "screenshot with explicit windowId of the already focused window",
			"receipt":                    "inputTarget.id, required on one action in the same run, expires after 60 seconds",
			"coordinates":                "explicit x/y from that exact screenshot for every pointer action",
			"focusActivation":            goos == "darwin",
			"postEventAccess":            desktopInputGuard.postingAccess(),
			"pointerCoordinateAccessors": desktopInputGuard.pointerCoordinateAccessors(),
			"pointerCompatibility":       "macOS move/click packets require private coordinate accessors; each packet verifies its exact window and global/window-local points before posting; no global fallback",
			"windowSelection":            "macOS initial selection only: window_list with windowId and pid returns a single-use focusTarget; window_focus requires all three. Never clears or replaces an anchor/latch. A fresh screenshot is required afterward.",
			"key_combo":                  "keys is a string array: primary key first, then modifiers; for example [r,cmd] or [enter]",
			"key_type":                   "text is 1..4096 printable UTF-8 bytes; newline/tab refused; use a fresh screenshot before a separate key_combo",
			"mouse_click":                "explicit x/y, optional left/right/middle button and cmd/ctrl/alt/shift modifiers; single click only",
			"interruption":               "latched until operator review and worker restart; no automatic retry or reacquisition",
			"withinWindowObservation":    "On macOS a completed click/key gesture consumes its receipt. With unchanged HID counters and activation generation, a fresh stable screenshot may observe a replaced field in the identical retained window within 60 seconds. No input is allowed before that new receipt. Focus-query errors and target changes still latch.",
			"limitation":                 "process-addressed delivery; AX window/field checks are sampled, not atomic; delayed effects may stop input",
		}
	}
	return CapabilityDescriptor{
		Architecture:         runtime.GOARCH,
		InputTargetPolicy:    targetPolicy,
		ActionContracts:      contracts,
		Platform:             goos,
		DisplayServer:        displayServer,
		ComputerUseAvailable: hasDesktop,
		Actions:              actions,
		Displays:             displays,
		SchemaVersion:        capabilitySchemaVersion,
	}
}

// DetectDisplayServer reports the display server the live host
// session exposes ("quartz" / "wayland" / "x11" / "none"). Exported
// so the worker package can wire it as the Linux display-server
// preflight hook (memql-cockpit#164) and the setup wizard can print
// the same verdict the dispatcher will enforce -- one detection, no
// drift.
func DetectDisplayServer() string {
	return detectDisplayServer(runtime.GOOS, os.Getenv)
}

// detectDisplayServer names the display server the host environment
// exposes: "quartz" on macOS (always present), and on Linux
// "wayland" / "x11" / "none" from the session env vars. Everything
// else (windows, BSDs without the env vars) reports "none" until a
// dedicated probe lands.
func detectDisplayServer(goos string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return "quartz"
	case "linux":
		if getenv("WAYLAND_DISPLAY") != "" ||
			strings.EqualFold(strings.TrimSpace(getenv("XDG_SESSION_TYPE")), "wayland") {
			return "wayland"
		}
		if getenv("DISPLAY") != "" {
			return "x11"
		}
		return "none"
	}
	return "none"
}

// CapabilityDescriptorJSON marshals the live capability descriptor
// for the registration handshake (memql-cockpit#166): the worker
// copies it into Register.capability_descriptor_json so the server
// learns the action surface up front, without a round-trip through
// workerComputer.capabilities. Same source of truth as that action
// (ComputeCapabilities), so the two can never disagree.
func CapabilityDescriptorJSON() (string, error) {
	return CapabilityDescriptorJSONFor(nil)
}

// CapabilityDescriptorJSONFor includes the live repository scope advertisement.
func CapabilityDescriptorJSONFor(scopes map[string][]string) (string, error) {
	desc := ComputeCapabilities()
	desc.RepositoryScopes = scopes
	body, err := json.Marshal(desc)
	if err != nil {
		return "", fmt.Errorf("marshal capability descriptor: %w", err)
	}
	return string(body), nil
}

// runComputerCapabilities serves workerComputer.capabilities on
// every build variant. Routed by the dispatcher BEFORE the per-build
// dispatchComputer so the headless build answers instead of
// rejecting with computeruse_unavailable.
func runComputerCapabilities() (*memqlv1.Success, *memqlv1.Failure) {
	desc := ComputeCapabilities()
	preview := fmt.Sprintf("platform=%s displayServer=%s computerUseAvailable=%t actions=%d displays=%d",
		desc.Platform, desc.DisplayServer, desc.ComputerUseAvailable, len(desc.Actions), desc.Displays)
	return successComputerJSON(map[string]any{
		"actionContracts":      desc.ActionContracts,
		"inputTargetPolicy":    desc.InputTargetPolicy,
		"platform":             desc.Platform,
		"homeDirectory":        desc.HomeDirectory,
		"displayServer":        desc.DisplayServer,
		"computerUseAvailable": desc.ComputerUseAvailable,
		"actions":              desc.Actions,
		"displays":             desc.Displays,
		"schemaVersion":        desc.SchemaVersion,
	}, preview, 0), nil
}

// successComputerJSON is the structured-success helper for
// workerComputer handlers. Lives here (untagged) because both build
// variants need it: the computer-use handlers in computer_computeruse.go and the
// build-agnostic capabilities handler above.
func successComputerJSON(payload map[string]any, preview string, bytesOut int) *memqlv1.Success {
	body, _ := json.Marshal(payload)
	return &memqlv1.Success{
		ResultJson:    body,
		BytesOut:      uint64(bytesOut),
		OutputPreview: clampPreview(preview),
	}
}

// The vocabulary may include legacy handlers which now always refuse. Do not
// advertise those, or input on platforms without a target-addressed backend.
func availableComputerActions(vocabulary []string, goos string) []string {
	result := make([]string, 0, len(vocabulary))
	for _, action := range vocabulary {
		switch action {
		case "key_hold", "mouse_down", "mouse_up", "mouse_drag":
			continue
		case "window_focus", "key_type", "key_combo", "mouse_move", "mouse_click", "mouse_scroll":
			if goos != "darwin" {
				continue
			}
		}
		result = append(result, action)
	}
	return result
}
