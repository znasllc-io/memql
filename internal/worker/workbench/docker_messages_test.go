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

// A VM configured with 4 GB reports a little less once its kernel takes a
// share; that is enough for two 1.5 GiB workspaces with a fifth free.
func TestDockerReadyAcceptsAFourGigabyteVM(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	// 3.83 GiB, what Docker Desktop reports for its "4 GB" setting.
	d := Docker{Binary: fakeDocker(t, `{"ID":"x","OSType":"linux","NCPU":4,"MemTotal":4112797696}`, "")}
	if _, err := d.Ready(context.Background()); err != nil {
		t.Fatalf("a 4 GB VM must pass, got %v", err)
	}
	small := Docker{Binary: fakeDocker(t, `{"ID":"x","OSType":"linux","NCPU":4,"MemTotal":3221225472}`, "")}
	_, err := small.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "it has 4 CPUs and 3.2 GB") {
		t.Fatalf("a 3 GiB VM must be refused with what it has, got %v", err)
	}
}

func TestDockerMissingIsNamed(t *testing.T) {
	d := Docker{Binary: filepath.Join(t.TempDir(), "no-docker-here")}
	err := d.Pull(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Docker is not installed") {
		t.Fatalf("a missing docker must be named, got %v", err)
	}
}
