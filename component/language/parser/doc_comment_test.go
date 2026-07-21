package parser

// doc_comment_test.go -- memql#2633: the lexer captures /// doc-comment
// blocks on a side channel and the parser attaches them to the immediately
// following declaration per rulings 1-2 of
// docs/internal/design/doc-comments-description-source.md. Capture-only:
// Description fields and every consumer stay untouched (sourcing flips in
// #2634).

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// parseNormalised runs the real load pipeline shape: struct-form rewrite,
// then ParseFile (which wires the doc-comment side channel).
func parseNormalised(t *testing.T, src string) *File {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	file, err := ParseFile(normalised)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return file
}

func docOf(t *testing.T, def Node) string {
	t.Helper()
	switch d := def.(type) {
	case *FunctionDef:
		return d.DocComment
	case *ast.AutomationDef:
		return d.DocComment
	case *ast.ConceptDecl:
		return d.DocComment
	case *ast.ShapeDecl:
		return d.DocComment
	case *ast.ToolDecl:
		return d.DocComment
	case *ast.PromptDecl:
		return d.DocComment
	case *ast.ProviderDecl:
		return d.DocComment
	case *ast.PolicyDecl:
		return d.DocComment
	case *ast.SpecDecl:
		return d.DocComment
	case *ast.CapabilityDecl:
		return d.DocComment
	case *ast.ActionDecl:
		return d.DocComment
	}
	t.Fatalf("no DocComment accessor for %T", def)
	return ""
}

func firstDeclDoc(t *testing.T, src string) string {
	t.Helper()
	file := parseNormalised(t, src)
	for _, def := range file.Definitions {
		switch def.(type) {
		case *UseDeclaration:
			continue
		default:
			return docOf(t, def)
		}
	}
	t.Fatal("no declaration parsed")
	return ""
}

// Every describable construct kind from the design's ruling-1 target set
// carries the /// block attached above its annotations. Fixtures are
// minimal forms of live corpus declarations.
func TestDocComment_AttachesToEveryDescribableKind(t *testing.T) {
	const doc = "/// Probe doc."
	cases := map[string]string{
		"concept": doc + `
concept probeThing {
  ownerUserId string!
}`,
		"shape": doc + `
@row
shape candidate probeCard {
  status
}`,
		"query": doc + `
@actor
query candidate probeQuery {
  args {
    planId string!
  }
  filter planId == args.planId && ownerUserId == actor.userId
}`,
		"mutation": doc + `
mutate candidate probeMutation {
  args {
    widgetId string @required
  }
  insert {
    id: canonicalId(args.widgetId, candidate)
  }
}`,
		"logic": doc + `
logic probeLogic {
  args {
    a string @required
  }
  body {
    return coalesce(args.a, "")
  }
}`,
		"automation": doc + `
@trigger(event="system.shutdown")
automation probeAuto {
  args {
    node any
  }

  step persist {
    mutation createSpawnEvent (
      nodeId: coalesce(node.id, "")
    )
  }
}`,
		"tool": doc + `
@handler(type="function", name="probeTool")
tool probeTool {
  role string! @description("Specialist roleSlug.")
}`,
		"prompt": doc + `
@templateFile("prompts/probe.tmpl")
prompt probePrompt {
}`,
		"provider": doc + `
@base
@type("Anthropic")
provider probeProvider {
  auth {
    apiKey env("MEMQL_AI_ANTHROPIC_API_KEY")
  }
}`,
		"policy": doc + `
@primary("streamClaudeSonnet")
policy probePolicy {
}`,
		"spec": doc + `
spec actorEnvelope probeSpec {
  return role == "admin"
}`,
		"trait": doc + `
trait probeTrait {
  return kind == "assistant"
}`,
		"capability": doc + `
@sideEffect("read")
capability fs.readFile {
  args {
    subject string @required
  }
}`,
		"action": doc + `
action probeAction {
  args {
    workdir string!
  }
  capability script(script: "deploy.cloneRepo", workdir: args.workdir)
}`,
	}
	for kind, src := range cases {
		t.Run(kind, func(t *testing.T) {
			if got := firstDeclDoc(t, src); got != "Probe doc." {
				t.Errorf("%s: DocComment = %q, want %q", kind, got, "Probe doc.")
			}
		})
	}
}

// Ruling-2 join: strip /// plus exactly one space, space-join consecutive
// lines, bare /// is a paragraph break, deliberate extra indentation
// survives.
func TestDocComment_JoinRules(t *testing.T) {
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"space-join":        {[]string{" First sentence.", " Second sentence."}, "First sentence. Second sentence."},
		"paragraph-break":   {[]string{" Para one.", "", " Para two."}, "Para one.\nPara two."},
		"strip-exactly-one": {[]string{" Top.", "   - sub bullet"}, "Top.   - sub bullet"},
		"no-leading-space":  {[]string{"tight"}, "tight"},
		"trailing-trimmed":  {[]string{" padded   "}, "padded"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := JoinDocComment(tc.lines); got != tc.want {
				t.Errorf("JoinDocComment(%q) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}
}

// A blank line between the /// block and the declaration breaks attachment;
// the detached block is ignored, never an error. A plain // comment between
// them is transparent; //// divider art is never a doc comment.
func TestDocComment_AttachmentBoundaries(t *testing.T) {
	const logicBody = `
logic probeLogic {
  args {
    a string @required
  }
  body {
    return coalesce(args.a, "")
  }
}`

	t.Run("blank-line-breaks", func(t *testing.T) {
		src := "/// Detached doc.\n" + logicBody
		if got := firstDeclDoc(t, src); got != "" {
			t.Errorf("blank line must break attachment, got %q", got)
		}
	})
	t.Run("plain-comment-transparent", func(t *testing.T) {
		src := "/// Attached doc.\n// ordinary note\nlogic probeLogic {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
		if got := firstDeclDoc(t, src); got != "Attached doc." {
			t.Errorf("plain comment must be transparent, got %q", got)
		}
	})
	t.Run("four-slashes-ignored", func(t *testing.T) {
		src := "//// divider ////\nlogic probeLogic {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
		if got := firstDeclDoc(t, src); got != "" {
			t.Errorf("//// must not be a doc comment, got %q", got)
		}
	})
	t.Run("multi-line-block", func(t *testing.T) {
		src := "/// Line one.\n/// Line two.\nlogic probeLogic {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
		if got := firstDeclDoc(t, src); got != "Line one. Line two." {
			t.Errorf("multi-line join through attachment, got %q", got)
		}
	})
}

// /// alongside @description captures BOTH channels without altering
// Description -- capture-only, precedence flips in #2634 (ruling 3).
func TestDocComment_AlongsideDescriptionCapturesBoth(t *testing.T) {
	src := `/// The doc-comment channel.
@description("The annotation channel.")
logic probeLogic {
  args {
    a string @required
  }
  body {
    return coalesce(args.a, "")
  }
}`
	file := parseNormalised(t, src)
	var fn *FunctionDef
	for _, def := range file.Definitions {
		if f, ok := def.(*FunctionDef); ok {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("logic not parsed")
	}
	if fn.DocComment != "The doc-comment channel." {
		t.Errorf("DocComment = %q", fn.DocComment)
	}
	if fn.Description != "The annotation channel." {
		t.Errorf("Description must be untouched (capture-only), got %q", fn.Description)
	}
}

// /// above an args{} field lands in the args-field slot -- the only channel
// for arg descriptions (the args-field @description spelling stays retired).
func TestDocComment_ArgsFieldSlot(t *testing.T) {
	src := `logic probeLogic {
  args {
    /// The plan to inspect.
    planId string!
    limit number
  }
  body {
    return coalesce(args.planId, "")
  }
}`
	file := parseNormalised(t, src)
	var fn *FunctionDef
	for _, def := range file.Definitions {
		if f, ok := def.(*FunctionDef); ok {
			fn = f
		}
	}
	if fn == nil || fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) < 2 {
		t.Fatalf("args schema not parsed: %+v", fn)
	}
	if got := fn.ArgsSchema.Fields[0].DocComment; got != "The plan to inspect." {
		t.Errorf("args-field DocComment = %q", got)
	}
	if got := fn.ArgsSchema.Fields[1].DocComment; got != "" {
		t.Errorf("undocumented field must stay empty, got %q", got)
	}
}

// The struct-form rewriter splices original source, so /// blocks survive
// into its emitted output, and attachment works through the rewritten shape
// (/// -> annotations -> hoisted args{} -> func line).
func TestDocComment_SurvivesStructFormRewrite(t *testing.T) {
	src := `/// Rewritten and still documented.
@description("annotation")
query candidate probeQuery {
  args {
    planId string!
  }
  filter planId == args.planId
}`
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	if !strings.Contains(normalised, "/// Rewritten and still documented.") {
		t.Fatalf("rewriter dropped the /// block:\n%s", normalised)
	}
	if got := firstDeclDoc(t, src); got != "Rewritten and still documented." {
		t.Errorf("attachment through the rewritten shape = %q", got)
	}
}

// BlankComments must keep blanking /// blocks in the detection view the
// text-based header detectors consume (memql#1074) -- the parser retains
// them via the side channel, the detectors must not see them.
func TestDocComment_BlankCommentsStillBlanks(t *testing.T) {
	src := "/// doc line\nquery x {}"
	blanked := BlankComments(src)
	if strings.Contains(blanked, "doc line") {
		t.Errorf("BlankComments must blank /// content, got %q", blanked)
	}
	if len(blanked) != len(src) {
		t.Errorf("BlankComments must preserve offsets: len %d != %d", len(blanked), len(src))
	}
}

// Trailing comments after code are neither doc comments nor transparent
// lines (memql#2633 review majors): a trailing /// must not attach to the
// NEXT node, and a trailing // must not make its code line transparent.
func TestDocComment_TrailingCommentsAreOpaque(t *testing.T) {
	t.Run("trailing-triple-slash-not-captured", func(t *testing.T) {
		src := "concept alpha {\n  x string\n} /// about alpha, trailing\nconcept beta {\n  y string\n}"
		file := parseNormalised(t, src)
		for _, def := range file.Definitions {
			if c, ok := def.(*ast.ConceptDecl); ok {
				if c.DocComment != "" {
					t.Errorf("concept %s: trailing /// must not attach, got %q", c.Name, c.DocComment)
				}
			}
		}
	})
	t.Run("trailing-slash-in-args-not-stolen-by-next-field", func(t *testing.T) {
		src := "logic probeLogic {\n  args {\n    planId string! /// note about planId\n    limit number\n  }\n  body {\n    return coalesce(args.planId, \"\")\n  }\n}"
		file := parseNormalised(t, src)
		for _, def := range file.Definitions {
			if fn, ok := def.(*FunctionDef); ok && fn.ArgsSchema != nil {
				for _, f := range fn.ArgsSchema.Fields {
					if f.DocComment != "" {
						t.Errorf("field %s: trailing /// must not attach, got %q", f.Name, f.DocComment)
					}
				}
			}
		}
	})
	t.Run("trailing-comment-does-not-make-code-line-transparent", func(t *testing.T) {
		src := "/// Doc.\nuse cognition.concepts.{ space } // trailing note\nlogic probeLogic {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
		if got := firstDeclDoc(t, src); got != "" {
			t.Errorf("a /// block must not tunnel through a code line with a trailing comment, got %q", got)
		}
	})
	t.Run("trailing-block-comment-same", func(t *testing.T) {
		src := "/// Doc.\nuse cognition.concepts.{ space } /* trailing */\nlogic probeLogic {\n  args {\n    a string @required\n  }\n  body {\n    return coalesce(args.a, \"\")\n  }\n}"
		if got := firstDeclDoc(t, src); got != "" {
			t.Errorf("a /// block must not tunnel through a trailing block comment, got %q", got)
		}
	})
}

// The four single-decl entry points wire the side channel too.
func TestDocComment_SingleDeclEntries(t *testing.T) {
	if decl, err := ParseToolDecl("/// Tool doc.\n@handler(type=\"function\", name=\"probeTool\")\ntool probeTool {\n  role string!\n}"); err != nil || decl.DocComment != "Tool doc." {
		t.Errorf("ParseToolDecl: doc=%q err=%v", declDoc(decl), err)
	}
	if decl, err := ParseProviderDecl("/// Provider doc.\n@base\n@type(\"Anthropic\")\nprovider probeProvider {\n  auth {\n    apiKey env(\"X\")\n  }\n}"); err != nil || decl.DocComment != "Provider doc." {
		t.Errorf("ParseProviderDecl: doc=%q err=%v", declDoc(decl), err)
	}
	if decl, err := ParsePolicyDecl("/// Policy doc.\n@primary(\"p\")\npolicy probePolicy {\n}"); err != nil || decl.DocComment != "Policy doc." {
		t.Errorf("ParsePolicyDecl: doc=%q err=%v", declDoc(decl), err)
	}
	if decl, err := ParseSpecDecl("/// Spec doc.\nspec actorEnvelope probeSpec {\n  return role == \"admin\"\n}"); err != nil || decl.DocComment != "Spec doc." {
		t.Errorf("ParseSpecDecl: doc=%q err=%v", declDoc(decl), err)
	}
}

func declDoc(d any) string {
	switch v := d.(type) {
	case *ast.ToolDecl:
		if v != nil {
			return v.DocComment
		}
	case *ast.ProviderDecl:
		if v != nil {
			return v.DocComment
		}
	case *ast.PolicyDecl:
		if v != nil {
			return v.DocComment
		}
	case *ast.SpecDecl:
		if v != nil {
			return v.DocComment
		}
	}
	return "<nil>"
}

// Consume-once semantics: with two blank-separated blocks above one decl,
// only the adjacent block attaches and the earlier one stays detached; a
// consumed block cannot re-attach to a later declaration.
func TestDocComment_ConsumeOnceAndAdjacency(t *testing.T) {
	src := "/// Far block.\n\n/// Near block.\nconcept alpha {\n  x string\n}\nconcept beta {\n  y string\n}"
	file := parseNormalised(t, src)
	var docs []string
	for _, def := range file.Definitions {
		if c, ok := def.(*ast.ConceptDecl); ok {
			docs = append(docs, c.DocComment)
		}
	}
	if len(docs) != 2 || docs[0] != "Near block." || docs[1] != "" {
		t.Errorf("docs = %q, want [Near block., empty]", docs)
	}
}

// The FunctionDef arm mirrors the doc onto its AutomationDef body so the
// automations subsystem reads it without reaching back to the FunctionDef.
func TestDocComment_AutomationBodyMirror(t *testing.T) {
	src := `/// Automation doc.
@trigger(event="system.shutdown")
automation probeAuto {
  args {
    node any
  }

  step persist {
    mutation createSpawnEvent (
      nodeId: coalesce(node.id, "")
    )
  }
}`
	file := parseNormalised(t, src)
	for _, def := range file.Definitions {
		if fn, ok := def.(*FunctionDef); ok {
			auto, ok := fn.Body.(*ast.AutomationDef)
			if !ok || auto == nil {
				t.Fatal("automation body missing")
			}
			if auto.DocComment != "Automation doc." {
				t.Errorf("AutomationDef mirror = %q", auto.DocComment)
			}
			return
		}
	}
	t.Fatal("automation not parsed")
}

// A hoisted args block must not steal the declaration's own doc block for
// its first field: the decl doc attaches to the decl, the field slot stays
// empty unless the field carries its own /// inside the block.
func TestDocComment_HoistedArgsDoesNotStealDeclDoc(t *testing.T) {
	src := `/// Decl doc.
query candidate probeQuery {
  args {
    planId string!
  }
  filter planId == args.planId
}`
	file := parseNormalised(t, src)
	for _, def := range file.Definitions {
		if fn, ok := def.(*FunctionDef); ok {
			if fn.DocComment != "Decl doc." {
				t.Errorf("decl doc = %q", fn.DocComment)
			}
			if fn.ArgsSchema != nil && len(fn.ArgsSchema.Fields) > 0 && fn.ArgsSchema.Fields[0].DocComment != "" {
				t.Errorf("first field stole the decl doc: %q", fn.ArgsSchema.Fields[0].DocComment)
			}
			return
		}
	}
	t.Fatal("query not parsed")
}
