package dslconformance

import (
	"strings"
	"testing"
)

// TestClauseGuaranteesReadsBooleanStructure is the regression evidence for
// memql#2832. The classifier used to decide "owned" with
// strings.Contains(body, "actor.userId"), which a single owner-scoped
// disjunct satisfies -- so `row.ownerUserId == actor.userId ||
// row.visibility == "public"` classified as owned while returning rows the
// caller does not own, and the hard-failing `flagged` bucket became
// unreachable behind it.
//
// Every `want: false` case below contains the literal "actor.userId" and so
// passes a substring test. That is what makes these non-vacuous.
func TestClauseGuaranteesReadsBooleanStructure(t *testing.T) {
	ownerLeaf := func(p string) bool { return strings.Contains(p, "actor.userId") }

	cases := []struct {
		name   string
		clause string
		want   bool
	}{
		// Genuinely owner-scoped.
		{"bare owner predicate", `row => row.ownerUserId == actor.userId`, true},
		{"conjunct narrows", `row => row.ownerUserId == actor.userId && traitIsActiveRecord(row)`, true},
		{"conjunct narrows, owner second", `row => traitIsActiveRecord(row) && row.ownerUserId == actor.userId`, true},
		{"parenthesised owner", `row => (row.ownerUserId == actor.userId)`, true},
		{"disjunction inside a conjunct", `row => (row.a == args.a || row.b == args.b) && row.ownerUserId == actor.userId`, true},
		{"every arm scoped", `row => row.ownerUserId == actor.userId || row.createdBy == actor.userId`, true},

		// The defect: a disjunct WIDENS, so one unscoped arm breaks the
		// guarantee. All of these contain "actor.userId".
		{"public arm", `row => row.ownerUserId == actor.userId || row.visibility == "public"`, false},
		{"public arm first", `row => row.visibility == "public" || row.ownerUserId == actor.userId`, false},
		{"parenthesised disjunction", `row => (row.ownerUserId == actor.userId || row.visibility == "public")`, false},
		{"unscoped third arm", `row => row.ownerUserId == actor.userId || row.createdBy == actor.userId || row.visibility == "public"`, false},
		{"disjunct inside the scoped conjunct", `row => row.status == "a" && (row.ownerUserId == actor.userId || row.visibility == "public")`, false},

		// Negation inverts: "rows I do not own" is the opposite of scoped.
		{"negated owner", `row => !(row.ownerUserId == actor.userId)`, false},

		// The optional-argument guard admits every row when its argument is
		// absent, so it cannot carry the guarantee on its own.
		{"guarded owner", `row => (args.userId == nil || row.ownerUserId == actor.userId)`, false},
		{"guarded owner with a conjunct", `row => (args.userId == nil || row.ownerUserId == actor.userId) && row.status == "a"`, false},

		// An escaped quote must not hide a disjunct -- the headline case,
		// respelled.
		{"escaped quote hides the disjunct", `row => row.name == "a\"b" || row.ownerUserId == actor.userId`, false},
		{"escaped quote, owner arm first", `row => row.ownerUserId == actor.userId || row.name == "a\"b"`, false},
		// A quoted "actor.userId" is data, not a reference.
		{"a quoted reference", `row => row.note == "actor.userId"`, false},
		// A parameter shadowing a reserved root makes `actor.userId` a ROW
		// field: no caller check at all.
		{"a parameter named actor", `actor => actor.userId == args.x`, false},

		// A clause that does not parse must fail CLOSED, not be read as one
		// scoped predicate.
		{"unbalanced open paren", `row => (row.ownerUserId == actor.userId || row.visibility == "public"`, false},
		{"unterminated string", `row => row.name == "a || row.ownerUserId == actor.userId`, false},
		{"the retired comma", `row => (row.ownerUserId == actor.userId, row.visibility == "public")`, false},
		// And so must the retired `filter <predicate>` form.
		{"no lambda header", `ownerUserId==actor.userId`, false},

		// Nothing to guarantee.
		{"empty clause", ``, false},
		{"no owner reference", `row => row.status == "a" && row.active == true`, false},
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
		{`row => actor.isClusterOwner == true`, true},
		{`row => row.partitionId == args.partitionId && actor.isClusterOwner == true`, true},
		{`row => (row.fromE164 == args.e164 || row.toE164 == args.e164) && actor.isClusterOwner == true`, true}, // the shipped telephony shape
		{`row => actor.isClusterOwner == true || row.visibility == "public"`, false},
		{`row => requiresClusterOwner(actor) || row.status == "open"`, false},
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
			"\n  filter  row => row.ownerUserId == actor.userId\n  shape  userFull\n",
			"row => row.ownerUserId == actor.userId",
		},
		{
			"continuation line",
			"\n  filter  row => row.ownerUserId == actor.userId\n    && row.status == \"active\"\n  shape  userFull\n",
			"row => row.ownerUserId == actor.userId && row.status == \"active\"",
		},
		{
			"stops at sort",
			"\n  filter  row => row.a == args.a\n  sort  \"row.createdAt\", \"desc\"\n  shape  f\n",
			"row => row.a == args.a",
		},
		{
			"stops at paginate",
			"\n  filter  row => row.a == args.a\n  paginate 50\n",
			"row => row.a == args.a",
		},
		{
			"trailing comment is not part of the clause",
			"\n  filter  row => row.a == args.a // why\n  shape f\n",
			"row => row.a == args.a",
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
