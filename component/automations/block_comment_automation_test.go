package automations_test

// block_comment_automation_test.go -- coverage for block-comment awareness in
// automation slice extraction (memql#2861).
//
// Extraction ran a regex over RAW source. Nothing on that path knew about
// `/* */`, though the lexer has always treated a block comment as
// commented-out source, so three shapes misbehaved:
//
//   - a `/* ... */`-commented automation was extracted and loaded LIVE.
//     Commenting one out did not disable it -- it kept firing on its trigger,
//     on every replica that loaded the file. That is the reported defect.
//   - once #2830 turned a dropped automation into a refused boot, the same
//     blindness turned two shapes into a REFUSED BOOT rather than a silent
//     load: a commented-out automation whose `{` sits on the next line (the
//     loose-header path reports it unextractable), and a healthy automation
//     carrying a brace inside a block comment in its body (findMatchingBrace
//     counted it and truncated the slice).
//
// The semantics pinned here match the lexer (component/language/parser/
// lexer.go, skipBlockComment): block comments do NOT nest -- the first `*/`
// closes -- an unterminated `/*` runs to EOF, and `/*` inside a string literal
// is not a comment.
//
// NOT covered here, deliberately: an annotation ABOVE a block comment is still
// cut out of the slice by the @-attribute preamble walk, so `@disabled` and
// `@trigger` are silently dropped. That defect predates this file and is
// tracked in memql#2872 -- see the note on the walk in unified_loader.go for
// why fixing it is not a drive-by.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/automations"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// liveAutomation is a well-formed automation used as the control in every
// fixture below: it must survive whatever the block comment around it does.
const liveAutomation = `@enabled
@description("control")
@trigger(event="node.created", concept="v1:cluster:node")
automation blockCommentControl {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`

// loadFixtureAutomations registers src as a throwaway domain -- the same
// RegisterTree path a product DSL bundle mounts through -- and returns
// everything LoadAll yields, plus its error and the loader's log output.
func loadFixtureAutomations(t *testing.T, domain, src string) ([]*automations.Automation, string, error) {
	t.Helper()
	t.Setenv(memql.AllowSkipsEnvVar, "") // strict regardless of ambient env

	memqldsl.RegisterTree(domain, fstest.MapFS{"automations.memql": {Data: []byte(src)}})
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	loader := automations.NewLoader(automations.LoaderOptions{
		Registry: loadedRegistry(t),
		Logger:   logger,
	})
	loaded, err := loader.LoadAll()
	return loaded, logs.String(), err
}

// loadFixtureDomain is loadFixtureAutomations reduced to the names, for the
// tests that only care about presence.
func loadFixtureDomain(t *testing.T, domain, src string) ([]string, error) {
	t.Helper()
	loaded, _, err := loadFixtureAutomations(t, domain, src)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(loaded))
	for _, a := range loaded {
		names = append(names, a.Name)
	}
	return names, nil
}

// findAutomation returns the loaded automation with the given name, or nil.
func findAutomation(loaded []*automations.Automation, name string) *automations.Automation {
	for _, a := range loaded {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestBlockCommentedAutomationIsAbsent is the core acceptance test for #2861.
//
// A `/* ... */`-commented automation must not load. "Comment it out to disable
// it" is a universal expectation, and the alternative reading -- that a
// commented-out workflow keeps running -- has no defensible author intent
// behind it.
//
// Failing-first check: against the pre-fix extractor this returns the control
// PLUS `commentedOutAutomation`, and the test fails on the presence assertion.
func TestBlockCommentedAutomationIsAbsent(t *testing.T) {
	src := `/*
@trigger(event="node.created", concept="v1:cluster:node")
automation commentedOutAutomation {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
*/

` + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixturecommented", src)
	if err != nil {
		t.Fatalf("a file whose only defect is a commented-out automation must load clean, got: %v", err)
	}
	if contains(names, "commentedOutAutomation") {
		t.Error("a /* */-commented automation must NOT load: commenting one out is how an author disables it, and loading it anyway means the workflow keeps firing on its trigger (#2861)")
	}
	if !contains(names, "blockCommentControl") {
		t.Error("the live automation next to the comment must still load -- stripping comments must not eat real automations")
	}
}

// TestBlockCommentedAutomationBraceOnNextLineDoesNotRefuseBoot pins the shape
// that #2830 made severe. The strict struct regex requires `{` on the header
// line, so a commented-out automation spelled with `{` on the next line is
// caught by the LOOSE header scan and reported unextractable -- which since
// #2830 refuses the boot outright.
//
// Commenting out an automation must never take the node down.
func TestBlockCommentedAutomationBraceOnNextLineDoesNotRefuseBoot(t *testing.T) {
	src := `/*
@trigger(event="node.created", concept="v1:cluster:node")
automation commentedNextLineBrace
{
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
*/

` + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixturenextline", src)
	if err != nil {
		t.Fatalf("commenting out an automation must not refuse the boot; LoadAll failed with: %v", err)
	}
	if contains(names, "commentedNextLineBrace") {
		t.Error("the commented-out automation must not load")
	}
	if !contains(names, "blockCommentControl") {
		t.Error("the live control automation must still load")
	}
}

// TestBraceInsideBlockCommentDoesNotTruncateSlice pins the third shape: a
// HEALTHY automation carrying a brace inside a block comment in its body.
// findMatchingBrace was string- and line-comment aware but not block-comment
// aware, so it counted the `}` in `/* } */`, closed the slice early, and
// handed the compiler a truncated automation -- a refused boot since #2830.
func TestBraceInsideBlockCommentDoesNotTruncateSlice(t *testing.T) {
	src := `@enabled
@description("brace inside a block comment in the body")
@trigger(event="node.created", concept="v1:cluster:node")
automation braceInBlockComment {
  /* this comment closes a brace } and opens one { on purpose */
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`

	names, err := loadFixtureDomain(t, "s2861fixturebracecomment", src)
	if err != nil {
		t.Fatalf("a brace inside a block comment must not truncate the slice; LoadAll failed with: %v", err)
	}
	if !contains(names, "braceInBlockComment") {
		t.Error("the automation must load: a brace inside a block comment is not a brace (#2861)")
	}
}

// TestBlockCommentMarkerInsideStringIsNotAComment is the over-stripping guard.
// Comment stripping must be string-literal aware, or a `/*` appearing inside a
// step argument would blank the rest of the automation -- trading the reported
// bug for a worse one.
func TestBlockCommentMarkerInsideStringIsNotAComment(t *testing.T) {
	src := `@enabled
@description("a string carrying comment markers")
@trigger(event="node.created", concept="v1:cluster:node")
automation commentMarkerInString {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "/* not a comment */")
  }
}
`

	names, err := loadFixtureDomain(t, "s2861fixturestringmarker", src)
	if err != nil {
		t.Fatalf("`/*` inside a string literal is not a comment; LoadAll failed with: %v", err)
	}
	if !contains(names, "commentMarkerInString") {
		t.Error("the automation must load: comment stripping has to be string-literal aware (#2861)")
	}
}

// TestBlockCommentDoesNotNest pins the lexer's semantics rather than an
// intuition about them: block comments do NOT nest, so the FIRST `*/` closes
// the comment and everything after it is live code again.
//
// This is the one shape where matching the lexer means the automation DOES
// load. Getting it wrong in the other direction -- treating the inner `/*` as
// opening a nested comment -- would silently disable a live automation.
func TestBlockCommentDoesNotNest(t *testing.T) {
	src := `/* outer /* inner */
@enabled
@description("live again after the first close")
@trigger(event="node.created", concept="v1:cluster:node")
automation afterFirstClose {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`

	names, err := loadFixtureDomain(t, "s2861fixturenonest", src)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if !contains(names, "afterFirstClose") {
		t.Error("block comments do not nest (lexer.go skipBlockComment): the first */ closes, so this automation is live code and must load (#2861)")
	}
}

// TestUnterminatedBlockCommentRunsToEOF pins the other lexer edge: an
// unterminated `/*` comments out the remainder of the file. Nothing after it
// loads -- which is exactly what the lexer does with the same input.
func TestUnterminatedBlockCommentRunsToEOF(t *testing.T) {
	src := liveAutomation + `
/* unterminated -- everything past here is comment
@trigger(event="node.created", concept="v1:cluster:node")
automation neverClosed {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`

	names, err := loadFixtureDomain(t, "s2861fixtureunterminated", src)
	if err != nil {
		t.Fatalf("an unterminated block comment must not refuse the boot, got: %v", err)
	}
	if contains(names, "neverClosed") {
		t.Error("an unterminated /* comments out the rest of the file (lexer.go skipBlockComment runs to EOF), so this must not load (#2861)")
	}
	if !contains(names, "blockCommentControl") {
		t.Error("the automation BEFORE the unterminated comment must still load")
	}
}

// TestLineCommentSuppressesBlockCommentOpen pins the interaction between the
// two comment forms: a `/*` that appears after `//` on the same line is inside
// a line comment, so it never opens a block comment. Getting this wrong would
// blank every automation below a line like `// see /* note */ above`.
func TestLineCommentSuppressesBlockCommentOpen(t *testing.T) {
	src := `// a line comment mentioning /* an opener that never opens
` + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixturelinesuppress", src)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if !contains(names, "blockCommentControl") {
		t.Error("a /* inside a // line comment does not open a block comment, so the automation below it must load (#2861)")
	}
}

// TestBlockCommentedAutomationIsNotReportedUnextractable guards the diagnostic
// channel as well as the load. A commented-out automation is ABSENT, not a
// dropped automation, so it must not surface as an unextractable-header WARN
// either -- an operator chasing a phantom name they deliberately commented out
// is the same defect wearing a different hat.
func TestBlockCommentedAutomationIsNotReportedUnextractable(t *testing.T) {
	src := `/*
automation phantomHeader
{
  step s { mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r") }
}
*/

` + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixturephantom", src)
	if err != nil {
		if strings.Contains(err.Error(), "phantomHeader") {
			t.Fatalf("a commented-out automation must not be reported as unextractable, got: %v", err)
		}
		t.Fatalf("LoadAll failed: %v", err)
	}
	if contains(names, "phantomHeader") {
		t.Error("the commented-out automation must not load")
	}
}

// TestBlockCommentedTerseAutomationIsAbsent covers the TERSE spelling
// (memql#2215) -- `automation NAME @trigger(...) => logic X`, trigger inline.
//
// This matters because terse lowering runs over RAW source BEFORE extraction,
// and measured, it DOES rewrite text inside a block comment (lowering it to
// longhand in place). The comment survives the rewrite, so the blanked
// extractor is what decides -- and it must decide "absent".
//
// The subtest asserting the fixture really is terse is deliberate: an earlier
// version of this test put @trigger on its own line, which is not the terse
// form at all, so it silently duplicated the loose-header case and passed for
// the wrong reason (#2861 review D4).
func TestBlockCommentedTerseAutomationIsAbsent(t *testing.T) {
	const terse = `automation terseCommented @trigger(event="node.created", concept="v1:cluster:node") => logic logicNoSuchThing`

	if !languageParser.LooksLikeTerseAutomation(terse) {
		t.Fatalf("fixture is not a terse automation, so this test would silently cover something else: %q", terse)
	}

	src := "/*\n" + terse + "\n*/\n\n" + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixtureterse", src)
	if err != nil {
		t.Fatalf("a commented-out terse automation must not refuse the boot, got: %v", err)
	}
	if contains(names, "terseCommented") {
		t.Error("a commented-out TERSE automation must not load (#2861)")
	}
	if !contains(names, "blockCommentControl") {
		t.Error("the live control automation must still load")
	}
}

// TestBlockCommentedTerseArgsBlockDoesNotRefuseBoot is memql#2861 review D3.
//
// rejectTerseAutomationArgsBlock (component/language/parser/rewriter.go) scans
// RAW source and runs BEFORE extraction, so a commented-out args block plus
// terse automation raised an authoring violation and REFUSED THE BOOT -- with
// nothing outside the comment live. That is this issue's headline shape
// ("commenting out an automation took the node down") surviving in the lane
// the extraction fix does not reach.
func TestBlockCommentedTerseArgsBlockDoesNotRefuseBoot(t *testing.T) {
	src := `/*
args {
  x string @required
}
automation terseWithArgs @trigger(event="node.created", concept="v1:cluster:node") => logic logicNoSuchThing
*/

` + liveAutomation

	names, err := loadFixtureDomain(t, "s2861fixtureterseargs", src)
	if err != nil {
		t.Fatalf("a commented-out args block + terse automation must not refuse the boot; got: %v", err)
	}
	if contains(names, "terseWithArgs") {
		t.Error("the commented-out terse automation must not load")
	}
	if !contains(names, "blockCommentControl") {
		t.Error("the live control automation must still load")
	}
}

// TestUnterminatedBlockCommentWarns closes the silent-drop channel this fix
// would otherwise open. An unterminated `/*` comments out the rest of the
// file, so every automation below it goes absent -- correct per the lexer, but
// #2830's doctrine is that a dropped automation is never silent. The input is
// lexer-legal, so it warns rather than refusing the boot.
func TestUnterminatedBlockCommentWarns(t *testing.T) {
	src := liveAutomation + `
/* unterminated -- everything past here is comment
@trigger(event="node.created", concept="v1:cluster:node")
automation swallowed {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`

	loaded, logs, err := loadFixtureAutomations(t, "s2861fixtureunterminatedwarn", src)
	if err != nil {
		t.Fatalf("an unterminated block comment is lexer-legal and must not refuse the boot, got: %v", err)
	}
	if findAutomation(loaded, "swallowed") != nil {
		t.Error("an unterminated /* runs to EOF, so the automation below it must not load")
	}
	if !strings.Contains(logs, "unterminated block comment") {
		t.Errorf("silently swallowing every automation below a typo'd /* is exactly what #2830 outlawed -- expected a WARN, got logs:\n%s", logs)
	}
	// liveAutomation is 8 lines + trailing newline, then a blank line, so the
	// unterminated opener sits on line 10.
	if !strings.Contains(logs, "line=10") {
		t.Errorf("the WARN must name the line the comment opens on so the operator can find it, got logs:\n%s", logs)
	}
}

// TestUnterminatedBlockCommentLineIgnoresNonOpeners guards the WARN against
// false positives: a `/*` inside a string literal or inside a `//` line
// comment is not an opener, and a closed comment is not unterminated. A
// spurious warning on healthy files trains operators to ignore the real one.
func TestUnterminatedBlockCommentLineIgnoresNonOpeners(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"closed comment", "/* fine */\nautomation a {}\n", 0},
		{"marker in a string", "automation a { reason: \"/* not an opener\" }\n", 0},
		{"marker in a line comment", "// see /* note\nautomation a {}\n", 0},
		{"no comments at all", "automation a {}\n", 0},
		{"unterminated on line 1", "/* opens and never closes\nautomation a {}\n", 1},
		{"unterminated on line 3", "automation a {}\n\n/* opens\n", 3},
		{"closed then unterminated", "/* one */\n/* two\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := languageParser.UnterminatedBlockCommentLine(tc.src); got != tc.want {
				t.Errorf("UnterminatedBlockCommentLine = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBlockCommentAboveAutomationDoesNotDisturbTheLoad pins the blast radius
// of this change at the preamble boundary.
//
// Two rejected attempts at the memql#2872 preamble defect made the walk step
// OVER comment lines, which pulled the comment body INTO the emitted slice.
// compileMemQL then ran its raw-text gates over commented-out text, so an
// ordinary explanatory comment could refuse the node's boot. These cases are
// all valid MemQL (memqllint-clean) and must load cleanly; each one refused
// the boot under those attempts.
func TestBlockCommentAboveAutomationDoesNotDisturbTheLoad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		comment string
	}{
		{"mentions $steps.", "/* the old form used $steps.foo -- do not reintroduce */"},
		{"contains an annotation", "/*\n@public\n*/"},
		{"contains a retired annotation", "/*\n@useConcept(node)\n*/"},
		{"contains a direct mutation call", "/*\n  x := mutation(concept: \"v1:cluster:node\")\n*/"},
		{"has two blank lines", "/* para one\n\n\npara two\n*/"},
		{"is a parked copy of the automation", "/*\n@enabled\n@trigger(event=\"node.created\", concept=\"v1:cluster:node\")\nautomation parkedCopy {\n  step s { logic x { v: $steps.prev.result } }\n}\n*/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.comment + "\n" + liveAutomation
			names, err := loadFixtureDomain(t, "s2861fixtureabove"+strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' {
					return r
				}
				return -1
			}, tc.name), src)
			if err != nil {
				t.Fatalf("an ordinary block comment above an automation must not affect the load, got: %v", err)
			}
			if !contains(names, "blockCommentControl") {
				t.Error("the automation below the comment must load")
			}
		})
	}
}

// TestAnnotationGateStillSeesTheLiveAutomation guards the memql#2712
// annotation gate against the same over-step. ValidateConstructAnnotations
// cuts its header scan at the first `automation ... {`; if a commented-out
// automation above reaches the slice, the cut lands on the COMMENTED header
// and the live automation's annotations are never inspected -- silently
// re-opening the gap #2712 closed.
func TestAnnotationGateStillSeesTheLiveAutomation(t *testing.T) {
	src := `/*
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation parkedOne {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
*/
@public
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation gatedAutomation {
  step persist {
    mutation createSpawnEvent (nodeId: "a", nodeType: "b", action: "stopped", reason: "r")
  }
}
`
	_, err := loadFixtureDomain(t, "s2861fixturegate", src)
	if err == nil {
		t.Fatal("@public is not valid on an automation and the #2712 gate must still reject it -- a commented-out automation above must not become the header the gate scans (#2861 review round 2, F3)")
	}
	if !strings.Contains(err.Error(), "gatedAutomation") && !strings.Contains(err.Error(), "@public") {
		t.Errorf("the refusal must name the live automation or the offending annotation, got: %v", err)
	}
}
