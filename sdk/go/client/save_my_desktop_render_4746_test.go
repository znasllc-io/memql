package client

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The wire text a roaming desktop actually sends (epic memql#4746).
//
// A mutation reaches the engine as MemQL TEXT, rendered here, and this one
// carries text nobody vetted: a folder is named by its owner and a file is
// titled by whatever was uploaded, so quotes, backslashes, newlines, tabs and
// astral-plane characters are ORDINARY inputs on this path. The item ids are
// object KEYS, and `item-1` is not a bare identifier to the MemQL lexer -- an
// unquoted one lexes as `item`, `-`, `1` and the whole call fails to parse in
// a caller that did nothing wrong (the memql#3009 shape, met once already on
// worker labels).
//
// It lives HERE, beside the renderer, rather than in an engine test, and that
// is a module-direction constraint rather than a preference: `sdk/go/client`
// belongs to the root module, so a leaf module like `component/memql` cannot
// import it without dragging the published root module into all eighteen
// modules above it. The lexer and parser are in `component/language/parser`,
// which both sides already depend on -- so the whole question this test asks,
// "does what a client sends parse", is answerable right here with no engine
// and no database.

func hostileDesktopDocument() map[string]any {
	return map[string]any{
		"version": 1,
		"desks": []any{
			map[string]any{"id": "desk-1", "createdBy": "user"},
			map[string]any{"id": "desk-2", "createdBy": "auto"},
		},
		"activeDeskId": "desk-1",
		"surfaces": map[string]any{
			"desk-1": map[string]any{
				"items": map[string]any{
					"item-1": map[string]any{
						"kind": "folder", "id": "item-1",
						"name":     "Q3 \"final\"\\draft\n\ttaxes \u00e9\u00fc \U0001F5C2",
						"children": []any{},
					},
					"item-2": map[string]any{
						"kind": "file", "id": "item-2",
						"artifactId": "artifact-abc", "title": "notes: a/b \\ c \"quoted\"",
						"fileKind": "document", "source": "uploaded",
					},
				},
				"positions": map[string]any{
					"item-1": map[string]any{"col": 0, "row": 0},
					"item-2": map[string]any{"col": 3, "row": 1},
				},
			},
		},
		"dock":      map[string]any{"pinned": []any{"settings", "fleet"}},
		"themePack": "graphite",
	}
}

func TestSaveMyDesktopBuild_RendersAParseableCall(t *testing.T) {
	doc := hostileDesktopDocument()
	call := SaveMyDesktopBuild(SaveMyDesktopArgs{Revision: 7, Document: doc})

	// The negative control. Every assertion below would pass vacuously on a
	// fixture whose text had been flattened to something benign, so state what
	// the rendered call must actually contain -- and state it as the ESCAPED
	// form, which is the thing under test.
	for _, want := range []string{`\"final\"`, `\\draft`, `\n`, `\t`, `"item-1"`, `"desk-1"`} {
		if !strings.Contains(call, want) {
			t.Fatalf("the rendered call is missing %s, so the rest of this test proves nothing.\ncall: %s", want, call)
		}
	}

	tokens, err := langparser.NewLexer(call).Tokenize()
	if err != nil {
		t.Fatalf("the SDK rendered a call the MemQL lexer rejects: %v\ncall: %s", err, call)
	}
	if _, err := langparser.NewParser(tokens).Parse(); err != nil {
		t.Fatalf("the SDK rendered a call the MemQL parser rejects: %v\ncall: %s", err, call)
	}
}

// The escapes must SURVIVE, not merely be accepted. A lexer that dropped a
// backslash or collapsed `\n` to the letter n would still parse the call, and
// the desktop would come back on the other machine with a folder named
// something the person never typed.
func TestSaveMyDesktopBuild_TextSurvivesTheLexer(t *testing.T) {
	doc := hostileDesktopDocument()
	call := SaveMyDesktopBuild(SaveMyDesktopArgs{Revision: 7, Document: doc})

	tokens, err := langparser.NewLexer(call).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	decoded := map[string]bool{}
	for _, tok := range tokens {
		if tok.Type == langparser.TokenString {
			decoded[tok.Literal] = true
		}
	}

	surfaces := doc["surfaces"].(map[string]any)
	items := surfaces["desk-1"].(map[string]any)["items"].(map[string]any)
	for _, want := range []string{
		items["item-1"].(map[string]any)["name"].(string),  // the awkward folder name
		items["item-2"].(map[string]any)["title"].(string), // the awkward file title
		"item-1", // an object KEY, quoted because it is not a bare identifier
		"desk-1",
	} {
		if !decoded[want] {
			t.Errorf("the lexer did not deliver %q back intact; it decoded: %v", want, keysOf(decoded))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
