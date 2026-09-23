# Machine sharing grants Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A machine's owner can lend it to specific people and groups, and exactly those people's model calls land on it, on any replica, with the owner seeing counts only.

**Architecture:** The owner's consent stays on `v1:worker:registration.sharing`, now a closed block with a `people` mode and two id lists. One predicate in `component/worker` (`ServesPerson`) decides every person-path admission: the catalog, the shared plan and the replica-hop receiver. System work keeps `ServesTheCluster`. A new `fleetShareDirectory` builtin offers co-members (everyone from admin rank), and `fleetSetSharing` validates new subjects against it. MemQL OS gets a share dialog and a split ledger sentence.

**Tech Stack:** Go (engine, `-tags agent` for `integrations/agent/worker`), MemQL DSL, PostgreSQL (db-gated tests), React + TypeScript + Vitest (`clients/os`).

**Spec:** `docs/superpowers/specs/2026-09-23-machine-sharing-grants-design.md` (rulings G1-G15).

## Global Constraints

- Worktree: `/home/znas/memql-projects/epic-machine-sharing-grants`, branch `epic/machine-sharing-grants`, ONE PR closing #5344-#5350.
- Stage by explicit path, never `git add -A` or `git add .`; commit subjects `Issue #<N>: <what>`.
- Verify with `make test` (never `go test ./...`), `go test -count=1 -tags agent ./integrations/agent/worker/`, the db lane with `MEMQL_REQUIRE_DB=1`, and `cd clients/os && npm run typecheck && npx vitest run test/fleet`.
- Never run prettier. No emojis. The repo is hand-formatted.
- `people` needs at least one subject; at most 50 subjects; the owner is never stored in their own list.
- Mode values are exactly `owner`, `people`, `cluster`; a reader maps anything else to `owner`.
- Refusal codes: `invalid_sharing_mode`, `share_needs_someone`, `share_too_many`, `share_subject_unknown`, plus the existing `not_your_machine` and `machine_revoked`.
- Generated artifacts regenerated, never hand-edited: `make sdk-gen`, `make arch-model` (only if `make arch-model-check` reports drift).

## Review Focus

1. **One id in two spellings.** A stored `v1:identity:user:ana` must admit an acting `ana`, and the reverse; the same for group ids. Pinned in Task 1.
2. **A `people` share naming nobody** (legacy or hand-edited row): it reads as `owner` and admits nobody. Pinned in Task 1.
3. **The membership source is nil or fails**: listed users are still admitted and group members are not (narrowing, never widening). Pinned in Tasks 1 and 4.
4. **A stored subject that has left the owner's directory**: re-saving with it still present succeeds, and the directory marks it `inDirectory: false`. Pinned in Task 6.
5. **A synthetic or empty acting user** (`system:*`, `""`): never admitted by a `people` share. Pinned in Tasks 1 and 4.

---

### Task 1: The consent model and the one predicate (#5346, #5347)

**Files:**
- Modify: `component/worker/sharing.go`
- Test: `component/worker/sharing_test.go`

**Interfaces:**
- Produces:
  - `const SharingModePeople = "people"`
  - `type Sharing struct { Mode string; UserIds []string; GroupIds []string; SharedAt string; SharedBy string }`
  - `func SharingFromRow(v any) Sharing` (reads the lists only under `people`; an empty `people` reads as `owner`)
  - `func (s Sharing) Row() map[string]any` (always emits both lists, empty outside `people`)
  - `type GroupResolver func(ctx context.Context, userId string) []string`
  - `func InstalledGroups(ctx context.Context, userId string) []string`
  - `type Person struct{ UserId string; ... }`, `func NewPerson(ctx context.Context, userId string, resolve GroupResolver) *Person`, `func (p *Person) Groups() []string` (resolved at most once, lazily)
  - `func (s Sharing) Admits(p *Person) bool`
  - `func ServesPerson(s Sharing, cockpitServe string, p *Person) bool`
  - `func PersonRefusal(s Sharing, cockpitServe string, p *Person) string` ("" when served)
  - `func SystemRefusal(s Sharing, cockpitServe string) string` ("" when served)
  - `func SameSubjectId(a, b string) bool`
  - `ServesTheCluster` and `SharingRefusal` keep their signatures and meaning.

- [ ] **Step 1: Write the failing tests** (append to `sharing_test.go`; change the round-trip test to `reflect.DeepEqual`, since `Sharing` now holds slices)

```go
func TestAPeopleShareAdmitsExactlyItsSubjects(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"v1:identity:user:ana"}, GroupIds: []string{"design"}}
	groupsOf := func(_ context.Context, userId string) []string {
		if userId == "bo" {
			return []string{"v1:identity:group:design"}
		}
		return nil
	}
	cases := []struct {
		name string
		user string
		want bool
	}{
		{"listed person, bare id", "ana", true},
		{"listed person, canonical id", "v1:identity:user:ana", true},
		{"member of a listed group", "bo", true},
		{"somebody else", "cy", false},
		{"no acting person", "", false},
		{"a synthetic actor", "system:fleet-inference", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPerson(context.Background(), tc.user, groupsOf)
			if got := s.Admits(p); got != tc.want {
				t.Fatalf("Admits(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}

func TestGroupsAreResolvedOnlyWhenAGroupShareNeedsThem(t *testing.T) {
	calls := 0
	count := func(context.Context, string) []string { calls++; return nil }
	for _, s := range []Sharing{
		{Mode: SharingModeCluster},
		{Mode: SharingModeOwner},
		{Mode: SharingModePeople, UserIds: []string{"ana"}},
	} {
		p := NewPerson(context.Background(), "ana", count)
		_ = s.Admits(p)
	}
	if calls != 0 {
		t.Fatalf("resolved groups %d times for shares that name no group", calls)
	}
	p := NewPerson(context.Background(), "bo", count)
	s := Sharing{Mode: SharingModePeople, GroupIds: []string{"g1"}}
	_ = s.Admits(p)
	_ = s.Admits(p)
	if calls != 1 {
		t.Fatalf("resolved groups %d times for one person, want exactly 1", calls)
	}
}

func TestANilGroupSourceNarrowsAndNeverWidens(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}, GroupIds: []string{"design"}}
	nobody := func(context.Context, string) []string { return nil }
	if !s.Admits(NewPerson(context.Background(), "ana", nobody)) {
		t.Fatal("a listed person must still be admitted when groups cannot be read")
	}
	if s.Admits(NewPerson(context.Background(), "bo", nobody)) {
		t.Fatal("an unresolvable membership must admit nobody through a group")
	}
}

func TestAPeopleShareNamingNobodyIsOwner(t *testing.T) {
	got := SharingFromRow(map[string]any{"mode": "people"})
	if got.Mode != SharingModeOwner || len(got.UserIds) != 0 || len(got.GroupIds) != 0 {
		t.Fatalf("an empty people share must read as owner, got %+v", got)
	}
}

func TestTheListsAreReadOnlyUnderPeople(t *testing.T) {
	got := SharingFromRow(map[string]any{"mode": "cluster", "userIds": []any{"ana"}, "groupIds": []any{"g"}})
	if got.Mode != SharingModeCluster || len(got.UserIds) != 0 || len(got.GroupIds) != 0 {
		t.Fatalf("residue lists must be ignored outside people, got %+v", got)
	}
	got = SharingFromRow(map[string]any{"mode": "people", "userIds": []any{" ana ", "", "ana", "v1:identity:user:ana"}})
	if got.Mode != SharingModePeople || !reflect.DeepEqual(got.UserIds, []string{"ana"}) {
		t.Fatalf("ids must be trimmed and de-duplicated across spellings, got %+v", got.UserIds)
	}
}

func TestServesPersonNeedsTheCockpitToo(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	ana := NewPerson(context.Background(), "ana", nil)
	if ServesPerson(s, InferenceServeOwner, ana) {
		t.Fatal("the cockpit's half is still required for a people share")
	}
	if !ServesPerson(s, InferenceServeCluster, ana) {
		t.Fatal("both halves given must serve the listed person")
	}
	if ServesTheCluster(SharingModePeople, InferenceServeCluster) {
		t.Fatal("a people share is never a cluster share: system work must not ride it")
	}
}

func TestRefusalsNameTheMissingHalfForPeopleShares(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	if got := PersonRefusal(s, InferenceServeCluster, NewPerson(context.Background(), "bo", nil)); !strings.Contains(got, "specific people") {
		t.Fatalf("a person not on the list: %q", got)
	}
	if got := PersonRefusal(s, InferenceServeOwner, NewPerson(context.Background(), "ana", nil)); !strings.Contains(got, "policy.yaml") {
		t.Fatalf("a listed person on a machine that has not agreed: %q", got)
	}
	if got := SystemRefusal(s, InferenceServeCluster); !strings.Contains(got, "cluster's own work") {
		t.Fatalf("system work on a people share: %q", got)
	}
	if got := PersonRefusal(s, InferenceServeCluster, NewPerson(context.Background(), "ana", nil)); got != "" {
		t.Fatalf("a served person has no refusal, got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 github.com/znasllc-io/memql/component/worker/ -run 'People|Groups|NilGroup|ServesPerson|Refusals|Lists'`
Expected: compile failure (`SharingModePeople`, `NewPerson`, `Admits` ... undefined).

- [ ] **Step 3: Implement** in `sharing.go`: add the constant, extend `Sharing`, rewrite `SharingFromRow` / `Row`, add `GroupResolver`, `InstalledGroups` (reads `auth.InstalledMembershipSource()`), `Person` (a `sync.Once` around the resolver), `Admits`, `ServesPerson`, `PersonRefusal`, `SystemRefusal`, `SameSubjectId` (bare/canonical tolerant, the `sameSubjectId` rule from `shared_resolution.go`) and an unexported `subjectIds(v any) []string` (trims, drops empties, de-duplicates by bare id, keeps the first spelling). `PersonRefusal` under `people`: not admitted -> "Its owner has shared it with specific people, and not with you."; admitted but the cockpit says owner -> "Its owner has shared it with you, but its cockpit is not willing to serve anyone but its owner. Set inference.serve to cluster in that machine's policy.yaml." Under `owner`/`cluster` it returns `SharingRefusal(mode, cockpit)`. `SystemRefusal` under `people` -> "Its owner has shared it with specific people, not with the cluster's own work."; otherwise `SharingRefusal`.

- [ ] **Step 4: Run** the Step 2 command plus `go test -count=1 github.com/znasllc-io/memql/component/worker/`. Expected: PASS.

- [ ] **Step 5: Commit** `git add component/worker/sharing.go component/worker/sharing_test.go` then `git commit -m "Issue #5346: a people share and the one predicate that admits a person"`.

---

### Task 2: The DSL contract and the generated SDKs (#5346)

**Files:**
- Modify: `dsl/worker/concepts.memql` (`sharing` becomes a closed block)
- Modify: `dsl/worker/mutations.memql` (`setWorkerSharing` args + write)
- Modify: `dsl/common/builtins.memql` (`fleetSetSharing` args; new `fleetShareDirectory`)
- Modify: `component/memql/engine_types.go` (`BuiltinExecutorFleetShareDirectory = "fleetShareDirectory"`)
- Regenerate: `sdk/go/client/generated_builtins.go`, `sdk/ts/src/client/generated_builtins.ts` (and whatever else `make sdk-gen` touches)

**Interfaces:**
- Produces the wire: `fleetSetSharing({registrationId, mode, userIds?, groupIds?})`; `fleetShareDirectory({registrationId})`.

- [ ] **Step 1:** Replace the `sharing object @description(...)` line with a closed block. The concept-level explanation moves to `//` comments above it (the `preferences` precedent):

```memql
  sharing {
    mode      enum("owner", "people", "cluster")  @description("owner (the default and the absent case): its owner only. people: the listed users and the ACTIVE members of the listed ACTIVE groups. cluster: everyone signed in, plus the cluster's own work.")
    userIds   []string  @description("Under people: the users this machine is lent to. Empty under the other two modes.")
    groupIds  []string  @description("Under people: the groups whose active members may use it. Empty under the other two modes.")
    sharedAt  datetime  @description("When the owner last changed this consent.")
    sharedBy  string    @description("Who changed it: always the owner, stamped from the actor.")
  }
```

- [ ] **Step 2:** `setWorkerSharing`: `mode enum("owner", "people", "cluster")!`, two new documented args `userIds []string` and `groupIds []string`, and write `userIds: args.userIds ?? []`, `groupIds: args.groupIds ?? []` into the block. The `@serverOnly` doc comment gains one sentence: the lists are validated by `fleetSetSharing` against the owner's directory, because a `.memql` body cannot.

- [ ] **Step 3:** `fleetSetSharing` gains `userIds []string` and `groupIds []string` (each with `@description`), and its mode description names `people`. Add:

```memql
@sdk
@executor("fleetShareDirectory")
@description("Who you can lend one of YOUR OWN machines to: the people and groups you may pick, and the ones already on this machine's list, by display name. Everyone in the cluster when you are at admin rank or above; otherwise the members of the groups you are in, and those groups. The machine is resolved through your own machines, so another user's id answers exactly as a made-up one does. It never returns an email address unless you already see it in Users.")
builtin fleetShareDirectory {
  registrationId  string!  @description("v1:worker:registration.id of the machine being shared. It must be one of the caller's own.")
}
```

- [ ] **Step 4:** Add `BuiltinExecutorFleetShareDirectory` to `engine_types.go` beside `BuiltinExecutorFleetSetSharing` (the table entry lands in Task 6).

- [ ] **Step 5: Verify** `go run ./cmd/memqllint dsl/` (expect no errors), then `make sdk-gen && make sdk-gen-check`. Read the SDK diff for deletions: a detached doc comment shows as a lost `/** ... */`.

- [ ] **Step 6: Commit** the four DSL/Go files plus every file `make sdk-gen` changed, by path: `Issue #5346: the sharing block, the people mode and the directory builtin`.

---

### Task 3: A person's catalog includes machines shared with them (#5347, G8)

**Files:**
- Modify: `component/worker/fleetcatalog/machine.go` (Candidate fields and methods)
- Modify: `component/worker/fleetcatalog/store.go` (parse the lists)
- Modify: `component/worker/fleetcatalog/catalog.go` (`Reader.Groups`, the person half)
- Test: `component/worker/fleetcatalog/catalog_test.go`, `component/worker/fleetcatalog/graph_db_test.go`

**Interfaces:**
- Consumes: Task 1's `Sharing`, `Person`, `ServesPerson`, `PersonRefusal`, `SystemRefusal`, `GroupResolver`.
- Produces: `Candidate.SharedUserIds []string`, `Candidate.SharedGroupIds []string`, `func (c Candidate) Sharing() workerservice.Sharing`, `func (c Candidate) ServesPerson(p *workerservice.Person) bool`, `func (c Candidate) PersonRefusal(p *workerservice.Person) string`, `func (c Candidate) SystemRefusal() string`; `Reader.Groups workerservice.GroupResolver`.

- [ ] **Step 1: Write the failing test.** Replace `graphFixture` with one that answers BOTH reads and asserts the right actor on each (a person's catalog now makes the cross-owner read too):

```go
type dualFixture struct {
	t     *testing.T
	owner string
	own   []any
	all   []any
}

func (f dualFixture) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	claims, _ := auth.ClaimsFromContext(ctx)
	switch query {
	case "query myWorkersWithStatus()":
		if claims["sub"] != f.owner {
			f.t.Fatalf("owner read ran as %v, want %s", claims["sub"], f.owner)
		}
		return memql.NewResultWithOutput(f.own), nil
	case "query allWorkersWithStatus()":
		if claims["sub"] != systemFleetActor {
			f.t.Fatalf("cross-owner read ran as %v", claims["sub"])
		}
		return memql.NewResultWithOutput(f.all), nil
	}
	f.t.Fatalf("unexpected query %q", query)
	return nil, nil
}

func TestAPersonsCatalogIncludesMachinesSharedWithThem(t *testing.T) {
	now := time.Now().UTC()
	mine := machineRow("mine", "model-mine", now)
	mine["ownerUserId"] = "alice"
	withAlice := machineRow("with-alice", "model-with-alice", now)
	withAlice["ownerUserId"] = "bob"
	withAlice["sharing"] = map[string]any{"mode": "people", "userIds": []any{"alice"}}
	withAlice["capabilityDescriptor"] = map[string]any{"inferenceServe": "cluster"}
	viaGroup := machineRow("via-group", "model-via-group", now)
	viaGroup["ownerUserId"] = "bob"
	viaGroup["sharing"] = map[string]any{"mode": "people", "groupIds": []any{"design"}}
	viaGroup["capabilityDescriptor"] = map[string]any{"inferenceServe": "cluster"}
	withCarol := machineRow("with-carol", "model-with-carol", now)
	withCarol["ownerUserId"] = "bob"
	withCarol["sharing"] = map[string]any{"mode": "people", "userIds": []any{"carol"}}
	withCarol["capabilityDescriptor"] = map[string]any{"inferenceServe": "cluster"}
	everyone := machineRow("everyone", "model-everyone", now)
	everyone["ownerUserId"] = "bob"
	everyone["sharing"] = map[string]any{"mode": "cluster"}
	everyone["capabilityDescriptor"] = map[string]any{"inferenceServe": "cluster"}

	r := &Reader{
		Store:  &EngineStore{Engine: dualFixture{t: t, owner: "alice", own: []any{mine}, all: []any{mine, withAlice, viaGroup, withCarol, everyone}}},
		Now:    func() time.Time { return now },
		Groups: func(_ context.Context, userId string) []string { if userId == "alice" { return []string{"v1:identity:group:design"} }; return nil },
	}
	got, err := r.Catalog(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range got {
		seen[m.ModelId] = true
	}
	for _, want := range []string{"model-mine", "model-with-alice", "model-via-group", "model-everyone"} {
		if !seen[want] {
			t.Fatalf("catalog %v is missing %s", seen, want)
		}
	}
	if seen["model-with-carol"] {
		t.Fatal("a machine shared with somebody else leaked into alice's catalog")
	}
	shared, err := (&Reader{Store: &EngineStore{Engine: dualFixture{t: t, all: []any{withAlice, everyone}}}, Now: func() time.Time { return now }}).Catalog(context.Background(), "")
	if err != nil || len(shared) != 1 || shared[0].ModelId != "model-everyone" {
		t.Fatalf("system catalog = %+v, %v; a people share must never serve the cluster's own work", shared, err)
	}
}
```

Update `TestIndependentGraphReadersProjectTheSameOwnerCatalog` to use `dualFixture{owner: "alice", own: []any{row}, all: []any{row}}`, and `TestSharedGraphCatalogRequiresBothConsents` to use `dualFixture{all: rows}`.

- [ ] **Step 2: Run to verify it fails:** `go test -count=1 github.com/znasllc-io/memql/component/worker/fleetcatalog/ -run 'Person|Graph'`. Expected: FAIL (`Reader.Groups` undefined, then the missing shared models).

- [ ] **Step 3: Implement.** In `store.go`, both readers read `s := workerservice.SharingFromRow(row["sharing"])` into `SharingMode`, `SharedUserIds` and `SharedGroupIds`. In `catalog.go`:

```go
	} else {
		machines, err = r.Store.WorkersForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		// The machines SHARED WITH this person (design G8). The own half is a
		// complete answer to a narrower question, so a failed cross-owner read
		// keeps it rather than failing the catalog -- PlanUserModelWithShared's
		// rule, applied to the read that decides availability.
		if shared, ok := r.Store.(SharedStoreReader); ok {
			if all, sharedErr := shared.SharedInferenceWorkers(ctx); sharedErr == nil {
				machines = append(machines, sharedWithPerson(ctx, owner, machines, all, r.Groups)...)
			}
		}
	}
```

with `sharedWithPerson` skipping ids already present and machines whose owner is this person, and keeping `m.ServesPerson(person)` only.

- [ ] **Step 4:** In `graph_db_test.go`, the owner assertion becomes "alice sees mine AND shared, never private or owner-consent-only" -- the old assertion pinned G8's bug. Add a `people` row shared with alice and one shared with nobody she is.

- [ ] **Step 5: Run** the Step 2 command and `MEMQL_REQUIRE_DB=1 go test -count=1 github.com/znasllc-io/memql/component/worker/fleetcatalog/`. Expected: PASS.

- [ ] **Step 6: Commit** `Issue #5347: a person's catalog includes the machines shared with them`.

---

### Task 4: The shared plan and system work (#5347, #5348)

**Files:**
- Modify: `integrations/agent/worker/router.go` (`groups` field, `SetGroupResolver`)
- Modify: `integrations/agent/worker/shared_resolution.go`, `integrations/agent/worker/model_routing.go`
- Test: `integrations/agent/worker/shared_resolution_test.go`, `integrations/agent/worker/model_routing_test.go`

**Interfaces:**
- Consumes: Task 3's `Candidate` methods.
- Produces: `func (r *Router) SetGroupResolver(fn workerservice.GroupResolver)`.

- [ ] **Step 1: Write the failing tests** (in `shared_resolution_test.go`):

```go
func peopleMachine(id, owner string, userIds, groupIds []string) Candidate {
	c := sharedMachine(id, owner)
	c.SharingMode = workerservice.SharingModePeople
	c.SharedUserIds = userIds
	c.SharedGroupIds = groupIds
	return c
}

func TestAPeopleShareServesExactlyItsPeople(t *testing.T) {
	forAna := peopleMachine("for-ana", "bob", []string{"v1:identity:user:ana"}, nil)
	forDesign := peopleMachine("for-design", "bob", nil, []string{"design"})
	store := &sharedFleet{fakeFleet: &fakeFleet{owner: "ana"}, all: []Candidate{forAna, forDesign}}
	router := modelRouter(t, store)
	router.SetGroupResolver(func(_ context.Context, userId string) []string {
		if userId == "cy" {
			return []string{"v1:identity:group:design"}
		}
		return nil
	})
	cases := map[string][]string{"ana": {"for-ana"}, "cy": {"for-design"}, "dee": nil}
	for user, want := range cases {
		plan, err := router.PlanUserModelWithShared(context.Background(), user, smallModel, ModelNeeds{})
		if err != nil {
			t.Fatalf("%s: %v", user, err)
		}
		if got := ids(plan.Candidates); !equalStrings(got, want) {
			t.Fatalf("%s: candidates = %v, want %v", user, got, want)
		}
	}
}

func TestSystemWorkNeverReachesAPeopleShare(t *testing.T) {
	forAna := peopleMachine("for-ana", "bob", []string{"ana"}, nil)
	everyone := sharedMachine("everyone", "bob")
	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{forAna, everyone}}
	plan, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "everyone" {
		t.Fatalf("system candidates = %v, want only the machine shared with everyone", got)
	}
	if why := plan.Rejected["for-ana"]; !strings.Contains(why, "cluster's own work") {
		t.Fatalf("the refusal must say why: %q", why)
	}
}

func TestOwnMachinesStillComeFirstBeforeAPeopleShare(t *testing.T) {
	mine := privateMachine("mine", "ana")
	forAna := peopleMachine("for-ana", "bob", []string{"ana"}, nil)
	store := &sharedFleet{fakeFleet: &fakeFleet{machines: []Candidate{mine}, owner: "ana"}, all: []Candidate{mine, forAna}}
	plan, err := modelRouter(t, store).PlanUserModelWithShared(context.Background(), "ana", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(plan.Candidates); len(got) != 2 || got[0] != "mine" {
		t.Fatalf("preferOwnMachines: %v", got)
	}
}
```

(`equalStrings` treats nil and empty as equal; add it beside `ids` if no helper exists.)

- [ ] **Step 2: Run to verify they fail:** `go test -count=1 -tags agent ./integrations/agent/worker/ -run 'PeopleShare|SystemWork|OwnMachinesStill'`. Expected: FAIL (`SetGroupResolver` undefined).

- [ ] **Step 3: Implement.** `Router` gains `groups workerservice.GroupResolver` and `SetGroupResolver`. In `PlanUserModelWithShared`, build `person := workerservice.NewPerson(ctx, actingUserId, r.groups)` once, and replace `case !c.ServesCluster(): pendingShareNoise[...] = c.SharingRefusal()` with `case !c.ServesPerson(person): pendingShareNoise[c.RegistrationId] = c.PersonRefusal(person)`. In `PlanSharedModel`, `rejected[c.RegistrationId] = c.SystemRefusal()`. Update both doc comments: a person reaches the machines their owners lent to them; system work reaches only machines lent to everyone (G3).

- [ ] **Step 4: Run** Step 2's command and the whole package: `go test -count=1 -tags agent ./integrations/agent/worker/`. Expected: PASS.

- [ ] **Step 5: Commit** `Issue #5348: system work stays on machines shared with everyone`.

---

### Task 5: The replica hop admits exactly the granted people (#5347)

**Files:**
- Modify: `integrations/agent/worker/forward_handler.go`
- Test: `integrations/agent/worker/shared_model_hop_test.go`

**Interfaces:**
- Produces: `func (h *ForwardHandler) SetGroupResolver(fn workerservice.GroupResolver)`.

- [ ] **Step 1: Write the failing tests.** `newSharedHop` gains a variant taking the owner's lists; the receiver gets a resolver that answers on the RECEIVING replica only:

```go
func newPeopleHop(t *testing.T, userIds, groupIds []string, groupsOf workerservice.GroupResolver) *sharedHop {
	h := newSharedHop(t, workerservice.SharingModePeople, true, true)
	store := h.link.handler.store.(*sharedFleet)
	for i := range store.all {
		store.all[i].SharedUserIds = userIds
		store.all[i].SharedGroupIds = groupIds
	}
	h.link.handler.SetGroupResolver(groupsOf)
	return h
}

func TestAListedPersonIsServedAcrossTheHop(t *testing.T) {
	h := newPeopleHop(t, []string{sharedHopCaller}, nil, nil)
	out, err := h.call(t, h.caller)
	if err != nil || out.RefusedBeforeStart {
		t.Fatalf("a listed person must be served across the hop: %v %s %s", err, out.ErrorCode, out.ErrorMessage)
	}
}

func TestAGroupMemberIsServedAcrossTheHopByTheReceiversOwnRead(t *testing.T) {
	groupsOf := func(_ context.Context, userId string) []string {
		if sameSubject(userId, sharedHopCaller) {
			return []string{"v1:identity:group:design"}
		}
		return nil
	}
	h := newPeopleHop(t, nil, []string{"design"}, groupsOf)
	out, err := h.call(t, h.caller)
	if err != nil || out.RefusedBeforeStart {
		t.Fatalf("a member of a listed group must be served across the hop: %v %s %s", err, out.ErrorCode, out.ErrorMessage)
	}
}

func TestSomebodyNotOnTheListIsRefusedAcrossTheHop(t *testing.T) {
	h := newPeopleHop(t, []string{"v1:identity:user:somebody-else"}, []string{"design"}, func(context.Context, string) []string { return nil })
	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("a person the owner did not list must not be served across the hop")
	}
}
```

- [ ] **Step 2: Run to verify they fail:** `go test -count=1 -tags agent ./integrations/agent/worker/ -run 'AcrossTheHop'`. Expected: FAIL (`SetGroupResolver` undefined; then the listed person refused because the receiver still asks `ServesCluster`).

- [ ] **Step 3: Implement.** `ForwardHandler` gains `groups workerservice.GroupResolver` + `SetGroupResolver`. In `verifySharedRegistration`, `person := workerservice.NewPerson(ctx, actingUserId, h.groups)` and `if !m.ServesPerson(person) { return errRegistrationNotShared }`. The doc comment names the verified authority's subject as the person, resolved on this replica.

- [ ] **Step 4: Run** the package: `go test -count=1 -tags agent ./integrations/agent/worker/`. Expected: PASS, including the five existing D10 hop tests.

- [ ] **Step 5: Commit** `Issue #5347: the replica hop admits exactly the people a machine is lent to`.

---

### Task 6: The directory, the write, and aggregated refusals (#5346, #5347)

**Files:**
- Create: `component/memql/fleet_share_directory.go`
- Modify: `component/memql/fleet_machine_acts.go` (`evaluateFleetSetSharingExpression`)
- Modify: `component/memql/fleet_model_pull.go` (`modelPullMachine` carries the stored share)
- Modify: `component/memql/fleet_refusal.go` (classify `specific people`; export `IsForeignShareRefusal`)
- Modify: `component/memql/executor_builtin.go` (table entry)
- Test: `component/memql/fleet_share_directory_db_test.go` (db-gated), `component/memql/fleet_refusal_test.go` (or the existing refusal test file), `integrations/agent/worker/sharing_refusal_parity_test.go`

**Interfaces:**
- Consumes: Task 2's DSL.
- Produces: `func (e *MemQLEngine) evaluateFleetShareDirectoryExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error)` answering one virtual row of concept `v1:worker:shareDirectory` with payload `{machineId, everyone bool, people [{id, name, detail}], groups [{id, name, members}], current {people [{id, name, inDirectory}], groups [{id, name, inDirectory}]}}`; `func IsForeignShareRefusal(why string) bool`; `fleetSetSharing` receipt `{machineId, mode, people, groups, sentence}`.

- [ ] **Step 1: Write the failing tests.**
  - Parity (agent tag, imports both packages): every non-empty sentence from `workerservice.PersonRefusal` / `SystemRefusal` / `SharingRefusal` across modes x cockpit x listed/unlisted satisfies `memqlengine.IsForeignShareRefusal`.
  - db-gated directory + write test, following `graph_db_test.go`'s setup (real engine, rows inserted directly, cleanup by id): users ana, bo, cy, dee; groups design (ana, bo) and ops (cy); a machine owned by ana.
    - As ana (rank `user`): `fleetShareDirectory` offers people {bo} and groups {design}, `everyone` false, no `detail`.
    - As an admin-rank owner of a second machine: offers every active person with `detail` their email, and both groups.
    - `fleetSetSharing(mode: people, userIds: [cy])` as ana refuses `share_subject_unknown`; `userIds: [bo]` succeeds and the row stores `{mode: people, userIds: [bo], groupIds: []}`.
    - Remove bo from design, then `fleetSetSharing(mode: people, userIds: [bo], groupIds: [design])` still succeeds (bo is grandfathered), and the directory's `current.people` lists bo with `inDirectory: false`.
    - `mode: people` with no ids refuses `share_needs_someone`; `mode: owner` with ids stores empty lists (G10); `mode: shared` refuses `invalid_sharing_mode`; ana in her own list is dropped.
    - A registration row written with the pre-change block `{mode: cluster, sharedAt, sharedBy}` survives a `refreshWorkerRegistration`-style update under the closed block (G11).

- [ ] **Step 2: Run to verify they fail:** `MEMQL_REQUIRE_DB=1 go test -count=1 github.com/znasllc-io/memql/component/memql/ -run 'ShareDirectory|SetSharing'` and `go test -count=1 -tags agent ./integrations/agent/worker/ -run Parity`. Expected: FAIL.

- [ ] **Step 3: Implement the directory** in `fleet_share_directory.go`: resolve the machine with `modelPullMachineFor`; decide `everyone` with `rankFloorAdmits(ladder, "admin", ladder.rankOf(role))`; read users, groups and memberships in ONE `DISTINCT ON (id)` select over the three concepts, carrying the verdict `// staged-data: MUST-NOT-GATE -- the directory must offer exactly the memberships activeGroupIdsForUser honours; gating here would hide an in-force group share from its owner's dialog and refuse re-saving it`; build people/groups per G2 (active users only; active memberships in active groups; the caller excluded; names from displayName, then first + last name, then "Unnamed person"; `detail` = primaryEmail only when `everyone`); build `current` from the machine's stored share. Sort people and groups by name.

- [ ] **Step 4: Implement the write** in `evaluateFleetSetSharingExpression`: accept the three modes; under `people` normalise both lists (trim, drop empties, de-duplicate across spellings, drop the owner), refuse zero or more than 50, compute the NEW subjects (not already on `machine.SharedUserIds` / `SharedGroupIds`) and refuse `share_subject_unknown` unless each is in the directory; outside `people` send empty lists. Render `setWorkerSharing` with all four arguments under the existing internal-origin stamp. The receipt's sentence: owner -> "Kept to you. Nobody else's work will run on this machine."; people -> "Lent to N people and M groups." followed by the cockpit sentence when the machine has not agreed; cluster -> the existing sentence.

- [ ] **Step 5: Implement the classifier:** add `"specific people"` to `isForeignPrivateShareNoise` and export `IsForeignShareRefusal` as its alias.

- [ ] **Step 6: Run** Step 2's commands, then `make test`. Expected: PASS (watch `staged_read_site_classification_test.go`, `TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin`, the builtin reply shape).

- [ ] **Step 7: Commit** `Issue #5347: the share directory and a write that only lends to people you can pick`.

---

### Task 7: The ledger splits you from others (#5349, G4)

**Files:**
- Modify: `component/memql/sharing_ledger.go`, `component/memql/sharing_ledger_read.go`
- Test: the existing ledger test file (`component/memql/sharing_ledger_test.go`)

**Interfaces:**
- Produces: `func FoldLedger(machineId, ownerUserId, week string, calls []LedgerCall) LedgerEntry`; `LedgerEntry` gains `OtherCalls`, `OtherPeople`, `SystemCalls`; `Row()` gains `otherCalls`, `otherPeople`, `systemCalls`.

- [ ] **Step 1: Write the failing table test:**

```go
func TestTheLedgerSentenceSplitsYouFromOthers(t *testing.T) {
	const owner = "v1:identity:user:olivia"
	call := func(user string) LedgerCall { return LedgerCall{MachineId: "m", ActingUserId: user, Level: "fast", Week: "2026-W39"} }
	repeat := func(n int, user string) []LedgerCall {
		out := make([]LedgerCall, n)
		for i := range out {
			out[i] = call(user)
		}
		return out
	}
	join := func(parts ...[]LedgerCall) []LedgerCall {
		var out []LedgerCall
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name  string
		calls []LedgerCall
		want  string
	}{
		{"none", nil, "No calls have run on this machine this week."},
		{"one, yours", repeat(1, "olivia"), "Served 1 call this week, for you."},
		{"all yours", repeat(41, owner), "Served 41 calls this week, all of them yours."},
		{"mixed", join(repeat(29, owner), repeat(6, "ana"), repeat(6, "bo")), "Served 41 calls this week, 12 of them for 2 other people."},
		{"mixed, one other", join(repeat(29, owner), repeat(12, "ana")), "Served 41 calls this week, 12 of them for 1 other person."},
		{"mixed with system", join(repeat(26, owner), repeat(12, "ana"), repeat(3, "")), "Served 41 calls this week, 12 of them for 1 other person and 3 for the cluster's own work."},
		{"yours and system", join(repeat(38, owner), repeat(3, "")), "Served 41 calls this week, 3 of them for the cluster's own work."},
		{"all others", join(repeat(6, "ana"), repeat(6, "bo")), "Served 12 calls this week, all for 2 other people."},
		{"one other", repeat(1, "ana"), "Served 1 call this week, for 1 other person."},
		{"all system", repeat(5, ""), "Served 5 calls this week, all for the cluster's own work."},
		{"others and system", join(repeat(12, "ana"), repeat(3, "")), "Served 15 calls this week: 12 for 1 other person and 3 for the cluster's own work."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FoldLedger("m", owner, "2026-W39", tc.calls).Sentence(); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails:** `go test -count=1 github.com/znasllc-io/memql/component/memql/ -run 'Ledger'`. Expected: compile failure (FoldLedger arity).

- [ ] **Step 3: Implement:** the fold counts a call with an empty acting user as system work, one whose acting user is the owner (bare/canonical tolerant) as the owner's, and every other as another person's, with distinct others counted. `Sentence()` follows the table exactly. `sharing_ledger_read.go` passes `machine.OwnerUserId`. `TestTheLedgerCarriesNoPromptContent` must still pass: the three new fields are integers.

- [ ] **Step 4: Run** Step 2's command. Expected: PASS.

- [ ] **Step 5: Commit** `Issue #5349: the ledger says how much of a machine's week was lent`.

---

### Task 8: MemQL OS: the share dialog (#5349)

Load `frontend-design:frontend-design` and follow `clients/os/SUPERVISED-VISUAL-COMPOSITION.md` and `clients/os/DESIGN.md`. Reuse `ChoiceStack` (`voice="prose"`), `Button`, `Input`, `Chip`, `Notice`, `Caption`, `InfoDetail`, and a native `<dialog>` opened with `showModal()` (the `InfoDetail` / `DetailDialog` pattern: focus containment, Escape and focus return come from the platform).

**Files:**
- Modify: `clients/os/src/apps/fleet/rows.ts` (`sharingMode: "owner" | "people" | "cluster"`, `sharedUserIds`, `sharedGroupIds`)
- Modify: `clients/os/src/apps/fleet/machines/useMachineWrites.ts` (`setSharing(registrationId, choice)` resolving to a receipt or null)
- Create: `clients/os/src/apps/fleet/machines/useShareDirectory.ts`
- Create: `clients/os/src/apps/fleet/machines/ShareDialog.tsx`
- Modify: `clients/os/src/apps/fleet/machines/SharingGroup.tsx`
- Modify: `clients/os/src/apps/fleet/FleetWorkspace.tsx` (the anchor link's words, the attention marker)
- Create: `clients/os/src/apps/fleet/machines/sharingAttention.tsx` (runtime publisher)
- Modify: the Fleet stylesheet the panel already uses
- Test: `clients/os/test/fleet/sharing.test.tsx`, `clients/os/test/fleet/harness.tsx`, `clients/os/test/fleet/rows.test.ts`

**Interfaces:**
- Consumes: `fleetShareDirectory({registrationId})` -> one row `{everyone, people[], groups[], current{people[], groups[]}}`; `fleetSetSharing({registrationId, mode, userIds, groupIds})` -> receipt `{sentence}`.
- Produces: `interface ShareChoice { mode: "owner" | "people" | "cluster"; userIds: string[]; groupIds: string[] }`.

- [ ] **Step 1: Write the failing tests** (`sharing.test.tsx`, through the harness's `builtinReply` wire shape):
  1. `rows.ts`: `{mode: "people", userIds: ["a"]}` -> `people` with the list; `{mode: "people"}` -> `owner`; `{mode: "cluster", userIds: ["a"]}` -> `cluster` with empty lists.
  2. The panel states: "Only you" / "Shared with Ana Ruiz and Design" / "Everyone in this cluster", with both consent lines always present.
  3. The dialog: opening reads the directory once; choosing "Specific people and groups" shows the search; picking adds a removable chip; Save is disabled until the draft differs; Save sends exactly `{registrationId, mode: "people", userIds, groupIds}`.
  4. A stale subject (`inDirectory: false`) renders marked "No longer available to pick" and stays removable, and saving without touching it still sends it.
  5. A refusal keeps the dialog open with the draft intact and shows the engine's sentence; success closes it and shows the receipt sentence.
  6. Escape closes without writing; focus returns to "Change sharing".
  7. A non-owner sees no "Change sharing" button, only the sentence saying who can change it.
  8. The ledger line renders for `people` and `cluster` when the machine is serving, and as a warning notice when `readable` is false.
  9. The attention marker is published only when the viewer owns an unrevoked machine, and is acknowledged only when the Sharing view is visible.

- [ ] **Step 2: Run to verify they fail:** `cd clients/os && npx vitest run test/fleet/sharing.test.tsx test/fleet/rows.test.ts`. Expected: FAIL.

- [ ] **Step 3: Implement** the files above. Copy, verbatim:
  - Choices: "Only me" / "Nobody else's work runs on it."; "Specific people and groups" / "The people you choose, and the members of the groups you choose."; "Everyone in this cluster" / "Anyone signed in to this cluster, and the cluster's own automations."
  - Terms: "You see how many calls ran and for how many people. You never see what anybody asked or what the model answered."
  - Stopping: "Changes take effect on the next call. A call already running finishes."
  - Empty directory (not admin): "Nobody shares a group with you yet. Choose Everyone, or ask an admin to add you to a group."
  - The consent line under `people` when the cockpit has not agreed: "Its cockpit has not agreed to serve anyone but you. Set inference.serve to cluster in this machine's policy.yaml."

- [ ] **Step 4: Run** `npx vitest run test/fleet`, `npm run typecheck`, `make os-build`. Expected: PASS.

- [ ] **Step 5: Real-browser pass** with a Vite QA harness over explicitly labelled fixtures: desktop and narrow (361px), light and dark, empty directory, populated, stale subject, refusal. Keyboard only: Tab through the choices, the search, the chips, Save; Escape. Record what ran against fixtures and what ran against services.

- [ ] **Step 6: Commit** `Issue #5349: share a machine with people and groups from Fleet`.

---

### Task 9: Docs, and the whole-branch verification (#5350)

**Files:**
- Modify: `docs/public/operate/shared-machines.md` (three modes, the directory rule, system work, the ledger sentence, what a person the machine is lent to sees)
- Modify: `docs/superpowers/specs/2026-09-23-machine-sharing-grants-design.md` (only if implementation changed a ruling)
- Delete: this plan, in the last commit before the merge

- [ ] **Step 1:** Rewrite `shared-machines.md`'s consent and "who may use whose machine" sections for the three modes. Keep the two-consent table, add the directory rule, and replace the ledger sentence with the new table.
- [ ] **Step 2: Run the whole verification set:** `make test`; `MEMQL_REQUIRE_DB=1` over `scripts/ci/db-gated-packages.sh --trees`; `go test -count=1 -tags agent ./integrations/agent/ ./integrations/agent/worker/ ./component/node/`; `go test -count=1 .`; `go run ./cmd/memqllint dsl/`; `make sdk-gen-check`; `make arch-model-check`; `make concept-snapshot` (expect no change); `cd clients/os && npm run typecheck && npx vitest run && make os-build`.
- [ ] **Step 3:** `/code-review` over the branch; fix what survives verification.
- [ ] **Step 4:** Delete this plan (`git rm`), commit `Issue #5350: shared-machines docs, and the spent plan`, push, open the ONE PR (`Closes #5344` ... `#5350` each on its own line), watch `ci-required`, merge through `scripts/dev/merge-as-owner.sh`, verify `main`, close anything left open, delete the branch and the worktree.
