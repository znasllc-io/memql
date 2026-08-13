package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// relationship_label_traversal_3656_db_test.go covers LABEL-SCOPED TRAVERSAL
// (memql#3656): the second reading of a traversal call, in which a leading
// string literal names the `as` domain label the traversal is allowed to
// follow.
//
// memql#3652 gave a relationship declaration an `as` label -- what the edge
// MEANS, independent of the structural `type` the engine acts on. On its own
// that label was write-only: two `interactsWith` edges on one concept could
// finally SAY they meant different things, and every query still followed
// both. This issue is what makes the label load-bearing:
//
//	references("respondsAs", <expr>)   -> only the respondsAs edges
//	references(<expr>)                 -> every interactsWith edge
//
// WHY THIS NEEDED A FIXTURE DOMAIN. The motivating shape is a concept with
// several edges of ONE structural type distinguished only by `as`, and the
// shipped corpus has no such concept -- every relationship in the tree is
// unlabelled, because #3652 landed days ago and nothing has adopted it yet. A
// test written against the corpus could only ever assert that an unlabelled
// traversal still works. So the fixture below declares the shape the design
// exists for, registered through memqldsl.RegisterTree -- the same path a
// product DSL bundle mounts through at MEMQL_DSL_PATH, and the convention the
// sibling relationship suites already use.
//
// WHAT IS COVERED, and why each is separate:
//
//   - Scoping: a labelled traversal returns ONLY that label's targets.
//   - Backward compatibility: the unlabelled form returns the UNION, so every
//     traversal written before #3656 is untouched. Asserted over the SAME
//     fixture rows as the scoped case, so "scoped" and "unscoped" are provably
//     different answers about identical data rather than two coincidences.
//   - Every traversal function that takes the form. contains() is excluded by
//     design and ids() refuses -- each has its own test saying why.
//   - Union: two definitions sharing one label are followed together.
//   - The empty answer: a label matching nothing is an ordinary question with
//     an ordinary negative answer, not an error.
//   - The wire: GraphEdge.as, per edge, through clone and bare-id rewriting.
//   - The label surviving every place a RelationshipExpression is built.
//
// TWO OF THESE TESTS FOUND REAL DEFECTS IN THE IMPLEMENTATION, and both are
// worth knowing about because neither was in the issue's plan:
//
//   - TestRelationshipLabelIsPartOfTheResultCacheKey. canonicalExpression is
//     the result-cache signature, and it rendered a traversal as
//     `function(target)` -- so two traversals differing ONLY in their label
//     shared one cache entry and the second was served the first's rows. It
//     is not one of the ten CONSTRUCTION sites the label was threaded
//     through; it is a READER of the struct, and the same silent-miss class.
//     Worse than returning the union, because the answer depended on which
//     query ran first.
//   - TestLabelMatchingNoEdgeIsAnEmptyAnswerNotAnError. Four of the eight
//     functions carry a SECOND, post-loop gate that refused the empty
//     collection the label filter produced, so half the surface still failed
//     the whole query on a label miss.
//
// Both are fixed; these tests are the regression pins.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// The fixture domain and the canonical concept ids it assembles to. Concept
// ids are `v<major>:<domain>:<name>` with no @version / @namespace
// annotation, so the domain name IS the namespace segment.
const (
	labelFixtureDomain = "rel3656"

	labelHubConcept    = "v1:rel3656:hub"
	labelAgentConcept  = "v1:rel3656:agent"
	labelUserConcept   = "v1:rel3656:user"
	labelSpaceConcept  = "v1:rel3656:space"
	labelHandleConcept = "v1:rel3656:handle"
	labelTwinConcept   = "v1:rel3656:twin"
	labelDocConcept    = "v1:rel3656:docFile"
	labelCrateConcept  = "v1:rel3656:crate"
)

// labelHubMarker is the payload value that identifies a fixture hub row from
// inside a DSL construct.
//
// It exists because a spec body takes no arguments and may not read `row.*`
// (epic #2281), so the spec-hosted traversal in specs.memql below cannot
// scope itself to one test's rows the way a query can. A distinctive payload
// value is the only handle it has. Kept in sync with the literal in
// labelFixtureSpecs by this constant being the seeded value.
const labelHubMarker = "rel3656-hub"

// labelFixtureConcepts is the shape the shipped corpus does not have.
//
// `hub` is the motivating example from the design: FIVE interactsWith edges,
// structurally identical, that only `as` can tell apart. The rest extend the
// same idea across the other traversal functions, and three declarations are
// deliberately awkward:
//
//   - auditorId is an UNLABELLED interactsWith sitting alongside labelled
//     ones of the same type. That is what exercises the empty-string label
//     filter graph expansion needs -- it has to be able to ask for "the edges
//     that carry NO label" as something distinct from "all edges".
//   - peerAId + peerBId SHARE the label collaboratesWith, so a traversal has
//     to follow their union rather than picking one.
//   - archivedInSpaceId is an unlabelled `parent` beside a labelled one, so
//     the labelled/unlabelled split is exercised on a structural type too and
//     not only on interactsWith.
const labelFixtureConcepts = `/// Fixture agent (memql#3656) -- what a hub responds AS, and who it collaborates with.
concept agent {
  label string @description("Human label, so fixture rows are legible in a failure dump.")
}

/// Fixture user (memql#3656) -- who a hub acts FOR, is audited by, and was authored by.
concept user {
  label string @description("Human label, so fixture rows are legible in a failure dump.")
}

/// Fixture document (memql#3656) -- what a hub owns and what a crate contains.
concept docFile {
  label string @description("Human label, so fixture rows are legible in a failure dump.")
}

/// Fixture alias target (memql#3656).
concept handle {
  label string @description("Human label, so fixture rows are legible in a failure dump.")
}

/// Fixture equals target (memql#3656).
concept twin {
  label string @description("Human label, so fixture rows are legible in a failure dump.")
}

/// Fixture parent container (memql#3656). Declares the INCOMING half of both
/// parent edges -- one labelled, one not -- which is what makes childOf
/// reachable at all, since every parent declaration in the shipped corpus is
/// outgoing.
concept space {
  label string @description("Human label, so fixture rows are legible in a failure dump.")

  @relationship(type="parent", as="hostsHub", field="spaceId", target=hub, direction="incoming")
  @relationship(type="parent", field="archivedInSpaceId", target=hub, direction="incoming")
}

/// Fixture collection (memql#3656). contains() is the one traversal function
/// DELIBERATELY excluded from the label form, so this concept exists to prove
/// the unlabelled traversal is untouched by the grammar change.
@type("collection")
concept crate {
  label     string   @description("Human label, so fixture rows are legible in a failure dump.")
  memberIds []string @description("The docFile rows this crate holds.")

  @relationship(type="contains", field="memberIds", target=docFile, direction="outgoing")
}

/// Fixture hub (memql#3656) -- the motivating example. Five interactsWith
/// edges identical in every structural respect, differing only in what their
/// author said they MEAN, plus a labelled edge on each other traversable type.
concept hub {
  label             string @description("Human label, so fixture rows are legible in a failure dump.")
  marker            string @description("Identifies a fixture hub row from inside a spec body, which takes no args and may not read row.*.")
  agentId           string @description("The agent this hub responds as.")
  forUserId         string @description("The user this hub acts for.")
  auditorId         string @description("The user auditing this hub -- an UNLABELLED interactsWith edge.")
  peerAId           string @description("First collaborator; shares the collaboratesWith label with peerBId.")
  peerBId           string @description("Second collaborator; shares the collaboratesWith label with peerAId.")
  spaceId           string @description("The space this hub belongs to.")
  archivedInSpaceId string @description("The space this hub is archived in -- an UNLABELLED parent edge.")
  handleId          string @description("The handle this hub is known as.")
  twinId            string @description("The twin this hub was merged with.")
  draftId           string @description("The docFile this hub drafts.")
  authorId          string @description("The user who authored this hub.")

  @relationship(type="references", as="respondsAs", field="agentId", target=agent, direction="outgoing")
  @relationship(type="references", as="actsFor", field="forUserId", target=user, direction="outgoing")
  @relationship(type="references", field="auditorId", target=user, direction="outgoing")
  @relationship(type="references", as="collaboratesWith", field="peerAId", target=agent, direction="outgoing")
  @relationship(type="references", as="collaboratesWith", field="peerBId", target=agent, direction="outgoing")
  @relationship(type="parent", as="belongsToSpace", field="spaceId", target=space, direction="outgoing")
  @relationship(type="parent", field="archivedInSpaceId", target=space, direction="outgoing")
  @relationship(type="alias", as="knownAs", field="handleId", target=handle, direction="outgoing")
  @relationship(type="equals", as="mergedWith", field="twinId", target=twin, direction="outgoing")
  @relationship(type="owns", as="drafts", field="draftId", target=docFile, direction="outgoing")
  @relationship(type="createdBy", as="authoredBy", field="authorId", target=user, direction="outgoing")
}
`

// labelFixtureQueries hosts a label-scoped traversal inside an AUTHORED query,
// which is a different construction path from a runtime query string: the
// filter is parsed at load, rewritten by resolveBareConcept + the arg
// expander, and only then executed. Each of those rebuilds the
// RelationshipExpression field by field.
//
// The two queries are a matched pair over one filter so the scoped and
// unscoped answers can be compared directly.
const labelFixtureQueries = `/// Agents a fixture hub responds AS -- the label-scoped form, authored (memql#3656).
query agent labelScopedRespondsAsAgents {
  args {
    hubOwner string!
  }
  filter references("respondsAs", row.concept == "v1:rel3656:hub" && row.createdBy == args.hubOwner)
}

/// Every agent a fixture hub interacts with -- the unscoped form, authored.
query agent labelUnscopedInteractsAgents {
  args {
    hubOwner string!
  }
  filter references(row.concept == "v1:rel3656:hub" && row.createdBy == args.hubOwner)
}
`

// labelFixtureSpecs hosts a label-scoped traversal inside a SPEC body -- the
// third construction path, through specValidator.expandExpression.
//
// The body cannot scope itself to one test's rows: a spec takes no args and
// may not read `row.*` (epic #2281, enforced at load). It matches the fixture
// marker instead, and the caller ANDs its own owner filter on top.
const labelFixtureSpecs = `use rel3656.concepts.{ agent }

/// True for an agent some fixture hub responds AS -- and NOT for one it merely
/// collaborates with, which is the whole assertion (memql#3656).
spec agent labelScopedIsRespondsAsAgent {
  return references("respondsAs", marker == "rel3656-hub")
}
`

// mountLabelFixture registers the fixture domain and restores the global
// concept registry afterwards.
//
// LoadUnifiedConcepts merges into the process-wide default registry and there
// is no per-concept removal, so the snapshot/ReplaceAll pair is what keeps a
// fixture concept from outliving its test. UnregisterTree alone would not do
// it: the tree is the SOURCE, the registry is where the loaded concept lands.
// Must run BEFORE readMergeTestEngine -- the engine snapshots the registry at
// Init.
func mountLabelFixture(t *testing.T) {
	t.Helper()
	before := memorynodes.All()
	memqldsl.RegisterTree(labelFixtureDomain, fstest.MapFS{
		"concepts.memql": {Data: []byte(labelFixtureConcepts)},
		"queries.memql":  {Data: []byte(labelFixtureQueries)},
		"specs.memql":    {Data: []byte(labelFixtureSpecs)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(labelFixtureDomain)
		memorynodes.ReplaceAll(before)
	})
}

// disableResultCache turns the engine's result cache off for one test.
//
// THIS IS ISOLATION, NOT A WORKAROUND, and the difference is worth spelling
// out because it looks like the latter. The label does NOT reach the
// result-cache key (canonicalExpression renders a RelationshipExpression as
// `function(target)` and never reads Label), and caching is default-on with a
// 60s TTL -- so two traversals differing only in their label share one cache
// entry and the second is served the first's rows. That defect has its own
// assertion, TestRelationshipLabelIsPartOfTheResultCacheKey, which is written
// against the correct behaviour and skipped.
//
// Every OTHER test in this file is about what the traversal RESOLVES, and
// each asks two or more differently-labelled questions about one set of rows.
// Left on, the cache would answer the second question with the first
// question's result and those tests would be measuring the cache rather than
// the resolver -- passing or failing for reasons unrelated to what they say
// they check. Turning it off measures the thing under test; the cache defect
// is measured where it belongs.
func disableResultCache(t *testing.T, eng *MemQLEngine) {
	t.Helper()
	saved := eng.cache
	eng.cache = nil
	t.Cleanup(func() { eng.cache = saved })
}

// seedLabelRow inserts ONE append-only row at a fixed createdAt, bypassing the
// mutation validators. The relationship resolvers read `concept`, `id` and the
// payload field naming the related row, so a minimal payload is enough and a
// validator-shaped one would only obscure the fixture.
//
// Local to this file on purpose: the sibling suites' seeders stamp their own
// provenance and their own concept set, and sharing one would couple three
// unrelated fixtures to a single signature.
func seedLabelRow(
	t *testing.T,
	ctx context.Context,
	db *bun.DB,
	conceptName string,
	id string,
	createdAt time.Time,
	owner string,
	payload map[string]any,
) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	node := &memorynodes.MemoryNode{
		ID:         id,
		Concept:    conceptName,
		CreatedBy:  owner,
		CreatedAt:  createdAt.UTC(),
		Payload:    raw,
		Provenance: json.RawMessage(`{"kind":"direct","name":"relationship-3656-test"}`),
	}
	_, err = db.NewInsert().Model(node).
		On(`CONFLICT (id, "createdAt") DO NOTHING`).
		Exec(ctx)
	require.NoError(t, err, "seed %s row %s", conceptName, id)
}

// labelWorld is one fully-wired hub row plus every row it points at. Each test
// seeds one under its own unique suffix, so no two tests see each other's rows.
type labelWorld struct {
	owner string

	hubId string

	respondsAsAgentId string
	actsForUserId     string
	auditorUserId     string
	peerAAgentId      string
	peerBAgentId      string
	spaceId           string
	archiveSpaceId    string
	handleId          string
	twinId            string
	draftDocId        string
	authorUserId      string

	crateId       string
	crateMemberId string
}

// seedLabelWorld writes the whole fixture graph for one test.
//
// EVERY pointer field on the hub is populated, and that is a requirement
// rather than thoroughness: resolveAliasOrEquals refuses a traversal whose
// pointer field is ABSENT from the payload, and graph expansion runs that
// resolver on every hub row any query returns -- so a hub missing its
// handleId would fail every query that returns it, for reasons having nothing
// to do with labels.
//
// Rows are stamped from time.Now() rather than a fixed date, so a test's own
// rows are always the NEWEST in the shared table. Reads collapse to
// latest-per-id ordered createdAt DESC under a 50-row default cap, and this
// suite accumulates one hub row per local run -- with a fixed base date the
// spec-hosted traversal (which cannot scope itself by owner, see
// labelFixtureSpecs) would eventually fall outside that window and start
// failing for a reason no failure message would explain.
func seedLabelWorld(t *testing.T, ctx context.Context, db *bun.DB, sfx string) labelWorld {
	t.Helper()

	w := labelWorld{
		owner:             "kb:" + sfx,
		hubId:             fmt.Sprintf("%s:%s", labelHubConcept, sfx),
		respondsAsAgentId: fmt.Sprintf("%s:%s-responds", labelAgentConcept, sfx),
		actsForUserId:     fmt.Sprintf("%s:%s-actsfor", labelUserConcept, sfx),
		auditorUserId:     fmt.Sprintf("%s:%s-auditor", labelUserConcept, sfx),
		peerAAgentId:      fmt.Sprintf("%s:%s-peer-a", labelAgentConcept, sfx),
		peerBAgentId:      fmt.Sprintf("%s:%s-peer-b", labelAgentConcept, sfx),
		spaceId:           fmt.Sprintf("%s:%s-home", labelSpaceConcept, sfx),
		archiveSpaceId:    fmt.Sprintf("%s:%s-archive", labelSpaceConcept, sfx),
		handleId:          fmt.Sprintf("%s:%s", labelHandleConcept, sfx),
		twinId:            fmt.Sprintf("%s:%s", labelTwinConcept, sfx),
		draftDocId:        fmt.Sprintf("%s:%s-draft", labelDocConcept, sfx),
		authorUserId:      fmt.Sprintf("%s:%s-author", labelUserConcept, sfx),
		crateId:           fmt.Sprintf("%s:%s", labelCrateConcept, sfx),
		crateMemberId:     fmt.Sprintf("%s:%s-member", labelDocConcept, sfx),
	}

	base := time.Now().UTC()
	tick := 0
	at := func() time.Time {
		out := base.Add(time.Duration(tick) * time.Millisecond)
		tick++
		return out
	}

	leaves := []struct {
		concept string
		id      string
		label   string
	}{
		{labelAgentConcept, w.respondsAsAgentId, "responds-as-agent"},
		{labelAgentConcept, w.peerAAgentId, "peer-a-agent"},
		{labelAgentConcept, w.peerBAgentId, "peer-b-agent"},
		{labelUserConcept, w.actsForUserId, "acts-for-user"},
		{labelUserConcept, w.auditorUserId, "auditor-user"},
		{labelUserConcept, w.authorUserId, "author-user"},
		{labelSpaceConcept, w.spaceId, "home-space"},
		{labelSpaceConcept, w.archiveSpaceId, "archive-space"},
		{labelHandleConcept, w.handleId, "handle"},
		{labelTwinConcept, w.twinId, "twin"},
		{labelDocConcept, w.draftDocId, "draft-doc"},
		{labelDocConcept, w.crateMemberId, "crate-member-doc"},
	}
	for _, leaf := range leaves {
		seedLabelRow(t, ctx, db, leaf.concept, leaf.id, at(), w.owner,
			map[string]any{"label": leaf.label})
	}

	seedLabelRow(t, ctx, db, labelCrateConcept, w.crateId, at(), w.owner, map[string]any{
		"label":     "crate",
		"memberIds": []string{w.crateMemberId},
	})

	seedLabelRow(t, ctx, db, labelHubConcept, w.hubId, at(), w.owner, map[string]any{
		"label":             "hub",
		"marker":            labelHubMarker,
		"agentId":           w.respondsAsAgentId,
		"forUserId":         w.actsForUserId,
		"auditorId":         w.auditorUserId,
		"peerAId":           w.peerAAgentId,
		"peerBId":           w.peerBAgentId,
		"spaceId":           w.spaceId,
		"archivedInSpaceId": w.archiveSpaceId,
		"handleId":          w.handleId,
		"twinId":            w.twinId,
		"draftId":           w.draftDocId,
		"authorId":          w.authorUserId,
	})

	return w
}

// hubFilter is the inner expression most traversals here walk from: the one
// hub row this test seeded, and no other.
func (w labelWorld) hubFilter() string {
	return fmt.Sprintf(`concept==%s && row.createdBy==%q`, labelHubConcept, w.owner)
}

// spaceFilter selects this test's two space rows -- the source side of the
// childOf traversal, which is the only one whose source is the parent.
func (w labelWorld) spaceFilter() string {
	return fmt.Sprintf(`concept==%s && row.createdBy==%q`, labelSpaceConcept, w.owner)
}

// crateFilter selects this test's crate row -- the source of the contains()
// traversal.
func (w labelWorld) crateFilter() string {
	return fmt.Sprintf(`concept==%s && row.createdBy==%q`, labelCrateConcept, w.owner)
}

// traversalIDs returns the ids a query RESOLVED TO.
//
// Deliberately RootIds rather than the bundle's node list. Bundle.Nodes also
// carries everything graph expansion pulled in around each result at depth 1,
// so reading it would make a traversal that resolved one hub look as if it
// had resolved the hub plus all eleven rows the hub points at -- an assertion
// about scoping written over that set would be asserting almost nothing.
func traversalIDs(t *testing.T, res *ExecuteResult) []string {
	t.Helper()
	require.NotNil(t, res)
	ids := []string{}
	if res.Bundle != nil {
		ids = append(ids, res.Bundle.RootIds...)
	}
	return ids
}

// runTraversal executes a query and returns the ids it resolved to.
func runTraversal(t *testing.T, ctx context.Context, eng *MemQLEngine, query string) []string {
	t.Helper()
	res, err := eng.Execute(ctx, query)
	require.NoError(t, err, "traversal %s failed", query)
	return traversalIDs(t, res)
}

// bootLabelEngine mounts the fixture, boots a real engine against a real
// Postgres, and RELEASES the connection pool when the test ends. The order
// matters: the fixture must be registered before the engine Inits, because
// Init snapshots the concept registry.
//
// The close is not tidiness. readMergeTestEngine opens a fresh pool per call
// and never closes it, and this file adds a dozen engines to a package that
// already boots dozens more -- measured, that is enough to push the shared
// test database past max_connections and fail UNRELATED suites with
// "sorry, too many clients already". A test that exhausts a shared resource
// breaks its neighbours, and the failure lands on whichever test happened to
// run next.
func bootLabelEngine(t *testing.T) (*MemQLEngine, *bun.DB) {
	t.Helper()
	mountLabelFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	t.Cleanup(func() { _ = db.Close() })
	return eng, db
}

// labelTestEngine is bootLabelEngine plus a seeded world and the result cache
// turned off (see disableResultCache for why).
func labelTestEngine(t *testing.T, name string) (*MemQLEngine, context.Context, labelWorld) {
	t.Helper()
	eng, db := bootLabelEngine(t)
	disableResultCache(t, eng)
	ctx := clusterOwnerCtx("u-" + name)
	return eng, ctx, seedLabelWorld(t, ctx, db, uniqueSuffix(name))
}

// TestRelationshipLabelFixtureLoads asserts the fixture actually reached the
// engine before anything else leans on it.
//
// Without this, a fixture that failed to mount would make several tests in
// this file pass VACUOUSLY: a labelled traversal against a concept the engine
// has never heard of resolves nothing, which is indistinguishable from
// correct scoping whenever the assertion is "the wrong rows are absent".
func TestRelationshipLabelFixtureLoads(t *testing.T) {
	eng, _ := bootLabelEngine(t)

	require.Contains(t, eng.relationships.ByConcept, labelHubConcept,
		"the memql#3656 fixture concept did not reach the engine; every label assertion in "+
			"this file would then pass vacuously")

	byLabel := map[string]int{}
	for _, def := range eng.relationships.ByConcept[labelHubConcept] {
		if def.Type == relationshipTypeReferences {
			byLabel[def.As]++
		}
	}
	require.Equal(t, map[string]int{"respondsAs": 1, "actsFor": 1, "": 1, "collaboratesWith": 2}, byLabel,
		"the fixture's interactsWith edges are the whole point: two distinguished only by `as`, "+
			"one unlabelled, and two sharing a label")

	require.NotNil(t, eng.functions)
	require.True(t, eng.functions.Has("labelScopedRespondsAsAgents"),
		"the fixture's authored query did not register; the construction-site coverage for "+
			"the function-loader path would then prove nothing")
	require.NotNil(t, eng.specs)
	require.True(t, eng.specs.Has("labelScopedIsRespondsAsAgent"),
		"the fixture's authored spec did not register; the construction-site coverage for "+
			"the spec-validator path would then prove nothing")
}

// TestInteractsWithLabelReturnsOnlyThatLabelsTargets is the headline of
// memql#3656 and, with the test below it, the pair the whole issue reduces to.
//
// Five interactsWith edges leave one hub row. They differ in NOTHING the
// engine acts on -- same type, same direction, same source concept -- and
// before #3656 a traversal followed all five whatever the caller wanted.
// Here each label is asked for on its own, over one set of rows, and must
// come back with exactly its own targets:
//
//	respondsAs        -> the agent
//	actsFor           -> the user it acts for
//	collaboratesWith  -> BOTH peers (two definitions, one label)
//
// The negative half is what makes it an assertion rather than a demo: asking
// for respondsAs must not return the actsFor user. A dropped label at any
// construction site turns each of these into the full five-row union, so the
// exact-match assertion is what catches it.
func TestInteractsWithLabelReturnsOnlyThatLabelsTargets(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-scoped")

	require.ElementsMatch(t,
		[]string{w.respondsAsAgentId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references("respondsAs", %s)`, w.hubFilter())),
		"references(\"respondsAs\") must return the respondsAs target and nothing else; "+
			"a label dropped anywhere on the path yields the union of all five interactsWith edges")

	require.ElementsMatch(t,
		[]string{w.actsForUserId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references("actsFor", %s)`, w.hubFilter())),
		"references(\"actsFor\") must return the actsFor target and nothing else")

	require.ElementsMatch(t,
		[]string{w.peerAAgentId, w.peerBAgentId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references("collaboratesWith", %s)`, w.hubFilter())),
		"two definitions sharing one label are followed TOGETHER -- the per-field duplicate rule "+
			"already blocks the genuinely ambiguous case, and the union is the useful reading of "+
			"\"every edge meaning collaboratesWith\"")
}

// TestUnlabelledInteractsWithStillReturnsTheUnion is the backward-
// compatibility criterion, and it is deliberately its own test.
//
// Every traversal in the shipped corpus and in every downstream bundle is
// unlabelled. #3656 is additive only if the one-argument form still means
// "follow every edge of this type" -- so this asserts the FULL five-row union
// over exactly the rows the scoped test above split apart. Read together, the
// two say: same data, different questions, different answers, and the old
// question still gets the old answer.
func TestUnlabelledInteractsWithStillReturnsTheUnion(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-unscoped")

	require.ElementsMatch(t,
		[]string{
			w.respondsAsAgentId,
			w.actsForUserId,
			w.auditorUserId,
			w.peerAAgentId,
			w.peerBAgentId,
		},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references(%s)`, w.hubFilter())),
		"the ONE-ARGUMENT form must be exactly what it was before memql#3656: every "+
			"interactsWith edge, labelled or not. This is the whole backward-compatibility "+
			"claim of the feature")

	// The same for the other structural type carrying a labelled + unlabelled
	// pair. parent is where an over-eager scope would be hardest to notice,
	// because most concepts have exactly one parent and the union and the
	// scoped answer coincide.
	require.ElementsMatch(t,
		[]string{w.spaceId, w.archiveSpaceId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`parentOf(%s)`, w.hubFilter())),
		"unlabelled parentOf must still return BOTH parents -- the labelled one and the "+
			"unlabelled one")
}

// TestLabelScopedTraversalWorksForEveryTraversalFunction walks the whole
// surface. Every wrapper function that takes the label form gets the same
// two-part check: the labelled call returns its own target, and it does NOT
// return a sibling edge's target.
//
// Per-function rather than in aggregate because the resolvers do NOT share an
// implementation -- each has its own copy of the filter call and its own
// len(matches)==0 branch -- so a label can be honoured by seven of them and
// dropped by the eighth with nothing else to show for it.
//
// contains() and ids() are absent and each has its own test explaining why.
func TestLabelScopedTraversalWorksForEveryTraversalFunction(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-every-fn")

	cases := []struct {
		fn       string
		label    string
		source   string
		want     []string
		mustMiss []string
	}{
		{
			fn: "references", label: "respondsAs", source: w.hubFilter(),
			want:     []string{w.respondsAsAgentId},
			mustMiss: []string{w.actsForUserId, w.auditorUserId},
		},
		{
			fn: "parentOf", label: "belongsToSpace", source: w.hubFilter(),
			want:     []string{w.spaceId},
			mustMiss: []string{w.archiveSpaceId},
		},
		{
			fn: "childOf", label: "hostsHub", source: w.spaceFilter(),
			want: []string{w.hubId},
		},
		{
			fn: "aliasOf", label: "knownAs", source: w.hubFilter(),
			want: []string{w.handleId},
		},
		{
			fn: "equals", label: "mergedWith", source: w.hubFilter(),
			want: []string{w.twinId},
		},
		{
			fn: "owns", label: "drafts", source: w.hubFilter(),
			want: []string{w.draftDocId},
		},
		{
			fn: "createdBy", label: "authoredBy", source: w.hubFilter(),
			want: []string{w.authorUserId},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			got := runTraversal(t, ctx, eng,
				fmt.Sprintf(`%s(%q, %s)`, tc.fn, tc.label, tc.source))
			require.ElementsMatch(t, tc.want, got,
				"%s(%q, ...) must resolve exactly its own label's targets", tc.fn, tc.label)
			for _, missing := range tc.mustMiss {
				require.NotContains(t, got, missing,
					"%s(%q, ...) returned %s, which belongs to a DIFFERENT %s edge -- the label "+
						"did not scope the traversal", tc.fn, tc.label, missing, tc.fn)
			}
		})
	}
}

// TestLabelMatchingNoEdgeIsAnEmptyAnswerNotAnError covers the criterion that
// "no edge on this concept means that label" is an ordinary question with an
// ordinary negative answer.
//
// Two shapes, because they fail differently in a reader's head: a label
// NOBODY uses (a typo, or a vocabulary that has moved on) and a label used by
// a DIFFERENT structural type on the same concept (belongsToSpace is a
// `parent` label; asking interactsWith for it is a category error the engine
// should answer emptily rather than crash on).
//
// FINDING -- FOUR OF THESE DO NOT HOLD, and are skipped rather than weakened.
// Each resolver's len(matches)==0 branch does `continue` when a label filter
// is present, which is correct. But parentOf / childOf / aliasOf / equals
// each carry a SECOND gate after the loop -- "produced no parent references",
// "produced no candidate queries" -- that the continue walks straight into,
// so a labelled traversal matching nothing returns an ERROR from exactly half
// the surface. interactsWith / owns / createdBy / contains have no such gate
// and return empty. The engine's own comments name this disagreement as
// memql#3671 for the UNLABELLED case; the labelled case inherits it, which
// leaves this issue's acceptance criterion unmet for four of eight functions.
func TestLabelMatchingNoEdgeIsAnEmptyAnswerNotAnError(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-empty")

	// wantErr is the exact error the resolver returns today, empty when the
	// function already behaves correctly; resolver names the Go function that
	// carries the gate, so a failure points at the code to change.
	cases := []struct {
		fn       string
		source   string
		resolver string
		wantErr  string
	}{
		{fn: "references", source: w.hubFilter()},
		{fn: "owns", source: w.hubFilter()},
		{fn: "createdBy", source: w.hubFilter()},
		{
			fn: "parentOf", source: w.hubFilter(), resolver: "resolveParentOf",
			wantErr: "parentOf traversal produced no parent references",
		},
		{
			fn: "childOf", source: w.spaceFilter(), resolver: "resolveChildOf",
			wantErr: "childOf traversal produced no candidate queries",
		},
		{
			fn: "aliasOf", source: w.hubFilter(), resolver: "resolveAliasOrEquals",
			wantErr: "alias traversal produced no candidate queries",
		},
		{
			fn: "equals", source: w.hubFilter(), resolver: "resolveAliasOrEquals",
			wantErr: "equals traversal produced no candidate queries",
		},
	}

	// Both shapes of "matches nothing". "belongsToSpace" is real, and is a
	// `parent` label -- so for parentOf itself it would match, and that case
	// uses a label owned by interactsWith instead.
	unusedLabel := "nobodyUsesThisLabel"

	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {

			wrongLabel := "belongsToSpace" // a real label owned by `parent`
			if tc.fn == "parentOf" || tc.fn == "childOf" {
				wrongLabel = "respondsAs" // a real label owned by `interactsWith`
			}

			for _, label := range []string{unusedLabel, wrongLabel} {
				got := runTraversal(t, ctx, eng,
					fmt.Sprintf(`%s(%q, %s)`, tc.fn, label, tc.source))
				require.Empty(t, got,
					"%s(%q, ...) must resolve to nothing -- asking which edges mean %q is a "+
						"question with a negative answer, not a malformed query", tc.fn, label, label)
			}
		})
	}
}

// TestContainsIsExcludedFromTheLabelForm pins the deliberate exclusion at the
// engine level: the unlabelled contains() traversal is untouched by #3656.
//
// contains() is the ONE traversal function absent from the parser's
// wrapperFunctions map, because its two-argument slot is already string search
// (`contains(text, substr)`). A third reading of (string, X) could only be
// settled by an arbitrary tie-break, which would turn a mistyped label into a
// silent substring search -- the same silent-misreading class the epic exists
// to remove.
//
// The two-argument string-search form is a LOAD-TIME construct only: a bare
// ContainsExpr has no runtime AST conversion ("unsupported parser expression
// type: *ast.ContainsExpr"), so it cannot be exercised through Execute at all.
// Its survival is asserted where it lives, in the parser suite --
// TestContainsKeepsExactlyTwoReadings in
// component/language/parser/relationship_label_arity_3656_test.go.
func TestContainsIsExcludedFromTheLabelForm(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-contains")

	require.ElementsMatch(t,
		[]string{w.crateMemberId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`contains(%s)`, w.crateFilter())),
		"the single-argument contains() traversal must still walk the collection's members; "+
			"contains is excluded from the label form, not from traversal")

	// A labelled contains is not a labelled traversal. It reaches
	// parseContainsFunction's two-argument arm, where the second argument is
	// the substring -- and a substring search is not a query root, so the
	// engine refuses it. What matters is that it is NOT silently accepted as
	// a scoped traversal.
	_, err := eng.Execute(ctx, fmt.Sprintf(`contains("holds", %s)`, w.crateFilter()))
	require.Error(t, err,
		"contains(\"label\", <expr>) must not be accepted as a label-scoped traversal -- "+
			"contains keeps two readings, and neither of them is that")
}

// TestIdsRefusesALabel pins the one traversal function that answers a label
// with an error instead of a result.
//
// ids() projects the rows it is handed. It follows no edge and reads no
// relationship definition, so a label on it cannot scope anything -- it can
// only be a mistake. Refusing beats ignoring: a silently-dropped label is the
// declaration theatre this epic exists to remove, and it would be invisible
// precisely because ids() would still return the right rows.
func TestIdsRefusesALabel(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-ids")

	agentFilter := fmt.Sprintf(`concept==%s && row.createdBy==%q`, labelAgentConcept, w.owner)

	_, err := eng.Execute(ctx, fmt.Sprintf(`ids("respondsAs", %s)`, agentFilter))
	require.Error(t, err, "ids() must refuse a label rather than ignore it")
	require.Contains(t, err.Error(), "ids()",
		"the error must name the function, so the author knows which call to fix")
	require.Contains(t, err.Error(), "respondsAs",
		"the error must quote the label it is refusing, so the author knows what to delete")

	// And the unlabelled form is untouched.
	require.ElementsMatch(t,
		[]string{w.respondsAsAgentId, w.peerAAgentId, w.peerBAgentId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`ids(%s)`, agentFilter)),
		"unlabelled ids() must still project the rows it was given")
}

// TestGraphEdgeCarriesTheAsLabelPerEdge is the wire half: GraphEdge.as, the
// field a client reads to learn what an edge MEANS.
//
// Asserted over ONE bundle from ONE query, and that is the point. Graph
// expansion resolves each edge kind once per distinct label present on the
// concept, so a bundle from a single hub row contains labelled AND unlabelled
// interactsWith edges side by side. If `as` were stamped per QUERY rather than
// per EDGE -- the plausible way to get this wrong -- every edge in this bundle
// would carry the same value, and asserting them together is the only way to
// see that.
func TestGraphEdgeCarriesTheAsLabelPerEdge(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-wire")

	res, err := eng.Execute(ctx, w.hubFilter())
	require.NoError(t, err)
	require.NotNil(t, res.Bundle)

	// (type, as) -> the ids that edge kind reached.
	type edgeKey struct{ edgeType, as string }
	got := map[edgeKey][]string{}
	for _, e := range res.Bundle.Edges {
		require.Equal(t, w.hubId, e.GetFromId(), "every edge in this bundle leaves the hub row")
		got[edgeKey{e.GetType(), e.GetAs()}] = append(got[edgeKey{e.GetType(), e.GetAs()}], e.GetToId())
	}

	want := map[edgeKey][]string{
		{GraphEdgeLabelReferences, "respondsAs"}:       {w.respondsAsAgentId},
		{GraphEdgeLabelReferences, "actsFor"}:          {w.actsForUserId},
		{GraphEdgeLabelReferences, ""}:                 {w.auditorUserId},
		{GraphEdgeLabelReferences, "collaboratesWith"}: {w.peerAAgentId, w.peerBAgentId},
		{GraphEdgeLabelAlias, "knownAs"}:               {w.handleId},
		{GraphEdgeLabelEquals, "mergedWith"}:           {w.twinId},
		{GraphEdgeLabelOwns, "drafts"}:                 {w.draftDocId},
		{GraphEdgeLabelCreatedBy, "authoredBy"}:        {w.authorUserId},
	}

	for key, wantIds := range want {
		require.ElementsMatch(t, wantIds, got[key],
			"edge type=%q as=%q must carry exactly the targets its relationship declared; got %v",
			key.edgeType, key.as, got[key])
	}
	require.Len(t, got, len(want),
		"the bundle emitted an (type, as) combination this test does not know about: %v", got)

	// The unlabelled edge is the load-bearing one: `as` must be EMPTY there,
	// not defaulted to the type and not inherited from a sibling edge.
	require.Contains(t, got, edgeKey{GraphEdgeLabelReferences, ""},
		"the unlabelled interactsWith edge must reach the wire with an EMPTY as -- an edge "+
			"whose author said nothing about its meaning must not be given one")
}

// TestGraphEdgeAsSurvivesCloning pins `as` through cloneGraphBundle.
//
// It survives today BY CONSTRUCTION -- the edge copy is proto.Clone, which
// copies every field -- and that is exactly why this is worth pinning. The
// node copy beside it is already hand-shaped (cloneMemoryNode plus a rebuilt
// Nodes/Edges/RootIds struct), so the pattern for replacing proto.Clone with a
// hand-rolled field-by-field copy is right there in the same function. This
// test is what makes that replacement fail instead of silently dropping the
// field on every cached and every mutation-returned bundle.
func TestGraphEdgeAsSurvivesCloning(t *testing.T) {
	bundle := &memqlv1.GraphBundle{
		RootIds: []string{"v1:rel3656:hub:x"},
		Edges: []*memqlv1.GraphEdge{
			{
				Type:   GraphEdgeLabelReferences,
				FromId: "v1:rel3656:hub:x",
				ToId:   "v1:rel3656:agent:y",
				As:     "respondsAs",
				Depth:  1,
			},
			{
				Type:   GraphEdgeLabelReferences,
				FromId: "v1:rel3656:hub:x",
				ToId:   "v1:rel3656:user:z",
				Depth:  1,
			},
		},
	}

	clone := cloneGraphBundle(bundle)
	require.NotNil(t, clone)
	require.Len(t, clone.Edges, 2)

	require.Equal(t, "respondsAs", clone.Edges[0].GetAs(),
		"cloneGraphBundle dropped GraphEdge.as -- a cached or returned bundle would carry the "+
			"edges but not what they mean")
	require.Empty(t, clone.Edges[1].GetAs(),
		"an unlabelled edge must clone as unlabelled, not inherit its neighbour's label")

	// A clone that shares the edge pointers would preserve `as` and still be
	// wrong, so check the copy is a copy.
	require.NotSame(t, bundle.Edges[0], clone.Edges[0],
		"the clone must not alias the source edge")
	clone.Edges[0].As = "mutated"
	require.Equal(t, "respondsAs", bundle.Edges[0].GetAs(),
		"mutating the clone must not reach the original")
}

// TestGraphEdgeAsSurvivesBareIdRewriting pins `as` through
// WireBareifyBundle, the egress seam that strips the `{concept}:` prefix off
// every id-position value on its way to a browser or SDK.
//
// That rewriter touches FromId and ToId by name and leaves everything else
// alone, so `as` survives by omission. Worth its own assertion because `as`
// is a STRING on an edge next to two strings that ARE rewritten, and the
// obvious future edit -- "walk every string field on the edge" -- would
// quietly bare-ify a domain label. A label like `v1:respondsAs` would be
// unharmed; the point is that the seam must not be reasoning about the field
// at all.
func TestGraphEdgeAsSurvivesBareIdRewriting(t *testing.T) {
	bundle := &memqlv1.GraphBundle{
		RootIds: []string{"v1:rel3656:hub:x"},
		Edges: []*memqlv1.GraphEdge{
			{
				Type:   GraphEdgeLabelReferences,
				FromId: "v1:rel3656:hub:x",
				ToId:   "v1:rel3656:agent:y",
				As:     "respondsAs",
				Depth:  1,
			},
			{
				Type:   GraphEdgeLabelReferences,
				FromId: "v1:rel3656:hub:x",
				ToId:   "v1:rel3656:user:z",
				Depth:  1,
			},
		},
	}

	wire := WireBareifyBundle(bundle)
	require.NotNil(t, wire)
	require.Len(t, wire.Edges, 2)

	require.Equal(t, "x", wire.Edges[0].GetFromId(), "the endpoint ids are bare on the wire")
	require.Equal(t, "y", wire.Edges[0].GetToId())
	require.Equal(t, "respondsAs", wire.Edges[0].GetAs(),
		"WireBareifyBundle must leave GraphEdge.as verbatim -- it is a domain label, not an "+
			"id, and a client reads it to learn what the edge means")
	require.Empty(t, wire.Edges[1].GetAs(),
		"an unlabelled edge stays unlabelled across the wire seam")

	require.Equal(t, "respondsAs", bundle.Edges[0].GetAs(),
		"the input bundle must be untouched -- the engine's internal copy stays canonical")
	require.Equal(t, "v1:rel3656:hub:x", bundle.Edges[0].GetFromId(),
		"the input bundle must be untouched")
}

// TestRelationshipLabelSurvivesEveryConstructionSite is the test whose PURPOSE
// is its name.
//
// RelationshipExpression is built from scratch at ten sites across eight
// files, each restating the field list by hand. A site that forgets Label
// does not fail: it silently downgrades a scoped traversal to an unscoped
// one, returning MORE rows than were asked for. There is no error, no
// warning, and the query still looks like it worked -- which is why the
// sibling issue's author had to find those sites by hand.
//
// Three routes reach different sites, and all three are exercised on one
// engine over one set of rows so their answers are directly comparable:
//
//   - a RUNTIME query string      -> ASTConverter.convertRelationshipExpr
//   - an AUTHORED query's filter  -> resolveBareConcept +
//     functionValidator.expandExpressionWithArgs
//   - an AUTHORED spec body       -> specValidator.expandExpression
//
// Each must return the respondsAs agent ALONE. The peer agents are the trap:
// they belong to the same owner and the same concept, so an unscoped
// traversal returns them too and only an exact match notices.
func TestRelationshipLabelSurvivesEveryConstructionSite(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-sites")

	scoped := []string{w.respondsAsAgentId}
	unscopedAgents := []string{w.respondsAsAgentId, w.peerAAgentId, w.peerBAgentId}

	t.Run("runtimeQueryString", func(t *testing.T) {
		require.ElementsMatch(t, scoped,
			runTraversal(t, ctx, eng,
				fmt.Sprintf(`references("respondsAs", %s)`, w.hubFilter())),
			"a label in a runtime query string must survive ASTConverter.convertRelationshipExpr")
	})

	t.Run("authoredQueryFilter", func(t *testing.T) {
		require.ElementsMatch(t, scoped,
			runTraversal(t, ctx, eng,
				fmt.Sprintf(`query labelScopedRespondsAsAgents(hubOwner: %s)`, langparser.QuoteString(w.owner))),
			"a label in an authored query's filter must survive the function loader's "+
				"resolveBareConcept and the validator's arg expansion, both of which rebuild "+
				"the RelationshipExpression field by field")

		// The unscoped sibling proves the authored path can see the other
		// edges at all -- otherwise the scoped answer above could be an
		// artifact of the authored path resolving nothing.
		require.ElementsMatch(t, unscopedAgents,
			runTraversal(t, ctx, eng,
				fmt.Sprintf(`query labelUnscopedInteractsAgents(hubOwner: %s)`, langparser.QuoteString(w.owner))),
			"the unlabelled authored query must still reach every interactsWith agent")
	})

	t.Run("authoredSpecBody", func(t *testing.T) {
		got := runTraversal(t, ctx, eng, fmt.Sprintf(
			`concept==%s && row.createdBy==%q && labelScopedIsRespondsAsAgent`,
			labelAgentConcept, w.owner))
		require.ElementsMatch(t, scoped, got,
			"a label in a spec body must survive specValidator.expandExpression; the peer "+
				"agents are this owner's too, so an unscoped traversal would return all three")
	})
}

// TestEveryRelationshipExpressionLiteralSetsTheLabel is the structural half of
// the test above, and the one that covers a site nobody has written yet.
//
// The behavioural test can only reach the construction sites its three routes
// happen to traverse. This one walks the package's own source and requires
// EVERY composite literal of RelationshipExpression to set Label -- including
// the eleventh site added next month by someone who has never heard of #3656.
// That is the whole failure mode: the field is optional to Go, its absence is
// a valid zero value, and the zero value means "unscoped", so forgetting it
// widens a query instead of breaking it.
func TestEveryRelationshipExpressionLiteralSetsTheLabel(t *testing.T) {
	fset := token.NewFileSet()
	sites := 0

	for _, file := range productionGoFiles(t, ".") {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "RelationshipExpression" {
				return true
			}
			sites++

			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Label" {
					return true
				}
			}
			t.Errorf("%s: this RelationshipExpression literal does not set Label -- a "+
				"construction site that omits it silently downgrades a label-scoped traversal "+
				"to an unscoped one, returning MORE rows than the caller asked for, with no "+
				"error anywhere (memql#3656)", fset.Position(lit.Pos()))
			return true
		})
	}

	if sites == 0 {
		t.Fatal("found no RelationshipExpression composite literals -- this pin has stopped " +
			"covering the construction sites; find where the expression is built now")
	}
}

// TestRelationshipLabelIsPartOfTheResultCacheKey records a FINDING.
//
// The label does not reach the result-cache key. canonicalExpression
// (component/memql/canonical.go) renders a RelationshipExpression as
// `function(target)` and never reads Label, that string IS the cache
// signature, and result caching is default-on with a 60s TTL for any read
// whose dependency concepts are nameable and not denylisted.
//
// So two traversals differing only in their label -- and the unlabelled
// traversal alongside them -- share ONE cache entry, and whichever runs first
// answers for all of them within the TTL. Observed directly:
//
//	references("actsFor",    <hub>)  -> [the actsFor user]     (computed)
//	references("respondsAs", <hub>)  -> [the actsFor user]     (cache hit)
//	references(<hub>)                -> [the actsFor user]     (cache hit)
//
// and the two plan signatures compare EQUAL:
//
//	actor:...|interactswith(AND(concept=="v1:rel3656:hub",createdBy=="kb:..."))
//
// This is the feature's own failure mode reappearing one layer up: the
// resolvers scope correctly (every other test in this file proves it with the
// cache off) and the cache then serves another label's rows anyway. It is
// worse than the union, because the answer depends on which query ran first.
//
// canonicalExpression is not one of the ten RelationshipExpression
// CONSTRUCTION sites #3656 threaded Label through -- it is a READER of the
// struct, and the same class of miss: a place that must learn the new field
// or silently ignore it.
func TestRelationshipLabelIsPartOfTheResultCacheKey(t *testing.T) {
	eng, db := bootLabelEngine(t) // result cache deliberately LEFT ON
	ctx := clusterOwnerCtx("u-rel-3656-cache")
	w := seedLabelWorld(t, ctx, db, uniqueSuffix("rel-3656-cache"))

	// Ask for actsFor first, so a colliding key is populated with an answer
	// that is unmistakably not respondsAs.
	require.ElementsMatch(t, []string{w.actsForUserId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references("actsFor", %s)`, w.hubFilter())))

	require.ElementsMatch(t, []string{w.respondsAsAgentId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references("respondsAs", %s)`, w.hubFilter())),
		"a second label over the same inner filter must not be served the first label's "+
			"cached rows")

	require.ElementsMatch(t,
		[]string{w.respondsAsAgentId, w.actsForUserId, w.auditorUserId, w.peerAAgentId, w.peerBAgentId},
		runTraversal(t, ctx, eng, fmt.Sprintf(`references(%s)`, w.hubFilter())),
		"the unlabelled traversal must not be served a labelled traversal's cached rows")
}

// TestRelationshipLabelMatchingIsTrimmedAndCaseSensitive pins the two edges of
// how a label is compared, both of which follow from `as` being an open
// vocabulary that is never checked against a list.
//
// Trimming is forgiveness for formatting; case sensitivity is refusal to
// guess. `as` is a lowerCamelCase identifier by rule (memql#3652), so
// "RespondsAs" is not a spelling of "respondsAs" -- it is a different label
// nobody declared, and the honest answer is nothing. Case-insensitive matching
// would be a small kindness that makes the vocabulary ambiguous.
func TestRelationshipLabelMatchingIsTrimmedAndCaseSensitive(t *testing.T) {
	eng, ctx, w := labelTestEngine(t, "rel-3656-matching")

	require.ElementsMatch(t, []string{w.respondsAsAgentId},
		runTraversal(t, ctx, eng,
			fmt.Sprintf(`references("  respondsAs  ", %s)`, w.hubFilter())),
		"surrounding whitespace in a label is formatting, not meaning")

	require.Empty(t,
		runTraversal(t, ctx, eng,
			fmt.Sprintf(`references("RespondsAs", %s)`, w.hubFilter())),
		"label matching is CASE SENSITIVE: `as` is an open vocabulary the engine never "+
			"validates against a list, so it cannot know that RespondsAs was meant to be "+
			"respondsAs -- and guessing would make two spellings one label")
}

// TestFilterRelationshipDefinitionsLabelSemantics is the unit-level pin under
// everything above, on the three-state distinction the whole feature turns on:
//
//	nil          -> unscoped; every label matches (the one-argument form)
//	[]string{X}  -> exactly the definitions labelled X
//	[]string{""} -> ONLY the definitions carrying no label
//
// The third is the one that cannot be expressed by "empty means all", and it
// is not reachable from the query grammar at all -- graph expansion is its
// only caller, which needs it to attribute an unlabelled edge to the
// definition that produced it. So it gets a direct assertion rather than
// riding on a traversal that happens to exercise it.
func TestFilterRelationshipDefinitionsLabelSemantics(t *testing.T) {
	defs := []RelationshipDefinition{
		{Type: relationshipTypeReferences, Field: "agentId", Direction: relationshipDirectionOutgoing, As: "respondsAs"},
		{Type: relationshipTypeReferences, Field: "forUserId", Direction: relationshipDirectionOutgoing, As: "actsFor"},
		{Type: relationshipTypeReferences, Field: "auditorId", Direction: relationshipDirectionOutgoing},
		{Type: relationshipTypeReferences, Field: "peerAId", Direction: relationshipDirectionOutgoing, As: "collaboratesWith"},
		{Type: relationshipTypeReferences, Field: "peerBId", Direction: relationshipDirectionOutgoing, As: "collaboratesWith"},
		{Type: relationshipTypeParent, Field: "spaceId", Direction: relationshipDirectionOutgoing, As: "belongsToSpace"},
	}

	fieldsOf := func(got []RelationshipDefinition) []string {
		out := make([]string, 0, len(got))
		for _, def := range got {
			out = append(out, def.Field)
		}
		return out
	}

	t.Run("nilIsUnscoped", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, nil)
		require.ElementsMatch(t,
			[]string{"agentId", "forUserId", "auditorId", "peerAId", "peerBId"}, fieldsOf(got),
			"a nil label filter is the one-argument traversal form: follow every edge of "+
				"the type, which is what every query predating memql#3656 does")
	})

	t.Run("exactLabel", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, []string{"respondsAs"})
		require.ElementsMatch(t, []string{"agentId"}, fieldsOf(got))
	})

	t.Run("sharedLabelIsAUnion", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, []string{"collaboratesWith"})
		require.ElementsMatch(t, []string{"peerAId", "peerBId"}, fieldsOf(got),
			"two definitions carrying one label are BOTH selected")
	})

	t.Run("emptyStringSelectsOnlyUnlabelled", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, []string{""})
		require.ElementsMatch(t, []string{"auditorId"}, fieldsOf(got),
			"[]string{\"\"} must mean \"the edges carrying NO label\" -- distinct from nil, "+
				"which means \"every edge\". Graph expansion depends on being able to ask "+
				"that question, and nothing else can")
	})

	t.Run("unknownLabelSelectsNothing", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, []string{"nobodyUsesThis"})
		require.Empty(t, got)
	})

	t.Run("labelDoesNotCrossStructuralTypes", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, []string{"belongsToSpace"})
		require.Empty(t, got,
			"belongsToSpace is a `parent` label; the structural type is filtered first and a "+
				"label never reaches across it")
	})

	t.Run("labelComposesWithDirection", func(t *testing.T) {
		got := filterRelationshipDefinitions(defs, relationshipTypeParent,
			[]string{relationshipDirectionIncoming}, []string{"belongsToSpace"})
		require.Empty(t, got,
			"the label must not override the direction filter -- belongsToSpace is declared "+
				"outgoing, so an incoming-only traversal must not find it")
	})

	t.Run("labelsPresentIsDistinctAndIncludesTheUnlabelled", func(t *testing.T) {
		interacts := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, nil)
		require.Equal(t,
			[]string{"respondsAs", "actsFor", "", "collaboratesWith"},
			relationshipLabelsPresent(interacts),
			"graph expansion resolves once per DISTINCT label, in declaration order, with the "+
				"empty string standing for the unlabelled edges -- two definitions sharing a "+
				"label must produce one pass, not two")
	})
}
