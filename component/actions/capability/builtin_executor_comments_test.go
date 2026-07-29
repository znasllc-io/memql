package capability

import (
	"regexp"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// builtin_executor_comments_test.go -- memql#2896 defect 3.
//
// builtinExecutors scans source for `builtin` headers and attributes the LAST
// @executor annotation appearing between the previous header and this one.
// Before #2896 both steps ran on RAW source, so a block-commented builtin was
// scanned as if it existed:
//
//	/* builtin zzParked { } */   <- does not exist, but was entered in the map
//
// This is the security-adjacent one of the three defects. `SideEffecting`
// returns false for an unknown executor, so a bogus or mis-attributed entry
// changes how the actions side-effect classifier rates a builtin. The load path
// for the same `builtin` keyword is already comment-aware (#2868), so the two
// disagree about which builtins exist.
//
// Note the shape: this scanner does no brace matching, so it is NOT a copy of
// the slice walk the other four call sites share. It needs the comment-blanked
// VIEW, not the walk.
//
// # WHY THE FIXTURES BELOW ARE SHAPED THE WAY THEY ARE
//
// An earlier version of this file asserted, on a fixture with the block comment
// sitting BETWEEN the @executor and the live builtin, that zzLive ends up with
// `integration.workbench.dispatchHost`. That assertion is wrong, and locking it
// in would have been worse than the bug it replaced. Measured against the real
// slicer, the loader emits for that source:
//
//	slice name="zzLive" source="builtin zzLive {\n  b string\n}"
//
// -- no @executor at all. The loader's preamble walk (preambleStartOf) climbs
// only contiguous `@` and `//` lines, and the `*/` closing the comment is
// neither, so the walk stops there and the annotation is left outside the
// slice. The engine therefore registers zzLive with NO executor.
//
// So on that fixture the pre-#2896 behaviour was accidentally in agreement with
// the loader (both gave zzLive nothing), and "fixing" the scanner to report the
// executor moved the defect from a HOLE (read-only for a real exec builtin) to
// NOISE (exec for a builtin the engine can never dispatch). Direction matters,
// and noise is the safer direction, but neither is agreement -- and agreement
// is the property #2896 is about.
//
// Whether an @executor above a `*/` SHOULD attach to the builtin below is a
// real question about the loader's preamble rule, not about this scanner. It is
// filed as memql#2965 rather than settled by an assertion here.
//
// The fixtures below are therefore split:
//
//   - parkedAbovePreambleSource -- the realistic shape, where both paths agree
//     and the full attribution can be asserted.
//   - parkedBetweenPreambleAndHeaderSource -- the awkward shape, where only the
//     existence claim is asserted, because the executor claim is the open
//     question above.

// parkedAbovePreambleSource is the realistic parked-builtin shape: the retired
// builtin is commented out ABOVE the live one's annotation preamble, so the
// preamble stays contiguous and both paths see the same thing.
//
// The parked builtin keeps its OWN @executor above it. That is not decoration:
// builtinExecutors only records a name when it finds an annotation governing
// it, so a parked builtin with no executor above it would be absent from the
// map even under the old raw scan, and this fixture would pass against the very
// bug it is meant to catch. With the annotation present, the raw scan enters
// zzParked in the map and the existence assertion fails -- as it must.
const parkedAbovePreambleSource = `
@executor("integration.agents.invoke")
/*
builtin zzParked {
  a string
}
*/
@executor("integration.workbench.dispatchHost")
@description("does real work")
builtin zzLive {
  b string
}
`

// parkedBetweenPreambleAndHeaderSource puts the comment between the annotation
// and the header it was written for. See the file comment: only the existence
// claim is assertable here.
const parkedBetweenPreambleAndHeaderSource = `
@executor("integration.workbench.dispatchHost")
@description("does real work")
/*
builtin zzParked {
  a string
}
*/
builtin zzLive {
  b string
}
`

// TestBuiltinExecutorsIgnoresBlockCommentedBuiltins is the core claim, and the
// one that fails against the pre-#2896 raw scan: a builtin that exists only
// inside a block comment does not exist. Asserted on BOTH fixtures, because
// this claim is independent of where the comment sits.
func TestBuiltinExecutorsIgnoresBlockCommentedBuiltins(t *testing.T) {
	for name, src := range map[string]string{
		"parkedAbovePreamble":            parkedAbovePreambleSource,
		"parkedBetweenPreambleAndHeader": parkedBetweenPreambleAndHeaderSource,
	} {
		t.Run(name, func(t *testing.T) {
			got := builtinExecutors(src)
			if _, present := got["zzParked"]; present {
				t.Errorf("block-commented builtin zzParked was scanned; it does not exist.\n"+
					"got map: %#v", got)
			}
			if _, present := got["zzLive"]; !present {
				t.Errorf("live builtin zzLive is missing from the map entirely.\ngot map: %#v", got)
			}
		})
	}
}

// TestBuiltinExecutorAttributionMatchesTheLoader is the agreement assertion, and
// it is deliberately a COMPARISON rather than a hardcoded string. #2896 is about
// two paths disagreeing over the same source; pinning each side to its own
// literal would let them drift apart again while both tests stayed green. Here
// the loader's answer is computed from the real slicer, so this fails if either
// side moves without the other.
func TestBuiltinExecutorAttributionMatchesTheLoader(t *testing.T) {
	const wantExec = "integration.workbench.dispatchHost"

	scannerSaw := builtinExecutors(parkedAbovePreambleSource)["zzLive"]
	loaderSaw := executorInLoaderSlice(t, parkedAbovePreambleSource, "zzLive")

	if loaderSaw != wantExec {
		t.Fatalf("fixture no longer exercises the agreeing case: the loader slice for zzLive "+
			"carries executor %q, want %q -- the preamble walk changed, so re-derive this "+
			"fixture before trusting the comparison below", loaderSaw, wantExec)
	}
	if scannerSaw != loaderSaw {
		t.Errorf("builtinExecutors and the loader disagree about zzLive's executor.\n"+
			"  scanner: %q\n  loader:  %q\n"+
			"That disagreement IS memql#2896; the two paths must resolve the same source "+
			"to the same executor.", scannerSaw, loaderSaw)
	}
}

// TestSideEffectingSurvivesAParkedBuiltin is the consequence, asserted
// separately because this is the assertion with reach outside the loader: an
// exec-class builtin misclassified as read-only skips whatever gating the
// classifier feeds. Uses the agreeing fixture, so a true classification here
// reflects an executor the engine will actually dispatch.
func TestSideEffectingSurvivesAParkedBuiltin(t *testing.T) {
	classify := classifierFromExecutors(builtinExecutors(parkedAbovePreambleSource))

	if !classify("zzLive") {
		t.Error("SideEffecting(\"zzLive\") is false for an exec-class builtin: " +
			"its @executor was absorbed by a block-commented builtin, so the " +
			"actions side-effect classifier treats it as read-only")
	}
}

// TestBuiltinExecutorsStillAttributesWithoutComments pins that the fix does not
// break the ordinary case -- the last @executor before a header still governs.
func TestBuiltinExecutorsStillAttributesWithoutComments(t *testing.T) {
	const src = `
@executor("integration.workbench.dispatchHost")
builtin zzFirst {
  a string
}

@executor("integration.agents.invoke")
builtin zzSecond {
  b string
}
`
	got := builtinExecutors(src)
	for name, want := range map[string]string{
		"zzFirst":  "integration.workbench.dispatchHost",
		"zzSecond": "integration.agents.invoke",
	} {
		if got[name] != want {
			t.Errorf("%s: got %q, want %q (full map: %#v)", name, got[name], want, got)
		}
	}
}

// executorInLoaderSlice reports the @executor the LOAD path attributes to the
// named builtin: it takes the declaration slice the loader would parse and
// reads the annotation out of that slice's own text. An annotation the preamble
// walk left outside the slice is one the engine never sees, which is exactly
// the distinction this helper exists to measure.
func executorInLoaderSlice(t *testing.T, source, name string) string {
	t.Helper()

	headerRe := regexp.MustCompile(`(?m)^[ \t]*builtin[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	execRe := regexp.MustCompile(`@executor\("([^"]+)"\)`)

	for _, slice := range languageParser.ExtractDeclarationSlices(source, headerRe) {
		if slice.Name != name {
			continue
		}
		// Only the LAST annotation governs, matching builtinExecutors.
		if m := execRe.FindAllStringSubmatch(slice.Source, -1); len(m) > 0 {
			return m[len(m)-1][1]
		}
		return ""
	}
	t.Fatalf("loader produced no slice named %q; slices present: %v",
		name, loaderSliceNames(source, headerRe))
	return ""
}

func loaderSliceNames(source string, headerRe *regexp.Regexp) string {
	var names []string
	for _, s := range languageParser.ExtractDeclarationSlices(source, headerRe) {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}
