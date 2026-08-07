package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeDomain writes a minimal <root>/<domain>/concepts.memql fixture.
func writeDomain(t *testing.T, root, domain, body string) {
	t.Helper()
	dir := filepath.Join(root, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "concepts.memql"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// treeHas reports whether the unified Tree() can open path.
func treeHas(path string) bool {
	f, err := dsl.Tree().Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// TestMountRuntimeDomainsFromEnv_MountsProductDomains proves the core
// platform-consolidation mechanism (#2472): with MEMQL_DSL_PATH set, a
// product-domain directory on disk becomes visible in Tree() via RegisterTree,
// exactly as a compile-time carrier domain would be — so a product-agnostic
// engine binary loads product DSL delivered at runtime.
func TestMountRuntimeDomainsFromEnv_MountsProductDomains(t *testing.T) {
	root := t.TempDir()
	writeDomain(t, root, "myprod", "// runtime product domain\n")
	// A directory that collides with a core embedded domain must be skipped
	// (the embedded tree owns it; RegisterTree would reject the collision).
	writeDomain(t, root, "cognition", "// should be ignored\n")
	// Soft-disabled/hidden dirs are skipped.
	writeDomain(t, root, "_disabled", "// skipped\n")

	t.Setenv("MEMQL_DSL_PATH", root)

	mounted := dsl.MountRuntimeDomainsFromEnv(nil)
	t.Cleanup(func() { dsl.UnregisterTree("myprod") })

	// Only the non-core, non-hidden domain mounts.
	if len(mounted) != 1 || mounted[0] != "myprod" {
		t.Fatalf("mounted = %v, want [myprod]", mounted)
	}
	if !treeHas("myprod/concepts.memql") {
		t.Fatal("Tree() should expose the runtime-mounted myprod/concepts.memql")
	}
	// The core collision must NOT have been shadowed by the disk copy.
	if got := readTreeFile(t, "cognition/concepts.memql"); wantContains(got, "should be ignored") {
		t.Fatal("core domain 'cognition' must be owned by the embedded tree, not the disk override")
	}
}

// TestMountRuntimeDomainsFromEnv_NoopWhenUnset guards the default path: every
// carrier/engine node that ships its DSL at compile time must be untouched.
func TestMountRuntimeDomainsFromEnv_NoopWhenUnset(t *testing.T) {
	t.Setenv("MEMQL_DSL_PATH", "")
	if got := dsl.MountRuntimeDomainsFromEnv(nil); got != nil {
		t.Fatalf("expected no mounts when MEMQL_DSL_PATH is unset, got %v", got)
	}
}

func readTreeFile(t *testing.T, path string) string {
	t.Helper()
	f, err := dsl.Tree().Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func wantContains(hay, needle string) bool {
	return len(hay) > 0 && len(needle) > 0 && (indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
