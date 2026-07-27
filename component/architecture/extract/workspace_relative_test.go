package extract

import "testing"

// TestWorkspaceRelative unit-tests the containment rule that keeps the
// committed architecture model reproducible across machines (memql#2844).
//
// filepath.Rel walks upwards happily, so a file in GOROOT or the module cache
// came out as a `../../..` chain whose LENGTH encodes how deep the checkout
// sits on the generating machine's disk. Twenty-seven nodes carried such paths,
// so two checkouts of the same commit produced different artifacts and the
// drift gate was unsatisfiable anywhere but the machine that regenerated.
//
// The invariant this establishes is re-asserted over the committed artifact by
// TestNoSourceRefEscapesTheWorkspace one package up; this is the rule itself.
func TestWorkspaceRelative(t *testing.T) {
	const root = "/ws"
	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"inside", "/ws/a/b.go", "a/b.go", true},
		{"at the root", "/ws/b.go", "b.go", true},
		{"the root itself", "/ws", ".", true},
		{"one level out", "/b.go", "", false},
		{"GOROOT shaped", "/usr/local/go/src/sync/mutex.go", "", false},
		{"module cache shaped", "/home/u/go/pkg/mod/x@v1/f.go", "", false},
		// The case a naive strings.HasPrefix(root) check gets WRONG: a sibling
		// directory sharing the root's name prefix is not inside it.
		{"shared name prefix is not containment", "/wsx/f.go", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := workspaceRelative(root, tc.path)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("workspaceRelative(%q, %q) = (%q, %v), want (%q, %v)",
					root, tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}
}
