package dslconformance

import (
	"strings"
	"testing"
)

// TestClauseGuaranteesReadsBooleanStructure is the regression evidence for
// memql#2832. The classifier used to decide "owned" with
// strings.Contains(body, "actor.userId"), which a single owner-scoped
// disjunct satisfies — so `ownerUserId==actor.userId || visibility=="public"`
// classified as owned while returning rows the caller does not own, and the
// hard-failing `flagged` bucket became unreachable behind it.
//
// Every `want: false` case below contains the literal "actor.userId" and so
// passed the old substring test. That is what makes these non-vacuous.
func TestClauseGuaranteesReadsBooleanStructure(t *testing.T) {
	ownerLeaf := func(p string) bool { return strings.Contains(p, "actor.userId") }

	cases := []struct {
		name   string
		clause string
		want   bool
	}{
		// Genuinely owner-scoped.
		{"bare owner predicate", `ownerUserId==actor.userId`, true},
		{"conjunct narrows", `ownerUserId==actor.userId && traitIsActiveRecord`, true},
		{"conjunct narrows, owner second", `traitIsActiveRecord && ownerUserId==actor.userId`, true},
		{"parenthesised owner", `(ownerUserId==actor.userId)`, true},
		{"disjunction inside a conjunct", `(a==args.a || b==args.b) && ownerUserId==actor.userId`, true},
		{"every arm scoped", `ownerUserId==actor.userId || createdBy==actor.userId`, true},

		// The defect: a disjunct WIDENS, so one unscoped arm breaks the
		// guarantee. All of these contain "actor.userId".
		{"public arm", `ownerUserId==actor.userId || visibility=="public"`, false},
		{"public arm first", `visibility=="public" || ownerUserId==actor.userId`, false},
		{"parenthesised disjunction", `(ownerUserId==actor.userId || visibility=="public")`, false},
		{"unscoped third arm", `ownerUserId==actor.userId || createdBy==actor.userId || visibility=="public"`, false},
		{"disjunct inside the scoped conjunct", `status=="a" && (ownerUserId==actor.userId || visibility=="public")`, false},

		// Negation inverts: "rows I do not own" is the opposite of scoped.
		{"negated owner", `!(ownerUserId==actor.userId)`, false},

		// A when() guard is dropped entirely when its arg is absent, so it
		// cannot carry the guarantee on its own.
		{"when-guarded owner", `when(args.userId) { ownerUserId==actor.userId }`, false},
		{"when-guarded owner with a conjunct", `when(args.userId) { ownerUserId==actor.userId } && status=="a"`, false},

		// Review round 1: two respellings that reopened the hole.
		//
		// `when (x)` with a space is legal MemQL -- the lexer is token-based
		// and memqlfmt does not normalise it -- so a prefix test for "when("
		// missed the guard entirely and read it as a guarantee.
		{"when guard with a space", `when (args.userId) { ownerUserId==actor.userId }`, false},
		{"when guard with a tab", "when\t(args.userId) { ownerUserId==actor.userId }", false},
		// An escaped quote left the string scanner stuck inside a literal, so
		// the `||` after it went unseen -- the headline case, respelled.
		{"escaped quote hides the disjunct", `name=="a\"b" || ownerUserId==actor.userId`, false},
		{"escaped quote, owner arm first", `ownerUserId==actor.userId || name=="a\"b"`, false},
		// `whenever` merely starts with "when"; it is not a guard.
		{"identifier starting with when", `whenClosed==actor.userId`, true},

		// Unbalanced text is unreachable (the parser rejects it) but must fail
		// CLOSED, not be read as one scoped predicate.
		{"unbalanced open paren", `(ownerUserId==actor.userId || visibility=="public"`, false},
		{"unbalanced close paren", `) || ownerUserId==actor.userId`, false},
		{"unterminated string", `name=="a || ownerUserId==actor.userId`, false},

		// Nothing to guarantee.
		{"empty clause", ``, false},
		{"no owner reference", `status=="a" && active==true`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clauseGuarantees(tc.clause, ownerLeaf); got != tc.want {
				t.Errorf("clauseGuarantees(%q) = %v, want %v", tc.clause, got, tc.want)
			}
		})
	}
}

// TestClauseGuaranteesAdminTier applies the same structure to the admin gate,
// which had the identical substring weakness: an `isClusterOwner` check on one
// arm of a disjunction does not gate the query.
func TestClauseGuaranteesAdminTier(t *testing.T) {
	adminLeaf := func(p string) bool {
		return strings.Contains(p, "actor.isClusterOwner") || strings.Contains(p, "requiresClusterOwner")
	}
	cases := []struct {
		clause string
		want   bool
	}{
		{`actor.isClusterOwner==true`, true},
		{`partitionId==args.partitionId && actor.isClusterOwner==true`, true},
		{`(fromE164==args.e164 || toE164==args.e164) && actor.isClusterOwner==true`, true}, // the shipped telephony shape
		{`actor.isClusterOwner==true || visibility=="public"`, false},
		{`requiresClusterOwner || status=="open"`, false},
	}
	for _, tc := range cases {
		if got := clauseGuarantees(tc.clause, adminLeaf); got != tc.want {
			t.Errorf("clauseGuarantees(%q) = %v, want %v", tc.clause, got, tc.want)
		}
	}
}

// TestFilterClauseOf pins the clause extraction the classifier runs on: it
// must capture continuation lines and stop at the next clause keyword, or the
// structural test would be reading the wrong text.
func TestFilterClauseOf(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"single line",
			"\n  filter  ownerUserId==actor.userId\n  shape  userFull\n",
			"ownerUserId==actor.userId",
		},
		{
			"continuation line",
			"\n  filter  ownerUserId==actor.userId &&\n    status==\"active\"\n  shape  userFull\n",
			"ownerUserId==actor.userId && status==\"active\"",
		},
		{
			"stops at sort",
			"\n  filter  a==args.a\n  sort  \"row.createdAt\", \"desc\"\n  shape  f\n",
			"a==args.a",
		},
		{
			"stops at paginate",
			"\n  filter  a==args.a\n  paginate 50\n",
			"a==args.a",
		},
		{
			"trailing comment is not part of the clause",
			"\n  filter  a==args.a // why\n  shape f\n",
			"a==args.a",
		},
		{
			"no filter clause",
			"\n  args {\n    x string\n  }\n  shape  f\n",
			"",
		},
		{
			"insert block is not a filter",
			"\n  insert {\n    ownerUserId: actor.userId\n  }\n",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterClauseOf(tc.body); got != tc.want {
				t.Errorf("filterClauseOf(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
