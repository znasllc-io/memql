package memql

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestModelSetupDBAdmissionAndCrossReplicaRevision(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(peer, memorynodes.DefaultRegistry()))
	key := uniqueSuffix("model-setup")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	model := key + "-chat"
	insert := func(conceptID, rowID string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		row := memorynodes.MemoryNode{ID: rowID, Concept: conceptID, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err = db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptID).Where("id = ?", rowID).Exec(context.Background())
			require.NoError(t, err)
		})
	}
	insert(WorkerRegistrationConcept, registration, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Test machine", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "capabilities": []string{"MODEL"}, "concurrency": map[string]any{}, "labels": map[string]any{"model:" + model: "structured=1,tools=1"}, "hardware": map[string]any{"memoryBytes": 64 << 30, "gpu": map[string]any{"backend": "metal"}, "runtimes": []any{map[string]any{"name": "ollama"}}}, "platformInfo": map[string]any{"os": "darwin"}})
	insert(ModelProfileConcept, ModelProfileConcept+":"+model, map[string]any{"modelId": model, "category": "text", "runtime": "ollama", "memoryNeedBytes": 8 << 30, "flags": []string{"structured", "tools"}, "minMachineClass": "16", "active": true})
	actor := auth.ContextWithUserActor(context.Background(), owner)
	_, err = eng.evaluateFleetSetModelSetup(auth.ContextWithUserActor(context.Background(), "someone-else"), map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"tools": model}})
	require.ErrorContains(t, err, "no machine of yours")
	_, err = eng.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"vision": model}})
	require.ErrorContains(t, err, "support for vision")
	// Two independent engines share no local setup state or lock. The database
	// fence/revision admits one update, even when two BFF replicas race.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, engine := range []*MemQLEngine{eng, peer} {
		wg.Add(1)
		go func(e *MemQLEngine) {
			defer wg.Done()
			<-start
			_, err := e.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"tools": model, "structured": model}})
			errs <- err
		}(engine)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, refused := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			refused++
			t.Logf("refused: %v", err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, refused)
	row, err := peer.modelSetupRow(actor, registration)
	require.NoError(t, err)
	setup := ModelSetupFromRow(row["modelSetup"])
	require.NotEmpty(t, setup.Revision)
	require.Equal(t, model, setup.Choices["tools"])
	_, err = eng.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{}})
	require.Error(t, err)
	// A raw write cannot bypass the memory and capability checks.
	err = validateWorkerConsentServerOnly(actor, row, map[string]any{"ownerUserId": owner, "modelSetup": map[string]any{"revision": "forged", "choices": map[string]any{"tools": "unknown"}}})
	require.ErrorContains(t, err, "modelSetup")
}

// THE WAY BACK FROM A SAVED SETUP (fleetClearModelSetup). Saving always mints a
// revision and ModelSetup.Allows reads an empty revision as automatic, so before
// this write a machine could never read as automatic again, and "Use suggested"
// left it on saved choices capped at ModelSetupContextLimit. Every refusal below
// also reads the stored row: a refusal raised after the write landed would pass.
type clearedSetupFixture struct {
	shareDirFixture
	actor        context.Context
	registration string
	picked       string
	leftOut      string
}

func newClearedSetupFixture(t *testing.T) clearedSetupFixture {
	t.Helper()
	eng, db, _ := sharedReadMergeEngine(t)
	base := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("model-setup-clear")}
	owner := base.person("owner", true)
	f := clearedSetupFixture{
		shareDirFixture: base,
		actor:           auth.ContextWithUserActor(context.Background(), owner),
		registration:    base.id(WorkerRegistrationConcept, "machine"),
		picked:          base.prefix + "-picked",
		leftOut:         base.prefix + "-left-out",
	}
	base.insert(WorkerRegistrationConcept, f.registration, map[string]any{
		"ownerUserId": owner, "identityId": "test", "name": "Test machine", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"capabilities": []string{"MODEL"}, "concurrency": map[string]any{},
		"labels":       map[string]any{"model:" + f.picked: "structured=1,tools=1", "model:" + f.leftOut: "structured=1,tools=1"},
		"hardware":     map[string]any{"memoryBytes": 64 << 30, "gpu": map[string]any{"backend": "metal"}, "runtimes": []any{map[string]any{"name": "ollama"}}},
		"platformInfo": map[string]any{"os": "darwin"},
	})
	for _, model := range []string{f.picked, f.leftOut} {
		base.insert(ModelProfileConcept, ModelProfileConcept+":"+model, map[string]any{"modelId": model, "category": "text", "runtime": "ollama", "memoryNeedBytes": 8 << 30, "flags": []string{"structured", "tools"}, "minMachineClass": "16", "active": true})
	}
	return f
}

// save stores a setup through the one owner write and returns its revision.
func (f clearedSetupFixture) save(revision string, choices map[string]any) string {
	f.t.Helper()
	rows, err := f.eng.evaluateFleetSetModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision, "choices": choices})
	require.NoError(f.t, err)
	saved := virtualRowPayload(f.t, rows)["revision"].(string)
	require.NotEmpty(f.t, saved)
	return saved
}

func (f clearedSetupFixture) clear(ctx context.Context, revision string) error {
	_, err := f.eng.evaluateFleetClearModelSetup(ctx, map[string]any{"registrationId": f.registration, "revision": revision})
	return err
}

func (f clearedSetupFixture) stored(eng *MemQLEngine) ModelSetup {
	f.t.Helper()
	row, err := eng.modelSetupRow(f.actor, f.registration)
	require.NoError(f.t, err)
	return ModelSetupFromRow(row["modelSetup"])
}

// versions counts the row's stored versions: a write that decided to do nothing
// must not append one, and a refused write must not have appended one first.
func (f clearedSetupFixture) versions() int64 {
	f.t.Helper()
	count, err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", WorkerRegistrationConcept).Where("id = ?", f.registration).Count(context.Background())
	require.NoError(f.t, err)
	return count
}

// The whole sentence, because the fence's own "row version changed" contains the
// word too, and a person should be told in words.
const modelSetupChangedRefusal = "The model setup changed. Reload it and try again."

// engineWithRenameOnWrite is a second replica whose writes of this concept see an
// ordinary write of the same row land mid-write, on the attempts rename chooses.
func (f clearedSetupFixture) engineWithRenameOnWrite(rename func(attempt int) bool) (writer struct {
	engine   *MemQLEngine
	attempts *int
}) {
	f.t.Helper()
	engine, err := New(f.db)
	require.NoError(f.t, err)
	require.NoError(f.t, engine.Init(memorynodes.DefaultRegistry()))
	attempts := 0
	engine.SetBeforeWriteHookSource("rename", map[string][]BeforeWriteHook{WorkerRegistrationConcept: {{
		Name: "rename", On: "update",
		Apply: func(context.Context, map[string]any) error {
			attempts++
			if rename(attempts) {
				call, err := langparser.RenderCall("renameWorker", map[string]any{"registrationId": f.registration, "displayName": "Renamed"})
				require.NoError(f.t, err)
				_, err = f.eng.Execute(f.actor, call)
				require.NoError(f.t, err)
			}
			return nil
		},
	}}})
	writer.engine, writer.attempts = engine, &attempts
	return writer
}

func virtualRowPayload(t *testing.T, rows []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	require.Len(t, rows, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &payload))
	return payload
}

func TestAClearedMachineServesAnyModelItOffersAndContextsAbove32K(t *testing.T) {
	f := newClearedSetupFixture(t)
	peer, err := New(f.db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	revision := f.save("", map[string]any{"tools": f.picked})

	// The provider's own gate, fed the setup as the row carries it.
	served := func(setup ModelSetup, model string, needs FleetNeeds) bool {
		m := FleetModelAdvertisedOnEveryMachineForTest(FleetModel{ModelId: model, Tools: true, ContextWindow: 131072, Machines: []FleetMachine{{Online: true, ModelSetup: setup}}})
		ok, _ := m.eligibleFor(needs)
		return ok
	}
	narrow, wide := FleetNeeds{Tools: true}, FleetNeeds{Tools: true, MinContextWindow: 2 * ModelSetupContextLimit}
	saved := f.stored(f.eng)
	require.True(t, served(saved, f.picked, narrow), "the saved pick serves its task")
	require.False(t, served(saved, f.leftOut, narrow), "a saved setup leaves the other model out")
	require.False(t, served(saved, f.picked, wide), "a saved setup caps its machine at 32K")

	// Through the builtin the client calls, not only the evaluator behind it.
	call, err := langparser.RenderCall("builtin fleetClearModelSetup", map[string]any{"registrationId": f.registration, "revision": revision})
	require.NoError(t, err)
	res, err := f.eng.Execute(f.actor, call)
	require.NoError(t, err)
	receipts := MaterializeRows(res.OutputPayload())
	require.Len(t, receipts, 1)
	require.Equal(t, true, receipts[0]["automatic"], "the receipt says what is now true")

	// A second replica shares no local state with the one that wrote.
	cleared := f.stored(peer)
	require.Empty(t, cleared.Revision)
	require.Empty(t, cleared.Choices)
	require.True(t, served(cleared, f.leftOut, narrow), "the model the setup left out serves again")
	require.True(t, served(cleared, f.picked, wide), "a context above 32K serves again")
	require.True(t, served(cleared, f.leftOut, wide))
	require.True(t, cleared.Allows(f.leftOut, "vision", 262144), "no purpose stays narrowed by the old choices")

	// What the Fleet page reads, so it can say "MemQL chooses automatically".
	rows, err := f.eng.evaluateFleetModelSetup(f.actor, map[string]any{"registrationId": f.registration})
	require.NoError(t, err)
	view := virtualRowPayload(t, rows)
	require.Equal(t, "", view["revision"])
	require.Empty(t, view["choices"])
	require.Equal(t, "", view["warning"])

	// Every heartbeat and rename read-merges over this field. An ordinary write
	// by the owner must carry the cleared block through, and be admitted.
	rename, err := langparser.RenderCall("renameWorker", map[string]any{"registrationId": f.registration, "displayName": "Renamed"})
	require.NoError(t, err)
	_, err = f.eng.Execute(f.actor, rename)
	require.NoError(t, err)
	require.Empty(t, f.stored(peer).Revision, "a later write must not bring the old setup back")
}

func TestClearingAModelSetupRefusesAStaleRevisionAndAMachineThatIsNotYours(t *testing.T) {
	f := newClearedSetupFixture(t)
	revision := f.save("", map[string]any{"tools": f.picked})
	operator := f.person("operator", true)
	stranger := f.person("stranger", true)
	before := f.versions()

	refusals := []struct {
		name     string
		ctx      context.Context
		revision string
		want     string
	}{
		{"a stale revision", f.actor, "an-older-revision", modelSetupChangedRefusal},
		{"no revision, as a page that last read it automatic sends", f.actor, "", modelSetupChangedRefusal},
		{"somebody else's machine", auth.ContextWithUserActor(context.Background(), stranger), revision, "no machine of yours"},
		// The row's tier ORs in clusterOwner, so a write guard alone would let a
		// cluster owner through. The owner's own list is what refuses them.
		{"a cluster owner who does not own it", shareDirActor(context.Background(), operator, auth.RoleOwner), revision, "no machine of yours"},
		{"nobody at all", context.Background(), revision, "no machine of yours"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, f.clear(tc.ctx, tc.revision), tc.want)
			require.Equal(t, revision, f.stored(f.eng).Revision, "a refused clear must leave the setup in place")
			require.Equal(t, before, f.versions(), "a refused clear must not append a version")
		})
	}

	t.Run("the revision the clear replaced", func(t *testing.T) {
		require.NoError(t, f.clear(f.actor, revision))
		require.ErrorContains(t, f.clear(f.actor, revision), modelSetupChangedRefusal)
	})
}

func TestClearingARevokedMachineIsRefused(t *testing.T) {
	f := newClearedSetupFixture(t)
	revision := f.save("", map[string]any{"tools": f.picked})
	row, err := f.eng.modelSetupRow(f.actor, f.registration)
	require.NoError(t, err)
	row["revokedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	delete(row, "id")
	f.insert(WorkerRegistrationConcept, f.registration, row)
	before := f.versions()
	require.ErrorContains(t, f.clear(f.actor, revision), "Pair this machine again")
	require.Equal(t, revision, f.stored(f.eng).Revision, "a refused clear must leave the setup in place")
	require.Equal(t, before, f.versions(), "a refused clear must not append a version")
}

// Nothing to clear is a settled answer, not a reason to write. A machine on
// automatic gets no new version, and the owner can still save from there.
func TestClearingAnAutomaticMachineWritesNothing(t *testing.T) {
	f := newClearedSetupFixture(t)
	before := f.versions()
	call, err := langparser.RenderCall("builtin fleetClearModelSetup", map[string]any{"registrationId": f.registration, "revision": ""})
	require.NoError(t, err)
	_, err = f.eng.Execute(f.actor, call)
	require.NoError(t, err)
	require.Equal(t, before, f.versions(), "a machine already on automatic has nothing to write")

	first := f.save("", map[string]any{"tools": f.picked})
	require.NoError(t, f.clear(f.actor, first))
	second := f.save("", map[string]any{"tools": f.leftOut})
	require.NotEqual(t, first, second)
	require.True(t, f.stored(f.eng).Allows(f.leftOut, "tools", 8192))
	require.False(t, f.stored(f.eng).Allows(f.picked, "tools", 8192), "saving again takes the machine off automatic")
}

// A clear and "serve nothing" must stay two states: the second is a saved setup,
// with a revision, and it still refuses every model.
func TestASavedSetupWithNoChoicesStillServesNothingAfterAClear(t *testing.T) {
	f := newClearedSetupFixture(t)
	require.NoError(t, f.clear(f.actor, f.save("", map[string]any{"tools": f.picked})))
	f.save("", map[string]any{})
	setup := f.stored(f.eng)
	require.NotEmpty(t, setup.Revision)
	require.False(t, setup.Allows(f.picked, "tools", 8192))
}

// fleetClearModelSetup is the one way to the empty block. The raw literal never
// consults @serverOnly, so the consent guard has to refuse it on its own.
func TestOnlyTheOwnerWriteCanClearAModelSetup(t *testing.T) {
	f := newClearedSetupFixture(t)
	revision := f.save("", map[string]any{"tools": f.picked})
	operator := f.person("operator", true)
	empty := map[string]any{"modelSetup": map[string]any{}}
	direct, err := langparser.RenderCall("setWorkerModelSetup", map[string]any{"registrationId": f.registration, "setup": map[string]any{}})
	require.NoError(t, err)
	cases := []struct {
		name      string
		ctx       context.Context
		statement string
	}{
		{"the owner's raw write", f.actor, rawRegistrationWrite(f.registration, empty)},
		{"a cluster owner's raw write", shareDirActor(context.Background(), operator, auth.RoleOwner), rawRegistrationWrite(f.registration, empty)},
		{"the owner calling the internal mutation", f.actor, direct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.eng.Execute(tc.ctx, tc.statement)
			require.Error(t, err)
			require.Equal(t, revision, f.stored(f.eng).Revision, "a refused write must leave the setup in place")
		})
	}
}

// THE GAP ONLY ADMISSION'S FENCE COVERS, without leaving it to timing. decide runs
// after the revision check and before the write, so a rival landing there is
// exactly what that fence exists for. The write path fences this concept itself
// just before its read-merge, so a fence taken that late would witness the rival's
// version, pass, and overwrite a decision the check never saw, which is also why
// the hook test below holds with or without this one. The racing test cannot be
// relied on here: the faster write almost always lands first.
func TestARivalLandingAfterAdmissionRefusesTheAdmittedWrite(t *testing.T) {
	f := newClearedSetupFixture(t)
	revision := f.save("", map[string]any{"tools": f.picked})
	rival := ""
	err := f.eng.writeModelSetup(f.actor, f.registration, revision, func(context.Context, map[string]any) (map[string]any, error) {
		rival = f.save(revision, map[string]any{"tools": f.leftOut})
		return map[string]any{}, nil
	})
	require.ErrorContains(t, err, modelSetupChangedRefusal)
	require.Equal(t, rival, f.stored(f.eng).Revision, "the rival's decision must stand")
}

// A rival landing while the write is under way, on another replica. A before-write
// hook runs after the write's read-merge and before it commits, so the interleaving
// is exact. The fence refuses the write, and the next attempt's revision check is
// what tells the person, in words, that their read is out of date.
func TestARivalLandingMidWriteRefusesTheWriteItBeat(t *testing.T) {
	cases := []struct {
		name  string
		write func(e *MemQLEngine, f clearedSetupFixture, revision string) error
		rival func(f clearedSetupFixture, revision string) string
	}{
		{
			"a clear does not overwrite a rival save",
			func(e *MemQLEngine, f clearedSetupFixture, revision string) error {
				_, err := e.evaluateFleetClearModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision})
				return err
			},
			func(f clearedSetupFixture, revision string) string {
				return f.save(revision, map[string]any{"tools": f.leftOut})
			},
		},
		{
			"a save does not overwrite a rival clear",
			func(e *MemQLEngine, f clearedSetupFixture, revision string) error {
				_, err := e.evaluateFleetSetModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision, "choices": map[string]any{"tools": f.leftOut}})
				return err
			},
			func(f clearedSetupFixture, revision string) string {
				require.NoError(f.t, f.clear(f.actor, revision))
				return ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newClearedSetupFixture(t)
			writer, err := New(f.db)
			require.NoError(t, err)
			require.NoError(t, writer.Init(memorynodes.DefaultRegistry()))
			revision := f.save("", map[string]any{"tools": f.picked})
			landed, standing := false, ""
			writer.SetBeforeWriteHookSource("rival", map[string][]BeforeWriteHook{WorkerRegistrationConcept: {{
				Name: "rival", On: "update",
				Apply: func(context.Context, map[string]any) error {
					if !landed {
						landed, standing = true, tc.rival(f, revision)
					}
					return nil
				},
			}}})
			require.ErrorContains(t, tc.write(writer, f, revision), modelSetupChangedRefusal)
			require.True(t, landed, "the rival never landed, so this proved nothing")
			require.Equal(t, standing, f.stored(f.eng).Revision, "the rival's decision must stand")
		})
	}
}

// A HEARTBEAT IS NOT A RIVAL. It moves the row's version and leaves the revision
// alone, so the fence refuses the write and the next attempt goes through, keeping
// what the heartbeat wrote. A rename stands in for one: any ordinary write of the
// row moves its version the same way.
func TestAHeartbeatRacingTheWriteDoesNotRefuseIt(t *testing.T) {
	cases := []struct {
		name  string
		write func(e *MemQLEngine, f clearedSetupFixture, revision string) error
		check func(t *testing.T, setup ModelSetup)
	}{
		{
			"a clear",
			func(e *MemQLEngine, f clearedSetupFixture, revision string) error {
				_, err := e.evaluateFleetClearModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision})
				return err
			},
			func(t *testing.T, setup ModelSetup) { require.Empty(t, setup.Revision) },
		},
		{
			"a save",
			func(e *MemQLEngine, f clearedSetupFixture, revision string) error {
				_, err := e.evaluateFleetSetModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision, "choices": map[string]any{"tools": f.leftOut}})
				return err
			},
			func(t *testing.T, setup ModelSetup) { require.NotEmpty(t, setup.Revision) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newClearedSetupFixture(t)
			writer := f.engineWithRenameOnWrite(func(attempt int) bool { return attempt == 1 })
			revision := f.save("", map[string]any{"tools": f.picked})
			require.NoError(t, tc.write(writer.engine, f, revision))
			require.Equal(t, 2, *writer.attempts, "the write had to try again, or this proved nothing")
			tc.check(t, f.stored(f.eng))
			row, err := f.eng.modelSetupRow(f.actor, f.registration)
			require.NoError(t, err)
			require.Equal(t, "Renamed", row["displayName"], "the retry must keep what the heartbeat wrote")
		})
	}
}

// The retries are bounded, and running out of them changes nothing.
func TestAWriteThatNeverStopsRacingGivesUpWithoutWriting(t *testing.T) {
	f := newClearedSetupFixture(t)
	writer := f.engineWithRenameOnWrite(func(int) bool { return true })
	revision := f.save("", map[string]any{"tools": f.picked})
	_, err := writer.engine.evaluateFleetClearModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision})
	require.ErrorContains(t, err, "kept changing")
	require.Equal(t, modelSetupWriteAttempts, *writer.attempts)
	require.Equal(t, revision, f.stored(f.eng).Revision, "giving up must leave the setup in place")
}

// Two BFF replicas share no lock. The row version fence and the revision admit
// one of a clear and a save, and the row then says what the winner decided.
func TestAClearAndASaveRacingOnTwoReplicasAdmitOne(t *testing.T) {
	f := newClearedSetupFixture(t)
	peer, err := New(f.db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	revision := f.save("", map[string]any{"tools": f.picked})
	for attempt := 0; attempt < 5; attempt++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		var clearErr, saveErr error
		var saveRows []memorynodes.MemoryNode
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			clearErr = f.clear(f.actor, revision)
		}()
		go func() {
			defer wg.Done()
			<-start
			saveRows, saveErr = peer.evaluateFleetSetModelSetup(f.actor, map[string]any{"registrationId": f.registration, "revision": revision, "choices": map[string]any{"tools": f.leftOut}})
		}()
		close(start)
		wg.Wait()
		require.NotEqual(t, clearErr == nil, saveErr == nil, "exactly one must win, got clear=%v save=%v", clearErr, saveErr)
		stored := f.stored(f.eng)
		if clearErr == nil {
			require.Empty(t, stored.Revision, "the clear won, so the machine is on automatic")
			revision = f.save("", map[string]any{"tools": f.picked})
			continue
		}
		saved := virtualRowPayload(t, saveRows)["revision"].(string)
		require.Equal(t, saved, stored.Revision, "the save won, so the machine is on its choices")
		revision = saved
	}
}
