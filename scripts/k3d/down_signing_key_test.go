package k3d

import (
	"os"
	"path/filepath"
	"testing"
)

// The local cluster's token signing key outlives a teardown and is deleted by a
// purge (design record 2026-11-21-vendor-accounts-and-api-keys, A7).
//
// A plain `make down`, and the `make up-refresh` that begins with one, must
// KEEP the key: keeping it is what makes the recreated cluster the same issuer,
// so the public keys pasted at a vendor stay good. PURGE=1 is the operator
// saying this cluster is gone for good, and the key is a secret that should not
// outlive it.

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
