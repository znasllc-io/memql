package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backup.roots -- the machine's own veto over a folder the cluster named
// (memql#4841).
//
// The default-deny half is the one worth a test with a name this long: an
// empty allow list is the state of every machine upgrading into this feature,
// and if it ever came to mean "all", every one of them would start uploading
// whatever any watch row pointed at.

func policyWith(t *testing.T, body string) *Policy {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackupRootsIsDefaultDeny(t *testing.T) {
	p := policyWith(t, "shell:\n  allow: [ls]\n")
	err := p.CheckBackupPath("/Users/ana/Clients")
	if err == nil {
		t.Fatal("a machine with no backup.roots accepted a folder -- default-deny is gone")
	}
	// The refusal has to name the repair: it is reported to the cluster and
	// rendered verbatim in the Files app, where it is the only account of what
	// is wrong that a person will see.
	if !strings.Contains(err.Error(), "backup.roots") {
		t.Errorf("the refusal does not name the setting: %v", err)
	}
}

func TestBackupRootsAdmitsAListedFolderAndWhatIsBeneathIt(t *testing.T) {
	root := t.TempDir()
	p := policyWith(t, "backup:\n  roots:\n    - "+root+"\n")
	for _, path := range []string{root, filepath.Join(root, "2026"), filepath.Join(root, "2026", "acme")} {
		if err := p.CheckBackupPath(path); err != nil {
			t.Errorf("a listed root refused %q: %v", path, err)
		}
	}
	// The reachable positive's other half: something outside is still refused,
	// so the acceptances above are about the list rather than about the check
	// having stopped working.
	if err := p.CheckBackupPath(filepath.Dir(root)); err == nil {
		t.Error("a folder ABOVE the listed root was accepted")
	}
}

func TestTheFsDenyListStillAppliesToBackups(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	p := policyWith(t, "backup:\n  roots:\n    - "+root+"\nfs:\n  deny:\n    - "+secret+"\n")
	// A directory somebody marked never-touch is not a directory to upload
	// either, and the deny list is checked BEFORE the allow list so a root
	// that contains it cannot re-admit it.
	if err := p.CheckBackupPath(secret); err == nil {
		t.Error("a denied folder was accepted for backup because a root contained it")
	}
	if err := p.CheckBackupPath(root); err != nil {
		t.Errorf("the root itself was refused: %v", err)
	}
}

func TestBackupRootsSurviveASIGHUPAndMergeRatherThanReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	first := filepath.Join(dir, "one")
	second := filepath.Join(dir, "two")
	if err := os.WriteFile(path, []byte("backup:\n  roots:\n    - "+first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("backup:\n  roots:\n    - "+second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	// MERGE, like every other scalar allow list here: a SIGHUP adds a folder
	// without a restart, and without silently un-authorising the one that was
	// already being backed up.
	if err := p.CheckBackupPath(second); err != nil {
		t.Errorf("a reloaded root was not honoured: %v", err)
	}
	if err := p.CheckBackupPath(first); err != nil {
		t.Errorf("a reload dropped a root that was already in force: %v", err)
	}
}

func TestBackupRootsAreCopiedOut(t *testing.T) {
	p := policyWith(t, "backup:\n  roots:\n    - /a\n")
	roots := p.BackupRoots()
	if len(roots) != 1 {
		t.Fatalf("want one root, got %v", roots)
	}
	roots[0] = "/mutated"
	// A caller that appended to a shared slice would be editing the live
	// policy under a lock it no longer holds -- the reason every accessor
	// here copies.
	if again := p.BackupRoots(); again[0] != "/a" {
		t.Errorf("BackupRoots handed out the live slice: %v", again)
	}
}
