package memql

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestRowAuthzShadowReport is the measurement #2921 exists to produce.
//
// It runs the same pure analyzer the runtime hook runs, over every
// query construct in the loaded tree, and prints the table that
// #2803's Phase 3 enforcement ruling is taken against. It asserts
// almost nothing about the numbers on purpose -- the numbers are data,
// not a contract, and a gate that hard-fails on them would have to be
// edited every time an author writes a query. What it DOES assert is
// that the measurement is honest: every verdict is one of the three,
// and every non-implied verdict carries a reason.
//
// Run it directly to regenerate the report:
//
//	go test ./component/memql/ -run TestRowAuthzShadowReport -v
func TestRowAuthzShadowReport(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	type row struct {
		construct string
		concept   string
		tier      string
		predicate string
		verdict   ShadowVerdict
		reason    string
	}

	var (
		measured   []row
		undeclared []string // constructs whose concept declares no tier
		unbound    int      // query constructs with no bound concept
		queries    int
	)

	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		queries++
		if strings.TrimSpace(fn.BoundConcept) == "" {
			unbound++
			continue
		}
		concept, err := memoryNodes.Get(fn.BoundConcept)
		if err != nil || concept == nil {
			unbound++
			continue
		}
		if concept.RowAuthz == nil {
			undeclared = append(undeclared, fmt.Sprintf("%s (%s)", fn.Name, fn.BoundConcept))
			continue
		}
		verdict, reason := AnalyzeShadow(fn.Expr, concept.RowAuthz)
		measured = append(measured, row{
			construct: fn.Name,
			concept:   fn.BoundConcept,
			tier:      string(concept.RowAuthz.Tier),
			predicate: InjectedPredicate(concept.RowAuthz),
			verdict:   verdict,
			reason:    reason,
		})
	}

	sort.Slice(measured, func(i, j int) bool {
		if measured[i].concept != measured[j].concept {
			return measured[i].concept < measured[j].concept
		}
		return measured[i].construct < measured[j].construct
	})
	sort.Strings(undeclared)

	counts := map[ShadowVerdict]int{}
	for _, r := range measured {
		counts[r.verdict]++
	}

	// --- the report ---

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== rowAuthz shadow-mode measurement (memql#2921) ===\n\n")
	fmt.Fprintf(&b, "query constructs in the tree      %d\n", queries)
	fmt.Fprintf(&b, "  measured (concept declares)     %d\n", len(measured))
	fmt.Fprintf(&b, "  not measurable (undeclared)     %d\n", len(undeclared))
	fmt.Fprintf(&b, "  no resolvable bound concept     %d\n\n", unbound)
	fmt.Fprintf(&b, "verdicts over the measured set:\n")
	for _, v := range []ShadowVerdict{ShadowAlreadyImplied, ShadowWouldNarrow, ShadowUndecidable} {
		fmt.Fprintf(&b, "  %-16s %d\n", v, counts[v])
	}

	// The would-narrow list is the actual deliverable. Each entry is
	// either a latent authorization bug being caught or a legitimate
	// admin-tier read needing an explicit declaration, and that
	// distinction cannot be inferred from the count.
	fmt.Fprintf(&b, "\n--- WOULD-NARROW (the blast radius of enforcement) ---\n")
	found := false
	for _, r := range measured {
		if r.verdict != ShadowWouldNarrow {
			continue
		}
		found = true
		fmt.Fprintf(&b, "  %s\n      concept   %s (%s)\n      injects   %s\n      because   %s\n",
			r.construct, r.concept, r.tier, r.predicate, r.reason)
	}
	if !found {
		fmt.Fprintf(&b, "  (none)\n")
	}

	fmt.Fprintf(&b, "\n--- UNDECIDABLE (enforcement would change these blindly) ---\n")
	found = false
	for _, r := range measured {
		if r.verdict != ShadowUndecidable {
			continue
		}
		found = true
		fmt.Fprintf(&b, "  %s\n      concept   %s (%s)\n      because   %s\n",
			r.construct, r.concept, r.tier, r.reason)
	}
	if !found {
		fmt.Fprintf(&b, "  (none)\n")
	}

	fmt.Fprintf(&b, "\n--- ALREADY-IMPLIED (enforcement is a no-op for these) ---\n")
	for _, r := range measured {
		if r.verdict == ShadowAlreadyImplied {
			fmt.Fprintf(&b, "  %-42s %s\n", r.construct, r.concept)
		}
	}

	// Coverage, stated rather than implied. Phase 1 declared 13 of 100
	// concepts, so most of the tree is NOT measurable here -- and a
	// report that showed only the measured set would read as full
	// coverage of a population it never touched.
	fmt.Fprintf(&b, "\n--- NOT MEASURABLE: concept declares no tier (%d constructs) ---\n", len(undeclared))
	fmt.Fprintf(&b, "  Shadow mode computes a predicate only where a tier is declared.\n")
	fmt.Fprintf(&b, "  These are not 'no change' -- they are 'not measured'.\n")
	for _, u := range undeclared {
		fmt.Fprintf(&b, "  %s\n", u)
	}

	// --- graph expansion, the path that is NOT tautological ---
	//
	// #2803 design decision 3: expandGraph traverses relationships from
	// a result row and never reaches a filter, so a measurement covering
	// only named queries reports a clean bill of health it has not
	// earned. Every declared concept reachable as a relationship target
	// is reachable with NO filter at all.
	fmt.Fprintf(&b, "\n--- GRAPH EXPANSION (traversal reaches these with no filter) ---\n")
	traversable := traversableDeclaredConcepts()
	if len(traversable) == 0 {
		fmt.Fprintf(&b, "  No DECLARED concept is a relationship target, so traversal currently\n")
		fmt.Fprintf(&b, "  reaches nothing measurable. That is not reassurance -- it is the same\n")
		fmt.Fprintf(&b, "  finding from another angle. The concepts expansion actually walks into\n")
		fmt.Fprintf(&b, "  are the most-referenced ones, and every one of them is undeclared:\n")
		for _, tc := range topRelationshipTargets(8) {
			state := "UNDECLARED"
			if c, err := memoryNodes.Get(tc.name); err == nil && c != nil && c.RowAuthz != nil {
				state = "declares " + string(c.RowAuthz.Tier)
			}
			fmt.Fprintf(&b, "    %-34s %3d inbound relationship(s)  %s\n", tc.name, tc.count, state)
		}
	}
	for _, name := range traversable {
		c, err := memoryNodes.Get(name)
		if err != nil || c == nil || c.RowAuthz == nil {
			continue
		}
		verdict, reason := AnalyzeShadow(nil, c.RowAuthz)
		fmt.Fprintf(&b, "  %-34s %-16s %s\n", name, verdict, reason)
	}

	// --- the blast radius the declared set cannot produce ---
	//
	// The measured set above is tautological (see the note at the end),
	// so on its own this phase hands back no blast radius. Rather than
	// report that as "unknown", hypothesise a tier for each UNDECLARED
	// concept from its own queries and measure against that. Nothing is
	// written to the tree -- these are hypotheses used to compute a
	// number, which is exactly what shadow mode is for.
	fmt.Fprintf(&b, "\n=== HYPOTHETICAL TIERS: the blast radius over the undeclared set ===\n\n")
	fmt.Fprintf(&b, "Not declarations. A tier is guessed per concept from the most common\n")
	fmt.Fprintf(&b, "caller-scope term among its own queries, purely so enforcement's cost\n")
	fmt.Fprintf(&b, "can be counted before anyone rules on it.\n\n")

	hypoCounts := map[ShadowVerdict]int{}
	type hypoRow struct{ construct, concept, tier, predicate, reason string }
	var hypoNarrow []hypoRow
	hypoConcepts := 0

	for _, conceptName := range undeclaredConceptsWithQueries(registry) {
		hypo := hypothesiseTier(registry, conceptName)
		if hypo == nil {
			continue
		}
		hypoConcepts++
		for _, fn := range registry.List() {
			if fn == nil || fn.FunctionKind != "query" || fn.BoundConcept != conceptName {
				continue
			}
			verdict, reason := AnalyzeShadow(fn.Expr, hypo)
			hypoCounts[verdict]++
			if verdict == ShadowWouldNarrow {
				hypoNarrow = append(hypoNarrow, hypoRow{fn.Name, conceptName, string(hypo.Tier), InjectedPredicate(hypo), reason})
			}
		}
	}

	fmt.Fprintf(&b, "concepts with a hypothesised tier   %d\n", hypoConcepts)
	for _, v := range []ShadowVerdict{ShadowAlreadyImplied, ShadowWouldNarrow, ShadowUndecidable} {
		fmt.Fprintf(&b, "  %-16s %d\n", v, hypoCounts[v])
	}
	sort.Slice(hypoNarrow, func(i, j int) bool {
		if hypoNarrow[i].concept != hypoNarrow[j].concept {
			return hypoNarrow[i].concept < hypoNarrow[j].concept
		}
		return hypoNarrow[i].construct < hypoNarrow[j].construct
	})
	fmt.Fprintf(&b, "\n--- WOULD-NARROW under a hypothesised tier (%d constructs) ---\n", len(hypoNarrow))
	fmt.Fprintf(&b, "Each is either a latent authorization bug or a legitimate broader read\n")
	fmt.Fprintf(&b, "that needs an explicit declaration. Which one CANNOT be read off the count.\n")
	fmt.Fprintf(&b, "Entries marked [@public] carry an explicit unscoped-by-intent annotation --\n")
	fmt.Fprintf(&b, "Phase 1's codemod treats that as a hard blocker, so they are the ones most\n")
	fmt.Fprintf(&b, "likely to be legitimate rather than defective.\n")
	for _, r := range hypoNarrow {
		// The PREDICATE, not just the tier. A would-narrow list offered
		// as evidence for an authorization ruling that omits what it
		// measured against is not checkable by the person ruling.
		marker := ""
		if isPublicConstruct(r.construct) {
			marker = "  [@public]"
		}
		fmt.Fprintf(&b, "  %s%s\n      concept   %s (%s)\n      injects   %s\n",
			r.construct, marker, r.concept, r.tier, r.predicate)
	}

	declared, total := declaredConceptCounts()
	fmt.Fprintf(&b, "\nconcepts declaring a tier: %d of %d\n", declared, total)

	fmt.Fprintf(&b, `
--- READ THIS BEFORE RULING ON PHASE 3 ---

The filter-path result above is TAUTOLOGICAL, and saying so is the
honest reading of it. Phase 1's codemod declared a tier for a concept
only when EVERY query over that concept already carried the tier's term
as a top-level conjunct. This phase asks whether each query carries the
tier's term as a top-level conjunct. Same property, so the measured set
can only come back already-implied, and it does.

That is not a wasted run -- it is a CROSS-VALIDATION. Phase 1 decided
textually, on blanked source, before load; this analyzer decides
structurally, on the parsed AST, after load. Two independent
implementations agreeing on all %d constructs is real evidence they
encode the same rule. A would-narrow here would have meant they
disagree, which is a defect in one of them rather than a finding about
authorization.

What the DECLARED set does not give you is a blast radius: over those
concepts this analyzer agrees with Phase 1 by construction, so the
answer there is tautological. Enforcement's blast radius lives in the
%d constructs over the %d concepts that declare nothing, where no
predicate can be computed from a tier nobody stated -- which is exactly
what the HYPOTHETICAL TIERS section above estimates.

Read the two together. This paragraph used to open "what it does NOT
give you is a blast radius", flatly, while a section printed ~77 lines
earlier in this same report was headed "the blast radius over the
undeclared set" (memql#2984). The report does produce one; it is an
estimate over hypothetical tiers rather than a measurement over
declared ones, and that distinction is the whole of it.

So the Phase 3 question is not "how many constructs would narrow" but
"how do the undeclared concepts get a tier", and neither this issue nor
#2920 answers that. Options worth putting to the owner: declare the
remainder by hand (87 concepts, expensive but exact), relax Phase 1's
inference and re-measure (cheap, and this analyzer would then produce a
real blast radius), or enforce only over declared concepts (safe, but
leaves the recurrence mechanism #2799 identified fully open).

The graph-expansion section is the one non-tautological result: those
verdicts hold regardless of what any filter says, because traversal has
no filter.
`, len(measured), len(undeclared), total-declared)

	t.Log(b.String())

	// --- what this test actually enforces ---

	for _, r := range measured {
		switch r.verdict {
		case ShadowAlreadyImplied, ShadowWouldNarrow, ShadowUndecidable:
		default:
			t.Errorf("%s: verdict %q is not one of the three", r.construct, r.verdict)
		}
		if r.verdict != ShadowAlreadyImplied && strings.TrimSpace(r.reason) == "" {
			t.Errorf("%s: verdict %q carries no reason; the list is what a ruling is taken against",
				r.construct, r.verdict)
		}
	}
	if len(measured) == 0 {
		t.Error("measured nothing -- either the tree failed to load or Phase 1's declarations are gone")
	}

	// THE CROSS-VALIDATION, asserted rather than merely printed.
	//
	// Phase 1 declared a tier only where every query over the concept
	// already carried the tier's term as a top-level conjunct. This
	// analyzer tests the same property against the loaded AST rather
	// than the source text. So over the DECLARED set, every construct
	// must come back already-implied -- and a would-narrow means the
	// two implementations disagree about what the term is, which is a
	// defect in one of them, not a finding about authorization.
	//
	// This is also the guard that would have caught the first run of
	// this measurement, which reported 33 of 33 would-narrow because
	// the analyzer was reading the shape/paginate/sort wrapper instead
	// of the filter underneath.
	for _, r := range measured {
		if r.verdict == ShadowWouldNarrow {
			t.Errorf("%s (%s, tier %s) reports would-narrow, but Phase 1 only declared this concept "+
				"because every query over it already carried %q as a top-level conjunct.\n"+
				"Two causes are possible and they need different fixes:\n"+
				"  (a) a NEW query was added over an already-declared concept. If it carries "+
				"@serverOnly, the codemod exempts it and this analyzer does not know about the "+
				"annotation -- teach the analyzer, or re-run the codemod, which would now decline "+
				"to declare the concept at all.\n"+
				"  (b) the textual inference and this AST analyzer genuinely disagree about what "+
				"the term is, which is a defect in one of them.\n"+
				"Either way this is NOT a blast-radius finding. Reason given: %s",
				r.construct, r.concept, r.tier, r.predicate, r.reason)
		}
	}
}

// traversableDeclaredConcepts returns every concept that declares a
// tier AND is named as a relationship target by some concept, sorted.
// Those are the ones graph expansion can walk into.
func traversableDeclaredConcepts() []string {
	targets := map[string]bool{}
	for _, c := range memoryNodes.All() {
		if c == nil {
			continue
		}
		for _, rel := range c.Relationships {
			if t := strings.TrimSpace(rel.TargetConcept); t != "" {
				targets[t] = true
			}
		}
	}
	var out []string
	for name := range targets {
		c, err := memoryNodes.Get(name)
		if err != nil || c == nil || c.RowAuthz == nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// declaredConceptCounts reports how many loaded concepts carry a tier.
func declaredConceptCounts() (declared, total int) {
	for _, c := range memoryNodes.All() {
		total++
		if c != nil && c.RowAuthz != nil {
			declared++
		}
	}
	return declared, total
}

// Shadow mode must not change a result. This asserts the strongest
// cheap form of that: the analyzer is pure (it cannot reach the
// executor), and the hook's only effect is to append to a collector.
func TestShadowModeChangesNoExpression(t *testing.T) {
	expr := and(ownerScoped("ownerUserId"), fieldCmp("status", OpEq, "active"))
	before := fmt.Sprintf("%#v", expr)

	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if _, _, err := analyzeTwice(expr, decl); err != nil {
		t.Fatal(err)
	}

	if after := fmt.Sprintf("%#v", expr); after != before {
		t.Fatalf("the analyzer mutated the expression it was given:\n before %s\n after  %s", before, after)
	}
}

func analyzeTwice(expr ExpressionNode, decl *langparser.RowAuthzDecl) (ShadowVerdict, ShadowVerdict, error) {
	a, _ := AnalyzeShadow(expr, decl)
	b, _ := AnalyzeShadow(expr, decl)
	if a != b {
		return a, b, fmt.Errorf("the analyzer is not deterministic: %q then %q", a, b)
	}
	return a, b, nil
}

// targetCount is one concept and how many relationships point at it.
type targetCount struct {
	name  string
	count int
}

// topRelationshipTargets returns the n most-referenced relationship
// targets, descending. These are the concepts graph expansion actually
// walks into.
func topRelationshipTargets(n int) []targetCount {
	counts := map[string]int{}
	for _, c := range memoryNodes.All() {
		if c == nil {
			continue
		}
		for _, rel := range c.Relationships {
			if t := strings.TrimSpace(rel.TargetConcept); t != "" {
				counts[t]++
			}
		}
	}
	out := make([]targetCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, targetCount{name: name, count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// undeclaredConceptsWithQueries returns every concept that has at least
// one query bound to it and declares no tier, sorted.
func undeclaredConceptsWithQueries(registry *FunctionRegistry) []string {
	seen := map[string]bool{}
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" || strings.TrimSpace(fn.BoundConcept) == "" {
			continue
		}
		c, err := memoryNodes.Get(fn.BoundConcept)
		if err != nil || c == nil || c.RowAuthz != nil {
			continue
		}
		seen[fn.BoundConcept] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hypothesiseTier guesses what a concept WOULD declare, from the
// caller-scope terms its own queries already carry.
//
// This is not the Phase 1 codemod's rule and must not be confused with
// it. The codemod refuses to declare unless every query clears the
// floor, precisely so a guess never becomes a declaration. This
// function guesses ON PURPOSE, because the whole point is to count what
// enforcement would cost -- and a hypothesis used to compute a number,
// never written to the tree, cannot mislead a future author the way a
// wrong declaration would.
//
// Returns nil when no query over the concept carries any caller-scope
// term, since there is then nothing to hypothesise from.
func hypothesiseTier(registry *FunctionRegistry, conceptName string) *langparser.RowAuthzDecl {
	ownerVotes := map[string]int{}
	adminVotes := 0

	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" || fn.BoundConcept != conceptName {
			continue
		}
		for _, c := range flattenForHypothesis(fn.Expr) {
			cmp, ok := c.(*ComparisonExpression)
			if !ok || cmp.Operator != OpEq {
				continue
			}
			if ref, isActor := cmp.Value.(*ActorReference); isActor &&
				strings.EqualFold(strings.TrimSpace(ref.Path), "userId") {
				// Only a TOP-LEVEL payload property is a candidate owner
				// field. A `row.id==actor.userId` term votes for nothing:
				// `id` is a row intrinsic, and hypothesising the bare
				// spelling would propose a predicate that compiles to a
				// JSONB lookup rather than the id column (memql#2779).
				if f := topLevelPayloadField(cmp.Field); f != "" && !isRowIntrinsicName(cmp.Field) {
					ownerVotes[f]++
				}
			}
			if isClusterOwnerLeaf(cmp) {
				adminVotes++
			}
		}
	}

	// The most-nominated owner field wins; ties break alphabetically so
	// the report is reproducible.
	best, bestN := "", 0
	fields := make([]string, 0, len(ownerVotes))
	for f := range ownerVotes {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		if ownerVotes[f] > bestN {
			best, bestN = f, ownerVotes[f]
		}
	}
	switch {
	case bestN > 0:
		return &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: best}
	case adminVotes > 0:
		return &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}
	default:
		return nil
	}
}

// flattenForHypothesis unwraps the read pipeline and flattens the AND
// chain, tolerating disjunctions (a term inside one is still evidence
// of what the author considers this concept's owner field, even though
// it does not IMPLY the predicate).
func flattenForHypothesis(expr ExpressionNode) []ExpressionNode {
	expr = unwrapToFilter(expr)
	if expr == nil {
		return nil
	}
	if logical, ok := expr.(*LogicalExpression); ok {
		return append(flattenForHypothesis(logical.Left), flattenForHypothesis(logical.Right)...)
	}
	return []ExpressionNode{expr}
}

// isPublicConstruct reports whether a construct carries `@public` in
// the authored tree.
//
// Read from source rather than from the loaded Function, because
// `@public` has no field on it -- the annotation is parsed and then
// consulted by nothing at runtime, which is itself the observation
// #2918 is open on. Line-anchored so a comment mentioning the
// annotation is not mistaken for it (memql#2875).
func isPublicConstruct(name string) bool {
	paths, err := filepath.Glob(filepath.Join("..", "..", "dsl", "*", "queries.memql"))
	if err != nil {
		return false
	}
	header := regexp.MustCompile(`(?m)^[ \t]*query[ \t]+[\p{L}_][\p{L}\p{Nd}_]*[ \t]+` + regexp.QuoteMeta(name) + `[ \t]*\{`)
	public := regexp.MustCompile(`(?m)^[ \t]*@public\b`)
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		src := string(raw)
		loc := header.FindStringIndex(src)
		if loc == nil {
			continue
		}
		// Scan back over the contiguous annotation/comment preamble.
		start := loc[0]
		for start > 0 {
			prevEnd := start - 1
			ps := strings.LastIndexByte(src[:prevEnd], '\n') + 1
			trimmed := strings.TrimSpace(src[ps:prevEnd])
			if trimmed == "" || (!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//")) {
				break
			}
			start = ps
		}
		return public.MatchString(src[start:loc[0]])
	}
	return false
}
