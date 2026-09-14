package main

// bodies_test.go -- golden cases for `--rewrite=bodies` (epic memql#5370, task
// memql#5373).
//
// Each directory under testdata/bodies is one tree: `in/` holds the files as
// the retired forms wrote them, with `.in` appended to every name so no .memql
// walker in the repository ever reads a form this epic retires; `out/` holds,
// under `.golden`, every file the rewrite changes; `error.txt`, when present,
// holds text the refusal must contain (and then nothing may be written).
//
// Regenerate a golden with MEMQLMIGRATE_UPDATE_GOLDEN=1 and then READ the
// diff: a golden is a reviewed statement of what the rewrite does, not a
// recording of whatever it did last.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

const bodiesTestdata = "testdata/bodies"

// readGoldenTree reads one directory of `.in` or `.golden` files into a files
// map keyed by the path the rewrite sees.
func readGoldenTree(t *testing.T, dir, suffix string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return out
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, suffix) {
			t.Fatalf("%s: every file here ends in %s", p, suffix)
		}
		rel, _ := filepath.Rel(dir, p)
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(strings.TrimSuffix(rel, suffix))] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func goldenCases(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(bodiesTestdata)
	if err != nil {
		t.Fatalf("read %s: %v", bodiesTestdata, err)
	}
	var cases []string
	for _, e := range entries {
		if e.IsDir() {
			cases = append(cases, e.Name())
		}
	}
	sort.Strings(cases)
	if len(cases) == 0 {
		t.Fatal("no golden cases: the test would pass by checking nothing")
	}
	return cases
}

func TestBodiesGolden(t *testing.T) {
	update := os.Getenv("MEMQLMIGRATE_UPDATE_GOLDEN") == "1"
	for _, name := range goldenCases(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(bodiesTestdata, name)
			in := readGoldenTree(t, filepath.Join(dir, "in"), ".in")
			if len(in) == 0 {
				t.Fatalf("%s has no input", dir)
			}
			got, err := rewriteBodies("tree", in)
			wantErr, _ := os.ReadFile(filepath.Join(dir, "error.txt"))
			if len(wantErr) > 0 {
				if err == nil {
					t.Fatalf("the rewrite succeeded; the case says it refuses with %q", strings.TrimSpace(string(wantErr)))
				}
				for _, line := range strings.Split(strings.TrimSpace(string(wantErr)), "\n") {
					if !strings.Contains(err.Error(), line) {
						t.Errorf("the refusal does not contain %q:\n%v", line, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			outDir := filepath.Join(dir, "out")
			if update {
				_ = os.RemoveAll(outDir)
				for p, b := range got {
					dst := filepath.Join(outDir, filepath.FromSlash(p)+".golden")
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(dst, b, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			want := readGoldenTree(t, outDir, ".golden")
			for p, b := range want {
				if g, ok := got[p]; !ok {
					t.Errorf("%s: the golden says the rewrite changes it; it did not", p)
				} else if string(g) != string(b) {
					t.Errorf("%s differs from its golden:\n%s", p, lineDiff(string(b), string(g)))
				}
			}
			for p := range got {
				if _, ok := want[p]; !ok {
					t.Errorf("%s: the rewrite changed it and no golden says it should", p)
				}
			}
		})
	}
}

// TestBodiesRewriteIsIdempotent runs the rewrite over each golden output and
// expects nothing to change: a tree in edition 2026 comes back as it went in.
func TestBodiesRewriteIsIdempotent(t *testing.T) {
	for _, name := range goldenCases(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(bodiesTestdata, name)
			if _, err := os.Stat(filepath.Join(dir, "error.txt")); err == nil {
				t.Skip("a refusal case writes nothing")
			}
			tree := readGoldenTree(t, filepath.Join(dir, "in"), ".in")
			for p, b := range readGoldenTree(t, filepath.Join(dir, "out"), ".golden") {
				tree[p] = b
			}
			again, err := rewriteBodies("tree", tree)
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			for p, b := range again {
				t.Errorf("%s changed on the second run:\n%s", p, lineDiff(string(tree[p]), string(b)))
			}
		})
	}
}

// lineDiff is a small unified-style diff for failure messages.
func lineDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	var b strings.Builder
	n := len(w)
	if len(g) > n {
		n = len(g)
	}
	shown := 0
	for i := 0; i < n && shown < 40; i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			b.WriteString("  line " + strconv.Itoa(i+1) + ":\n    want: " + wl + "\n    got:  " + gl + "\n")
			shown++
		}
	}
	return b.String()
}
