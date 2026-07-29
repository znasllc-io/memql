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
  filter  ownerUserId==actor.userId
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
  filter  actor.isClusterOwner==true
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
  filter  row.id==args.id
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
  filter  ownerUserId==actor.userId
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
  filter  ownerUserId==actor.userId || visibility=="public"
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
  filter  ownerUserId==actor.userId && kind=="doc" || shared==true
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
  filter  (kind=="doc" || kind=="sheet") && ownerUserId==actor.userId
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
  filter  ownerUserId==actor.userId
  shape   planFull
}

query plan plansForSpace {
  filter  spaceId==args.spaceId
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
  filter  ownerUserId==actor.userId
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
// query. library.artifact's `libraryWorkspaceLiveSources` documents its
// rows as having no owner at all, so declaring the concept owned
// asserts something that query disproves.
func TestAPublicQueryBlocksAnOwnedTier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"library/concepts.memql": "concept artifact {\n  ownerUserId string\n}\n",
		"library/queries.memql": `query artifact myArtifacts {
  filter  ownerUserId==actor.userId
  shape   artifactFull
}

@public
query artifact workspaceLiveSources {
  filter  ownerUserId==""
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
  filter  ownerUserId==actor.userId
  shape   userFull
}

@serverOnly
query user userById {
  filter  row.id==args.id
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
  filter  a=="b" // && ownerUserId==actor.userId
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
  filter  pattern=="{"
  shape   noteFull
}

query note second {
  filter  ownerUserId==actor.userId
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
  filter  ownerUserId==actor.userId
  shape   noteFull
}
`,
		"notes/mutations.memql": `mutate note updateNote {
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

// A mutation whose stamped value happens to mention actor.userId must
// not be read as establishing a tier on its own.
func TestAMutationCannotEstablishATier(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n}\n",
		"notes/mutations.memql": `mutate note createNote {
  insert {
    ownerUserId: actor.userId
  }
}
`,
	})
	if decl, ok := got.Tiers["notes"]["note"]; ok {
		t.Fatalf("a mutation established %+v; only queries may vote", decl)
	}
}

// A query in a nested directory belongs to the same domain and must be
// seen. Dropping its evidence means declaring a tier it disproves.
func TestNestedFilesAreWalked(t *testing.T) {
	got := inferOne(t, map[string]string{
		"notes/concepts.memql": "concept note {\n  ownerUserId string\n  kind string\n}\n",
		"notes/queries.memql": `query note myNotes {
  filter  ownerUserId==actor.userId
  shape   noteFull
}
`,
		"notes/extra/more.memql": `query note allNotesByKind {
  filter  kind==args.kind
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
  filter  status=="opted_out"
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
  filter  kind==args.kind
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
	for _, want := range []string{"NOT examined", "mutations", "granted", "public"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report = %q, want it to state that %q is not examined", report, want)
		}
	}
}

// Disagreement is recorded, not resolved. Picking a winner would
// launder the conflict into a declaration Phase 2 then trusts.
func TestDisagreeingQueriesAbstainWithAReason(t *testing.T) {
	got := inferOne(t, map[string]string{
		"authoring/concepts.memql": "concept bundle {\n  ownerUserId string\n}\n",
		"authoring/queries.memql": `query bundle mine {
  filter  ownerUserId==actor.userId
  shape   bundleFull
}

query bundle all {
  filter  actor.isClusterOwner==true
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
  filter  ownerUserId==actor.userId
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
  filter  ownerUserId==actor.userId
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
