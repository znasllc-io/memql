package access

import (
	"github.com/znasllc-io/memql/core/repowalk"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoCockpitCodePathNamesTheRetiredRoleEnum is memql-cockpit#403's second
// acceptance criterion, kept as a test rather than as a promise:
//
//	"No cockpit code path names the USER-ROLE-prefixed enum values, or
//	 compares the caller's cluster role to a legacy slug."
//
// (Paraphrased, because quoting the criterion verbatim would make this file
// match itself -- see the note on the needles below.)
//
// The criterion holds vacuously today: the 2026-08-25 slim-down deleted the
// TUI that used to read the enum, so there is nothing left to migrate. That is
// exactly why it is worth pinning. A vacuous truth is invisible, and the next
// person who needs the caller's role will find the generated getter for
// cluster_role sitting on the pinned proto, use it because it compiles, and
// put the cockpit back where the issue found it -- silently, because a cluster
// whose owner happens to hold one of the five predefined roles renders
// identically either way. It breaks only for the custom role the epic exists
// to serve, which is the case nobody tests by hand.
//
// The needles are assembled from fragments, and the prose above avoids them,
// so every Go file in the repository is scanned with NOTHING excluded --
// including this one. An exclusion list is how a guard like this quietly stops
// covering the file somebody actually edits.
func TestNoCockpitCodePathNamesTheRetiredRoleEnum(t *testing.T) {
	needles := map[string]string{
		"USER_" + "ROLE_":  "the retired enum's values",
		"Cluster" + "Role": "the retired cluster_role field on MyAccessResult",
		"User" + "Role":    "the retired enum type",
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	scanned := 0

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			// `vendor` is skipped for a different reason than the build
			// directories beside it: `go mod vendor` would drop the engine's
			// generated memql.pb.go in here, where the retired enum legitimately
			// appears dozens of times. Scanning it fails the guard three times
			// over a file nobody can edit, with repair advice that means nothing
			// for generated vendored code.
			switch d.Name() {
			case ".git", "bin", "dist", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// One unreadable file is not a reason to abandon the sweep and
			// report a guard failure that names the wrong problem.
			t.Logf("skipping %s: %v", path, readErr)
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		text := string(body)
		for needle, what := range needles {
			if strings.Contains(text, needle) {
				t.Errorf("%s names %q (%s).\n"+
					"The role is a catalog slug with a rank. Read it through internal/access, "+
					"which takes the slug, its name and its rank off the wire by field name.",
					rel, needle, what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A walk that covered nothing would pass silently.
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files under %s; the walk is not covering the repository", scanned, root)
	}
}
