package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --project-manifest must reach main() (memql#6209). A downstream repo
// registers its Application with --app-project=<product> and its own
// AppProject manifest; with the flag unread, the guard right after the
// parameters refused every one of them as "requires --project-manifest".
//
// A manifest path that does not exist proves the flag was read without
// touching docker: an UNREAD flag is refused as absent (exit 2), a READ one
// reaches the next check and is refused as missing (exit 4, naming the path).
func TestUpReadsProjectManifest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "project.yaml")
	cmd := exec.Command("bash", upDomainScript(t),
		"--app-project=demo",
		"--project-manifest="+missing,
		"--repo-url=https://github.com/example/demo.git",
		"--revision=main",
	)
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MEMQL_K3D_PROJECT_MANIFEST=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("up.sh did not exit with a refusal (err %v):\n%s", err, out)
	}
	if code := exitErr.ExitCode(); code != 4 {
		t.Fatalf("exit %d, want 4 (manifest not found) -- exit 2 means --project-manifest was not read:\n%s", code, out)
	}
	if !strings.Contains(string(out), "project manifest not found at "+missing) {
		t.Errorf("refusal does not name the manifest path %s:\n%s", missing, out)
	}
}
