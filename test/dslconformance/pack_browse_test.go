package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"testing"
	"testing/fstest"
)

// TestListPackDomains_EmbeddedCore verifies the enumerator surfaces the
// real baked-in core domains and labels them "embedded". cognition is a
// known core domain with multiple construct files.
func TestListPackDomains_EmbeddedCore(t *testing.T) {
	domains := dsl.ListPackDomains()
	if len(domains) == 0 {
		t.Fatal("expected at least one embedded domain")
	}

	byName := map[string]dsl.PackDomain{}
	for _, d := range domains {
		byName[d.Name] = d
		if d.Origin != "embedded" && !startsWith(d.Origin, "pack:") {
			t.Errorf("domain %q has unexpected origin %q", d.Name, d.Origin)
		}
	}

	cog, ok := byName["cognition"]
	if !ok {
		t.Fatal("expected core domain 'cognition' in the embedded tree")
	}
	if cog.Origin != "embedded" {
		t.Errorf("cognition origin = %q, want embedded", cog.Origin)
	}
	if cog.FileCount == 0 {
		t.Error("cognition should report a non-zero file count")
	}

	// The "_reference" authoring skeleton must NOT appear as a domain.
	if _, present := byName["_reference"]; present {
		t.Error("_reference skeleton dir leaked into the domain list")
	}

	// Sorted by name.
	for i := 1; i < len(domains); i++ {
		if domains[i-1].Name > domains[i].Name {
			t.Errorf("domains not sorted: %q before %q", domains[i-1].Name, domains[i].Name)
		}
	}
}

// TestPackBrowse_RegisteredPack registers a throwaway pack subtree and
// verifies it shows up as a pack domain, its files enumerate, and a file
// reads back with the pack: origin.
func TestPackBrowse_RegisteredPack(t *testing.T) {
	const dom = "fixturepack2127"
	pack := fstest.MapFS{
		"queries.memql":         {Data: []byte("query x q { }")},
		"prompts/reply.tmpl":    {Data: []byte("hi {{.name}}")},
		"prompts/prompts.memql": {Data: []byte("prompt reply { }")},
		"notes.txt":             {Data: []byte("ignored non-memql file")},
	}
	dsl.RegisterTree(dom, pack)
	t.Cleanup(func() { dsl.UnregisterTree(dom) })

	// Domain listing includes the pack with the right origin + count
	// (3 browsable files: queries.memql, prompts/reply.tmpl, prompts/prompts.memql).
	var found *dsl.PackDomain
	for _, d := range dsl.ListPackDomains() {
		if d.Name == dom {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatalf("registered pack %q missing from domain list", dom)
	}
	if found.Origin != "pack:"+dom {
		t.Errorf("origin = %q, want pack:%s", found.Origin, dom)
	}
	if found.FileCount != 3 {
		t.Errorf("file count = %d, want 3 (notes.txt excluded)", found.FileCount)
	}

	// File listing: 3 browsable files, sorted, non-memql excluded.
	files := dsl.ListPackFiles(dom)
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %+v", len(files), files)
	}
	want := []string{"prompts/prompts.memql", "prompts/reply.tmpl", "queries.memql"}
	for i, w := range want {
		if files[i].Path != w {
			t.Errorf("files[%d] = %q, want %q", i, files[i].Path, w)
		}
	}

	// Read a file back.
	src, origin, err := dsl.ReadPackFile(dom, "queries.memql")
	if err != nil {
		t.Fatalf("ReadPackFile: %v", err)
	}
	if string(src) != "query x q { }" {
		t.Errorf("source = %q", string(src))
	}
	if origin != "pack:"+dom {
		t.Errorf("origin = %q, want pack:%s", origin, dom)
	}
}

// TestReadPackFile_PathTraversalRejected guards the path-validation:
// "..", absolute paths, and non-browsable suffixes must be refused.
func TestReadPackFile_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		domain, path string
	}{
		{"cognition", "../identity/concepts.memql"}, // escape attempt
		{"cognition", "/etc/passwd"},                // absolute
		{"cognition", "queries.go"},                 // non-browsable suffix
		{"cognition", ""},                           // empty
		{"", "queries.memql"},                       // empty domain
		{"foo/bar", "queries.memql"},                // slash in domain
		{"_reference", "_concept/x.memql"},          // underscore domain
	}
	for _, c := range cases {
		if _, _, err := dsl.ReadPackFile(c.domain, c.path); err == nil {
			t.Errorf("ReadPackFile(%q, %q) should have errored", c.domain, c.path)
		}
	}
}

// TestListPackFiles_UnknownDomain returns empty (not a panic / error).
func TestListPackFiles_UnknownDomain(t *testing.T) {
	if files := dsl.ListPackFiles("definitely-not-a-domain"); len(files) != 0 {
		t.Errorf("expected empty file list for unknown domain, got %d", len(files))
	}
}

// TestPackFileExists covers the handler's pre-read validation helper.
func TestPackFileExists(t *testing.T) {
	if !dsl.PackFileExists("cognition", "queries.memql") {
		t.Error("cognition/queries.memql should exist")
	}
	if dsl.PackFileExists("cognition", "no-such-file.memql") {
		t.Error("nonexistent file reported as existing")
	}
	if dsl.PackFileExists("cognition", "../identity/concepts.memql") {
		t.Error("traversal path reported as existing")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
