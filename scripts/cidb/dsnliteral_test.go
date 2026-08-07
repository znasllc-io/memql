package cidb

// dsnliteral_test.go -- the fifteenth-copy gate (memql#3149).
//
// The memql#3030 / #3096 defect was a credential in dbtest's own defaultDSN
// (`memql_local_dev`) that matched nothing in this project and diverged from
// what the test files used. Divergence was only POSSIBLE because the shared
// test DSN was written out by hand fourteen times, and `dbtest.DSN()` -- which
// already does exactly the env-or-default resolution all fourteen hand-rolled
// -- had exactly one caller, the gate testing it.
//
// Routing them through DSN() fixes today. This gate is what keeps it fixed:
// without it the fifteenth copy lands the same way the first fourteen did, one
// reasonable-looking `dsn := os.Getenv(...)` at a time, and the class of drift
// comes back.
//
// # What is flagged, and what deliberately is not
//
// Not every `postgres://` string in a test is a copy of anything. The tree is
// full of legitimate ones: deliberately-unreachable probes
// (`postgres://nobody:nobody@127.0.0.1:1/none`), option-plumbing fixtures
// (`postgres://main`, `postgres://direct`), env-precedence fixtures
// (`postgres://tiger-cloud-prod`), and redaction fixtures
// (`postgres://u:sup3rs3cret@h:5432/d`). None of them can drift with the shared
// database, because none of them names it.
//
// So the rule is narrow and says what it means: a connection string that names
// THIS project's shared test database -- user `memql`, or database `memql`.
// That is precisely the string whose fourteen copies caused the defect, and
// precisely what a credential or host change has to keep in step.
//
// Honest limit, stated rather than implied: a copy that reached the same
// database under some other spelling -- a different superuser, a hostname
// alias, a DSN assembled from parts at runtime -- is not caught. This gate
// narrows the everyday path (someone pastes the DSN they found in a sibling
// test), not every conceivable one. That is the same standard doc.go holds the
// rest of this package to.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sharedDBUser and sharedDBName identify this project's shared test database.
// A postgres URI naming either is a copy of the DSN dbtest.DSN() resolves.
const (
	sharedDBUser = "memql"
	sharedDBName = "memql"
)

// dsnResolverDir is the one directory allowed to spell the shared DSN out: it
// is where the resolution LIVES. Matched as a path prefix, because everything
// under it is part of that implementation or its own tests.
const dsnResolverDir = "component/database/dbtest"

// dsnLiteralExemptions are the _test.go files outside dsnResolverDir permitted
// to carry a shared-DSN literal, each with the reason it cannot route through
// dbtest.DSN(). The reason lives HERE, next to the permission, so adding the
// fifteenth copy costs a deliberate edit with a written justification rather
// than a line nobody reviews.
//
// Both entries were CHECKED, not assumed (memql#3149 asks for exactly that):
var dsnLiteralExemptions = map[string]string{
	// Measured 2026-08-07: adding the import and routing this through
	// dbtest.DSN() compiles, creates no import cycle, and leaves
	// `go test ./scripts/cidb/...` GREEN -- harness_test.go declares no Test
	// functions, and scanDBGatedTests is per-file, so it would contribute
	// nothing to the db-gated set. So the reason is NOT "the gate would red".
	//
	// The reason is that an import of dbtest from a _test.go file is this
	// package's DEFINITION of db-gated (doc.go, "Why the import, not the
	// filename"). test/conformance runs in the separate mcp-conformance lane
	// with its own Postgres service and its own HasDB gating -- it never uses
	// dbtest.Unreachable. Importing dbtest would make the gate's central
	// heuristic wrong for this package and eventually demand a second,
	// selfPkg-shaped hardcoded exemption. Planting that to save one string is
	// a bad trade.
	"test/conformance/harness_test.go": "runs in the separate mcp-conformance lane; importing dbtest " +
		"would make it db-gated by this package's own definition and demand a second selfPkg-shaped exemption",

	// Not a DSN anything connects to: it is a line inside a YAML fixture
	// modelling ci.yml's db-tests job, i.e. this gate's own test input. The
	// fixture has to contain the real string for the parser tests to mean
	// anything.
	"scripts/cidb/dbgate_unit_test.go": "the literal is a line inside a YAML fixture modelling ci.yml, " +
		"which is this package's test input rather than a DSN it dials",
}

// dsnLiteral is one shared-DSN string found in a _test.go file.
type dsnLiteral struct {
	file string // repo-relative
	line int
	dsn  string
}

// sharedDSNsIn returns every substring of s that parses as a postgres URI
// naming the shared test database.
//
// It scans for occurrences rather than parsing s whole, because a literal is
// not always a bare DSN: the ci.yml fixture in this package embeds one inside a
// multi-line YAML document. Each candidate runs to the first character that
// cannot appear in a URI.
func sharedDSNsIn(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		rest := s[i:]
		var off int
		switch {
		case strings.HasPrefix(rest, "postgres://"):
			off = 0
		case strings.HasPrefix(rest, "postgresql://"):
			off = 0
		default:
			next := strings.Index(rest[1:], "postgres")
			if next < 0 {
				return out
			}
			i += next + 1
			continue
		}
		cand := rest[off:]
		if end := strings.IndexAny(cand, " \t\r\n\"'`,)"); end >= 0 {
			cand = cand[:end]
		}
		if namesSharedTestDB(cand) {
			out = append(out, cand)
		}
		i += len(cand)
		if len(cand) == 0 {
			i++
		}
	}
	return out
}

// namesSharedTestDB reports whether a postgres URI targets this project's
// shared test database, by user or by database name.
func namesSharedTestDB(candidate string) bool {
	if !strings.HasPrefix(candidate, "postgres://") && !strings.HasPrefix(candidate, "postgresql://") {
		return false
	}
	u, err := url.Parse(candidate)
	if err != nil {
		// Unparseable is not a copy of anything. Refusing to guess here is
		// what keeps the gate from redding on a malformed fixture.
		return false
	}
	if u.User != nil && u.User.Username() == sharedDBUser {
		return true
	}
	return strings.TrimPrefix(u.Path, "/") == sharedDBName
}

// dsnLiteralFindings returns one message per shared-DSN literal that is neither
// inside dsnResolverDir nor exempt.
//
// Pure, for the same reason coverageFindings and laneRunFindings are: inline,
// it would be evaluated only against the real tree, so deleting it would change
// nothing observable.
func dsnLiteralFindings(found []dsnLiteral, exemptions map[string]string) []string {
	var out []string
	for _, l := range found {
		if strings.HasPrefix(l.file, dsnResolverDir+"/") || l.file == dsnResolverDir {
			continue
		}
		if _, ok := exemptions[l.file]; ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d hardcodes the shared test DSN %q. That string is "+
			"resolved by dbtest.DSN() (component/database/dbtest), which returns MEMQL_DATABASE_DSN "+
			"when set and the shared default otherwise -- exactly what a hand-rolled "+
			"`os.Getenv(...)` + literal fallback does, minus the ability to drift.\n\n"+
			"This is the fifteenth copy. The first fourteen are why memql#3030 happened: a "+
			"credential in dbtest's own default diverged from what the test files used, and "+
			"nothing could notice because the string existed in fourteen places.\n\n"+
			"REMEDY: replace the literal (and any `os.Getenv(\"MEMQL_DATABASE_DSN\")` fallback "+
			"around it) with `dbtest.DSN()`. If this file genuinely cannot -- it is not a DSN it "+
			"dials, or importing dbtest would misclassify the package -- add it to "+
			"dsnLiteralExemptions in this file WITH the reason (memql#3149).", l.file, l.line, l.dsn))
	}
	sort.Strings(out)
	return out
}

// scanTestDSNLiterals walks the tree for shared-DSN strings in _test.go files.
//
// String literals only, via the AST: a `postgres://` in a comment is prose, and
// redding on prose would teach people to stop writing it.
func scanTestDSNLiterals(t *testing.T, root string) []dsnLiteral {
	t.Helper()

	var out []dsnLiteral
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "node_modules" || n == "vendor" ||
				strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		// Parsed WITHOUT build-constraint filtering, unlike scanDBGatedTests:
		// a hardcoded DSN behind a build tag is still a copy that drifts, and
		// nothing here depends on the file running in the default build.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", rel, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			for _, dsn := range sharedDSNsIn(val) {
				out = append(out, dsnLiteral{
					file: rel,
					line: fset.Position(lit.Pos()).Line,
					dsn:  dsn,
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// TestNoHardcodedSharedDSNInTests is the gate: the shared test DSN is resolved
// in ONE place, and a new copy reds CI (memql#3149).
func TestNoHardcodedSharedDSNInTests(t *testing.T) {
	root := repoRoot(t)

	found := scanTestDSNLiterals(t, root)
	if len(found) == 0 {
		t.Fatalf("found no shared-DSN literal anywhere, not even inside %s. That is the scanner "+
			"being broken, not the tree: the resolver's own tests carry them by construction "+
			"(memql#3149).", dsnResolverDir)
	}

	for _, f := range dsnLiteralFindings(found, dsnLiteralExemptions) {
		t.Error(f)
	}

	// Every exemption must still name a file that HAS a literal. A stale
	// exemption is a hole nobody knows is open -- the file it names may have
	// been fixed, moved, or deleted, and the entry silently keeps permitting
	// whatever takes its place.
	carries := map[string]bool{}
	for _, l := range found {
		carries[l.file] = true
	}
	for file, reason := range dsnLiteralExemptions {
		if !carries[file] {
			t.Errorf("dsnLiteralExemptions permits %q (%q) but that file carries no shared-DSN "+
				"literal. Remove the entry: a stale exemption is a hole nobody knows is open.", file, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("dsnLiteralExemptions[%q] has no reason. An exemption without one is "+
				"indistinguishable from an oversight.", file)
		}
	}

	// The point of the whole task: DSN() has real callers, not just the gate
	// asserting on it.
	t.Logf("shared-DSN literals: %d total, %d inside %s, %d exempt",
		len(found), countIn(found, dsnResolverDir), dsnResolverDir, len(dsnLiteralExemptions))
}

func countIn(found []dsnLiteral, dir string) int {
	n := 0
	for _, l := range found {
		if strings.HasPrefix(l.file, dir+"/") {
			n++
		}
	}
	return n
}

// TestDSNResolverHasRealCallers pins memql#3149's actual goal. Routing the
// fourteen copies through dbtest.DSN() is the change; this is the assertion
// that it stays routed. Before the change DSN() had exactly ONE caller -- the
// unit test in this package checking it -- which is a resolver nothing
// resolves through.
func TestDSNResolverHasRealCallers(t *testing.T) {
	root := repoRoot(t)

	callers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "node_modules" || n == "vendor" ||
				strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		// selfPkg is the gate testing DSN(), and dsnResolverDir is where it
		// lives. Neither counts as something resolving through it.
		if dir == selfPkg || dir == dsnResolverDir || strings.HasPrefix(dir, dsnResolverDir+"/") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(src), "dbtest.DSN()") {
			callers[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(callers) == 0 {
		t.Fatalf("dbtest.DSN() has no caller outside %s and %s. It is the single resolution "+
			"point the db-gated suites are supposed to share (memql#3149); with no callers it "+
			"is dead code and every suite is hand-rolling the resolution again.",
			dsnResolverDir, selfPkg)
	}
	names := make([]string, 0, len(callers))
	for d := range callers {
		names = append(names, d)
	}
	sort.Strings(names)
	t.Logf("dbtest.DSN() resolves the DSN for %d packages: %v", len(names), names)
}
