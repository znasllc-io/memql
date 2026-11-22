package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The local cluster's token signing key outlives a teardown and is deleted by a
// purge (design record 2026-11-21-vendor-accounts-and-api-keys, A7).
//
// A plain `make down` must KEEP the key, and so must `make up-refresh`, whose
// own teardown is a purge (it asks for the key to be kept): keeping it is what
// makes the recreated cluster the same issuer, so the public keys pasted at a
// vendor stay good. An operator's own PURGE=1 is them saying this cluster is
// gone for good, and the key is a secret that should not outlive it.

func writeSigningKey(t *testing.T, home string) string {
	t.Helper()
	key := signingKeyPath(home, "memql")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(placeholderSigningKey("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDownKeepsTheSigningKey(t *testing.T) {
	home := t.TempDir()
	key := writeSigningKey(t, home)

	code, _, _ := downRunWithHome(t, home, nil)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("a plain teardown deleted the signing key (%v): the next cluster would be a new issuer, "+
			"and every key pasted at a vendor would be stale", err)
	}
}

func TestDownPurgeFalseKeepsTheSigningKey(t *testing.T) {
	home := t.TempDir()
	key := writeSigningKey(t, home)

	code, _, _ := downRunWithHome(t, home, nil, "--purge=false")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("--purge=false deleted the signing key: %v", err)
	}
}

func TestDownPurgeDeletesTheSigningKey(t *testing.T) {
	home := t.TempDir()
	key := writeSigningKey(t, home)
	// Something else under the same root must survive: only this cluster's key
	// is the purge's to delete.
	other := signingKeyPath(home, "memql-other")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("another cluster's key"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, _ := downRunWithHome(t, home, nil, "--purge=true")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("a purge left the signing key behind (stat: %v): a secret that mints tokens for a cluster "+
			"that no longer exists", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a purge of one cluster deleted another cluster's signing key: %v", err)
	}
}

// A purge with no key to delete is not a failure: a cluster created before the
// key existed, or on a machine with no openssl, has none.
func TestDownPurgeWithNoSigningKeySucceeds(t *testing.T) {
	code, _, _ := downRunWithHome(t, t.TempDir(), nil, "--purge=true")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

// A clean-slate bring-up tears the cluster down WITH a purge (bringup.sh,
// nuke), and that purge must not take the signing key with it: keeping the key
// across `make up-refresh` is the reason the key is kept at all. The teardown
// therefore takes an explicit "keep the signing key", which only that caller
// passes. An operator's own `make down PURGE=1` still deletes it.
func TestDownPurgeKeepsTheSigningKeyWhenAsked(t *testing.T) {
	home := t.TempDir()
	key := writeSigningKey(t, home)

	code, out, _ := downRunWithHome(t, home, nil, "--purge=true", "--keep-signing-key=true")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("the purge deleted a signing key it was asked to keep: %v", err)
	}
	if !out.Result.Purged {
		t.Error("keeping the signing key must not stop the purge of the kubeconfig context")
	}
}

// The clean-slate path itself, end to end through the real down.sh: after
// `make up-refresh`'s teardown the key is still there, so the cluster that is
// created next is the same issuer.
func TestCleanSlateBringUpKeepsTheSigningKey(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	home := t.TempDir()
	key := writeSigningKey(t, home)

	dir := t.TempDir()
	for name, body := range map[string]string{"k3d": downStubK3d, "kubectl": downStubKubectl, "docker": downStubDocker} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	calls := filepath.Join(dir, "calls.log")
	bringup, err := filepath.Abs("bringup.sh")
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(dir, "harness.sh")
	script := "#!/usr/bin/env bash\nset -euo pipefail\nsource \"" + bringup + "\"\nCLUSTER_NAME=memql\nSTEP_NUKE=\"[1/5]\"\nnuke\n"
	if err := os.WriteFile(harness, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOWN_CALLS="+calls,
		"PURGE=", "HOME="+home, "DOWN_DOCKER_DOWN=", "DOWN_MEDIA_RUNNING=", "DOWN_MEDIA_DOWN_FAILS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the clean-slate teardown failed: %v\n%s", err, out)
	}

	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the teardown ran nothing: %v", err)
	}
	if !strings.Contains(string(logged), "k3d cluster delete memql") {
		t.Fatalf("the cluster was not torn down:\n%s", logged)
	}
	if !strings.Contains(string(logged), "kubectl config delete-context k3d-memql") {
		t.Fatalf("the clean slate did not purge the kubeconfig context:\n%s", logged)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("`make up-refresh` deleted the signing key (%v): the recreated cluster is a new issuer, "+
			"and every key pasted at a vendor is stale", err)
	}
}
