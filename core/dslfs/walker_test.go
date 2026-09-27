package dslfs

import (
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

// TestWalkMemqlFiles_HappyPath locks the basic walk: every .memql
// file under the root, sorted, slash-separated paths.
func TestWalkMemqlFiles_HappyPath(t *testing.T) {
	root := fstest.MapFS{
		"a.memql":            {Data: []byte("// a")},
		"cognition/b.memql":  {Data: []byte("// b")},
		"common/c.memql":     {Data: []byte("// c")},
		"common/sub/d.memql": {Data: []byte("// d")},
	}
	got, err := WalkMemqlFiles(root)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	want := []string{
		"a.memql",
		"cognition/b.memql",
		"common/c.memql",
		"common/sub/d.memql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWalkMemqlFiles_SkipsUnderscoreFiles locks the soft-disable
// rule for individual files.
func TestWalkMemqlFiles_SkipsUnderscoreFiles(t *testing.T) {
	root := fstest.MapFS{
		"a.memql":         {Data: []byte("// a")},
		"_disabled.memql": {Data: []byte("// skip")},
		"sub/_wip.memql":  {Data: []byte("// skip")},
		"sub/ok.memql":    {Data: []byte("// ok")},
	}
	got, err := WalkMemqlFiles(root)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	want := []string{"a.memql", "sub/ok.memql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWalkMemqlFiles_SkipsUnderscoreDirs locks the soft-disable rule
// for entire directories.
func TestWalkMemqlFiles_SkipsUnderscoreDirs(t *testing.T) {
	root := fstest.MapFS{
		"a.memql":               {Data: []byte("// a")},
		"_disabled/x.memql":     {Data: []byte("// skip")},
		"_disabled/sub/y.memql": {Data: []byte("// skip")},
		"keep/z.memql":          {Data: []byte("// z")},
	}
	got, err := WalkMemqlFiles(root)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	want := []string{"a.memql", "keep/z.memql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWalkMemqlFiles_IgnoresNonMemqlExtensions locks that helper
// files (templates, docs, json) aren't returned.
func TestWalkMemqlFiles_IgnoresNonMemqlExtensions(t *testing.T) {
	root := fstest.MapFS{
		"a.memql":         {Data: []byte("// a")},
		"agentReply.tmpl": {Data: []byte("template")},
		"schema.json":     {Data: []byte("{}")},
		"CLAUDE.md":       {Data: []byte("docs")},
		"notes.txt":       {Data: []byte("text")},
	}
	got, err := WalkMemqlFiles(root)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	want := []string{"a.memql"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWalkMemqlFiles_EmptyTree locks the no-files case.
func TestWalkMemqlFiles_EmptyTree(t *testing.T) {
	root := fstest.MapFS{}
	got, err := WalkMemqlFiles(root)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty slice", got)
	}
}

// TestWalkMemqlFiles_RejectsNil locks the nil-root rejection.
func TestWalkMemqlFiles_RejectsNil(t *testing.T) {
	_, err := WalkMemqlFiles(nil)
	if err == nil {
		t.Fatal("expected error for nil root, got nil")
	}
}

// TestFileBasename locks the extension-stripping helper.
func TestFileBasename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo.memql", "foo"},
		{"sub/bar.memql", "bar"},
		{"a/b/c/baz.memql", "baz"},
		{"no_ext", "no_ext"},
		{"weird.foo.memql", "weird.foo"},
	}
	for _, c := range cases {
		got := FileBasename(c.in)
		if got != c.want {
			t.Errorf("FileBasename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHoldsMemqlFile (memql#5426 review) is the one answer to "is this
// directory a DSL domain": a .memql file the walker reads, at any depth -- a
// domain of sub-namespace directories included -- and a soft-disabled file or
// directory counts no more here than in WalkMemqlFiles.
func TestHoldsMemqlFile(t *testing.T) {
	root := fstest.MapFS{
		"nested/billing/concepts.memql":  {Data: []byte("// a")},
		"direct/concepts.memql":          {Data: []byte("// b")},
		"parked/_old/concepts.memql":     {Data: []byte("// c")},
		"parked/_draft.memql":            {Data: []byte("// d")},
		"sidecar/README.md":              {Data: []byte("# e")},
		"templates/prompts/reply.tmpl":   {Data: []byte("f")},
		"_disabled/inner/concepts.memql": {Data: []byte("// g")},
	}
	for dir, want := range map[string]bool{
		"nested":    true,
		"direct":    true,
		"parked":    false,
		"sidecar":   false,
		"templates": false,
		"missing":   false,
	} {
		if got := HoldsMemqlFile(root, dir); got != want {
			t.Errorf("HoldsMemqlFile(%q) = %v, want %v", dir, got, want)
		}
		files, err := WalkMemqlFiles(mustSub(t, root, dir))
		if dir != "missing" && err == nil && (len(files) > 0) != want {
			t.Errorf("HoldsMemqlFile(%q) disagrees with WalkMemqlFiles, which returns %v", dir, files)
		}
	}
}

func mustSub(t *testing.T, root fs.FS, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(root, dir)
	if err != nil {
		t.Fatalf("fs.Sub(%q): %v", dir, err)
	}
	return sub
}
