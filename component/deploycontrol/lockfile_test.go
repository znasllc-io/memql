package deploycontrol

import "testing"

const lockfileFixture = `version: "0.9.9"
validatedAt: "2026-06-02T00:00:00Z"
gate: "phase1-digest-cutover"
engineVersion: "0.9.9"
components:
  memql-identity:
    repo: znasllc-io/memql
    digest: sha256:5c5ced82097f8d1248081b4054a72f4f7c51265e5efde25fc07135c9cf3da5da
  memql-cognition:
    repo: znasllc-io/memql
    digest: sha256:3d3eae79e9f97b2bc334410b07f340b6e279bb13b750401e5f11da8469313003
  memql-voice:
    repo: znasllc-io/memql
    digest: sha256:bc858386a975e37ee74f212a58e6b6399b50c49c8c342ffdaf5919fdc0a6aca1
  memql-agent:
    repo: znasllc-io/memql
    digest: sha256:36418df78e1ecd587ed1e195d8ea9ccc5a2cb07556a1ad9bc8b923dbcc00cb3c
  memql-planner:
    repo: znasllc-io/memql
    digest: sha256:350ea124a7a57b9d9bce2bbac50ed673bcfe6c53e4355aa62d4c32a4b98f793f
  memql-workbench:
    repo: znasllc-io/memql
    digest: sha256:45dd55a955351eb569b337802bec91706cb8929293c2407d31ee9903e0a887c0
  memql-examplepack:
    repo: example-org/memql-examplepack
    digest: sha256:c3e3dd89fe052842e13d563731ec9b5a4d0d46f8f5416aae6c47122ffa86fa0a
    builtAgainstEngine: "0.9.9"
  exampleapp:
    repo: example-org/exampleapp
    digest: sha256:dcca35c767456c744491fa205c60b723a715ca261c00450326efddcf8ba8f25d
    builtAgainstEngine: "0.9.9"
`

func TestParseLockfile(t *testing.T) {
	lf, err := ParseLockfile([]byte(lockfileFixture))
	if err != nil {
		t.Fatalf("ParseLockfile: %v", err)
	}
	if lf.Version != "0.9.9" {
		t.Errorf("Version = %q, want 0.9.9", lf.Version)
	}
	if lf.ValidatedAt != "2026-06-02T00:00:00Z" {
		t.Errorf("ValidatedAt = %q", lf.ValidatedAt)
	}
	if lf.Gate != "phase1-digest-cutover" {
		t.Errorf("Gate = %q", lf.Gate)
	}
	if lf.EngineVersion != "0.9.9" {
		t.Errorf("EngineVersion = %q", lf.EngineVersion)
	}
	if len(lf.Components) != 8 {
		t.Fatalf("got %d components, want 8", len(lf.Components))
	}

	identity, ok := lf.Components["memql-identity"]
	if !ok {
		t.Fatal("missing memql-identity component")
	}
	if identity.Repo != "znasllc-io/memql" {
		t.Errorf("memql-identity repo = %q", identity.Repo)
	}
	if identity.Digest != "sha256:5c5ced82097f8d1248081b4054a72f4f7c51265e5efde25fc07135c9cf3da5da" {
		t.Errorf("memql-identity digest = %q", identity.Digest)
	}
	if identity.BuiltAgainstEngine != "" {
		t.Errorf("memql-identity should not carry builtAgainstEngine, got %q", identity.BuiltAgainstEngine)
	}
}

func TestParseLockfileCrossRepoCoherence(t *testing.T) {
	lf, err := ParseLockfile([]byte(lockfileFixture))
	if err != nil {
		t.Fatalf("ParseLockfile: %v", err)
	}
	for _, name := range []string{"memql-examplepack", "exampleapp"} {
		c, ok := lf.Components[name]
		if !ok {
			t.Fatalf("missing cross-repo component %q", name)
		}
		if c.BuiltAgainstEngine != lf.EngineVersion {
			t.Errorf("%s builtAgainstEngine = %q, want %q (engineVersion)",
				name, c.BuiltAgainstEngine, lf.EngineVersion)
		}
	}
}

func TestParseLockfileBadYAML(t *testing.T) {
	if _, err := ParseLockfile([]byte("components: [not : valid")); err == nil {
		t.Fatal("expected error on malformed yaml")
	}
}
