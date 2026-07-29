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
