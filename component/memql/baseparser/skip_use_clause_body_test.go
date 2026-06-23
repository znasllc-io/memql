package baseparser

import (
	"testing"
)

// TestSkipUseClauseBody_FormA confirms the helper consumes a single-
// line legacy `use ns.concept [as alias]` statement.
func TestSkipUseClauseBody_FormA(t *testing.T) {
	cases := []string{
		`use cognition.participant
nextkeyword`,
		`use cognition.session as cognSess
nextkeyword`,
	}
	for _, src := range cases {
		var b Base
		b.Init(src, "test.memql")
		if !b.MatchWord("use") {
			t.Fatalf("MatchWord(\"use\") failed for %q", src)
		}
		b.SkipUseClauseBody()
		b.SkipWhitespaceAndComments()
		// Next non-blank token should be `nextkeyword`.
		got := b.ReadWord()
		if got != "nextkeyword" {
			t.Errorf("after SkipUseClauseBody, ReadWord = %q, want \"nextkeyword\". Source: %q", got, src)
		}
	}
}

// TestSkipUseClauseBody_FormB_SingleLine confirms the helper consumes
// a single-line Form B `use path.{ a, b }` statement.
func TestSkipUseClauseBody_FormB_SingleLine(t *testing.T) {
	src := `use cognition.shapes.{ participantFull, sessionFull }
nextkeyword`
	var b Base
	b.Init(src, "test.memql")
	if !b.MatchWord("use") {
		t.Fatal("MatchWord(\"use\") failed")
	}
	b.SkipUseClauseBody()
	b.SkipWhitespaceAndComments()
	got := b.ReadWord()
	if got != "nextkeyword" {
		t.Errorf("after SkipUseClauseBody, ReadWord = %q, want \"nextkeyword\"", got)
	}
}

// TestSkipUseClauseBody_FormB_MultiLine locks the multi-line body case.
func TestSkipUseClauseBody_FormB_MultiLine(t *testing.T) {
	src := `use common.traits.{
  isActiveRecord,
  isNotDeleted,
}
nextkeyword`
	var b Base
	b.Init(src, "test.memql")
	if !b.MatchWord("use") {
		t.Fatal("MatchWord(\"use\") failed")
	}
	b.SkipUseClauseBody()
	b.SkipWhitespaceAndComments()
	got := b.ReadWord()
	if got != "nextkeyword" {
		t.Errorf("after SkipUseClauseBody, ReadWord = %q, want \"nextkeyword\"", got)
	}
}
