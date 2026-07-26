package memql

import (
	"os"
	"regexp"
	"testing"
)

// server_only_gate_parity_test.go -- memql#2875, the drift half.
//
// The FIX for #2875 is in dsl/server_only_parsed_test.go, and both dsl-side
// gates now consume it: TestPerRowAuthzClassification's `hasServerOnly` and
// TestServerOnlyConstructsAreDocumented's membership check read
// serverOnlyConstructs(), which applies the loader's own rule --
// hasFlagAttribute(attrs, "serverOnly") -- to the same parse. Those two cannot
// diverge from Function.ServerOnly.
//
// TWO REGEX SITES REMAIN, and this file pins their pattern:
//
//	sdk/gen/gen.go                 the SDK skip. Must stay regex -- it is a
//	                               GENERATOR that runs over an arbitrary --dsl
//	                               root without booting an engine, so a parsed
//	                               verdict would couple codegen to engine Init.
//	dsl/server_only_authz_test.go  uses it only to LOCATE the annotation line,
//	                               because the doc block is found by walking up
//	                               from it. WHICH constructs are server-only
//	                               comes from the parsed set; a location whose
//	                               construct is not in that set is skipped.
//
// An earlier version of this file also ran a tree-wide regex/registry
// comparison. That is deleted rather than repaired, because review found it
// wrong in two ways at once and the parsed verdict supersedes it:
//
//   - it regexed the whole SLICE (preamble + body + prepended `use` lines)
//     while the sites regex the PREAMBLE, so an `@serverOnly` inside a BODY
//     annotation string was reported as FAIL-OPEN with three false claims
//     attached, on a construct nothing had exempted;
//   - its hand-rolled preambleOf diverged from sdk/gen's attrPreamble on blank
//     lines. ExtractFunctionSlices' own preamble walk breaks at a blank line, so
//     for `@serverOnly\n\nquery foo {` the slice arrived with NO preamble:
//     preambleOf said false, Function.ServerOnly said false, the test compared
//     false to false and PASSED -- while sdk/gen genuinely computed true and
//     dropped the construct from the client SDK. The one disagreement it existed
//     to catch was the one it could not see.
//
// Comparing a reconstruction against the real thing was the error. Pinning the
// pattern is smaller and cannot be wrong in that direction.

// serverOnlyRegexSource is the pattern both remaining sites compile. Duplicated
// here deliberately: the point is to notice an edit to those literals, which
// importing them would make invisible by construction.
const serverOnlyRegexSource = `(?m)^@serverOnly\b`

// TestServerOnlyRegexPatternIsPinnedAtEverySiteThatStillUsesIt guards the two
// remaining regex verdicts against drifting from the parsed one.
func TestServerOnlyRegexPatternIsPinnedAtEverySiteThatStillUsesIt(t *testing.T) {
	for _, site := range []struct{ path, varName string }{
		{"../../sdk/gen/gen.go", "serverOnlyRe"},
		{"../../dsl/server_only_authz_test.go", "serverOnlyRe"},
	} {
		raw, err := os.ReadFile(site.path)
		if err != nil {
			t.Errorf("read %s: %v -- if the file moved, move this check with it; without it a "+
				"regex verdict can drift from the parsed audit unnoticed", site.path, err)
			continue
		}
		decl := regexp.MustCompile(regexp.QuoteMeta(site.varName) +
			`\s*=\s*regexp\.MustCompile\(` + "`" + `([^` + "`" + `]*)` + "`" + `\)`)
		m := decl.FindSubmatch(raw)
		if m == nil {
			t.Errorf("%s: could not find `%s = regexp.MustCompile(...)`. If this site now derives "+
				"its verdict from the parsed tree, delete its entry here rather than repairing "+
				"this check.", site.path, site.varName)
			continue
		}
		if got := string(m[1]); got != serverOnlyRegexSource {
			t.Errorf("%s: %s compiles %q, this file expects %q -- that site's verdict has drifted "+
				"from the parsed audit in dsl/server_only_parsed_test.go; reconcile them.",
				site.path, site.varName, got, serverOnlyRegexSource)
		}
	}
}
