package dsl

import (
	"strings"
	"testing"
)

// memql#2787: splitPredicates split a filter clause on `&&` only, and
// splitFilterRef returns "" for any predicate not starting with an identifier
// character. A violation inside an `||` disjunct, inside parens, or behind a
// `!` therefore reached TestFilterSyntaxCanonical (the `payload.` prefix ban)
// as an unreadable predicate and was inspected by nothing.
//
// TestNoInlineTraitablePredicates, the splitter's other consumer, was not
// blind -- it regex-matches predicate TEXT rather than the head -- but it
// reported the whole parenthesized group instead of the offending comparison,
// and lost anything OR-ed with a `when(x) { }` guard, because
// unwrapWhenPredicate cuts at the last `}`.
//
// The gates walk the shipped corpus, so a hole in the splitter is silent: CI
// stays green on a clause it never actually read. These tests pin the splitter
// directly, which is the shared root both gates consume.

// predicateHeads is what the gates actually see: the leading identifier of
// every predicate the splitter yields. A head the splitter never emits is a
// head no gate can reject.
func predicateHeads(clause string) []string {
	var heads []string
	for _, p := range splitPredicates(clause) {
		head, _ := splitFilterRef(p)
		heads = append(heads, head)
	}
	return heads
}

func containsHead(heads []string, want string) bool {
	for _, h := range heads {
		if h == want {
			return true
		}
	}
	return false
}

// TestSplitPredicatesSeesIntoDisjuncts is the issue's demonstrated case: the
// exact clause from dsl/telephony/queries.memql:48 with a retired `payload.`
// prefix planted in the second disjunct. Before the fix the whole
// parenthesized group came back as one predicate starting with `(`, so
// splitFilterRef returned "" and the violation passed CI.
func TestSplitPredicatesSeesIntoDisjuncts(t *testing.T) {
	for _, tc := range []struct {
		name, clause, wantHead string
	}{
		{
			name:     "violation in a parenthesized disjunct",
			clause:   `(fromE164==args.e164 || payload.toE164==args.e164) && actor.isClusterOwner==true`,
			wantHead: "payload",
		},
		{
			name:     "violation in a top-level disjunct",
			clause:   `fromE164==args.e164 || payload.toE164==args.e164`,
			wantHead: "payload",
		},
		{
			name:     "violation behind a negation",
			clause:   `!payload.deleted && ownerUserId==actor.userId`,
			wantHead: "payload",
		},
		{
			name:     "violation behind a negated group",
			clause:   `!(payload.deleted == true)`,
			wantHead: "payload",
		},
		{
			name:     "violation nested two groups deep",
			clause:   `a==args.a && (b==args.b || (c==args.c && payload.d==args.d))`,
			wantHead: "payload",
		},
		{
			name:     "violation in the second half of a when() guard",
			clause:   `when(args.x) { a==args.a && payload.b==args.b }`,
			wantHead: "payload",
		},
		{
			// unwrapWhenPredicate cuts at the LAST `}`, so everything OR-ed
			// after a guard was discarded outright rather than merely
			// unreadable -- the predicate never reached any gate at all.
			name:     "violation OR-ed after a when() guard",
			clause:   `when(args.x) { a==args.a } || payload.b==args.b`,
			wantHead: "payload",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heads := predicateHeads(tc.clause)
			if !containsHead(heads, tc.wantHead) {
				t.Errorf("splitter never surfaced a %q head, so no gate can reject it\n  clause: %s\n  heads:  %v",
					tc.wantHead, tc.clause, heads)
			}
		})
	}
}

// The splitter must not invent predicates or lose the ones it already found:
// every head below is a legitimate one the gates rely on seeing.
func TestSplitPredicatesKeepsLegitimateHeads(t *testing.T) {
	for _, tc := range []struct {
		clause string
		want   []string
	}{
		{`ownerUserId==actor.userId`, []string{"ownerUserId"}},
		{`spaceId==args.spaceId && traitIsActiveRecord`, []string{"spaceId", "traitIsActiveRecord"}},
		{`(fromE164==args.e164 || toE164==args.e164) && actor.isClusterOwner==true`, []string{"fromE164", "toE164", "actor"}},
		{`when(args.status) { status==args.status }`, []string{"status"}},
	} {
		heads := predicateHeads(tc.clause)
		for _, want := range tc.want {
			if !containsHead(heads, want) {
				t.Errorf("clause %q lost head %q; heads=%v", tc.clause, want, heads)
			}
		}
		// A `(` head means a predicate the gates cannot inspect at all.
		for _, h := range heads {
			if h == "" {
				t.Errorf("clause %q produced an unreadable predicate (empty head); heads=%v", tc.clause, heads)
			}
		}
	}
}

// String literals must still suppress splitting: a connective inside a quoted
// value is data, not structure.
func TestSplitPredicatesIgnoresConnectivesInStrings(t *testing.T) {
	clause := `label=="a || b" && ownerUserId==actor.userId`
	got := splitPredicates(clause)
	if len(got) != 2 {
		t.Fatalf("want 2 predicates (the quoted || is data), got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"a || b"`) {
		t.Errorf("first predicate lost its quoted value: %q", got[0])
	}
}
