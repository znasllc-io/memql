package library

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// This file makes the stub engine's DSL fidelity (artifactFullShapeFields,
// artifactEnumValues -- both declared, unpopulated, in library_test.go) a
// DERIVATION from the real DSL source on disk, evaluated once in TestMain
// before any test runs, instead of two hand-maintained Go literals
// (memql#4298).
//
// The drift this closes already happened once: on main,
// artifactEnumValues["source"] hand-listed six values and silently dropped
// "user_created" -- the seventh, and the exact value the portal's create
// path wrote (retired in epic memql#4984; the value is still in the DSL
// enum, which is the point). Nothing went
// red, because no test happened to push that value through the stub. A
// derivation cannot go stale this way: the source of truth IS the DSL file,
// read fresh on every test run.
//
// Why a hand-rolled text parse instead of the engine's real lexer/parser
// (component/language): that package is its OWN Go module
// (github.com/znasllc-io/memql/component/language, per its go.mod) while
// integrations/library lives in the root module -- pulling it in would add
// a cross-module dependency edge to a test-only file just to read two flat
// field lists out of two small, stably-shaped files. shapes.memql's
// artifactFull body is a bare list of field paths with no nesting at all,
// and createArtifact's args block is a flat `name enum("a", "b", ...)`
// list -- both are well inside what a couple of targeted regexes can parse
// correctly and LOUDLY refuse to parse when the shape of the source
// surprises them. That is the trade this file makes deliberately: simple
// and auditable over reusing the engine's own parser across a module
// boundary for a two-file, test-only need.
//
// Fail-loud contract: every parse step below returns an error naming
// exactly what it could not find (block missing, block empty, a line that
// does not look like a field, an enum with zero values) -- never a silent
// empty/partial result. TestMain treats any such error as fatal and exits
// before a single test runs, so a DSL edit that breaks this parser's
// assumptions cannot be mistaken for "the stub says the DSL is fine."

const (
	// Verified against `go test ./integrations/library/...` and against
	// `make test`'s package-path selector: a Go test binary's working
	// directory is always its own package's source directory
	// (integrations/library/), never the repo root or the invoker's cwd,
	// regardless of which pattern selected the package. Two levels up
	// reaches the repo root from there.
	dslShapesPath    = "../../dsl/library/shapes.memql"
	dslMutationsPath = "../../dsl/library/mutations.memql"
)

var (
	// artifactFullShapeBlockRe captures the body of `shape artifact
	// artifactFull { ... }`. Non-greedy up to the FIRST '}' is safe here
	// because the shape body is a flat field-path list with no nested
	// braces and no braces appear in the doc comments above it.
	artifactFullShapeBlockRe = regexp.MustCompile(`(?s)\bshape\s+artifact\s+artifactFull\s*\{(.*?)\}`)

	// shapeFieldLineRe matches one shape-body line: either a bare payload
	// field (`ownerUserId`) or a single-segment row intrinsic
	// (`row.id`). The `row.` prefix is a non-capturing group, so group 1
	// is always the bare key the stub should use either way --
	// projectShapeFields keys its output map by the same bare name a
	// wire-decoded row would carry.
	shapeFieldLineRe = regexp.MustCompile(`^(?:row\.)?([A-Za-z_][A-Za-z0-9_]*)$`)

	// createArtifactHeaderRe locates the mutation DECLARATION, not just any
	// mention of the name -- mutations.memql's doc comments say
	// "createArtifact" in prose ("...an automation folds it into ... via
	// createArtifact.") without the preceding "mutate artifact", so this
	// exact phrase is what keeps the match pinned to the one real
	// declaration.
	createArtifactHeaderRe = regexp.MustCompile(`\bmutate\s+artifact\s+createArtifact\b`)

	// argsHeaderRe locates the `args {` sub-block header inside a mutation
	// body. It deliberately requires a literal '{' right after "args" (up
	// to whitespace), which is what keeps it from matching the many
	// `args.<field>` references inside createArtifact's own `insert{}`
	// block -- those have a '.', never a '{', immediately after "args".
	argsHeaderRe = regexp.MustCompile(`\bargs\s*\{`)

	// enumFieldRe finds every `<fieldName> enum(<values>)` declaration
	// inside an args block body. Every enum arg in createArtifact is
	// declared on one line, so a whole-body match (rather than per-line)
	// is both simpler and sufficient.
	enumFieldRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s+enum\(([^)]*)\)`)
)

// findMatchingBrace returns the index of the '}' that closes the '{' at
// src[openIdx], skipping over the contents of double-quoted string
// literals so a brace inside a quoted value is never mistaken for
// structural nesting. Generic on purpose: both callers below (a mutation
// body, then its nested args block) reuse it.
func findMatchingBrace(src string, openIdx int) (int, error) {
	if openIdx < 0 || openIdx >= len(src) || src[openIdx] != '{' {
		return -1, fmt.Errorf("findMatchingBrace: index %d is not an opening '{'", openIdx)
	}
	depth := 0
	inString := false
	escaped := false
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return -1, fmt.Errorf("findMatchingBrace: no matching '}' found for '{' at index %d", openIdx)
}

// deriveArtifactFullShapeFields parses the field-path list out of the REAL
// `shape artifact artifactFull { ... }` block in dsl/library/shapes.memql.
// Returns an error -- never an empty or partial list -- when the block is
// missing, empty, or contains a line this simple parser does not
// recognise as a field path.
func deriveArtifactFullShapeFields(src []byte) ([]string, error) {
	m := artifactFullShapeBlockRe.FindSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("no `shape artifact artifactFull { ... }` block found in %s", dslShapesPath)
	}

	var fields []string
	for _, rawLine := range strings.Split(string(m[1]), "\n") {
		line := rawLine
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fm := shapeFieldLineRe.FindStringSubmatch(line)
		if fm == nil {
			return nil, fmt.Errorf(
				"artifactFull shape body in %s has a line that does not look like a field path: %q",
				dslShapesPath, line)
		}
		fields = append(fields, fm[1])
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("artifactFull shape body in %s parsed with zero fields", dslShapesPath)
	}
	return fields, nil
}

// deriveArtifactEnumValues parses every `<field> enum(<values>)` argument
// declared inside the REAL `mutate artifact createArtifact { args { ... } }`
// block in dsl/library/mutations.memql. Returns an error -- never an
// empty map, and never a field mapped to zero values -- when the mutation,
// its args block, or any individual enum's value list cannot be found.
func deriveArtifactEnumValues(src []byte) (map[string][]string, error) {
	text := string(src)

	headerLoc := createArtifactHeaderRe.FindStringIndex(text)
	if headerLoc == nil {
		return nil, fmt.Errorf("no `mutate artifact createArtifact` declaration found in %s", dslMutationsPath)
	}
	relOpen := strings.IndexByte(text[headerLoc[1]:], '{')
	if relOpen < 0 {
		return nil, fmt.Errorf("createArtifact declaration in %s has no opening '{'", dslMutationsPath)
	}
	bodyOpen := headerLoc[1] + relOpen
	bodyClose, err := findMatchingBrace(text, bodyOpen)
	if err != nil {
		return nil, fmt.Errorf("createArtifact body in %s: %w", dslMutationsPath, err)
	}
	mutationBody := text[bodyOpen+1 : bodyClose]

	argsLoc := argsHeaderRe.FindStringIndex(mutationBody)
	if argsLoc == nil {
		return nil, fmt.Errorf("no `args { ... }` block found inside createArtifact in %s", dslMutationsPath)
	}
	argsOpen := argsLoc[1] - 1 // argsLoc[1] points just past the matched '{'.
	argsClose, err := findMatchingBrace(mutationBody, argsOpen)
	if err != nil {
		return nil, fmt.Errorf("createArtifact args block in %s: %w", dslMutationsPath, err)
	}
	argsBody := mutationBody[argsOpen+1 : argsClose]

	enums := map[string][]string{}
	for _, m := range enumFieldRe.FindAllStringSubmatch(argsBody, -1) {
		field, valuesText := m[1], m[2]
		var values []string
		for _, vm := range quotedItemRe.FindAllStringSubmatch(valuesText, -1) {
			values = append(values, unescape(vm[1]))
		}
		if len(values) == 0 {
			return nil, fmt.Errorf(
				"enum arg %q in createArtifact (%s) parsed with zero values", field, dslMutationsPath)
		}
		enums[field] = values
	}
	if len(enums) == 0 {
		return nil, fmt.Errorf("createArtifact args block in %s contained no enum(...) fields", dslMutationsPath)
	}
	return enums, nil
}

// loadArtifactFullShapeFields / loadArtifactEnumValues read the DSL source
// from disk and derive from it. Kept separate from the pure derive*
// functions above so a scratch-copy or -overlay drift test can exercise
// deriveArtifactFullShapeFields / deriveArtifactEnumValues directly against
// edited bytes without touching the working tree.
func loadArtifactFullShapeFields() ([]string, error) {
	src, err := os.ReadFile(dslShapesPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dslShapesPath, err)
	}
	return deriveArtifactFullShapeFields(src)
}

func loadArtifactEnumValues() (map[string][]string, error) {
	src, err := os.ReadFile(dslMutationsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dslMutationsPath, err)
	}
	return deriveArtifactEnumValues(src)
}

// TestMain derives the stub's DSL fidelity ONCE, before any test in this
// package runs, and populates the package-level artifactFullShapeFields /
// artifactEnumValues vars declared in library_test.go. A derivation
// failure exits the whole test binary with a FATAL message naming exactly
// what could not be found -- no test runs, so there is no way for a broken
// derivation to be mistaken for "all tests passed." This is the
// requirement-1 fail-loud contract: an empty or hardcoded fallback is
// never reachable from here.
func TestMain(m *testing.M) {
	fields, err := loadArtifactFullShapeFields()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"FATAL: could not derive artifactFull shape fields from the DSL -- %v\n"+
				"(memql#4298: the stub's shaped-read fidelity must come from dsl/library/shapes.memql, "+
				"not a hand-maintained list; refusing to fall back to an empty or stale one)\n", err)
		os.Exit(1)
	}
	artifactFullShapeFields = fields

	enums, err := loadArtifactEnumValues()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"FATAL: could not derive createArtifact enum values from the DSL -- %v\n"+
				"(memql#4298: the stub's enum-validation fidelity must come from dsl/library/mutations.memql, "+
				"not a hand-maintained map; refusing to fall back to an empty or stale one)\n", err)
		os.Exit(1)
	}
	artifactEnumValues = enums

	os.Exit(m.Run())
}

// TestArtifactFullShapeFieldsDerivedFromDSL is the reachable positive for
// the shape derivation: "the block was found and zero fields fell out"
// and "the block was found and the RIGHT fields fell out" both compile and
// both run, so only an assertion on actual content tells them apart. Also
// pins the two fields the review's real bug hinged on
// (producedByWorkerId / producedByWorkerName -- see artifactFullShapeFields'
// doc comment in library_test.go).
func TestArtifactFullShapeFieldsDerivedFromDSL(t *testing.T) {
	if len(artifactFullShapeFields) == 0 {
		t.Fatal("artifactFullShapeFields is empty -- TestMain should have failed loudly before any test ran")
	}
	for _, want := range []string{"id", "createdAt", "labels", "producedByWorkerId", "producedByWorkerName"} {
		if !slices.Contains(artifactFullShapeFields, want) {
			t.Fatalf("artifactFullShapeFields = %v, missing %q -- derivation from %s is not tracking the real shape",
				artifactFullShapeFields, want, dslShapesPath)
		}
	}
}

// TestArtifactEnumValuesDerivedFromDSL is the direct memql#4298 regression:
// on main, the hand-maintained artifactEnumValues["source"] listed six
// values and silently dropped "user_created" (the value the portal's own
// create path wrote, before epic memql#4984 retired it) --
// nothing failed, because no existing test happened to push that value
// through the stub. A derived list cannot go stale this way; this proves
// the derivation actually recovers all eight declared values in their
// declared order, "user_created" included.
func TestArtifactEnumValuesDerivedFromDSL(t *testing.T) {
	if len(artifactEnumValues) == 0 {
		t.Fatal("artifactEnumValues is empty -- TestMain should have failed loudly before any test ran")
	}

	wantFields := []string{"lens", "kind", "source", "format", "scope", "validationStatus"}
	for _, field := range wantFields {
		if len(artifactEnumValues[field]) == 0 {
			t.Fatalf("artifactEnumValues[%q] is empty -- derivation from %s missed a declared enum arg",
				field, dslMutationsPath)
		}
	}

	source := artifactEnumValues["source"]
	if len(source) != 8 {
		t.Fatalf("artifactEnumValues[\"source\"] has %d values, want 8 (memql#4298: the hand-maintained "+
			"list silently dropped one -- \"user_created\" -- and the derivation must not repeat that): %v",
			len(source), source)
	}
	// "exported" joined in memql#4340/#4342: v1:library:file can hold it, and a
	// promotion passes the backing row's source straight through, so the index
	// enum has to contain it or that file never gets an index row. The
	// containment itself is gated by TestEveryBackingSourceValueIsPromotable in
	// component/memql; this list is the ORDER-SENSITIVE mirror, so it moves with
	// the declaration rather than deriving the same fact twice.
	wantSource := []string{
		"uploaded", "exported", "workbench_generated", "computer_use", "agent_generated",
		"derived", "user_created", "live",
	}
	if !slices.Equal(source, wantSource) {
		t.Fatalf("artifactEnumValues[\"source\"] = %v, want %v", source, wantSource)
	}
	if !slices.Contains(source, "user_created") {
		t.Fatal("artifactEnumValues[\"source\"] is missing \"user_created\" -- this is the exact memql#4298 drift")
	}
}
