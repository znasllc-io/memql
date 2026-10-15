//go:build !computeruse

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

// fakeEnv returns a getenv func backed by the supplied map -- the
// env-injectable seam for detectDisplayServer / computeCapabilities
// so the tests never mutate the process environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestComputerPermissionContractOnlyAdvertisesSupportedPlatforms(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows", "freebsd"} {
		got := computeCapabilities(platform, fakeEnv(nil)).ActionContracts["workerHost.computer_permission"]
		want := 0
		if platform == "darwin" || platform == "linux" {
			want = 1
		}
		if got != want {
			t.Fatalf("platform %s advertised contract %d, want %d", platform, got, want)
		}
	}
}

func TestComputeCapabilities_Headless(t *testing.T) {
	// This test file is !computeruse, so it always exercises the headless
	// variant: computerUseAvailable=false, empty actions, displayServer
	// "none" regardless of what the host env claims.
	desc := computeCapabilities("linux", fakeEnv(map[string]string{
		"DISPLAY":         ":0",
		"WAYLAND_DISPLAY": "wayland-0",
	}))
	if desc.ComputerUseAvailable {
		t.Error("headless build must report computerUseAvailable=false")
	}
	if desc.DisplayServer != "none" {
		t.Errorf("headless build must report displayServer=none regardless of env; got %q", desc.DisplayServer)
	}
	// Actions means "dispatchable actions" (memql-cockpit#166): the
	// build-agnostic `wait` works headless, so it IS advertised even
	// with computerUseAvailable=false. No computer-use-backed action may appear.
	if len(desc.Actions) != 1 || desc.Actions[0] != "wait" {
		t.Errorf(`headless build must advertise exactly ["wait"]; got %v`, desc.Actions)
	}
	if desc.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", desc.SchemaVersion)
	}
	if desc.Platform != "linux" {
		t.Errorf("platform = %q, want %q", desc.Platform, "linux")
	}
	if desc.Displays != 0 {
		t.Errorf("headless build must report displays=0; got %d", desc.Displays)
	}
}

func TestComputeCapabilities_PlatformFollowsRuntime(t *testing.T) {
	desc := ComputeCapabilities()
	if desc.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want runtime.GOOS %q", desc.Platform, runtime.GOOS)
	}
}

func TestDetectDisplayServer(t *testing.T) {
	cases := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"darwin is always quartz", "darwin", nil, "quartz"},
		{"darwin ignores DISPLAY", "darwin", map[string]string{"DISPLAY": ":0"}, "quartz"},
		{"linux wayland via WAYLAND_DISPLAY", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "wayland"},
		{"linux wayland via XDG_SESSION_TYPE", "linux", map[string]string{"XDG_SESSION_TYPE": "wayland"}, "wayland"},
		{"linux wayland XDG case-insensitive", "linux", map[string]string{"XDG_SESSION_TYPE": "Wayland"}, "wayland"},
		{"linux wayland wins over x11", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, "wayland"},
		{"linux x11 via DISPLAY", "linux", map[string]string{"DISPLAY": ":0"}, "x11"},
		{"linux x11 with non-wayland session type", "linux", map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":1"}, "x11"},
		{"linux headless", "linux", nil, "none"},
		{"linux tty session without DISPLAY", "linux", map[string]string{"XDG_SESSION_TYPE": "tty"}, "none"},
		{"windows is none for now", "windows", nil, "none"},
		{"freebsd is none for now", "freebsd", map[string]string{"DISPLAY": ":0"}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectDisplayServer(tc.goos, fakeEnv(tc.env))
			if got != tc.want {
				t.Errorf("detectDisplayServer(%q, %v) = %q, want %q", tc.goos, tc.env, got, tc.want)
			}
		})
	}
}

// TestDispatcher_CapabilitiesWorksHeadless is the core #162
// contract: workerComputer.capabilities must answer on the headless
// build instead of returning computeruse_unavailable -- it's the one action
// that must never fail.
func TestDispatcher_CapabilitiesWorksHeadless(t *testing.T) {
	gate := &fakeGate{decision: consent.Decision{Allowed: true, Reason: "consent granted"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)

	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "capabilities",
		ArgsJson: []byte(`{}`),
		CallId:   "caps-1",
	}
	success, failure := d.Dispatch(context.Background(), dispatch)
	if failure != nil {
		t.Fatalf("capabilities must never fail; got %s: %s", failure.GetErrorCode(), failure.GetErrorMessage())
	}
	if success == nil {
		t.Fatal("expected a success payload")
	}
	if gate.calls != 1 {
		t.Errorf("consent gate should be consulted exactly once; got %d", gate.calls)
	}

	var desc CapabilityDescriptor
	if err := json.Unmarshal(success.GetResultJson(), &desc); err != nil {
		t.Fatalf("result is not valid descriptor JSON: %v (raw: %s)", err, success.GetResultJson())
	}
	if desc.ComputerUseAvailable {
		t.Error("headless dispatch must report computerUseAvailable=false")
	}
	if len(desc.Actions) != 1 || desc.Actions[0] != "wait" {
		t.Errorf(`headless dispatch must report actions=["wait"]; got %v`, desc.Actions)
	}
	if desc.DisplayServer != "none" {
		t.Errorf("headless dispatch must report displayServer=none; got %q", desc.DisplayServer)
	}
	if desc.SchemaVersion != capabilitySchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", desc.SchemaVersion, capabilitySchemaVersion)
	}
	if desc.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", desc.Platform, runtime.GOOS)
	}

	// The wire shape must carry a JSON array (never null) so JSON
	// consumers can iterate without a nil check. Headless advertises
	// exactly the build-agnostic set: ["wait"] (memql-cockpit#166).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(success.GetResultJson(), &raw); err != nil {
		t.Fatalf("re-decode raw: %v", err)
	}
	if string(raw["actions"]) != `["wait"]` {
		t.Errorf(`"actions" must marshal to ["wait"]; got %s`, raw["actions"])
	}
	// displays is additive (memql-cockpit#165) and must be present --
	// 0 on the headless build, never null/absent.
	if string(raw["displays"]) != "0" {
		t.Errorf(`"displays" must marshal to 0 on the headless build; got %s`, raw["displays"])
	}
}

// TestDispatcher_CapabilitiesGatedAsObserve confirms the call still
// routes through the consent gate (a denied gate denies it like any
// other call) and that the classifier files it as observe-class.
func TestDispatcher_CapabilitiesGatedAsObserve(t *testing.T) {
	if got := consent.Classify("workerComputer", "capabilities"); got != consent.ClassObserve {
		t.Errorf("Classify(workerComputer, capabilities) = %q, want %q", got, consent.ClassObserve)
	}

	gate := &fakeGate{decision: consent.Decision{Allowed: false, Reason: "no window open"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)
	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "capabilities",
		ArgsJson: []byte(`{}`),
		CallId:   "caps-2",
	}
	_, failure := d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() != "consent_required" {
		t.Fatalf("a denying gate must still gate capabilities; got %+v", failure)
	}
}

// TestDispatcher_OtherComputerActionsStillGuiUnavailable pins the
// headless build's pre-existing behavior for every NON-capabilities
// workerComputer action: computeruse_unavailable, not a silent success.
// window_list / window_focus are real on the computeruse build since
// memql-cockpit#167 but must stay computeruse_unavailable here.
func TestDispatcher_OtherComputerActionsStillGuiUnavailable(t *testing.T) {
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)
	for _, action := range []string{"screenshot", "mouse_click", "mouse_down", "mouse_up", "key_hold", "display_info", "window_list", "window_focus"} {
		dispatch := &memqlv1.ToolDispatch{
			Tool:     "workerComputer",
			Action:   action,
			ArgsJson: []byte(`{}`),
			CallId:   "caps-3-" + action,
		}
		_, failure := d.Dispatch(context.Background(), dispatch)
		if failure == nil || failure.GetErrorCode() != "computeruse_unavailable" {
			t.Errorf("headless workerComputer.%s should be computeruse_unavailable; got %+v", action, failure)
		}
	}
}

func TestPipelineActionContractComesFromTheBuild(t *testing.T) {
	descriptor := ComputeCapabilities()
	if got := descriptor.ActionContracts["workerHost.pipeline_step"]; got != 3 {
		t.Fatalf("pipeline contract = %d", got)
	}
	raw, err := CapabilityDescriptorJSONFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CapabilityDescriptor
	if err = json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ActionContracts["workerHost.pipeline_step"] != 3 {
		t.Fatal("registration lost the pipeline execution contract")
	}
}

func TestCapabilitiesReportsThePhysicalHomeForScopedReads(t *testing.T) {
	realHome := t.TempDir()
	link := filepath.Join(t.TempDir(), "Home With Spaces")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	expected, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := ComputeCapabilities().HomeDirectory; got != expected {
		t.Fatalf("home = %q, want physical path %q", got, expected)
	}
	raw, err := CapabilityDescriptorJSON()
	if err != nil {
		t.Fatal(err)
	}
	var registered CapabilityDescriptor
	if err := json.Unmarshal([]byte(raw), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.HomeDirectory != expected {
		t.Fatal("registration lost the home directory")
	}
	t.Setenv("HOME", filepath.Join(realHome, "missing"))
	if got := ComputeCapabilities().HomeDirectory; got != "" {
		t.Fatalf("invented home: %q", got)
	}
	t.Setenv("HOME", "/")
	if got := ComputeCapabilities().HomeDirectory; got != "" {
		t.Fatalf("offered root as home: %q", got)
	}
}
