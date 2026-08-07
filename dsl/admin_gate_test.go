package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// ownerScopeLeaf and adminGateLeaf name the leaf predicates the authz gates
// recognise. They live here rather than inline so TestPerRowAuthzClassification
// and TestAdminGateIsATopLevelConjunct cannot drift about what counts as a
// caller check -- the drift this file's sibling gates keep being filed for.
func ownerScopeLeaf(pred string) bool { return strings.Contains(pred, "actor.userId") }

// adminGateRe matches a predicate that establishes admin-ness WHEN TRUE.
//
// POLARITY is load-bearing, and a bare strings.Contains does not carry it: the
// composition rule asks whether a FALSE gate zeroes the row set, so the leaf
// must be a term that is false for a non-admin. `actor.isClusterOwner!=true`
// and `==false` contain the same identifier and inverT the meaning -- under
// them a non-owner who satisfies the other conjunct gets rows and the cluster
// owner gets none, which is the very failure this gate exists to refuse.
// stripLeadingNot deliberately leaves `!=` alone (it is a comparison, not a
// negation), so nothing upstream catches it either.
//
// Word boundaries matter for the same reason: `requiresClusterOwnerXyz` is a
// different identifier, and a substring test accepted it as the gate.
//
// The spec alternatives are ordered longest-first so `requiresOwnerOrAdmin`
// cannot be consumed as `requiresOwner`.
//
// Which of these names is DECLARED, and which the corpus actually uses, is no
// longer written here (memql#3016). Both statements went stale: this comment
// and its twin in the table below claimed "none is currently used in a filter"
// long after `requiresOwnerOrAdmin` was gating live identity queries. A stale
// claim in the file that defines the vocabulary is how the classifier drifted
// from it in the first place.
//
// Both facts are now COMPUTED, by TestAdminGateNamesAreDeclaredOrRecorded
// below, which reads the tree instead of describing it.
//
// That covers ONE direction: every name in the recogniser is declared or
// recorded. The converse -- every declared caller-scope spec appears in the
// recogniser -- is TestEveryDeclaredActorGateIsRecognised, added in the
// memql#3071 review after `requiresDeveloperOrAbove` was found declared at
// dsl/deployment/specs.memql and present in neither pattern. A gate the
// recogniser does not know is not a gate: adminGateLeaf returns false for it,
// the composition rule never runs on a filter that uses it, and the per-row
// authz classifier does not count it as caller-scoped. It is the same defect
// as the requiresClusterOwner case this file already records, pointing the
// other way, which is why one direction alone could not be "nothing left that
// can go stale".
//
// That converse check paid for itself immediately. Besides
// requiresDeveloperOrAbove it found `forgeDeveloper` and `forgeApprover`
// (dsl/forge/specs.memql), both of which are used as LIVE filter conjuncts --
// dsl/forge/queries.memql's `status == "needs_validation" && forgeDeveloper`
// and `status == "needs_approval" && forgeApprover`. Two authorization gates
// in production filters that the composition rule had never once run on. They
// are correctly written, top-level and affirmative -- but that was luck rather
// than a checked property, which is the whole distinction this file exists to
// make.
var adminGateRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner[ \t]*==[ \t]*true|requiresDeveloperOrAbove|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

func adminGateLeaf(pred string) bool { return adminGateRe.MatchString(pred) }

// adminGateMentionRe is the POLARITY-BLIND twin, and the two must stay
// separate: selection has to be broad and assertion strict.
//
// If the gate selected constructs with the strict predicate, an inverted
// filter (`actor.isClusterOwner!=true`) would simply not be recognised as
// admin-gated, get skipped, and sail through -- swapping one fail-open for
// another. Selecting on any MENTION and then demanding the strict form is what
// turns the inverted spelling into an error instead of a silence.
var adminGateMentionRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner|requiresDeveloperOrAbove|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner|forgeApprover|forgeDeveloper)([^A-Za-z0-9_]|$)`)

func mentionsAdminGate(clause string) bool { return adminGateMentionRe.MatchString(clause) }

// TestAdminGateIsATopLevelConjunct hard-fails when an admin-gated filter
// composes its gate so a FALSE gate cannot zero the result set (memql#2839).
//
// The engine primitive is correct: a request with no AccessContext resolves
// `actor.isClusterOwner` to false (#2801/#2811), and the SQL bind and the
// in-process post-filter agree on it (#2822). Whether that false leaf actually
// empties the row set is a property of how the AUTHOR composed it, and nothing
// checked that:
//
//	filter  fromE164==args.e164 && actor.isClusterOwner==true   // gate holds
//	filter  fromE164==args.e164 || actor.isClusterOwner==true   // gate is OFF
//
// On the second, a false gate zeroes nothing -- the left term alone admits
// rows, so the construct reads as gated while serving any caller who supplies
// `fromE164`. `tryCompileCombinedFilter`'s LogicalOr branch emits
// `(left OR right)` and the database returns the left side.
//
// That is a fail-open COMPOSITION of a correct primitive, and it was invisible
// to everything: the term is well-formed, the field resolves, and the engine
// tests assert the leaf's value rather than its placement.
//
// This is the hard-failing half of #2832. That change stopped such a filter
// being MISLABELLED `admin` in the classification table; it could not fail the
// build, because reaching the `flagged` bucket additionally requires a
// user-scope field and an admin filter need not have one. Same predicate,
// different job: classify there, refuse here.
func TestAdminGateIsATopLevelConjunct(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	checked := 0
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)

		for _, m := range constructHeaderRe.FindAllStringSubmatchIndex(src, -1) {
			closeIdx := matchingClose(src, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := src[m[1]:closeIdx]
			name := src[m[4]:m[5]]

			// filterClauseOf strips comments, so the prose in
			// dsl/authoring/queries.memql and dsl/identity/queries.memql that
			// merely NAMES the gate is excluded, as is every `actor.` read in
			// a logic body -- neither is a filter.
			clause := filterClauseOf(body)
			if clause == "" || !mentionsAdminGate(clause) {
				continue
			}
			checked++
			if !clauseGuarantees(clause, adminGateLeaf) {
				lineNo := strings.Count(src[:m[0]], "\n") + 1
				t.Errorf("%s:%d  %s: the admin gate does not hold on every path through its filter -- a false gate would NOT zero the result set.\n    filter  %s\n    The gate must be a top-level conjunct in the affirmative form (`<predicate> && actor.isClusterOwner==true`). A gate inside a disjunction is switched off by the other arm; an inverted one (`!=true`, `==false`) admits exactly the callers it should refuse.",
					p, lineNo, name, clause)
			}
		}
	}

	// A corpus that stopped producing admin-gated filters, or an extractor
	// that stopped finding them, would leave this test green while protecting
	// nothing.
	if checked == 0 {
		t.Fatal("no admin-gated filter clauses found; the corpus shape or filterClauseOf changed and this gate has silently stopped protecting anything")
	}
	t.Logf("checked %d admin-gated filter clause(s)", checked)
}

// TestAdminGateCompositionRules pins the rule itself, independently of the
// corpus -- which today happens to satisfy it, so the corpus alone cannot
// prove the gate works.
func TestAdminGateCompositionRules(t *testing.T) {
	cases := []struct {
		name   string
		clause string
		want   bool
	}{
		{"bare gate", `actor.isClusterOwner==true`, true},
		{"gate as a conjunct", `partitionId==args.partitionId && actor.isClusterOwner==true`, true},
		{"gate first", `actor.isClusterOwner==true && statusIsActive`, true},
		// The shipped telephony shape: a disjunction is fine as long as the
		// gate sits OUTSIDE it.
		{"disjunction inside, gate outside", `(fromE164==args.e164 || toE164==args.e164) && actor.isClusterOwner==true`, true},
		{"spec form", `requiresClusterOwner && statusIsActive`, true},

		// The defect: the gate is switched off by the other arm.
		{"gate as a disjunct", `fromE164==args.e164 || actor.isClusterOwner==true`, false},
		{"gate as a disjunct, first", `actor.isClusterOwner==true || fromE164==args.e164`, false},
		{"parens dropped", `fromE164==args.e164 || toE164==args.e164 && actor.isClusterOwner==true`, false},
		{"spec form as a disjunct", `requiresClusterOwner || statusIsActive`, false},
		{"negated gate", `!(actor.isClusterOwner==true)`, false},
		{"gate behind a when guard", `when(args.adminMode) { actor.isClusterOwner==true }`, false},

		// Review round 1: POLARITY. These contain the gate identifier and a
		// top-level `&&`, so a substring leaf accepted them -- while inverting
		// the meaning. Under `!=true` a non-owner satisfying the other conjunct
		// gets rows and the cluster owner gets none.
		{"inverted with !=", `fromE164==args.e164 && actor.isClusterOwner!=true`, false},
		{"inverted with ==false", `fromE164==args.e164 && actor.isClusterOwner==false`, false},
		{"bare inverted", `actor.isClusterOwner!=true`, false},

		// Word boundaries: a different identifier is not the gate.
		{"identifier containing the spec name", `x==args.x && requiresClusterOwnerXyz`, false},
		{"identifier prefixed by the spec name", `x==args.x && myRequiresAdmin`, false},

		// The live context-spec gates. Which of them the corpus uses is
		// computed by TestAdminGateNamesAreDeclaredOrRecorded rather than
		// claimed here -- the claim that used to sit on this line said "none
		// of which the corpus filters use yet" and was false (memql#3016).
		{"requiresAdmin as a conjunct", `statusIsActive && requiresAdmin`, true},
		{"requiresOwnerOrAdmin as a conjunct", `statusIsActive && requiresOwnerOrAdmin`, true},
		{"requiresOwnerOrAdmin is not requiresOwner", `statusIsActive && requiresOwnerOrAdmin`, true},
		{"requiresAdmin as a disjunct", `statusIsActive || requiresAdmin`, false},

		// Whitespace around the comparison is legal.
		{"spaced comparison", `x==args.x && actor.isClusterOwner == true`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clauseGuarantees(tc.clause, adminGateLeaf); got != tc.want {
				t.Errorf("clauseGuarantees(%q, adminGateLeaf) = %v, want %v", tc.clause, got, tc.want)
			}
		})
	}
}

// adminGateNamesDeliberatelyUndeclared records recogniser alternatives that no
// `spec` in dsl/ declares, and why that is on purpose (memql#3016).
//
// The recognisers above are the vocabulary the per-row-authz classifier gates
// on. A name listed there but declared nowhere reads as real from the outside,
// and that is not hypothetical: it is exactly how `spec("requiresClusterOwner")`
// survived in six documents (memql#2983) before anyone checked whether the
// spec existed.
//
// Keeping the alternative and recording its status beats deleting it. Deleting
// makes the recogniser silently stop matching the day someone declares the
// spec -- the gate would go blind at the moment it started mattering. The
// entry costs one line and the reason is the thing that keeps it honest.
var adminGateNamesDeliberatelyUndeclared = map[string]string{
	"requiresClusterOwner": "no `spec ... requiresClusterOwner` exists in dsl/. It is the name the " +
		"per-row-authz audit doc uses and a #54 follow-up placeholder. Kept in the recognisers so " +
		"the gate does not go blind the first time someone declares it (memql#3016).",
}

// adminGateSpecNames are the spec-name alternatives the recognisers match --
// the actor.isClusterOwner comparison is a field reference, not a spec, and is
// excluded.
//
// Derived from the recogniser SOURCE rather than restated, so a name added to
// the pattern is automatically required to be declared or recorded. A hand-kept
// second copy is the drift this whole test is about.
func adminGateSpecNames(t *testing.T) []string {
	t.Helper()
	src := adminGateRe.String()
	found := regexp.MustCompile(`requires[A-Za-z]+`).FindAllString(src, -1)
	if len(found) == 0 {
		t.Fatal("no spec names extracted from the admin-gate recogniser -- the pattern shape " +
			"changed and this check would now pass vacuously (memql#3016)")
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range found {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// Every name the admin-gate recognisers match must be declared in dsl/ or
// recorded as deliberately undeclared. This is the check that replaces the two
// stale prose claims (memql#3016).
//
// It also reports which names the corpus actually FILTERS on, so the fact the
// old comments got wrong is computed instead of written down. Reported rather
// than asserted: a name going from unused to used is normal and must not need
// a test edit -- the failure this guards against is a name nothing declares,
// not a name nothing uses yet.
func TestAdminGateNamesAreDeclaredOrRecorded(t *testing.T) {
	declared := map[string]bool{}
	specDecl := regexp.MustCompile(`(?m)^spec\s+\S+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var specFiles int
	for _, path := range paths {
		if !strings.HasSuffix(path, "specs.memql") {
			continue
		}
		specFiles++
		for _, m := range specDecl.FindAllStringSubmatch(readTreeFile(t, path), -1) {
			declared[m[1]] = true
		}
	}
	if specFiles == 0 {
		t.Fatal("found no specs.memql files -- the sweep has stopped resolving them and every " +
			"name would report as undeclared")
	}
	if len(declared) == 0 {
		t.Fatal("parsed no spec declarations at all -- the declaration pattern has stopped " +
			"matching and this check would fail on every name for the wrong reason")
	}

	for _, name := range adminGateSpecNames(t) {
		if declared[name] {
			if reason, recorded := adminGateNamesDeliberatelyUndeclared[name]; recorded {
				t.Errorf("%q is recorded as deliberately undeclared (%q) but dsl/ now declares it. "+
					"Remove the entry -- a stale record is worse than none, because the next "+
					"reader believes it.", name, reason)
			}
			continue
		}
		if _, recorded := adminGateNamesDeliberatelyUndeclared[name]; !recorded {
			t.Errorf("the admin-gate recogniser matches %q, but no `spec ... %s {` is declared "+
				"anywhere in dsl/. A name gated on but declared nowhere reads as real from the "+
				"outside -- that is how spec(\"requiresClusterOwner\") survived in six documents "+
				"(memql#2983). Declare it, or add it to adminGateNamesDeliberatelyUndeclared "+
				"with the reason (memql#3016).", name, name)
		}
	}
}

// TestNamedQueriesKeepTheirAdminGate pins, BY NAME, the queries whose only
// caller-scope protection is an admin gate in the filter.
//
// TestAdminGateIsATopLevelConjunct above checks the SHAPE of every gate it
// finds -- that a gate present is a gate that holds. It cannot check that a
// gate is present at all, so deleting one is invisible to it: the query simply
// stops being one of the clauses it inspects.
//
// Measured during the memql#3064 review: dropping `&& requiresOwnerOrAdmin`
// from BOTH audit queries left the entire dsl suite green. Nothing else
// reaches them either -- `grep auditEventsByActor dsl/*_test.go` was empty,
// and dsl/conformance_test.go's classifier accepts the remaining
// `actorUserId==args.userId` as caller-scoped even though `args.userId` is
// caller-supplied (its own note at the caller-arg regex says `args.userId` is
// deliberately excluded from the `actor.userId` match). So memql#2987 gated
// five queries and pinned three of them; these are the other two.
//
// These project actorEmail, targetEmail, sourceIP and userAgent. An ungated
// filter keyed on a caller-supplied userId hands that to anyone who guesses an
// id, which is the disclosure memql#2987 exists to close.
func TestNamedQueriesKeepTheirAdminGate(t *testing.T) {
	// path -> query name. Add a row when a query's caller-scope protection is
	// an admin gate rather than an actor-keyed filter or @serverOnly.
	want := map[string]map[string]bool{
		"identity/queries.memql": {
			"auditEventsByActor":  true,
			"auditEventsByTarget": true,
			// memql#3178: the operator arm of the patIdentitiesForUser split.
			// It keeps a CALLER-SUPPLIED userId (the CLI's `--user-id` and the
			// /admin/tokens roll-up both list somebody else's tokens on
			// purpose), so `requiresOwnerOrAdmin` is the only thing standing
			// between it and the enumeration hole the split closed. The
			// self-scoped half, patIdentitiesForSelf, needs no entry here --
			// it is actor-keyed rather than gated.
			"patIdentitiesForUser": true,
		},
	}

	found := map[string]map[string]bool{}
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		names, ok := want[p]
		if !ok {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)

		for _, m := range constructHeaderRe.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[4]:m[5]]
			if !names[name] {
				continue
			}
			closeIdx := matchingClose(src, m[1]-1)
			if closeIdx < 0 {
				t.Errorf("%s: %s has no closing brace", p, name)
				continue
			}
			if found[p] == nil {
				found[p] = map[string]bool{}
			}
			found[p][name] = true

			clause := filterClauseOf(src[m[1]:closeIdx])
			if !mentionsAdminGate(clause) {
				lineNo := strings.Count(src[:m[0]], "\n") + 1
				t.Errorf("%s:%d  %s: the admin gate is GONE from this query's filter.\n"+
					"    filter  %s\n"+
					"It projects caller PII (actorEmail / targetEmail / sourceIP / userAgent) and "+
					"its filter keys on a CALLER-SUPPLIED userId, so without the gate anyone who "+
					"guesses an id reads another user's audit trail. If this query is now protected "+
					"some other way -- @serverOnly, or an actor-keyed filter -- move it out of this "+
					"table and say which in the same change (memql#2987, memql#3064).",
					p, lineNo, name, clause)
			}
		}
	}

	// Anti-vacuity: a renamed or deleted query must fail loudly rather than
	// silently dropping out of the sweep, which is how a by-name pin rots.
	for p, names := range want {
		for name := range names {
			if !found[p][name] {
				t.Errorf("%s: %s was not found in the tree. If it was renamed or removed, update "+
					"this table in the same change -- an entry that matches nothing protects "+
					"nothing and this test would otherwise pass vacuously.", p, name)
			}
		}
	}
}

// The converse of TestAdminGateNamesAreDeclaredOrRecorded: every caller-scope
// spec the tree DECLARES must be one the recognisers know.
//
// That test computes recogniser -> declared. This one computes declared ->
// recogniser, and without it the pair is half a check. Found in the memql#3071
// review: `spec actorEnvelope requiresDeveloperOrAbove` is declared in
// dsl/deployment/specs.memql and documented there as "the FORWARD-DEPLOY gate
// (#1876)", and appeared in NEITHER adminGateRe nor adminGateMentionRe.
//
// What that costs is silence, not noise. A filter gated only by an unrecognised
// spec is not selected by TestAdminGateIsATopLevelConjunct, so the composition
// rule -- gate must be a top-level conjunct in the affirmative form -- never
// runs on it; and the per-row-authz classifier does not count it as
// caller-scoped. The gate reads as present to a human and is absent to every
// machine that checks. That is exactly the shape memql#2983 and memql#3016
// are about, and it is why the recogniser needs both directions computed
// rather than one.
//
// Scoped to @actor-bound specs: those are the caller-context predicates, the
// only ones that can serve as an admin gate. A row-spec is a SQL predicate over
// payload fields and belongs in no gate vocabulary.
func TestEveryDeclaredActorGateIsRecognised(t *testing.T) {
	// Names that are @actor-bound but deliberately NOT gate vocabulary. Empty
	// today, and an entry here is a claim that a caller-scope spec is not a
	// caller-scope GATE -- which wants a reason beside it.
	notGateVocabulary := map[string]string{}

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	// `spec actorEnvelope <name> {` -- the @actor binding is the signature's
	// first identifier, so the declaration alone says whether it is a
	// context-spec. No AST needed and none available here.
	actorSpec := regexp.MustCompile(`(?m)^spec[ \t]+actorEnvelope[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	declared := map[string]string{}
	var specFiles int
	for _, p := range paths {
		if !strings.HasSuffix(p, "specs.memql") {
			continue
		}
		specFiles++
		for _, m := range actorSpec.FindAllStringSubmatch(readTreeFile(t, p), -1) {
			declared[m[1]] = p
		}
	}

	if specFiles == 0 {
		t.Fatal("found no specs.memql files -- the sweep has stopped resolving them and this " +
			"check would now pass vacuously (memql#3071)")
	}
	if len(declared) == 0 {
		t.Fatal("found no `spec actorEnvelope <name>` declarations at all -- the declaration " +
			"pattern has stopped matching and this check would now pass vacuously (memql#3071)")
	}

	for name, path := range declared {
		if why, ok := notGateVocabulary[name]; ok {
			t.Logf("KNOWN (not gate vocabulary): %s in %s -- %s", name, path, why)
			continue
		}
		if adminGateLeaf(name) && mentionsAdminGate(name) {
			continue
		}
		t.Errorf("%s declares the caller-scope spec %q, which neither admin-gate recogniser "+
			"matches.\n"+
			"An unrecognised gate is a SILENT one: a filter gated only by it is not selected by "+
			"TestAdminGateIsATopLevelConjunct, so the composition rule never runs on it, and the "+
			"per-row-authz classifier does not count it as caller-scoped. It reads as protected "+
			"and is unprotected to every machine that checks.\n"+
			"Add it to adminGateRe AND adminGateMentionRe -- both, since they are the strict and "+
			"polarity-blind twins -- or record it in notGateVocabulary with the reason it is not "+
			"a gate (memql#3071).", path, name)
	}
}
