package automations

import (
	"strings"
	"testing"
)

// annotation_preamble_test.go -- memql#2872.
//
// The @-attribute preamble walk in extractAutomationSlicesReporting stopped at
// any line not starting with `@` or `//`. A `/* ... */` line is exactly such a
// line, so EVERY annotation above the comment was cut out of the emitted slice.
// Both directions are silent:
//
//   - `@disabled` dropped  -> the automation loads ENABLED and fires on every
//     replica. Verbatim the "the author turned it off and it runs anyway"
//     defect.
//   - `@trigger` dropped   -> the automation loads with a NIL trigger:
//     registered, counted in the loaded total, never subscribed, never
//     scheduled. IsEventTriggered() is false for a nil trigger, so it is
//     invisible to the shippedAutomationCount guard and to every #2830 problem
//     channel.
//
// WHY THE OBVIOUS FIX IS WRONG, and what this file therefore has to pin.
//
// "Make the walk step over comment lines" was tried twice in #2866 and reverted
// twice: stepping over a comment pulls the COMMENT BODY into the emitted slice,
// and compileMemQL runs raw-text gates over slice text. On ordinary, valid,
// memqllint-clean input that loads fine today, that turns a silent drop into a
// BOOT REFUSAL -- a comment mentioning `$steps.`, or containing an
// `@`-annotation, or a commented-out `mutation(...)` call. Worse, a
// commented-out copy of an automation above the live one (exactly what an
// author writes when parking a version) silently DISABLES the #2712 annotation
// gate, because ValidateConstructAnnotations cuts its header scan at the first
// `automation ... {` -- which lands on the commented-out header.
//
// So the fix is not "skip comments in the walk". It is "every gate that scans
// raw construct text must scan a COMMENT-BLANKED view", which is the same
// defect class as #1074 / #2868 / #2896. These tests pin BOTH halves: the
// annotations survive, AND ordinary comment content cannot trip a gate.

// sliceFor extracts the named automation's slice from source.
func sliceFor(t *testing.T, source, name string) string {
	t.Helper()
	slices, unextracted := extractAutomationSlicesReporting(source)
	if len(unextracted) > 0 {
		t.Fatalf("headers went unextracted: %v", unextracted)
	}
	for _, s := range slices {
		if s.Name == name {
			return s.Source
		}
	}
	t.Fatalf("automation %q not sliced; got %d slice(s)", name, len(slices))
	return ""
}

const bodyStub = `{
  step s {
    logic someLogic ( event: event )
  }
}`

// TestSlicePreambleKeepsAnnotationsAboveABlockComment is the headline case.
func TestSlicePreambleKeepsAnnotationsAboveABlockComment(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			name: "disabled above a whole-line block comment",
			src: `@disabled
/* parked 2026-07-01, see #123 */
@trigger(event="node.created", concept="v1:cluster:node")
automation x ` + bodyStub,
			want: "@disabled",
		},
		{
			name: "trigger above a block comment",
			src: `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
/* @description("temporarily removed") */
automation y ` + bodyStub,
			want: "@trigger(",
		},
		{
			name: "comment delimiter sharing the annotation's line",
			src: `/* parked, see #123 */ @disabled
@trigger(event="node.created", concept="v1:cluster:node")
automation b ` + bodyStub,
			want: "@disabled",
		},
		{
			name: "multi-line comment ending on the annotation's line",
			src: `@enabled
/* note
   more */ @trigger(event="node.created", concept="v1:cluster:node")
automation a ` + bodyStub,
			want: "@trigger(",
		},
		{
			name: "annotation above a doc comment above a block comment",
			src: `@disabled
/// a doc comment
/* and a block one */
@trigger(event="node.created", concept="v1:cluster:node")
automation c ` + bodyStub,
			want: "@disabled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.TrimSpace(tc.src[strings.LastIndex(tc.src, "automation ")+len("automation "):])
			name = strings.Fields(name)[0]
			got := sliceFor(t, tc.src, name)
			if !strings.Contains(got, tc.want) {
				t.Errorf("the emitted slice lost %s.\n\nsource:\n%s\n\nslice:\n%s\n\n"+
					"An annotation above a block comment is cut out of the slice, and both "+
					"directions are silent: a dropped @disabled means the automation RUNS on "+
					"every replica, and a dropped @trigger means it loads with a nil trigger "+
					"that is never subscribed and is invisible to the shipped-count guard "+
					"(memql#2872).", tc.want, tc.src, got)
			}
		})
	}
}

// TestSlicePreambleStopsAtRealCode is the other direction. The walk must not
// become "swallow everything above": a blank line or a preceding construct
// still ends the preamble, or one automation's slice absorbs the previous
// automation's annotations.
func TestSlicePreambleStopsAtRealCode(t *testing.T) {
	src := `@disabled
@trigger(event="node.created", concept="v1:cluster:node")
automation first ` + bodyStub + `

@enabled
@trigger(event="node.updated", concept="v1:cluster:node")
automation second ` + bodyStub

	second := sliceFor(t, src, "second")
	if strings.Contains(second, "@disabled") {
		t.Errorf("the second automation's slice absorbed the FIRST one's @disabled -- the "+
			"preamble walk no longer stops at a construct boundary, so parking one automation "+
			"would silently disable its neighbour.\nslice:\n%s", second)
	}
	if strings.Contains(second, "automation first") {
		t.Errorf("the second slice absorbed the first automation entirely:\n%s", second)
	}
	if !strings.Contains(second, "@enabled") {
		t.Errorf("the second slice lost its own @enabled:\n%s", second)
	}
}

// TestOrdinaryCommentContentDoesNotRefuseBoot pins the half that made the two
// naive attempts worse than the bug. Each of these is valid, memqllint-clean
// input that loads today; pulling the comment into the slice made
// compileMemQL's raw-text gates refuse the whole boot.
func TestOrdinaryCommentContentDoesNotRefuseBoot(t *testing.T) {
	l := &Loader{}
	for _, tc := range []struct{ name, comment string }{
		{"mentions $steps", `/* the old form used $steps.foo */`},
		{"contains an annotation", "/*\n@public\n*/"},
		{"contains a retired annotation", "/*\n@useConcept(node)\n*/"},
		{"contains a direct mutation call", "/*\n x := mutation(concept: \"v1:cluster:node\")\n*/"},
		{"contains an inline step block", "/*\n step s { query: \"x\" }\n*/"},
		{"line comment mentions $steps", `// the old form used $steps.foo`},
		// The G5 retirement gate (#2367) is a FIFTH raw-text scan --
		// scrubSourceForPayloadScan blanked string literals and `//` comments
		// but had no `/*` arm, so this note refused the whole tree and the
		// diagnostic blamed the automation for a read that exists only in a
		// comment.
		{"mentions a retired event.payload read", `/* before #2367 this read event.payload.status directly */`},
		{"line comment mentions event.payload", `// before #2367 this read event.payload.status directly`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.comment + `
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation ok ` + bodyStub

			if _, err := l.compileMemQL(src, "test:"+tc.name); err != nil {
				t.Errorf("an ordinary COMMENT refused the boot: %v\n\nsource:\n%s\n\n"+
					"compileMemQL's raw-text gates scan slice text, so comment content trips "+
					"them. They must scan a comment-blanked view -- otherwise fixing the "+
					"dropped-annotation bug just trades a silent drop for a boot refusal on "+
					"ordinary comments (memql#2872).", err, src)
			}
		})
	}
}

// TestGatesStillFireOnRealCode is the direction that keeps the fix honest: the
// gates must still refuse the constructs they exist to refuse when the text is
// REAL and not inside a comment.
func TestGatesStillFireOnRealCode(t *testing.T) {
	l := &Loader{}
	for _, tc := range []struct{ name, src, wantErr string }{
		{
			name: "real $steps reference",
			src: `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation bad {
  step s {
    logic someLogic ( event: $steps.other.result )
  }
}`,
			wantErr: "$steps.",
		},
		{
			name: "real unknown annotation",
			src: `@enabled
@public
@trigger(event="node.created", concept="v1:cluster:node")
automation bad ` + bodyStub,
			wantErr: "@public",
		},
		{
			name: "real retired annotation",
			src: `@enabled
@useConcept(node)
@trigger(event="node.created", concept="v1:cluster:node")
automation bad ` + bodyStub,
			wantErr: "retired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := l.compileMemQL(tc.src, "test:"+tc.name)
			if err == nil {
				t.Fatalf("the gate did not fire on REAL code -- blanking comments must not "+
					"blank live source.\nsource:\n%s", tc.src)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("wrong refusal: %v (want it to mention %q)", err, tc.wantErr)
			}
		})
	}
}

// TestCommentedOutHeaderDoesNotShadowTheAnnotationGate is the subtlest half.
//
// ValidateConstructAnnotations cuts its header scan at the first
// `automation ... {`. If that lands on a COMMENTED-OUT header -- exactly what an
// author writes when parking a version above the live one -- the live
// automation's annotations are never inspected at all, and the #2712 gate is
// silently disabled. An invalid @public then loads clean.
func TestCommentedOutHeaderDoesNotShadowTheAnnotationGate(t *testing.T) {
	l := &Loader{}
	src := `/*
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation parkedOldVersion {
  step s {
    logic someLogic ( event: event )
  }
}
*/
@enabled
@public
@trigger(event="node.created", concept="v1:cluster:node")
automation live ` + bodyStub

	_, err := l.compileMemQL(src, "test:shadow")
	if err == nil {
		t.Fatal("an invalid @public on the LIVE automation loaded clean.\n\n" +
			"A commented-out automation header above it shadowed the annotation gate: " +
			"ValidateConstructAnnotations cuts its header scan at the first `automation ... {`, " +
			"which landed inside the comment, so the live automation's annotations were never " +
			"inspected. The gate must scan a comment-blanked view (memql#2872 / #2712).")
	}
	if !strings.Contains(err.Error(), "@public") {
		t.Errorf("refused, but not for the right reason: %v", err)
	}
}

// TestSliceNeverStartsInsideAComment is BLOCKER 1 from the #2872 review.
//
// The preamble walk is per-line, so it could stop with preambleStart already
// INSIDE a comment span. The shape is a block comment whose OPENER shares a
// line with real code, directly above the next automation's preamble: on the
// opener's line the code before `/*` survives blanking, so the stop test sees a
// non-empty line and breaks -- but preambleStart has already advanced onto the
// comment's LAST line.
//
// The emitted slice then begins with dangling comment text and parses as code.
// Because the strict gate is all-or-nothing, that ONE ordinary comment refuses
// the whole tree: every shipped automation fails to load. It is the documented
// "slice starts mid-comment" failure that sank both earlier attempts, reached
// by a route no column-0 fixture can hit.
func TestSliceNeverStartsInsideAComment(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{
			name: "block comment opener shares a line with code",
			src: `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation firstOne ` + bodyStub + ` /* parked 2026-07-01, see #123
     second line of the note */
@enabled
@trigger(event="node.updated", concept="v1:cluster:node")
automation secondOne ` + bodyStub,
		},
		{
			name: "opener shares a line, comment spans three lines",
			src: `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation firstOne ` + bodyStub + ` /* one
   two
   three */
@enabled
@trigger(event="node.updated", concept="v1:cluster:node")
automation secondOne ` + bodyStub,
		},
		{
			name: "blank line inside the block comment",
			src: `@disabled
/* parked

   see #123 */
@trigger(event="node.created", concept="v1:cluster:node")
automation withBlank ` + bodyStub,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slices, unextracted := extractAutomationSlicesReporting(tc.src)
			if len(unextracted) > 0 {
				t.Fatalf("headers went unextracted: %v", unextracted)
			}
			l := &Loader{}
			for _, s := range slices {
				if _, err := l.compileMemQL(s.Source, "test:"+s.Name); err != nil {
					t.Errorf("slice %q does not compile: %v\n\nslice:\n%s\n\n"+
						"The slice starts inside a comment, so the dangling comment text parses "+
						"as code. The strict automation gate is all-or-nothing, so one ordinary "+
						"comment like this refuses the WHOLE tree -- every shipped automation "+
						"fails to load (memql#2872).", s.Name, err, s.Source)
				}
			}
		})
	}
}

// TestCommentedOutPreconditionIsNotLive is BLOCKER 2 from the review.
//
// extractPreconditions scanned RAW slice text and STRIPPED what it matched. It
// was unreachable from above the automation header until the preamble walk
// started carrying comment bodies into the slice -- after which a COMMENTED-OUT
// precondition became a live one, silently.
//
// It fails CLOSED: a precondition miss aborts the automation before any step
// runs and emits healing.precondition.missed. So commenting a precondition out
// ENFORCED it, which is worse than either direction of the original bug.
func TestCommentedOutPreconditionIsNotLive(t *testing.T) {
	src := `/*
precondition envIsStaging {
  check: $config.MEMQL_ENV == "staging"
  literal: MEMQL_ENV
  description: "Only drive the staging deploy spine in staging."
}
*/
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation live ` + bodyStub

	l := &Loader{}
	auto, err := l.compileMemQL(src, "test:commented-precondition")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if len(auto.Preconditions) != 0 {
		t.Errorf("a COMMENTED-OUT precondition loaded as live: %d precondition(s), first=%+v\n\n"+
			"extractPreconditions scans slice text and strips what it matches. A precondition "+
			"fails CLOSED -- a miss aborts the automation before any step runs -- so commenting "+
			"one out ENFORCED it (memql#2872).", len(auto.Preconditions), auto.Preconditions[0])
	}
}

// TestRealPreconditionStillLoads is the honesty direction for the above.
func TestRealPreconditionStillLoads(t *testing.T) {
	src := `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation live {
  precondition envIsStaging {
    check: $config.MEMQL_ENV == "staging"
    literal: MEMQL_ENV
    description: "Only drive the staging deploy spine in staging."
  }
  step s {
    logic someLogic ( event: event )
  }
}`
	l := &Loader{}
	auto, err := l.compileMemQL(src, "test:real-precondition")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if len(auto.Preconditions) != 1 || auto.Preconditions[0].ID != "envIsStaging" {
		t.Fatalf("a REAL precondition did not load: %+v\nBlanking comments must not blank live "+
			"source.", auto.Preconditions)
	}
}

// TestIndentedCommentKeepsThePreamble covers the `original != ""` half of the
// walk's condition, which the review found completely untested: dropping it
// went undetected by the whole suite.
//
// It matters for an INDENTED comment. `  // parked` has its lineStart BEFORE
// the comment span begins, so the span check says "not in a comment" and only
// `original != ""` keeps the walk going.
func TestIndentedCommentKeepsThePreamble(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			name: "indented line comment",
			src: `@disabled
  // parked, see #123
@trigger(event="node.created", concept="v1:cluster:node")
automation x ` + bodyStub,
			want: "@disabled",
		},
		{
			name: "indented block comment",
			src: `@disabled
    /* parked, see #123 */
@trigger(event="node.created", concept="v1:cluster:node")
automation y ` + bodyStub,
			want: "@disabled",
		},
		{
			name: "tab-indented comment",
			src:  "@disabled\n\t/* parked */\n@trigger(event=\"node.created\", concept=\"v1:cluster:node\")\nautomation z " + bodyStub,
			want: "@disabled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.Fields(strings.TrimSpace(tc.src[strings.LastIndex(tc.src, "automation ")+len("automation "):]))[0]
			got := sliceFor(t, tc.src, name)
			if !strings.Contains(got, tc.want) {
				t.Errorf("an INDENTED comment cut the preamble: slice lost %s\n\nsource:\n%s\n\nslice:\n%s",
					tc.want, tc.src, got)
			}
		})
	}
}

// TestRegexGatesStillFireOnRealCode covers the two REGEX gates.
// TestGatesStillFireOnRealCode only reached the `$steps.` substring gate and
// the annotation gate, so blanking could have neutered these two unnoticed.
func TestRegexGatesStillFireOnRealCode(t *testing.T) {
	l := &Loader{}
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			name:    "direct mutation call",
			body:    `  step s {` + "\n" + `    x := mutation(concept: "v1:cluster:node")` + "\n" + `  }`,
			wantErr: "direct query() and mutation() calls",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// With a comment present, so the blanked path is the one exercised.
			src := `/* an ordinary note */
@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation bad {
` + tc.body + `
}`
			_, err := l.compileMemQL(src, "test:"+tc.name)
			if err == nil {
				t.Fatalf("the regex gate did not fire on REAL code:\n%s", src)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("wrong refusal: %v (want %q)", err, tc.wantErr)
			}
		})
	}
}

// TestCommentedOutPreconditionFieldDoesNotWin covers the other half of the
// precondition scan.
//
// parsePreconditionBody assigns on every regex match, so LAST MATCH WINS. A
// commented-out `check:` AFTER the live one silently replaced it -- and it is
// ordering-dependent, so the same comment above the live line was harmless.
// Pre-existing (body comments were always in the slice), but the same defect in
// the same function whose detection half this PR fixed.
func TestCommentedOutPreconditionFieldDoesNotWin(t *testing.T) {
	src := `@enabled
@trigger(event="node.created", concept="v1:cluster:node")
automation live {
  precondition p {
    check: $config.MEMQL_ENV == "staging"
    description: "live"
    /*
    check: $config.MEMQL_ENV == "PARKED"
    description: "parked"
    */
  }
  step s {
    logic someLogic ( event: event )
  }
}`
	l := &Loader{}
	auto, err := l.compileMemQL(src, "test:precondition-field-shadow")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if len(auto.Preconditions) != 1 {
		t.Fatalf("expected 1 precondition, got %d", len(auto.Preconditions))
	}
	pc := auto.Preconditions[0]
	if strings.Contains(pc.Check, "PARKED") {
		t.Errorf("a COMMENTED-OUT check replaced the live one: check=%q\n\n"+
			"parsePreconditionBody assigns on every match, so the last one wins and a commented "+
			"block AFTER the live line silently takes over (memql#2872).", pc.Check)
	}
	if pc.Description == "parked" {
		t.Errorf("a COMMENTED-OUT description replaced the live one: %q", pc.Description)
	}
	if !strings.Contains(pc.Check, "staging") {
		t.Errorf("the LIVE check was lost: %q -- blanking must not blank live source", pc.Check)
	}
}
