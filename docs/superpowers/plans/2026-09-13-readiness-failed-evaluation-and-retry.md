# Readiness: Failed Evaluation Is Unknown, and the Rewrite Retries -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A readiness evaluation that could not ask (a failed fleet read, a failed integration probe) is recorded as `unknown` with a reason -- never as `unconfigured` -- and a node whose rewrite failed retries on its own until one pass lands, so `bff-znas`/`edge` pods that boot into a saturated database stop pinning `Inference: Partly set up` until the next deploy.

**Architecture:** Three layers change together and nothing else does. (1) The pure `component/memql/readiness` package gains an `Unknown` state, a closed-vocabulary `Reason` on a node report and an `Unknown []string` list on the fold's verdict; the fold excludes unknown rows from the vote and the TypeScript mirror in `clients/os` does the same over the same fixtures. (2) The engine's evaluator returns `Unknown` instead of `Unconfigured`/`NotApplicable` when a resolver errored, and `WriteModuleReadiness` refuses to persist an unknown verdict over a known row it wrote earlier in this process (an in-process memory, keyed by module, designed so a later cluster-scope rule is a field added to one struct). (3) The recompute subscriber becomes the one place every rewrite runs: it retries a failed or unknown pass with jittered exponential backoff (2 s doubling to a 60 s cap), collapses notifications that arrive during a backoff into the retry, runs a safety-net pass every 10 minutes (jittered), logs success at INFO, and the boot write is handed to it after a 0-5 s jitter. The database connector's boot-time retry grows from SQLSTATE 53300 only to dial i/o timeouts too, inside a bounded ~15 s budget.

**Tech Stack:** Go 1.26 (`component/memql`, `component/database`, `app`), MemQL DSL (`dsl/platform`), TypeScript + vitest (`clients/os`), the concept-field snapshot (`component/conceptfields`).

**Spec:** `the readiness analysis attached as a comment on znasllc-io/memql#5316 (rulings D1-D7)` (rulings D1, D2, D7 only; D3-D6 are a later plan), with the production evidence in `the production evidence attached as a comment on znasllc-io/memql#5316`. Task 8 copies the rulings into the repo as a design record so the argument survives the scratchpad.

## Global Constraints

- **Additive schema only.** Removing a concept field or an enum value BRICKS stored rows (root `CLAUDE.md`, memql#5199/#5209). The `moduleReadiness.state` enum GAINS `"unknown"`; nothing is removed or made `@required`. `make concept-snapshot` regenerates the snapshot; `make concept-snapshot-check` must pass with no `retired` entry.
- **D1 invariant:** a verdict produced by a failed evaluation is never persisted over a known one. The rule is a pure function over a struct input (`readinessPersistInput`) so D3's cluster scope is added as one more field, never as a second code path.
- **The reason is a closed vocabulary.** `readiness.ReasonFleetReadFailed = "fleetReadFailed"` and `readiness.ReasonIntegrationProbeFailed = "integrationProbeFailed"`. An error string never reaches a row (rows are broadcast; `TestReadinessRowsCarryNoValues` greps them).
- **D3 compatibility:** an `unknown` verdict is written ONLY when this process has never written a known verdict for that module. A cluster-scoped row (future) must never be overwritten by `unknown`; the input struct carries the fact the future rule needs.
- **Retry timings (D2), exact values:** debounce `2s` (unchanged), retry base `2s`, retry cap `60s`, safety net `10m` jittered +-20%, boot jitter `0-5s`, boot re-write delay `30s` (unchanged). The safety net re-evaluates and writes every pass; write-only-if-changed is D4 and is NOT in this plan.
- **Connector budget (D7):** retry on SQLSTATE 53300 AND on a dial/handshake i/o timeout, both inside a wall-clock budget of `15s` for a caller with no earlier deadline; a context deadline always wins. Pool sizes stay 4/2.
- **Production facts the record must state correctly:** the deployed Postgres is ONE CNPG instance with `max_connections=200` (the conn-monitor's budget is 183), deployed from the `memql-znas` repo's overlay -- NOT the 400/3-instance `top` preset the engine repo's cloud overlay declares. SQLSTATE 53300 during a rolling deploy comes from old and new pods overlapping: 16 old + up to 18 new pods x up to 8 connections.
- **Go/TS fold parity:** every fixture under `component/memql/readiness/testdata/fold/` must pass on both sides (`fold_test.go`, `clients/os/test/system/readinessFold.test.ts`). Fixture 10 turns the TS suite red at Task 1 and green at Task 6; both tasks ship in this one branch.
- **Testing:** never verify with `go test ./...` (it misses `component/memql`); use the exact commands in each Run step, and `make test` at the end. Db-gated cases self-skip without a database; run them with `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:5432/memql` against a real Postgres (an open port 5432 from k3d is not a database).
- **Repo rules:** branch + PR, never push `main`; stage files by explicit path (`git add <file>`), never `git add -A`; no emojis anywhere; no backwards-compat shims; no `if env == ...` branching in engine code; end every commit message with the attribution trailer shown in the commit steps.
- **Out of scope (later plan):** D3 cluster row / scope, D4 write-on-change and stopped-node deletion, D5 card copy, D6 mesh island gate.

---

## File map

| File | Responsibility in this plan |
|---|---|
| `component/memql/readiness/readiness.go` | `Unknown` state, `Reason*` constants, `NodeReport.Reason`, `Verdict.Unknown`, fold rule |
| `component/memql/readiness/fold_test.go` | fixture reader learns `expect[].unknown`; floor 10 fixtures |
| `component/memql/readiness/testdata/fold/10-unknown-is-not-a-vote.json` | the shared fixture (new) |
| `component/memql/readiness_eval.go` | evaluator returns `Unknown` + reason on a failed read / probe |
| `component/memql/readiness_inference_test.go`, `readiness_eval_test.go` | inverted tests |
| `dsl/platform/concepts.memql`, `dsl/platform/mutations.memql` | enum + `reason` field / arg |
| `component/conceptfields/concept-fields.snapshot.json` | regenerated |
| `component/memql/readiness_write.go` | `readinessMemory`, `persistReadiness`, `ReadinessUnknownError`, `writeModuleReadiness` core |
| `component/memql/engine.go` | `readinessMemory` field |
| `component/memql/readiness_write_test.go` (new), `readiness_db_test.go` | write-skip tests |
| `component/memql/readiness_recompute_subscriber.go` + `_test.go` | retry, collapse, safety net, INFO log, boot jitter, comment rewrite |
| `app/run.go` | boot write handed to the subscriber after jitter |
| `component/node/routing.go`, `component/node/CLAUDE.md` | one sentence each on delivery |
| `clients/os/src/system/readinessFold.ts`, `clients/os/src/live/readiness.tsx` | TS mirror |
| `clients/os/test/system/readinessFold.test.ts`, `clients/os/test/live/readinessRows.test.ts` (new) | TS tests |
| `component/database/conn_retry.go` + `_test.go` | dial-timeout retry inside a budget |
| `docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md` (new), `docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md`, `docs/public/operate/configuration-readiness.md` | the record and the public fold rule |

---

### Task 1: The `readiness` package -- `Unknown` state, `Reason`, `Verdict.Unknown`, and the shared fixture

**Files:**
- Modify: `component/memql/readiness/readiness.go:28-40` (states), `:58-67` (NodeReport), `:84-91` (Verdict), `:116-192` (rank, Fold)
- Modify: `component/memql/readiness/fold_test.go:12-59`
- Create: `component/memql/readiness/testdata/fold/10-unknown-is-not-a-vote.json`

**Interfaces:**
- Consumes: nothing new.
- Produces (later tasks rely on these exact names):
  - `readiness.Unknown State = "unknown"`
  - `readiness.ReasonFleetReadFailed = "fleetReadFailed"`, `readiness.ReasonIntegrationProbeFailed = "integrationProbeFailed"`
  - `NodeReport.Reason string` (`json:"reason,omitempty"`)
  - `Verdict.Unknown []string` (`json:"unknown"`, never nil)
  - `Fold` excludes `Unknown` rows from the vote and lists their node ids, sorted, in `Verdict.Unknown`; a module whose live rows are all unknown is `Unreported`.

- [ ] **Step 0: Branch**

```bash
cd /home/znas/projects/memql/memql
git checkout main && git pull --ff-only
git checkout -b fix/readiness-unknown-and-retry
```

- [ ] **Step 1: Write the failing fixture and extend the fixture reader**

Create `component/memql/readiness/testdata/fold/10-unknown-is-not-a-vote.json`:

```json
{
  "name": "unknown is not a vote: excluded from the verdict, listed by node; all-unknown is unreported",
  "now": "2026-09-13T12:00:00Z",
  "reports": [
    {"module": "storage", "nodeId": "bff-a", "nodeType": "bff", "state": "configured", "core": true, "lanes": [], "reportedAt": "2026-09-13T11:58:00Z"},
    {"module": "storage", "nodeId": "bff-b", "nodeType": "bff", "state": "unknown", "reason": "fleetReadFailed", "core": true, "lanes": [], "reportedAt": "2026-09-13T11:58:00Z"},
    {"module": "storage", "nodeId": "agent-dead", "nodeType": "agent", "state": "unknown", "reason": "fleetReadFailed", "core": true, "lanes": [], "reportedAt": "2026-09-13T11:58:00Z"},
    {"module": "email", "nodeId": "bff-a", "nodeType": "bff", "state": "unknown", "reason": "integrationProbeFailed", "core": true, "lanes": [], "reportedAt": "2026-09-13T11:58:00Z"},
    {"module": "email", "nodeId": "bff-b", "nodeType": "bff", "state": "unknown", "reason": "integrationProbeFailed", "core": true, "lanes": [], "reportedAt": "2026-09-13T11:58:00Z"}
  ],
  "nodes": [
    {"nodeId": "bff-a", "health": "healthy", "lastSeen": "2026-09-13T11:59:40Z"},
    {"nodeId": "bff-b", "health": "healthy", "lastSeen": "2026-09-13T11:59:40Z"},
    {"nodeId": "agent-dead", "health": "stopped", "lastSeen": "2026-09-13T11:00:00Z"}
  ],
  "expect": [
    {"module": "email", "state": "unreported", "disagreement": [], "unknown": ["bff-a", "bff-b"]},
    {"module": "storage", "state": "configured", "disagreement": [], "unknown": ["bff-b"]}
  ]
}
```

Modify `component/memql/readiness/fold_test.go` -- the fixture struct and the loop:

```go
type foldFixture struct {
	Name    string         `json:"name"`
	Now     time.Time      `json:"now"`
	Reports []NodeReport   `json:"reports"`
	Nodes   []NodeLiveness `json:"nodes"`
	Expect  []struct {
		Module       string   `json:"module"`
		State        State    `json:"state"`
		Disagreement []string `json:"disagreement"`
		// Unknown is OPTIONAL in a fixture: the nine older cases predate the
		// field and mean "no unknown node", which is what a nil decodes to
		// and what the normalisation below compares against.
		Unknown []string `json:"unknown"`
	} `json:"expect"`
}

// THE FIXTURES ARE SHARED. clients/os/test/system/readinessFold.test.ts reads
// the same directory, so a case added here is a case the TypeScript mirror
// must also pass; that is the whole parity mechanism.
func TestFoldFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "fold", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 10 {
		t.Fatalf("expected at least 10 fold fixtures, found %d -- the parity set is incomplete", len(paths))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx foldFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		t.Run(fx.Name, func(t *testing.T) {
			got := Fold(fx.Reports, fx.Nodes, fx.Now)
			if len(got) != len(fx.Expect) {
				t.Fatalf("got %d verdicts, want %d: %+v", len(got), len(fx.Expect), got)
			}
			for i, want := range fx.Expect {
				if got[i].Module != want.Module || got[i].State != want.State {
					t.Errorf("verdict %d: got %s=%s, want %s=%s", i, got[i].Module, got[i].State, want.Module, want.State)
				}
				if !reflect.DeepEqual(got[i].Disagreement, want.Disagreement) {
					t.Errorf("verdict %d disagreement: got %v, want %v", i, got[i].Disagreement, want.Disagreement)
				}
				wantUnknown := want.Unknown
				if wantUnknown == nil {
					wantUnknown = []string{}
				}
				if got[i].Unknown == nil {
					t.Errorf("verdict %d: Unknown is nil; the JSON contract is an empty list, never null", i)
				}
				if !reflect.DeepEqual(got[i].Unknown, wantUnknown) {
					t.Errorf("verdict %d unknown: got %v, want %v", i, got[i].Unknown, wantUnknown)
				}
			}
		})
	}
}
```

Also add, at the bottom of `fold_test.go`, a unit case that does not depend on the fixture file:

```go
// A NODE THAT COULD NOT EVALUATE IS NOT A NODE THAT SAID "NO". Before this,
// a failed fleet read on one replica was written as `unconfigured`, folded
// against the others' `configured`, and read as "Partly set up" on every
// surface until the next deploy (production, 2026-09-13).
func TestAnUnknownRowNeverPinsAVerdict(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	nodes := []NodeLiveness{
		{NodeId: "a", Health: "healthy", LastSeen: now.Add(-time.Second)},
		{NodeId: "b", Health: "healthy", LastSeen: now.Add(-time.Second)},
	}
	got := Fold([]NodeReport{
		{Module: "ai", NodeId: "a", NodeType: "agent", State: Configured, ReportedAt: now},
		{Module: "ai", NodeId: "b", NodeType: "bff", State: Unknown, Reason: ReasonFleetReadFailed, ReportedAt: now},
	}, nodes, now)
	if len(got) != 1 || got[0].State != Configured {
		t.Fatalf("an unknown row changed the verdict: %+v", got)
	}
	if len(got[0].Disagreement) != 0 {
		t.Errorf("an unknown row counted as disagreement: %v", got[0].Disagreement)
	}
	if !reflect.DeepEqual(got[0].Unknown, []string{"b"}) {
		t.Errorf("the unknown node is not named: %v", got[0].Unknown)
	}
	for _, nv := range got[0].Nodes {
		if nv.NodeId == "b" {
			t.Errorf("the unknown node appears among the voters: %+v", got[0].Nodes)
		}
	}
}
```

- [ ] **Step 2: Run the fixture tests to verify they fail**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/readiness/ -run 'TestFoldFixtures|TestAnUnknownRowNeverPinsAVerdict' -v`
Expected: compile FAIL -- `undefined: Unknown`, `undefined: ReasonFleetReadFailed`, `got[i].Unknown undefined`.

- [ ] **Step 3: Implement the state, the reason vocabulary, the fields and the fold rule**

In `component/memql/readiness/readiness.go`, replace the `const (...)` state block with:

```go
const (
	Configured    State = "configured"
	Partial       State = "partial"
	Unconfigured  State = "unconfigured"
	NotApplicable State = "notApplicable"
	// Unreported is the fold's word for "no live node reported this module".
	// It is never spelled Unconfigured: not knowing and not being configured
	// are different answers, and the OS draws nothing for this one.
	Unreported State = "unreported"
	// Unknown is a NODE's word for "I could not evaluate this": a resolver the
	// verdict depends on errored (the fleet read, an integration probe). It is
	// never a vote. The fold excludes it from the verdict and names the node
	// in Verdict.Unknown, and the writer never persists it over a known row
	// (component/memql/readiness_write.go). The distinction is the same one
	// the OS keeps for passkeys ("a failed read is unknown, never none"): a
	// read that broke, written as `unconfigured`, was folded against other
	// nodes' correct rows and read as "Partly set up" until the next deploy.
	Unknown State = "unknown"
)

// Reasons an evaluation answers Unknown. A CLOSED vocabulary: rows are
// broadcast to every signed-in reader, so an error string -- which can carry
// an address, a role name or a DSN fragment -- never rides on one. The log
// line at the evaluator carries the error; the row carries only which
// resolver could not answer.
const (
	ReasonFleetReadFailed        = "fleetReadFailed"
	ReasonIntegrationProbeFailed = "integrationProbeFailed"
)
```

Replace `NodeReport` with:

```go
// NodeReport is one row of v1:platform:moduleReadiness.
type NodeReport struct {
	Module     string       `json:"module"`
	NodeId     string       `json:"nodeId"`
	NodeType   string       `json:"nodeType"`
	State      State        `json:"state"`
	// Reason is set only when State is Unknown, from the Reason* vocabulary.
	Reason     string       `json:"reason,omitempty"`
	Core       bool         `json:"core"`
	Lanes      []LaneReport `json:"lanes"`
	ReportedAt time.Time    `json:"reportedAt"`
}
```

Replace `Verdict` with:

```go
// Verdict is the cluster-wide answer for one module.
type Verdict struct {
	Module       string        `json:"module"`
	State        State         `json:"state"`
	Core         bool          `json:"core"`
	Disagreement []string      `json:"disagreement"`
	Nodes        []NodeVerdict `json:"nodes"`
	// Unknown names every LIVE node whose row says it could not evaluate,
	// sorted. Those rows are not votes: they are neither in Nodes nor in
	// Disagreement. Never nil on the wire -- an empty list, so a reader
	// that iterates it needs no null check.
	Unknown []string `json:"unknown"`
}
```

Replace the `Fold` doc comment and body with:

```go
// Fold turns every node's report into one verdict per module.
//
//  1. Keep reports from live nodes only, so a dead replica's stale row cannot
//     pin a verdict.
//  2. Drop notApplicable.
//  3. Set aside unknown: a node that could not evaluate is named in
//     Verdict.Unknown and casts no vote.
//  4. Worst state wins.
//  5. If the kept reports disagree, the verdict is partial and Disagreement
//     names every live reporter as nodeId=state, worst state first.
//  6. A module with no kept report is unreported -- including a module whose
//     every live row is unknown, which is the honest answer and the one that
//     lets the core gate open rather than hold on a read that broke.
//
// Modules appear in name order, so two folds over the same rows are equal.
func Fold(reports []NodeReport, nodes []NodeLiveness, now time.Time) []Verdict {
	live := map[string]bool{}
	for _, n := range nodes {
		if NodeIsLive(n, now) {
			live[n.NodeId] = true
		}
	}
	kept := map[string][]NodeReport{}
	unknown := map[string][]string{}
	core := map[string]bool{}
	seen := map[string]bool{}
	var order []string
	for _, r := range reports {
		if !seen[r.Module] {
			seen[r.Module] = true
			order = append(order, r.Module)
		}
		core[r.Module] = core[r.Module] || r.Core
		if !live[r.NodeId] || r.State == NotApplicable {
			continue
		}
		if r.State == Unknown {
			unknown[r.Module] = append(unknown[r.Module], r.NodeId)
			continue
		}
		kept[r.Module] = append(kept[r.Module], r)
	}
	sort.Strings(order)
	out := make([]Verdict, 0, len(order))
	for _, module := range order {
		v := Verdict{Module: module, Core: core[module], Disagreement: []string{}, Nodes: []NodeVerdict{}, Unknown: []string{}}
		if ids := unknown[module]; len(ids) > 0 {
			sort.Strings(ids)
			v.Unknown = ids
		}
		rs := kept[module]
		if len(rs) == 0 {
			v.State = Unreported
			out = append(out, v)
			continue
		}
		sort.Slice(rs, func(i, j int) bool {
			if rank(rs[i].State) != rank(rs[j].State) {
				return rank(rs[i].State) > rank(rs[j].State)
			}
			return rs[i].NodeId < rs[j].NodeId
		})
		states := map[State]bool{}
		for _, r := range rs {
			states[r.State] = true
			v.Nodes = append(v.Nodes, NodeVerdict{NodeId: r.NodeId, NodeType: r.NodeType, State: r.State, ReportedAt: r.ReportedAt})
		}
		if len(states) > 1 {
			v.State = Partial
			for _, r := range rs {
				v.Disagreement = append(v.Disagreement, r.NodeId+"="+string(r.State))
			}
		} else {
			v.State = rs[0].State
		}
		out = append(out, v)
	}
	return out
}
```

`rank` is unchanged (Unknown never reaches it).

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `cd /home/znas/projects/memql/memql && go vet ./component/memql/readiness/ && go test -count=1 ./component/memql/readiness/ -v`
Expected: PASS, including all ten fixtures and `TestAnUnknownRowNeverPinsAVerdict`. Then run the TS side to record the expected red:

Run: `cd /home/znas/projects/memql/memql/clients/os && npx vitest run test/system/readinessFold.test.ts`
Expected: FAIL on exactly the fixture-10 case (`unknown` folds as a state on the TS side until Task 6) and on "finds the shared fixtures" only if it still says 9 -- it says `>= 9`, so it passes. This red is expected and is cleared by Task 6 on this same branch.

- [ ] **Step 5: Commit**

```bash
cd /home/znas/projects/memql/memql
git add component/memql/readiness/readiness.go component/memql/readiness/fold_test.go component/memql/readiness/testdata/fold/10-unknown-is-not-a-vote.json
git commit -m "feat(readiness): an unknown row is not a vote -- Unknown state, Reason, Verdict.Unknown, fixture 10

A node that could not evaluate (a failed fleet read, a failed probe) was
written as unconfigured and folded against other nodes' correct rows, so
one replica booting into a saturated database read as \"Partly set up\"
on every surface until the next deploy. Unknown is now its own state:
excluded from the verdict, named per node, unreported when it is all
there is. The TS mirror follows in the same branch.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 2: The evaluator returns `Unknown` with a reason on a failed read and a failed probe

**Files:**
- Modify: `component/memql/readiness_eval.go:49-59` (Logger doc), `:137-173` (ai arm), `:174-194` (integration arm)
- Modify: `component/memql/readiness_inference_test.go:97-113`, `:356-404`
- Modify: `component/memql/readiness_eval_test.go:204-216`

**Interfaces:**
- Consumes: `readiness.Unknown`, `readiness.ReasonFleetReadFailed`, `readiness.ReasonIntegrationProbeFailed`, `NodeReport.Reason` (Task 1).
- Produces: `evaluateModule` answers `State: readiness.Unknown, Reason: <vocabulary>, Lanes: []` when `Registrations` errors, and `State: readiness.Unknown, Reason: readiness.ReasonIntegrationProbeFailed` when `IntegrationState` errors; `!registered` without an error stays `NotApplicable`. Signature unchanged: `evaluateModule(ctx, r readinessResolvers, mod envregistry.Module, nodeId, nodeType string, now time.Time) readiness.NodeReport`.

- [ ] **Step 1: Invert the tests that pin the old behaviour**

In `component/memql/readiness_inference_test.go`, replace `TestAFailedRegistrationReadShutsEveryDoor` (lines 97-113) with:

```go
// A READ THAT FAILS IS UNKNOWN, NOT UNCONFIGURED (D1, 2026-09-13).
//
// The shipped arm left every door shut on a failed read, on the argument
// that "we could not ask" reported as an open door sends somebody into a
// console whose features refuse. That is the right direction for the GATE
// and the wrong one for a PERSISTED row: the row outlives the failure, is
// folded against other nodes' correct rows, and read "Partly set up" on the
// owner's cluster for a whole deploy. Unknown is excluded from the fold
// (readiness.Fold rule 3) and never persisted over a known row
// (readiness_write.go), so the gate still never opens on a read that broke
// -- it simply stops closing on one.
func TestAFailedRegistrationReadIsUnknownNotUnconfigured(t *testing.T) {
	r := readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("the read did not land")
		},
	}
	got := evaluateModule(context.Background(), r, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Unknown {
		t.Fatalf("a failed read produced %s, want unknown", got.State)
	}
	if got.Reason != readiness.ReasonFleetReadFailed {
		t.Errorf("reason %q, want %q -- the row must say WHICH resolver could not answer", got.Reason, readiness.ReasonFleetReadFailed)
	}
	if len(got.Lanes) != 0 {
		t.Errorf("a failed read reported %d lanes; there is no lane evidence to report: %+v", len(got.Lanes), got.Lanes)
	}
	// A CLOSED VOCABULARY: the error text never reaches the report, which is
	// broadcast to every signed-in reader.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "the read did not land") {
		t.Errorf("the error string leaked into the report: %s", raw)
	}
}
```

Add `"encoding/json"` to that file's imports.

Replace the body of `TestAFailedFleetReadIsLoggedRatherThanSilentlyShut` (lines 356-392) -- keep the name, change the doc and the assertion:

```go
// A FLEET READ THAT BROKE IS SAID OUT LOUD (memql#5118), and the verdict now
// says it too: the row reads `unknown` with a reason. The log line still
// carries what the row may not -- the error itself and the node -- because
// the repair is an operator's, not a person's.
func TestAFailedFleetReadIsLoggedRatherThanSilentlyShut(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	broken := readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("registration read refused")
		},
		Logger: logger,
	}

	got := evaluateModule(context.Background(), broken, aiModule(), "bff-1", "bff", inferenceNow)

	if got.State != readiness.Unknown {
		t.Errorf("a failed fleet read reported %s -- an unaskable door is unknown, never a verdict", got.State)
	}
	line := buf.String()
	if line == "" {
		t.Fatal("a failed fleet read produced no log line at all: an operator sees an `unknown` row " +
			"with nothing anywhere saying why the read broke")
	}
	for _, want := range []string{"registration read refused", "bff-1", "ai"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line does not carry %q, so it cannot be acted on: %s", want, line)
		}
	}
}
```

Replace `TestAResolverSetWithNoLoggerStillEvaluates` (lines 394-404) with:

```go
// A resolver set with no logger is legal, and the pure tests pass one.
func TestAResolverSetWithNoLoggerStillEvaluates(t *testing.T) {
	got := evaluateModule(context.Background(), readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("boom")
		},
	}, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Unknown || got.Reason != readiness.ReasonFleetReadFailed {
		t.Errorf("state %s reason %q", got.State, got.Reason)
	}
}
```

In `component/memql/readiness_eval_test.go`, replace `TestIntegrationEvaluatorErrorIsNotApplicable` (lines 204-216) with:

```go
// An integration that answers with an ERROR is unknown: a probe that failed
// says nothing about whether a person did the setup. It used to be
// notApplicable, which is the word for "not hosted here" -- and a failed
// probe on a node that does host the integration hid behind the same word
// as a node that does not. An integration that is NOT REGISTERED (no error)
// stays notApplicable; that case is in TestIntegrationEvaluator.
func TestIntegrationEvaluatorErrorIsUnknown(t *testing.T) {
	mod := envregistry.Module{Name: "email", Core: true, Description: "d", Evaluator: "integration:email"}
	r := fakeResolvers(nil, nil, nil)
	r.IntegrationState = func(context.Context, string) (string, bool, bool, error) {
		return "", false, true, errors.New("probe failed")
	}
	got := evalOne(t, r, mod)
	if got.State != readiness.Unknown {
		t.Fatalf("got %s, want unknown", got.State)
	}
	if got.Reason != readiness.ReasonIntegrationProbeFailed {
		t.Fatalf("reason %q, want %q", got.Reason, readiness.ReasonIntegrationProbeFailed)
	}
}
```

- [ ] **Step 2: Run the three tests to verify they fail**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/ -run 'TestAFailedRegistrationReadIsUnknownNotUnconfigured|TestAFailedFleetReadIsLoggedRatherThanSilentlyShut|TestAResolverSetWithNoLoggerStillEvaluates|TestIntegrationEvaluatorErrorIsUnknown' -v`
Expected: FAIL -- `a failed read produced unconfigured, want unknown`, `got notApplicable, want unknown`.

- [ ] **Step 3: Change the two arms**

In `component/memql/readiness_eval.go`, replace the `Logger` field comment (lines 49-59) with:

```go
	// Logger records the reads that FAILED. A failed read answers Unknown
	// with a closed-vocabulary reason (D1, 2026-09-13); the log line is where
	// the error itself survives, because a row is broadcast and an error
	// string may carry an address or a DSN fragment.
	//
	// Optional: nil is a valid resolver set, and the pure tests pass one.
	Logger *slog.Logger
```

Replace the `ai` arm (the `case mod.Evaluator == envregistry.EvaluatorInferenceStatus:` block, lines 138-173) with:

```go
	case mod.Evaluator == envregistry.EvaluatorInferenceStatus:
		// A READ THAT FAILS IS UNKNOWN, NOT A SHUT DOOR (D1, 2026-09-13).
		// The old arm left every door shut, which is the right direction
		// for the gate and the wrong one for a row: the row outlives the
		// failure and is folded against other nodes' correct rows. The fold
		// excludes Unknown from the vote and the writer never persists it
		// over a known row, so the gate still cannot OPEN on a read that
		// broke -- it stops CLOSING on one.
		var regs []readiness.RegistrationFacts
		if r.Registrations != nil {
			got, err := r.Registrations(ctx)
			if err != nil {
				if r.Logger != nil {
					r.Logger.Warn("readiness: could not read the fleet; the inference verdict is unknown, not unconfigured",
						"module", mod.Name, "nodeId", nodeId, "nodeType", nodeType, "error", err)
				}
				out.State = readiness.Unknown
				out.Reason = readiness.ReasonFleetReadFailed
				return out
			}
			regs = got
		}
		out.Lanes = readiness.InferenceLanes(readiness.InferenceInput{
			Registrations:        regs,
			FederationConfigured: r.FederationConfigured != nil && r.FederationConfigured(),
			Now:                  now,
		})
		// ONE COMPLETE LANE CONFIGURES THE MODULE, the same rule the default
		// arm below applies to every lane-driven module. There is deliberately
		// no `partial` here: a door is configured or it is not, and a lane
		// that is complete but not live is a machine asleep -- which the
		// `live` slot says, and which is not a half-finished setup.
		if readiness.InferenceConfigured(out.Lanes) {
			out.State = readiness.Configured
		} else {
			out.State = readiness.Unconfigured
		}
```

Replace the integration arm's `switch` (lines 180-194) with:

```go
		switch {
		// A probe that FAILED says nothing about whether a person did the
		// setup, and it is not "not hosted here" either: unknown, with the
		// reason, so a fresh node can say "could not evaluate" and a node
		// with a known row keeps it (readiness_write.go).
		case err != nil:
			if r.Logger != nil {
				r.Logger.Warn("readiness: integration probe failed; the verdict is unknown",
					"module", mod.Name, "integration", name, "nodeId", nodeId, "nodeType", nodeType, "error", err)
			}
			out.State = readiness.Unknown
			out.Reason = readiness.ReasonIntegrationProbeFailed
		// An integration this node does not carry is not applicable here.
		case !registered:
			out.State = readiness.NotApplicable
		// unhealthy is CONFIGURED: the setup was done and the send is
		// failing for some other reason, which is a different repair.
		case state == "configured" || state == "unhealthy":
			out.State = readiness.Configured
		case touched:
			out.State = readiness.Partial
		default:
			out.State = readiness.Unconfigured
		}
```

- [ ] **Step 4: Run the evaluator suites to verify they pass**

Run: `cd /home/znas/projects/memql/memql && go vet ./component/memql/ && go test -count=1 ./component/memql/ -run 'Readiness|Inference|Integration|FleetRead|RegistrationRead|Resolver' -v`
Expected: PASS; `TestEveryNodeTypeProducesTheSameInferenceReport`, `TestReadinessEvaluatesAsTheClusterAndNotAsTheCaller`, `TestIntegrationEvaluator` and `TestReportsCarryNoValues` unchanged and green.

- [ ] **Step 5: Commit**

```bash
cd /home/znas/projects/memql/memql
git add component/memql/readiness_eval.go component/memql/readiness_inference_test.go component/memql/readiness_eval_test.go
git commit -m "fix(readiness): a failed fleet read or probe evaluates to unknown, never unconfigured or notApplicable

The ai arm mapped a FAILED registration read to unconfigured and the
integration arm mapped a failed probe to notApplicable. Both are verdicts
about the cluster; neither was one. The report now answers Unknown with a
closed-vocabulary reason (fleetReadFailed, integrationProbeFailed); the
error stays in the log line. Two tests that pinned the old mapping are
inverted.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 3: The DSL -- `unknown` in the enum, a `reason` field, the snapshot regenerated

**Files:**
- Modify: `dsl/platform/concepts.memql:710-728` (the `moduleReadiness` concept)
- Modify: `dsl/platform/mutations.memql:2137-2169` (`recordModuleReadiness`)
- Modify (generated): `component/conceptfields/concept-fields.snapshot.json` (via `make concept-snapshot`)
- Modify: `component/memql/readiness_write.go:57-77` (`renderRecordModuleReadiness` renders `reason`)
- Test: `component/memql/readiness_write_render_test.go` (new, pure)

**Interfaces:**
- Consumes: `readiness.NodeReport.Reason` (Task 1).
- Produces: the mutation accepts `state: "unknown"` and an optional `reason: string`; `renderRecordModuleReadiness(r readiness.NodeReport, rowId string) (string, error)` emits `, reason: "<vocab>"` only when `r.Reason != ""`. Task 4 writes through this render.

- [ ] **Step 1: Write the failing render test**

Create `component/memql/readiness_write_render_test.go`:

```go
package memql

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/readiness"
)

var renderNow = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

// THE REASON RIDES ON THE ROW, and only when there is one. A known verdict
// renders no reason argument at all, so the mutation's own default applies
// and the nine older readers of this row see nothing new.
func TestRenderCarriesTheReasonOnlyWhenUnknown(t *testing.T) {
	unknown := readiness.NodeReport{Module: "ai", NodeId: "bff-1", NodeType: "bff", State: readiness.Unknown,
		Reason: readiness.ReasonFleetReadFailed, Core: true, ReportedAt: renderNow}
	call, err := renderRecordModuleReadiness(unknown, readinessRowID("ai", "bff-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`state: "unknown"`, `reason: "fleetReadFailed"`, `lanes: []`} {
		if !strings.Contains(call, want) {
			t.Errorf("the rendered call lacks %s: %s", want, call)
		}
	}

	known := readiness.NodeReport{Module: "ai", NodeId: "bff-1", NodeType: "bff", State: readiness.Configured,
		Core: true, ReportedAt: renderNow}
	call, err = renderRecordModuleReadiness(known, readinessRowID("ai", "bff-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(call, "reason") {
		t.Errorf("a known verdict rendered a reason argument: %s", call)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/ -run TestRenderCarriesTheReasonOnlyWhenUnknown -v`
Expected: FAIL -- `the rendered call lacks reason: "fleetReadFailed"`.

- [ ] **Step 3: Edit the concept, the mutation and the render**

In `dsl/platform/concepts.memql`, replace the `moduleReadiness` concept (lines 710-728) with:

```memql
/// One node's verdict on one module (design record 2026-09-06-configuration-readiness, section
/// 4.4). Rewritten as a new version of the same id, v1:platform:moduleReadiness:<module>--<nodeId>,
/// on every boot and after a providers reload or an integration configure, so a read collapses to
/// the latest. Carries presence and source only, never a value.
///
/// TIER: public, requiresIdentity. Any signed-in person reads it -- the setup surface has to tell a
/// viewer "not set up" as honestly as it tells an owner -- and nobody anonymous does. The write is
/// the engine's alone: recordModuleReadiness is @serverOnly.
///
/// `unknown` (design record 2026-09-13-readiness-unknown-and-retry, D1) is the node's word for
/// "I could not evaluate this": a resolver the verdict depends on errored. It is never a vote in
/// the fold and is never written over a known row; `reason` names which resolver, from a closed
/// vocabulary (fleetReadFailed, integrationProbeFailed), never an error string. ADDITIVE on
/// purpose: dropping an enum value bricks stored rows (root CLAUDE.md).
@rowAuthz(public, requiresIdentity)
@displayCard(primary="module", secondary="nodeId", tertiary="nodeType", status="state")
concept moduleReadiness {
  module      string!  @description("The readiness module id from the env manifest's modules block: ai, storage, email, githubApp, campaigns, workbench, localApps.")
  nodeId      string!  @description("The reporting node's MEMQL_NODE_ID.")
  nodeType    string!  @description("The reporting node's type.")
  state       enum("configured", "partial", "unconfigured", "notApplicable", "unknown")!  @description("This node's verdict; notApplicable means the node does not host the module; unknown means the node could not evaluate it (see reason) and casts no vote.")
  reason      string   @description("Why the state is unknown, from a closed vocabulary: fleetReadFailed, integrationProbeFailed. Empty for every other state.")
  core        bool     @description("Whether the module is one the first-run wizard walks.")
  lanes       []object @description("One entry per lane: { name, configurableFrom, complete, slots: [{ name, present, source, optional }] }. Presence and source only.")
  reportedAt  datetime!  @description("When this node evaluated, RFC3339.")
}
```

In `dsl/platform/mutations.memql`, replace the `recordModuleReadiness` mutation (lines 2146-2169) with:

```memql
@serverOnly
@description("Write one node's readiness verdict for one module as a new version of its deterministic row.")
mutate moduleReadiness recordModuleReadiness {
  args {
    rowId       string!
    module      string!
    nodeId      string!
    nodeType    string!
    state       enum("configured", "partial", "unconfigured", "notApplicable", "unknown")!
    reason      string
    core        boolean
    lanes       []object
    reportedAt  string!
  }
  insert {
    id:          args.rowId
    module:      args.module
    nodeId:      args.nodeId
    nodeType:    args.nodeType
    state:       args.state
    reason:      args.reason ?? ""
    core:        args.core ?? false
    lanes:       args.lanes ?? []
    reportedAt:  args.reportedAt
  }
}
```

(The `///` doc block above the mutation, lines 2137-2145, stays as it is.)

In `component/memql/readiness_write.go`, replace `renderRecordModuleReadiness` (lines 53-77) with:

```go
// renderRecordModuleReadiness renders the @serverOnly call as MemQL TEXT.
// Every string goes through QuoteString -- the lexer's own escaping, which
// diverges from Go's %q on four control characters (memql#4256) -- and the
// lanes ride as a JSON literal, which the parser accepts as a list of objects.
//
// `reason` is rendered only when the report carries one (an Unknown verdict),
// so a known verdict's call is byte-for-byte what it was before the field.
func renderRecordModuleReadiness(r readiness.NodeReport, rowId string) (string, error) {
	lanes := r.Lanes
	if lanes == nil {
		lanes = []readiness.LaneReport{}
	}
	raw, err := json.Marshal(lanes)
	if err != nil {
		return "", err
	}
	reason := ""
	if r.Reason != "" {
		reason = ", reason: " + langparser.QuoteString(r.Reason)
	}
	return fmt.Sprintf(
		"mutation recordModuleReadiness(rowId: %s, module: %s, nodeId: %s, nodeType: %s, state: %s%s, core: %t, lanes: %s, reportedAt: %s)",
		langparser.QuoteString(rowId),
		langparser.QuoteString(r.Module),
		langparser.QuoteString(r.NodeId),
		langparser.QuoteString(r.NodeType),
		langparser.QuoteString(string(r.State)),
		reason,
		r.Core,
		string(raw),
		langparser.QuoteString(r.ReportedAt.UTC().Format(time.RFC3339)),
	), nil
}
```

- [ ] **Step 4: Regenerate the snapshot, lint the DSL, run the tests**

Run: `cd /home/znas/projects/memql/memql && make dsl-lint && make concept-snapshot && make concept-snapshot-check`
Expected: lint clean; the snapshot diff for `v1:platform:moduleReadiness` adds `"reason"` to `fields` and `"unknown"` to `enums.state`, and adds nothing under `retired`; the check passes.

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/ -run 'TestRenderCarriesTheReasonOnlyWhenUnknown|TestTheRegistrationQueryLoadsFromTheEmbeddedTree' -v`
Expected: PASS (the second proves the edited tree still loads).

Run (db-gated round trip through the real mutation): `cd /home/znas/projects/memql/memql && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:5432/memql go test -count=1 ./component/memql/ -run 'TestBootWritesOneRowPerModuleAndARewriteVersionsThem|TestReadinessRowsCarryNoValues' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/znas/projects/memql/memql
git add dsl/platform/concepts.memql dsl/platform/mutations.memql component/conceptfields/concept-fields.snapshot.json component/memql/readiness_write.go component/memql/readiness_write_render_test.go
git commit -m "feat(dsl): moduleReadiness gains the unknown state and a reason field, additively

The enum grows; nothing is removed or made required, so stored rows keep
writing (root CLAUDE.md, memql#5199). reason is a closed vocabulary and
is rendered only on an unknown verdict. Snapshot regenerated with no
retirement.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 4: `WriteModuleReadiness` -- never persist `unknown` over a known row; a typed error when any module was unknown

**Files:**
- Modify: `component/memql/readiness_write.go:79-100` (comment), `:180-209` (`WriteModuleReadiness`)
- Modify: `component/memql/engine.go:114-115` (add the memory field beside `readinessNodeType`)
- Create: `component/memql/readiness_write_test.go`
- Modify: `component/memql/readiness_db_test.go:63-73` (the state switch), append one db-gated case

**Interfaces:**
- Consumes: `evaluateModules(ctx, r readinessResolvers, mods []envregistry.Module, nodeId, nodeType string, now time.Time) []readiness.NodeReport` (Task 2), `renderRecordModuleReadiness` (Task 3), `readiness.Unknown`.
- Produces (Task 5 and the tests rely on these):
  - `type readinessMemory struct{...}` with `hasKnown(module string) bool` and `remember(module string, s readiness.State)`; the engine holds one at `e.readinessMemory`.
  - `type readinessPersistInput struct { State readiness.State; KnownBefore bool }` and `func persistReadiness(in readinessPersistInput) bool`.
  - `type ReadinessUnknownError struct { Modules []string; Reasons map[string]string }` implementing `error`.
  - `type readinessExecutor func(ctx context.Context, call string) error`
  - `func writeModuleReadiness(ctx context.Context, r readinessResolvers, mods []envregistry.Module, nodeId, nodeType string, mem *readinessMemory, exec readinessExecutor) (int, error)`
  - `func (e *MemQLEngine) readinessExecutor() readinessExecutor`
  - `func (e *MemQLEngine) WriteModuleReadiness(ctx context.Context) (int, error)` -- signature UNCHANGED; returns `written, *ReadinessUnknownError` when any module was unknown (rows for known modules are still written), and `written, <wrapped error>` on a failed write as before.

- [ ] **Step 1: Write the failing unit tests (no database)**

Create `component/memql/readiness_write_test.go`:

```go
package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// recordingExecutor captures every rendered mutation instead of executing it.
type recordingExecutor struct {
	calls []string
	fail  error
}

func (x *recordingExecutor) exec(_ context.Context, call string) error {
	if x.fail != nil {
		return x.fail
	}
	x.calls = append(x.calls, call)
	return nil
}

func (x *recordingExecutor) callsFor(module string) []string {
	var out []string
	for _, c := range x.calls {
		if strings.Contains(c, `module: "`+module+`"`) {
			out = append(out, c)
		}
	}
	return out
}

// flippingFleet answers the fleet read from a switch the test flips.
func flippingFleet(ok *bool) readinessResolvers {
	r := fakeResolvers(nil, nil, nil)
	r.Registrations = func(context.Context) ([]readiness.RegistrationFacts, error) {
		if !*ok {
			return nil, errors.New("read tcp 10.244.0.38:38472->10.0.185.57:5432: i/o timeout")
		}
		return []readiness.RegistrationFacts{{
			Labels:     map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
			LastSeenAt: inferenceNow,
		}}, nil
	}
	return r
}

// THE INCIDENT, IN ONE TEST. bff-znas booted into a saturated database, the
// fleet read failed, and `unconfigured` was written over nothing -- then
// the 30 s re-write failed the same way, and the row stood for a deploy.
// Now: a pass that could not evaluate never writes over a known row.
func TestAnUnknownReportIsNotPersistedOverAKnownRow(t *testing.T) {
	ok := true
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	x := &recordingExecutor{}
	mods := []envregistry.Module{aiModule()}

	if _, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", mem, x.exec); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := x.callsFor("ai"); len(got) != 1 || !strings.Contains(got[0], `state: "configured"`) {
		t.Fatalf("first pass did not write configured: %v", got)
	}

	ok = false
	written, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", mem, x.exec)
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("a pass with an unknown module returned %v, want *ReadinessUnknownError so the caller retries", err)
	}
	if written != 0 {
		t.Errorf("wrote %d rows over a known verdict; want 0", written)
	}
	if got := x.callsFor("ai"); len(got) != 1 {
		t.Fatalf("the unknown pass rendered a write for ai: %v", got)
	}
	if len(unknown.Modules) != 1 || unknown.Modules[0] != "ai" || unknown.Reasons["ai"] != readiness.ReasonFleetReadFailed {
		t.Errorf("the error does not name the module and reason: %+v", unknown)
	}
	// The error text is what the subscriber logs; it must be actionable and
	// must not carry the resolver's error string (that stayed in the
	// evaluator's own log line).
	if !strings.Contains(err.Error(), "ai=fleetReadFailed") || strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error text: %q", err.Error())
	}
}

// A FRESH PROCESS SAYS "UNKNOWN", WITH THE REASON, so the Modules page can
// show "could not evaluate: fleet read failed" instead of nothing -- and so
// the fold can name the node rather than count it.
func TestAFreshNodeWritesUnknownWithAReason(t *testing.T) {
	ok := false
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	x := &recordingExecutor{}

	written, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", mem, x.exec)
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("got %v, want *ReadinessUnknownError", err)
	}
	if written != 1 {
		t.Errorf("wrote %d, want 1 (the unknown row itself)", written)
	}
	got := x.callsFor("ai")
	if len(got) != 1 || !strings.Contains(got[0], `state: "unknown"`) || !strings.Contains(got[0], `reason: "fleetReadFailed"`) {
		t.Fatalf("the fresh node did not write unknown with its reason: %v", got)
	}
	if mem.hasKnown("ai") {
		t.Error("an unknown write was remembered as known; the next unknown pass would then be skipped forever")
	}

	// AND THE FIRST SUCCESS AFTER IT IS WRITTEN AND REMEMBERED.
	ok = true
	if _, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", mem, x.exec); err != nil {
		t.Fatalf("the recovering pass errored: %v", err)
	}
	if got := x.callsFor("ai"); len(got) != 2 || !strings.Contains(got[1], `state: "configured"`) {
		t.Fatalf("recovery did not write configured: %v", got)
	}
	if !mem.hasKnown("ai") {
		t.Error("a known verdict was not remembered")
	}
}

// Known modules in the same pass are still written; the unknown one is the
// only one held back, and the error names only it.
func TestKnownModulesStillWriteBesideAnUnknownOne(t *testing.T) {
	ok := false
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	mem.remember("ai", readiness.Configured)
	x := &recordingExecutor{}
	mods := []envregistry.Module{aiModule(), twoSlotModule}

	written, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", mem, x.exec)
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("got %v", err)
	}
	if written != 1 || len(x.callsFor("storage")) != 1 || len(x.callsFor("ai")) != 0 {
		t.Fatalf("written=%d calls=%v", written, x.calls)
	}
	if len(unknown.Modules) != 1 || unknown.Modules[0] != "ai" {
		t.Errorf("unknown modules %v, want [ai]", unknown.Modules)
	}
}

// A FAILED WRITE is a different failure from an unknown evaluation and keeps
// its shape: stop at the first module, wrap the error, no typed error.
func TestAFailedWriteIsNotAnUnknownError(t *testing.T) {
	ok := true
	r := flippingFleet(&ok)
	x := &recordingExecutor{fail: errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")}
	_, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", &readinessMemory{}, x.exec)
	var unknown *ReadinessUnknownError
	if err == nil || errors.As(err, &unknown) {
		t.Fatalf("got %v, want a plain wrapped write error", err)
	}
	if !strings.Contains(err.Error(), "module readiness: write ai:") {
		t.Errorf("error text: %q", err.Error())
	}
}

// THE RULE, PURE. This is the one place D3 will add a field: a cluster-scoped
// module never writes unknown, whether or not this process knew a verdict.
func TestPersistReadinessRule(t *testing.T) {
	cases := []struct {
		in   readinessPersistInput
		want bool
	}{
		{readinessPersistInput{State: readiness.Configured, KnownBefore: false}, true},
		{readinessPersistInput{State: readiness.Configured, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.Unconfigured, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.NotApplicable, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.Unknown, KnownBefore: false}, true},
		{readinessPersistInput{State: readiness.Unknown, KnownBefore: true}, false},
	}
	for _, c := range cases {
		if got := persistReadiness(c.in); got != c.want {
			t.Errorf("%+v: got %v want %v", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/ -run 'TestAnUnknownReportIsNotPersistedOverAKnownRow|TestAFreshNodeWritesUnknownWithAReason|TestKnownModulesStillWriteBesideAnUnknownOne|TestAFailedWriteIsNotAnUnknownError|TestPersistReadinessRule' -v`
Expected: compile FAIL -- `undefined: readinessMemory`, `undefined: writeModuleReadiness`, `undefined: ReadinessUnknownError`, `undefined: persistReadiness`.

- [ ] **Step 3: Implement the memory, the rule, the typed error and the core writer**

In `component/memql/engine.go`, directly after the `readinessNodeType string` field (line 114), add:

```go
	// readinessMemory is what THIS PROCESS remembers of its own known
	// verdicts, per module, so a pass that could not evaluate never writes
	// `unknown` over a row this node already knew (D1, 2026-09-13). The
	// identity of a row is exactly one process, which is why the memory is
	// in-process and not a read of the standing row.
	readinessMemory readinessMemory
```

In `component/memql/readiness_write.go`, add `"errors"`, `"sort"` and `"sync"` to the imports, replace the paragraph at lines 92-100 ("It is also what clears ... See readiness_email_probe_test.go.") with:

```go
// It is also what clears integrations/email's statusAuthorized, and that fixed
// a second bug by the same line. app/run.go's boot write passed
// context.Background(); the evaluation ran on it; statusAuthorized refuses a
// context with no AccessContext; and evaluateModule mapped an errored probe
// to notApplicable (it maps it to unknown now, D1 2026-09-13). So `email`
// read "not applicable" on every node of every cluster, permanently. The
// cause was recorded as a lazy-sender timing problem and is not one --
// integrations/email/status.go's describer is a reproduction of the
// resolution algorithm that never materializes a sender, so timing cannot
// reach it. See readiness_email_probe_test.go.
```

and replace `WriteModuleReadiness` (lines 180-209) with:

```go
// readinessMemory is this process's record of the known verdicts it wrote.
//
// It exists for one rule: an Unknown verdict is written ONLY when this
// process has never written a known one for that module. A fresh pod says
// "could not evaluate" (so the Modules page can show why); a pod that already
// knew keeps what it knew, because the standing row is a better answer than
// "the database was busy just now". The zero value is ready to use.
type readinessMemory struct {
	mu    sync.Mutex
	known map[string]readiness.State
}

func (m *readinessMemory) hasKnown(module string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.known[module]
	return ok
}

func (m *readinessMemory) remember(module string, s readiness.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.known == nil {
		m.known = map[string]readiness.State{}
	}
	m.known[module] = s
}

// readinessPersistInput is everything the persist rule looks at. A STRUCT,
// deliberately: D3 (cluster-scoped modules, one cluster row) adds `Scope`
// here and one clause to persistReadiness -- "a cluster-scoped module never
// writes unknown, because the standing cluster row is somebody else's
// successful evaluation" -- without a second code path or a new parameter
// at every call site.
type readinessPersistInput struct {
	State       readiness.State
	KnownBefore bool
}

// persistReadiness decides whether a report is written (D1, 2026-09-13).
//
// Every known state writes. Unknown writes only when nothing known was ever
// written by this process, so "could not ask" can never replace an answer.
func persistReadiness(in readinessPersistInput) bool {
	if in.State != readiness.Unknown {
		return true
	}
	return !in.KnownBefore
}

// ReadinessUnknownError says a pass could not evaluate one or more modules.
// The rows for every KNOWN module in the same pass were written; this is
// returned so a caller retries the pass (the subscriber does, with backoff)
// rather than believing the cluster is described. Reasons are the closed
// vocabulary from component/memql/readiness, never resolver error text.
type ReadinessUnknownError struct {
	Modules []string
	Reasons map[string]string
}

func (e *ReadinessUnknownError) Error() string {
	parts := make([]string, 0, len(e.Modules))
	for _, m := range e.Modules {
		parts = append(parts, m+"="+e.Reasons[m])
	}
	return fmt.Sprintf("module readiness: %d module(s) could not be evaluated: %s", len(e.Modules), strings.Join(parts, ", "))
}

// readinessExecutor runs one rendered @serverOnly call. Injected so the
// write decision is testable with no engine and no database.
type readinessExecutor func(ctx context.Context, call string) error

// readinessExecutor is the engine's own: the rendered call under the write
// context. Built once per pass so every module's write shares one actor.
func (e *MemQLEngine) readinessExecutor(ctx context.Context) readinessExecutor {
	wctx := readinessWriteContext(ctx)
	return func(_ context.Context, call string) error {
		_, err := e.Execute(wctx, call)
		return err
	}
}

// writeModuleReadiness is the whole write decision with its inputs handed in.
//
// A FAILED WRITE stops at the first module and leaves the PREVIOUS version of
// every remaining row standing, which is the right failure: a stale verdict a
// person can act on beats a half-rewritten set nobody can interpret.
//
// AN UNKNOWN EVALUATION does not stop the pass: every known module is
// written, the unknown ones are written only on a process that never knew
// (persistReadiness), and the pass returns *ReadinessUnknownError so the
// caller retries.
func writeModuleReadiness(ctx context.Context, r readinessResolvers, mods []envregistry.Module, nodeId, nodeType string, mem *readinessMemory, exec readinessExecutor) (int, error) {
	// THE CALLER'S CONTEXT, AND THAT IS DELIBERATE. `evaluateModule` applies
	// the evaluation actor itself, at the one place a context reaches a
	// resolver -- see the comment there for why this is not a line here.
	reports := evaluateModules(ctx, r, mods, nodeId, nodeType, time.Now().UTC())
	written := 0
	unknown := &ReadinessUnknownError{Reasons: map[string]string{}}
	for _, rep := range reports {
		if rep.State == readiness.Unknown {
			unknown.Modules = append(unknown.Modules, rep.Module)
			unknown.Reasons[rep.Module] = rep.Reason
		}
		if !persistReadiness(readinessPersistInput{State: rep.State, KnownBefore: mem.hasKnown(rep.Module)}) {
			continue
		}
		call, err := renderRecordModuleReadiness(rep, readinessRowID(rep.Module, nodeId))
		if err != nil {
			return written, fmt.Errorf("module readiness: render %s: %w", rep.Module, err)
		}
		if err := exec(ctx, call); err != nil {
			return written, fmt.Errorf("module readiness: write %s: %w", rep.Module, err)
		}
		written++
		if rep.State != readiness.Unknown {
			mem.remember(rep.Module, rep.State)
		}
	}
	if len(unknown.Modules) > 0 {
		sort.Strings(unknown.Modules)
		return written, unknown
	}
	return written, nil
}

// WriteModuleReadiness evaluates every module and writes this node's rows as
// new versions of their deterministic ids. Returns how many were written.
//
// When any module could not be evaluated the error is *ReadinessUnknownError
// (errors.As); the rows for the other modules were still written. Callers
// that need the cluster described retry -- the recompute subscriber does.
func (e *MemQLEngine) WriteModuleReadiness(ctx context.Context) (int, error) {
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		return 0, fmt.Errorf("module readiness: manifest: %w", err)
	}
	nodeId, nodeType := e.readinessIdentity()
	return writeModuleReadiness(ctx, e.readinessResolvers(), manifest.Modules, nodeId, nodeType, &e.readinessMemory, e.readinessExecutor(ctx))
}

// IsReadinessUnknown reports whether err is a pass that could not evaluate
// some module, as opposed to a pass whose write failed.
func IsReadinessUnknown(err error) bool {
	var u *ReadinessUnknownError
	return errors.As(err, &u)
}
```

- [ ] **Step 4: Run the unit tests to verify they pass**

Run: `cd /home/znas/projects/memql/memql && go vet ./component/memql/ && go test -count=1 ./component/memql/ -run 'TestAnUnknownReportIsNotPersistedOverAKnownRow|TestAFreshNodeWritesUnknownWithAReason|TestKnownModulesStillWriteBesideAnUnknownOne|TestAFailedWriteIsNotAnUnknownError|TestPersistReadinessRule|TestRenderCarriesTheReasonOnlyWhenUnknown' -v`
Expected: PASS.

- [ ] **Step 5: Add the db-gated case and admit `unknown` to the boot test's state switch**

In `component/memql/readiness_db_test.go`, change the switch at lines 68-71 to:

```go
			switch r.State {
			case readiness.Configured, readiness.Partial, readiness.Unconfigured, readiness.NotApplicable, readiness.Unknown:
			default:
				t.Errorf("%s: state %q is not one of the five", r.Module, r.State)
			}
```

Append to the same file:

```go
// THE STANDING ROW SURVIVES A FAILED READ, through the real mutation and the
// real read. The unit test above proves the decision; this proves the row.
func TestAFailedFleetReadLeavesTheStandingRowUnchanged(t *testing.T) {
	e := bootReadinessTestEngine(t)
	ctx := context.Background()
	e.SetReadinessIdentity("readiness-standing-node", "bff")
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WriteModuleReadiness(ctx); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var before readiness.NodeReport
	for _, r := range readinessRowsForTest(t, e) {
		if r.NodeId == "readiness-standing-node" && r.Module == "ai" {
			before = r
		}
	}
	if before.Module == "" || before.State == readiness.Unknown {
		t.Fatalf("negative control failed: no known ai row to protect: %+v", before)
	}

	// The same engine, the same identity, a fleet read that fails.
	broken := e.readinessResolvers()
	broken.Registrations = func(context.Context) ([]readiness.RegistrationFacts, error) {
		return nil, errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")
	}
	nodeId, nodeType := e.readinessIdentity()
	_, err = writeModuleReadiness(ctx, broken, manifest.Modules, nodeId, nodeType, &e.readinessMemory, e.readinessExecutor(ctx))
	if !IsReadinessUnknown(err) {
		t.Fatalf("got %v, want a ReadinessUnknownError", err)
	}

	for _, r := range readinessRowsForTest(t, e) {
		if r.NodeId != "readiness-standing-node" || r.Module != "ai" {
			continue
		}
		if r.State != before.State || !r.ReportedAt.Equal(before.ReportedAt) {
			t.Fatalf("the ai row moved under a failed read: before %s@%s, after %s@%s",
				before.State, before.ReportedAt, r.State, r.ReportedAt)
		}
		if r.Reason != "" {
			t.Fatalf("the standing row grew a reason: %q", r.Reason)
		}
		return
	}
	t.Fatal("the ai row disappeared")
}
```

Add `"errors"` to that file's imports.

Run: `cd /home/znas/projects/memql/memql && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:5432/memql go test -count=1 ./component/memql/ -run 'TestAFailedFleetReadLeavesTheStandingRowUnchanged|TestBootWritesOneRowPerModuleAndARewriteVersionsThem|TestRecomputeRefusesAClientOrigin' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/znas/projects/memql/memql
git add component/memql/readiness_write.go component/memql/engine.go component/memql/readiness_write_test.go component/memql/readiness_db_test.go
git commit -m "fix(readiness): never persist an unknown verdict over a known row; report unknown as a typed error

WriteModuleReadiness remembers the known verdicts this process wrote and
writes unknown only when it never knew (a fresh pod), so a failed fleet
read can no longer replace a correct row. A pass with an unknown module
returns *ReadinessUnknownError so the subscriber retries. The persist
rule is a pure function over a struct; D3 adds scope there.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 5: The subscriber retries with backoff, collapses notifications during a backoff, runs a safety net, logs success; boot hands its write to it

**Files:**
- Modify: `component/memql/readiness_recompute_subscriber.go` (whole file: header comment, struct, constants, constructor, loop, `NewReadinessRecomputeSubscriber`)
- Modify: `component/memql/readiness_recompute_subscriber_test.go` (whole file: every fake `write` returns `(int, error)`; three tests replaced, four added)
- Modify: `app/run.go:220-270`
- Modify: `component/node/routing.go:216-219` (comment), `component/node/CLAUDE.md:196-203` (one sentence)

**Interfaces:**
- Consumes: `(*MemQLEngine).WriteModuleReadiness(ctx) (int, error)`, `IsReadinessUnknown(err) bool` (Task 4).
- Produces:
  - `type readinessTimings struct { Debounce, RetryBase, RetryMax, SafetyNet time.Duration }`, `func productionReadinessTimings() readinessTimings`
  - exported constants `ReadinessRecomputeDebounce = 2s` (unchanged), `ReadinessRetryBase = 2s`, `ReadinessRetryMax = 60s`, `ReadinessSafetyNetInterval = 10m`, `ReadinessSafetyNetJitter = 0.2`, `ReadinessBootJitterMax = 5s`, `ReadinessBootRewriteDelay = 30s` (unchanged)
  - `func ReadinessBootJitter() time.Duration` -- uniform in `[0, ReadinessBootJitterMax)`
  - `func newReadinessRecomputeSubscriberFor(write func(context.Context) (int, error), t readinessTimings) *ReadinessRecomputeSubscriber`
  - `func (s *ReadinessRecomputeSubscriber) backoffFor(failures int) time.Duration`, `func (s *ReadinessRecomputeSubscriber) jitteredSafetyNet() time.Duration`
  - `Notify`, `Start`, `Stop`, `StartReadinessRecomputeSubscriber`, `NotifyReadinessRecompute` -- unchanged signatures.

- [ ] **Step 1: Rewrite the test file**

Replace `component/memql/readiness_recompute_subscriber_test.go` in full:

```go
package memql

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// The debounce these cases run at. Short enough that the suite does not wait,
// long enough that a burst genuinely lands inside one window on a loaded
// machine. The PRODUCTION values are asserted separately, below, because a
// test that ran at the real seconds would be a slow test asserting constants.
const testDebounce = 60 * time.Millisecond

// testTimings keeps the safety net an hour away so only the case about it
// ever sees a safety-net pass, and the retry base a few debounces so a
// retry is observable without being immediate.
func testTimings() readinessTimings {
	return readinessTimings{
		Debounce:  testDebounce,
		RetryBase: 3 * testDebounce,
		RetryMax:  6 * testDebounce,
		SafetyNet: time.Hour,
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, limit)
}

func countingWrite(writes *atomic.Int32) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		writes.Add(1)
		return 7, nil
	}
}

// A BURST IS ONE REWRITE.
//
// A cockpit reconnecting re-advertises every model and every app it holds,
// which is a run of graph.node.updated events inside a second. One rewrite per
// event would be one full module evaluation per event -- each of which reads
// every registration in the cluster, on every node, because those events are
// broadcast.
func TestABurstRewritesOnce(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	for i := 0; i < 50; i++ {
		sub.Notify("registration")
	}
	waitFor(t, 3*time.Second, "the first rewrite", func() bool { return writes.Load() >= 1 })
	time.Sleep(4 * testDebounce)
	if got := writes.Load(); got != 1 {
		t.Fatalf("%d rewrites for one burst, want 1", got)
	}
}

// A CHANGE AFTER THE WINDOW IS ITS OWN REWRITE.
func TestAChangeAfterTheWindowRewritesAgain(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the first rewrite", func() bool { return writes.Load() == 1 })
	sub.Notify("providers")
	waitFor(t, 3*time.Second, "the second rewrite", func() bool { return writes.Load() == 2 })
}

// NO BURST WITHOUT AN EVENT, ONE PASS PER SAFETY-NET PERIOD (D2, 2026-09-13).
//
// This replaces TestNothingRewritesWithoutAnEvent, which pinned "no timer".
// The premise behind "events only" -- that the registration broadcast reaches
// every replica -- is false for a node nobody dials (bff-znas, edge, mcp
// receive no mesh events at all), so a node whose boot write failed had no
// further chance until the next deploy. The safety net is the floor under
// that: one cluster read per node per period, two orders of magnitude below
// the heartbeat-driven rewrites the same nodes already perform.
func TestASafetyNetPassRunsWithoutAnEvent(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.SafetyNet = 10 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), timings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	sub.Start(ctx)

	// Nothing early: the period is jittered +-20%, so nothing before 8 windows.
	time.Sleep(5 * testDebounce)
	if got := writes.Load(); got != 0 {
		t.Fatalf("%d rewrites inside the first half-period with no event; a fast poll is what D5 refused and D2 still refuses", got)
	}
	waitFor(t, 3*time.Second, "the first safety-net pass", func() bool { return writes.Load() == 1 })
	first := time.Since(started)
	if first < 8*testDebounce {
		t.Fatalf("the first pass ran at %s, before the jittered period's floor of %s", first, 8*testDebounce)
	}
	waitFor(t, 3*time.Second, "the second safety-net pass", func() bool { return writes.Load() == 2 })
	if gap := time.Since(started) - first; gap < 8*testDebounce {
		t.Fatalf("the second pass followed the first by %s; one pass per period, not a burst", gap)
	}
}

// A FAILED REWRITE IS RETRIED WITHOUT AN EVENT (D2, 2026-09-13).
//
// bff-znas's boot write and its 30 s re-write both failed inside the
// rollout's database saturation, and there was no third attempt: the loop
// survived and waited for an event that never reaches that node. Now a
// failed pass schedules its own retry, exponentially backed off and
// jittered, until one pass lands.
func TestAFailedRewriteIsRetriedWithoutAnEvent(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) <= 2 {
			return 0, errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")
		}
		return 7, nil
	}, testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the third attempt, which succeeds", func() bool { return writes.Load() == 3 })
	// And then it STOPS: a success ends the retry ladder.
	time.Sleep(8 * testDebounce)
	if got := writes.Load(); got != 3 {
		t.Fatalf("%d writes after the success; the retry must end on the first pass that lands", got)
	}
}

// AN UNKNOWN PASS IS RETRIED TOO. WriteModuleReadiness answers
// *ReadinessUnknownError when a module could not be evaluated, and that is a
// pass that did not describe the cluster, however many rows it wrote.
func TestAnUnknownPassIsRetried(t *testing.T) {
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) == 1 {
			return 6, &ReadinessUnknownError{Modules: []string{"ai"}, Reasons: map[string]string{"ai": "fleetReadFailed"}}
		}
		return 7, nil
	}, testTimings())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)
	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the retry after an unknown pass", func() bool { return writes.Load() == 2 })
}

// A NOTIFY DURING A BACKOFF COLLAPSES INTO THE RETRY. The retry is a full
// re-evaluation, so it covers whatever the notification was about; running
// the pass early on every event would turn a saturated database's failure
// into a tight loop driven by the 15 s heartbeat.
func TestANotifyDuringBackoffCollapsesIntoIt(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.RetryBase = 10 * testDebounce
	timings.RetryMax = 10 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		if writes.Add(1) == 1 {
			return 0, errors.New("the write did not land")
		}
		return 7, nil
	}, timings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)

	sub.Notify("registration")
	waitFor(t, 3*time.Second, "the failing pass", func() bool { return writes.Load() == 1 })
	for i := 0; i < 20; i++ {
		sub.Notify("registration")
	}
	// Half the backoff (its jitter floor is 50%): nothing may run yet.
	time.Sleep(4 * testDebounce)
	if got := writes.Load(); got != 1 {
		t.Fatalf("%d writes during the backoff; a notification must not run the pass early", got)
	}
	waitFor(t, 3*time.Second, "the retry", func() bool { return writes.Load() == 2 })
	// And the twenty notifications produced no pass of their own.
	time.Sleep(6 * testDebounce)
	if got := writes.Load(); got != 2 {
		t.Fatalf("%d writes; the notifications during the backoff ran a second pass instead of collapsing", got)
	}
}

// THE BACKOFF IS BOUNDED AND JITTERED. Exponential from the base, capped at
// the max, with a jitter floor of half the step so two nodes that failed
// together do not retry together.
func TestBackoffIsBoundedAndJittered(t *testing.T) {
	sub := newReadinessRecomputeSubscriberFor(nil, productionReadinessTimings())
	for failures := 1; failures <= 12; failures++ {
		step := ReadinessRetryBase << (failures - 1)
		if step > ReadinessRetryMax || step <= 0 {
			step = ReadinessRetryMax
		}
		for i := 0; i < 50; i++ {
			d := sub.backoffFor(failures)
			if d < step/2 || d > step {
				t.Fatalf("failures=%d: backoff %s outside [%s, %s]", failures, d, step/2, step)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[sub.backoffFor(6)] = true
	}
	if len(seen) < 2 {
		t.Fatal("fifty backoffs at the same failure count were all equal; there is no jitter")
	}
	if sub.backoffFor(1) > ReadinessRetryBase || sub.backoffFor(40) > ReadinessRetryMax {
		t.Fatal("the backoff exceeds its bounds")
	}
}

// The safety-net period is jittered +-20% so every node in a rollout does not
// re-evaluate in the same second ten minutes after the deploy.
func TestTheSafetyNetIsJittered(t *testing.T) {
	sub := newReadinessRecomputeSubscriberFor(nil, productionReadinessTimings())
	lo := time.Duration(float64(ReadinessSafetyNetInterval) * (1 - ReadinessSafetyNetJitter))
	hi := time.Duration(float64(ReadinessSafetyNetInterval) * (1 + ReadinessSafetyNetJitter))
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := sub.jitteredSafetyNet()
		if d < lo || d > hi {
			t.Fatalf("safety net %s outside [%s, %s]", d, lo, hi)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("no jitter on the safety net")
	}
}

// The boot jitter spreads a rollout's simultaneous boot writes (every pod
// runs its write the moment waitForReady returns) across five seconds.
func TestBootJitterIsWithinBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		d := ReadinessBootJitter()
		if d < 0 || d >= ReadinessBootJitterMax {
			t.Fatalf("boot jitter %s outside [0, %s)", d, ReadinessBootJitterMax)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("a hundred boot jitters were all equal")
	}
}

// A SUCCESSFUL PASS IS LOGGED AT INFO with its reason and count, so an
// operator reading a pod's log can see the recovery and not only the failure
// that preceded it (on 2026-09-13 only the failure warned).
func TestASuccessfulPassIsLoggedWithReasonAndCount(t *testing.T) {
	var buf strings.Builder
	var writes atomic.Int32
	sub := newReadinessRecomputeSubscriberFor(countingWrite(&writes), testTimings())
	sub.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub.Start(ctx)
	sub.Notify("boot")
	waitFor(t, 3*time.Second, "the pass", func() bool { return writes.Load() == 1 })
	waitFor(t, time.Second, "the log line", func() bool { return strings.Contains(buf.String(), "rows written") })
	line := buf.String()
	for _, want := range []string{"reason=boot", "modules=7"} {
		if !strings.Contains(line, want) {
			t.Errorf("the success line lacks %s: %s", want, line)
		}
	}
}

// Cancelling the context ends the loop, retries and safety net included.
func TestStoppingEndsTheLoop(t *testing.T) {
	var writes atomic.Int32
	timings := testTimings()
	timings.SafetyNet = 3 * testDebounce
	sub := newReadinessRecomputeSubscriberFor(func(context.Context) (int, error) {
		writes.Add(1)
		return 0, errors.New("always")
	}, timings)
	ctx, cancel := context.WithCancel(context.Background())
	sub.Start(ctx)
	cancel()
	time.Sleep(2 * testDebounce)
	sub.Notify("registration")
	time.Sleep(8 * testDebounce)
	if got := writes.Load(); got != 0 {
		t.Fatalf("%d rewrites after the context was cancelled", got)
	}
}

// Every method is nil-safe, which is what lets NotifyReadinessRecompute be
// called from a path that runs on a hand-built engine in a test and on a
// binary that never started the subscriber.
func TestANilSubscriberIsANoOp(t *testing.T) {
	var sub *ReadinessRecomputeSubscriber
	sub.Notify("registration")
	sub.Start(context.Background())
	sub.Stop()
	var eng *MemQLEngine
	if eng.NotifyReadinessRecompute("registration") {
		t.Error("a nil engine reported the notification delivered")
	}
	if (&MemQLEngine{}).NotifyReadinessRecompute("registration") {
		t.Error("an engine with no subscriber reported the notification delivered -- a caller " +
			"relying on that answer would skip its fallback and lose the rewrite entirely")
	}
}

// The one graph subscription this node opens, and it must actually match the
// topics the CDC path publishes.
func TestTheRegistrationPatternsMatchTheCdcTopics(t *testing.T) {
	patterns := readinessRegistrationPatterns()
	if len(patterns) == 0 {
		t.Fatal("no registration patterns at all")
	}
	for _, topic := range []string{
		events.TopicNodeCreated(WorkerRegistrationConcept),
		events.TopicNodeUpdated(WorkerRegistrationConcept),
		events.TopicNodeDeleted(WorkerRegistrationConcept),
	} {
		matched := false
		for _, p := range patterns {
			if events.Match(p, topic) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%q matches none of %v", topic, patterns)
		}
	}
	for _, topic := range []string{
		events.TopicNodeCreated("v1:identity:user"),
		events.TopicNodeUpdated("v1:work:step"),
		events.TopicNodeCreated(ModuleReadinessConcept),
	} {
		for _, p := range patterns {
			if events.Match(p, topic) {
				t.Errorf("%q matches %q; the subscription must be the registration concept alone", p, topic)
			}
		}
	}
	for _, p := range patterns {
		if strings.Contains(p, ModuleReadinessConcept) {
			t.Fatalf("pattern %q names the readiness concept itself, which is a write loop", p)
		}
	}
}

// The production constants, asserted where a reader looking for them will find
// them rather than only in a comment.
func TestTheProductionTimingsAreWhatTheRecordSays(t *testing.T) {
	if ReadinessRecomputeDebounce != 2*time.Second {
		t.Errorf("the debounce is %s; D5 (2026-09-07) says two seconds", ReadinessRecomputeDebounce)
	}
	if ReadinessBootRewriteDelay != 30*time.Second {
		t.Errorf("the boot re-write delay is %s; D5 (2026-09-07) says thirty seconds", ReadinessBootRewriteDelay)
	}
	if ReadinessRetryBase != 2*time.Second || ReadinessRetryMax != 60*time.Second {
		t.Errorf("retry %s..%s; D2 (2026-09-13) says 2 s doubling to 60 s", ReadinessRetryBase, ReadinessRetryMax)
	}
	if ReadinessSafetyNetInterval != 10*time.Minute || ReadinessSafetyNetJitter != 0.2 {
		t.Errorf("safety net %s +-%.0f%%; D2 says ten minutes +-20%%", ReadinessSafetyNetInterval, ReadinessSafetyNetJitter*100)
	}
	if ReadinessBootJitterMax != 5*time.Second {
		t.Errorf("boot jitter %s; D2 says 0-5 s", ReadinessBootJitterMax)
	}
	p := productionReadinessTimings()
	if p.Debounce != ReadinessRecomputeDebounce || p.RetryBase != ReadinessRetryBase || p.RetryMax != ReadinessRetryMax || p.SafetyNet != ReadinessSafetyNetInterval {
		t.Errorf("productionReadinessTimings does not carry the constants: %+v", p)
	}
}

// THE WIRE, from a real event bus to a real rewrite (epic memql#5118, D5).
func TestAGraphEventOnTheBusReachesTheRewrite(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	eng := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	eng.SetEventBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub := eng.StartReadinessRecomputeSubscriber(ctx)
	if sub == nil {
		t.Fatal("no subscriber was returned; nothing is wired")
	}
	// Swap the write for a counter AFTER Start, so what is under test is the
	// subscription rather than the engine's own write path -- which needs a
	// database and is covered by the db-gated readiness tests.
	var writes atomic.Int32
	sub.write = countingWrite(&writes)
	sub.timings = testTimings()

	sub.Notify("probe")
	waitFor(t, 3*time.Second, "the probe rewrite", func() bool { return writes.Load() == 1 })

	bus.Publish(events.NewEvent(
		events.TopicNodeCreated(WorkerRegistrationConcept),
		events.KindNodeCreated,
		map[string]any{"id": "v1:worker:registration:m-1"},
	))
	waitFor(t, 3*time.Second, "the rewrite a paired machine causes", func() bool {
		return writes.Load() == 2
	})

	bus.Publish(events.NewEvent(
		events.TopicNodeUpdated(ModuleReadinessConcept),
		events.KindNodeUpdated,
		map[string]any{"id": "v1:platform:moduleReadiness:ai--n"},
	))
	bus.Publish(events.NewEvent(
		events.TopicNodeCreated("v1:identity:user"),
		events.KindNodeCreated,
		map[string]any{"id": "v1:identity:user:u-1"},
	))
	time.Sleep(6 * testDebounce)
	if got := writes.Load(); got != 2 {
		t.Fatalf("%d rewrites; the subscription is matching topics beyond the registration concept", got)
	}
}
```

Add `"log/slog"` to the imports of that file.

- [ ] **Step 2: Run the file to verify it fails**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/ -run 'Rewrite|SafetyNet|Backoff|Jitter|Notify|Subscriber|ProductionTimings|CdcTopics|LoggedWithReason' -v`
Expected: compile FAIL -- `undefined: readinessTimings`, `undefined: productionReadinessTimings`, `undefined: ReadinessRetryBase`, `sub.timings undefined`.

- [ ] **Step 3: Rewrite the subscriber**

Replace `component/memql/readiness_recompute_subscriber.go` in full:

```go
package memql

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// READINESS RECOMPUTES ON THE EVENTS THAT CHANGE IT (design record
// docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md, D5),
// AND ON TWO MECHANISMS THAT DO NOT NEED AN EVENT (design record
// docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md, D2).
//
// The shipped rows were rewritten at boot, on a providers reload, on an email
// configure, or by a manual recompute -- and nothing else. So pairing a
// machine, revoking one, or a cockpit re-advertising its models left the `ai`
// verdict exactly as it was at boot. The event subscription fixed that.
//
// ===========================================================================
// WHAT "BROADCAST" DELIVERS, AND WHY EVENTS ALONE ARE NOT ENOUGH
// ===========================================================================
// A routing rule with TargetType "" is broadcast, and D5 was written on the
// premise that a broadcast reaches every replica. It does not. The mesh
// transport pushes events only client->server along a DIAL, plus a TTL-3
// relay along the receiver's own outbound dials; nothing ever pushes
// server->client. A node is reached only by nodes that DIAL it, and in the
// cloud nobody dials bff-znas, edge or mcp -- they receive no mesh event at
// all. On 2026-09-13 every node's boot write failed inside a rolling deploy's
// database saturation; agent/planner/workbench/bff re-evaluated on the next
// registration heartbeat and recovered, bff-znas and edge had exactly two
// attempts (boot, boot+30 s) and reported `unconfigured` until the next
// deploy. The transport fix is its own change (D6); this loop does not wait
// for it:
//
//   RETRY   A failed or unknown pass re-runs on an exponential, jittered
//           backoff (ReadinessRetryBase doubling to ReadinessRetryMax) until
//           one pass lands. A Notify during the backoff COLLAPSES into the
//           retry -- the retry is a full re-evaluation, so it covers whatever
//           the notification was about, and running early on every heartbeat
//           would turn a saturated database into a tight loop.
//   SAFETY  A pass every ReadinessSafetyNetInterval, jittered +-20%, whether
//           or not anything arrived. One cluster read per node per ten
//           minutes: two orders of magnitude below the heartbeat-driven
//           rewrites the dialed nodes already perform. Write-only-if-changed
//           (D4) is the later change that makes this pass free when nothing
//           moved; until then it re-evaluates and writes.
//
// ===========================================================================
// DEBOUNCED, BECAUSE A RECONNECT IS A BURST
// ===========================================================================
// A cockpit reconnecting re-advertises every model and every app it holds, as
// a run of graph.node.updated events inside one second. One rewrite per event
// would be one full module evaluation per event, each reading every
// registration in the cluster, on every node that hears it.
//
// ===========================================================================
// AND IT SERIALIZES
// ===========================================================================
// Every rewrite this node performs -- event, retry, safety net, boot -- goes
// through one loop, so two triggers arriving together cannot run two
// evaluations that race on the same deterministic row ids. That is why the
// providers-reload subscriber and app/run.go's boot path notify this rather
// than calling WriteModuleReadiness themselves.
type ReadinessRecomputeSubscriber struct {
	write   func(context.Context) (int, error)
	logger  *slog.Logger
	timings readinessTimings

	// pending is a buffered channel of ONE, and that IS the coalescing: a
	// burst of a thousand events costs one slot and one rewrite.
	pending chan string

	mu   sync.Mutex
	stop context.CancelFunc
}

// readinessTimings is every duration the loop uses, so a test can shrink all
// of them together and the production set is one value.
type readinessTimings struct {
	Debounce  time.Duration
	RetryBase time.Duration
	RetryMax  time.Duration
	SafetyNet time.Duration
}

// ReadinessRecomputeDebounce is how long a burst is collapsed for.
//
// Two seconds: long enough to swallow a reconnect's whole advertisement, short
// enough that a person who has just paired a machine sees the wizard move
// before they have finished reading the confirmation.
const ReadinessRecomputeDebounce = 2 * time.Second

// ReadinessRetryBase and ReadinessRetryMax bound the retry ladder after a
// failed or unknown pass: 2 s, 4 s, 8 s ... 60 s, each jittered down to half.
const (
	ReadinessRetryBase = 2 * time.Second
	ReadinessRetryMax  = 60 * time.Second
)

// ReadinessSafetyNetInterval is the period of the pass that needs no event,
// jittered by ReadinessSafetyNetJitter either way.
const (
	ReadinessSafetyNetInterval = 10 * time.Minute
	ReadinessSafetyNetJitter   = 0.2
)

// ReadinessBootJitterMax spreads the boot write: every pod in a rollout runs
// it the moment waitForReady returns, on top of the seed materializer, the
// cluster registration and provider resolution, which is the peak of the
// surge. app/run.go sleeps ReadinessBootJitter() before notifying.
const ReadinessBootJitterMax = 5 * time.Second

// ReadinessBootRewriteDelay is the ONE re-write after boot.
//
// It covers a node whose integration materialized lazily AFTER the boot write
// ran -- the case the email plug-in's own resolution logged twenty seconds
// late on the owner's cluster. It goes through this loop, so a failure there
// is retried like any other pass.
const ReadinessBootRewriteDelay = 30 * time.Second

func productionReadinessTimings() readinessTimings {
	return readinessTimings{
		Debounce:  ReadinessRecomputeDebounce,
		RetryBase: ReadinessRetryBase,
		RetryMax:  ReadinessRetryMax,
		SafetyNet: ReadinessSafetyNetInterval,
	}
}

// ReadinessBootJitter answers a uniform delay in [0, ReadinessBootJitterMax).
func ReadinessBootJitter() time.Duration {
	return time.Duration(rand.Int63n(int64(ReadinessBootJitterMax)))
}

// readinessRegistrationPatterns is the one graph subscription this node needs.
//
// Composed through GraphSubscriptionPatterns rather than spelled here, so the
// `graph.node.<action>.<concept>` grammar has one author. With no actions it
// yields `graph.node.*.<concept>` -- `*` is exactly the one action segment,
// and a concept id carries no dots, so the trailing remainder is one segment
// too.
//
// ALL THREE VERBS MATTER and each is a different fact: created is a machine
// paired, updated is a reconnect re-advertising its models and apps or an
// owner revoking it, deleted is a registration removed. All three carry a
// broadcast rule (component/node/routing.go); see the header for what a
// broadcast actually reaches.
func readinessRegistrationPatterns() []string {
	return events.GraphSubscriptionPatterns(WorkerRegistrationConcept, nil)
}

// NewReadinessRecomputeSubscriber builds this node's subscriber.
func NewReadinessRecomputeSubscriber(eng *MemQLEngine) *ReadinessRecomputeSubscriber {
	if eng == nil {
		return nil
	}
	sub := newReadinessRecomputeSubscriberFor(eng.WriteModuleReadiness, productionReadinessTimings())
	sub.logger = eng.safeLogger()
	return sub
}

func newReadinessRecomputeSubscriberFor(write func(context.Context) (int, error), timings readinessTimings) *ReadinessRecomputeSubscriber {
	return &ReadinessRecomputeSubscriber{
		write:   write,
		timings: timings,
		pending: make(chan string, 1),
	}
}

// Notify records that something changed. NON-BLOCKING and coalescing, so a
// caller on a hot path never waits and a burst never queues.
func (s *ReadinessRecomputeSubscriber) Notify(reason string) {
	if s == nil {
		return
	}
	select {
	case s.pending <- reason:
	default:
	}
}

// Start runs the loop until ctx is done or Stop is called. Calling it twice
// is a no-op, so a node that wires it in two places does not get two loops
// writing the same rows.
func (s *ReadinessRecomputeSubscriber) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.mu.Unlock()
	go s.loop(runCtx)
}

// Stop ends the loop. Safe to call twice, and safe on a nil receiver.
func (s *ReadinessRecomputeSubscriber) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// backoffFor is the wait before retry number `failures` (1-based): the base
// doubled per failure, capped, then jittered to [step/2, step].
func (s *ReadinessRecomputeSubscriber) backoffFor(failures int) time.Duration {
	step := s.timings.RetryBase
	for i := 1; i < failures && step < s.timings.RetryMax; i++ {
		step *= 2
	}
	if step > s.timings.RetryMax || step <= 0 {
		step = s.timings.RetryMax
	}
	half := step / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// jitteredSafetyNet is the period +-ReadinessSafetyNetJitter.
func (s *ReadinessRecomputeSubscriber) jitteredSafetyNet() time.Duration {
	base := float64(s.timings.SafetyNet)
	spread := base * ReadinessSafetyNetJitter
	return time.Duration(base - spread + rand.Float64()*2*spread)
}

// drainPending clears the slot: whatever arrived is covered by the pass
// about to run.
func (s *ReadinessRecomputeSubscriber) drainPending() {
	select {
	case <-s.pending:
	default:
	}
}

// sleepUnless waits d or returns false when ctx ends first.
func sleepUnless(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *ReadinessRecomputeSubscriber) loop(ctx context.Context) {
	safety := time.NewTimer(s.jitteredSafetyNet())
	defer safety.Stop()
	failures := 0
	for {
		var reason string
		if failures > 0 {
			// IN BACKOFF: pending is deliberately NOT selected on, so a
			// notification collapses into the retry rather than running the
			// pass early against a database that just refused it.
			retry := time.NewTimer(s.backoffFor(failures))
			select {
			case <-ctx.Done():
				retry.Stop()
				return
			case <-retry.C:
				reason = "retry"
			case <-safety.C:
				retry.Stop()
				reason = "safetyNet"
				safety.Reset(s.jitteredSafetyNet())
			}
			s.drainPending()
		} else {
			select {
			case <-ctx.Done():
				return
			case reason = <-s.pending:
				// Wait out the window BEFORE writing, so a burst that is
				// still arriving is one rewrite rather than the first of many.
				if !sleepUnless(ctx, s.timings.Debounce) {
					return
				}
				s.drainPending()
			case <-safety.C:
				reason = "safetyNet"
				safety.Reset(s.jitteredSafetyNet())
			}
		}
		written, err := s.write(ctx)
		if err != nil {
			// A FAILURE LEAVES THE PREVIOUS ROWS STANDING, THE LOOP ALIVE,
			// AND A RETRY SCHEDULED. An unknown pass is a failure here too:
			// however many rows it wrote, it did not describe the cluster.
			failures++
			if s.logger != nil {
				s.logger.Warn("module readiness: rewrite failed; the previous rows stand and the pass will retry",
					"component", ComponentName, "reason", reason, "unknown", IsReadinessUnknown(err),
					"written", written, "failures", failures, "retryIn", s.backoffFor(failures).String(), "error", err)
			}
			continue
		}
		failures = 0
		if s.logger != nil {
			// SAID AT INFO, so a log shows the recovery and not only the
			// failure before it. "modules" is rows written this pass; D4 will
			// make it rows that CHANGED.
			s.logger.Info("module readiness: rows written",
				"component", ComponentName, "reason", reason, "modules", written)
		}
	}
}

// StartReadinessRecomputeSubscriber wires this node's readiness rewrites to the
// events that change them, and returns the subscriber so a caller can Notify it
// from a path that is not an event (the providers reload, the boot write).
//
// The subscription is scoped to ctx, exactly as StartProvidersReloadSubscriber
// is: when ctx is cancelled the unsubscribe runs and the loop exits.
func (e *MemQLEngine) StartReadinessRecomputeSubscriber(ctx context.Context) *ReadinessRecomputeSubscriber {
	if e == nil {
		return nil
	}
	sub := NewReadinessRecomputeSubscriber(e)
	sub.Start(ctx)
	// Stored BEFORE the subscriptions below, so a notification arriving from
	// another goroutine during startup finds the running subscriber rather
	// than a nil it silently drops.
	e.readinessRecompute.Store(sub)

	if e.eventBus == nil {
		return sub
	}
	for _, pattern := range readinessRegistrationPatterns() {
		unsubscribe := e.eventBus.Subscribe(
			pattern,
			func(events.Event) { sub.Notify("registration") },
			events.WithSubscriberName("readiness:recompute"),
		)
		if unsubscribe == nil {
			continue
		}
		go func() {
			<-ctx.Done()
			unsubscribe()
		}()
	}
	return sub
}

// NotifyReadinessRecompute asks this node to rewrite its readiness rows, on the
// debounce, from a path that is not a graph event.
//
// A no-op when the subscriber is not running, which is every hand-built engine
// in a test and every binary that has not called
// StartReadinessRecomputeSubscriber. The caller that needs the rewrite to have
// HAPPENED calls WriteModuleReadiness directly instead; this one is for the
// callers that only need it to happen soon.
//
// IT REPORTS WHETHER IT WAS DELIVERED, so a caller that must not simply lose
// the rewrite can fall back to a direct write (app/run.go's boot write does
// exactly that). Without the answer, "notify" and "silently dropped" are the
// same call.
func (e *MemQLEngine) NotifyReadinessRecompute(reason string) bool {
	if e == nil {
		return false
	}
	sub := e.readinessRecompute.Load()
	if sub == nil {
		return false
	}
	sub.Notify(reason)
	return true
}
```

- [ ] **Step 4: Run the subscriber suite to verify it passes**

Run: `cd /home/znas/projects/memql/memql && go vet ./component/memql/ && go test -count=1 -race ./component/memql/ -run 'Rewrite|SafetyNet|Backoff|Jitter|Notify|Subscriber|ProductionTimings|CdcTopics|LoggedWithReason|StoppingEndsTheLoop|GraphEventOnTheBus' -v`
Expected: PASS. (`TestAGraphEventOnTheBusReachesTheRewrite` assigns `sub.write`/`sub.timings` after `Start`, the pre-existing pattern; if `-race` reports it, move the two assignments to before `sub.Start` by constructing via `NewReadinessRecomputeSubscriber(eng)` + `sub.Start(ctx)` + `eng.readinessRecompute.Store(sub)` in the test instead of `StartReadinessRecomputeSubscriber`, and keep the bus subscription by calling `eng.StartReadinessRecomputeSubscriber` only after; do not weaken the assertion.)

- [ ] **Step 5: Hand the boot write to the subscriber, with jitter**

In `app/run.go`, replace lines 220-270 (from the `// MODULE READINESS` comment through the closing `}` of `if eng := application.Engine(); eng != nil {`) with:

```go
	// MODULE READINESS (design records 2026-09-06-configuration-readiness
	// section 6, and 2026-09-13-readiness-unknown-and-retry D2): evaluated
	// here, after every dependency is Ready and the plug-ins and providers
	// have materialized, under the same node identity the startup event
	// carries. Earlier would report an unconfigured module for every
	// integration that had not finished wiring up.
	//
	// THE WRITE GOES THROUGH THE RECOMPUTE SUBSCRIBER, which retries a pass
	// that failed or could not evaluate on a jittered backoff until one
	// lands. On 2026-09-13 every pod's boot write hit a saturated database
	// during the rollout (SQLSTATE 53300 and dial i/o timeouts); the direct
	// write here had no second chance, and nodes nobody dials (bff-znas,
	// edge) never receive the mesh event that would have prompted another.
	//
	// AND IT IS JITTERED 0-5 s, because every pod in a rollout reaches this
	// line at the same moment, on top of the seed materializer, the cluster
	// registration and provider resolution -- the peak of the surge.
	//
	// A failure never stops the node. A cluster that will not start because
	// it could not describe its own configuration is a worse outcome than a
	// stale verdict.
	//
	// AND ONE RE-WRITE THIRTY SECONDS LATER (epic memql#5118, D5). It covers a
	// node whose integration materialized LAZILY, after the first write ran,
	// and there is no event for "an integration finished resolving". It goes
	// through the same loop, so it too is retried on failure.
	bootRewrite, cancelBootRewrite := context.WithCancel(context.Background())
	defer cancelBootRewrite()
	if eng := application.Engine(); eng != nil {
		eng.SetReadinessIdentity(application.startupNodeID(), application.startupNodeType())
		// writeOrQueue notifies the subscriber where there is one and writes
		// directly where there is not (a binary with no event bus), because a
		// write that quietly went nowhere is the failure this exists to prevent.
		writeOrQueue := func(reason string) {
			if eng.NotifyReadinessRecompute(reason) {
				cfg.Logger.Info("module readiness: write queued", "reason", reason)
				return
			}
			if n, err := eng.WriteModuleReadiness(bootRewrite); err != nil {
				cfg.Logger.Warn("module readiness: direct write failed; the previous rows stand", "reason", reason, "error", err)
			} else {
				cfg.Logger.Info("module readiness: rows written", "reason", reason, "modules", n)
			}
		}
		go func() {
			jitter := time.NewTimer(memql.ReadinessBootJitter())
			defer jitter.Stop()
			select {
			case <-bootRewrite.Done():
				return
			case <-jitter.C:
			}
			writeOrQueue("boot")

			timer := time.NewTimer(memql.ReadinessBootRewriteDelay)
			defer timer.Stop()
			select {
			case <-bootRewrite.Done():
				return
			case <-timer.C:
			}
			writeOrQueue("bootRewrite")
		}()
	}
```

Run: `cd /home/znas/projects/memql/memql && go vet ./app/ && go build -o bin/memql . && go build -tags edge -o /dev/null . && go test -count=1 ./app/ -run TestNodeLifecycle -v`
Expected: builds clean for the bff and edge tags; the lifecycle tests pass.

- [ ] **Step 6: One sentence each on what a broadcast rule reaches**

In `component/node/routing.go`, replace the comment at lines 216-219 with:

```go
		// MODULE READINESS (design record 2026-09-06-configuration-readiness,
		// section 4.4). Every node writes its own verdict rows; the OS folds
		// them from a live feed on whichever replica it is attached to. No
		// delete rule: rows are rewritten as versions, never removed.
		//
		// A BROADCAST RULE REACHES THE NODES THAT DIAL THE WRITER OR ITS
		// RELAYS, NOT EVERY NODE (2026-09-13): the transport pushes only along
		// outbound dials, so a node nobody dials hears none of these. The
		// readiness subscriber carries its own retry and safety net for that
		// reason (component/memql/readiness_recompute_subscriber.go).
```

In `component/node/CLAUDE.md`, after the paragraph ending "...with fallback to direct publish if the channel is full." (line 201), add:

```markdown
**A broadcast rule reaches the nodes that dial the writer or its relays, not
every node.** Forwarding follows `sendTarget`, which is set only by the
OUTBOUND dial path (`AttachConnection`); inbound peers and children get no
push, and nothing constructs a server->client `EventForward`. In the cloud,
`bff-znas`, `edge` and `mcp` are dialed by nobody and receive no mesh event.
A consumer that must eventually be right on every node needs its own floor
(the readiness subscriber's retry and safety net, 2026-09-13 D2).
```

- [ ] **Step 7: Commit**

```bash
cd /home/znas/projects/memql/memql
git add component/memql/readiness_recompute_subscriber.go component/memql/readiness_recompute_subscriber_test.go app/run.go component/node/routing.go component/node/CLAUDE.md
git commit -m "feat(readiness): the rewrite retries with jittered backoff, runs a safety-net pass, and logs success; boot goes through it

A failed or unknown pass re-runs at 2 s doubling to 60 s until one lands;
a Notify during the backoff collapses into the retry; a pass every ten
minutes (+-20%) covers nodes nobody dials, which receive no mesh events.
Success is logged at INFO with the reason and count. app/run.go queues
the boot write on the subscriber after a 0-5 s jitter instead of writing
once and hoping. The NOT A TIMER section is rewritten to state what a
broadcast actually reaches.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 6: The OS mirror -- `unknown` in the fold, `unknown` as the default, tests

**Files:**
- Modify: `clients/os/src/system/readinessFold.ts:12-17` (state union), `:33-41` (NodeReport), `:56-69` (Verdict), `:92-157` (fold)
- Modify: `clients/os/src/live/readiness.tsx:44-57` (`reportFromRow`, exported)
- Modify: `clients/os/test/system/readinessFold.test.ts:19-45`
- Create: `clients/os/test/live/readinessRows.test.ts`

**Interfaces:**
- Consumes: fixture 10 (Task 1); the row shape written by Task 3 (`state: "unknown"`, `reason`).
- Produces: `ReadinessState` includes `"unknown"`; `NodeReport.reason?: string`; `Verdict.unknown: string[]`; `foldReadiness` excludes unknown rows from the vote and lists their node ids sorted; `export function reportFromRow(row: Row): NodeReport | null` defaults a missing state to `"unknown"`.

- [ ] **Step 1: Write the failing tests**

In `clients/os/test/system/readinessFold.test.ts`, replace lines 19-45 (the `Fixture` interface through the fixture loop) with:

```ts
interface Fixture {
  name: string;
  now: string;
  reports: NodeReport[];
  nodes: NodeLiveness[];
  expect: { module: string; state: string; disagreement: string[]; unknown?: string[] }[];
}

describe("the fold mirrors component/memql/readiness", () => {
  const files = readdirSync(FIXTURES)
    .filter((f) => f.endsWith(".json"))
    .sort();

  // Without this, a mistyped path reads as a suite with no cases and passes.
  it("finds the shared fixtures", () => {
    expect(files.length).toBeGreaterThanOrEqual(10);
  });

  for (const file of files) {
    const fx = JSON.parse(readFileSync(resolve(FIXTURES, file), "utf8")) as Fixture;
    it(fx.name, () => {
      const got = foldReadiness(fx.reports, fx.nodes, new Date(fx.now));
      expect(
        got.map((v) => ({
          module: v.module,
          state: v.state,
          disagreement: v.disagreement,
          unknown: v.unknown,
        })),
      ).toEqual(fx.expect.map((e) => ({ ...e, unknown: e.unknown ?? [] })));
    });
  }
```

Create `clients/os/test/live/readinessRows.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { reportFromRow } from "../../src/live/readiness";

// A ROW WITH NO STATE IS UNKNOWN, NEVER UNCONFIGURED (D1, 2026-09-13). The
// old default turned a malformed or partial row into a "not set up" vote,
// which is the one direction a default must never take: it sends somebody
// to a setup form over a row that said nothing.
describe("reportFromRow", () => {
  const base = {
    id: "v1:platform:moduleReadiness:ai--bff-a",
    module: "ai",
    nodeId: "bff-a",
    nodeType: "bff",
    core: true,
    lanes: [],
    reportedAt: "2026-09-13T11:58:00Z",
  };

  it("defaults a missing state to unknown", () => {
    const r = reportFromRow(base as unknown as Row);
    expect(r?.state).toBe("unknown");
    expect(r?.reason).toBeUndefined();
  });

  it("carries the reason of an unknown row", () => {
    const r = reportFromRow({ ...base, state: "unknown", reason: "fleetReadFailed" } as unknown as Row);
    expect(r?.state).toBe("unknown");
    expect(r?.reason).toBe("fleetReadFailed");
  });

  it("keeps a stated verdict as it is", () => {
    expect(reportFromRow({ ...base, state: "configured" } as unknown as Row)?.state).toBe("configured");
  });

  it("drops a row with no module", () => {
    expect(reportFromRow({ ...base, module: "" } as unknown as Row)).toBeNull();
  });
});
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd /home/znas/projects/memql/memql/clients/os && npx vitest run test/system/readinessFold.test.ts test/live/readinessRows.test.ts`
Expected: FAIL -- the fixture-10 case (`unknown` folds as a state: storage becomes `partial`, `unknown` is `undefined`), and `reportFromRow` is not exported / defaults to `unconfigured`.

- [ ] **Step 3: Mirror the fold and change the default**

In `clients/os/src/system/readinessFold.ts`, replace the state union (lines 12-17) with:

```ts
export type ReadinessState =
  | "configured"
  | "partial"
  | "unconfigured"
  | "notApplicable"
  | "unreported"
  /**
   * A node's word for "I could not evaluate this" (D1, 2026-09-13). Never a
   * vote: the fold sets it aside and names the node in Verdict.unknown.
   */
  | "unknown";
```

Replace `NodeReport` (lines 33-41) with:

```ts
export interface NodeReport {
  module: string;
  nodeId: string;
  nodeType: string;
  state: ReadinessState;
  /** Set only when state is "unknown": fleetReadFailed | integrationProbeFailed. */
  reason?: string;
  core?: boolean;
  lanes?: LaneReport[];
  reportedAt: string;
}
```

In `Verdict` (lines 56-69), add after `nodes: NodeVerdict[];`:

```ts
  /**
   * Every LIVE node whose row says it could not evaluate, sorted. Those rows
   * are neither in nodes nor in disagreement; they cast no vote.
   */
  unknown: string[];
```

Replace the `foldReadiness` doc comment and body (lines 92-157) with:

```ts
/**
 * Every node's report to one verdict per module:
 *
 *  1. Keep reports from live nodes only, so a dead replica's stale row cannot
 *     pin a verdict.
 *  2. Drop notApplicable.
 *  3. Set aside unknown: a node that could not evaluate is named in
 *     `unknown` and casts no vote.
 *  4. Worst state wins.
 *  5. If the kept reports disagree, the verdict is partial and disagreement
 *     names every live reporter as nodeId=state, worst first.
 *  6. A module with no kept report is unreported -- never unconfigured, and
 *     that includes a module whose every live row is unknown. Not knowing
 *     and not being set up are different answers, and only one of them
 *     should send somebody to a form.
 *
 * Modules come back in name order, so two folds over the same rows are equal.
 */
export function foldReadiness(reports: NodeReport[], nodes: NodeLiveness[], now: Date): Verdict[] {
  const live = new Set(nodes.filter((n) => nodeIsLive(n, now)).map((n) => n.nodeId));
  const kept = new Map<string, NodeReport[]>();
  const unknown = new Map<string, string[]>();
  const core = new Map<string, boolean>();
  const order: string[] = [];
  for (const r of reports) {
    if (!core.has(r.module)) {
      order.push(r.module);
      core.set(r.module, false);
    }
    if (r.core) core.set(r.module, true);
    if (!live.has(r.nodeId) || r.state === "notApplicable") continue;
    if (r.state === "unknown") {
      const ids = unknown.get(r.module) ?? [];
      ids.push(r.nodeId);
      unknown.set(r.module, ids);
      continue;
    }
    const list = kept.get(r.module) ?? [];
    list.push(r);
    kept.set(r.module, list);
  }
  order.sort();
  const out: Verdict[] = [];
  for (const module of order) {
    const unknownIds = [...(unknown.get(module) ?? [])].sort();
    const rs = kept.get(module) ?? [];
    if (rs.length === 0) {
      out.push({
        module,
        state: "unreported",
        core: core.get(module) ?? false,
        disagreement: [],
        nodes: [],
        unknown: unknownIds,
        lanes: [],
      });
      continue;
    }
    rs.sort(
      (a, b) => rank(b.state) - rank(a.state) || (a.nodeId < b.nodeId ? -1 : a.nodeId > b.nodeId ? 1 : 0),
    );
    const states = new Set(rs.map((r) => r.state));
    out.push({
      module,
      state: states.size > 1 ? "partial" : rs[0]!.state,
      core: core.get(module) ?? false,
      disagreement: states.size > 1 ? rs.map((r) => `${r.nodeId}=${r.state}`) : [],
      nodes: rs.map((r) => ({
        nodeId: r.nodeId,
        nodeType: r.nodeType,
        state: r.state,
        reportedAt: r.reportedAt,
      })),
      unknown: unknownIds,
      lanes: rs[0]!.lanes ?? [],
    });
  }
  return out;
}
```

In `clients/os/src/live/readiness.tsx`, replace `reportFromRow` (lines 44-57) with:

```tsx
/**
 * One feed row to a report. A MISSING STATE IS "unknown", never
 * "unconfigured" (D1, 2026-09-13): the fold sets unknown aside, while
 * unconfigured is a vote that holds the core gate and sends a person to a
 * form. Exported for its own test; the feed is the only production caller.
 */
export function reportFromRow(row: Row): NodeReport | null {
  const r = row as Record<string, unknown>;
  const module = typeof r.module === "string" ? r.module : "";
  if (module === "") return null;
  const reason = typeof r.reason === "string" && r.reason !== "" ? r.reason : undefined;
  return {
    module,
    nodeId: String(r.nodeId ?? ""),
    nodeType: String(r.nodeType ?? ""),
    state: String(r.state ?? "unknown") as NodeReport["state"],
    ...(reason !== undefined ? { reason } : {}),
    core: r.core === true,
    lanes: Array.isArray(r.lanes) ? (r.lanes as NodeReport["lanes"]) : [],
    reportedAt: String(r.reportedAt ?? ""),
  };
}
```

- [ ] **Step 4: Run the OS suites and the Go parity gate**

Run: `cd /home/znas/projects/memql/memql/clients/os && npm run typecheck && npx vitest run test/system/readinessFold.test.ts test/live/readinessRows.test.ts test/system/readinessReactivity.test.tsx test/setup test/chrome`
Expected: PASS -- all ten fixtures on the TS side, the four `reportFromRow` cases, and the gate/stops suites unchanged (an `unknown` row never reaches them as a verdict state).

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/memql/readiness/ -run 'TestNodeLiveWindowMatchesTheClient|TestFoldFixtures' -v`
Expected: PASS (the regexp gate still finds the numeric literal).

- [ ] **Step 5: Commit**

```bash
cd /home/znas/projects/memql/memql
git add clients/os/src/system/readinessFold.ts clients/os/src/live/readiness.tsx clients/os/test/system/readinessFold.test.ts clients/os/test/live/readinessRows.test.ts
git commit -m "feat(os): the readiness fold mirrors unknown -- set aside, named per node; a missing state defaults to unknown

Fixture 10 now passes on both sides. reportFromRow's default was
unconfigured, which turned a row that said nothing into a vote that
holds the core gate.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 7: The connector retries a dial i/o timeout too, inside a ~15 s budget

**Files:**
- Modify: `component/database/conn_retry.go` (whole file)
- Modify: `component/database/conn_retry_test.go` (extend)
- Modify: `component/database/database.go:344-348` (comment only)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `isRetryableConnectError(err error) bool` (53300 OR a dial/handshake timeout); `retryingConnector` gains `budget time.Duration`; `defaultConnRetryBudget = 15 * time.Second`, `defaultConnRetryAttempts = 12`; `Connect` stops at whichever comes first: the attempt cap, the budget, or the context. `newRetryingConnector(base, logger)` signature unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `component/database/conn_retry_test.go`:

```go
// timeoutErr is what net returns for a dial that ran out of time: a
// net.Error whose Timeout() is true. pgdriver wraps it, so the string form
// is matched too.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.0.185.57:5432: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsRetryableConnectError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"53300", errors.New("FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute (SQLSTATE=53300)"), true},
		{"net timeout", timeoutErr{}, true},
		{"wrapped net timeout", fmt.Errorf("connect: %w", timeoutErr{}), true},
		{"timeout by text", errors.New("read tcp 10.244.0.38:38472->10.0.185.57:5432: i/o timeout"), true},
		{"auth", errors.New("FATAL: password authentication failed"), false},
		{"refused", errors.New("dial tcp 10.0.185.57:5432: connect: connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryableConnectError(c.err); got != c.want {
				t.Errorf("isRetryableConnectError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A DIAL TIMEOUT IS RETRIED (D7, 2026-09-13). On 2026-09-13 bff-znas's boot
// readiness write failed on "read tcp ...: i/o timeout" against the -rw
// ClusterIP while old and new pods overlapped; the connector retried only
// 53300, so the timeout surfaced as a failed query on the first try.
func TestRetryingConnector_RetriesDialTimeout(t *testing.T) {
	fc := &fakeConnector{failUntil: 2, err: timeoutErr{}}
	rc := newTestRetryConnector(fc)
	if _, err := rc.Connect(context.Background()); err != nil {
		t.Fatalf("expected recovery after a dial timeout cleared, got %v", err)
	}
	if fc.calls != 3 {
		t.Errorf("expected 3 Connect calls (2 timeouts + 1 success), got %d", fc.calls)
	}
}

// THE BUDGET IS BOUNDED. A server that stays saturated must not be retried
// forever; the wall-clock budget ends the ladder even when the attempt cap
// has not been reached.
func TestRetryingConnector_BudgetIsBounded(t *testing.T) {
	fc := &fakeConnector{failUntil: 1000, err: errors.New("SQLSTATE 53300")}
	rc := &retryingConnector{base: fc, attempts: 1000, baseWait: 2 * time.Millisecond, maxWait: 5 * time.Millisecond, budget: 40 * time.Millisecond}
	started := time.Now()
	_, err := rc.Connect(context.Background())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected the last error after the budget elapsed")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the budget did not bound the retry: %s elapsed", elapsed)
	}
	if fc.calls >= 1000 || fc.calls < 2 {
		t.Fatalf("calls=%d; the budget, not the attempt cap, should have ended the ladder after a few tries", fc.calls)
	}
}

// The production budget is what the record says: ~15 s, wide enough to
// cover a rollout's overlap window, narrow enough that a request path with
// its own deadline is bounded by that deadline first.
func TestTheProductionConnectBudgetIsWhatTheRecordSays(t *testing.T) {
	rc, ok := newRetryingConnector(&fakeConnector{}, nil).(*retryingConnector)
	if !ok {
		t.Fatal("newRetryingConnector did not return a *retryingConnector")
	}
	if rc.budget != 15*time.Second {
		t.Errorf("budget %s, want 15s", rc.budget)
	}
	if rc.maxWait != 2*time.Second || rc.baseWait != 100*time.Millisecond {
		t.Errorf("waits %s..%s, want 100ms..2s", rc.baseWait, rc.maxWait)
	}
	// Enough attempts that the budget, not the cap, is the binding limit:
	// 100ms, 200, 400, 800, 1.6s, then 2s x 7 = ~17s of ceilings > 15s.
	if rc.attempts < 12 {
		t.Errorf("attempts %d; fewer than 12 lets the cap end the ladder before the budget", rc.attempts)
	}
}
```

Add `"fmt"` to the imports, and change `newTestRetryConnector` to include a budget so the existing cases keep the attempt cap as their binding limit:

```go
func newTestRetryConnector(base driver.Connector) *retryingConnector {
	return &retryingConnector{base: base, attempts: 5, baseWait: time.Millisecond, maxWait: 5 * time.Millisecond, budget: time.Second}
}
```

and in `TestRetryingConnector_RespectsContext` add `budget: time.Minute` to the literal.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd /home/znas/projects/memql/memql && go test -count=1 ./component/database/ -run 'TestIsRetryableConnectError|TestRetryingConnector|TestTheProductionConnectBudget' -v`
Expected: compile FAIL -- `undefined: isRetryableConnectError`, `unknown field budget`.

- [ ] **Step 3: Rewrite the connector**

Replace `component/database/conn_retry.go` in full:

```go
package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"time"
)

// conn_retry.go makes physical connection establishment resilient to two
// transient failures a rolling deploy produces (znasllc-io/memql#1076, and
// design record 2026-09-13-readiness-unknown-and-retry D7):
//
//   - SQLSTATE 53300 ("remaining connection slots are reserved ..." / "too
//     many clients already"), raised by the SERVER at Connect() when the
//     cluster's pods collectively demand more than max_connections. The
//     deployed Postgres is ONE CNPG instance at max_connections=200 (the
//     conn-monitor's budget is 183); a rolling deploy overlaps 16 old pods
//     with up to 18 new ones at up to 8 connections each, which is how the
//     ceiling is reached for tens of seconds.
//   - A dial or handshake I/O TIMEOUT, which is what the same window looks
//     like from a pod that never got a slot at all.
//
// Both are transient: a slot frees as old pods drain. database/sql does NOT
// retry Connect on its own, so a single rejected open surfaces as a failed
// query (a dropped seed materialization, health write, readiness write ...).
// Wrapping the driver.Connector so Connect() retries inside a BOUNDED budget
// turns those into a brief wait instead of a hard failure.
//
// THE BUDGET IS WALL-CLOCK, ~15 s, and a context deadline always wins. Boot
// paths call with no deadline and get the whole budget, which is what covers
// the overlap window; a request path carries its own shorter deadline and
// is bounded by it first. The ladder deliberately does NOT retry forever --
// that would pile pressure onto an exhausted server.
//
// This is defense-in-depth: the primary mitigation is right-sizing the pool
// (MAX_OPEN_CONNS) so steady+surge demand stays under max_connections.

const (
	defaultConnRetryAttempts = 12
	defaultConnRetryBase     = 100 * time.Millisecond
	defaultConnRetryMax      = 2 * time.Second
	defaultConnRetryBudget   = 15 * time.Second
)

// isConnSlotExhaustion reports whether err is a Postgres connection-slot
// exhaustion (SQLSTATE 53300). Matched on the error text so it works across
// driver error types (bun's pgdriver surfaces the SQLSTATE + message).
func isConnSlotExhaustion(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "53300") ||
		strings.Contains(s, "too many clients already") ||
		strings.Contains(s, "remaining connection slots are reserved")
}

// isConnectTimeout reports whether err is a dial or handshake that ran out
// of time: a net.Error with Timeout(), or the text form pgdriver wraps it in.
// "connection refused" is NOT one: that is a server that is not there, and
// waiting on it is not a repair.
func isConnectTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// isRetryableConnectError is the union the ladder retries.
func isRetryableConnectError(err error) bool {
	return isConnSlotExhaustion(err) || isConnectTimeout(err)
}

// retryingConnector wraps a driver.Connector and retries Connect() on a
// transient failure (53300, dial timeout) inside a bounded budget. All other
// errors pass through unchanged.
type retryingConnector struct {
	base     driver.Connector
	logger   *slog.Logger
	attempts int
	baseWait time.Duration
	maxWait  time.Duration
	// budget is the wall-clock ceiling over every attempt and wait; a
	// context deadline that comes first wins.
	budget time.Duration
}

// newRetryingConnector wraps base so Connect() retries transient failures. A
// nil base returns nil (caller falls back to the unwrapped connector).
func newRetryingConnector(base driver.Connector, logger *slog.Logger) driver.Connector {
	if base == nil {
		return nil
	}
	return &retryingConnector{
		base:     base,
		logger:   logger,
		attempts: defaultConnRetryAttempts,
		baseWait: defaultConnRetryBase,
		maxWait:  defaultConnRetryMax,
		budget:   defaultConnRetryBudget,
	}
}

func (r *retryingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	deadline := time.Now().Add(r.budget)
	var lastErr error
	for attempt := 0; attempt <= r.attempts; attempt++ {
		conn, err := r.base.Connect(ctx)
		if err == nil {
			if attempt > 0 && r.logger != nil {
				r.logger.Warn("db connect recovered after a transient failure",
					"component", "memoryNodes", "attempt", attempt+1)
			}
			return conn, nil
		}
		if !isRetryableConnectError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == r.attempts {
			break
		}
		wait := backoffWithJitter(r.baseWait, r.maxWait, attempt)
		if time.Now().Add(wait).After(deadline) {
			if r.logger != nil {
				r.logger.Warn("db connect retry budget exhausted",
					"component", "memoryNodes", "attempt", attempt+1, "budget", r.budget.String(), "error", err)
			}
			break
		}
		if r.logger != nil {
			r.logger.Warn("db connect hit a transient failure; retrying",
				"component", "memoryNodes", "attempt", attempt+1, "backoff", wait.String(),
				"slotExhaustion", isConnSlotExhaustion(err), "timeout", isConnectTimeout(err))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func (r *retryingConnector) Driver() driver.Driver { return r.base.Driver() }

// backoffWithJitter returns an exponential backoff (base * 2^attempt) capped at
// maxWait, with full jitter to avoid a thundering herd of pods all retrying in
// lockstep (which would re-exhaust the server the instant a slot frees).
func backoffWithJitter(base, maxWait time.Duration, attempt int) time.Duration {
	d := base << attempt
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	return time.Duration(rand.Int63n(int64(d)) + 1)
}
```

In `component/database/database.go`, replace the three-line comment above `sql.OpenDB(newRetryingConnector(...))` (lines 344-347) with:

```go
			// Wrap the connector so Connect() retries transient Postgres
			// failures -- connection-slot exhaustion (SQLSTATE 53300) and a
			// dial i/o timeout -- with jittered backoff inside a ~15 s
			// budget, instead of failing the query outright (memql#1076;
			// 2026-09-13 D7).
```

- [ ] **Step 4: Run the database package tests**

Run: `cd /home/znas/projects/memql/memql && go vet ./component/database/ && go test -count=1 ./component/database/ -run 'TestIsConnSlotExhaustion|TestIsRetryableConnectError|TestRetryingConnector|TestTheProductionConnectBudget' -v`
Expected: PASS, including the four pre-existing cases.

- [ ] **Step 5: Commit**

```bash
cd /home/znas/projects/memql/memql
git add component/database/conn_retry.go component/database/conn_retry_test.go component/database/database.go
git commit -m "fix(database): retry a dial i/o timeout at Connect as well as 53300, inside a 15 s budget

A rolling deploy overlaps 16 old with up to 18 new pods against one CNPG
instance at max_connections=200; from a pod that never got a slot the
window looks like a dial timeout, which the connector passed through on
the first try. Both classes now retry with full jitter until a wall-clock
budget (15 s, a context deadline wins) or the attempt cap ends it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 8: The design record, the amendment to D5 of 2026-09-07, and the public fold rule

**Files:**
- Create: `docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md`
- Modify: `docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md:166-178` (an AMENDED note under D5, in the style of the 2026-09-06 record's section 4.5 note)
- Modify: `docs/public/operate/configuration-readiness.md:52-66` (the numbered fold rules)

**Interfaces:**
- Consumes: every name introduced above, cited exactly: `readiness.Unknown`, `Reason*`, `Verdict.Unknown`, `persistReadiness`, `readinessPersistInput`, `ReadinessUnknownError`, `ReadinessRetryBase/Max`, `ReadinessSafetyNetInterval`, `ReadinessBootJitterMax`, `isRetryableConnectError`, `defaultConnRetryBudget`.
- Produces: the record the concept doc comment (Task 3) and the subscriber header (Task 5) already cite by filename.

- [ ] **Step 1: Write the design record**

Create `docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md`:

```markdown
# Readiness: a failed evaluation is unknown, and the rewrite retries -- Design

- **Date:** 2026-09-13
- **Status:** approved by the owner on 2026-09-13 from the production incident
  below. D1, D2 and D7 ship in this record's PR; D3-D6 are recorded here as the
  next plan's rulings and are NOT implemented by it.
- **Owner areas:** `component/memql` (evaluator, writer, recompute subscriber),
  `component/memql/readiness` and `clients/os/src/system/readinessFold.ts` (the
  fold and its mirror), `dsl/platform` (the concept), `component/database` (the
  connector), `app/run.go` (boot).
- **Depends on:** 2026-09-06-configuration-readiness (the rows and the fold),
  2026-09-07-core-gate-and-honest-install D3 and D5 (inference from rows;
  event-driven recompute). Amends D5.

---

## 1. Problem

On 2026-09-13, after a rolling deploy of engine 0.21.24, the desk's Set up card on
the owner's cluster read `Inference: Partly set up` while inference worked and the
Cluster app's Readiness page said the fleet met the floor. Modules > Components showed
the disagreement: the two `bff-znas` pods and the two `edge` pods reported `ai`
`unconfigured`; agent, bff, planner and workbench reported `configured`.

The pod logs showed the same three lines on every node type: the `ai` evaluator's
registration read failed (`read tcp ...: i/o timeout` on some pods, `FATAL: remaining
connection slots are reserved ... (SQLSTATE=53300)` on others) while the database was
saturated by the rollout; the evaluator mapped the failed read to `unconfigured`; and
`WriteModuleReadiness` persisted it. The 30 s boot re-write failed the same way. The
nodes that later read `configured` recovered because a registration heartbeat event
reached them and triggered a rewrite once the database had settled; `bff-znas` and
`edge` never recovered because no mesh event ever reaches them (section 3, D6).

Four defects, in order of blame:

1. `evaluateModule` reported a FAILED read as `unconfigured`, and the write persisted
   it. "Could not ask" and "no door" were the same row. The test suite pinned this
   (`TestAFailedRegistrationReadShutsEveryDoor`). The integration arm did the same
   with `notApplicable` on a failed probe.
2. Nothing retried. One boot write, one re-write 30 s later, then only events. The
   subscriber survived a failure and waited for an event that, on a node nobody
   dials, never comes.
3. The database: one CNPG instance, `max_connections=200` (the conn-monitor's budget
   is 183), deployed from the `memql-znas` repo's overlay rather than the 400 /
   3-instance `top` preset this repo's cloud overlay declares. A rolling deploy
   overlaps 16 old pods with up to 18 new ones at up to 8 connections each, which
   exceeds 200 for tens of seconds. The connector retried 53300 for ~3 s and never
   retried a dial timeout.
4. `ai` is a cluster fact (2026-09-07 D3) stored per node and folded with "any
   disagreement is partial". Disagreement on `ai` can only ever be rollout skew, a
   failed evaluation, or a stale row on an event-island node -- none of which is
   "partly set up". Deferred (D3).

## 2. What the tree already has

- The fold (`component/memql/readiness.Fold`, mirrored in `readinessFold.ts`, held equal
  by the fixtures under `component/memql/readiness/testdata/fold/`): live nodes only,
  drop `notApplicable`, worst wins, disagreement is `partial`, nothing is `unreported`.
- The OS already keeps the distinction this record adds, for passkeys: `stops.ts`,
  "a failed read is `unknown`, never `none`".
- The recompute subscriber (`readiness_recompute_subscriber.go`): one loop, debounced
  2 s, notified by the registration broadcast, the providers reload and the email
  configure; a failure is logged and the loop waits for the next event.
- The connector (`component/database/conn_retry.go`): retries SQLSTATE 53300 at
  Connect, 5 attempts, 100 ms doubling to a 2 s cap (~3 s total); nothing else.

## 3. Decisions

### D1 -- A failed evaluation never persists a verdict

Chosen over (a) keeping the fail-closed row, and (b) writing `unconfigured` with a
reason field. Fail-closed is the right direction for the GATE and the wrong one for a
PERSISTED row: the row outlives the failure and is folded against other nodes'
correct rows. "Could not ask" and "no door" are different answers.

- `evaluateModule` returns `State: readiness.Unknown` with `Reason` from a CLOSED
  vocabulary (`readiness.ReasonFleetReadFailed`, `readiness.ReasonIntegrationProbeFailed`)
  when a resolver the verdict depends on errored: the `ai` arm on a failed
  registration read (no lanes), the integration arm on a failed probe. An integration
  that is not registered stays `notApplicable`. The error string stays in the WARN
  line; a row is broadcast and never carries it.
- The fold sets `unknown` aside: it casts no vote, the node is named in
  `Verdict.Unknown` (sorted, never null), and a module whose every live row is unknown
  is `unreported` -- which opens the core gate rather than holding it on a read that
  broke (2026-09-07, section 5).
- `WriteModuleReadiness` persists `unknown` ONLY when this process has never written
  a known verdict for that module (`readinessMemory`, in-process, per module), so a
  fresh pod can say "could not evaluate: fleet read failed" and a pod that already
  knew keeps what it knew. The rule is a pure function over a struct,
  `persistReadiness(readinessPersistInput{State, KnownBefore})`; D3 adds `Scope` to
  that struct and one clause ("a cluster-scoped module never writes unknown").
- A pass with any unknown module returns `*ReadinessUnknownError` (rows for the known
  modules were still written) so the caller retries (D2).
- The concept enum gains `unknown` and the concept gains `reason`, both ADDITIVE:
  dropping an enum value bricks stored rows (root CLAUDE.md). The OS default for a
  row with no state becomes `unknown`.

### D2 -- Retry with backoff until one success, and a low-frequency safety-net pass

Chosen over "events only" (2026-09-07 D5) because D5's premise -- a broadcast reaches
every replica -- is false for any node nobody dials (D6), and over a fast poll, the
cost D5 rightly refused.

- The subscriber loop retries a failed or unknown pass on an exponential, jittered
  backoff: `ReadinessRetryBase` 2 s doubling to `ReadinessRetryMax` 60 s, each step
  jittered to [step/2, step], until one pass lands. A `Notify` during the backoff
  collapses into the retry (the retry is a full re-evaluation). One loop, serialized.
- A safety-net pass every `ReadinessSafetyNetInterval` (10 min, jittered
  `ReadinessSafetyNetJitter` = +-20%). It re-evaluates and writes; write-only-if-changed
  is D4. One cluster read per node per ten minutes, two orders of magnitude below the
  heartbeat-driven rewrites the dialed nodes already perform.
- A successful pass logs at INFO (`module readiness: rows written`, `reason`,
  `modules`), so a log shows the recovery and not only the failure.
- Boot: `app/run.go` sleeps `ReadinessBootJitter()` (0-5 s, `ReadinessBootJitterMax`)
  and hands the write to the subscriber (`NotifyReadinessRecompute("boot")`); the direct
  write remains only for a binary with no subscriber. The 30 s re-write goes through
  the same loop.
- The subscriber's "NOT A TIMER" section is rewritten to state the delivery fact and
  the two mechanisms; `component/node/routing.go` and `component/node/CLAUDE.md` each
  gain one sentence: a broadcast rule reaches the nodes that dial the writer or its
  relays, not every node.

### D3 -- A cluster-scoped module is one cluster row (DEFERRED to the next plan)

Chosen over per-node rows with an "any successful configured wins" fold rule. The
manifest declares `scope: cluster | node` per module; `ai` is `cluster`; a
cluster-scoped module has ONE logical row (`moduleReadiness:ai--cluster`, `nodeId`
`cluster`, plus `evaluatedBy`), refreshed by any node from a SUCCESSFUL evaluation
only; the fold takes the one row and never produces `partial` from disagreement for
it. D1's `readinessPersistInput` is where the scope clause lands.

### D4 -- Rows are written on change, and a stopped node's rows are deleted (DEFERRED)

`WriteModuleReadiness` skips an unchanged verdict unless the standing row is older
than twice the safety-net period; a `@serverOnly` `deleteModuleReadinessForNode`
runs from the reconciler's stopped transition and the prune cron; a
`graph.node.deleted.v1:platform:moduleReadiness` routing rule; one concept-scoped
purge migration.

### D5 -- The card says why; "Partly set up" is reserved for a real partial lane (DEFERRED)

`stops.ts` names the nodes for a node-scoped disagreement and appends "(N nodes could
not evaluate)" from `Verdict.unknown`; `CoreGate` shows the reason under the headline.

### D6 -- The mesh island is recorded, not fixed here (DEFERRED)

`EventBridge.forwardToPeers` sends only where `sendTarget` returns a connection, which
`AttachConnection` (the outbound dial path) alone sets; nothing constructs a
server->client `EventForward`. `bff-znas`, `edge` and `mcp` are dialed by nobody and
receive no mesh event -- registration, providers reload, or the site-cache
invalidation. A separate issue and a named, skipped gate
(`TestABroadcastReachesAnUndialedNode`) record it; D2's safety net makes readiness
correct without it.

### D7 -- Pool sizing and rollout

Chosen over raising `MAX_OPEN_CONNS`. Keep 4/2. The connector retries a dial or
handshake i/o timeout as well as 53300 (`isRetryableConnectError`), with full jitter,
inside a wall-clock budget `defaultConnRetryBudget` = 15 s that a context deadline
always beats: a boot path with no deadline gets the whole budget, a request path is
bounded by its own. The boot readiness write moves behind the subscriber with jitter
(D2), so the burst is spread. Operationally: verify the live `max_connections` and the
conn-monitor's leak report; if the primary rolled in the same window, sequence the
database reconcile away from the application rollout (ArgoCD sync waves).

## 4. Failure modes

- Boot evaluation fails on a fresh pod: `unknown` rows with a reason are written for
  the modules that could not evaluate, the known ones write normally, the pass returns
  `ReadinessUnknownError`, the subscriber retries at 2 s, 4 s, 8 s ... The fold names
  the pod in `unknown` and its verdict is decided by the other nodes' rows.
- Every node fails at once (the database is down): every `ai` row is `unknown` or
  stale-known. Stale-known rows stand (never overwritten by unknown); if there is no
  known row at all the verdict is `unreported` and the gate opens. The first
  successful pass on any node writes a known row.
- A node nobody dials boots fine and a change happens elsewhere: it re-evaluates
  within one safety-net period (10 min +-20%). D3 removes the need for its `ai` row
  to move at all.
- The retry ladder never succeeds: it keeps retrying at the 60 s cap, one cluster read
  per minute per failing node, and every attempt logs at WARN with `failures` and
  `retryIn`.

## 5. Testing

- `readiness`: fixture `10-unknown-is-not-a-vote.json` on both sides;
  `TestAnUnknownRowNeverPinsAVerdict`.
- Evaluator: `TestAFailedRegistrationReadIsUnknownNotUnconfigured` (inverted from the
  shipped test), `TestIntegrationEvaluatorErrorIsUnknown`, the WARN line kept.
- Writer: `TestAnUnknownReportIsNotPersistedOverAKnownRow`,
  `TestAFreshNodeWritesUnknownWithAReason`, `TestKnownModulesStillWriteBesideAnUnknownOne`,
  `TestPersistReadinessRule`; db-gated `TestAFailedFleetReadLeavesTheStandingRowUnchanged`.
- Subscriber: `TestAFailedRewriteIsRetriedWithoutAnEvent`, `TestAnUnknownPassIsRetried`,
  `TestANotifyDuringBackoffCollapsesIntoIt`, `TestASafetyNetPassRunsWithoutAnEvent`
  (replaces `TestNothingRewritesWithoutAnEvent`), `TestBackoffIsBoundedAndJittered`,
  `TestTheSafetyNetIsJittered`, `TestBootJitterIsWithinBounds`,
  `TestASuccessfulPassIsLoggedWithReasonAndCount`, `TestTheProductionTimingsAreWhatTheRecordSays`.
- OS: `reportFromRow` defaults to `unknown`; the fixture suite at ten.
- Connector: `TestRetryingConnector_RetriesDialTimeout`, `TestRetryingConnector_BudgetIsBounded`,
  `TestTheProductionConnectBudgetIsWhatTheRecordSays`.

## 6. Delivery

One PR for D1 + D2 + D7 (this record's plan,
`docs/superpowers/plans/2026-09-13-readiness-failed-evaluation-and-retry.md`, deleted
in the merge per docs/CLAUDE.md). A second plan for D3 + D4 + D5; D6 as an issue and
a skipped gate.

## 7. Facts to re-verify before starting the next plan

`SHOW max_connections` on the live primary (expected 200); the conn-monitor's
`backends N/183` line; whether the CNPG cluster rolled between 16:24 and 16:33 on
2026-09-13; that `bff-znas`'s Deployment carries `envFrom: memql-db-pool`.
```

- [ ] **Step 2: Amend D5 of the 2026-09-07 record and the public fold rule**

In `docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md`, directly after the D5 heading (line 166, `### D5 -- Readiness recomputes on the events that change it, with one delayed boot re-write`), insert:

```markdown

> **AMENDED on 2026-09-13.** The premise that a broadcast reaches every replica is
> false for a node nobody dials (`bff-znas`, `edge`, `mcp` receive no mesh event at
> all), and a failed pass had no retry. D2 of
> `2026-09-13-readiness-unknown-and-retry-design.md` adds a jittered exponential retry
> after a failed or unknown pass and a 10-minute jittered safety-net pass; "not a
> timer" no longer holds as stated, and `TestNothingRewritesWithoutAnEvent` became
> `TestASafetyNetPassRunsWithoutAnEvent`. The event subscription, the debounce and the
> 30 s boot re-write stand.
```

In `docs/public/operate/configuration-readiness.md`, replace the numbered list (lines 57-64) with:

```markdown
1. Reports from nodes that are not live are dropped. A node is live when its
   health is one of healthy, connecting, degraded or draining **and** it was
   seen within the last 60 seconds.
2. Reports of `notApplicable` -- a node that does not host the module -- are
   dropped.
3. Reports of `unknown` -- a node that could not evaluate the module, because
   the read it depends on failed -- are set aside: they cast no vote, and the
   verdict's `unknown` list names them. A module whose live reports are all
   `unknown` is `unreported`, never `unconfigured`.
4. The worst remaining state wins.
5. **If the remaining reports disagree, the verdict is `partial` and names
   every reporter**, as `nodeId=state`, worst first.
```

- [ ] **Step 3: Check the doc gates and the citations**

Run: `cd /home/znas/projects/memql/memql && grep -rn "2026-09-13-readiness-unknown-and-retry" dsl/ component/ docs/ | grep -v "docs/superpowers/plans" && go test -count=1 ./docs/... 2>&1 | tail -3`
Expected: the concept doc comment, the subscriber header, `routing.go`, `node/CLAUDE.md` and the 2026-09-07 amendment all cite the record by its filename; any docs-tree test that exists passes (the `superpowers/` tree is exempt from the front-matter gate; `public/operate/configuration-readiness.md` keeps its existing front-matter untouched).

- [ ] **Step 4: Commit**

```bash
cd /home/znas/projects/memql/memql
git add docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md docs/public/operate/configuration-readiness.md
git commit -m "docs(readiness): record D1/D2/D7 -- unknown is not a verdict, the rewrite retries, the connector budget; amend D5 of 2026-09-07

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 9: Whole-tree verification and the PR

**Files:** none modified.

- [ ] **Step 1: The full Go tree, the OS, and the db-gated engine tree**

Run: `cd /home/znas/projects/memql/memql && make vet && make test`
Expected: PASS across every module (`make test` names the module path, which is what reaches `component/memql`; `go test ./...` does not).

Run: `cd /home/znas/projects/memql/memql && MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:5432/memql go test -count=1 ./component/memql/... ./component/database/...`
Expected: PASS with no skips.

Run: `cd /home/znas/projects/memql/memql/clients/os && npm run typecheck && npm test`
Expected: PASS.

Run: `cd /home/znas/projects/memql/memql && make concept-snapshot-check && make dsl-lint && make frontdoor-paths-check`
Expected: PASS (no HTTP route changed; the last is a no-op guard).

- [ ] **Step 2: Open the PR**

```bash
cd /home/znas/projects/memql/memql
git push -u origin fix/readiness-unknown-and-retry
gh pr create --repo znasllc-io/memql --base main --title "readiness: a failed evaluation is unknown, the rewrite retries, the connector budget covers a rollout" --body "$(cat <<'EOF'
## Why

On 2026-09-13 a rolling deploy saturated the single CNPG instance (max_connections=200; 16 old + up to 18 new pods x up to 8 connections). Every pod's boot readiness evaluation failed its fleet read; the evaluator wrote the failure as `unconfigured`; the 30 s re-write failed too; and the pods nobody dials (bff-znas, edge) never received the mesh event that let the others recover. The desk read "Inference: Partly set up" for a whole deploy while inference worked.

## What

Design record: `docs/superpowers/specs/2026-09-13-readiness-unknown-and-retry-design.md` (D1, D2, D7 here; D3-D6 next).

- D1: `unknown` state with a closed-vocabulary `reason`; the fold sets it aside and names the node; `WriteModuleReadiness` never persists unknown over a known row; typed `ReadinessUnknownError`.
- D2: the recompute subscriber retries a failed/unknown pass (2 s doubling to 60 s, jittered), collapses notifications during a backoff, runs a 10-minute jittered safety-net pass, logs success at INFO; boot goes through it after a 0-5 s jitter.
- D7: the connector retries a dial i/o timeout as well as 53300, inside a 15 s wall-clock budget.
- Additive DSL only (enum + `reason`); snapshot regenerated with no retirement.

## Frontend

The `moduleReadiness` row gains `state: "unknown"` and an optional `reason`; the OS fold and `reportFromRow` in this PR handle both. No endpoint or required field changed.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP
EOF
)"
```

Then let CI go green and enqueue with the bare `gh pr merge <n> --repo znasllc-io/memql` (root CLAUDE.md, Branch Workflow). Delete this plan file in the merge (docs/CLAUDE.md: a plan is spent when it ships).

---

## Self-review

**Spec coverage (D1, D2, D7 and the corrections):**

| Requirement | Task |
|---|---|
| D1: evaluator returns Unknown + closed-vocabulary reason on a failed registration read; integration arm's failed probe becomes Unknown | 2 |
| D1: write unknown only when this process never wrote a known verdict; skip otherwise; typed result/error | 4 |
| D1: cluster-scoped rows never overwritten by unknown (D3-compatible, additive) | 4 (`readinessPersistInput` struct + `persistReadiness`; the D3 clause is one field and one `if`) |
| D1: fold ignores unknown, lists nodes in `Verdict.unknown`; fixture shared with TS | 1, 6 |
| D1: enum gains `unknown` additively, `reason` field, snapshot regen | 3 |
| D1: OS default `unknown` at `readiness.tsx:52` | 6 |
| D1 tests named in the spec: inverted registration test, not-persisted-over-known, fresh-node-writes-unknown, fixture 10, db-gated unchanged row, integration error is unknown | 2, 4, 1, 4, 2 |
| D2: jittered exponential retry 2 s..60 s until success; Notify during backoff collapses; serialized | 5 |
| D2: safety net every 10 min +-20%, re-evaluates and writes (D4 deferred) | 5 |
| D2: boot handed to the subscriber, 0-5 s jitter; direct write only where no subscriber | 5 |
| D2: "NOT A TIMER" rewritten; `routing.go` and `component/node/CLAUDE.md` one sentence each | 5 |
| D2 tests named in the spec: retried without an event, backoff bounded and jittered, safety-net replaces no-timer test, production timings, notify-during-backoff collapses | 5 |
| Correction: INFO log on success with reason and count | 5 (`TestASuccessfulPassIsLoggedWithReasonAndCount`) |
| D7: connector retries dial i/o timeouts within a ~15 s budget for boot callers; test | 7 |
| Correction: 200 / 183 / single CNPG from memql-znas overlay; old+new pod overlap | Global Constraints, Task 7 header comment, Task 8 record section 1 and D7 |
| Design record in D-ruling style under `docs/superpowers/specs/`, D5 amendment, public fold rule | 8 |
| Repo rule: additive enum, never remove a field | 3, Global Constraints |

Gaps: none in D1/D2/D7. D3-D6 are deliberately absent and recorded as deferred in Task 8 so the next plan starts from the record, not the scratchpad.

**Placeholder scan:** no "TBD"/"TODO"/"similar to Task N"; every code step carries the code; every Run step carries the command and the expected outcome. The one conditional instruction (Task 5 Step 4, if `-race` flags the pre-existing after-Start assignment) says exactly what to change and forbids weakening the assertion.

**Type consistency:** `write func(context.Context) (int, error)` is used identically in Task 5's constructor, tests and `NewReadinessRecomputeSubscriber` (which passes `eng.WriteModuleReadiness`, whose signature Task 4 leaves as `(int, error)`). `readinessTimings` field names (`Debounce`, `RetryBase`, `RetryMax`, `SafetyNet`) match between struct, `productionReadinessTimings`, `testTimings` and the loop. `IsReadinessUnknown` is defined in Task 4 and used in Task 5 and the db-gated test. `readinessExecutor(ctx)` takes a context in Task 4's definition and both call sites. `reportFromRow` is exported in Task 6 and imported by its test. `Verdict.Unknown` (Go, `json:"unknown"`) and `Verdict.unknown` (TS) agree with fixture 10's `expect[].unknown`. Fixture floors are 10 on both sides. The connector's `budget` field appears in the struct, the constructor, every test literal and the production-budget test.
