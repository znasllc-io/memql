package sense

import (
	"strings"
	"testing"
)

// TestBareRowIntrinsicRuleFlagsFilters is the edit-time half of
// dsl.TestFilterIntrinsicsUseRowNamespace (memql#2779).
func TestBareRowIntrinsicRuleFlagsFilters(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // the intrinsic that should be flagged, "" for no diagnostic
	}{
		{"bare id", "query widget q {\n  filter id == args.widgetId\n}\n", "id"},
		{"bare id tight", "query widget q {\n  filter id==args.widgetId\n}\n", "id"},
		{"bare createdAt", "query widget q {\n  filter createdAt < args.before\n}\n", "createdAt"},
		{"inside a when guard", "query widget q {\n  filter when(args.x) { id==args.x }\n}\n", "id"},
		{"conjunction", "query widget q {\n  filter ownerUserId==actor.userId && id==args.x\n}\n", "id"},

		// Already namespaced -- the canonical form.
		{"row.id", "query widget q {\n  filter row.id == args.widgetId\n}\n", ""},
		{"row.createdAt", "query widget q {\n  filter row.createdAt < args.before\n}\n", ""},
		// Other namespaces and payload properties must not flag.
		{"args field", "query widget q {\n  filter ownerUserId == args.id\n}\n", ""},
		{"actor field", "query widget q {\n  filter ownerUserId == actor.userId\n}\n", ""},
		{"payload prop ending in id", "query widget q {\n  filter threadId == args.threadId\n}\n", ""},
		{"payload prop named region", "query widget q {\n  filter region == args.region\n}\n", ""},
		// A spec/trait body reads bound fields bare and REJECTS row.* (#2281);
		// this rule must not reach it.
		{"spec return body", "spec widget isSeeded {\n  return createdBy == \"seed\"\n}\n", ""},
		// Commented-out code is not authored code.
		{"comment", "query widget q {\n  // filter id == args.x\n  filter region == args.r\n}\n", ""},
		// Reviewer-found holes (memql#2780 review): boolean shape must not
		// hide a bare intrinsic, and a string literal must not manufacture one.
		{"or-joined", "query widget q {\n  filter ownerUserId==actor.userId || id==args.x\n}\n", "id"},
		{"parenthesized", "query widget q {\n  filter (id==args.x)\n}\n", "id"},
		{"negated", "query widget q {\n  filter !(id==args.x)\n}\n", "id"},
		{"in operator", "query widget q {\n  filter id in args.ids\n}\n", "id"},
		{"lowercase intrinsic", "query widget q {\n  filter createdat < args.x\n}\n", "createdAt"},
		{"provenance leaf", "query widget q {\n  filter provenance.kind==\"automation\"\n}\n", "provenance.kind"},
		// A filter clause SPANS LINES. This case once asserted the opposite, on
		// the ground that parseStructQueryBody hard-errored on a continuation
		// line; memql#4123 made the normaliser fold them, so `id==args.x` below
		// reaches the engine as a payload read and must be flagged. The
		// edition-2026 codemod wraps every long filter this way.
		{"continuation line is part of the clause", "query widget q {\n  filter ownerUserId==actor.userId &&\n    id==args.x\n}\n", "id"},
		{"leading-operator continuation", "query widget q {\n  filter ownerUserId==actor.userId\n    && id==args.x\n}\n", "id"},
		// ...but the clause ends where the normaliser ends it.
		{"clause ends at the next keyword", "query widget q {\n  filter ownerUserId==actor.userId\n  shape x\n}\n", ""},
		// Edition 2026: a row intrinsic is a member of the parameter, and a
		// name a lambda binds is not the row's.
		{"v1 row member", "query widget q {\n  filter  row => row.id == args.x\n          && row.createdAt > args.since\n}\n", ""},
		{"v1 lambda-bound name", "query widget q {\n  filter  row => row.items.any(id => id == args.x)\n}\n", ""},
		{"v1 bare intrinsic on a wrapped line", "query widget q {\n  filter  row => row.a == 1\n          && id == args.x\n}\n", "id"},
		// Leafless `provenance` has no push-down and `row.provenance` is
		// rejected outright, so flagging it would name a spelling nothing
		// accepts.
		{"leafless provenance is not flagged", "query widget q {\n  filter provenance==\"x\"\n}\n", ""},
		// Commented-out code in a block comment is not authored code.
		{"block comment", "query widget q {\n  /* filter id == args.x */\n  filter region == args.r\n}\n", ""},
		{"url literal does not truncate", "query widget q {\n  filter url==\"http://x\" && id==args.a\n}\n", "id"},
		// A string literal containing what looks like a predicate is not one.
		{"intrinsic inside a string literal", "query widget q {\n  filter name==\"a id==b\"\n}\n", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bareRowIntrinsicRule(tc.src)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected no diagnostic, got %d: %s", len(got), got[0].Message)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 diagnostic, got %d", len(got))
			}
			if !strings.Contains(got[0].Message, "`row."+tc.want+"`") {
				t.Errorf("message must name the replacement `row.%s`, got: %s", tc.want, got[0].Message)
			}
			if got[0].Severity != SeverityWarning {
				t.Errorf("Severity = %v, want Warning (the engine still resolves bare intrinsics; only the gates retired them)", got[0].Severity)
			}
			if got[0].Code != "bare-row-intrinsic" {
				t.Errorf("Code = %q, want bare-row-intrinsic", got[0].Code)
			}
		})
	}
}

// TestBareRowIntrinsicRuleAnchorsOnTheIntrinsic -- the squiggle must land on
// the offending token, not on the line start, or the quick-fix has nothing to
// replace.
func TestBareRowIntrinsicRuleAnchorsOnTheIntrinsic(t *testing.T) {
	src := "query widget q {\n  filter id == args.widgetId\n}\n"
	got := bareRowIntrinsicRule(src)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(got))
	}
	d := got[0]
	if d.Range.Start.Line != 2 {
		t.Errorf("Line = %d, want 2", d.Range.Start.Line)
	}
	line := strings.Split(src, "\n")[1]
	if got := line[d.Range.Start.Column-1 : d.Range.End.Column-1]; got != "id" {
		t.Errorf("range covers %q, want \"id\"", got)
	}
}
