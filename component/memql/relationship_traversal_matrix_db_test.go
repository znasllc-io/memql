package memql

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// relationship_traversal_matrix_db_test.go is the coverage matrix for
// executor_relationship.go (memql#3658) -- the nine traversal functions, every
// direction each one supports, both field shapes, and the graph-expansion fan-
// out that reaches the same resolvers from the other side.
//
// WHY A WHOLE FILE FOR CODE NOTHING IN THIS REPO CALLS. `parentOf` / `childOf`
// / `aliasOf` / `equals` / `interactsWith` / `contains` / `owns` / `createdBy`
// / `ids` have ZERO call sites in dsl/, clients/ or editors/. They are reached
// only over gRPC / WS / MCP, and by downstream client repos this repo cannot
// see. That makes them look dormant and makes them exactly the wrong thing to
// leave unmeasured: memql#3432 -- one parent returning at most ONE child --
// was a severe correctness defect that lived in resolveChildOf until
// 2026-08-09, and nothing in this repo would ever have noticed. Before this
// file the coverage was two db tests, both regressions
// (relationship_versioned_ids_3397_db_test.go, relationship_incoming_target_
// 3432_db_test.go), leaving resolveContains, resolveOwns, resolveAliasOrEquals,
// resolveInteractsWith, resolveCreatedBy, resolveIds, the dispatch switch and
// expandGraph's fan-out with none at all.
//
// WHY A FIXTURE DOMAIN. All 141 @relationship declarations in the shipped
// corpus are direction="outgoing" and type="parent". Against that corpus
// childOf, the incoming branches of owns / interactsWith / createdBy, the
// array field shape, contains, alias, equals and the table-sourced createdBy
// are all UNREACHABLE -- a test written against the corpus cannot fail no
// matter how broken the resolver is. So the fixture below declares the shapes
// the corpus does not have, mounted through memqldsl.RegisterTree: the same
// path a product DSL bundle mounts through at MEMQL_DSL_PATH, and the
// convention the other bespoke-DSL tests in this package already use.
//
// The fixture MUST be mounted before readMergeTestEngine, because the engine
// snapshots the relationship registry during Init.
//
// WHAT THE ASSERTIONS READ. A traversal's answer is the bundle's RootIds, not
// its Nodes. buildGraphBundle expands every returned row one level by default,
// so `childOf(<folder>)` puts the docs in RootIds and their authors/creators
// in Nodes alongside them. RootIds is exactly what evaluateExpression returned
// -- the traversal -- which is what these tests are about. The one test that
// is about the expansion reads Nodes and Edges on purpose.
//
// CLUSTERED SEEDING, carried over from memql#3388 / #3397 / #3432: every
// version of an id occupies a contiguous createdAt run, the shape a
// repeatedly-updated row actually has. Interleaving hands consecutive raw rows
// to DIFFERENT ids and lets a short window collapse to a full result by luck.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// The fixture domain and the concept ids it assembles to. Concept ids are
// `v<major>:<domain>:<name>` with no @version / @namespace annotation, so the
// domain name IS the namespace segment.
const traversalFixtureDomain = "reltrav"

const (
	tvFolder    = "v1:reltrav:folder"
	tvDoc       = "v1:reltrav:doc"
	tvPerson    = "v1:reltrav:person"
	tvRoster    = "v1:reltrav:roster"
	tvTeam      = "v1:reltrav:team"
	tvBundle    = "v1:reltrav:bundle"
	tvDocAlias  = "v1:reltrav:docAlias"
	tvDocMirror = "v1:reltrav:docMirror"
	tvSigner    = "v1:reltrav:signer"
	tvAudit     = "v1:reltrav:auditEntry"
	tvSquad     = "v1:reltrav:squad"
	tvCadet     = "v1:reltrav:cadet"
	tvProbe     = "v1:reltrav:probe"
)

// traversalFixtureConcepts declares one concept per cell of the matrix.
//
// Two loader rules shape it and are worth naming, because breaking either is
// a boot failure rather than a test failure:
//
//   - checkDuplicateRelationship keys on (concept, field|fieldSource, type),
//     so two relationships of the same type on one concept must name
//     different fields.
//   - checkRelationshipDirection keys on (source, target, type, fieldSource)
//     WITHOUT the field, and requires a pair linked by the same type from both
//     sides to be outgoing/incoming (or bidirectional). folder/doc,
//     doc/person, roster/person, squad/cadet and signer/auditEntry are each
//     declared from both ends and satisfy it.
const traversalFixtureConcepts = `/// Fixture container. Declares the INCOMING side of a parent relationship,
/// which no concept in the shipped corpus does, so childOf(folder) resolves.
concept folder {
  label string @description("Human label, so fixture rows are legible in a failure dump.")

  @relationship(type="parent", field="folderId", target=doc, direction="incoming")
}

/// The fixture's workhorse row: outgoing parent, outgoing interactsWith and
/// outgoing payload-sourced createdBy, all on scalar fields.
concept doc {
  folderId  string @description("The v1:reltrav:folder.id this doc hangs off.")
  authorId  string @description("The v1:reltrav:person.id this doc interacts with.")
  creatorId string @description("The v1:reltrav:person.id that created this doc.")
  label     string @description("Human label.")

  @relationship(type="parent", field="folderId", target=folder, direction="outgoing")
  @relationship(type="interactsWith", field="authorId", target=person, direction="outgoing")
  @relationship(type="createdBy", field="creatorId", target=person, direction="outgoing")
}

/// The far side of doc's interactsWith / createdBy edges and of roster's owns
/// edge. Three INCOMING declarations -- the reverse-lookup shape the corpus
/// has none of.
concept person {
  label string @description("Human label.")

  @relationship(type="interactsWith", field="authorId", target=doc, direction="incoming")
  @relationship(type="createdBy", field="creatorId", target=doc, direction="incoming")
  @relationship(type="owns", field="ownerPersonId", target=roster, direction="incoming")
}

/// owns over a SCALAR field, outgoing. The reverse of person's owns edge.
concept roster {
  ownerPersonId string @description("The v1:reltrav:person.id this roster is owned by.")
  label         string @description("Human label.")

  @relationship(type="owns", field="ownerPersonId", target=person, direction="outgoing")
}

/// owns over an ARRAY field, outgoing. Exercises extractStringValueOrArrayFromMap's
/// []any branch, which the scalar roster above never reaches.
concept team {
  memberIds []string @description("The v1:reltrav:person.ids this team owns.")
  label     string   @description("Human label.")

  @relationship(type="owns", field="memberIds", target=person, direction="outgoing")
}

/// contains over an ARRAY field. A collection concept must declare exactly
/// this, and contains is outgoing-only by construction (normalizeRelationshipDefinition
/// rejects the incoming form).
@type("collection")
concept bundle {
  memberIds []string @description("The v1:reltrav:doc.ids this bundle contains.")
  label     string   @description("Human label.")

  @relationship(type="contains", field="memberIds", target=doc, direction="outgoing")
}

/// alias -- a reference row pointing at its canonical doc.
@type("reference")
concept docAlias {
  targetId string @description("The v1:reltrav:doc.id this alias names.")
  label    string @description("Human label.")

  @relationship(type="alias", field="targetId", target=doc, direction="outgoing")
}

/// equals -- the sibling of alias, resolved by the same function under a
/// different relationship type.
@type("reference")
concept docMirror {
  sameAsId string @description("The v1:reltrav:doc.id this mirror equals.")
  label    string @description("Human label.")

  @relationship(type="equals", field="sameAsId", target=doc, direction="outgoing")
}

/// The far side of auditEntry's TABLE-sourced createdBy edge. createdBy is a
/// reserved payload field, so deriveRelationshipFieldSource routes it to the
/// MemoryNodes column rather than the payload -- the FieldSourceTable branch
/// of resolveCreatedBy, which the payload-sourced doc/person pair never reaches.
concept signer {
  label string @description("Human label.")

  @relationship(type="createdBy", field="createdBy", target=auditEntry, direction="incoming")
}

/// A row whose creator is named by the createdBy COLUMN, not by a payload field.
concept auditEntry {
  label string @description("Human label.")

  @relationship(type="createdBy", field="createdBy", target=signer, direction="outgoing")
}

/// squad/cadet is owns declared from both ends over an ARRAY field. The
/// outgoing half works; the incoming half is the subject of
/// TestRelationshipOwns_IncomingArrayField_FindsEveryOwner.
concept squad {
  memberIds []string @description("The v1:reltrav:cadet.ids this squad owns.")
  label     string   @description("Human label.")

  @relationship(type="owns", field="memberIds", target=cadet, direction="outgoing")
}

/// The reverse-lookup side of squad.memberIds.
concept cadet {
  label string @description("Human label.")

  @relationship(type="owns", field="memberIds", target=squad, direction="incoming")
}

/// Two relationships of the SAME type over payload string fields, differing
/// only in direction. Subject of TestRelationshipBidirectional_SkipsIdCanonicalization.
concept probe {
  outgoingRef string @description("A bare or canonical v1:reltrav:doc.id.")
  bidiRef     string @description("A bare or canonical v1:reltrav:person.id.")
  label       string @description("Human label.")

  @relationship(type="parent", field="outgoingRef", target=doc, direction="outgoing")
  @relationship(type="parent", field="bidiRef", target=person, direction="bidirectional")
}
`

// mountTraversalFixture registers the fixture domain and restores the global
// concept registry afterwards.
//
// LoadUnifiedConcepts merges into the process-wide default registry and there
// is no per-concept removal, so the snapshot/ReplaceAll pair is what keeps a
// fixture concept from outliving its test. UnregisterTree alone would not do
// it: the tree is the SOURCE, the registry is where the loaded concept lands.
func mountTraversalFixture(t *testing.T) {
	t.Helper()
	before := memorynodes.All()
	memqldsl.RegisterTree(traversalFixtureDomain, fstest.MapFS{
		"concepts.memql": {Data: []byte(traversalFixtureConcepts)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(traversalFixtureDomain)
		memorynodes.ReplaceAll(before)
	})
}

// traversalSeeder writes fixture rows on one monotonically advancing clock so
// every id's versions land in a contiguous createdAt run.
type traversalSeeder struct {
	t     *testing.T
	ctx   context.Context
	db    *bun.DB
	sfx   string
	owner string
	base  time.Time
	tick  int
}

// id composes the canonical `<concept>:<suffix>-<name>` node id. The suffix
// keeps concurrent local runs from colliding in a shared database.
func (s *traversalSeeder) id(conceptName, name string) string {
	return fmt.Sprintf("%s:%s-%s", conceptName, s.sfx, name)
}

// row writes `versions` clustered versions of one row under the seeder's owner
// and returns the id it wrote.
func (s *traversalSeeder) row(conceptName, name string, versions int, payload map[string]any) string {
	s.t.Helper()
	return s.rowCreatedBy(conceptName, name, s.owner, versions, payload)
}

// rowCreatedBy is row with an explicit createdBy column value, which the
// TABLE-sourced createdBy relationship reads as its foreign key.
func (s *traversalSeeder) rowCreatedBy(conceptName, name, createdBy string, versions int, payload map[string]any) string {
	s.t.Helper()
	id := s.id(conceptName, name)
	for v := 0; v < versions; v++ {
		p := make(map[string]any, len(payload)+1)
		for k, val := range payload {
			p[k] = val
		}
		p["label"] = fmt.Sprintf("%s-v%d", name, v)
		seedRawRow(s.t, s.ctx, s.db, conceptName, id,
			s.base.Add(time.Duration(s.tick)*time.Second), createdBy, p)
		s.tick++
	}
	return id
}

// traversalRootIDs is the traversal's answer: the ids evaluateExpression
// returned, before buildGraphBundle expanded each of them one level. See the
// file header for why this is not pageIDs.
func traversalRootIDs(t *testing.T, res *ExecuteResult) []string {
	t.Helper()
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)
	ids := []string{}
	ids = append(ids, res.Bundle.RootIds...)
	return ids
}

// traversalQuery is the shape every case in the matrix uses:
// `<fn>(concept==<concept> && createdBy==<scope>)`.
func traversalQuery(fn, conceptName, scope string) string {
	return fmt.Sprintf(`%s(concept==%s && createdBy==%q)`, fn, conceptName, scope)
}

// traversalWorld is the shared row set the matrix traverses, plus the ids of
// everything in it so a case can name its expectation.
type traversalWorld struct {
	folder   string
	docs     []string
	author   string // P1 -- doc.authorId and doc.creatorId
	rostered string // P2 -- roster.ownerPersonId, and a team member
	teamOnly string // P3 -- a team member only
	roster   string
	team     string
	bundle   string
	alias    string
	mirror   string
	signer   string
	audits   []string
}

// seedTraversalWorld writes one interconnected world covering every cell of
// the matrix. Cardinality is deliberate: ONE folder with THREE docs
// (one-to-many, the memql#3432 shape), THREE docs naming ONE person
// (many-to-one), ONE alias naming ONE doc (one-to-one).
func seedTraversalWorld(t *testing.T, s *traversalSeeder) traversalWorld {
	t.Helper()

	w := traversalWorld{}

	// Persons first so the docs can name them. Clustered versions throughout.
	w.author = s.row(tvPerson, "author", 4, nil)
	w.rostered = s.row(tvPerson, "rostered", 3, nil)
	w.teamOnly = s.row(tvPerson, "team-only", 2, nil)

	w.folder = s.row(tvFolder, "folder", 3, nil)

	for _, name := range []string{"doc-a", "doc-b", "doc-c"} {
		w.docs = append(w.docs, s.row(tvDoc, name, 2, map[string]any{
			"folderId":  w.folder,
			"authorId":  w.author,
			"creatorId": w.author,
		}))
	}

	w.roster = s.row(tvRoster, "roster", 2, map[string]any{"ownerPersonId": w.rostered})
	w.team = s.row(tvTeam, "team", 2, map[string]any{
		"memberIds": []any{w.rostered, w.teamOnly},
	})
	w.bundle = s.row(tvBundle, "bundle", 2, map[string]any{
		"memberIds": []any{w.docs[0], w.docs[1]},
	})
	w.alias = s.row(tvDocAlias, "alias", 2, map[string]any{"targetId": w.docs[0]})
	w.mirror = s.row(tvDocMirror, "mirror", 2, map[string]any{"sameAsId": w.docs[1]})

	// The TABLE-sourced pair: the audit rows' createdBy COLUMN names the
	// signer, which is the whole point of FieldSourceTable.
	w.signer = s.row(tvSigner, "signer", 2, nil)
	for _, name := range []string{"audit-a", "audit-b"} {
		w.audits = append(w.audits, s.rowCreatedBy(tvAudit, name, w.signer, 2, nil))
	}

	return w
}

// TestRelationshipFixture_DeclaresEveryShapeUnderTest asserts the fixture
// actually reached the engine before anything traverses it.
//
// Without this, a fixture that failed to mount would not fail loudly: several
// resolvers return an empty set rather than an error when a concept declares
// no matching relationship, so a silently-absent fixture would turn the whole
// matrix green while measuring nothing.
func TestRelationshipFixture_DeclaresEveryShapeUnderTest(t *testing.T) {
	mountTraversalFixture(t)
	eng, _, _ := readMergeTestEngine(t)

	cases := []struct {
		concept     string
		relType     string
		field       string
		direction   string
		fieldSource string
	}{
		{tvFolder, relationshipTypeParent, "folderId", relationshipDirectionIncoming, memorynodes.FieldSourcePayload},
		{tvDoc, relationshipTypeParent, "folderId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvDoc, relationshipTypeInteracts, "authorId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvDoc, relationshipTypeCreatedBy, "creatorId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvPerson, relationshipTypeInteracts, "authorId", relationshipDirectionIncoming, memorynodes.FieldSourcePayload},
		{tvPerson, relationshipTypeCreatedBy, "creatorId", relationshipDirectionIncoming, memorynodes.FieldSourcePayload},
		{tvPerson, relationshipTypeOwns, "ownerPersonId", relationshipDirectionIncoming, memorynodes.FieldSourcePayload},
		{tvRoster, relationshipTypeOwns, "ownerPersonId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvTeam, relationshipTypeOwns, "memberIds", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvBundle, relationshipTypeContains, "memberIds", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvDocAlias, relationshipTypeAlias, "targetId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvDocMirror, relationshipTypeEquals, "sameAsId", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvSigner, relationshipTypeCreatedBy, "createdBy", relationshipDirectionIncoming, memorynodes.FieldSourceTable},
		{tvAudit, relationshipTypeCreatedBy, "createdBy", relationshipDirectionOutgoing, memorynodes.FieldSourceTable},
		{tvSquad, relationshipTypeOwns, "memberIds", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvCadet, relationshipTypeOwns, "memberIds", relationshipDirectionIncoming, memorynodes.FieldSourcePayload},
		{tvProbe, relationshipTypeParent, "outgoingRef", relationshipDirectionOutgoing, memorynodes.FieldSourcePayload},
		{tvProbe, relationshipTypeParent, "bidiRef", relationshipDirectionBidirectional, memorynodes.FieldSourcePayload},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s/%s", tc.concept, tc.relType, tc.field), func(t *testing.T) {
			defs := eng.relationshipDefinitionsForConcept(tc.concept)
			require.NotEmpty(t, defs,
				"concept %q reached the engine with no relationship definitions; the fixture did not mount", tc.concept)

			var found bool
			for _, def := range defs {
				if def.Type == tc.relType && def.Field == tc.field {
					found = true
					require.Equal(t, tc.direction, def.Direction)
					require.Equal(t, tc.fieldSource, def.FieldSource)
				}
			}
			require.True(t, found, "concept %q declares no %s relationship on %q", tc.concept, tc.relType, tc.field)
		})
	}
}

// TestRelationshipTraversalMatrix drives all NINE relationship functions
// through Execute against one shared world, which is also what covers
// evaluateRelationshipExpression's dispatch switch: each case reaches a
// different arm of it.
//
// Two functions in the table never consult direction at all, and that is not
// an omission in the fixture:
//
//   - aliasOf / equals call filterRelationshipDefinitions with a nil direction
//     list, so every alias / equals definition matches regardless of what it
//     declares.
//   - ids consults no relationship definition whatsoever; it strips payload +
//     schema off the rows the inner expression already produced.
func TestRelationshipTraversalMatrix(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-matrix")
	sfx := uniqueSuffix("rel-matrix")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
	}
	w := seedTraversalWorld(t, s)

	cases := []struct {
		name  string
		query string
		want  []string
		why   string
	}{
		{
			name:  "parentOf/outgoing/scalar/many-to-one",
			query: traversalQuery("parentOf", tvDoc, owner),
			want:  []string{w.folder},
			why:   "three docs naming one folder must collapse to that one folder",
		},
		{
			name:  "childOf/incoming/scalar/one-to-many",
			query: traversalQuery("childOf", tvFolder, owner),
			want:  w.docs,
			why:   "one folder must reach EVERY child (the memql#3432 shape)",
		},
		{
			name:  "aliasOf/one-to-one",
			query: traversalQuery("aliasOf", tvDocAlias, owner),
			want:  []string{w.docs[0]},
			why:   "an alias row resolves to the canonical row its targetId names",
		},
		{
			name:  "equals/one-to-one",
			query: traversalQuery("equals", tvDocMirror, owner),
			want:  []string{w.docs[1]},
			why:   "equals is alias under a different relationship type",
		},
		{
			name:  "interactsWith/outgoing/scalar/many-to-one",
			query: traversalQuery("interactsWith", tvDoc, owner),
			want:  []string{w.author},
			why:   "the outgoing branch reads each doc's authorId and fetches by id",
		},
		{
			name:  "interactsWith/incoming/scalar/one-to-many",
			query: traversalQuery("interactsWith", tvPerson, owner),
			want:  w.docs,
			why:   "the incoming branch asks which docs name this person as author",
		},
		{
			name:  "contains/outgoing/array/one-to-many",
			query: traversalQuery("contains", tvBundle, owner),
			want:  []string{w.docs[0], w.docs[1]},
			why:   "contains reads an ARRAY field via extractStringArrayFromMap",
		},
		{
			name:  "owns/outgoing/scalar/one-to-one",
			query: traversalQuery("owns", tvRoster, owner),
			want:  []string{w.rostered},
			why:   "the outgoing branch accepts a scalar via extractStringValueOrArrayFromMap",
		},
		{
			name:  "owns/outgoing/array/one-to-many",
			query: traversalQuery("owns", tvTeam, owner),
			want:  []string{w.rostered, w.teamOnly},
			why:   "the same branch accepts an array; the scalar case above never reaches it",
		},
		{
			name:  "owns/incoming/scalar/many-to-one",
			query: traversalQuery("owns", tvPerson, owner),
			want:  []string{w.roster},
			why:   "three persons asked, one roster names any of them",
		},
		{
			name:  "createdBy/outgoing/payload/many-to-one",
			query: traversalQuery("createdBy", tvDoc, owner),
			want:  []string{w.author},
			why:   "the FieldSourcePayload branch reads creatorId out of the payload",
		},
		{
			name:  "createdBy/incoming/payload/one-to-many",
			query: traversalQuery("createdBy", tvPerson, owner),
			want:  w.docs,
			why:   "the reverse question: which docs name this person as creator",
		},
		{
			name:  "createdBy/outgoing/table/many-to-one",
			query: traversalQuery("createdBy", tvAudit, w.signer),
			want:  []string{w.signer},
			why:   "the FieldSourceTable branch reads the createdBy COLUMN via nodeFieldValue",
		},
		{
			name:  "createdBy/incoming/table/one-to-many",
			query: traversalQuery("createdBy", tvSigner, owner),
			want:  w.audits,
			why:   "fetchNodesByNodeFieldValues answers which rows this identity created",
		},
		{
			name:  "ids/no-relationship-consulted",
			query: traversalQuery("ids", tvDoc, owner),
			want:  w.docs,
			why:   "ids() dedupes the inner set by id and strips payload + schema",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := eng.Execute(ctx, tc.query)
			require.NoError(t, err, "query: %s", tc.query)
			require.ElementsMatch(t, tc.want, traversalRootIDs(t, res), tc.why)
		})
	}
}

// TestRelationshipIds_StripsPayloadAndCollapsesClusteredVersions is the
// clustered-versions case for ids(), which the matrix above covers only for
// its id set.
//
// ids() is the one function that consults no relationship definition, so what
// it has to get right is different from every other cell: ONE row per id
// however many versions that id has, and payload + schema removed. Seeded with
// deep clustered runs so the dedupe is doing work rather than passing on a
// single-version happy path.
func TestRelationshipIds_StripsPayloadAndCollapsesClusteredVersions(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-ids")
	sfx := uniqueSuffix("rel-ids")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}

	const (
		distinct = 6
		versions = 9
	)
	want := make([]string, 0, distinct)
	for i := 0; i < distinct; i++ {
		want = append(want, s.row(tvPerson, fmt.Sprintf("p-%02d", i), versions, nil))
	}

	res, err := eng.Execute(ctx, traversalQuery("ids", tvPerson, owner))
	require.NoError(t, err)
	require.ElementsMatch(t, want, traversalRootIDs(t, res),
		"ids() must return each distinct id exactly once, not once per version")

	require.NotNil(t, res.Bundle)
	require.Len(t, res.Bundle.Nodes, distinct,
		"the bundle carries one node per id; a person has no outgoing edge to expand into")
	for _, n := range res.Bundle.Nodes {
		require.Empty(t, n.GetPayload(), "ids() strips the payload off every node it returns")
	}
}

// TestRelationshipOwns_IncomingArrayField_FindsEveryOwner is the array shape of
// the INCOMING lookup: "which squads have this cadet in their memberIds".
//
// SKIPPED, and the skip is the finding. fetchNodesByJSONFieldValues compiles
// the reverse lookup to
//
//	payload #>> '{memberIds}' IN ('v1:reltrav:cadet:...')
//
// and `#>>` on an array-valued key returns the array's JSON TEXT. Confirmed
// against the fixture rows:
//
//	payload #>> '{memberIds}'
//	  -> ["v1:reltrav:cadet:...-cadet", "v1:reltrav:cadet:...-other"]
//
// which can never equal a single member id. So every incoming lookup against
// an array-valued field answers "nobody" -- silently, with no error and no
// warning, exactly the failure mode memql#3432 had.
//
// The blast radius is the SHARED HELPER, not owns: childOf, the incoming
// branch of interactsWith, and the payload-sourced incoming branch of
// createdBy all reach the same fetchNodesByJSONFieldValues, so every one of
// them is blind to an array-valued relationship field the same way.
//
// The outgoing half of the same relationship works (see the owns/outgoing/array
// case in the matrix), which is what makes this asymmetric and easy to miss:
// the declaration looks symmetrical and one direction of it quietly is not.
// Reaching it needs a jsonb containment predicate (`payload->'memberIds' ?
// <value>` or `@>`), not the scalar equality the shared helper emits.
//
// The assertion below is what the engine SHOULD answer; unskip it when the
// containment predicate lands. Verified to FAIL as written, with an empty
// result set against two seeded squads.
func TestRelationshipOwns_IncomingArrayField_FindsEveryOwner(t *testing.T) {
	t.Skip("SUSPECTED DEFECT (memql#3658): fetchNodesByJSONFieldValues compares `payload #>> '{field}'` " +
		"to a scalar id, so an INCOMING lookup against an ARRAY-valued relationship field matches " +
		"nothing. Needs a jsonb containment predicate. Unskip when fixed.")

	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-owns-array-in")
	sfx := uniqueSuffix("rel-owns-array-in")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	}

	cadet := s.row(tvCadet, "cadet", 3, nil)
	other := s.row(tvCadet, "other", 3, nil)
	squadA := s.row(tvSquad, "squad-a", 2, map[string]any{"memberIds": []any{cadet, other}})
	squadB := s.row(tvSquad, "squad-b", 2, map[string]any{"memberIds": []any{cadet}})

	// Sanity: the OUTGOING half of the very same declaration resolves, so a
	// failure below is about the direction and not about the fixture.
	res, err := eng.Execute(ctx, fmt.Sprintf(
		`owns(concept==%s && id==%q)`, tvSquad, squadB))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{cadet}, traversalRootIDs(t, res))

	res, err = eng.Execute(ctx, fmt.Sprintf(
		`owns(concept==%s && id==%q)`, tvCadet, cadet))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{squadA, squadB}, traversalRootIDs(t, res),
		"an incoming owns lookup must find every row whose ARRAY field contains this id")
}

// TestRelationshipEmptyAnswer_ResolversDisagree is a CHARACTERIZATION
// test: it pins what each resolver does when the honest answer is "nothing",
// because they do not agree and a client cannot tell which it will get.
//
// Same question -- "traverse this edge from rows that have none" -- four
// different answers:
//
//	contains / childOf / owns(incoming)  -> empty set, nil error
//	parentOf                             -> ERROR "parentOf traversal produced
//	                                        no parent references"
//	aliasOf, field ABSENT                -> ERROR "alias traversal produced no
//	                                        candidate queries"
//	aliasOf, field EMPTY STRING          -> the alias row ITSELF
//
// The last one is the sharpest: resolveAliasOrEquals falls back to
// `canonical = node.ID` when the extracted value is blank, so a dangling alias
// reports itself as its own canonical target -- a self-loop a client following
// aliasOf would take as a real edge. And the two spellings of "no target"
// (key absent vs key present but empty) land on opposite sides of that
// fallback, so which one a writer used decides whether the caller gets an
// error or a wrong answer.
//
// parentOf erroring matters because it is one of the two most-reached
// functions on this surface: "who is this row's parent" asked about root rows
// is an ordinary question, and "none" is an ordinary answer. Today it fails
// the whole query instead -- but only when NO row in the set has a parent, so
// a mixed set hides it.
//
// Written as assertions on the CURRENT behaviour rather than skipped, because
// none of these is a silently wrong count: they are loud, and pinning them
// means a future change that makes the family consistent has to update this
// test deliberately rather than drift past it.
func TestRelationshipEmptyAnswer_ResolversDisagree(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-empty")
	sfx := uniqueSuffix("rel-empty")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}

	// Rows that genuinely have nothing on the far side of the edge.
	s.row(tvDoc, "orphan-a", 2, map[string]any{"authorId": "x"})
	s.row(tvDoc, "orphan-b", 2, map[string]any{"authorId": "x"})
	lonelyFolder := s.row(tvFolder, "lonely", 2, nil)
	emptyBundle := s.row(tvBundle, "empty", 2, nil)
	unowned := s.row(tvPerson, "unowned", 2, nil)
	blankAlias := s.row(tvDocAlias, "blank", 2, map[string]any{"targetId": ""})
	absentAlias := s.row(tvDocAlias, "absent", 2, nil)
	danglingAlias := s.row(tvDocAlias, "dangling", 2, map[string]any{
		"targetId": tvDoc + ":" + sfx + "-does-not-exist",
	})

	byId := func(fn, conceptName, id string) string {
		return fmt.Sprintf(`%s(concept==%s && id==%q)`, fn, conceptName, id)
	}

	// The empty-set majority.
	res, err := eng.Execute(ctx, byId("childOf", tvFolder, lonelyFolder))
	require.NoError(t, err)
	require.Empty(t, traversalRootIDs(t, res), "childOf answers a childless parent with an empty set")

	res, err = eng.Execute(ctx, byId("contains", tvBundle, emptyBundle))
	require.NoError(t, err)
	require.Empty(t, traversalRootIDs(t, res), "contains answers an empty collection with an empty set")

	res, err = eng.Execute(ctx, byId("owns", tvPerson, unowned))
	require.NoError(t, err)
	require.Empty(t, traversalRootIDs(t, res), "an incoming owns lookup with no owners answers with an empty set")

	res, err = eng.Execute(ctx, byId("aliasOf", tvDocAlias, danglingAlias))
	require.NoError(t, err)
	require.Empty(t, traversalRootIDs(t, res), "an alias naming a row that does not exist answers with an empty set")

	// The two dissenters.
	_, err = eng.Execute(ctx, traversalQuery("parentOf", tvDoc, owner))
	require.Error(t, err, "parentOf ERRORS where its siblings return an empty set")
	require.Contains(t, err.Error(), "produced no parent references")

	_, err = eng.Execute(ctx, byId("aliasOf", tvDocAlias, absentAlias))
	require.Error(t, err, "an alias row with NO targetId key errors rather than answering empty")
	require.Contains(t, err.Error(), "produced no candidate queries")

	// The self-loop.
	res, err = eng.Execute(ctx, byId("aliasOf", tvDocAlias, blankAlias))
	require.NoError(t, err)
	require.Equal(t, []string{blankAlias}, traversalRootIDs(t, res),
		"an alias row whose targetId is the EMPTY STRING resolves to ITSELF: resolveAliasOrEquals "+
			"falls back to node.ID when the extracted value is blank, so the traversal reports a "+
			"self-loop instead of 'this alias points nowhere'")
}

// TestRelationshipTraversal_PaginateBoundsTheResult pins the limit the
// traversal is given.
//
// Worth its own test rather than an assertion tacked onto the childOf case:
// every resolver threads `limit` through several distinct places (the id-set
// accumulation loop, the per-definition break, the fetch, and a final trim),
// and memql#3432 was precisely a bound applied in the wrong one of those.
func TestRelationshipTraversal_PaginateBoundsTheResult(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-limit")
	sfx := uniqueSuffix("rel-limit")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}

	const (
		children = 9
		limit    = 4
	)
	folder := s.row(tvFolder, "folder", 3, nil)
	seeded := make(map[string]struct{}, children)
	for i := 0; i < children; i++ {
		seeded[s.row(tvDoc, fmt.Sprintf("d-%02d", i), 2, map[string]any{"folderId": folder})] = struct{}{}
	}

	res, err := eng.Execute(ctx, fmt.Sprintf(
		`paginate(childOf(concept==%s && createdBy==%q), %d)`, tvFolder, owner, limit))
	require.NoError(t, err)

	got := traversalRootIDs(t, res)
	require.Len(t, got, limit, "paginate must bound the traversal result")
	for _, id := range got {
		require.Contains(t, seeded, id, "a bounded traversal must still return rows from the matching set")
	}
}

// TestRelationshipGraphExpansion_StopsAtRequestedDepth covers expandGraph,
// which reaches the same resolvers from the other side: not from a query
// naming a traversal function, but from buildGraphBundle walking every
// relationship a returned row declares.
//
// This is the ONLY test in the file that reads the bundle's Nodes rather than
// its RootIds, because the expansion IS the subject here.
//
// The world is folder -> doc -> person, so the depth boundary is observable:
// at depth 1 the docs are reached and their author is not; at depth 2 the
// author is. Each hop also asserts its EDGE, since a node can enter the bundle
// as a root without any traversal having happened.
func TestRelationshipGraphExpansion_StopsAtRequestedDepth(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-depth")
	sfx := uniqueSuffix("rel-depth")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}

	author := s.row(tvPerson, "author", 3, nil)
	folder := s.row(tvFolder, "folder", 2, nil)
	docs := []string{}
	for _, name := range []string{"doc-a", "doc-b"} {
		docs = append(docs, s.row(tvDoc, name, 2, map[string]any{
			"folderId":  folder,
			"authorId":  author,
			"creatorId": author,
		}))
	}

	bundleIDs := func(res *ExecuteResult) []string {
		ids := []string{}
		for _, n := range res.Bundle.GetNodes() {
			ids = append(ids, n.GetId())
		}
		return ids
	}
	hasEdge := func(edges []*memqlv1.GraphEdge, from, to, relType string) bool {
		for _, e := range edges {
			if e.GetFromId() == from && e.GetToId() == to && e.GetType() == relType {
				return true
			}
		}
		return false
	}

	depth1, err := eng.Execute(ctx, fmt.Sprintf(
		`withDepth(concept==%s && createdBy==%q, 1)`, tvFolder, owner))
	require.NoError(t, err)
	require.ElementsMatch(t, append([]string{folder}, docs...), bundleIDs(depth1),
		"depth 1 reaches the folder's children and stops; the docs' author is one hop further out")
	for _, doc := range docs {
		require.True(t, hasEdge(depth1.Bundle.GetEdges(), folder, doc, GraphEdgeLabelChild),
			"the folder -> %s child edge must be recorded", doc)
	}

	depth2, err := eng.Execute(ctx, fmt.Sprintf(
		`withDepth(concept==%s && createdBy==%q, 2)`, tvFolder, owner))
	require.NoError(t, err)
	require.ElementsMatch(t, append([]string{folder, author}, docs...), bundleIDs(depth2),
		"depth 2 takes the second hop, through each doc's interactsWith / createdBy edge")
	require.True(t, hasEdge(depth2.Bundle.GetEdges(), docs[0], author, relationshipTypeInteracts),
		"the doc -> author interactsWith edge must be recorded at depth 2")
	require.True(t, hasEdge(depth2.Bundle.GetEdges(), docs[0], author, relationshipTypeCreatedBy),
		"the doc -> author createdBy edge must be recorded at depth 2")
}

// TestRelationshipDispatch_RejectsAnUnknownFunction covers the dispatch
// switch's default arm, which no query string can reach: the parser turns an
// unrecognised wrapper name into a generic FunctionCallExpr rather than a
// RelationshipExpression, so the only caller that can present an unknown
// function is Go code constructing the node itself -- which is exactly what a
// future relationship function would be mid-wiring.
func TestRelationshipDispatch_RejectsAnUnknownFunction(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-dispatch")
	sfx := uniqueSuffix("rel-dispatch")
	owner := "kb:" + sfx

	s := &traversalSeeder{
		t: t, ctx: ctx, db: db, sfx: sfx, owner: owner,
		base: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	}
	s.row(tvPerson, "p", 2, nil)

	plan, err := eng.Parse(fmt.Sprintf(`concept==%s && createdBy==%q`, tvPerson, owner))
	require.NoError(t, err)

	expr := &RelationshipExpression{Function: RelationshipFunction("siblingOf"), Target: plan.Root}
	_, err = eng.evaluateRelationshipExpression(ctx, expr, nil, eng.config.MaxResults)
	require.Error(t, err)
	require.Contains(t, err.Error(), "siblingOf",
		"an unsupported relationship function must be refused by name, not silently resolved to nothing")
}

// TestRelationshipBidirectional_SkipsIdCanonicalization is a CHARACTERIZATION
// test recording why direction="bidirectional" is being removed before v1
// (memql#3658). DELETE IT ALONG WITH THE FEATURE.
//
// `bidirectional` is accepted by normalizeRelationshipDefinition, is honoured
// by every resolver in executor_relationship.go, and has ZERO declarations
// across the whole corpus. What it does NOT have is the two id canonicalizers,
// both of which test the string literal "outgoing":
//
//	partition_context.go  canonicalizeRelationshipFields      (the WRITE path)
//	executor_filter.go    canonicalizeRelationshipFieldValue  (the FILTER path)
//
// So a bidirectional relationship field is skipped by both. Its ids persist in
// whatever shape the caller sent, and the `(concept, id)` lookups that assume
// the canonical form quietly miss -- while an outgoing field on the same row,
// written in the same insert, is rewritten. That divergence is invisible: no
// error, no warning, and the write succeeds.
//
// The two fields below differ ONLY in direction. Same relationship type, same
// declared payload type, same insert, both targets registered concepts.
func TestRelationshipBidirectional_SkipsIdCanonicalization(t *testing.T) {
	mountTraversalFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-bidi")
	sfx := uniqueSuffix("rel-bidi")

	res, err := eng.Execute(ctx, fmt.Sprintf(
		`insert(%q, id=%q, payload={"outgoingRef":"bare-doc","bidiRef":"bare-person","label":"probe"})`,
		tvProbe, "probe-"+sfx))
	require.NoError(t, err)
	require.NotNil(t, res.Bundle)
	require.NotEmpty(t, res.Bundle.Nodes)
	storedId := res.Bundle.Nodes[0].GetId()

	stored := latestPayload(t, ctx, db, tvProbe, storedId)

	require.Equal(t, tvDoc+":bare-doc", stored["outgoingRef"],
		"CONTROL: an outgoing relationship field IS canonicalized on write")

	require.Equal(t, "bare-person", stored["bidiRef"],
		"EVIDENCE FOR REMOVAL: the bidirectional field on the same row, written by the same "+
			"insert, is skipped by canonicalizeRelationshipFields -- it tests the literal "+
			"\"outgoing\". The bare id persists, and every (concept, id) lookup that assumes "+
			"canonical form misses it. If this assertion starts failing because the value is now "+
			"%q, bidirectional has been taught to the write path and this test should be "+
			"re-read rather than re-pinned.", tvPerson+":bare-person")
}
