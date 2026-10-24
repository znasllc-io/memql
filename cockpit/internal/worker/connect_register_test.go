package worker

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// Server-side validation contract for Register.capability_descriptor_json
// (memql#1331). Encoded here verbatim so the cockpit can never drift
// into producing a descriptor the server would reject with
// register_failed:
//
//   - raw JSON <= 4096 bytes
//   - schemaVersion == 1
//   - <= 64 actions
//   - every action name matches [A-Za-z0-9._-]{1,64}
//   - no duplicate action names
const (
	registerDescriptorMaxBytes   = 4096
	registerDescriptorSchemaWant = 1
	registerDescriptorMaxActions = 64
)

var registerActionNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// TestBuildRegister_CarriesValidCapabilityDescriptor asserts the
// Register handshake message populates capability_descriptor_json
// (memql-cockpit#166) with JSON that satisfies every rule the memql
// server validates (memql#1331), and that it matches the exact
// descriptor workerComputer.capabilities would return -- one source
// of truth, two surfaces.
func TestBuildRegister_CarriesValidCapabilityDescriptor(t *testing.T) {
	register := buildRegister(Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})

	raw := register.GetCapabilityDescriptorJson()
	if raw == "" {
		t.Fatal("Register must carry capability_descriptor_json")
	}
	if len(raw) > registerDescriptorMaxBytes {
		t.Fatalf("descriptor is %d bytes; server rejects > %d", len(raw), registerDescriptorMaxBytes)
	}

	var desc tools.CapabilityDescriptor
	if err := json.Unmarshal([]byte(raw), &desc); err != nil {
		t.Fatalf("capability_descriptor_json is not valid descriptor JSON: %v (raw: %s)", err, raw)
	}
	if desc.SchemaVersion != registerDescriptorSchemaWant {
		t.Errorf("schemaVersion = %d; server requires %d", desc.SchemaVersion, registerDescriptorSchemaWant)
	}
	if len(desc.Actions) > registerDescriptorMaxActions {
		t.Errorf("%d actions; server rejects > %d", len(desc.Actions), registerDescriptorMaxActions)
	}
	seen := make(map[string]bool, len(desc.Actions))
	for _, a := range desc.Actions {
		if !registerActionNamePattern.MatchString(a) {
			t.Errorf("action %q does not match the server's name pattern %s", a, registerActionNamePattern)
		}
		if seen[a] {
			t.Errorf("duplicate action %q; server rejects duplicates", a)
		}
		seen[a] = true
	}
	if desc.Platform == "" {
		t.Error("descriptor must carry the platform")
	}
	if desc.DisplayServer == "" {
		t.Error("descriptor must carry the displayServer")
	}

	// The handshake descriptor and the capabilities action must agree
	// byte-for-byte: both serialize tools.ComputeCapabilities().
	want, err := tools.CapabilityDescriptorJSON()
	if err != nil {
		t.Fatalf("CapabilityDescriptorJSON: %v", err)
	}
	if raw != want {
		t.Errorf("handshake descriptor diverges from the capabilities action:\nregister: %s\naction:   %s", raw, want)
	}
}

// The pipelines label is how the cluster's router knows this machine's
// owner opted in to running CI steps (memql#5494). It is advertised exactly
// when policy.yaml says pipelines.allow -- and never otherwise, not even
// when worker.yaml's own labels claim it, because a machine that would
// refuse every step routed on the label must not carry it.
func TestRegisterAdvertisesPipelinesOnlyWhenThePolicyAllows(t *testing.T) {
	cfg := Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
		StateDir:     t.TempDir(),
		Labels:       map[string]string{"team": "core"},
	}
	on := buildRegister(cfg, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: true})
	// The literal pair is the wire contract: the engine requires exactly
	// pipelines=allowed of a machine it routes a pipeline step to.
	if got := on.GetLabels()["pipelines"]; got != "allowed" {
		t.Fatalf("pipelines.allow: true advertised pipelines=%q, want \"allowed\" (labels %v)", got, on.GetLabels())
	}
	if on.GetLabels()["team"] != "core" {
		t.Errorf("the operator's own labels were lost: %v", on.GetLabels())
	}

	off := buildRegister(cfg, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
	if v, ok := off.GetLabels()["pipelines"]; ok {
		t.Fatalf("a machine whose policy allows no pipelines advertised pipelines=%q", v)
	}

	cfg.Labels = map[string]string{"pipelines": "allowed", "team": "core"}
	claimed := buildRegister(cfg, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
	if v, ok := claimed.GetLabels()["pipelines"]; ok {
		t.Fatalf("a worker.yaml label claimed pipelines=%q that policy.yaml does not grant", v)
	}
	if claimed.GetLabels()["team"] != "core" {
		t.Errorf("the operator's own labels were lost: %v", claimed.GetLabels())
	}
	if cfg.Labels["pipelines"] != "allowed" {
		t.Error("buildRegister edited the config's own label map")
	}
}

// The worker reports capabilities; only the owner's stored Fleet audience
// grants sharing. No second consent is advertised by the machine.
func TestRegisterNeedsNoSharingConfiguration(t *testing.T) {
	register := buildRegister(Config{Name: "test-worker", Capabilities: []string{"HEADLESS"}}, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{})
	var got map[string]any
	if err := json.Unmarshal([]byte(register.GetCapabilityDescriptorJson()), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["inferenceServe"]; exists {
		t.Fatal("sharing must be controlled by Fleet")
	}
	if got["schemaVersion"] != float64(1) {
		t.Fatal("descriptor schema changed")
	}
}

func TestRegisterCarriesTheExactRepositoryConsent(t *testing.T) {
	for _, policy := range []tools.PipelinesPolicy{
		{}, {Allow: true}, {Allow: true, Repos: []string{" O/A.GIT ", "o/b"}},
	} {
		reg := buildRegister(Config{Name: "scope", StateDir: t.TempDir()}, nil, models.Inventory{}, hardware.Inventory{}, policy)
		var descriptor tools.CapabilityDescriptor
		if err := json.Unmarshal([]byte(reg.GetCapabilityDescriptorJson()), &descriptor); err != nil {
			t.Fatal(err)
		}
		repos, present := descriptor.RepositoryScopes["workerHost.pipeline_step"]
		if present != policy.Allow {
			t.Fatalf("allow=%v scopes=%v", policy.Allow, descriptor.RepositoryScopes)
		}
		if len(repos) != len(policy.Repos) {
			t.Fatalf("scope lost restrictions: %v", repos)
		}
		if len(repos) > 0 && repos[0] != "o/a" {
			t.Fatalf("noncanonical scope: %v", repos)
		}
	}
}
