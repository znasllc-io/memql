package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// writeDSLTree materialises a throwaway dsl/ tree from a map of
// "<domain>/<file>.memql" -> source.
func writeDSLTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func inferOne(t *testing.T, files map[string]string) *rowAuthzInference {
	t.Helper()
	got, err := inferRowAuthz(writeDSLTree(t, files))
	if err != nil {
		t.Fatalf("inferRowAuthz: %v", err)
	}
	return got
}

// A caller-scoped filter is proof the rows belong to a user.
func TestInferOwnedFromCallerScopedFilter(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["notes"]["note"] != want {
		t.Fatalf("inferred %+v, want %+v (abstained: %v)", got.Tiers["notes"]["note"], want, got.Abstained)
	}
}

func TestInferClusterOwnerFromAdminGate(t *testing.T) {
	got := inferOne(t, map[string]string{
		"telephony/concepts.memql": "concept call {\n  fromE164 string\n}\n",
		"telephony/queries.memql": `query call allCalls {
  filter  row => actor.isClusterOwner == true
  shape   callFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}
	if got.Tiers["telephony"]["call"] != want {
		t.Fatalf("inferred %+v, want %+v", got.Tiers["telephony"]["call"], want)
	}
}

// THE LOAD-BEARING ABSTENTION.
//
// A construct-level `@public` says "this CALL is intentionally
// unscoped". It is an author's acknowledgement, it carries no runtime
// semantics, and it answers a different question from "may anyone see
// these ROWS". Promoting it to a concept tier would declare
// identity.user -- email, phone, birthdate -- publicly readable off a
// single annotation on the pre-auth bootstrap query, which is exactly
// the silent permissiveness #2803 wants to end.
func TestPublicAnnotationNeverInfersAPublicTier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept user {\n  primaryEmail string\n}\n",
		"identity/queries.memql": `@public
query user userById {
  args {
    id  string!
  }
  filter  row => row.id == args.id
  shape   userFull
}
`,
	})
	if decl, ok := got.Tiers["identity"]["user"]; ok {
		t.Fatalf("inferred %+v for identity.user from a construct-level @public; it must abstain", decl)
	}
	if _, ok := got.Abstained[conceptKey{Domain: "identity", Name: "user"}]; !ok {
		t.Fatal("identity.user is neither declared nor recorded as abstained")
	}
	if got.Counts[langparser.RowAuthzPublic] != 0 {
		t.Fatalf("public tier count = %d, want 0 -- public is never inferred", got.Counts[langparser.RowAuthzPublic])
	}
}

// @serverOnly is evidence about the call surface, not about the rows.
func TestServerOnlyQueryAbstains(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept user {\n  ownerUserId string\n}\n",
		"identity/queries.memql": `@serverOnly
query user resolveUser {
  filter  row => row.ownerUserId == actor.userId
  shape   userFull
}
`,
	})
	if decl, ok := got.Tiers["identity"]["user"]; ok {
		t.Fatalf("a @serverOnly query voted %+v; it must abstain", decl)
	}
}

// A term inside a top-level disjunction guarantees nothing: the other
// arm still returns rows it would have excluded (memql#2832).
func TestPermissiveDisjunctDoesNotInferOwned(t *testing.T) {
	got := inferOne(t, map[string]string{
		"library/concepts.memql": "concept artifact {\n  ownerUserId string\n  visibility string\n}\n",
		"library/queries.memql": `query artifact artifacts {
  filter  row => row.ownerUserId == actor.userId || row.visibility == "public"
  shape   artifactFull
}
`,
	})
	if decl, ok := got.Tiers["library"]["artifact"]; ok {
		t.Fatalf("inferred %+v from a filter whose caller-scope term is one arm of a disjunction", decl)
	}
}

// The sharper form of the same defect, and the one anchoring alone
// does not catch. `&&` binds tighter than `||`, so
//
//	ownerUserId==actor.userId && kind=="doc" || shared==true
//
// parses as `(ownerUserId==actor.userId && kind=="doc") || shared==true`
// -- the right arm returns rows the caller does not own. Splitting on
// `&&` first yields `ownerUserId==actor.userId` as a lone conjunct,
// which reads as caller-scoped while the query is not.
func TestOwnedTermUnderATopLevelDisjunctionDoesNotCount(t *testing.T) {
	got := inferOne(t, map[string]string{
		"library/concepts.memql": "concept artifact {\n  ownerUserId string\n  kind string\n  shared boolean\n}\n",
		"library/queries.memql": `query artifact artifacts {
  filter  row => row.ownerUserId == actor.userId && row.kind == "doc" || row.shared == true
  shape   artifactFull
}
`,
	})
	if decl, ok := got.Tiers["library"]["artifact"]; ok {
		t.Fatalf("inferred %+v; the caller-scope term sits under a top-level `||`, so it guarantees nothing", decl)
	}
}

// A caller-scope term that IS a top-level conjunct still counts, even
// alongside a parenthesised disjunction.
func TestConjunctAlongsideDisjunctionStillInfersOwned(t *testing.T) {
	got := inferOne(t, map[string]string{
		"library/concepts.memql": "concept artifact {\n  ownerUserId string\n  kind string\n}\n",
		"library/queries.memql": `query artifact artifacts {
  filter  row => (row.kind == "doc" || row.kind == "sheet") && row.ownerUserId == actor.userId
  shape   artifactFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["library"]["artifact"] != want {
		t.Fatalf("inferred %+v, want %+v", got.Tiers["library"]["artifact"], want)
	}
}

// THE RULE THAT MAKES THE INFERENCE SOUND.
//
// A tier is a FLOOR -- the predicate that will eventually be AND-ed
// into every access. So a query carrying no caller-scope term is not a
// neutral bystander, it is a COUNTEREXAMPLE: it reads rows the floor
// would exclude. Counting only the positive votes declared
// planner.plan owned off 2 of its 10 queries while the primary
// user-facing read was space-scoped.
func TestAnUnscopedQueryBlocksTheTier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"planner/concepts.memql": "concept plan {\n  ownerUserId string\n  spaceId string\n}\n",
		"planner/queries.memql": `query plan myPlans {
  filter  row => row.ownerUserId == actor.userId
  shape   planFull
}

query plan plansForSpace {
  filter  row => row.spaceId == args.spaceId
  shape   planFull
}
`,
	})
	if decl, ok := got.Tiers["planner"]["plan"]; ok {
		t.Fatalf("inferred %+v; plansForSpace reads other users' rows, so `owned` is not a floor this concept satisfies", decl)
	}
	reason := got.Abstained[conceptKey{Domain: "planner", Name: "plan"}]
	if !strings.Contains(reason, "plansForSpace") {
		t.Fatalf("abstention reason = %q, want it to name the counterexample query", reason)
	}
}

// A query with no filter at all reads every row, which blocks any
// narrowing tier just as surely.
func TestAnUnfilteredQueryBlocksTheTier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}

query note allNotes {
  shape   noteFull
}
`,
	})
	if decl, ok := got.Tiers["notes"]["note"]; ok {
		t.Fatalf("inferred %+v despite an unfiltered query over the same concept", decl)
	}
}

// The composite case that shipped wrong: owned queries PLUS a @public
// query. library.artifact's `libraryWorkspaceLiveSources` documented its
// rows as having no owner at all, so declaring the concept owned
// asserted something that query disproved. The fixture below is
// synthetic and stays the live test; the real read was rescoped when
// memql#4340 declared that concept's tier by hand.
func TestAPublicQueryBlocksAnOwnedTier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"library/concepts.memql": "concept artifact {\n  ownerUserId string\n}\n",
		"library/queries.memql": `query artifact myArtifacts {
  filter  row => row.ownerUserId == actor.userId
  shape   artifactFull
}

@public
query artifact workspaceLiveSources {
  filter  row => row.ownerUserId == ""
  shape   artifactFull
}
`,
	})
	if decl, ok := got.Tiers["library"]["artifact"]; ok {
		t.Fatalf("inferred %+v; the @public sibling reads rows the tier would exclude", decl)
	}
	if !strings.Contains(got.Abstained[conceptKey{Domain: "library", Name: "artifact"}], "@public") {
		t.Fatalf("abstention reason should name the @public query: %q",
			got.Abstained[conceptKey{Domain: "library", Name: "artifact"}])
	}
}

// @serverOnly is the ONE exempt verdict: not a client-callable read, so
// it neither votes nor blocks (#2803 design decision 4 reserves an
// explicit system actor for that path).
func TestServerOnlyQueryDoesNotBlock(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept user {\n  ownerUserId string\n}\n",
		"identity/queries.memql": `query user myUser {
  filter  row => row.ownerUserId == actor.userId
  shape   userFull
}

@serverOnly
query user userById {
  filter  row => row.id == args.id
  shape   userFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["identity"]["user"] != want {
		t.Fatalf("inferred %+v, want %+v -- a @serverOnly query must not block",
			got.Tiers["identity"]["user"], want)
	}
}

// A trailing comment must not be read as part of the filter clause. On
// raw source `filter a==b // && ownerUserId==actor.userId` splits into
// two conjuncts and MANUFACTURES a tier the query does not have.
func TestACommentCannotManufactureATier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n  a string\n}\n",
		"notes/queries.memql": `query note sneaky {
  filter  row => row.a == "b" // && row.ownerUserId == actor.userId
  shape   noteFull
}
`,
	})
	if decl, ok := got.Tiers["notes"]["note"]; ok {
		t.Fatalf("inferred %+v from a conjunct that lives inside a comment", decl)
	}
}

// A brace inside a string must not run one construct's body into the
// next, or a query votes with a different construct's filter.
func TestABraceInAStringDoesNotMergeBodies(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n  pattern string\n}\n",
		"notes/queries.memql": `query note first {
  filter  row => row.pattern == "{"
  shape   noteFull
}

query note second {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`,
	})
	// `first` is unscoped, so it blocks -- which it can only do if its
	// body was sliced correctly rather than swallowing `second`.
	if decl, ok := got.Tiers["notes"]["note"]; ok {
		t.Fatalf("inferred %+v; the unscoped `first` must block, and it can only be seen if bodies slice correctly", decl)
	}
	if !strings.Contains(got.Abstained[conceptKey{Domain: "notes", Name: "note"}], "first") {
		t.Fatalf("abstention should name `first`: %q", got.Abstained[conceptKey{Domain: "notes", Name: "note"}])
	}
}

// The same hazard through a literal that SPANS lines (memql#3116), and
// the consumer assessment for BlankCommentsAndStrings: a blanker that
// closed the literal at the newline exposed the `}` on its second line
// as code, so the brace walk hit depth 0 at the args block and the
// query's body was truncated before its filter. A construct whose
// filter is invisible reads as unfiltered, which BLOCKS -- so the
// symptom is a correct declaration silently dropped.
func TestAMultiLineStringDoesNotTruncateABody(t *testing.T) {
	src := `query note myNotes {
  args {
    label  string  @description("a label
} that wraps")
  }
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`
	if _, err := langparser.NewLexer(src).Tokenize(); err != nil {
		t.Fatalf("the fixture must lex cleanly, or it proves nothing about valid input: %v", err)
	}
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n  label string\n}\n",
		"notes/queries.memql":  src,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["notes"]["note"] != want {
		t.Fatalf("inferred %+v, want %+v -- the filter is only visible if the multi-line literal is blanked whole (abstained: %v)",
			got.Tiers["notes"]["note"], want, got.Abstained)
	}
}

// A mutation neither votes nor blocks. It cannot vote (actor.userId
// there is a stamped value, not a row selection) and it must not block
// (an ungated `update { id: args.x }` is the gap #2803 exists to
// close, not evidence that the concept's rows are unowned). Blocking
// on it dropped 6 of 13 correct declarations, `telephony.call`
// included.
func TestAMutationNeitherVotesNorBlocks(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`,
		"notes/mutations.memql": `mutation note updateNote {
  args {
    noteId  string!
  }
  update {
    id: args.noteId
  }
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["notes"]["note"] != want {
		t.Fatalf("inferred %+v, want %+v -- an ungated update must not block (abstained: %v)",
			got.Tiers["notes"]["note"], want, got.Abstained)
	}
}

// The verdict itself, asserted directly. Going through inferRowAuthz
// here would be vacuous: a mutation has no `filter` line, so it
// abstains via the no-clause path whether or not the exempt branch
// exists, and the test would pass against an implementation that
// blocks on every mutation.
func TestClassifyConstructVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		preamble string
		body     string
		want     verdictKind
	}{
		{"mutation stamping the actor", "mutation", "", "{\n  insert {\n    ownerUserId: actor.userId\n  }\n}", verdictExempt},
		{"mutation updating by arg", "mutation", "", "{\n  update {\n    id: args.noteId\n  }\n}", verdictExempt},
		{"serverOnly query", "query", "@serverOnly\n", "{\n  filter  row => row.id == args.id\n}", verdictExempt},
		{"public query", "query", "@public\n", "{\n  filter  row => row.active == true\n}", verdictBlocks},
		{"query with no filter", "query", "", "{\n  shape noteFull\n}", verdictBlocks},
		{"unscoped query", "query", "", "{\n  filter  row => row.spaceId == args.spaceId\n}", verdictBlocks},
		{"caller-scoped query", "query", "", "{\n  filter  row => row.ownerUserId == actor.userId\n}", verdictVote},
		{"admin-gated query", "query", "", "{\n  filter  row => actor.isClusterOwner == true\n}", verdictVote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyConstruct(tc.kind, tc.preamble, tc.body, tc.body)
			if got.Kind != tc.want {
				t.Fatalf("classifyConstruct(%s) Kind = %v, want %v (reason %q, decl %+v)",
					tc.kind, got.Kind, tc.want, got.Reason, got.Decl)
			}
			if got.Kind == verdictBlocks && got.Reason == "" {
				t.Fatal("a blocking verdict must carry a reason")
			}
			if got.Kind == verdictVote {
				if _, err := langparser.FormatRowAuthz(got.Decl); err != nil {
					t.Fatalf("a vote must carry a formattable decl: %v", err)
				}
			}
		})
	}
}

// An @serverOnly separated from its header by a comment line must
// still be seen. Computing the preamble boundary on the blanked view
// stops at the comment (it trims to ""), which silently reclassifies
// the construct as a blocker.
func TestPreambleReachesPastACommentLine(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept user {\n  ownerUserId string\n}\n",
		"identity/queries.memql": `query user myUser {
  filter  row => row.ownerUserId == actor.userId
  shape   userFull
}

@serverOnly
// Resolves sub -> user before an actor exists.
query user userById {
  filter  row => row.id == args.id
  shape   userFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["identity"]["user"] != want {
		t.Fatalf("inferred %+v, want %+v -- the @serverOnly above a comment line was lost (abstained: %v)",
			got.Tiers["identity"]["user"], want, got.Abstained)
	}
}

// A query in a nested directory belongs to the same domain and must be
// seen. Dropping its evidence means declaring a tier it disproves.
func TestNestedFilesAreWalked(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n  kind string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`,
		"notes/extra/more.memql": `query note allNotesByKind {
  filter  row => row.kind == args.kind
  shape   noteFull
}
`,
	})
	if decl, ok := got.Tiers["notes"]["note"]; ok {
		t.Fatalf("inferred %+v; the nested unscoped query must block", decl)
	}
	if !strings.Contains(got.Abstained[conceptKey{Domain: "notes", Name: "note"}], "allNotesByKind") {
		t.Fatalf("abstention should name the nested query: %q",
			got.Abstained[conceptKey{Domain: "notes", Name: "note"}])
	}
}

// The abstention reason is the human-facing product and Phase 2's
// worklist. Quoting the blanked view prints string literals as runs of
// spaces, which makes the "why" unreadable.
func TestAbstentionReasonQuotesTheAuthorsText(t *testing.T) {
	got := inferOne(t, map[string]string{
		"telephony/concepts.memql": "concept consent {\n  status string\n}\n",
		"telephony/queries.memql": `query consent optedOut {
  filter  row => row.status == "opted_out"
  shape   consentFull
}
`,
	})
	reason := got.Abstained[conceptKey{Domain: "telephony", Name: "consent"}]
	// The literal's CONTENTS must survive. Quoting the blanked view
	// would print `status=="        "`, which says nothing.
	if !strings.Contains(reason, "opted_out") {
		t.Fatalf("reason = %q, want the string literal's contents, not a run of spaces", reason)
	}
	if strings.Contains(reason, `"          "`) || strings.Contains(reason, "==\\\"    ") {
		t.Fatalf("reason = %q, want the author's text rather than a blanked clause", reason)
	}
}

// A `use` inside a block comment is not an import, and treating it as
// one re-homes every vote in the file to the wrong domain.
func TestACommentedOutImportDoesNotRehomeVotes(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept thing {\n  ownerUserId string\n}\n",
		"notes/concepts.memql":    "concept thing {\n  ownerUserId string\n  kind string\n}\n",
		"notes/queries.memql": `/*
use identity.concepts.{ thing }
*/

query thing unscoped {
  filter  row => row.kind == args.kind
  shape   thingFull
}
`,
	})
	// The blocker must land on notes.thing, not identity.thing.
	if !strings.Contains(got.Abstained[conceptKey{Domain: "notes", Name: "thing"}], "unscoped") {
		t.Fatalf("notes.thing lost its blocker: %q", got.Abstained[conceptKey{Domain: "notes", Name: "thing"}])
	}
	if strings.Contains(got.Abstained[conceptKey{Domain: "identity", Name: "thing"}], "unscoped") {
		t.Fatal("identity.thing absorbed a vote from a commented-out import")
	}
}

// The report has to say what it did NOT examine, or it reads as full
// coverage.
func TestReportStatesWhatItDidNotExamine(t *testing.T) {
	report := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  x string\n}\n",
	}).Report()
	idx := strings.Index(report, "NOT examined")
	if idx < 0 {
		t.Fatalf("report = %q, want a section stating what was not examined", report)
	}
	// Assert against THAT section only -- "public" and "granted" also
	// appear in the tier table above it, which would make this pass
	// regardless.
	limits := report[idx:]
	for _, want := range []string{"mutations", "granted", "public"} {
		if !strings.Contains(limits, want) {
			t.Fatalf("the not-examined section = %q, want it to name %q", limits, want)
		}
	}
}

// Disagreement is recorded, not resolved. Picking a winner would
// launder the conflict into a declaration Phase 2 then trusts.
func TestDisagreeingQueriesAbstainWithAReason(t *testing.T) {
	got := inferOne(t, map[string]string{
		"authoring/concepts.memql": "concept bundle {\n  ownerUserId string\n}\n",
		"authoring/queries.memql": `query bundle mine {
  filter  row => row.ownerUserId == actor.userId
  shape   bundleFull
}

query bundle all {
  filter  row => actor.isClusterOwner == true
  shape   bundleFull
}
`,
	})
	if decl, ok := got.Tiers["authoring"]["bundle"]; ok {
		t.Fatalf("inferred %+v despite disagreeing queries", decl)
	}
	reason := got.Abstained[conceptKey{Domain: "authoring", Name: "bundle"}]
	if !strings.Contains(reason, "disagree") {
		t.Fatalf("abstention reason = %q, want it to say the queries disagree", reason)
	}
	for _, want := range []string{"mine", "all"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("abstention reason = %q, want it to name query %q", reason, want)
		}
	}
}

// A query in one domain over a concept imported from another must be
// counted against the DECLARING domain, or the rewrite edits the wrong
// concepts.memql.
func TestImportedConceptResolvesToItsDeclaringDomain(t *testing.T) {
	got := inferOne(t, map[string]string{
		"identity/concepts.memql": "concept user {\n  ownerUserId string\n}\n",
		"library/concepts.memql":  "concept artifact {\n  x string\n}\n",
		"library/queries.memql": `use identity.concepts.{ user }

query user myUser {
  filter  row => row.ownerUserId == actor.userId
  shape   userFull
}
`,
	})
	want := langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	if got.Tiers["identity"]["user"] != want {
		t.Fatalf("identity.user inferred %+v, want %+v", got.Tiers["identity"]["user"], want)
	}
	if _, wrong := got.Tiers["library"]["user"]; wrong {
		t.Fatal("a library-domain tier was recorded for an identity-domain concept")
	}
}

// Underscore-prefixed directories are the tree's soft-disable
// convention; the loader skips them and the codemod must too.
func TestUnderscoreDomainsAreSkipped(t *testing.T) {
	got := inferOne(t, map[string]string{
		"_reference/concepts.memql": "concept _example {\n  ownerUserId string\n}\n",
		"notes/concepts.memql":      "concept note {\n  ownerUserId string\n}\n",
	})
	if _, ok := got.Abstained[conceptKey{Domain: "_reference", Name: "_example"}]; ok {
		t.Fatal("a _reference concept was counted")
	}
	if len(got.Abstained) != 1 {
		t.Fatalf("abstained = %v, want only notes.note", got.Abstained)
	}
}

// The report must state what was left alone, not only what was done.
// A run that prints only its successes reads as full coverage.
func TestReportNamesTheUndeclared(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  x string\n}\n",
	})
	report := got.Report()
	for _, want := range []string{"undeclared", "notes.note", "TOTAL"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report = %q, want it to contain %q", report, want)
		}
	}
}

// End to end: infer, rewrite, and confirm the file gained exactly the
// declaration the inference decided on.
func TestRewriteAppliesTheInferredTier(t *testing.T) {
	root := writeDSLTree(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  row => row.ownerUserId == actor.userId
  shape   noteFull
}
`,
	})
	t.Cleanup(func() { delete(rowAuthzInferenceCache, root) })

	path := filepath.Join(root, "notes", "concepts.memql")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out, err := rewriteRowAuthz(path, src)
	if err != nil {
		t.Fatalf("rewriteRowAuthz: %v", err)
	}
	if !strings.Contains(string(out), `@rowAuthz(owner="ownerUserId")`+"\nconcept note") {
		t.Fatalf("rewritten file:\n%s", out)
	}

	// A non-concepts file is never a target.
	qPath := filepath.Join(root, "notes", "queries.memql")
	qSrc, err := os.ReadFile(qPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	qOut, err := rewriteRowAuthz(qPath, qSrc)
	if err != nil {
		t.Fatalf("rewriteRowAuthz(queries): %v", err)
	}
	if string(qOut) != string(qSrc) {
		t.Fatalf("queries.memql was rewritten:\n%s", qOut)
	}
}

// TestClassifyConstructReadsTheWholeClause: an edition-2026 filter is read as a
// tree, wrapped lines included -- a first-line read saw one conjunct of
// several -- and a clause in the pre-2026 spelling, which the engine refuses,
// evidences nothing and blocks.
func TestClassifyConstructReadsTheWholeClause(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  verdictKind
		tier  langparser.RowAuthzTier
		owner string
	}{
		{"caller-scoped", "{\n  filter  row => row.ownerUserId == actor.userId\n}", verdictVote, langparser.RowAuthzOwned, "ownerUserId"},
		{"reversed operands", "{\n  filter  row => actor.userId == row.userId && row.active == true\n}", verdictVote, langparser.RowAuthzOwned, "userId"},
		{"owner on a wrapped line", "{\n  filter  row => row.status == \"open\"\n          && row.ownerUserId == actor.userId\n  shape   noteFull\n}", verdictVote, langparser.RowAuthzOwned, "ownerUserId"},
		{"admin gate", "{\n  filter  row => row.targetId == args.targetId && actor.isClusterOwner == true\n}", verdictVote, langparser.RowAuthzClusterOwner, ""},
		// Blocks: nothing guarantees the caller.
		{"unscoped", "{\n  filter  row => row.spaceId == args.spaceId\n}", verdictBlocks, "", ""},
		{"owner widened by a disjunct", "{\n  filter  row => row.ownerUserId == actor.userId || row.visibility == \"public\"\n}", verdictBlocks, "", ""},
		{"owner behind a guard", "{\n  filter  row => (args.mine == nil || row.ownerUserId == actor.userId)\n}", verdictBlocks, "", ""},
		{"nested payload path is not an owner field", "{\n  filter  row => row.credentials.userId == actor.userId\n}", verdictBlocks, "", ""},
		{"pre-2026 clause", "{\n  filter  ownerUserId==actor.userId\n}", verdictBlocks, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyConstruct("query", "", tc.body, tc.body)
			if got.Kind != tc.want {
				t.Fatalf("Kind = %v, want %v (reason %q, decl %+v)", got.Kind, tc.want, got.Reason, got.Decl)
			}
			if tc.want == verdictVote && (got.Decl.Tier != tc.tier || got.Decl.Owner != tc.owner) {
				t.Errorf("decl = %+v, want tier %v owner %q", got.Decl, tc.tier, tc.owner)
			}
			if got.Kind == verdictBlocks && got.Reason == "" {
				t.Fatal("a blocking verdict must carry a reason")
			}
		})
	}
	if got := classifyConstruct("query", "", "{\n  filter  ownerUserId==actor.userId\n}", "{\n  filter  ownerUserId==actor.userId\n}"); !strings.Contains(got.Reason, "pre-2026") {
		t.Errorf("a pre-2026 clause blocks with reason %q; want it to say the clause is pre-2026", got.Reason)
	}
}

// legacyEvidenceTree is a dsl/ tree in the legacy grammar, one domain per
// kind of evidence the inference reads, spelled the way dsl/ spelled it before
// the tree was migrated (memql#5368): a caller-scoped filter carrying a trait
// and a when() guard beside its owner term, a cluster-owner gate, the @public
// and @serverOnly surfaces that abstain, a disjunction that widens the owner
// term away and one that does not, an unscoped query that blocks its
// concept, a query over a concept imported from another domain, and a
// mutation that neither votes nor blocks.
var legacyEvidenceTree = map[string]string{
	"common/traits.memql":   "trait isNotDeleted {\n  return deleted == false\n}\n",
	"worker/concepts.memql": "concept invocation {\n  ownerUserId string\n  runId string\n  action string\n  deleted boolean\n}\n",
	"worker/queries.memql": `use common.traits.{ isNotDeleted }

@actor
query invocation invocationsForRun {
  args {
    runId   string!
    action  string
  }
  filter  runId==args.runId && ownerUserId==actor.userId && isNotDeleted && when(args.action) { action==args.action }
  sort     "row.createdAt", "desc"
  paginate 50
  shape   workerInvocationFull
}
`,
	"telephony/concepts.memql": "concept call {\n  fromE164 string\n}\n",
	"telephony/queries.memql": `query call allCalls {
  filter  actor.isClusterOwner==true && fromE164!=""
  shape   callFull
}
`,
	"identity/concepts.memql": "concept user {\n  primaryEmail string\n  ownerUserId string\n}\n",
	"identity/queries.memql": `@public
query user userById {
  args {
    id  string!
  }
  filter  row.id==args.id
  shape   userFull
}

@serverOnly
query user resolveUser {
  filter  ownerUserId==actor.userId
  shape   userFull
}
`,
	"library/concepts.memql": "concept artifact {\n  ownerUserId string\n  visibility string\n}\n",
	"library/queries.memql": `query artifact artifacts {
  filter  ownerUserId==actor.userId || visibility=="public"
  shape   artifactFull
}
`,
	"tasks/concepts.memql": "concept task {\n  ownerUserId string\n  status string\n}\n",
	"tasks/queries.memql": `query task myOpenOrDone {
  filter  ownerUserId==actor.userId && (status=="open" || status=="done")
  shape   taskFull
}
`,
	"flags/concepts.memql": "concept flag {\n  ownerUserId string\n  status string\n}\n",
	"flags/queries.memql": `query flag myFlags {
  filter  ownerUserId==actor.userId
  shape   flagFull
}

query flag flagsOn {
  filter  status=="on"
  shape   flagFull
}
`,
	"notes/concepts.memql": "concept note {\n  authorUserId string\n}\n",
	"notes/mutations.memql": `mutation note createNote {
  args {
    id  string!
  }
  insert {
    id: args.id
    authorUserId: actor.userId
  }
}
`,
	"boards/concepts.memql": "concept board {\n  title string\n}\n",
	"boards/queries.memql": `use notes.concepts.{ note }

query note myNotes {
  filter  authorUserId==actor.userId
  shape   noteFull
}
`,
}

// TestRowAuthzInferenceReadsAMigratedBundle: a bundle that has not run the
// expressions codemod yet -- legacyEvidenceTree -- gets its tiers inferred once
// it has, and none before.
//
// The inference reads edition 2026 only. A legacy clause is one the engine
// refuses, so it evidences nothing: every concept a legacy query reads is
// blocked, with a reason naming the codemod, and no tier is inferred from text
// the engine would not load. Once migrated, the same tree yields the verdicts
// the fixture was written to produce -- the reachable positive over every kind
// of evidence the inference reads, since a tree it could no longer read would
// infer nothing at all. TestRowAuthzInferenceReadsTheMigratedTree holds the
// tree-scale positive over dsl/.
func TestRowAuthzInferenceReadsAMigratedBundle(t *testing.T) {
	files := map[string][]byte{}
	for p, src := range legacyEvidenceTree {
		files[p] = []byte(src)
	}
	changed, err := rewriteExpressions("", files)
	if err != nil {
		t.Fatalf("expressions rewrite: %v", err)
	}
	// Every file carrying a predicate must come out rewritten, or the
	// migrated tree below is still the legacy one.
	for p := range legacyEvidenceTree {
		if !strings.HasSuffix(p, "/queries.memql") && p != "common/traits.memql" {
			continue
		}
		if _, ok := changed[p]; !ok {
			t.Errorf("the codemod left %s as it was", p)
		}
	}
	migrated := map[string]string{}
	for p, src := range legacyEvidenceTree {
		migrated[p] = src
		if next, ok := changed[p]; ok {
			migrated[p] = string(next)
		}
	}

	got := inferOne(t, migrated)
	owned := func(owner string) langparser.RowAuthzDecl {
		return langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: owner}
	}
	wantTiers := []struct {
		domain, name string
		decl         langparser.RowAuthzDecl
	}{
		{"worker", "invocation", owned("ownerUserId")},
		{"telephony", "call", langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		{"tasks", "task", owned("ownerUserId")},
		{"notes", "note", owned("authorUserId")},
	}
	for _, want := range wantTiers {
		if decl := got.Tiers[want.domain][want.name]; decl != want.decl {
			t.Errorf("%s.%s: the migrated tree infers %+v, want %+v (abstained: %q)", want.domain, want.name, decl, want.decl, got.Abstained[conceptKey{Domain: want.domain, Name: want.name}])
		}
	}
	for _, key := range []conceptKey{
		{Domain: "identity", Name: "user"},
		{Domain: "library", Name: "artifact"},
		{Domain: "flags", Name: "flag"},
	} {
		if _, ok := got.Abstained[key]; !ok {
			t.Errorf("%v: the migrated tree infers %+v, want an abstention", key, got.Tiers[key.Domain][key.Name])
		}
	}

	// The same bundle before the codemod: nothing is inferred, and each
	// concept the migrated tree gave a tier to is blocked by name.
	legacy := inferOne(t, legacyEvidenceTree)
	for domain, tiers := range legacy.Tiers {
		for name, decl := range tiers {
			t.Errorf("%s.%s: inferred %+v from a pre-2026 filter the engine refuses", domain, name, decl)
		}
	}
	for _, want := range wantTiers {
		key := conceptKey{Domain: want.domain, Name: want.name}
		if reason := legacy.Abstained[key]; !strings.Contains(reason, "memqlmigrate --rewrite=expressions") {
			t.Errorf("%v: the legacy tree abstains with %q; want the reason to name the codemod", key, reason)
		}
	}
}

// TestRowAuthzInferenceReadsTheMigratedTree is the tree-scale positive: over
// dsl/ as it is -- edition 2026 since memql#5368 -- the inference still reads
// its evidence, and blocks nothing for being pre-2026. Measured when the floor
// was set: 57 inferred tiers and 104 abstentions, the same numbers the legacy
// tree gave.
func TestRowAuthzInferenceReadsTheMigratedTree(t *testing.T) {
	got, err := inferRowAuthz(filepath.Join("..", "..", "dsl"))
	if err != nil {
		t.Fatalf("infer over dsl/: %v", err)
	}
	inferred := 0
	for _, tiers := range got.Tiers {
		inferred += len(tiers)
	}
	if inferred < 40 {
		t.Errorf("inferred %d tiers over dsl/ -- the inference has stopped reading evidence", inferred)
	}
	for key, reason := range got.Abstained {
		if strings.Contains(reason, "pre-2026") {
			t.Errorf("%v abstains on a pre-2026 clause in the migrated tree: %s", key, reason)
		}
	}
	t.Logf("%d tiers and %d abstentions over dsl/", inferred, len(got.Abstained))
}
