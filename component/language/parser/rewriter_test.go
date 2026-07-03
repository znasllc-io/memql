package parser

import "testing"

// G5 (#2367): the step-call lowering expands puns textually for CONSTRUCT
// calls -- without it a punned arg lowered to a positional "0" key and the
// runtime rebuilt a broken query (the #2408 conformance fallout). Bare calls
// keep positional args.
func TestFinishCallKinded_ExpandsPuns(t *testing.T) {
	got, err := finishCallKinded("bootstrapSession", "( event )", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "bootstrapSession( event: event )" {
		t.Fatalf("pun expansion = %q", got)
	}
	got, err = finishCallKinded("createArtifact", `( title, summary: coalesce(summary, ""), kind: "memory" )`, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != `createArtifact( title: title, summary: coalesce(summary, ""), kind: "memory" )` {
		t.Fatalf("mixed pun expansion = %q", got)
	}
	// Bare (non-construct) calls untouched -- primitives keep positional.
	got, err = finishCallKinded("coalesce", "(a, b)", false)
	if err != nil || got != "coalesce(a, b)" {
		t.Fatalf("bare call changed: %q err=%v", got, err)
	}
}
