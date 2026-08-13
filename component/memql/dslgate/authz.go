package dslgate

// authz.go carries the two authorization contract gates, moved to load time by
// memql#3629:
//
//   - GateUserScopeSelection -- the hard-failing bucket of the per-row authz
//     classification (docs/public/operate/auth/per-row-authz-audit.md): a
//     construct that SELECTS rows by a user-scope column while checking nothing
//     about the caller.
//   - GateAdminGateComposition -- an admin gate composed so that a FALSE gate
//     does not zero the result set (memql#2839). The engine primitive is
//     correct; whether the false leaf actually empties the row set is a
//     property of how the author composed it, and nothing checked that.
//
// Both are contract gates in the strict sense: what they protect is not a
// convention but who can read which rows. Both were tests over this repo's own
// tree, so a product DSL bundle -- the primary delivery path under platform
// consolidation -- ran neither.

import (
	"fmt"
	"strings"
)

// construct is one row-accessing declaration located in a file's source.
type construct struct {
	Kind     string // query / mutate / seed
	Name     string
	Line     int // 1-based line of the header
	Preamble string
	Body     string
}

// forEachConstruct walks every `query` / `mutate` / `seed` declaration in src
// and calls fn with its header line, annotation preamble and body.
//
// The preamble walk goes UPWARD from the header over contiguous `@`- and
// `//`-prefixed lines, stopping at the first blank or other line, which is
// where a construct's own annotations live and where a preceding construct's
// do not.
func forEachConstruct(src string, fn func(construct)) {
	for _, m := range ConstructHeaderRe.FindAllStringSubmatchIndex(src, -1) {
		openIdx := m[1] - 1
		closeIdx := MatchingClose(src, openIdx)
		if closeIdx < 0 {
			continue
		}
		preambleStart := m[0]
		for k := m[0] - 1; k >= 0; k-- {
			lineStart := strings.LastIndexByte(src[:k], '\n') + 1
			line := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
			if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
				preambleStart = lineStart
				k = lineStart - 1
				continue
			}
			if line == "" {
				break
			}
			break
		}
		fn(construct{
			Kind:     src[m[2]:m[3]],
			Name:     src[m[4]:m[5]],
			Line:     strings.Count(src[:m[0]], "\n") + 1,
			Preamble: src[preambleStart:m[0]],
			Body:     src[m[1]:closeIdx],
		})
	}
}

// Bucket is one construct's per-row authorization classification, as the audit
// doc defines them (docs/public/operate/auth/per-row-authz-audit.md). Only
// BucketFlagged is a failure; the rest are reported for the audit table.
//
// `granted` IS a bucket in the doc but has no counter here -- it would need
// relationship-spec resolution, so a granted construct lands in BucketOwned or
// BucketOther (memql#2983).
type Bucket string

const (
	// BucketServerOnly -- carries @serverOnly; not client-callable, so no
	// caller check applies. Checked FIRST.
	BucketServerOnly Bucket = "serverOnly"
	// BucketPublic -- carries @public; the author has acknowledged the read.
	BucketPublic Bucket = "public"
	// BucketOwned -- the filter's structure guarantees caller ownership, or a
	// write stamps the owner from actor.userId.
	BucketOwned Bucket = "owned"
	// BucketAdmin -- an admin gate holds on every path through the filter.
	BucketAdmin Bucket = "admin"
	// BucketFlagged -- none of the above AND the construct selects rows by a
	// user-scope column. The only failing bucket.
	BucketFlagged Bucket = "flagged"
	// BucketOther -- none of the above and no user-scope selection (concept
	// catalogs, cluster topology, system metadata reads).
	BucketOther Bucket = "other"
)

// Classification is one construct's bucket, for the audit table.
type Classification struct {
	Kind   string
	Name   string
	Line   int
	Bucket Bucket
}

// ClassifySource classifies every row-accessing construct in one file.
//
// The audit table and the gate read the SAME function, so a construct the table
// reports as `owned` cannot be one the gate flags, and vice versa. They were
// two switches over the same variables, which is how the table came to report a
// correctly admin-gated identity query as ungated (memql#2983).
func ClassifySource(path, src string, opts Options) []Classification {
	var out []Classification
	forEachConstruct(src, func(c construct) {
		bucket, _ := classifyConstruct(path, c, opts)
		out = append(out, Classification{Kind: c.Kind, Name: c.Name, Line: c.Line, Bucket: bucket})
	})
	return out
}

// scanConstructAuthz reports the two authorization violations for every
// construct in one file.
func scanConstructAuthz(path, src string, opts Options) []Violation {
	var out []Violation
	forEachConstruct(src, func(c construct) {
		bucket, clause := classifyConstruct(path, c, opts)
		if v, ok := adminCompositionViolation(path, c, clause); ok {
			out = append(out, v)
		}
		if bucket != BucketFlagged {
			return
		}
		out = append(out, Violation{
			Gate:      GateUserScopeSelection,
			File:      path,
			Line:      c.Line,
			Kind:      c.Kind,
			Construct: c.Name,
			Detail: "selects rows by a user-scope column with no caller check, no admin gate, and no @public or @serverOnly annotation. " +
				"Resolve by (1) adding a caller-scope filter (`args.X == actor.userId`), (2) naming an admin gate as a top-level " +
				"affirmative conjunct (`actor.isClusterOwner==true`, `requiresAdmin`, `requiresOwnerOrAdmin`), (3) annotating @public " +
				"with a comment explaining why no caller check applies, or (4) annotating @serverOnly if no client may call it. " +
				"See docs/public/operate/auth/per-row-authz-audit.md",
		})
	})
	return out
}

// classifyConstruct places one construct in its authorization bucket and
// returns the filter clause it read (empty for a non-query), so the caller can
// reuse it without extracting it twice.
func classifyConstruct(path string, c construct, opts Options) (Bucket, string) {
	// A QUERY's scoping lives in its filter's boolean STRUCTURE, so the
	// classification evaluates the clause rather than substring-matching the
	// body (memql#2832). A substring test reads
	// `ownerUserId==actor.userId || visibility=="public"` as owned, even
	// though the right arm returns rows the caller does not own -- and since
	// `owned` is checked before the hard-failing bucket, one permissive
	// disjunct made the gate unreachable while reporting that authz had been
	// classified.
	//
	// A MUTATION is not a filter: `actor.userId` there appears as a stamped
	// VALUE (`ownerUserId: actor.userId`), which is exactly what makes the
	// write caller-scoped, so the substring test is the right question for
	// that kind.
	isQuery := c.Kind == "query"
	clause := ""
	if isQuery {
		clause = FilterClauseOf(c.Body)
	}

	// @serverOnly resolves the same question @public does, from the other
	// direction (memql#2800): not callable by a client at all, so a
	// caller-scope filter is neither present nor meaningful. Unlike @public it
	// is ENFORCED at every dispatch point against auth.CallOrigin, so accepting
	// it here rests on a runtime guarantee rather than on a promise.
	if opts.serverOnly(path, c.Name) {
		return BucketServerOnly, clause
	}
	if strings.Contains(c.Preamble, "@public") {
		return BucketPublic, clause
	}

	// Presence test over the SAME leaf vocabulary AdminGateLeaf uses, so the
	// classification and the composition gate cannot drift about what counts
	// as a gate term. They had: this was a pair of hand-inlined Contains checks
	// naming `actor.isClusterOwner` and `requiresClusterOwner` -- the latter a
	// spec declared nowhere in the tree -- so a construct gated by
	// `requiresOwnerOrAdmin` satisfied the composition gate and was still
	// reported ungated here.
	hasAdmin := AdminGateMentionRe.MatchString(c.Body)
	hasOwner := strings.Contains(c.Body, "actor.userId")
	if isQuery {
		// An empty filter guarantees nothing, so a query with no clause can
		// never classify as owned/admin on this path.
		hasAdmin = hasAdmin && ClauseGuarantees(clause, AdminGateLeaf)
		hasOwner = hasOwner && ClauseGuarantees(clause, OwnerScopeLeaf)
	}
	switch {
	case hasOwner:
		return BucketOwned, clause
	case hasAdmin:
		return BucketAdmin, clause
	}

	if !UserScopeFieldRe.MatchString(RowSelectionSurface(c.Body)) {
		return BucketOther, clause
	}
	if _, exempt := UserScopeSelectionExemptions[path+" "+c.Name]; exempt {
		return BucketOther, clause
	}
	return BucketFlagged, clause
}

// adminCompositionViolation reports a filter whose admin gate would not zero
// the result set when the gate is false (memql#2839):
//
//	filter  fromE164==args.e164 && actor.isClusterOwner==true   // gate holds
//	filter  fromE164==args.e164 || actor.isClusterOwner==true   // gate is OFF
//
// On the second, a false gate zeroes nothing -- the left term alone admits
// rows, so the construct reads as gated while serving any caller who supplies
// `fromE164`. That is a fail-open COMPOSITION of a correct primitive, and it is
// invisible to everything else: the term is well-formed, the field resolves,
// and the engine tests assert the leaf's value rather than its placement.
//
// Selection is polarity-BLIND (any mention) and the assertion is strict, which
// is what turns an inverted spelling (`!=true`, `==false`) into an error rather
// than a silence -- see AdminGateMentionRe.
func adminCompositionViolation(path string, c construct, clause string) (Violation, bool) {
	if clause == "" || !MentionsAdminGate(clause) {
		return Violation{}, false
	}
	if ClauseGuarantees(clause, AdminGateLeaf) {
		return Violation{}, false
	}
	return Violation{
		Gate:      GateAdminGateComposition,
		File:      path,
		Line:      c.Line,
		Kind:      c.Kind,
		Construct: c.Name,
		Detail: fmt.Sprintf("the admin gate does not hold on every path through its filter -- a false gate would NOT zero the result set.\n    filter  %s\n"+
			"    The gate must be a top-level conjunct in the affirmative form (`<predicate> && actor.isClusterOwner==true`). "+
			"A gate inside a disjunction is switched off by the other arm; an inverted one (`!=true`, `==false`) admits exactly the callers it should refuse.",
			clause),
	}, true
}
