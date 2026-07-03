package parser

import (
	"strings"
	"testing"
)

// negative_grammar_test.go -- S7 of the fail-loud syntax epic (memql#2383 /
// #2351). This is the PARSER/EXPRESSION half of the systematic negative-syntax
// conformance suite; the load/lint half lives in
// component/memql/negative_load_test.go.
//
// The owner's ask (2026-07-03): "I don't think we have tested putting erroneous
// syntax... enough testing for edge cases and scenarios where certain syntax
// should not work or should return an error." The 2026-07-03 audit found every
// hole in this class EMPIRICALLY -- garbage bodies loading silently, unknown
// kind prefixes dropping calls, trailing tokens accepted -- because NO test
// asked "does this malformed input error?". This suite asks that question,
// per construct kind, so a regression that re-opens a silent-acceptance hole
// fails here.
//
// It PINS the behavior established by S1 (#2356, load-side fail-loud) and S3
// (#2358, parser fail-loud: expect-EOF, unknown-invocation-kind rejection,
// keyword did-you-mean). It deliberately overlaps a few pre-existing rejection
// tests (parser_hardening_test.go, kind_prefixed_invocation_test.go,
// reject_unknown_annotations_test.go, body_rule_test.go) with DIFFERENT inputs
// so the matrix is complete per kind; cross-references are noted inline.
//
// Every ACTIVE case asserts the malformed input FAILS (err != nil) and, for the
// top cases, that the message is actionable (names the construct / failure /
// migration hint). Behaviors that are silently accepted TODAY and are NOT fixed
// by S1/S3 are pinned as t.Skip cases with a HOLE marker + a pointer for the
// coordinator to triage (they are NOT fixed in this story per the S7 charter).
//
// This package (parser) cannot import component/language/compiler
// (import cycle), so the rewriter-family kinds (query / mutate / logic /
// automation) are exercised via NormaliseAll (which lives here) rather than
// ParseFileSource.

// Rewriter-family kinds (query / mutate / logic / automation) reach the parser
// only after NormaliseAll expands their struct form. Structural errors (missing
// concept, a forbidden body{} block) surface in NormaliseAll; annotation / token
// errors (a malformed @trigger) surface in the subsequent parse. The full
// rewrite-then-parse path is provided by rewriteAndParse in body_rule_test.go
// (same package) -- reused here rather than re-imported through
// component/language/compiler (which would form an import cycle).

// assertParseErr fails unless err is non-nil; when wantSubstr are given each
// must appear in the message. It is the shared assertion for the tables below.
func assertParseErr(t *testing.T, label string, err error, wantSubstr ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected a parse error, got nil (silently accepted)", label)
		return
	}
	msg := err.Error()
	for _, want := range wantSubstr {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: error message missing %q\n  full: %s", label, want, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. Malformed bodies -- one garbage-body fixture per struct-form decl kind
//    that has an exported Parse<Kind>Decl entry point. Each must FAIL to parse
//    rather than register a half-broken construct.
// ---------------------------------------------------------------------------

func TestNegative_MalformedDeclBody(t *testing.T) {
	cases := []struct {
		kind  string
		src   string
		parse func(string) error
	}{
		{"shape", "@row\nshape s {\n  123 456 789\n}\n",
			func(s string) error { _, e := ParseShapeDecl(s); return e }},
		{"builtin", "@executor(\"integration.x.y\")\nbuiltin b {\n  arg string @@@ !!! broken\n}\n",
			func(s string) error { _, e := ParseBuiltinDecl(s); return e }},
		{"prompt", "@templateFile(\"x.tmpl\")\nprompt p {\n  field @@@ !!! broken\n}\n",
			func(s string) error { _, e := ParsePromptDecl(s); return e }},
		{"tool", "@handler(type=\"function\", name=\"fn\")\ntool t {\n  arg string @@@ !!! broken\n}\n",
			func(s string) error { _, e := ParseToolDecl(s); return e }},
		{"provider", "@extends(\"openai\")\n@model(\"m\")\nprovider pr {\n  params {\n    contextWindow @@@ broken\n  }\n}\n",
			func(s string) error { _, e := ParseProviderDecl(s); return e }},
		{"policy", "@primary(\"x\")\npolicy p {\n", // unterminated brace
			func(s string) error { _, e := ParsePolicyDecl(s); return e }},
		{"spec", "@enabled\nspec activeRowTrait s {\n  return status ==== \"x\" &&&& true\n}\n",
			func(s string) error { _, e := ParseSpecDecl(s); return e }},
		{"trait", "@enabled\ntrait t {\n  return active ==== true\n}\n",
			func(s string) error { _, e := ParseSpecDecl(s); return e }},
		{"seed", "seed agent sd {\n  name: @@@ broken !!!\n}\n",
			func(s string) error { _, e := ParseSeedDecl(s); return e }},
		{"action", "action doThing {\n  garbage !!! not-a-capability-call\n}\n",
			func(s string) error { _, e := ParseActionDecl(s); return e }},
		{"capability", "capability cap {\n  garbage !!! broken\n}\n",
			func(s string) error { _, e := ParseCapabilityDecl(s); return e }},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			assertParseErr(t, tc.kind+" garbage body", tc.parse(tc.src))
		})
	}
}

// ---------------------------------------------------------------------------
// 2. Structural violations.
// ---------------------------------------------------------------------------

// 2a. Body rule (ADR Decision 5): `body { }` is MANDATORY on logic, FORBIDDEN
// on every other construct. Cross-ref: body_rule_test.go covers the direct
// decl-parser sites; here we cover the rewriter-family sites (query / mutate /
// automation) via NormaliseAll plus the logic-missing-body half.
func TestNegative_BodyRule(t *testing.T) {
	t.Run("logic-missing-body", func(t *testing.T) {
		_, err := NormaliseAll("logic l {\n  args { event object @required }\n  return 1\n}\n")
		assertParseErr(t, "logic without body{}", err,
			"logic", "must wrap its procedural code in a `body { }` block")
	})
	t.Run("query-with-body", func(t *testing.T) {
		_, err := NormaliseAll("use cognition.concepts.{ space }\nquery space q {\n  filter active == true\n  body { return 1 }\n}\n")
		assertParseErr(t, "query with body{}", err,
			"must not declare a `body { }` block", "reserved for `logic`")
	})
	t.Run("mutation-with-body", func(t *testing.T) {
		_, err := NormaliseAll("use cognition.concepts.{ space }\nmutate space m {\n  args { x string @required }\n  body { return 1 }\n}\n")
		assertParseErr(t, "mutation with body{}", err,
			"must not declare a `body { }` block")
	})
	t.Run("spec-with-body", func(t *testing.T) {
		// Direct decl-parser site (spec): a body{} block is forbidden.
		_, err := ParseSpecDecl("@enabled\nspec activeRowTrait s {\n  body { return active == true }\n}\n")
		assertParseErr(t, "spec with body{}", err,
			"must not declare a `body { }` block")
	})
}

// 2b. Signature arity: the two-identifier `<kind> <Concept> <name>` signature
// must carry exactly the right number of identifiers.
func TestNegative_SignatureArity(t *testing.T) {
	t.Run("query-missing-concept", func(t *testing.T) {
		_, err := NormaliseAll("query q {\n  filter active == true\n  shape s\n}\n")
		assertParseErr(t, "query missing concept binding", err, "missing concept binding")
	})
	t.Run("mutation-missing-concept", func(t *testing.T) {
		_, err := NormaliseAll("mutate m {\n  args { x string @required }\n  insert { name: args.x }\n}\n")
		assertParseErr(t, "mutation missing concept binding", err, "missing concept binding")
	})
	t.Run("shape-too-many-idents", func(t *testing.T) {
		_, err := ParseShapeDecl("@row\nshape space extra s {\n  row.id\n}\n")
		assertParseErr(t, "shape with three signature identifiers", err)
	})
}

// 2c. Unknown annotations -- the kinds that DO reject (fail loud). tool /
// provider are also covered by reject_unknown_annotations_test.go; seed is the
// NEW positive assertion here. The kinds that still SILENTLY accept an unknown
// annotation are pinned as HOLE skips below (TestHOLE_UnknownAnnotationSilent).
func TestNegative_UnknownAnnotation_Rejected(t *testing.T) {
	cases := []struct {
		kind, src, want string
		parse           func(string) error
	}{
		{"tool", "@bogusAnno\n@handler(type=\"function\", name=\"fn\")\ntool t {\n  a string\n}\n", "unknown annotation @bogusAnno",
			func(s string) error { _, e := ParseToolDecl(s); return e }},
		{"provider", "@bogusAnno\n@extends(\"openai\")\nprovider p {\n  params { contextWindow 1 }\n}\n", "unknown annotation @bogusAnno",
			func(s string) error { _, e := ParseProviderDecl(s); return e }},
		{"seed", "@bogusAnno\nseed agent s {\n  name: \"x\"\n}\n", "unknown seed annotation @bogusAnno",
			func(s string) error { _, e := ParseSeedDecl(s); return e }},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			assertParseErr(t, tc.kind+" unknown annotation", tc.parse(tc.src), tc.want)
		})
	}
}

// 2d. Malformed @trigger on an automation -- an incomplete trigger annotation
// must not lower to a silently-untriggered automation.
func TestNegative_MalformedTrigger(t *testing.T) {
	cases := map[string]string{
		"empty-event-value": "@trigger(event=)\nautomation a {\n  step run { logic doThing { event: event } }\n}\n",
		"unclosed-trigger":  "@trigger(event=\"x\"\nautomation a {\n  step run { logic doThing { event: event } }\n}\n",
	}
	for label, src := range cases {
		t.Run(label, func(t *testing.T) {
			// A malformed @trigger survives the rewrite (it is an annotation,
			// not structure) and is rejected by the subsequent parse, so the
			// full rewrite-then-parse path is required here.
			_, err := rewriteAndParse(t, src)
			assertParseErr(t, "automation "+label, err)
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Invocation-site errors (expression grammar). Cross-ref: parser_hardening_
//    test.go (#2358) and kind_prefixed_invocation_test.go (#2335) already pin
//    the canonical probes; these use DIFFERENT inputs so a broader set of typos
//    is covered by the matrix.
// ---------------------------------------------------------------------------

func TestNegative_UnknownInvocationKind(t *testing.T) {
	cases := []struct{ src, wantHint string }{
		{`quer allNodes(x: 1)`, "did you mean 'query'?"},
		{`mutatio createNode(id: "x")`, "did you mean 'mutation'?"},
		{`logi decide(x: 1)`, "did you mean 'logic'?"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			_, err := ParseExpression(tc.src)
			assertParseErr(t, "unknown invocation kind "+tc.src, err,
				"not a construct-invocation kind", "would be silently dropped", tc.wantHint)
		})
	}
}

func TestNegative_LegacyObjectLiteralArgs(t *testing.T) {
	// Retired object-literal call form must be rejected (cross-ref
	// kind_prefixed_invocation_test.go, different call name here).
	for _, src := range []string{`createUser({ id: "x" })`, `createUser({})`} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			assertParseErr(t, "legacy object-literal "+src, err, "object-literal call args are removed")
		})
	}
}

func TestNegative_BadNamedArgSyntax(t *testing.T) {
	// A named-arg call with a missing value / missing key / missing colon must
	// fail rather than parse a garbage arg map.
	cases := []string{
		`createNode(id: )`,         // missing value
		`createNode(: "x")`,        // missing key
		`createNode(id "x")`,       // missing colon
		`mutation createNode(id:)`, // kind-prefixed, missing value
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			assertParseErr(t, "bad named-arg "+src, err)
		})
	}
}

func TestNegative_TrailingTokens(t *testing.T) {
	// The two EOF-enforcing entry points reject trailing garbage with distinct
	// messages; both are pinned so neither regresses to a silent drop (S3).
	t.Run("ParseExpression", func(t *testing.T) {
		_, err := ParseExpression(`active == true bogus trailing tokens`)
		assertParseErr(t, "ParseExpression trailing", err, "after expression")
	})
	t.Run("Parser.Parse-method", func(t *testing.T) {
		lex := NewLexer(`active == true bogus trailing tokens`)
		toks, err := lex.Tokenize()
		if err != nil {
			t.Fatalf("tokenize: %v", err)
		}
		_, err = NewParser(toks).Parse()
		assertParseErr(t, "Parse() method trailing", err, "after expression")
	})
}

func TestNegative_WordLogicalOperators(t *testing.T) {
	// The English `and` / `or` infix forms are not lexer keywords, so the parser
	// rejects them (the EOF check / body parse trips). This is a parser-level
	// backstop for the tree-wide dsl/no_word_logical_operators_test.go gate.
	t.Run("and-in-expression", func(t *testing.T) {
		_, err := ParseExpression(`a == 1 and b == 2`)
		assertParseErr(t, "`and` infix", err)
	})
	t.Run("or-in-expression", func(t *testing.T) {
		_, err := ParseExpression(`a == 1 or b == 2`)
		assertParseErr(t, "`or` infix", err)
	})
	t.Run("or-in-spec-body", func(t *testing.T) {
		_, err := ParseSpecDecl("@enabled\nspec activeRowTrait s {\n  return a == 1 or b == 2\n}\n")
		assertParseErr(t, "`or` in spec body", err)
	})
}

// ---------------------------------------------------------------------------
// 4. Position + message quality for the top cases: a parser-level rejection
//    must carry a source position ("line N, column M") so the author can find
//    the offending token.
// ---------------------------------------------------------------------------

func TestNegative_ErrorsCarryPosition(t *testing.T) {
	cases := map[string]func() error{
		"unknown-kind-prefix": func() error { _, e := ParseExpression(`quer allNodes(x: 1)`); return e },
		"trailing-tokens": func() error {
			lex := NewLexer(`active == true bogus`)
			toks, _ := lex.Tokenize()
			_, e := NewParser(toks).Parse()
			return e
		},
		"typo-top-level-keyword": func() error { _, e := ParseFile("@enabled\nconept foo { }"); return e },
		"spec-body-block": func() error {
			_, e := ParseSpecDecl("@enabled\nspec activeRowTrait s {\n  body { return active == true }\n}\n")
			return e
		},
	}
	for label, run := range cases {
		t.Run(label, func(t *testing.T) {
			err := run()
			if err == nil {
				t.Fatalf("%s: expected an error with a position", label)
			}
			msg := err.Error()
			if !strings.Contains(msg, "line ") || !strings.Contains(msg, "column ") {
				t.Errorf("%s: error should carry a `line N, column M` position\n  full: %s", label, msg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Retired-operator LAYER BOUNDARY (behavior pin, not a hole).
//
//    The retired filter operators `has` / `?.` / `;`-AND / `,`-OR are NOT
//    rejected by the parser -- they still lex/parse. They are enforced by the
//    tree-wide grep gate dsl/no_retired_operators_test.go (and word and/or by
//    dsl/no_word_logical_operators_test.go). This test PINS that layer split so
//    that (a) a future author knows WHERE each form is caught, and (b) if a
//    later story adds parser-level rejection, this pin fails and forces the doc
//    + the enforcement note to be updated in lockstep.
// ---------------------------------------------------------------------------

func TestRetiredOperators_ParserAcceptsToTreeScanGate(t *testing.T) {
	// These parse clean at the expression level today; the tree-scan gate is
	// what rejects them across the live .memql tree.
	acceptedByParser := []string{
		`tags has "x"`, // `has` -> enforced by dsl/no_retired_operators_test.go
		`a == 1 ; b == 2`,
		`a == 1 , b == 2`,
	}
	for _, src := range acceptedByParser {
		if _, err := ParseExpression(src); err != nil {
			t.Errorf("LAYER PIN: parser rejects %q now (err=%v).\n  If a story intentionally added parser-level rejection, move this case to an active negative test and update dsl/no_retired_operators_test.go's role note.", src, err)
		}
	}
}

// ===========================================================================
// KNOWN SILENT-ACCEPTANCE HOLES (surfaced by this suite, NOT fixed in S7).
//
// Per the S7 charter these are pinned as explicit skips + reported to the
// coordinator (memql#2383) rather than fixed here -- fixing a fail-loud gap is
// a production change that belongs to its own sub-story of epic #2351.
// ===========================================================================

// HOLE 1: shape / builtin / prompt / spec / policy silently ACCEPT an unknown
// annotation. tool / provider / seed reject (see the active test above); the
// 2026-07-03 audit (Part 3) flagged builtin + prompt specifically. There is no
// single annotation registry, so recognition is split and these five kinds have
// no unknown-annotation gate.
func TestHOLE_UnknownAnnotationSilentlyAccepted(t *testing.T) {
	t.Skip("HOLE (memql#2383 report): shape/builtin/prompt/spec/policy silently accept an unknown annotation (e.g. @bogusAnno). tool/provider/seed already reject. Needs a fail-loud sub-story of epic #2351 (single annotation registry). Un-skip + assert rejection once fixed.")

	// The intended assertions once the hole is closed:
	//   assertParseErr(t, "shape @bogusAnno",   mustErr(ParseShapeDecl(...)),   "unknown annotation @bogusAnno")
	//   assertParseErr(t, "builtin @bogusAnno", mustErr(ParseBuiltinDecl(...)), "unknown annotation @bogusAnno")
	//   assertParseErr(t, "prompt @bogusAnno",  mustErr(ParsePromptDecl(...)),  "unknown annotation @bogusAnno")
	//   assertParseErr(t, "spec @bogusAnno",    mustErr(ParseSpecDecl(...)),    "unknown annotation @bogusAnno")
	//   assertParseErr(t, "policy @bogusAnno",  mustErr(ParsePolicyDecl(...)),  "unknown annotation @bogusAnno")
}

// HOLE 2: positional args on a kind-prefixed construct call are silently
// accepted (mapped to keys "0", "1", ...). Bare positional args are legitimate
// for primitive builtins (coalesce(a, b)), so a fix must scope the rejection to
// kind-prefixed CONSTRUCT calls only.
func TestHOLE_PositionalArgsOnKindPrefixedCall(t *testing.T) {
	t.Skip("HOLE (memql#2383 report): a kind-prefixed construct call with positional args (e.g. `mutation createNode(\"x\", 1)`) is silently accepted and the positionals are mapped to keys \"0\"/\"1\". Primitive builtins legitimately take positional args, so a fix must scope rejection to construct calls. Un-skip once decided.")
}

// LAYER NOTE (not a hole): `spec("name")` / `trait("name")` parse clean at the
// expression level -- the parser lowers them to a plain FunctionCallExpr named
// "spec"/"trait". The retired call form is rejected downstream at the engine
// converter (component/memql/ast_converter.go: "the spec(\"name\") call form is
// removed; use the predicate form `spec <name>`"). Pinned here so the layer
// boundary is documented alongside the other invocation-site cases.
func TestLayerNote_SpecStringCallAcceptedAtParser(t *testing.T) {
	if _, err := ParseExpression(`spec("requiresOwner")`); err != nil {
		t.Errorf("LAYER PIN: parser now rejects spec(\"name\") (err=%v). If intentional, update the layer note -- rejection historically lived in ast_converter.go, not the parser.", err)
	}
}
