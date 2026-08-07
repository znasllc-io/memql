package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// structFormSample is one minimal authored construct plus a marker that
// only appears in that construct's BODY (never in its hoisted
// `args { ... }` block), so the test below can tell "extractFunctionBody
// found the function body" from "it settled on the wrong brace".
type structFormSample struct {
	source     string
	bodyMarker string
}

// structFormSamples is keyed by author-facing struct-form keyword so
// the test can prove it covers every entry of
// parser.StructFormKeywords -- the rewriter's own list -- rather than a
// hardcoded set that can drift from it. That drift is what left a
// retired `mutation ` keyword sitting in precededByBodyOpener until
// memql#3194.
var structFormSamples = map[string]structFormSample{
	"query": {
		source: `query space sampleQuery {
  args {
    id string @required
  }
  filter row.id==args.id
  shape spaceCard
}
`,
		bodyMarker: "spaceCard",
	},
	"mutate": {
		source: `mutate space sampleMutation {
  args {
    id string @required
  }
  insert {
    id: args.id
  }
}
`,
		bodyMarker: "insert(",
	},
	"logic": {
		source: `logic sampleLogic {
  args {
    event object @required
  }
  body {
    return sampleBuiltin({ id: args.event.payload.id })
  }
}
`,
		bodyMarker: "sampleBuiltin(",
	},
	"automation": {
		source: `@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation sampleAutomation {
  args {
    id any
  }

  step decide {
    logic sampleLogic ( event )
  }
}
`,
		bodyMarker: "sampleLogic(",
	},
}

// TestBodyOpenerOnlySeesPostRewriteHeaders pins the invariant
// precededByBodyOpener rests on: NormaliseAll rewrites every
// author-facing struct-form keyword into a `func (Receiver) ...`
// header, and the function-loader snapshot the body extractor runs
// against (rawSourceForUsage) is taken AFTER that rewrite. So the
// extractor only ever needs to recognise the `func ` header, and a
// struct-form keyword arm alongside it would be dead by construction
// (memql#3194).
//
// If the rewriter ever stops emitting a `func ` header for one of
// these keywords -- or the snapshot moves above NormaliseAll -- this
// test fails loudly, instead of extractFunctionBody quietly returning
// "" and every validator fed that snapshot (declared usage, logic
// event binding, actor binding, logic event fields) failing open.
func TestBodyOpenerOnlySeesPostRewriteHeaders(t *testing.T) {
	if len(languageParser.StructFormKeywords) == 0 {
		t.Fatal("parser.StructFormKeywords is empty -- the rewriter chain lost its construct set")
	}

	for _, kw := range languageParser.StructFormKeywords {
		sample, ok := structFormSamples[kw]
		if !ok {
			t.Fatalf("no sample authored source for struct-form keyword %q -- parser.StructFormKeywords grew a construct this test does not cover; add a sample so the post-rewrite header invariant stays proven", kw)
		}

		t.Run(kw, func(t *testing.T) {
			rewritten, err := languageParser.NormaliseAll(sample.source)
			if err != nil {
				t.Fatalf("NormaliseAll(%s sample): %v", kw, err)
			}

			if !containsHeaderLine(rewritten, "func ") {
				t.Errorf("rewritten %s source carries no `func (Receiver) ...` header -- precededByBodyOpener recognises nothing else\nrewritten:\n%s", kw, rewritten)
			}
			if containsHeaderLine(rewritten, kw+" ") {
				t.Errorf("rewritten %s source still opens a construct with the author-facing keyword -- the struct-form rewrite did not run\nrewritten:\n%s", kw, rewritten)
			}

			body := extractFunctionBody(rewritten)
			if strings.TrimSpace(body) == "" {
				t.Fatalf("extractFunctionBody returned an empty body for the rewritten %s sample -- every validator fed this snapshot now fails open\nrewritten:\n%s", kw, rewritten)
			}
			if !strings.Contains(body, sample.bodyMarker) {
				t.Errorf("extractFunctionBody(%s sample) did not return the function body (marker %q missing) -- it matched some other brace\nbody:\n%s", kw, sample.bodyMarker, body)
			}
		})
	}
}

// TestBodyOpenerRejectsAuthoredStructForm records the flip side: an
// authored (un-rewritten) struct-form header is NOT a body opener.
// That is intentional -- such a header cannot appear in the snapshot
// (see precededByBodyOpener) -- and it is why matching one there would
// be dead code rather than a safety net.
func TestBodyOpenerRejectsAuthoredStructForm(t *testing.T) {
	for _, kw := range languageParser.StructFormKeywords {
		header := kw + " sampleName "
		if precededByBodyOpener(header) {
			t.Errorf("precededByBodyOpener accepted the authored struct-form header %q -- it should only recognise the post-rewrite `func ` form", header)
		}
	}
	if !precededByBodyOpener("func (Query) sampleName(_ any) ") {
		t.Error("precededByBodyOpener rejected the post-rewrite `func (Query) ...` header it exists to recognise")
	}
}

// containsHeaderLine reports whether any line of source begins with
// prefix -- the same "keyword opens the line" shape
// precededByBodyOpener checks.
func containsHeaderLine(source, prefix string) bool {
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}
