package memql

// construct_origin_parity_test.go -- the ORIGIN vocabulary gate (memql#3934).
//
// The twin of cmd/memql-lsp's TestTrainingWireNamesMatchTheExtension, and it
// exists for the same reason that one does: every consumer of `origin` DEGRADES
// on a value it does not recognise, so a divergence is invisible on both sides
// and looks exactly like the feature working.
//
// The two degrades, both deliberate, both silent:
//
//   - sdk/ts's originFromWire collapses anything outside its union to "core".
//     The editor then renders that construct SEALED and read-only with reason
//     `coreSealed` -- on a file its author can edit freely. Not a crash, not a
//     missing row: a confident wrong answer.
//   - cmd/memql-lsp's trainingStateFor routes anything that is not `promoted`
//     to `seeded`, the one training state with no actions at all. A staged
//     construct would render with no Train and no Demote, which is
//     indistinguishable from a construct that needs a rollout.
//
// Neither degrade is wrong -- an older client meeting a newer cluster has to do
// something, and both choices are the least-wrong one available at that moment.
// What makes them dangerous is that they are the SAME behaviour a correct client
// shows for a construct that really is core or really is seeded. So the
// vocabulary is pinned rather than trusted.
//
// It reads the TypeScript as text rather than executing it, the same trade
// vscodeimportrule_test.go and the training gate make: a textual scan can be
// fooled by a comment, and being fooled costs a two-second edit, where NOT
// having the gate costs a surface that is quietly wrong.

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The TypeScript declarations that mirror this vocabulary, and the file each
// lives in. Both are closed unions over the same engine field.
const (
	sdkConstructsModulePath  = "../../sdk/ts/src/constructs/constructs.ts"
	vscodeReadonlyModulePath = "../../editors/vscode/src/constructs/readonly.ts"
)

// tsOriginUnion finds a `"a" | "b" | "c"` union on a line declaring an origin --
// `export type ConstructOrigin = ...` in the SDK, `origin: ...` in the
// extension's OriginatedConstruct -- and returns the raw union text.
var tsOriginUnion = regexp.MustCompile(`(?m)^\s*(?:export type ConstructOrigin =|origin:)\s*((?:"[a-z]+"\s*\|\s*)*"[a-z]+")\s*;`)

// tsQuoted pulls the individual values out of a union.
var tsQuoted = regexp.MustCompile(`"([a-z]+)"`)

// TestConstructOriginVocabularyIsExhaustive pins ConstructOriginVocabulary to
// what deriveConstructOrigin can actually return.
//
// The vocabulary is what every other check in this file compares against, so a
// value the derivation emits but the list omits would make the whole gate agree
// with itself about the wrong set.
func TestConstructOriginVocabularyIsExhaustive(t *testing.T) {
	known := map[string]bool{}
	for _, o := range ConstructOriginVocabulary {
		if known[o] {
			t.Errorf("ConstructOriginVocabulary lists %q twice", o)
		}
		known[o] = true
	}

	// Every input shape deriveConstructOrigin distinguishes, and the value it
	// answers with. A new branch that returns a new constant fails here unless
	// the constant is in the vocabulary.
	packDomains := map[string]string{"acme": "pack:acme"}
	for _, tc := range []struct {
		name       string
		originPath string
		promoted   bool
		staged     bool
		domains    map[string]string
	}{
		{name: "registered from Go", originPath: ""},
		{name: "embedded tree", originPath: "cognition/queries.memql"},
		{name: "mounted product domain", originPath: "acme/queries.memql", domains: packDomains},
		{name: "durably promoted", promoted: true},
		{name: "staged", staged: true},
		{name: "staged wins over promoted", promoted: true, staged: true},
	} {
		got := deriveConstructOrigin(tc.originPath, tc.promoted, tc.staged, tc.domains)
		if !known[got] {
			t.Errorf("%s: deriveConstructOrigin returned %q, which ConstructOriginVocabulary does not list -- "+
				"every client degrades an unlisted origin silently", tc.name, got)
		}
	}

	if got := deriveConstructOrigin("", true, true, nil); got != ConstructOriginStaged {
		t.Errorf("staged+promoted = %q; want %q -- a construct in both places resolves through the SHARED "+
			"registry, so reporting it as staged would be the lie", got, ConstructOriginStaged)
	}
}

// TestConstructOriginVocabularyMatchesTheClients pins the engine's origin set to
// the closed unions the TypeScript clients declare over the same field.
func TestConstructOriginVocabularyMatchesTheClients(t *testing.T) {
	for _, path := range []string{sdkConstructsModulePath, vscodeReadonlyModulePath} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v -- this gate cannot be skipped by deleting the file it reads", path, err)
		}
		matches := tsOriginUnion.FindAllStringSubmatch(string(source), -1)
		if len(matches) == 0 {
			t.Fatalf("%s declares no origin union this gate can find; if the declaration moved, move the gate with it", path)
		}
		for _, m := range matches {
			declared := map[string]bool{}
			for _, q := range tsQuoted.FindAllStringSubmatch(m[1], -1) {
				declared[q[1]] = true
			}
			for _, o := range ConstructOriginVocabulary {
				if !declared[o] {
					t.Errorf("%s declares the origin union %s, missing %q -- the engine emits it, and this "+
						"client would collapse it to \"core\" and render an editable construct as sealed",
						path, m[1], o)
				}
			}
			for value := range declared {
				if !slices.Contains(ConstructOriginVocabulary, value) {
					t.Errorf("%s declares origin %q, which the engine never emits -- dead rendering, and the "+
						"same drift in the other direction", path, value)
				}
			}
		}
	}
}

// TestOriginFromWireAcceptsEveryOrigin pins the SDK's decoder, not just its
// type. The union above is erased at runtime; originFromWire is the code that
// actually decides, and a value missing from ITS list degrades to "core"
// however carefully the type was written.
func TestOriginFromWireAcceptsEveryOrigin(t *testing.T) {
	source, err := os.ReadFile(sdkConstructsModulePath)
	if err != nil {
		t.Fatalf("read %s: %v", sdkConstructsModulePath, err)
	}
	body := functionBodyText(string(source), "function originFromWire")
	if body == "" {
		t.Fatalf("%s: originFromWire not found; if it was renamed, rename it here too", sdkConstructsModulePath)
	}
	for _, o := range ConstructOriginVocabulary {
		if o == ConstructOriginCore {
			// The fallback. It is what the function returns for everything it
			// does not name, so requiring it to be named would fail on a
			// correct implementation.
			continue
		}
		if !strings.Contains(body, `"`+o+`"`) {
			t.Errorf("originFromWire does not compare against %q, so it collapses that origin to %q. Body: %s",
				o, ConstructOriginCore, strings.TrimSpace(body))
		}
	}
}

// functionBodyText returns the text between the first `{` after decl and its
// matching `}`. Brace-counting rather than a regex because a body can nest.
func functionBodyText(source, decl string) string {
	start := strings.Index(source, decl)
	if start < 0 {
		return ""
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		return ""
	}
	open += start
	depth := 0
	for i := open; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[open+1 : i]
			}
		}
	}
	return ""
}
