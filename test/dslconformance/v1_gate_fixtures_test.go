package dslconformance

// v1_gate_fixtures_test.go -- a CATCH and a PASS fixture in the edition-2026
// forms for each gate in this package that reads expression text (epic
// memql#5363, task memql#5368). The corpus runs (bothCorpora) prove a gate
// still reads the migrated tree; these prove it still FAILS on it, which a
// clean corpus cannot.

import (
	"regexp"
	"strings"
	"testing"
)

const v1FixtureConcepts = `/// A person, in miniature.
concept user {
  displayName   string  @pii
  primaryEmail  string  @pii
  role          string
}
`

const v1FixtureShapes = `/// The actor envelope.
@actor
shape actorEnvelope {
  actor.userId
  actor.role
}

/// A PII-bearing card.
@row
shape user userFixtureCard {
  row.id
  displayName
  primaryEmail
}
`

const v1FixtureSpecs = `/// Caller is the fixture's operator.
spec actorEnvelope isFixtureOperator = actor => actor.role == "owner"
`

// v1FixtureQueries are person-scoped reads in the v1 forms. The name says what
// each gate must decide about it: Catch* must be flagged, Pass* must not.
const v1FixtureQueries = `/// Fixture.
query user catchById {
  args {
    userId  string
  }
  filter  row => row.id == args.userId
  shape   userFixtureCard
}

/// Operands reversed: the legacy regex reads id==args left to right only.
query user catchByIdReversed {
  args {
    userId  string
  }
  filter  row => args.userId == row.id
  shape   userFixtureCard
}

/// A guard is conditional: absent, it admits every row.
query user catchByIdGuarded {
  args {
    userId  string
  }
  filter  row => row.role == "member"
              && (args.userId == nil || row.id == args.userId)
  shape   userFixtureCard
}

/// One arm of the disjunction is the caller's; the other is anybody's.
query user catchByIdDisjunct {
  args {
    userId  string
  }
  filter  row => row.id == args.userId || row.id == actor.userId
  shape   userFixtureCard
}

/// Scoped to the caller.
query user passSelf {
  filter  row => row.id == actor.userId
  shape   userFixtureCard
}

/// Gated by a context-spec, which carries no ` + "`actor.`" + ` where it is used.
query user passByIdSpecGated {
  args {
    userId  string
  }
  filter  row => row.id == args.userId && isFixtureOperator(actor)
  shape   userFixtureCard
}
`

func v1FixtureCorpus() corpus {
	return newCorpus("v1-fixture", map[string]string{
		"fixture/concepts.memql": v1FixtureConcepts,
		"fixture/shapes.memql":   v1FixtureShapes,
		"fixture/specs.memql":    v1FixtureSpecs,
		"fixture/queries.memql":  v1FixtureQueries,
	})
}

// checkFixtureVerdicts asserts that exactly the catch* constructs of the
// fixture are among the findings.
func checkFixtureVerdicts(t *testing.T, gate string, findings []string) {
	t.Helper()
	joined := strings.Join(findings, "\n")
	for _, name := range []string{"catchById", "catchByIdReversed", "catchByIdGuarded", "catchByIdDisjunct", "passSelf", "passByIdSpecGated"} {
		flagged := strings.Contains(joined, ": "+name+" ")
		if want := strings.HasPrefix(name, "catch"); flagged != want {
			t.Errorf("%s: %s flagged = %v, want %v\nfindings:\n%s", gate, name, flagged, want, joined)
		}
	}
}

func TestCallerArgGateOnV1Fixtures(t *testing.T) {
	flagged, _, scanned := callerArgFindings(t, v1FixtureCorpus())
	if scanned != 6 {
		t.Fatalf("scanned %d fixture constructs, want 6", scanned)
	}
	// passSelf selects by the caller, not by an argument, so the caller-arg
	// gate does not reach it at all; the other pass fixture is reached and
	// cleared.
	checkFixtureVerdicts(t, "caller-arg selection", flagged)
}

func TestPiiProjectionGateOnV1Fixtures(t *testing.T) {
	flagged, _, scanned := piiProjectionFindings(t, v1FixtureCorpus())
	if scanned != 6 {
		t.Fatalf("scanned %d fixture constructs, want 6", scanned)
	}
	checkFixtureVerdicts(t, "PII projection", flagged)
}

func TestSpecDeclarationsAreReadInBothEditions(t *testing.T) {
	for _, src := range []string{
		"spec actorEnvelope isFixtureOperator {\n  return role == \"owner\"\n}\n",
		"spec actorEnvelope isFixtureOperator = actor => actor.role == \"owner\"\n",
	} {
		m := specDeclRe.FindStringSubmatch(src)
		if m == nil || m[1] != "actorEnvelope" || m[2] != "isFixtureOperator" {
			t.Errorf("specDeclRe did not read the declaration in %q: %v", src, m)
		}
	}
}

// TestConstructHeadStopsAtABraceLessDeclaration: the head of a construct is
// bounded by the construct above it, and a brace-less spec has no brace to
// stop at.
func TestConstructHeadStopsAtABraceLessDeclaration(t *testing.T) {
	src := "query user above {\n  shape x\n}\n\n" +
		"/// A spec whose own annotations must stay its own.\n" +
		"@requiresCapability(\"update\", \"principal\")\n" +
		"spec user isFoo = row => row.role == \"a\"\n" +
		"                   && row.displayName != \"\"\n\n" +
		"/// The construct under test.\n" +
		"query user below {\n  shape x\n}\n"
	if carriesAnnotationGate(src, "below") {
		t.Error("an annotation above a brace-less spec was read as the head of the query below it")
	}
	gated := strings.Replace(src, "/// The construct under test.\n", "/// The construct under test.\n@requiresRank(\"admin\")\n", 1)
	if !carriesAnnotationGate(gated, "below") {
		t.Error("the query's own annotation was not found in its head")
	}
}

// TestRowIdMirrorGateOnV1Fixtures: a v1 filter reads the self-mirror as
// `row.deploymentId`, which the gate's bare-name read cannot see.
func TestRowIdMirrorGateOnV1Fixtures(t *testing.T) {
	mutations := "/// Fixture.\nmutate deployment createDeploymentFixture {\n  args {\n    deploymentId  string\n  }\n" +
		"  insert {\n    id:           args.deploymentId\n    deploymentId: args.deploymentId\n  }\n}\n"
	query := func(name, filter string) string {
		return "/// Fixture.\nquery deployment " + name + " {\n  args {\n    deploymentId  string\n  }\n  filter  " +
			filter + "\n  shape   deploymentFull\n}\n"
	}
	c := newCorpus("v1-fixture", map[string]string{
		"fixture/mutations.memql": mutations,
		"fixture/queries.memql": query("catchReadsTheMirror", "row => row.deploymentId == args.deploymentId") +
			query("catchReadsTheMirrorReversed", "row => row.status != \"retired\"\n          && args.deploymentId == row.deploymentId") +
			query("passReadsTheRowId", "row => row.id == args.deploymentId"),
	})
	flagged, _, examined := rowIdMirrorFindings(t, c)
	if examined != 3 {
		t.Fatalf("examined %d fixture filters, want 3", examined)
	}
	joined := strings.Join(flagged, "\n")
	for _, name := range []string{"catchReadsTheMirror", "catchReadsTheMirrorReversed", "passReadsTheRowId"} {
		got := strings.Contains(joined, ": "+name+" ")
		if want := strings.HasPrefix(name, "catch"); got != want {
			t.Errorf("%s flagged = %v, want %v\nfindings:\n%s", name, got, want, joined)
		}
	}
}

// TestFilterPredicateWalkReadsV1Leaves: the walk TestFilterSyntaxCanonical and
// TestNoInlineTraitablePredicates share emits an edition-2026 clause's leaves,
// each on its own line, with the parameter root removed -- so a v1
// `row.payload.x` still reports the `payload` head and `row.active == true`
// still reads as the traitable comparison.
func TestFilterPredicateWalkReadsV1Leaves(t *testing.T) {
	src := "query gadget q {\n" +
		"  filter  row => row.payload.status == \"row.x\"\n" +
		"          && row.active == true\n" +
		"          && (args.x == nil || row.kind == args.x)\n" +
		"          && isActiveRecord(row)\n" +
		"  shape   gadgetFull\n" +
		"}\n"
	type emitted struct {
		line int
		pred string
	}
	var got []emitted
	walkFilterPredicates("fixture/queries.memql", src, func(_ string, line int, pred string) {
		got = append(got, emitted{line, pred})
	})
	want := []emitted{
		{2, `payload.status == "row.x"`}, // a string literal keeps its text
		{3, `active == true`},
		{4, `args.x == nil`},
		{4, `kind == args.x`},
		{5, `isActiveRecord(row)`},
	}
	if len(got) != len(want) {
		t.Fatalf("walk emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("predicate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// CATCH: the two gates' own rules fire on what the walk emitted.
	if head, _ := splitFilterRef(got[0].pred); head != "payload" {
		t.Errorf("TestFilterSyntaxCanonical would read head %q, want payload", head)
	}
	if !regexp.MustCompile(`\bactive\s*==\s*true\b`).MatchString(got[1].pred) {
		t.Errorf("TestNoInlineTraitablePredicates' isActiveRecord rule does not match %q", got[1].pred)
	}
	// PASS: a legitimate v1 predicate trips neither.
	if head, _ := splitFilterRef(got[3].pred); head == "payload" {
		t.Errorf("a plain payload read reported the payload head")
	}
}

// TestServerOnlyLocatorStopsAtABraceLessSpec: an annotation above a
// brace-less spec belongs to the spec, not to the next braced header below it.
func TestServerOnlyLocatorStopsAtABraceLessSpec(t *testing.T) {
	src := "@serverOnly\nspec user isFixtureSpec = row => row.role == \"a\"\n\nquery user below {\n  shape x\n}\n"
	if name, ok := constructNameAfter(src, 0); !ok || name != "isFixtureSpec" {
		t.Errorf("constructNameAfter = %q %v, want isFixtureSpec", name, ok)
	}
	if name, _ := constructNameAfter("@serverOnly\nspec user isFixtureSpec {\n  return role == \"a\"\n}\n", 0); name != "isFixtureSpec" {
		t.Errorf("the braced spec is no longer located: %q", name)
	}
}
