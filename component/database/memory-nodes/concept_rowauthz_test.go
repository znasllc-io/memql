package memoryNodes

import (
	"encoding/json"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// buildConcept parses a single-concept source and builds it, the way
// the unified loader does.
func buildConcept(t *testing.T, src string) (*Concept, error) {
	t.Helper()
	file, err := langparser.ParseFile(src)
	if err != nil {
		t.Fatalf("ParseFile:\n%s\nerror: %v", src, err)
	}
	for _, def := range file.Definitions {
		if cd, ok := def.(*langparser.ConceptDecl); ok {
			return BuildConceptFromDecl(cd, "v1:test:probe")
		}
	}
	t.Fatalf("no concept declaration in:\n%s", src)
	return nil, nil
}

const rowAuthzProbeBody = ` probe {
  ownerUserId  string  @required
  name         string
}
`

// Each of the four tiers loads, and lands on the built Concept with
// the meaning it was declared with.
func TestConceptLoadsEachRowAuthzTier(t *testing.T) {
	cases := []struct {
		annotation string
		want       langparser.RowAuthzDecl
	}{
		{`@rowAuthz(public)`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}},
		{`@rowAuthz(clusterOwner)`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		{`@rowAuthz(owner="ownerUserId")`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}},
		{`@rowAuthz(via="spaceMember")`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"}},
	}
	for _, tc := range cases {
		t.Run(tc.annotation, func(t *testing.T) {
			c, err := buildConcept(t, tc.annotation+"\nconcept"+rowAuthzProbeBody)
			if err != nil {
				t.Fatalf("BuildConceptFromDecl(%s): %v", tc.annotation, err)
			}
			if c.RowAuthz == nil {
				t.Fatalf("BuildConceptFromDecl(%s): RowAuthz is nil", tc.annotation)
			}
			if *c.RowAuthz != tc.want {
				t.Fatalf("BuildConceptFromDecl(%s): RowAuthz = %+v, want %+v", tc.annotation, *c.RowAuthz, tc.want)
			}
		})
	}
}

// A concept with no declaration loads and carries no tier. Phase 1
// warns; it does not refuse.
func TestConceptWithoutRowAuthzStillLoads(t *testing.T) {
	c, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	if c.RowAuthz != nil {
		t.Fatalf("RowAuthz = %+v, want nil for an undeclared concept", c.RowAuthz)
	}
}

// Every malformed declaration refuses to load, and the diagnostic
// names the concept and the problem.
func TestConceptRejectsMalformedRowAuthz(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		wantHint   string
	}{
		{"unknown tier", `@rowAuthz(everyone)`, `unknown tier "everyone"`},
		{"no tier", `@rowAuthz()`, "requires a tier"},
		{"bare value", `@rowAuthz("public")`, "does not take a bare value"},
		{"two tiers", `@rowAuthz(public, clusterOwner)`, "takes exactly one tier"},
		{"empty owner", `@rowAuthz(owner="")`, "is empty"},
		{"owner names an undeclared field", `@rowAuthz(owner="noSuchField")`, "does not declare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConcept(t, tc.annotation+"\nconcept"+rowAuthzProbeBody)
			if err == nil {
				t.Fatalf("BuildConceptFromDecl(%s): want a load error, got nil", tc.annotation)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantHint) {
				t.Fatalf("error = %q, want it to contain %q", msg, tc.wantHint)
			}
			if !strings.Contains(msg, "v1:test:probe") && !strings.Contains(msg, "probe") {
				t.Fatalf("error = %q, want it to name the concept", msg)
			}
		})
	}
}

// Two declarations on one concept must refuse to load rather than
// silently last-win. The parser folds attributes in source order, so
// without this a reader scanning top-down sees a tier the engine does
// not use.
func TestConceptRejectsTwoRowAuthzDeclarations(t *testing.T) {
	sources := []string{
		"@rowAuthz(public)\n@rowAuthz(owner=\"ownerUserId\")\nconcept" + rowAuthzProbeBody,
		"@rowAuthz(public)\n@rowAuthz(public)\nconcept" + rowAuthzProbeBody,
		// Same line -- the parser accepts several annotations per line.
		"@description(\"d\") @rowAuthz(public)\n@rowAuthz(clusterOwner)\nconcept" + rowAuthzProbeBody,
	}
	for _, src := range sources {
		_, err := buildConcept(t, src)
		if err == nil {
			t.Fatalf("two declarations loaded without error:\n%s", src)
		}
		if !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("error = %q, want it to say the annotation was declared more than once", err)
		}
	}
}

// The `owner=` field-existence check must name the fields that DO
// exist, so an author can fix a typo without opening the concept.
func TestRowAuthzOwnerErrorListsDeclaredFields(t *testing.T) {
	_, err := buildConcept(t, `@rowAuthz(owner="ownerUserID")`+"\nconcept"+rowAuthzProbeBody)
	if err == nil {
		t.Fatal("want a load error for a mis-cased owner field, got nil")
	}
	for _, want := range []string{"ownerUserId", "name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to list declared field %q", err, want)
		}
	}
}

// THE AGREEMENT TEST (#2621's lesson, restated for #2920).
//
// The codemod's only emitter is FormatRowAuthz and the loader's only
// reader is ParseRowAuthz. If those two ever disagree, a codemod run
// writes a tree the loader refuses to boot. This asserts the whole
// path end to end: render a declaration the way the codemod does,
// insert it the way the codemod does, then load it the way the engine
// does, and require the meaning to survive.
func TestCodemodOutputLoadsWithTheSameMeaning(t *testing.T) {
	decls := []langparser.RowAuthzDecl{
		{Tier: langparser.RowAuthzPublic},
		{Tier: langparser.RowAuthzClusterOwner},
		{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
		{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"},
	}
	for _, want := range decls {
		rewritten, err := langparser.RewriteRowAuthz(
			[]byte("concept"+rowAuthzProbeBody),
			map[string]langparser.RowAuthzDecl{"probe": want},
		)
		if err != nil {
			t.Fatalf("RewriteRowAuthz(%+v): %v", want, err)
		}
		c, err := buildConcept(t, string(rewritten))
		if err != nil {
			t.Fatalf("codemod emitted a tree the loader refuses (%+v):\n%s\nerror: %v", want, rewritten, err)
		}
		if c.RowAuthz == nil || *c.RowAuthz != want {
			t.Fatalf("codemod emitted %+v; loader read %+v", want, c.RowAuthz)
		}
	}
}

// Adding the annotation must not change the concept's JSON schema --
// the schema is what validation and filtering are built from, so a
// byte-identical schema is a direct statement that nothing downstream
// can behave differently because of the declaration.
func TestRowAuthzDoesNotChangeTheConceptSchema(t *testing.T) {
	plain, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("undeclared concept: %v", err)
	}
	for _, annotation := range []string{
		`@rowAuthz(public)`,
		`@rowAuthz(clusterOwner)`,
		`@rowAuthz(owner="ownerUserId")`,
		`@rowAuthz(via="spaceMember")`,
	} {
		declared, err := buildConcept(t, annotation+"\nconcept"+rowAuthzProbeBody)
		if err != nil {
			t.Fatalf("%s: %v", annotation, err)
		}
		for key, want := range plain.Schemas {
			got, ok := declared.Schemas[key]
			if !ok {
				t.Fatalf("%s: schema %q disappeared", annotation, key)
			}
			if string(got) != string(want) {
				t.Fatalf("%s: schema %q changed\n got: %s\nwant: %s", annotation, key, got, want)
			}
		}
		if len(declared.Schemas) != len(plain.Schemas) {
			t.Fatalf("%s: schema set changed size", annotation)
		}
		// Everything else the executor reads off a Concept must be
		// identical too.
		if declared.NodeType != plain.NodeType || declared.Version != plain.Version ||
			len(declared.Relationships) != len(plain.Relationships) {
			t.Fatalf("%s: concept metadata changed", annotation)
		}
	}
}

// The declaration must round-trip through the Concept's JSON form,
// because that is how it reaches the cockpit's concept surface.
func TestRowAuthzSurvivesConceptJSON(t *testing.T) {
	c, err := buildConcept(t, `@rowAuthz(owner="ownerUserId")`+"\nconcept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	blob, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Concept
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.RowAuthz == nil || *back.RowAuthz != *c.RowAuthz {
		t.Fatalf("RowAuthz did not survive JSON: %+v -> %+v", c.RowAuthz, back.RowAuthz)
	}
	// An undeclared concept must not grow an empty object in its JSON.
	plain, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	plainBlob, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plainBlob), "rowAuthz") {
		t.Fatalf("undeclared concept serialises a rowAuthz key: %s", plainBlob)
	}
}

// TestRowAuthzIsInert is the gate that keeps row-authz from enforcing
// anything before someone decides it should.
//
// TestRowAuthzIsInert is RETIRED here (memql#3076, Phase 3).
//
// It walked the Go tree and failed if any non-allow-listed file read
// Concept.RowAuthz, on the premise that Phase 1 was "vocabulary only -- no
// predicate is injected and no query result changes". That premise is no
// longer true: enforcement is live, the executor ANDs the declared tier into
// the filter, and the gate's own comment named this commit as its exit
// condition --
//
//	"The list GREW in #2921, and that growth is the mechanism working rather
//	 than the gate weakening ... Phase 3 lands the same way."
//
// Retired rather than extended, because extending it would have meant adding
// the enforcer to an allow-list whose whole claim is that nothing enforces.
//
// The tree is NOT left with nothing in its place, which was the condition on
// retiring it. TestRowAuthzShadowReport now fails on any measured access that
// is would-narrow OR undecidable -- a stronger statement than "who may read
// the field", because it constrains what enforcement is allowed to DO rather
// than who may look at the declaration. See the Phase 3 gate at the end of
// component/memql/rowauthz_shadow_report_test.go.
