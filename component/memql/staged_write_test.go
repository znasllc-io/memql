package memql

// staged_write_test.go -- coverage for the WRITE chokepoint's half of the
// STAGED DATA tier (epic memql#3974, task memql#3985).
//
// Two things are under test and they are not equally important.
//
// The EVENT SUPPRESSION is the visible feature, and it is asserted against a real
// engine, a real Postgres and a real events.Bus -- never against the predicate
// alone. A test that only checks withholdGraphWriteEvent() returns true would
// still pass if someone deleted the guard from executeWrite, which is the entire
// failure it exists to catch. So every event case here writes an actual row and
// listens on an actual bus, and each one opens with a POSITIVE CONTROL on a live
// concept: "nothing arrived" is the same observation as "the subscription was
// never wired", and only the control tells them apart.
//
// The PARTIAL-UPDATE case is the one that matters more, despite testing an
// absence of behaviour. Event suppression failing is a visible leak somebody
// notices. The read-merge being "helpfully" gated by a later pass -- aligning
// every read on the staged marker, which is a reasonable-sounding thing to do --
// is SILENT DATA LOSS on every partial update to a staged concept, and nothing
// reports it. That test is written to fail loudly and name the cause.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/provenance"
)

// --- fixtures -------------------------------------------------------------

// stagedWriteConcept is the concept these DB tests write to. v1:cluster:node is
// chosen because the read-merge suite already proves its createNode /
// updateNodeHealth pair works end to end against a live engine (memql#1628), so
// a failure here is about staging rather than about the fixture.
//
// It is a CORE concept, and in production only an author-promoted concept is
// ever staged. That difference does not reach this seam: the write chokepoint
// asks conceptDataIsStaged(name), which is a lookup on the in-memory marker and
// does not consult the promotion registry at all. Marking a core concept is
// therefore the cheapest way to drive REAL mutations, through the REAL DSL
// layer, into a REAL database -- which is what these tests need and what a
// synthesised authored concept would not give without a fixture large enough to
// hide a bug in.
const stagedWriteConcept = "v1:cluster:node"

// eventRecorder subscribes to a topic pattern and collects what arrives.
//
// events.Bus.Publish delivers in a goroutine per subscriber, so "no event" can
// only ever be observed by waiting. awaitOne bounds the positive case tightly;
// requireSilence spends a longer, deliberately generous window proving the
// negative -- an under-short window there would turn a real leak into a pass.
type eventRecorder struct {
	got         chan events.Event
	unsubscribe func()
}

func recordEvents(t *testing.T, bus *events.Bus, pattern string) *eventRecorder {
	t.Helper()
	r := &eventRecorder{got: make(chan events.Event, 64)}
	r.unsubscribe = bus.Subscribe(pattern, func(e events.Event) {
		select {
		case r.got <- e:
		default:
		}
	}, events.WithSubscriberName("test:staged-write"))
	t.Cleanup(func() {
		if r.unsubscribe != nil {
			r.unsubscribe()
		}
	})
	return r
}

// awaitOne is the POSITIVE control: it fails if the expected event does not
// arrive, which is what makes a later "nothing arrived" assertion mean anything.
func (r *eventRecorder) awaitOne(t *testing.T, why string) events.Event {
	t.Helper()
	select {
	case e := <-r.got:
		return e
	case <-time.After(5 * time.Second):
		t.Fatalf("positive control failed: %s -- no event arrived, so this test cannot distinguish suppression from a dead subscription", why)
		return events.Event{}
	}
}

func (r *eventRecorder) requireSilence(t *testing.T, why string) {
	t.Helper()
	select {
	case e := <-r.got:
		t.Fatalf("%s -- got topic %q carrying %d payload fields; the staged row was published to every subscriber on the bus", why, e.Topic, len(e.Payload))
	case <-time.After(2 * time.Second):
	}
}

// stagedWriteEngine is readMergeTestEngine plus a real event bus, since the
// engine's publish helpers no-op on a nil bus and every event assertion below
// would then pass for the wrong reason.
//
// The PRIVATE boot is deliberate while most of the package borrows
// sharedReadMergeEngine (memql#4075): these tests mark v1:cluster:node -- a
// CORE concept -- data-staged on the engine and never clear it, because the
// mark used to die with the per-test engine. On a shared engine the stranded
// mark makes the NEXT test's unstaged positive control silent: both
// Publishes* tests failed exactly that way when this file was converted
// ("an UNSTAGED write must publish graph.node.created -- no event arrived").
func stagedWriteEngine(t *testing.T) (*MemQLEngine, context.Context) {
	t.Helper()
	eng, _, ctx := readMergeTestEngine(t)
	eng.SetEventBus(events.NewBus())
	return eng, auth.ContextWithUserActor(ctx, "system:staged-write-test")
}

func stagedWriteNodeId(name string) string {
	return fmt.Sprintf("node-staged-%s-%d", name, os.Getpid())
}

// --- the predicate --------------------------------------------------------

// TestStagedWrite_WithholdGraphWriteEventDefaultsToEmitting is the counterpart
// of the tier's default-is-live rule, at the seam that consumes it.
//
// A predicate that answered true by mistake would silence the graph event bus
// for every concept in the installation at once. Automations stop firing, CDC
// subscribers stop seeing writes, and every row still lands in the database --
// so nothing looks broken from the storage side and nothing reacts.
func TestStagedWrite_WithholdGraphWriteEventDefaultsToEmitting(t *testing.T) {
	e := promoteConceptEngine(t)
	if e.withholdGraphWriteEvent(stagedWriteConcept) {
		t.Error("an unmarked concept withholds its write events: every automation in the installation would stop firing while every write still succeeded")
	}
	if e.withholdGraphWriteEvent("") {
		t.Error("the empty concept name withholds write events")
	}

	e.markConceptDataStaged(stagedWriteConcept)
	if !e.withholdGraphWriteEvent(stagedWriteConcept) {
		t.Error("a staged concept still publishes its write events")
	}
	// Staging one concept must not silence its neighbours.
	if e.withholdGraphWriteEvent("v1:cluster:nodeType") {
		t.Error("staging one concept withheld another concept's write events")
	}
}

// --- the read-merge: THE IMPORTANT ONE ------------------------------------

// TestStagedWrite_PartialUpdatePreservesOmittedFields is the guard against the
// most expensive mistake available on this path, and it is a mistake that looks
// like tidying up.
//
// executeWrite read-merges on update: loadPriorPayload fetches the stored row and
// the partial payload is splatted on top, so a caller sending {id, health} keeps
// address, nodeType and everything else. The staged-DATA tier adds a marker that
// hides a concept's rows from readers -- and loadPriorPayload is, syntactically,
// a reader. A later pass making "every read consults conceptDataIsStaged" true
// everywhere would gate it, run the suite, and see green from every test that
// does not write to a staged concept.
//
// WHAT THAT COSTS: the staged prior row comes back empty, exists is false, the
// read-merge has nothing to merge onto, and the UPDATE silently becomes a CREATE.
// Every field the caller did not restate is gone. No error, no warning, a
// successful mutation, and a row that has forgotten most of itself -- on data
// whose entire purpose was to sit there accumulating until training made it
// visible.
//
// So: this test stages the concept FIRST, then issues minimal writes, and
// asserts the omitted fields survived. If loadPriorPayload ever grows a staged
// gate, this is the test that goes red, and this comment is why.
//
// It covers BOTH authoring forms, because the failure they produce is not the
// same failure and only one of them is loud:
//
//   - an update{}-authored mutation runs with requirePrior=true, so a prior row
//     the gate has hidden surfaces as an explicit "row does not exist, use
//     insert()" error. Bad, but visible.
//   - an insert{}-authored mutation with a partial payload -- the memql#1709
//     shape, still how leaveSpace and revokeDelegation are written -- runs with
//     requirePrior=false, so a hidden prior row is simply "a create". Every
//     OPTIONAL field on the stored row is written away, schema validation passes
//     because the required ones were supplied, the mutation returns success, and
//     nothing anywhere reports it. That is the silent half, and it is the reason
//     this test exists.
func TestStagedWrite_PartialUpdatePreservesOmittedFields(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	nodeId := stagedWriteNodeId("partial-update")

	// `region` is an OPTIONAL concept field (declared without `!`), so a later
	// write that omits it still validates -- which is exactly what makes its
	// loss silent rather than a rejection.
	storedId := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.42:50051",
		"health":   "healthy",
		"lastSeen": "2026-08-16T00:00:00Z",
		"region":   "us-central1",
	})

	// Stage the concept AFTER the seed row exists: the tier's whole shape is
	// "rows arrive, then the concept is trained", so the row under test is
	// exactly the kind of row staging is for.
	eng.markConceptDataStaged(stagedWriteConcept)
	require.True(t, eng.conceptDataIsStaged(stagedWriteConcept), "fixture: the concept must actually be staged or this test proves nothing")

	conceptMeta, err := eng.concepts.Get(stagedWriteConcept)
	require.NoError(t, err)

	// --- the update{} form: LOUD if the read-merge is gated ---------------
	//
	// The MINIMAL update -- id + the two changed fields, and deliberately NOT
	// address, nodeType or region.
	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
		"id":       nodeId,
		"health":   "degraded",
		"lastSeen": "2026-08-16T01:00:00Z",
	})

	// Read the stored row back through the engine's OWN read-merge seam rather
	// than through a query: the query path is memql#3983's to gate, and this
	// test must keep meaning what it means after that lands.
	payload, exists, err := eng.loadPriorPayload(ctx, conceptMeta, storedId)
	require.NoError(t, err)
	require.True(t, exists,
		"loadPriorPayload cannot see the staged row. If a staged gate was added to it, EVERY partial write to a staged concept now drops every field the caller did not restate -- an UPDATE degraded into a CREATE. Revert that gate; see the comment on loadPriorPayload.")

	require.Equal(t, "degraded", payload["health"], "the update must still apply")
	require.Equal(t, "10.0.0.42:50051", payload["address"],
		"address was dropped by a partial update to a STAGED concept -- the read-merge lost the prior row, so the update wrote a fresh record instead of merging onto it (memql#3985).")
	require.Equal(t, "bff", payload["nodeType"], "nodeType was dropped -- same cause as address above.")
	require.Equal(t, "us-central1", payload["region"], "region was dropped -- same cause as address above.")

	// --- the insert{} form: SILENT if the read-merge is gated -------------
	//
	// Driven straight at executeInsert with a partial payload, which is the
	// memql#1709 chokepoint and the exact shape leaveSpace / revokeDelegation
	// still use. requirePrior is false here, so a hidden prior row raises
	// nothing at all -- it just becomes a create, and `region` (optional, so it
	// passes validation by its absence) is gone.
	// Provenance is what engine.go stamps onto a raw insert() before dispatch;
	// the row layer refuses every write without it, so omitting it here would
	// measure the wrong refusal.
	insertCtx := provenance.ContextWithProvenance(ctx, provenance.Direct("rawInsert:"+stagedWriteConcept))
	_, err = eng.executeInsert(insertCtx, MutationNode{
		Concept:    stagedWriteConcept,
		ID:         storedId,
		PayloadRaw: `{"nodeType":"bff","address":"10.0.0.42:50051","health":"draining"}`,
	})
	require.NoError(t, err, "a partial insert() onto an existing staged row must succeed")

	payload, exists, err = eng.loadPriorPayload(ctx, conceptMeta, storedId)
	require.NoError(t, err)
	require.True(t, exists, "the partial insert() lost the row entirely")
	require.Equal(t, "draining", payload["health"], "the partial insert() must still apply its change")
	require.Equal(t, "us-central1", payload["region"],
		"SILENT DATA LOSS: an insert{}-authored partial write to a STAGED concept dropped the optional field `region`, which the caller never mentioned and never intended to clear. This is what gating loadPriorPayload costs -- requirePrior=false means a hidden prior row raises no error, so the write succeeds and the row quietly forgets everything the caller did not restate. Revert the gate; see the comment on loadPriorPayload (memql#3985).")
	require.Equal(t, "2026-08-16T01:00:00Z", payload["lastSeen"],
		"SILENT DATA LOSS: lastSeen was dropped by an insert{}-authored partial write to a staged concept -- same cause as region above.")
}

// TestStagedWrite_CheckNodeExistsSeesStagedRows pins the second ungated read.
//
// checkNodeExists backs previewInsert's content-addressed id collision probe. If
// it were staged-filtered it would report an id as free while a staged row sits
// on it, and the caller -- told the id is unused -- inserts. The append-only
// store then files that write as a NEW VERSION of the row it was just told did
// not exist.
func TestStagedWrite_CheckNodeExistsSeesStagedRows(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	nodeId := stagedWriteNodeId("exists-probe")

	storedId := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.43:50051",
		"health":   "healthy",
	})
	require.True(t, eng.checkNodeExists(ctx, stagedWriteConcept, storedId), "fixture: the row must exist before staging")

	eng.markConceptDataStaged(stagedWriteConcept)

	require.True(t, eng.checkNodeExists(ctx, stagedWriteConcept, storedId),
		"the id-collision probe stopped seeing a staged row. previewInsert would now report the id as free, and the caller's insert would land as another version of the staged row it was told did not exist (memql#3985).")
}

// --- the write is ACCEPTED ------------------------------------------------

// TestStagedWrite_IsAcceptedNotRefused: the gate this task adds sits one branch
// away from memql#3756's retirement REFUSAL in the same function, and reaches
// the opposite verdict.
//
// Worth an explicit test because the symmetry is inviting and getting it wrong
// destroys the feature rather than degrading it: rows arriving before the
// concept is trained IS the staged-data tier. A staged concept that refused
// writes would have nothing to make visible at training time.
func TestStagedWrite_IsAcceptedNotRefused(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	nodeId := stagedWriteNodeId("accepted")

	eng.markConceptDataStaged(stagedWriteConcept)

	storedId := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.44:50051",
		"health":   "healthy",
	})
	require.NotEmpty(t, storedId, "a write to a staged concept must succeed")

	// Durable, addressable, and complete -- withheld from readers, not withheld
	// from the database.
	conceptMeta, err := eng.concepts.Get(stagedWriteConcept)
	require.NoError(t, err)
	payload, exists, err := eng.loadPriorPayload(ctx, conceptMeta, storedId)
	require.NoError(t, err)
	require.True(t, exists, "the staged write did not persist a row")
	require.Equal(t, "10.0.0.44:50051", payload["address"], "the staged row was written incomplete")
}

// --- the event suppression ------------------------------------------------

// TestStagedWrite_PublishesNoGraphNodeCreatedEvent is the substantive change.
//
// The event build in executeWrite copies the ENTIRE stored payload into the
// envelope twice -- flattened into the top level by maps.Copy and retained whole
// under "payload" -- and events.Bus.Publish fans a clone of that out to every
// subscriber whose pattern matches the topic. component/events holds no
// AccessContext and no authorization hook, so there is nothing downstream that
// could narrow it. Hiding the row from readers while emitting this event would
// publish the complete row to every in-process subscriber and call it hidden.
func TestStagedWrite_PublishesNoGraphNodeCreatedEvent(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	rec := recordEvents(t, eng.EventBus(), events.TopicNodeCreated(stagedWriteConcept))

	// POSITIVE CONTROL: unstaged, the event must arrive. Without this the
	// silence asserted below would also be produced by a broken subscription.
	liveId := stagedWriteNodeId("created-live")
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       liveId,
		"nodeType": "bff",
		"address":  "10.0.0.45:50051",
		"health":   "healthy",
	})
	got := rec.awaitOne(t, "an UNSTAGED write must publish graph.node.created")
	require.Equal(t, events.TopicNodeCreated(stagedWriteConcept), got.Topic)
	require.Equal(t, "10.0.0.45:50051", got.Payload["address"],
		"fixture check: the event really does carry the row's fields flattened into it, which is why it must be withheld for a staged concept")

	// Now stage, and write again.
	eng.markConceptDataStaged(stagedWriteConcept)
	stagedId := stagedWriteNodeId("created-staged")
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       stagedId,
		"nodeType": "bff",
		"address":  "10.0.0.46:50051",
		"health":   "healthy",
	})
	rec.requireSilence(t, "a write to a STAGED concept published graph.node.created")
}

// TestStagedWrite_PublishesNoGraphNodeUpdatedEvent covers the second emit.
//
// executeUpdate's build deliberately "mirrors the executeWrite publish shape",
// so it leaks the same complete row -- plus the PRIOR row's status as oldStatus.
// Suppressing only .created would hide a staged row's birth and then announce
// every subsequent write to it in full, which is a worse outcome than suppressing
// neither: it would look like the feature worked.
func TestStagedWrite_PublishesNoGraphNodeUpdatedEvent(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	rec := recordEvents(t, eng.EventBus(), events.TopicNodeUpdated(stagedWriteConcept))

	// POSITIVE CONTROL on a live concept.
	liveId := stagedWriteNodeId("updated-live")
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       liveId,
		"nodeType": "bff",
		"address":  "10.0.0.47:50051",
		"health":   "healthy",
	})
	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
		"id":       liveId,
		"health":   "degraded",
		"lastSeen": "2026-08-16T00:00:00Z",
	})
	got := rec.awaitOne(t, "an UNSTAGED update must publish graph.node.updated")
	require.Equal(t, "10.0.0.47:50051", got.Payload["address"],
		"fixture check: the .updated event carries the merged row too")

	// Seed a second row while still unstaged, then stage and update it. The
	// seed is an insert() and so emits .created, not .updated -- this recorder
	// listens on .updated alone and must stay silent through it.
	stagedId := stagedWriteNodeId("updated-staged")
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       stagedId,
		"nodeType": "bff",
		"address":  "10.0.0.48:50051",
		"health":   "healthy",
	})

	eng.markConceptDataStaged(stagedWriteConcept)
	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
		"id":       stagedId,
		"health":   "degraded",
		"lastSeen": "2026-08-16T00:00:00Z",
	})
	rec.requireSilence(t, "an update to a STAGED concept published graph.node.updated")
}

// TestStagedWrite_StillPublishesCacheInvalidate pins the ruling that the
// cache-invalidation emit is NOT suppressed, which is the asymmetry most likely
// to be "corrected" by someone applying the obvious symmetry.
//
// It is not the same emit. Its payload is {"concept": <id>} and its sole
// subscriber does not read the payload at all -- result_cache_invalidation.go
// recovers the concept from the TOPIC and evicts. A staged concept's NAME is
// public regardless: it is registered, resolvable and listed, which is exactly
// what separates this tier from memql#3928's construct staging.
//
// And suppressing it would cause the failure the tier exists to prevent. While a
// concept is staged its reads answer empty, and empty results are cacheable; with
// no invalidation the cache still holds "empty" at the moment memql#3986 trains
// the concept, so rows that just went live stay invisible until a TTL lapses or a
// node restarts. Rows intact, addressable, absent from every read -- the state
// authoring_concept_staged.go's fold refuses to point towards, arrived at from
// the other end.
func TestStagedWrite_StillPublishesCacheInvalidate(t *testing.T) {
	eng, ctx := stagedWriteEngine(t)
	rec := recordEvents(t, eng.EventBus(), events.TopicCacheInvalidateForConcept(stagedWriteConcept))

	eng.markConceptDataStaged(stagedWriteConcept)
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       stagedWriteNodeId("cache-invalidate"),
		"nodeType": "bff",
		"address":  "10.0.0.49:50051",
		"health":   "healthy",
	})

	got := rec.awaitOne(t, "a write to a staged concept must STILL invalidate the result cache")
	require.Equal(t, events.TopicCacheInvalidateForConcept(stagedWriteConcept), got.Topic)
	// The whole payload, so the "nothing to leak" half of the ruling is pinned
	// too: if a future change starts attaching row data here, this fails and the
	// suppression question has to be reopened.
	require.Equal(t, map[string]any{"concept": stagedWriteConcept}, got.Payload,
		"the cache-invalidation event grew a payload beyond the concept id; it is exempt from staged suppression only because it carries nothing about the row")
}
