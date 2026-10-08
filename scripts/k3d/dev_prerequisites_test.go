package k3d

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The listing may continue after the requested cluster. Under pipefail an
// early reader exit must not turn that successful match into a missing cluster.
func TestDevPrerequisitesDrainClusterListing(t *testing.T) {
	cmd := exec.Command("bash", "-c", `
source "$1"
function docker() { :; }
function kubectl() { :; }
function k3d() {
    printf 'memql 1/1 2/2 true\n'
    local n
    for ((n=0; n<10000; n++)); do
        printf 'another-cluster-%s 1/1 1/1 true\n' "$n"
    done
}
CLUSTER_NAME=memql
check_prerequisites
cap_ok '{}'
`, "test", filepath.Join(repoRoot(t), "scripts", "k3d", "dev.sh"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("healthy cluster listing was refused: %v\n%s", err, out)
	}
}
