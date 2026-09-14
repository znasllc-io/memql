package dsl

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// The embedded tree's language line (memql#5357). Whether it names the line
// the ENGINE speaks is asserted where the parser is importable
// (component/memql TestLanguageLine_EmbeddedTreeSpeaksTheEngineLine); here it
// must read, and every core domain must be recognised as one.
func TestEmbeddedManifestDeclaresBothKeys(t *testing.T) {
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("dsl/memql.toml does not read: %v", err)
	}
	if m.Language == "" || m.Edition == "" {
		t.Fatalf("dsl/memql.toml must declare both keys, got %+v", m)
	}
	domains := coreDomains()
	if len(domains) == 0 {
		t.Fatal("no core domain found, so the IsCoreDomain checks below examine nothing")
	}
	for _, d := range domains {
		if !IsCoreDomain(d) {
			t.Errorf("IsCoreDomain(%q) = false for a directory the embedded tree ships", d)
		}
	}
	for _, d := range []string{"", "_reference", "memql.toml", "nosuchdomainhere"} {
		if IsCoreDomain(d) {
			t.Errorf("IsCoreDomain(%q) = true; only the embedded domain directories are core", d)
		}
	}
}

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A memql.toml at the root of MEMQL_DSL_PATH is never read -- the mount reads
// domain directories only -- so an author who put the line there must be told.
func TestRuntimeMountWarnsOfARootManifestNoMountReads(t *testing.T) {
	for _, withRoot := range []bool{true, false} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "rootmanifestprod", "queries.memql"), "// a file\n")
		writeFile(t, filepath.Join(root, "rootmanifestprod", "memql.toml"), "memql = \"1.0\"\nedition = \"2026\"\n")
		if withRoot {
			writeFile(t, filepath.Join(root, "memql.toml"), "memql = \"1.0\"\nedition = \"2026\"\n")
		}
		t.Setenv("MEMQL_DSL_PATH", root)
		logger, buf := captureLogger()
		mounted := MountRuntimeDomainsFromEnv(logger)
		for _, d := range mounted {
			UnregisterTree(d)
		}
		warned := strings.Contains(buf.String(), "is never read")
		if withRoot && !warned {
			t.Errorf("a root-level memql.toml under MEMQL_DSL_PATH must be warned about; log:\n%s", buf.String())
		}
		if !withRoot && warned {
			t.Errorf("no root-level memql.toml, yet the mount warned about one; log:\n%s", buf.String())
		}
	}
}

// A memql.toml at the root of a lint or package root is reported back to the
// caller -- which prints it -- rather than logged into a logger memqllint
// discards: the author wrote it believing it governs the tree, and it governs
// nothing (review of memql#5357).
func TestUnreadRootManifestIsReportedToTheCaller(t *testing.T) {
	manifest := &fstest.MapFile{Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")}

	msg, unread := UnreadRootManifest(fstest.MapFS{
		"memql.toml":                     manifest,
		"rootmanifestlint/queries.memql": {Data: []byte("// a file\n")},
		"rootmanifestlint/memql.toml":    manifest,
	})
	if !unread || !strings.Contains(msg, "is never read") || !strings.HasSuffix(msg, "[language_line_unread]") {
		t.Errorf("a root-level memql.toml beside a product domain must be reported, got %v %q", unread, msg)
	}

	// The engine's own dsl/ holds dsl/memql.toml at its root, and there it IS
	// read: it is the embedded line. A root of core domains alone is that tree.
	if msg, unread := UnreadRootManifest(fstest.MapFS{
		"memql.toml":            manifest,
		"library/queries.memql": {Data: []byte("// a file\n")},
	}); unread {
		t.Errorf("a root of core domains is the embedded tree, whose memql.toml is read; got %q", msg)
	}
	if msg, unread := UnreadRootManifest(fstest.MapFS{
		"rootmanifestlint/queries.memql": {Data: []byte("// a file\n")},
	}); unread {
		t.Errorf("no root-level memql.toml, yet one was reported: %q", msg)
	}
}
