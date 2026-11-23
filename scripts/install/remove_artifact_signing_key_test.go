package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE CLUSTER'S TOKEN SIGNING KEY GOES WITH THE CLUSTER THE INSTALL MADE.
//
// `make up` keeps a local cluster's signing key in
// ~/.memql/k3d/<cluster>/service-account.key, so a recreated cluster is the same
// issuer and the public keys given to a vendor stay good (scripts/k3d/up.sh,
// ensure_signing_key). The installer's uninstall deleted the cluster and left
// the key, and the installer lifecycle's round trip failed every leg on it:
// "memqlHome did not return to its baseline".
//
// The key is removed only when the receipt says the install created it
// (k3d.up's result.createdSigningKeyFile, passed as --signing-key), the same
// rule as everything else the uninstall takes back. It is one file at one
// exact shape of path, never a recursive removal, and a path of any other
// shape is refused before anything is removed.

// raSigningKey writes a stand-in signing key under a temp HOME and returns the
// HOME and the key's path.
func raSigningKey(t *testing.T, cluster string) (home, key string) {
	t.Helper()
	home = t.TempDir()
	key = filepath.Join(home, ".memql", "k3d", cluster, "service-account.key")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("not a key, a stand-in for one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, key
}

func raSigningKeyRemoved(t *testing.T, stdout string) bool {
	t.Helper()
	env, _ := raParse(t, stdout)
	var res struct {
		SigningKeyRemoved bool `json:"signingKeyRemoved"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	return res.SigningKeyRemoved
}

func TestRemoveArtifactStackTakesBackTheSigningKeyTheInstallMade(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	home, key := raSigningKey(t, "memql")
	stdout, stderr, code := raRun(t, append(w.env, "HOME="+home), "--kind=stack", "--cluster=memql", "--signing-key="+key)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(w.argv(t, "k3d"), "cluster delete memql") {
		t.Errorf("the cluster was not deleted; argv: %s", w.argv(t, "k3d"))
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("the signing key the install made survived the uninstall (stat err %v)", err)
	}
	if !raSigningKeyRemoved(t, stdout) {
		t.Error("result.signingKeyRemoved is not true after removing the key")
	}
	// Its now-empty directories go with it, and nothing above them.
	for _, dir := range []string{filepath.Dir(key), filepath.Dir(filepath.Dir(key))} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s was left behind empty", dir)
		}
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("the home directory itself was touched: %v", err)
	}
}

// A cluster somebody already deleted by hand still leaves its key to take back.
func TestRemoveArtifactStackTakesBackTheKeyWhenTheClusterIsAlreadyGone(t *testing.T) {
	w := raNewWorld(t, "NAME SERVERS AGENTS LOADBALANCER\n", raImageList)
	home, key := raSigningKey(t, "memql")
	stdout, stderr, code := raRun(t, append(w.env, "HOME="+home), "--kind=stack", "--cluster=memql", "--signing-key="+key)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("the key survived because its cluster was already gone")
	}
	env, _ := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false although the key was removed")
	}
	if strings.Contains(w.argv(t, "k3d"), "cluster delete") {
		t.Error("k3d cluster delete was run against a cluster that does not exist")
	}
}

// A key the receipt does not name is not this install's: a developer who kept
// one with `make down` keeps it.
func TestRemoveArtifactStackLeavesAKeyTheReceiptDoesNotName(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	home, key := raSigningKey(t, "memql")
	stdout, stderr, code := raRun(t, append(w.env, "HOME="+home), "--kind=stack", "--cluster=memql")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("a key the install did not make was removed: %v", err)
	}
	if raSigningKeyRemoved(t, stdout) {
		t.Error("result.signingKeyRemoved is true for a key that was kept")
	}
}

// Only the one file at the one shape of path, and never through a link. Each
// refusal comes before anything is removed, the cluster included.
func TestRemoveArtifactStackRefusesAnySigningKeyPathButTheClustersOwn(t *testing.T) {
	home, key := raSigningKey(t, "memql")
	_, otherKey := raSigningKey(t, "another-cluster")
	outside := filepath.Join(t.TempDir(), "service-account.key")
	if err := os.WriteFile(outside, []byte("somebody else's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkHome, link := raSigningKey(t, "memql")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, home, path string
	}{
		{"another cluster's key", home, otherKey},
		{"a file outside ~/.memql/k3d", home, outside},
		{"a path that climbs out", home, filepath.Join(home, ".memql", "k3d", "memql", "..", "..", "..", "service-account.key")},
		{"a relative path", home, ".memql/k3d/memql/service-account.key"},
		{"a link where the key should be", linkHome, link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := raNewWorld(t, raClusterList, raImageList)
			stdout, _, code := raRun(t, append(w.env, "HOME="+tc.home), "--kind=stack", "--cluster=memql", "--signing-key="+tc.path)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (refused before anything was removed)\nstdout: %s", code, stdout)
			}
			if w.ran(t, "k3d") && strings.Contains(w.argv(t, "k3d"), "cluster delete") {
				t.Error("the cluster was deleted before the bad key path was refused")
			}
		})
	}
	for _, f := range []string{key, otherKey, outside} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed: %v", f, err)
		}
	}
}

// A cluster that was already here is refused whole, and so is its key.
func TestRemoveArtifactStackKeepsTheKeyOfAClusterThatWasAlreadyHere(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	home, key := raSigningKey(t, "memql")
	_, _, code := raRun(t, append(w.env, "HOME="+home), "--kind=stack", "--cluster=memql", "--pre-existing=true", "--signing-key="+key)
	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused)", code)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("the key of a cluster the install did not make was removed: %v", err)
	}
	// The same, with the cluster already gone.
	gone := raNewWorld(t, "NAME SERVERS AGENTS LOADBALANCER\n", raImageList)
	stdout, _, code := raRun(t, append(gone.env, "HOME="+home), "--kind=stack", "--cluster=memql", "--pre-existing=true", "--signing-key="+key)
	if code != 0 && code != 3 {
		t.Fatalf("exit %d, want 0 or 3\nstdout: %s", code, stdout)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("the key of a cluster the install did not make was removed once the cluster was gone: %v", err)
	}
}
