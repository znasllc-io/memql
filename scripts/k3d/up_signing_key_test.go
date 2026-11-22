package k3d

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A local cluster keeps its token signing key across recreation (design record
// 2026-11-21-vendor-accounts-and-api-keys, A7).
//
// WHY. A vendor federates with a local cluster by being given the cluster's
// public keys (make federation-keys). `make up-refresh` deletes the cluster and
// creates it again, and a new cluster signs with a new key: every pasted key is
// stale and every sign-in is refused until somebody pastes again. A key kept
// in a file and mounted where k3s looks for its own makes the recreated
// cluster the same issuer with the same keys. Proven on a throwaway cluster
// (k3s v1.32.13, one server and two): the published keys were identical across
// delete and create.
//
// The file IS A SECRET: whoever holds it can mint tokens a vendor accepts for
// this cluster's rule. So it lives outside the repository, is readable by its
// owner only, and `make down PURGE=1` deletes it.

const signingKeyMount = "/var/lib/rancher/k3s/server/tls/service.key@server:*"

func signingKeyPath(home, cluster string) string {
	return filepath.Join(home, ".memql", "k3d", cluster, "service-account.key")
}

// pemKeyKind is the label of a PKCS#1 key file, which is what k3s writes for
// itself and what the spike proved. It is ASSEMBLED rather than written out, so
// that no test file here holds a BEGIN marker followed by an END marker: a
// secret scanner reads that pair as a key, and these tests only ever put a
// placeholder between them.
const pemKeyKind = "RSA PRIVATE " + "KEY"

// pemKeyHeader is the first line of such a file.
func pemKeyHeader() string { return "-----BEGIN " + pemKeyKind + "-----" }

// placeholderSigningKey is a file shaped like a key that is not one.
func placeholderSigningKey(body string) string {
	return pemKeyHeader() + "\n" + body + "\n-----END " + pemKeyKind + "-----\n"
}

func createClusterIn(t *testing.T, home, extraPrelude string) (out, calls string, code int) {
	t.Helper()
	prelude := "HOME='" + home + "'\nCLUSTER_NAME=memql\n" + extraPrelude
	return upRun(t, listing(), prelude, "create_cluster")
}

func TestCreateClusterMountsAPersistentSigningKey(t *testing.T) {
	home := t.TempDir()
	out, calls, code := createClusterIn(t, home, "")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}

	key := signingKeyPath(home, "memql")
	want := "--volume " + key + ":" + signingKeyMount
	var create string
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "k3d cluster create") {
			create = line
		}
	}
	if create == "" {
		t.Fatalf("the cluster was not created:\n%s", calls)
	}
	if !strings.Contains(create, want) {
		t.Fatalf("the cluster was created without its signing key mounted on every server.\nwant: %s\ngot:  %s", want, create)
	}

	info, err := os.Stat(key)
	if err != nil {
		t.Fatalf("no signing key was written: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("signing key mode = %o, want 600", mode)
	}
	dir, err := os.Stat(filepath.Dir(key))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Errorf("signing key directory mode = %o, want 700", mode)
	}
	raw, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), pemKeyHeader()) {
		t.Errorf("the signing key is not an RSA key in PKCS#1 PEM: %.40q", raw)
	}
	// The key is a secret, and the bring-up's output is pasted into issues.
	if strings.Contains(out, pemKeyKind) {
		t.Fatal("the bring-up printed private key material")
	}
}

// The whole point: a key that already exists is the one the new cluster gets.
func TestCreateClusterReusesTheSigningKeyItFinds(t *testing.T) {
	home := t.TempDir()
	key := signingKeyPath(home, "memql")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	kept := placeholderSigningKey("the key from before the recreation")
	if err := os.WriteFile(key, []byte(kept), 0o600); err != nil {
		t.Fatal(err)
	}

	out, calls, code := createClusterIn(t, home, "")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	raw, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != kept {
		t.Fatal("the existing signing key was replaced: the recreated cluster would be a new issuer, " +
			"and every key pasted at a vendor would be stale")
	}
	if !strings.Contains(calls, "--volume "+key+":"+signingKeyMount) {
		t.Fatalf("the existing key was not mounted:\n%s", calls)
	}
}

// Without openssl there is no key to mount. The cluster is still created, as it
// always was, and the bring-up says what that costs.
func TestCreateClusterWithoutOpensslStillCreatesTheCluster(t *testing.T) {
	home := t.TempDir()
	out, calls, code := createClusterIn(t, home, "function openssl() { return 127; }\n")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(calls, "k3d cluster create") {
		t.Fatalf("the cluster was not created:\n%s", calls)
	}
	if strings.Contains(calls, signingKeyMount) {
		t.Fatalf("a signing key that could not be generated was mounted anyway:\n%s", calls)
	}
	if _, err := os.Stat(signingKeyPath(home, "memql")); err == nil {
		t.Fatal("a signing key file exists although none could be generated")
	}
	if !strings.Contains(out, "signing key") {
		t.Errorf("the bring-up did not say the cluster has no persistent signing key:\n%s", out)
	}
}
