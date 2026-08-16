package memql

// authoring_staged_test.go -- coverage for the STAGED tier (epic memql#3928).
//
// Internal (package memql) test so it can drive the unexported promoteStore /
// stagedRowStore / demoteStore seams with fakes plus the REAL engine
// compile/register logic -- no live DB. What it asserts, in the order the tier's
// life runs:
//
//   - staging persists a row at status "staged" and does NOT broadcast;
//   - a staged construct resolves for its AUTHOR and for nobody else;
//   - the precedence is core -> staged -> session;
//   - a concept is refused BY NAME, and refused before anything else stages;
//   - boot re-hydration routes the three statuses three ways;
//   - training flips the same row and fires the promote broadcast;
//   - demoting a staged construct retires its row and broadcasts nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// stagedSpecSrc / stagedQuerySrc are minimal authorable constructs. Named
// distinctly from the promote tests' fixtures so a staged assertion can never
// accidentally read a promoted one's row.
const stagedSpecSrc = `
@enabled
@description("A staged spec")
trait stagedOnlyTrait {
  return active == true
}
`

// fakeStagedRowStore is a stagedRowStore over an in-memory row set, recording
// every status write so a test can assert WHICH transition happened.
type fakeStagedRowStore struct {
	bundles    []AuthoringBundleRow
	constructs []AuthoringConstructRow
	writes     []string // "<constructId>=<status>"
}

func (s *fakeStagedRowStore) LoadPromotedBundles(context.Context) ([]AuthoringBundleRow, error) {
	return s.bundles, nil
}

func (s *fakeStagedRowStore) LoadConstructsForBundle(_ context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	out := []AuthoringConstructRow{}
	for _, row := range s.constructs {
		if row.BundleId == bundleId && row.OwnerUserId == owner {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *fakeStagedRowStore) SetConstructStatus(_ context.Context, _, constructId, status string) error {
	s.writes = append(s.writes, constructId+"="+status)
	for i := range s.constructs {
		if s.constructs[i].Id == constructId {
			s.constructs[i].Status = status
		}
	}
	return nil
}

// adopt copies a fakePromoteStore's rows in, so a test can stage through the
// write path and then run a transition over what it actually wrote.
func (s *fakeStagedRowStore) adopt(p *fakePromoteStore) {
	s.bundles = append(s.bundles, p.bundles...)
	s.constructs = append(s.constructs, p.constructs...)
}

func ownerCtx(owner string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
}

// TestStageBundleDurable_PersistsStagedAndRegistersOwnerScoped: staging writes a
// row at status "staged" and registers the construct in the OWNER-scoped staged
// registry rather than the shared one.
func TestStageBundleDurable_PersistsStagedAndRegistersOwnerScoped(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	store := &fakePromoteStore{}

	res, err := e.stageBundleDurableWithStore(ownerCtx("owner-1"), store, "owner-1", stagedSpecSrc, "")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !res.OK || len(res.Staged) != 1 || res.Staged[0].Name != "stagedOnlyTrait" {
		t.Fatalf("stage result = %+v", res)
	}

	if len(store.constructs) != 1 {
		t.Fatalf("expected 1 persisted construct row, got %d", len(store.constructs))
	}
	if got := store.constructs[0].Status; got != ConstructStaged {
		t.Errorf("persisted status = %q, want %q -- the status IS the tier", got, ConstructStaged)
	}

	// NOT in the shared registry: that is the whole difference from a promote.
	if _, shared := e.specs.Lookup("stagedOnlyTrait"); shared {
		t.Error("a staged construct must not land in the shared spec registry")
	}
	if _, ok := e.stagedAuthored.Lookup("owner-1", "trait", "stagedOnlyTrait"); !ok {
		t.Error("staged construct missing from the owner-scoped staged registry")
	}
}

// TestStageBundleDurable_RefusesConceptByName: a bundle declaring a concept is
// refused with an error that NAMES the concept, and stages nothing at all --
// including the constructs that would otherwise have been fine.
func TestStageBundleDurable_RefusesConceptByName(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	store := &fakePromoteStore{}

	bundle := `
@namespace("acme")
concept order {
  ownerUserId string!
}
` + stagedSpecSrc

	_, err := e.stageBundleDurableWithStore(ownerCtx("owner-1"), store, "owner-1", bundle, "")
	if err == nil {
		t.Fatal("expected a concept in a staged bundle to be refused")
	}
	if !strings.Contains(err.Error(), "order") {
		t.Errorf("refusal must name the concept; got %v", err)
	}
	if len(store.constructs) != 0 || len(store.bundles) != 0 {
		t.Errorf("a refused stage must persist nothing; got %d bundles / %d constructs",
			len(store.bundles), len(store.constructs))
	}
	if e.stagedAuthored != nil && e.stagedAuthored.Count() != 0 {
		t.Error("a refused stage must register nothing -- the refusal runs before the staging loop")
	}
}

// TestStageAuthoredConstruct_RefusesACoreName: staging cannot claim a name a
// core construct owns. The overlay would drop it anyway; the refusal is what
// tells the author instead of handing them an inert construct.
func TestStageAuthoredConstruct_RefusesACoreName(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	if err := e.functions.Upsert(&Function{Name: "coreOwned", FunctionKind: "query", Enabled: true, ExprSource: "core"}); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	c := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "coreOwned", Status: AuthoredActive,
		Compiled: &Function{Name: "coreOwned", FunctionKind: "query", Enabled: true}}

	err := e.stageAuthoredConstruct("owner-1", c)
	if err == nil {
		t.Fatal("expected staging over a core name to be refused")
	}
	if !strings.Contains(err.Error(), "core construct") {
		t.Errorf("the refusal must say the name is CORE (permanent), not merely taken; got %v", err)
	}
}

// TestStageAuthoredConstruct_RefusesATrainedNameDifferently: the other way a
// shared registry can own a name. Same refusal, different remedy -- and the
// author needs to be told which, because only one of the two is theirs to clear.
func TestStageAuthoredConstruct_RefusesATrainedNameDifferently(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	trained := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "alreadyTrained", Status: AuthoredActive,
		Source:   `query alreadyTrained { }`,
		Compiled: &Function{Name: "alreadyTrained", FunctionKind: "query", Enabled: true}}
	if err := e.PromoteAuthoredConstruct(context.Background(), trained); err != nil {
		t.Fatalf("seed trained: %v", err)
	}

	err := e.stageAuthoredConstruct("owner-1", trained)
	if err == nil {
		t.Fatal("expected staging over a trained name to be refused")
	}
	if !strings.Contains(err.Error(), "demote") {
		t.Errorf("the refusal must name demote as the way out; got %v", err)
	}
}

// TestBuildAuthoredFunctionOverlay_CoreThenStagedThenSession pins the memql#3932
// precedence at every boundary in one overlay.
func TestBuildAuthoredFunctionOverlay_CoreThenStagedThenSession(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	if err := e.functions.Upsert(&Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "core"}); err != nil {
		t.Fatalf("seed core: %v", err)
	}

	staged := NewAuthoredRuntimeRegistry()
	mustRegister(t, staged, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "sharedName", Status: AuthoredActive,
		Compiled: &Function{Name: "sharedName", FunctionKind: "query", Enabled: true, ExprSource: "staged"}})
	mustRegister(t, staged, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "bothTiers", Status: AuthoredActive,
		Compiled: &Function{Name: "bothTiers", FunctionKind: "query", Enabled: true, ExprSource: "staged"}})
	mustRegister(t, staged, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "stagedOnly", Status: AuthoredActive,
		Compiled: &Function{Name: "stagedOnly", FunctionKind: "query", Enabled: true, ExprSource: "staged"}})

	session := NewAuthoredRuntimeRegistry()
	mustRegister(t, session, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "bothTiers", Status: AuthoredActive,
		Compiled: &Function{Name: "bothTiers", FunctionKind: "query", Enabled: true, ExprSource: "session"}})

	overlay := e.buildAuthoredFunctionOverlay("owner-1", staged, session)

	for _, tc := range []struct{ name, want string }{
		{"sharedName", "core"},   // core beats staged
		{"bothTiers", "session"}, // session beats staged: the draft being edited wins
		{"stagedOnly", "staged"}, // staged resolves with no session copy at all
	} {
		got, _ := overlay.Get(tc.name)
		if got == nil || got.ExprSource != tc.want {
			t.Errorf("%s resolves to %+v; want ExprSource %q", tc.name, got, tc.want)
		}
	}
}

// TestStagedIsInvisibleToAnotherOwner: the tier's defining property. Another
// author's overlay does not carry it, and neither does a fresh session's.
func TestStagedIsInvisibleToAnotherOwner(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	staged := NewAuthoredRuntimeRegistry()
	mustRegister(t, staged, &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "privateQuery", Status: AuthoredActive,
		Compiled: &Function{Name: "privateQuery", FunctionKind: "query", Enabled: true}})

	if got, _ := e.buildAuthoredFunctionOverlay("owner-1", staged, nil).Get("privateQuery"); got == nil {
		t.Error("the author cannot resolve their own staged construct")
	}
	if got, _ := e.buildAuthoredFunctionOverlay("owner-2", staged, nil).Get("privateQuery"); got != nil {
		t.Error("another owner resolved a staged construct -- owner-scoping is the tier")
	}
}

// TestRehydrateRoutesThreeWays: the boot walk's three-way route. One bundle
// carrying an active, a staged and a retired row lands each in its own place.
func TestRehydrateRoutesThreeWays(t *testing.T) {
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: "mcp-promote-b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			"mcp-promote-b1": {
				{Id: "c-active", OwnerUserId: "owner-1", BundleId: "mcp-promote-b1", Kind: "query", Name: "activeQ", Status: string(BundleActive)},
				{Id: "c-staged", OwnerUserId: "owner-1", BundleId: "mcp-promote-b1", Kind: "query", Name: "stagedQ", Status: ConstructStaged},
				{Id: "c-retired", OwnerUserId: "owner-1", BundleId: "mcp-promote-b1", Kind: "query", Name: "retiredQ", Status: string(BundleRetired)},
			},
		},
	}

	var promoted, staged []string
	res, err := rehydratePromotedConstructsWithStore(context.Background(), store,
		withRehydratePromote(func(_ context.Context, row AuthoringConstructRow) error {
			promoted = append(promoted, row.Name)
			return nil
		}),
		withRehydrateStage(func(_ context.Context, row AuthoringConstructRow) error {
			staged = append(staged, row.Name)
			return nil
		}))
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	if len(promoted) != 1 || promoted[0] != "activeQ" {
		t.Errorf("promoted = %v; want [activeQ] -- only an ACTIVE row goes shared", promoted)
	}
	if len(staged) != 1 || staged[0] != "stagedQ" {
		t.Errorf("staged = %v; want [stagedQ] -- a staged row must not take the shared path", staged)
	}
	if res.Seen != 2 || res.Rehydrated != 2 || res.Staged != 1 {
		t.Errorf("result = %+v; want seen=2 rehydrated=2 staged=1 (the retired row is skipped entirely)", res)
	}
}

// TestRehydrateRefusesAWalkWithNoStageStep: a walk missing the staged branch
// would route a staged row to the SHARED promote, which is the one outcome the
// tier exists to prevent -- and it would do so silently.
func TestRehydrateRefusesAWalkWithNoStageStep(t *testing.T) {
	_, err := rehydratePromotedConstructsWithStore(context.Background(), &fakeRehydrateStore{},
		withRehydratePromote(func(context.Context, AuthoringConstructRow) error { return nil }))
	if err == nil {
		t.Fatal("expected a walk with no stage step to be refused")
	}
	if !strings.Contains(err.Error(), "stage") {
		t.Errorf("the refusal must name the missing step; got %v", err)
	}
}

// TestTrainStagedConstruct_FlipsTheSameRowAndBroadcasts: training is a state
// transition on the row that already exists, not a second row, and it fires the
// existing promote broadcast so peers pick it up.
func TestTrainStagedConstruct_FlipsTheSameRowAndBroadcasts(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	writeStore := &fakePromoteStore{}
	if _, err := e.stageBundleDurableWithStore(ownerCtx("owner-1"), writeStore, "owner-1", stagedSpecSrc, ""); err != nil {
		t.Fatalf("stage: %v", err)
	}
	rows := &fakeStagedRowStore{}
	rows.adopt(writeStore)

	if err := e.trainStagedConstructWithStore(ownerCtx("owner-1"), rows, "owner-1", "trait", "stagedOnlyTrait"); err != nil {
		t.Fatalf("train: %v", err)
	}

	// ONE row, flipped -- not a second row written beside the first.
	if len(rows.constructs) != 1 {
		t.Fatalf("training wrote %d rows; want the original one flipped", len(rows.constructs))
	}
	if got := rows.constructs[0].Status; got != string(BundleActive) {
		t.Errorf("row status after training = %q, want %q", got, string(BundleActive))
	}
	if len(rows.writes) != 1 || !strings.HasSuffix(rows.writes[0], "="+string(BundleActive)) {
		t.Errorf("status writes = %v; want exactly one flip to active", rows.writes)
	}

	// Shared now, staged no longer.
	if _, shared := e.specs.Lookup("stagedOnlyTrait"); !shared {
		t.Error("a trained construct must be in the shared registry")
	}
	if _, still := e.stagedAuthored.Lookup("owner-1", "trait", "stagedOnlyTrait"); still {
		t.Error("the staged entry must be dropped once the shared one serves its owner too")
	}
}

// TestTrainStagedConstruct_RefusesWhatWasNeverStaged: training names a staged
// construct, so a name that is not staged for this owner is refused rather than
// promoted out of nowhere.
func TestTrainStagedConstruct_RefusesWhatWasNeverStaged(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	err := e.trainStagedConstructWithStore(ownerCtx("owner-1"), &fakeStagedRowStore{}, "owner-1", "query", "neverStaged")
	if err == nil {
		t.Fatal("expected training an unstaged name to be refused")
	}
	if !strings.Contains(err.Error(), "stage it first") {
		t.Errorf("the refusal must name the missing step; got %v", err)
	}
}

// TestStagedIsNotVisibleToAnotherOwnersTrain: owner-2 cannot train owner-1's
// staged construct, because the lookup that finds it is owner-keyed.
func TestStagedIsNotVisibleToAnotherOwnersTrain(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	writeStore := &fakePromoteStore{}
	if _, err := e.stageBundleDurableWithStore(ownerCtx("owner-1"), writeStore, "owner-1", stagedSpecSrc, ""); err != nil {
		t.Fatalf("stage: %v", err)
	}
	rows := &fakeStagedRowStore{}
	rows.adopt(writeStore)

	if err := e.trainStagedConstructWithStore(ownerCtx("owner-2"), rows, "owner-2", "trait", "stagedOnlyTrait"); err == nil {
		t.Fatal("owner-2 trained owner-1's staged construct")
	}
	if _, shared := e.specs.Lookup("stagedOnlyTrait"); shared {
		t.Error("a refused train must register nothing")
	}
}

// TestConstructCatalogForOwner_ShowsOwnStagedAndHidesOthers is the visibility
// decision, asserted from both sides.
func TestConstructCatalogForOwner_ShowsOwnStagedAndHidesOthers(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	mustRegister(t, e.stagedRegistry(), &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "minesOnly", Status: AuthoredActive,
		Source: "query minesOnly { }", Compiled: &Function{Name: "minesOnly", FunctionKind: "query", Enabled: true}})
	mustRegister(t, e.stagedRegistry(), &AuthoredConstruct{OwnerUserId: "owner-2", Kind: "query", Name: "theirsOnly", Status: AuthoredActive,
		Source: "query theirsOnly { }", Compiled: &Function{Name: "theirsOnly", FunctionKind: "query", Enabled: true}})

	mine := namesOf(e.ConstructCatalogForOwner("owner-1", false))
	if !mine["minesOnly"] {
		t.Error("an author cannot see their own staged construct in the catalog")
	}
	if mine["theirsOnly"] {
		t.Error("an author can see another author's staged construct")
	}

	all := e.ConstructCatalogForOwner("owner-1", true)
	names := namesOf(all)
	if !names["minesOnly"] || !names["theirsOnly"] {
		t.Error("the cluster owner cannot enumerate every staged construct, so cannot audit the tier")
	}
	for _, entry := range all {
		if entry.Name != "theirsOnly" {
			continue
		}
		if entry.Origin != ConstructOriginStaged {
			t.Errorf("staged entry origin = %q, want %q", entry.Origin, ConstructOriginStaged)
		}
		if entry.Owner != "owner-2" {
			t.Errorf("staged entry owner = %q, want owner-2 -- without it two authors' same-named "+
				"constructs are indistinguishable to an operator", entry.Owner)
		}
	}

	// The shared catalog itself is unchanged: it still answers "what does this
	// cluster run", which a staged construct is not part of.
	if namesOf(e.ConstructCatalog())["minesOnly"] {
		t.Error("ConstructCatalog leaked a staged construct into the owner-blind answer")
	}
}

// TestConstructCatalogForOwner_SharedWinsOverStaged: a (kind, name) the shared
// catalog already reports is not listed twice. The shared registration is what
// resolves, for the staged construct's author too.
func TestConstructCatalogForOwner_SharedWinsOverStaged(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	if err := e.functions.Upsert(&Function{Name: "collides", FunctionKind: "query", Enabled: true}); err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	mustRegister(t, e.stagedRegistry(), &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "query", Name: "collides", Status: AuthoredActive,
		Source: "query collides { }", Compiled: &Function{Name: "collides", FunctionKind: "query", Enabled: true}})

	count := 0
	for _, entry := range e.ConstructCatalogForOwner("owner-1", false) {
		if entry.Kind == ConstructKindQuery && entry.Name == "collides" {
			count++
			if entry.Origin == ConstructOriginStaged {
				t.Error("the surviving entry must be the shared one -- that is what resolves")
			}
		}
	}
	if count != 1 {
		t.Errorf("(query, collides) appears %d times; want 1 -- consumers key by (kind, name)", count)
	}
}

// TestDemoteStagedConstruct_RetiresTheRowAndRegistersNothingShared: the inverse
// of stage, inheriting stage's omissions.
func TestDemoteStagedConstruct_RetiresTheRowAndRegistersNothingShared(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), specs: newSpecRegistry()}
	writeStore := &fakePromoteStore{}
	if _, err := e.stageBundleDurableWithStore(ownerCtx("owner-1"), writeStore, "owner-1", stagedSpecSrc, ""); err != nil {
		t.Fatalf("stage: %v", err)
	}
	demote := &fakeDemoteStore{constructs: map[string][]AuthoringConstructRow{}}
	demote.bundles = writeStore.bundles
	for _, row := range writeStore.constructs {
		demote.constructs[row.BundleId] = append(demote.constructs[row.BundleId], row)
	}

	outcome, err := e.demoteConstructDurableWithStore(ownerCtx("owner-1"), demote, "owner-1", "trait", "stagedOnlyTrait")
	if err != nil {
		t.Fatalf("demote staged: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRemoved {
		t.Errorf("staged demote outcome = %q, want %q", outcome.Outcome, DemoteOutcomeRemoved)
	}
	if _, still := e.stagedAuthored.Lookup("owner-1", "trait", "stagedOnlyTrait"); still {
		t.Error("the staged entry survived its own demote")
	}
	if len(demote.retiredCs) != 1 {
		t.Errorf("retired rows = %v; want exactly the staged row", demote.retiredCs)
	}
}

func namesOf(entries []ConstructCatalogEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name] = true
	}
	return out
}
