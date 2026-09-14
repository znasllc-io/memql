package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// Every entry carries exactly one function, a name unique within its edition,
// an edition the engine reads, and the epic and doc the listing prints -- the
// registry is the migration channel, so an entry that cannot be listed or
// dispatched is a promise the tool does not keep.
func TestEveryRegisteredRewriteIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range registry {
		key := r.edition + "/" + r.name
		n := 0
		for _, set := range []bool{r.plain != nil, r.path != nil, r.tree != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s carries %d functions, want exactly one of plain / path / tree", key, n)
		}
		if seen[key] {
			t.Errorf("%s is registered twice", key)
		}
		seen[key] = true
		if err := resolveEdition(r.edition); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if r.epic == "" || r.doc == "" {
			t.Errorf("%s has no epic or no doc; the usage listing prints both", key)
		}
	}
}

func runMigrate(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = run(args, &out, &errb)
	return out.String(), errb.String(), err
}

// --edition=2026 --rewrite=<name> reaches the rewrite registered under that
// name for that edition.
func TestEditionAndRewriteDispatchToTheRegisteredRewrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "logic.memql")
	if err := os.WriteFile(file, []byte("x := coalesce(a, b)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runMigrate(t, "--edition="+langparser.Edition, "--rewrite=null-coalesce", file)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	if out != "x := a ?? b\n" {
		t.Errorf("rewritten output = %q", out)
	}
	// The edition defaults to the engine's.
	out, _, err = runMigrate(t, "--rewrite", "null-coalesce", file)
	if err != nil || out != "x := a ?? b\n" {
		t.Errorf("default edition: %q, %v", out, err)
	}
}

// A rewrite the edition does not hold is refused as a usage error that names
// every rewrite the edition does hold.
func TestUnknownRewriteIsRefusedNamingTheRegisteredSet(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.memql")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runMigrate(t, "--edition=2026", "--rewrite=expresions", file)
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if !strings.Contains(stderr, `no rewrite "expresions" is registered for edition 2026`) {
		t.Errorf("refusal does not name the rewrite and the edition:\n%s", stderr)
	}
	for _, name := range rewriteNames("2026") {
		if !strings.Contains(stderr, name) {
			t.Errorf("refusal does not list the registered rewrite %q:\n%s", name, stderr)
		}
	}
}

func TestUnknownEditionIsRefusedNamingTheKnownOnes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.memql")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runMigrate(t, "--edition=2031", "--rewrite=null-coalesce", file)
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if !strings.Contains(stderr, `edition "2031" is not one this engine reads`) || !strings.Contains(stderr, langparser.Edition) {
		t.Errorf("refusal does not name the edition and the known ones:\n%s", stderr)
	}
}

// The language-line rewrite declares the engine's line in every domain that
// has none -- and only there: a declared domain keeps what it declared, and
// `_`/`.` directories are skipped exactly as the engine's mounts skip them.
func TestLanguageLineDeclaresEveryUndeclaredDomain(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("shop/concepts.memql", "concept order {\n  id string\n}\n")
	write("billing/queries.memql", "// a domain\n")
	write("billing/memql.toml", "memql = \"1.0\"\nedition = \"2026\"\n# kept as written\n")
	write("_parked/concepts.memql", "concept x {\n  id string\n}\n")
	write("shop/prompts/reply.tmpl", "hello\n") // not a domain of its own

	checked, _, err := runMigrate(t, "--rewrite=language-line", "-check", root)
	if !errors.Is(err, errChanged) || strings.TrimSpace(checked) != filepath.Join(root, "shop", dslfs.ManifestFile) {
		t.Fatalf("-check = %q, %v; want exactly the undeclared domain's manifest", checked, err)
	}
	if _, _, err := runMigrate(t, "--rewrite=language-line", "-w", root); err != nil {
		t.Fatalf("-w: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "shop", dslfs.ManifestFile))
	if err != nil {
		t.Fatalf("shop has no manifest after the rewrite: %v", err)
	}
	m, err := dslfs.ParseManifest(got)
	if err != nil || m.Language != langparser.LanguageVersion || m.Edition != langparser.Edition {
		t.Errorf("written manifest %q parses to %+v, %v", got, m, err)
	}
	kept, _ := os.ReadFile(filepath.Join(root, "billing", dslfs.ManifestFile))
	if !strings.Contains(string(kept), "kept as written") {
		t.Errorf("a declared domain's manifest was rewritten: %q", kept)
	}
	for _, absent := range []string{"_parked/" + dslfs.ManifestFile, "shop/prompts/" + dslfs.ManifestFile, dslfs.ManifestFile} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(absent))); err == nil {
			t.Errorf("%s was written, but it is not a domain", absent)
		}
	}
	// Idempotent: a second run changes nothing.
	if out, _, err := runMigrate(t, "--rewrite=language-line", "-check", root); err != nil || out != "" {
		t.Errorf("second -check = %q, %v; want nothing to change", out, err)
	}
}

// A domain is what the loader calls one (parser.LanguageLineDomainOf): the
// first segment of a .memql file however deep, so a domain holding only a
// sub-namespace (beta/sub/concepts.memql) gets its line -- the loader refuses
// it without one. `_`/`.` segments stay skipped, as the loader skips them.
func TestLanguageLineDeclaresASubNamespaceOnlyDomain(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"beta/sub/concepts.memql":    "concept widget {\n  id string\n}\n",
		"gamma/_wip/concepts.memql":  "concept draft {\n  id string\n}\n",
		".attic/old/concepts.memql":  "concept old {\n  id string\n}\n",
		"delta/sub/_draft.memql":     "concept d {\n  id string\n}\n",
		"delta/sub/prompts/one.tmpl": "hello\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := runMigrate(t, "--rewrite=language-line", "-w", root); err != nil {
		t.Fatalf("-w: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "beta", dslfs.ManifestFile)); err != nil {
		t.Errorf("beta holds only a sub-namespace, which the loader reads as domain beta, but it got no manifest: %v", err)
	}
	for _, absent := range []string{"beta/sub/", "gamma/", ".attic/", "delta/", "delta/sub/", ""} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(absent), dslfs.ManifestFile)); err == nil {
			t.Errorf("%s%s was written, but the loader reads no domain there", absent, dslfs.ManifestFile)
		}
	}
}

// Passed a domain directory rather than the bundle root, the rewrite treats
// the root as the one domain.
func TestLanguageLineOnADomainDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "concepts.memql"), []byte("concept a {\n  id string\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runMigrate(t, "--rewrite=language-line", "-w", root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, dslfs.ManifestFile)); err != nil {
		t.Errorf("a domain directory passed as the root got no manifest: %v", err)
	}
}

// The loader asks no line of a domain the embedded tree owns: it speaks the
// embedded dsl/memql.toml, and every mount skips a directory that collides
// with one. So the rewrite writes nothing over the engine's own dsl/ -- where
// it used to list 41 files no loader would read -- and nothing for a
// core-named directory in a bundle, whether the bundle or the directory itself
// is the root. The non-core domain beside it is the positive control.
func TestLanguageLineWritesNoLineForACoreDomain(t *testing.T) {
	if out, stderr, err := runMigrate(t, "--rewrite=language-line", "-check", filepath.Join("..", "..", "dsl")); err != nil || out != "" {
		t.Errorf("over the engine's own dsl/: -check = %q, %v (%s); want nothing to write -- every domain there is core", out, err, stderr)
	}

	bundle := t.TempDir()
	for rel, content := range map[string]string{
		"library/queries.memql": "// shadows the core library domain\n",
		"shop/queries.memql":    "// a product domain\n",
	} {
		full := filepath.Join(bundle, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checked, _, err := runMigrate(t, "--rewrite=language-line", "-check", bundle)
	if !errors.Is(err, errChanged) || strings.TrimSpace(checked) != filepath.Join(bundle, "shop", dslfs.ManifestFile) {
		t.Errorf("-check = %q, %v; want exactly shop's manifest -- library is a core domain the loader reads from the embedded tree", checked, err)
	}

	// The core-named directory handed over as the root, as one domain.
	if out, _, err := runMigrate(t, "--rewrite=language-line", "-check", filepath.Join(bundle, "library")); err != nil || out != "" {
		t.Errorf("the core-named domain directory as the root: -check = %q, %v; want nothing to write", out, err)
	}
	if out, _, err := runMigrate(t, "--rewrite=language-line", "-check", filepath.Join(bundle, "shop")); !errors.Is(err, errChanged) ||
		strings.TrimSpace(out) != filepath.Join(bundle, "shop", dslfs.ManifestFile) {
		t.Errorf("a product domain directory as the root: -check = %q, %v; want its own manifest at the root", out, err)
	}
}

func TestTreeRewriteRefusesAFileArgument(t *testing.T) {
	file := filepath.Join(t.TempDir(), "concepts.memql")
	if err := os.WriteFile(file, []byte("concept a {\n  id string\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runMigrate(t, "--rewrite=language-line", file)
	if !errors.Is(err, errUsage) || !strings.Contains(stderr, "works on a whole tree; pass a directory") {
		t.Errorf("err = %v, stderr = %q", err, stderr)
	}
}
