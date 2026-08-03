package memql

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// shortid_fixpoint_2981_test.go -- memql#2981.
//
// `BareShortId` is TOTAL but NOT idempotent. It calls `id.ParseNodeId` once and
// strips one canonical prefix; where its own output is not already a fixed
// point, a second application strips again:
//
//	"v1:cluster:deployment:v2:x:y:z" once -> "v2:x:y:z"  twice -> "z"
//	"v1:cluster:deployment: abc"     once -> " abc"      twice -> "abc"
//
// That matters because `authoring-rules.md` §20 leans on `shortId()` to
// collapse a bare/canonical pair onto one id, and for those inputs it does not:
// two callers naming one logical deployment write two rows with distinct ids.
//
// # The ruling (memql#2981)
//
// `BareShortId` is left ALONE. It is the wire-egress primitive (memql#2441) --
// `wire_bareids.go` runs it over `n.Id`, `n.CreatedBy`, `e.FromId`, `e.ToId`
// and `clone.RootIds`, and `component/grpc` calls it directly on ids handed to
// clients -- so looping it to a fixpoint would change what every client
// receives, and would destroy strictly more of a genuinely-bare colon-bearing
// value. The fork is closed at the ARGUMENT BOUNDARY instead, exactly as
// memql#2980 closed its sibling: reject the input, change no existing id, touch
// no wire behaviour.
//
// # What the boundary has to require
//
// Not "at most one `v<digits>` segment" -- that is the formulation memql#2978
// got wrong twice. The property that actually closes the fork is:
//
//	shortId(arg) must be a FIXED POINT
//
// because canonical `v1:cluster:deployment:B` strips once to `B` while bare `B`
// strips to `shortId(B)`, so the two agree when `B == shortId(B)`.
//
// The pattern is SUFFICIENT for that, not equivalent to it: it rejects plenty
// of values whose shortId is a fixed point (`"v1:v1"`, `"v1:a:b:"`). It is
// deliberately conservative in the safe direction, and calling it "precisely
// that condition" would be the third imprecise restatement in this issue's
// history -- see the landing-review note in the authoring rules.
//
// READ FROM THE DSL, never hand-copied. A Go const holding a second copy of
// the authored pattern is a drift seam: the two could disagree and every test
// here would stay green while the shipped mutation carried the weaker one.
// That is the same class of defect as the runtime_evaluator fallback this PR
// documents (memql#2981 landing review).
func deploymentIDPattern(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../dsl/deployment/mutations.memql")
	if err != nil {
		t.Fatalf("read the authored mutations file: %v", err)
	}
	re := regexp.MustCompile(`deploymentId\s+string!\s+@pattern\("([^"]+)"\)`)
	all := re.FindAllStringSubmatch(string(src), -1)
	if len(all) != 2 {
		t.Fatalf("expected the @pattern on deploymentId in BOTH composite-id mutations, found %d. "+
			"createDeploymentNodeSpec and updateDeploymentNodeSpec share one hashed id, so a "+
			"constraint on only one of them forks the timeline it exists to keep single.", len(all))
	}
	if all[0][1] != all[1][1] {
		t.Fatalf("the two deploymentId patterns differ:\n  %s\n  %s", all[0][1], all[1][1])
	}
	return all[0][1]
}

// TestBareShortIdIsNotIdempotent pins the behaviour itself, so a future change
// that quietly makes it a fixpoint is caught rather than silently widening what
// the boundary above is compensating for.
//
// This is deliberately the OPPOSITE of an aspiration: the doc comment on
// `BareShortId` used to claim "Total + idempotent", and this test is what makes
// the corrected claim checkable.
func TestBareShortIdIsNotIdempotent(t *testing.T) {
	for _, c := range []struct {
		in, once, twice string
	}{
		{"v1:a:b:v2:c:d:e", "v2:c:d:e", "e"},
		{"v1:cluster:deployment:v2:x:y:z", "v2:x:y:z", "z"},
		{"v1:cluster:deployment: abc", " abc", "abc"},
	} {
		got1 := BareShortId(c.in)
		got2 := BareShortId(got1)
		if got1 != c.once || got2 != c.twice {
			t.Errorf("BareShortId(%q) = %q then %q; want %q then %q.\n"+
				"If it is now a fixed point, memql#2981's boundary pattern may be over-strict "+
				"and the `Total + idempotent` claim can go back -- but check the wire-egress "+
				"callers in wire_bareids.go first, because that is what the ruling protected.",
				c.in, got1, got2, c.once, c.twice)
		}
	}
}

// TestDeploymentIDPatternClosesTheBareCanonicalFork is the load-bearing one.
//
// It measures the invariant over a generated corpus rather than a case list,
// and it carries a CONTROL: the same corpus without the pattern must still
// fork. Without that control the test passes just as happily against a corpus
// where nothing forks in the first place, which is the shape of measurement
// this issue was filed about.
//
// The alphabet is chosen against memql#2978's own lesson. That measurement
// scored zero disagreements over 349,524 values and was still wrong, because
// its alphabet `{v1, a, b, v2}` contained no EMPTY and no WHITESPACE segment --
// the two classes its formulation missed. Both are present below.
func TestDeploymentIDPatternClosesTheBareCanonicalFork(t *testing.T) {
	re := regexp.MustCompile(deploymentIDPattern(t))

	// The alphabet IS the measurement. #2981's closing argument is that #2978
	// "verified" its formulation over 349,524 values and was still wrong,
	// because its alphabet carried no empty and no whitespace segments. This
	// one adds those -- and NON-ASCII whitespace, which the first fix's own
	// alphabet still lacked, which is exactly how it shipped a guard that
	// closed the fork for U+0020 and left it open for U+00A0.
	alphabet := []string{
		"v1", "v10", "", "v", " ", "A_b", " x", "cluster", "deployment", "z",
		"\u00a0", "\u3000", "\u2028", "\u00a0x",
	}
	var values []string
	var build func(parts []string, depth int)
	build = func(parts []string, depth int) {
		if depth > 0 {
			values = append(values, strings.Join(parts, ":"))
		}
		if depth == 4 {
			return
		}
		for _, a := range alphabet {
			build(append(parts, a), depth+1)
		}
	}
	build(nil, 0)

	const canonicalPrefix = "v1:cluster:deployment:"

	var accepted, pairs, forks, notFixed int
	for _, v := range values {
		if !re.MatchString(v) {
			continue
		}
		accepted++

		// 1. Every accepted value's shortId is a fixed point. This is the
		//    property the whole boundary exists to guarantee.
		if s := BareShortId(v); s != BareShortId(s) {
			notFixed++
			if notFixed <= 3 {
				t.Errorf("accepted %q but BareShortId(%q) = %q is not a fixed point -- the "+
					"pattern admits a value whose bare and canonical forms still diverge "+
					"(memql#2981)", v, v, s)
			}
		}

		// 2. The fork itself, over REACHABLE pairs only. A canonical spelling
		//    the pattern rejects cannot be passed, so it cannot fork against
		//    anything -- counting it would manufacture failures that no caller
		//    can produce.
		canonical := canonicalPrefix + v
		if !re.MatchString(canonical) {
			continue
		}
		pairs++
		if BareShortId(v) != BareShortId(canonical) {
			forks++
			if forks <= 3 {
				t.Errorf("bare %q and canonical %q are both accepted and derive different ids "+
					"(%q vs %q) -- two rows for one logical deployment (memql#2981)",
					v, canonical, BareShortId(v), BareShortId(canonical))
			}
		}
	}

	// The control. If the corpus does not fork WITHOUT the pattern, the
	// measurement above proves nothing about the pattern.
	var unguardedForks int
	for _, v := range values {
		if BareShortId(v) != BareShortId(canonicalPrefix+v) {
			unguardedForks++
		}
	}
	if unguardedForks == 0 {
		t.Fatalf("control failed: no value in the %d-value corpus forks even WITHOUT the "+
			"pattern, so this test cannot show the pattern closes anything. Widen the "+
			"alphabet -- and see memql#2981 on why an alphabet with no empty and no "+
			"whitespace segment misses the two classes that matter.", len(values))
	}

	if pairs == 0 {
		t.Fatal("no reachable bare/canonical pair was checked -- the pattern rejects every " +
			"canonical form, which would mean it also rejects the shape memql#2925 landed to " +
			"support. Check the pattern before trusting the zero-fork result above.")
	}

	t.Logf("corpus=%d accepted=%d reachable-pairs=%d not-fixed-point=%d forks=%d "+
		"(control, no pattern: %d forks)",
		len(values), accepted, pairs, notFixed, forks, unguardedForks)
}

// TestDeploymentIDPatternAdmitsWhatTheTreeSends is the over-strictness guard.
//
// A pattern that closes the fork by rejecting real traffic is not a fix. The
// only in-tree producer is examples/deploypack, which forwards a bare uuid from
// id.NewShortId; the canonical form is what memql#2925 landed to support.
func TestDeploymentIDPatternAdmitsWhatTheTreeSends(t *testing.T) {
	re := regexp.MustCompile(deploymentIDPattern(t))
	for _, ok := range []string{
		"9f8e7d6c-1234-4abc-9def-000000000001", // id.NewShortId shape
		"v1:cluster:deployment:9f8e7d6c-1234-4abc-9def-000000000001",
		"abc123",
		"d-abc123",
	} {
		if !re.MatchString(ok) {
			t.Errorf("the deploymentId pattern rejects %q, which is a shape callers legitimately "+
				"send. Closing the fork must not cost the canonical form (memql#2925) or the "+
				"uuid (memql#2981).", ok)
		}
	}
}
