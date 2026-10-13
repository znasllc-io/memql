package tools

import (
	"encoding/json"
	"fmt"
	"os"
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
// Actions lists the workerComputer actions this build can dispatch:
// the computer-use-backed set (derived from the same table the computer-use action
// router uses, so it can never drift from reality) plus the
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
//
// InferenceServe is this machine's sharing consent, from policy.yaml
// `inference.serve` (memql-cockpit#399, engine memql#5146 record D6).
// Additive in exactly the way Displays was, and SCHEMAVERSION STAYS 1
// -- the engine admits that number and no other, rejecting anything
// else by value ("unsupported schemaVersion %d"), so a bump here fails
// every registration on the fleet at once.
//
// The engine at the current pin (5c4f6ae9) TOLERATES this key and does
// not store it: ParseCapabilityDescriptor unmarshals into a struct
// without DisallowUnknownFields, so an unknown key survives validation,
// and its AsMap() then re-serialises only the fields it knows. So this
// travels safely and arrives nowhere until memql#5146 lands. That is
// the same one-way situation the model label's params/quant/tools
// attributes are in, and it is safe in this direction only: a reader
// that skips a key it does not know cannot be broken by one, while a
// key this side misspells is never rejected, never logged, and simply
// never read.
type CapabilityDescriptor struct {
	Architecture string `json:"architecture"`
	// ActionContracts comes from the implementation, never operator labels.
	// Version 2 requires explicit execution/platform and executes image digests.
	ActionContracts      map[string]int      `json:"actionContracts,omitempty"`
	RepositoryScopes     map[string][]string `json:"repositoryScopes,omitempty"`
	Platform             string              `json:"platform"`
	DisplayServer        string              `json:"displayServer"`
	ComputerUseAvailable bool                `json:"computerUseAvailable"`
	Actions              []string            `json:"actions"`
	Displays             int                 `json:"displays"`
	InferenceServe       string              `json:"inferenceServe"`
	SchemaVersion        int                 `json:"schemaVersion"`
}

// ComputeCapabilities builds the descriptor for the running binary
// and host. The build-variant bits (buildHasComputerUse +
// supportedComputerActions) come from capabilities_headless.go /
// capabilities_computeruse.go.
func ComputeCapabilities() CapabilityDescriptor {
	return computeCapabilities(runtime.GOOS, os.Getenv)
}

// computeCapabilities is the env-injectable core of
// ComputeCapabilities so tests can table-drive the display-server
// detection without mutating the process environment.
func computeCapabilities(goos string, getenv func(string) string) CapabilityDescriptor {
	// Dispatchable actions = the per-build computer-use router table plus the
	// build-agnostic set (`wait`). Merged + sorted for a stable wire
	// shape; the slices never overlap (the router table is computeruse-only,
	// the agnostic set is routed before the router).
	actions := append(supportedComputerActions(), buildAgnosticComputerActions...)
	sort.Strings(actions)
	if actions == nil {
		// Keep the JSON shape stable: "actions": [] -- never null.
		actions = []string{}
	}
	displayServer := "none"
	if buildHasComputerUse {
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
	contracts := map[string]int{"workerHost.pipeline_step": 3}
	if goos == "darwin" || goos == "linux" {
		contracts["workerHost.computer_permission"] = 1
	}
	return CapabilityDescriptor{
		Architecture:         runtime.GOARCH,
		ActionContracts:      contracts,
		Platform:             goos,
		DisplayServer:        displayServer,
		ComputerUseAvailable: buildHasComputerUse,
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
	return CapabilityDescriptorJSONFor(ServeOwner, nil)
}

// CapabilityDescriptorJSONFor is the same descriptor with this
// machine's sharing consent attached.
//
// The consent is passed IN rather than read from a policy here, because
// ComputeCapabilities describes the BINARY AND THE HOST and knows
// nothing about a config file. Reaching for one from inside it would
// give this function a filesystem dependency on the Register path, and
// a machine whose policy.yaml had gone missing would then fail its
// handshake over a field that is optional.
func CapabilityDescriptorJSONFor(serve string, scopes map[string][]string) (string, error) {
	desc := ComputeCapabilities()
	desc.RepositoryScopes = scopes
	// Normalised HERE as well as in the policy, because this function
	// takes a bare string from any caller. Anything that is not the
	// grant is the default, which is the direction a consent has to
	// fail in.
	desc.InferenceServe = ServeOwner
	if serve == ServeCluster {
		desc.InferenceServe = ServeCluster
	}
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
		"platform":             desc.Platform,
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
