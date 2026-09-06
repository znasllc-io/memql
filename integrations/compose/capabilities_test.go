package compose

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestCapabilityNamesMatchTheDSL is the gate that makes the five builtins in
// dsl/compose/builtins.memql actually reachable.
//
// A capability the DSL names and the registry LACKS is not a quiet no-op: it
// is a boot-time resolution failure on every node type, because
// AuditIntegrationExecutors resolves every enabled integration.* builtin
// against the registry at Init and strict boot refuses the report. And a
// capability the registry holds that the DSL does not name is dead code that
// nothing can call.
//
// So the assertion is SET EQUALITY, read out of the .memql file rather than
// restated here -- a list written twice is a list that disagrees.
func TestCapabilityNamesMatchTheDSL(t *testing.T) {
	declared := executorsDeclaredInDSL(t)
	if len(declared) == 0 {
		t.Fatal("found no integration.compose.* executors in dsl/compose/builtins.memql, so this test asserts nothing")
	}

	registered := map[string]bool{}
	for _, c := range (&Integration{}).Capabilities() {
		if registered[c.Name] {
			t.Errorf("capability %q is registered twice", c.Name)
		}
		registered[c.Name] = true
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("capability %q has no description; it is what the functions() introspection shows", c.Name)
		}
		if c.Handler == nil {
			t.Errorf("capability %q has no handler", c.Name)
		}
	}

	for name := range declared {
		if !registered[name] {
			t.Errorf("dsl/compose/builtins.memql declares @executor(\"integration.compose.%s\") and nothing registers it -- strict boot refuses a builtin whose executor does not resolve, on EVERY node type", name)
		}
	}
	for name := range registered {
		if !declared[name] {
			t.Errorf("integration.compose.%s is registered and no builtin declares it; nothing can call it", name)
		}
	}
}

// TestRegistrationNameIsTheLiteral. The module-taxonomy gate finds plug-in
// registrations by scanning source for the STRING LITERAL, because a computed
// name could not be classified at PR time -- so the literal and the constant
// must agree or the classification is about a name nothing registers.
func TestRegistrationNameIsTheLiteral(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(packageDir(t), "integration.go"))
	if err != nil {
		t.Fatalf("read integration.go: %v", err)
	}
	want := `memql.RegisterPlugin("` + integrationName + `"`
	if !strings.Contains(string(src), want) {
		t.Errorf("integration.go does not register the literal %q; the taxonomy gate scans for it and integrationName is %q", want, integrationName)
	}
}

// TestTheStampNeverEscapesItsCall.
//
// `auth.ContextWithInternalOrigin` opens EVERY @serverOnly construct in the
// tree for whatever context carries it. A marked context that is RETURNED is
// inherited by every later frame, which is the memql#2879 / memql#2989
// escalation: one call that needed the stamp silently grants it to
// everything that runs after.
//
// So the stamp appears exactly once in this package, INLINE as the argument
// to the one Execute that needs it, and this counts the site.
func TestTheStampNeverEscapesItsCall(t *testing.T) {
	sites := 0
	dir := packageDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// COMMENTS ARE STRIPPED FIRST, because store.go's header NAMES this
		// function in prose -- and a gate that counted mentions would fail
		// on the very comment explaining why there is one call. The same
		// trap the front-door path gate records about a path named in a
		// comment.
		n := strings.Count(withoutComments(string(raw)), "auth.ContextWithInternalOrigin(")
		if n > 0 && e.Name() != "store.go" {
			t.Errorf("%s stamps internal origin; the stamp lives in store.go alone, so a reader has one place to check", e.Name())
		}
		sites += n
	}
	if sites != 1 {
		t.Errorf("found %d internal-origin stamps in this package, want exactly 1 -- see store.go's header for why", sites)
	}
}

// TestComposableListsResolve holds every `@composable(list=...)` declared in
// THIS tree against the queries this tree declares.
//
// The concept pass runs before the query registry exists, so the loader can
// only check the value's FORM. This is the other half, at build time, for the
// corpus a build can see: a mark naming a query that does not exist reaches
// the person who picks that concept as an error rather than as rows.
//
// A PRODUCT BUNDLE MOUNTED AT MEMQL_DSL_PATH IS NOT COVERED HERE and cannot
// be -- no Go test in this repo walks that tree. It gets the use-time error
// instead, which is the same coverage split cmd/memqllint has.
func TestComposableListsResolve(t *testing.T) {
	marks := composableListsInDSL(t)
	if len(marks) == 0 {
		// NOT A FAILURE, and the distinction matters: no concept in the
		// engine tree is marked yet, because the concepts worth composing
		// from arrive in a product bundle. What would be a failure is a
		// mark whose query is missing, and this says plainly that it
		// checked and found nothing to check.
		t.Log("no concept in this tree declares @composable(list=...); nothing to resolve")
		return
	}
	queries := queryNamesInDSL(t)
	for concept, list := range marks {
		if !queries[list] {
			t.Errorf("%s declares @composable(list=%q) and no query by that name is declared in this tree -- the Sources column would offer the concept and fail on the click", concept, list)
		}
	}
}

// ---------------------------------------------------------------------------

// withoutComments strips `//` line comments and `/* */` blocks, so a gate
// counting CALLS is not tripped by the prose that explains them.
//
// It is deliberately naive about string literals containing "//": no file
// in this package has one, and a full lexer here would be more machinery
// than the assertion is worth. If that ever stops being true, this is the
// line to revisit rather than the count.
func withoutComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 4
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

var executorPattern = regexp.MustCompile(`@executor\("integration\.compose\.([A-Za-z0-9_]+)"\)`)

func executorsDeclaredInDSL(t *testing.T) map[string]bool {
	t.Helper()
	src := readRepoFile(t, filepath.Join("dsl", "compose", "builtins.memql"))
	out := map[string]bool{}
	for _, m := range executorPattern.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

var (
	composablePattern = regexp.MustCompile(`@composable\(([^)]*)\)`)
	listArgPattern    = regexp.MustCompile(`list\s*=\s*"([A-Za-z0-9_]+)"`)
	conceptPattern    = regexp.MustCompile(`(?m)^concept\s+([A-Za-z0-9_]+)\s*\{`)
	queryPattern      = regexp.MustCompile(`(?m)^query\s+[A-Za-z0-9_]+\s+([A-Za-z0-9_]+)\s*\{`)
)

// composableListsInDSL maps a concept file path plus concept name onto the
// `list` query it declares, across the whole dsl/ tree.
func composableListsInDSL(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join(repoRoot(t), "dsl")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(path) != "concepts.memql" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		src := string(raw)
		for _, m := range composablePattern.FindAllStringSubmatchIndex(src, -1) {
			args := src[m[2]:m[3]]
			listMatch := listArgPattern.FindStringSubmatch(args)
			if listMatch == nil {
				continue
			}
			// The concept the annotation belongs to is the next `concept`
			// declaration after it.
			rest := src[m[1]:]
			nameMatch := conceptPattern.FindStringSubmatch(rest)
			name := "?"
			if nameMatch != nil {
				name = nameMatch[1]
			}
			out[filepath.Base(filepath.Dir(path))+":"+name] = listMatch[1]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking dsl/: %v", err)
	}
	return out
}

func queryNamesInDSL(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	root := filepath.Join(repoRoot(t), "dsl")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(path) != "queries.memql" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, m := range queryPattern.FindAllStringSubmatch(string(raw), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking dsl/: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("found no queries in dsl/, so the resolution check above asserts nothing")
	}
	return out
}

// readRepoFile reads a path relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("could not read %s: %v", rel, err)
	}
	return string(raw)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir := packageDir(t)
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find the repository root walking up from %s", packageDir(t))
	return ""
}

func packageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the package directory")
	}
	return filepath.Dir(self)
}

var _ = sort.Strings
