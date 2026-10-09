package k3d

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDatabaseImageProbeDrainsLargeListings(t *testing.T) {
	for _, tc := range []struct {
		name, listing string
		want          bool
	}{
		{"present", `awk 'BEGIN { print "docker.io/library/memql-db:16-dev"; for (i = 0; i < 100000; i++) print "docker.io/library/unrelated-image:" i }'`, true},
		{"absent", `printf '%s\n' 'docker.io/library/other:local'`, false},
		{"unavailable", `return 1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `source "$1"
function docker() { ` + tc.listing + `; }
if cluster_holds_db_image; then exit 0; else exit 1; fi
`
			out, err := exec.Command("bash", "-c", script, "probe", filepath.Join(repoRoot(t), "scripts/k3d/dev.sh")).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("present = %v, want %v: %v\n%s", err == nil, tc.want, err, out)
			}
		})
	}
}
