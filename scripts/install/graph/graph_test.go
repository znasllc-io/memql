// graph_test.go -- install(13), memql#3370.
//
// THE ASSERTION THIS FILE EXISTS FOR: uninstall cannot drift behind install.
//
// Install grows. Somebody adds a step, ships it, and the uninstall graph keeps
// passing every test it has -- because nothing was ever asserted ABOUT THE PAIR.
// The failure is silent and it is asymmetric: the install half is exercised on
// every developer machine, the uninstall half only by the person who has
// already decided to leave, and what they find is a machine with a trusted CA
// nobody can account for, a hosts file pointing at a cluster that no longer
// exists, and a k3d cluster quietly holding a port.
//
// So the graphs are checked against EACH OTHER, structurally:
//
//   - every install step that writes a receipt has an uninstall step that
//     reverses it, and every `reverses` names a real receipt-writing step;
//   - every install step that CHANGES the machine either writes a receipt or
//     names the receipt that contains its change (`containedBy`), so a
//     mutation cannot be receipt-less by omission;
//   - teardown order is the install order reversed: if install A needed B
//     first, uninstall of B must wait for uninstall of A (you delete the
//     cluster before you take back the hosts entries it answers on);
//   - elevation is DECLARED, and an uninstall step carries at least the
//     privilege the install step it reverses did;
//   - every script named by either graph is in the engine's real allowlist;
//   - every result field a graph reads -- to verify a step, or to decide
//     whether an artifact pre-existed -- is a field the script actually emits.
//
// IF THIS TEST FAILS, ADD THE MISSING UNINSTALL STEP. Deleting the receipt to
// get to green removes the record that the artifact exists; the artifact does
// not go with it.
package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func bothGraphs(t *testing.T) (install, uninstall *Graph) {
	t.Helper()
	return mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)
}

// --------------------------------------------------------------------------
// the headline: uninstall covers install
// --------------------------------------------------------------------------

func TestEveryReceiptHasAReversal(t *testing.T) {
	in, un := bothGraphs(t)

	reversedBy := map[string]string{}
	for i := range un.Steps {
		s := &un.Steps[i]
		if s.Reverses == "" {
			t.Errorf("uninstall step %q reverses nothing -- an uninstall step that undoes no install step is undoing something nobody recorded", s.ID)
			continue
		}
		target := in.Step(s.Reverses)
		if target == nil {
			t.Errorf("uninstall step %q reverses %q, which is not an install step", s.ID, s.Reverses)
			continue
		}
		if target.Receipt == "" {
			t.Errorf("uninstall step %q reverses install step %q, which writes no receipt -- there is nothing to take back", s.ID, s.Reverses)
		}
		if prev, dup := reversedBy[s.Reverses]; dup {
			t.Errorf("install step %q is reversed twice: by %q and %q", s.Reverses, prev, s.ID)
			continue
		}
		reversedBy[s.Reverses] = s.ID
	}

	for i := range in.Steps {
		s := &in.Steps[i]
		if s.Receipt == "" {
			continue
		}
		if _, ok := reversedBy[s.ID]; !ok {
			t.Errorf("install step %q writes the %q receipt but NOTHING in the uninstall graph reverses it -- "+
				"add the uninstall step; do not delete the receipt to make this pass", s.ID, s.Receipt)
		}
	}
}

// A step that changes the machine and records nothing is drift by omission: it
// passes every other check in this file precisely because it declared nothing.
func TestEveryMutatingInstallStepIsAccountedFor(t *testing.T) {
	in, _ := bothGraphs(t)
	for i := range in.Steps {
		s := &in.Steps[i]
		if s.ReadOnly || s.Receipt != "" || s.ContainedBy != "" {
			continue
		}
		t.Errorf("install step %q changes the machine but declares neither a receipt nor a containedBy -- "+
			"say where this change gets taken back", s.ID)
	}
}

// containedBy is the honest alternative to a receipt, not a way around one: the
// step it names must itself be reversible, or the containment is a fiction.
func TestContainedByNamesAReversibleStep(t *testing.T) {
	in, un := bothGraphs(t)
	reversed := map[string]bool{}
	for i := range un.Steps {
		reversed[un.Steps[i].Reverses] = true
	}
	for i := range in.Steps {
		s := &in.Steps[i]
		if s.ContainedBy == "" {
			continue
		}
		host := in.Step(s.ContainedBy)
		if host == nil {
			t.Errorf("install step %q is containedBy %q, which is not a step in this graph", s.ID, s.ContainedBy)
			continue
		}
		if host.Receipt == "" {
			t.Errorf("install step %q is containedBy %q, which writes no receipt", s.ID, s.ContainedBy)
			continue
		}
		if !reversed[host.ID] {
			t.Errorf("install step %q is containedBy %q, which nothing reverses", s.ID, s.ContainedBy)
		}
		// Containment is only true if the container was already there when the
		// contained step ran.
		if !dependsOnTransitively(in, s.ID, host.ID) {
			t.Errorf("install step %q claims to be containedBy %q but does not depend on it -- "+
				"it could run before the thing that supposedly contains it exists", s.ID, s.ContainedBy)
		}
	}
}

// Teardown is the install order, reversed. If install A needed B in place
// first, then B's removal must wait for A's: the cluster is deleted before the
// hosts entries it answers on and the checkout it was reconciled from, and the
// k3d binary outlives the cluster it is needed to delete.
func TestUninstallOrderIsInstallOrderReversed(t *testing.T) {
	in, un := bothGraphs(t)

	reversal := map[string]string{} // install step id -> uninstall step id
	for i := range un.Steps {
		if r := un.Steps[i].Reverses; r != "" {
			reversal[r] = un.Steps[i].ID
		}
	}

	for i := range in.Steps {
		a := &in.Steps[i]
		ua, ok := reversal[a.ID]
		if !ok {
			continue
		}
		for _, b := range a.DependsOn {
			ub, ok := reversal[b]
			if !ok {
				continue // b leaves nothing behind, so nothing to order
			}
			if !dependsOnTransitively(un, ub, ua) {
				t.Errorf("install %q depends on %q, so uninstall %q (undoing %q) must depend on %q (undoing %q) -- "+
					"otherwise teardown can pull the ground out from under itself",
					a.ID, b, ub, b, ua, a.ID)
			}
		}
	}
}

// dependsOnTransitively reports whether from reaches to through dependsOn edges.
func dependsOnTransitively(g *Graph, from, to string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(id string) bool {
		if seen[id] {
			return false
		}
		seen[id] = true
		s := g.Step(id)
		if s == nil {
			return false
		}
		for _, d := range s.DependsOn {
			if d == to || walk(d) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// --------------------------------------------------------------------------
// no cycles
// --------------------------------------------------------------------------

func TestNeitherGraphHasACycle(t *testing.T) {
	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		if _, err := g.TopoOrder(); err != nil {
			t.Errorf("%s: %v", g.Name, err)
		}
	}
}

// --------------------------------------------------------------------------
// every script is one the engine would actually run
// --------------------------------------------------------------------------

func TestEveryGraphScriptIsAllowlisted(t *testing.T) {
	root := repoRoot(t)
	allow := allowlistPath(t, root)

	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		for i := range g.Steps {
			s := &g.Steps[i]
			rel, ok := allow[s.Script]
			if !ok {
				t.Errorf("%s step %q names script %q, which is not in capabilityScriptAllowlist -- "+
					"the engine would refuse it before exec, so the step is inert on the in-engine path",
					g.Name, s.ID, s.Script)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("%s step %q names script %q -> %q, which does not exist: %v", g.Name, s.ID, s.Script, rel, err)
			}
		}
	}
}

// --------------------------------------------------------------------------
// elevation is declared, never discovered
// --------------------------------------------------------------------------

func TestEveryStepDeclaresAKnownElevation(t *testing.T) {
	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		for i := range g.Steps {
			s := &g.Steps[i]
			switch s.Elevation {
			case ElevationNone, ElevationSudo, ElevationUserTrust:
			default:
				t.Errorf("%s step %q declares elevation %q -- privilege is declared up front, "+
					"never discovered halfway through a run", g.Name, s.ID, s.Elevation)
			}
		}
	}
}

// elevationRank orders the privileges so an uninstall step can be checked
// against the install step it reverses.
func elevationRank(e Elevation) int {
	switch e {
	case ElevationSudo:
		return 2
	case ElevationUserTrust:
		return 1
	default:
		return 0
	}
}

// Taking something back costs at least what putting it there did: a hosts block
// written as root is removed as root. An uninstall step that claims less
// privilege than its install counterpart will fail in the field, on the machine
// of the one person least inclined to debug it.
func TestUninstallElevationCoversInstall(t *testing.T) {
	in, un := bothGraphs(t)
	for i := range un.Steps {
		s := &un.Steps[i]
		target := in.Step(s.Reverses)
		if target == nil {
			continue
		}
		if elevationRank(s.Elevation) < elevationRank(target.Elevation) {
			t.Errorf("uninstall step %q declares elevation %q but reverses install step %q, which needed %q",
				s.ID, s.Elevation, target.ID, target.Elevation)
		}
	}
}

// --------------------------------------------------------------------------
// pre-existence, and the fields the graphs read
// --------------------------------------------------------------------------

func TestEveryReceiptDeclaresPreExistence(t *testing.T) {
	in, _ := bothGraphs(t)
	for i := range in.Steps {
		s := &in.Steps[i]
		if s.Receipt == "" {
			continue
		}
		if s.PreExistingPath == "" {
			t.Errorf("install step %q writes the %q receipt with no preExistingPath -- "+
				"uninstall could not tell an artifact we created from one that was already here", s.ID, s.Receipt)
		}
	}
}

// resultSetRe matches a cap_result_set / cap_result_set_raw emission of a named
// field in a capability script.
var resultSetRe = regexp.MustCompile(`(?m)^\s*cap_result_set(?:_raw)?\s+([A-Za-z][A-Za-z0-9_]*)`)

// scriptResultFields returns the result-envelope fields a capability script
// emits, read out of the script itself.
func scriptResultFields(t *testing.T, root, rel string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	out := map[string]bool{}
	for _, m := range resultSetRe.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s emits no result fields -- either the script or this parser is wrong", rel)
	}
	return out
}

// A graph that reads result.installed from a script that never writes it does
// not fail loudly at run time -- it reads a missing field, decides the step did
// not do its job, and stops an install that in fact worked. Or, for a
// pre-existence signal, it reads absent-as-false and deletes something that was
// already here. Both are caught by asking the script.
func TestGraphsOnlyReadFieldsTheScriptsEmit(t *testing.T) {
	root := repoRoot(t)
	allow := allowlistPath(t, root)
	cache := map[string]map[string]bool{}

	fields := func(script string) map[string]bool {
		if f, ok := cache[script]; ok {
			return f
		}
		rel, ok := allow[script]
		if !ok {
			t.Fatalf("script %q is not allowlisted", script)
		}
		f := scriptResultFields(t, root, rel)
		cache[script] = f
		return f
	}

	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		for i := range g.Steps {
			s := &g.Steps[i]
			emitted := fields(s.Script)

			if f := s.VerifyField(); f != "" && !emitted[f] {
				t.Errorf("%s step %q verifies on result.%s, which %s never emits -- "+
					"the step would read a missing field and call a working install broken",
					g.Name, s.ID, f, s.Script)
			}
			if f, _, ok := s.PreExisting(); ok && !emitted[f] {
				t.Errorf("%s step %q reads pre-existence from result.%s, which %s never emits -- "+
					"an absent signal reads as 'we made it' and uninstall deletes what was already here",
					g.Name, s.ID, f, s.Script)
			}
		}
	}
}

// --------------------------------------------------------------------------
// the uninstall graph's kinds line up with the receipts
// --------------------------------------------------------------------------

// removeArtifactKinds are the kinds remove-artifact.sh actually implements,
// read from the script's dispatch so a receipt naming a kind nobody handles is
// caught here rather than at teardown time.
func removeArtifactKinds(t *testing.T, root string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "scripts", "install", "remove-artifact.sh"))
	if err != nil {
		t.Fatalf("read remove-artifact.sh: %v", err)
	}
	body := string(b)
	start := strings.Index(body, "case \"$kind\" in")
	if start < 0 {
		t.Fatal("could not find the kind dispatch in remove-artifact.sh")
	}
	end := strings.Index(body[start:], "\n    esac")
	if end < 0 {
		end = len(body) - start
	}
	re := regexp.MustCompile(`(?m)^\s{8}([a-zA-Z]+)\)\s+remove_`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body[start:start+end], -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed zero removal kinds -- the parser is broken")
	}
	return out
}

func TestReceiptKindsAreRemovable(t *testing.T) {
	root := repoRoot(t)
	kinds := removeArtifactKinds(t, root)
	in, un := bothGraphs(t)

	for i := range in.Steps {
		s := &in.Steps[i]
		if s.Receipt == "" {
			continue
		}
		if !kinds[s.Receipt] {
			t.Errorf("install step %q writes the %q receipt, but remove-artifact.sh implements no such kind "+
				"(has: %s) -- the artifact would be recorded and then unremovable",
				s.ID, s.Receipt, strings.Join(sortedKeys(kinds), ", "))
		}
	}

	// And the uninstall step that reverses it must pin that same kind.
	for i := range un.Steps {
		s := &un.Steps[i]
		target := in.Step(s.Reverses)
		if target == nil {
			continue
		}
		if got := s.Params["kind"]; got != target.Receipt {
			t.Errorf("uninstall step %q reverses %q (receipt %q) but pins kind=%q",
				s.ID, target.ID, target.Receipt, got)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
