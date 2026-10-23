//go:build linux || darwin

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// The walk, and the two things about it that would be silently wrong.

func mk(t *testing.T, root, rel string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func relsOf(res ScanResult) []string {
	out := make([]string, 0, len(res.Entries))
	for _, entry := range res.Entries {
		out = append(out, entry.Rel)
	}
	return out
}

func TestScanSkipsWhatNoBackupWants(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "keep.txt")
	mk(t, root, "node_modules/left-pad/index.js")
	mk(t, root, ".git/config")
	mk(t, root, ".hidden")
	mk(t, root, "sub/also-kept.txt")

	res, err := Scan(root, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Derived and hidden trees are out; everything else is in. A backup that
	// filled with node_modules is one people turn off.
	want := map[string]bool{"keep.txt": true, "sub/also-kept.txt": true}
	if len(res.Entries) != len(want) {
		t.Fatalf("want %v, got %v", want, relsOf(res))
	}
	for _, rel := range relsOf(res) {
		if !want[rel] {
			t.Errorf("unexpected entry %q", rel)
		}
	}
}

func TestScanIncludesHiddenOnlyWhenAsked(t *testing.T) {
	root := t.TempDir()
	mk(t, root, ".env")
	res, err := Scan(root, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("includeHidden did not include a dotfile: %v", relsOf(res))
	}
	// The reachable positive for the test above: the same tree with the flag
	// off returns nothing, so "skipped" is about the flag and not about the
	// file being missing.
	res, err = Scan(root, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("a dotfile was swept with includeHidden off: %v", relsOf(res))
	}
}

func TestExcludePatternsMeanWhatSomebodyTypingThemMeans(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.tmp")
	mk(t, root, "deep/b.tmp")
	mk(t, root, "drafts/one.txt")
	mk(t, root, "drafts/nested/two.txt")
	mk(t, root, "final.txt")

	res, err := Scan(root, []string{"*.tmp", "drafts/**"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relsOf(res)
	if len(got) != 1 || got[0] != "final.txt" {
		// `*.tmp` has to match a base name ANYWHERE (filepath.Match's `*` does
		// not cross separators, so a naive match would miss deep/b.tmp), and
		// `drafts/**` has to mean a whole subtree, which filepath.Match cannot
		// express at all.
		t.Fatalf("want only final.txt, got %v", got)
	}
}

func TestAnUnparseablePatternExcludesNothing(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.txt")
	res, err := Scan(root, []string{"["}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Fails OPEN, deliberately: a typo that silently excluded the whole folder
	// would report a successful backup of no files, which is the one outcome
	// nobody would notice.
	if len(res.Entries) != 1 {
		t.Fatalf("a malformed pattern excluded a real file: %v", relsOf(res))
	}
}

func TestScanIsOrderedSoTwoSweepsAreComparable(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"c.txt", "a.txt", "b/z.txt", "b/a.txt"} {
		mk(t, root, rel)
	}
	res, err := Scan(root, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relsOf(res)
	want := []string{"a.txt", "b/a.txt", "b/z.txt", "c.txt"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestScanDoesNotFollowSymlinksOutOfTheWatchedFolder(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mk(t, outside, "secret.txt")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := Scan(root, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A link inside a watched folder would otherwise pull in anything on the
	// machine -- the policy veto defeated from the inside.
	if len(res.Entries) != 0 {
		t.Fatalf("the walk followed a symlink out of the folder: %v", relsOf(res))
	}
}

func TestStampComparesOnlyTheCheapHalf(t *testing.T) {
	a := Stamp{Size: 10, ModUnix: 100, SHA256: "aaa"}
	b := Stamp{Size: 10, ModUnix: 100, SHA256: "bbb"}
	// The digest is deliberately NOT part of it: the whole point is to decide
	// whether hashing is needed at all, and a comparison that needed the
	// digest would have to compute it first.
	if !a.Unchanged(b) {
		t.Error("Unchanged compared the digest, which defeats the cheap check")
	}
	if a.Unchanged(Stamp{Size: 11, ModUnix: 100}) {
		t.Error("a size change was not noticed")
	}
	if a.Unchanged(Stamp{Size: 10, ModUnix: 101}) {
		t.Error("a modification time change was not noticed")
	}
}

func TestALedgerSurvivesARoundTripAndAMissingFileIsEmptyNotAnError(t *testing.T) {
	dir := t.TempDir()
	l := LoadLedger(dir, "w-1")
	if len(l.Paths()) != 0 {
		t.Fatal("a fresh ledger was not empty")
	}
	l.Put("/a/b.txt", Record{Stamp: Stamp{Size: 1, ModUnix: 2, SHA256: "d"}, FileID: "f-1", LinkState: "synced"})
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	back := LoadLedger(dir, "w-1")
	rec, ok := back.Get("/a/b.txt")
	if !ok || rec.FileID != "f-1" || rec.Stamp.SHA256 != "d" {
		t.Fatalf("the ledger did not round-trip: %+v", rec)
	}

	// A CORRUPT ledger is an empty one, not a failure. The cost of ignoring it
	// is one expensive sweep; the cost of refusing to run is that somebody's
	// files stop being copied because a JSON file got truncated.
	if err := os.WriteFile(filepath.Join(dir, "backup", "w-2.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadLedger(dir, "w-2"); len(got.Paths()) != 0 {
		t.Error("a corrupt ledger was not treated as empty")
	}
}

// The deny list has to reach every entry, not just the watched root. Checking
// only the root left the stated invariant true of one path and false of
// everything beneath it.
func TestTheDenyListPrunesSubfoldersNotJustTheRoot(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "keep.txt")
	mk(t, root, ".ssh/id_ed25519")
	mk(t, root, "private/notes.txt")

	denied := filepath.Join(root, ".ssh")
	deny := func(path string) error {
		if path == denied || filepath.Dir(path) == denied {
			return os.ErrPermission
		}
		return nil
	}

	// includeHidden ON, so the only thing that can keep .ssh out is the deny.
	res, err := Scan(root, nil, true, deny)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range relsOf(res) {
		if rel == ".ssh/id_ed25519" {
			t.Fatalf("a denied subfolder was walked into the backup: %v", relsOf(res))
		}
	}
	// The reachable positive: with no deny, the same tree DOES yield it, so the
	// assertion above is about the veto rather than about hidden handling.
	res, err = Scan(root, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawKey bool
	for _, rel := range relsOf(res) {
		if rel == ".ssh/id_ed25519" {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatal("the control walk did not reach .ssh at all, so the deny test measures nothing")
	}
}

func TestBothSpellingsOfExcludeEverythingExcludeEverything(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.txt")
	mk(t, root, "deep/b.txt")
	for _, pattern := range []string{"**", "/**"} {
		res, err := Scan(root, []string{pattern}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		// `/**` used to cut down to an empty prefix that matched nothing, so
		// the most emphatic spelling of "exclude everything" excluded nothing.
		if len(res.Entries) != 0 {
			t.Errorf("pattern %q left %v", pattern, relsOf(res))
		}
	}
}

func TestAnExcludedNameIsSkippedEvenWhenItIsAFile(t *testing.T) {
	root := t.TempDir()
	mk(t, root, ".DS_Store")
	mk(t, root, "keep.txt")
	// includeHidden ON, which is the only way .DS_Store used to survive: the
	// excluded-name list was consulted for directories only.
	res, err := Scan(root, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relsOf(res)
	if len(got) != 1 || got[0] != "keep.txt" {
		t.Errorf("want only keep.txt, got %v", got)
	}
}
