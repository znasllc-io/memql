package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelsAllow_DefaultsToDeny.
//
// Serving a model call spends this machine's own GPU on somebody else's
// prompt, so it gets the posture apps.allow has: nothing is offered until
// the machine's owner says which model may be. An empty allow list is the
// state of every machine upgrading into this feature, and it must not
// mean "all".
func TestModelsAllow_DefaultsToDeny(t *testing.T) {
	if got := DefaultPolicy().ModelsAllow(); len(got) != 0 {
		t.Errorf("default models.allow = %v, want empty (default-deny)", got)
	}
	var nilPolicy *Policy
	if got := nilPolicy.ModelsAllow(); got != nil {
		t.Errorf("nil policy models.allow = %v, want nil", got)
	}
	if got := DefaultPolicy().ModelRuntimes(); len(got) != 0 {
		t.Errorf("default models.runtimes = %v, want empty", got)
	}
}

// TestModelsPolicy_LoadsFromPolicyYAML pins the shape the docs promise,
// so an operator following them gets what they were told they would.
func TestModelsPolicy_LoadsFromPolicyYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	const doc = `models:
  allow:
    - llama3.1:8b
    - nomic-embed-text
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
      api_key_env: LMSTUDIO_KEY
      models:
        - id: qwen2.5-7b-instruct
          context_window: 32768
          structured_output: true
          max_concurrent: 2
        - id: text-embed
          embeddings: true
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}

	allow := p.ModelsAllow()
	if len(allow) != 2 || allow[0] != "llama3.1:8b" || allow[1] != "nomic-embed-text" {
		t.Fatalf("models.allow = %v", allow)
	}
	// The returned slice must be a copy: the worker reads this on every
	// discovery and a shared slice would race a SIGHUP reload.
	allow[0] = "mutated"
	if again := p.ModelsAllow(); again[0] != "llama3.1:8b" {
		t.Errorf("ModelsAllow handed out its backing array: %v", again)
	}

	runtimes := p.ModelRuntimes()
	if len(runtimes) != 1 {
		t.Fatalf("models.runtimes = %v", runtimes)
	}
	rt := runtimes[0]
	if rt.Name != "lmstudio" || rt.BaseURL != "http://127.0.0.1:1234/v1" || rt.APIKeyEnv != "LMSTUDIO_KEY" {
		t.Errorf("runtime = %+v", rt)
	}
	if len(rt.Models) != 2 {
		t.Fatalf("declared models = %v", rt.Models)
	}
	if rt.Models[0].ContextWindow != 32768 || !rt.Models[0].StructuredOutput || rt.Models[0].MaxConcurrent != 2 {
		t.Errorf("declared attributes did not load: %+v", rt.Models[0])
	}
	if !rt.Models[1].Embeddings || rt.Models[1].StructuredOutput {
		t.Errorf("second model = %+v", rt.Models[1])
	}
	// The nested slice is copied too: a caller appending to it would be
	// editing the live policy under a lock this method already released.
	rt.Models[0].ID = "mutated"
	if again := p.ModelRuntimes(); again[0].Models[0].ID != "qwen2.5-7b-instruct" {
		t.Errorf("ModelRuntimes handed out its nested backing array: %v", again[0].Models)
	}
}

// TestModelsAllow_ReloadPicksUpANewModel: SIGHUP makes a newly pulled
// model offerable without a worker restart.
func TestModelsAllow_ReloadPicksUpANewModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("models:\n  allow:\n    - llama3.1:8b\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.ModelsAllow()) != 1 {
		t.Fatalf("models.allow = %v", p.ModelsAllow())
	}
	if err := os.WriteFile(path, []byte("models:\n  allow:\n    - llama3.1:8b\n    - qwen2.5:7b\n"), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := p.ModelsAllow(); len(got) != 2 {
		t.Fatalf("after reload models.allow = %v, want two", got)
	}
}

// TestModelRuntimes_ReloadReplacesRatherThanMerges.
//
// The asymmetry with models.allow is deliberate: an allow entry is a bare
// name, where a runtime is a record with a base URL, a key variable and a
// model list. Merging two records that share a name produces a hybrid
// neither the operator nor this code intended -- an endpoint moved to a
// new port would keep answering on the old one until a restart.
func TestModelRuntimes_ReloadReplacesRatherThanMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	const first = `models:
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
      models:
        - id: m
`
	const second = `models:
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:9999/v1
      models:
        - id: m
`
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got := p.ModelRuntimes()
	if len(got) != 1 {
		t.Fatalf("runtimes = %v, want exactly one after a replace", got)
	}
	if got[0].BaseURL != "http://127.0.0.1:9999/v1" {
		t.Errorf("base_url = %q, want the new one", got[0].BaseURL)
	}
}

// TestModelsPolicy_DoesNotDisturbTheRest. The models block is additive:
// a policy.yaml that never mentions it behaves exactly as before.
func TestModelsPolicy_DoesNotDisturbTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("apps:\n  homes:\n    local:\n      allow:\n        - claude-code\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if got := p.AppsAllowFor("local"); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("app consent = %v", got)
	}
	if got := p.ModelsAllow(); len(got) != 0 {
		t.Errorf("models.allow = %v, want empty", got)
	}
	if err := p.CheckShell("git status"); err != nil {
		t.Errorf("the shell policy must be unaffected: %v", err)
	}
}

// TestModelsPull_AbsentKeyMeansYes.
//
// models.pull is DEFAULT TRUE, the opposite posture from models.allow
// two fields above it, and the tri-state is what makes that survive
// contact with a yaml file. Every policy.yaml written before this key
// existed -- which is all of them -- omits it, and a plain bool reads an
// absent key as `pull: false`. That would turn every machine already in
// the fleet into one that refuses a pull, with nothing in any file on it
// saying why.
func TestModelsPull_AbsentKeyMeansYes(t *testing.T) {
	if !DefaultPolicy().ModelsPullAllowed() {
		t.Error("the shipped default must allow a pull")
	}
	var nilPolicy *Policy
	if !nilPolicy.ModelsPullAllowed() {
		t.Error("a build that loaded no policy has refused nothing")
	}
}

// TestModelsPull_ThreeStatesOverAPolicyFile drives the switch through a
// real load of a real file, because the state that matters -- absent --
// is the one a struct literal cannot express.
func TestModelsPull_ThreeStatesOverAPolicyFile(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		want bool
	}{
		"absent from a models block that says plenty else": {
			doc:  "models:\n  allow:\n    - llama3.1:8b\n",
			want: true,
		},
		"absent because the file has no models block": {
			doc:  "apps:\n  homes:\n    local:\n      allow:\n        - claude-code\n",
			want: true,
		},
		"said out loud": {
			doc:  "models:\n  pull: true\n  allow:\n    - llama3.1:8b\n",
			want: true,
		},
		"refused out loud": {
			doc:  "models:\n  pull: false\n  allow:\n    - llama3.1:8b\n",
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			if err := os.WriteFile(path, []byte(tc.doc), 0o600); err != nil {
				t.Fatalf("write policy: %v", err)
			}
			p, err := LoadPolicy(path)
			if err != nil {
				t.Fatalf("LoadPolicy: %v", err)
			}
			if got := p.ModelsPullAllowed(); got != tc.want {
				t.Errorf("models.pull = %v, want %v", got, tc.want)
			}
			// The rest of the models block still loads: the switch is
			// additive, not a replacement for the allow list.
			if strings.Contains(tc.doc, "llama3.1:8b") {
				if allow := p.ModelsAllow(); len(allow) != 1 || allow[0] != "llama3.1:8b" {
					t.Errorf("models.allow = %v", allow)
				}
			}
		})
	}
}

// TestModelsPull_MissingFileAllowsAPull. A machine with no policy.yaml at
// all -- every machine on its first day -- must not be the one machine
// that cannot be told to pull a model.
func TestModelsPull_MissingFileAllowsAPull(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if !p.ModelsPullAllowed() {
		t.Error("no policy file means no refusal")
	}
}

// TestModelsPull_ReloadReplacesRatherThanMerges.
//
// The merge every list field in this file does would make `pull: false`
// UNREMOVABLE without a worker restart: an operator who deleted the line
// would keep the refusal and have nothing left to read that explained it.
// A tri-state field has a meaningful absent state, so for this key the
// file is the whole truth -- the same argument models.runtimes makes.
func TestModelsPull_ReloadReplacesRatherThanMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("models:\n  pull: false\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.ModelsPullAllowed() {
		t.Fatal("an explicit false must be honoured")
	}
	if err := os.WriteFile(path, []byte("models:\n  allow:\n    - llama3.1:8b\n"), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !p.ModelsPullAllowed() {
		t.Error("deleting the key must restore the default without a restart")
	}
	if err := os.WriteFile(path, []byte("models:\n  pull: false\n"), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if p.ModelsPullAllowed() {
		t.Error("SIGHUP must be able to turn the switch back off")
	}
}
