package workbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker writes a docker stand-in that answers `context inspect` with a
// unix socket and `info` with the given JSON, and fails `pull` with the
// given stderr. Enough of Docker for the setup's own sentences.
func fakeDocker(t *testing.T, info, pullStderr string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")
	body := `#!/bin/sh
for a in "$@"; do last="$a"; done
case "$*" in
  *"context inspect"*) echo '"unix:///var/run/docker.sock"' ;;
  *" info "*|*" info") echo '` + info + `' ;;
  *" pull "*) echo '` + pullStderr + `' >&2; exit 1 ;;
  *"image inspect"*) echo '[]' ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// Docker's own words reach the error (memql-cockpit#524): an exit status
// alone names nothing a person can act on.
func TestDockerFailureCarriesDockersOwnWords(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	d := Docker{Binary: fakeDocker(t, `{}`, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")}
	err := d.Pull(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("the pull error must carry Docker's stderr, got %v", err)
	}
}

// Readiness does not reserve an unused second slot. Admission separately
// measures other containers before allowing either workspace to start.
func TestDockerReadyAcceptsOneWorkspaceWithCushion(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	d := Docker{Binary: fakeDocker(t, `{"ID":"x","OSType":"linux","NCPU":4,"MemTotal":2056398848}`, "")}
	if _, err := d.Ready(context.Background()); err != nil {
		t.Fatalf("a VM with room for one workspace must pass, got %v", err)
	}
	small := Docker{Binary: fakeDocker(t, `{"ID":"x","OSType":"linux","NCPU":4,"MemTotal":1610612736}`, "")}
	_, err := small.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "it has 4 CPUs and 1.6 GB") {
		t.Fatalf("a VM without room for the cushion must be refused with what it has, got %v", err)
	}
}

func TestDockerMissingIsNamed(t *testing.T) {
	d := Docker{Binary: filepath.Join(t.TempDir(), "no-docker-here")}
	err := d.Pull(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Docker is not installed") {
		t.Fatalf("a missing docker must be named, got %v", err)
	}
}
