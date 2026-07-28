package capability

import "testing"

// builtin_executor_comments_test.go -- memql#2896 defect 3.
//
// builtinExecutors scans RAW source: it finds `builtin` headers with a regex and
// attributes the LAST @executor annotation appearing in the region between the
// previous header and this one. Neither step is comment-aware, so a
// block-commented builtin between a live @executor and its live builtin
// absorbs that executor:
//
//	@executor("integration.workbench.dispatchHost")   <- meant for zzLive
//	/*
//	builtin zzParked { }                              <- steals it
//	*/
//	builtin zzLive { }                                <- gets ""
//
// This is the security-adjacent one of the three. `SideEffecting` returns false
// for an unknown executor, so an exec-class builtin whose executor was stolen
// classifies as READ-ONLY in the actions side-effect classifier. The load path
// for the same `builtin` keyword is already comment-aware (#2868), so the two
// disagree about which builtins exist.
//
// Note the shape: this scanner does no brace matching, so it is NOT a copy of
// the slice walk the other four call sites share. It needs the comment-blanked
// VIEW, not the walk.

// aParkedBuiltinMustNotStealTheLiveExecutor is the region the bug lives in: one
// @executor, one commented builtin, one live builtin.
const parkedThenLiveSource = `
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

func TestBuiltinExecutorsIgnoresBlockCommentedBuiltins(t *testing.T) {
	got := builtinExecutors(parkedThenLiveSource)

	if _, present := got["zzParked"]; present {
		t.Errorf("block-commented builtin zzParked was scanned; it does not exist.\n"+
			"got map: %#v", got)
	}

	const wantExec = "integration.workbench.dispatchHost"
	if got["zzLive"] != wantExec {
		t.Errorf("live builtin zzLive lost its @executor to a commented-out neighbour.\n"+
			"  got  %q\n  want %q\n  full map: %#v", got["zzLive"], wantExec, got)
	}
}

// TestSideEffectingSurvivesAParkedBuiltin is the consequence, asserted
// separately because this is the assertion with reach outside the loader: an
// exec-class builtin misclassified as read-only skips whatever gating the
// classifier feeds.
func TestSideEffectingSurvivesAParkedBuiltin(t *testing.T) {
	classify := classifierFromExecutors(builtinExecutors(parkedThenLiveSource))

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
