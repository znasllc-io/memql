package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultPolicy_ShellRlimitsNonZero asserts the post-Wave-6
// hardening: the default ShellPolicy ships non-zero rlimits so a
// runaway shell exec can't exhaust the user session's CPU / memory
// ceiling. Earlier defaults left these at zero (inherit parent),
// which was the runaway-blast-radius footgun.
func TestDefaultPolicy_ShellRlimitsNonZero(t *testing.T) {
	p := DefaultPolicy()
	if got := p.shell.MaxCPUSeconds; got <= 0 {
		t.Errorf("MaxCPUSeconds = %d, want > 0 (default cap)", got)
	}
	if got := p.shell.MaxMemoryMB; got <= 0 {
		t.Errorf("MaxMemoryMB = %d, want > 0 (default cap)", got)
	}
	if got := p.shell.MaxOpenFiles; got <= 0 {
		t.Errorf("MaxOpenFiles = %d, want > 0 (default cap)", got)
	}
}

// TestDefaultPolicy_AllowDenyShape sanity-checks the curated lists
// haven't drifted -- a regression here is a sign someone deleted
// the curated allow / deny by mistake (the lists are intentionally
// conservative).
func TestDefaultPolicy_AllowDenyShape(t *testing.T) {
	p := DefaultPolicy()
	if len(p.shell.Allow) == 0 {
		t.Fatalf("ShellPolicy.Allow must not be empty -- default allow-list missing")
	}
	// The deny list is hard-coded to a curated set; spot-check a few
	// known entries so an accidental wipe of the slice is caught.
	mustDeny := []string{"rm", "dd", "sudo", "kill"}
	denySet := make(map[string]bool, len(p.shell.Deny))
	for _, d := range p.shell.Deny {
		denySet[d] = true
	}
	for _, want := range mustDeny {
		if !denySet[want] {
			t.Errorf("default deny list missing %q", want)
		}
	}
}

// TestDenyPaths_ExpandsTheDenyList: an app session is handed fs.deny as paths
// its app may neither read nor write, and a sandbox cannot resolve "~". So
// the list comes back expanded -- the defaults and the owner's own entries,
// merged as CheckPath sees them -- and as a copy a SIGHUP cannot rewrite
// under a session that is reading it.
func TestDenyPaths_ExpandsTheDenyList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("fs:\n  deny:\n    - ~/secrets\n    - /srv/private\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}

	got := p.DenyPaths()
	has := map[string]bool{}
	for _, d := range got {
		has[d] = true
		if d == "~" || len(d) > 1 && d[:2] == "~/" {
			t.Errorf("DenyPaths() kept %q unexpanded", d)
		}
	}
	for _, want := range []string{
		filepath.Join(home, ".ssh"), // a default
		filepath.Join(home, "secrets"),
		"/srv/private",
	} {
		if !has[want] {
			t.Errorf("DenyPaths() = %v, want %s in it", got, want)
		}
	}

	got[0] = "/mutated"
	if p.DenyPaths()[0] == "/mutated" {
		t.Error("DenyPaths() handed out the policy's own slice")
	}
	var nilPolicy *Policy
	if nilPolicy.DenyPaths() != nil {
		t.Error("a nil policy denied something")
	}
}
