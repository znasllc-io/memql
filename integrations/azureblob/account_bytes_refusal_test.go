package azureblob

import (
	"context"
	"errors"
	"testing"
)

// The account export tells a refused reference from a failure of storage by
// ErrBytesReferenceRefused (integrations/accounts/export_bytes.go): a refusal
// is recorded and the run goes on, anything else fails the run.
func TestAccountBytesRefusalsAreTheSentinel(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	m.put(exportTestContainer, "library/u1/f1/report.pdf", []byte("pdf-bytes"))
	reader := exportTestClient(t, m).AccountBytes()
	for _, ref := range []string{
		"library/u2/f9/theirs.pdf",
		"https://blob.test/elsewhere/library/u1/f1/report.pdf",
		"exports/accounts/e1/account.memqlx",
		"blob://library",
		"library/u1/../../etc/passwd",
		"  ",
	} {
		if _, _, _, err := reader.Stat(ctx, ref, libraryPlace); !errors.Is(err, ErrBytesReferenceRefused) {
			t.Errorf("Stat(%q) = %v, want ErrBytesReferenceRefused", ref, err)
		}
	}
	if _, err := reader.List(ctx, "blob://library/u2/f9", libraryPlace); !errors.Is(err, ErrBytesReferenceRefused) {
		t.Errorf("List outside its place = %v, want ErrBytesReferenceRefused", err)
	}
	if _, err := reader.Open(ctx, "library/u2/f9/theirs.pdf", libraryPlace); !errors.Is(err, ErrBytesReferenceRefused) {
		t.Errorf("Open outside its place = %v, want ErrBytesReferenceRefused", err)
	}
}

// Resolve answers the key Stat, List and Open read, by their own parse.
func TestAccountBytesResolveIsTheReadersOwnParse(t *testing.T) {
	reader := exportTestClient(t, newMemoryBlobService()).AccountBytes()
	for _, tc := range []struct {
		ref, key      string
		tree, shipped bool
	}{
		{ref: "https://blob.test/memql/library/u1/f1/report.pdf", key: "library/u1/f1/report.pdf"},
		{ref: "library/u1/f1/report.pdf", key: "library/u1/f1/report.pdf"},
		{ref: "blob://sites/s1/v3/", key: "sites/s1/v3", tree: true},
		{ref: "file:///app/os", shipped: true},
	} {
		key, tree, shipped, err := reader.Resolve(tc.ref)
		if err != nil || key != tc.key || tree != tc.tree || shipped != tc.shipped {
			t.Errorf("Resolve(%q) = %q tree=%v shipped=%v err=%v", tc.ref, key, tree, shipped, err)
		}
	}
	if _, _, _, err := reader.Resolve("https://blob.test/elsewhere/library/u1/f1/report.pdf"); !errors.Is(err, ErrBytesReferenceRefused) {
		t.Errorf("Resolve of another container = %v, want ErrBytesReferenceRefused", err)
	}
}
