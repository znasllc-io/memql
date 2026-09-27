package parser

import (
	"errors"
	"strings"
	"testing"
)

// query_clause_duplicate_test.go -- memql#5429. A struct query's body is read
// line by line into one slot per clause, so a second `sort` line replaced the
// first one's keys, and a second `filter` line replaced the first filter --
// its conditions, an ownership test included -- without a word. A clause
// written twice is refused now, naming both lines.
func TestQueryClauseWrittenTwiceIsRefused(t *testing.T) {
	for _, tc := range []struct {
		clause, body string
		quotes       []string
	}{
		{"sort", "  filter row => row.status == \"open\"\n  sort \"row.createdAt\", \"desc\"\n  sort \"priority\", \"asc\"\n  paginate 20\n",
			[]string{"`sort \"row.createdAt\", \"desc\"`, then `sort \"priority\", \"asc\"`", "List every key in one sort clause"}},
		{"filter", "  filter row => row.ownerUserId == actor.userId\n  filter row => row.status == \"open\"\n  paginate 20\n",
			[]string{"`filter row => row.ownerUserId == actor.userId`, then `filter row => row.status == \"open\"`", "Join the conditions with `&&` in one filter"}},
		{"paginate", "  paginate 20\n  paginate 50\n",
			[]string{"`paginate 20`, then `paginate 50`", "Keep the one you mean."}},
	} {
		src := "query ticket probe {\n" + tc.body + "}\n"
		_, err := NormaliseQuerySource(src)
		if err == nil {
			t.Errorf("%s written twice: rewrote, want a refusal", tc.clause)
			continue
		}
		var dup *QueryClauseDuplicateError
		if !errors.As(err, &dup) || dup.Clause != tc.clause {
			t.Errorf("%s written twice: refused with %v, want a QueryClauseDuplicateError for %q", tc.clause, err, tc.clause)
			continue
		}
		if dup.RuleCode() != RuleQueryClauseDuplicate {
			t.Errorf("%s: code = %q", tc.clause, dup.RuleCode())
		}
		msg := err.Error()
		for _, want := range append(tc.quotes, `struct-form query "probe"`, "["+RuleQueryClauseDuplicate+"]") {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the refusal does not say %q:\n  %s", tc.clause, want, msg)
			}
		}
	}

	// The positive control: every clause once, a long filter continued on the
	// next line, rewrites clean.
	src := "query ticket probe {\n  filter row => row.ownerUserId == actor.userId\n    && row.status == \"open\"\n  sort \"priority\", \"desc\", \"row.createdAt\", \"desc\"\n  paginate 20\n  shape ticketCard\n}\n"
	if _, err := NormaliseQuerySource(src); err != nil {
		t.Errorf("a query writing each clause once was refused: %v", err)
	}
}

// The refusal is placed on the SECOND copy -- the line that used to win -- in
// the author's source.
func TestQueryClauseWrittenTwiceIsPlacedOnTheSecond(t *testing.T) {
	src := "query ticket probe {\n  sort \"row.createdAt\", \"desc\"\n  paginate 20\n  sort \"priority\", \"asc\"\n}\n"
	_, err := NormaliseQuerySource(src)
	placed := PositionRewriteError(src, err)
	var pe *PositionedRewriteError
	if !errors.As(placed, &pe) {
		t.Fatalf("the refusal carries no position: %v", err)
	}
	if line, col := pe.Parse.Position(); line != 4 || col != 3 {
		t.Errorf("placed at %d:%d, want 4:3 -- the second sort", line, col)
	}
}
