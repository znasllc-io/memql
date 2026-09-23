# Procedure certification and replay -- implementation plan (epic memql#5408)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A learned procedure climbs candidate -> shadow -> canary -> trusted on its own evidence with exactly one human approval, replays with no model once trusted, stops honestly on divergence and hands the app the partial trace, and demotes or retires itself -- visible in MemQL OS and proven in the proving suite.

**Architecture:** Every DECISION is pure: the ladder state machine, the serve decision, the comparison, the replay target and the reliability arithmetic live in `component/work`; binding, materialization, token-replay fitness, alignment and the learned initiation set live in `component/procedure`. `integrations/procedure` is the only half that reads or writes a row: it lifts (closing epic C's wiring gaps on the way), runs shadow comparisons when a recording succeeds, serves a trusted/canary goal through one embedded automation (`replayLearnedProcedure`), writes the ladder, raises the one `procedurePromotion` approval and runs the two maintenance sweeps. The runner executes the procedure TEMPLATE stored on the construct (`construct.procedure`), not the rendered MemQL source; the source is the auditable artifact the approval pins together with the template and the preconditions.

**Tech Stack:** Go 1.26 (leaf modules `component/work`, `component/procedure`), MemQL DSL (`dsl/authoring`, `dsl/work`, `dsl/procedure`), React/TS (MemQL OS, `clients/os`), the proving suite (`component/proving`, `cmd/memql-bench`, `test/proving`).

**Spec:** `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md` -- decisions D3, D4, D14 (retirement), D15, D16, D23 (certification half), section 4 epic D, section 7.

**Issues:** epic #5408; tasks #5409 (concepts), #5410 (component/work), #5411 (runner + sweeps), #5412 (OS), #5413 (proving). ONE PR closes all six.

---

## Global Constraints

- Values, not constants: `m=5`, `k=2`, canary matches `5`, failures to demote `2`, insufficient preconditions to demote `1`, retire after `30` days are ROWS (`v1:authoring:ladderPolicy:primary`), seeded; Go carries the same numbers only as `DefaultLadderPolicy()` for an absent/unreadable row, and a non-positive value normalizes to its default.
- Owner-scoped, composite tier (D17): every read of a person's rows runs under `auth.ContextWithUserActor(owner)`; every `@serverOnly` write goes through ONE `auth.ContextWithInternalOrigin` stamp site per package; a maintenance sweep iterates owners and borrows each one's actor (constructs are `@rowAuthz(owner="ownerUserId")` with NO clusterOwner -- a cluster-wide read returns zero rows and no error).
- Every field is ADDITIVE. No field is removed, no enum value is dropped, no `@required` is added to a concept with rows (memql#5199). `make concept-snapshot` must pass without a `retired` ledger entry.
- No `if env == ...` anywhere (`TestNoEnvironmentBranchingInEngineCode`).
- The recording's result digest is the COCKPIT's (text or canonical JSON of the app's tool result) and is NOT comparable across executors. Comparisons use only executor-independent observables: `isError`, `exitCode`, inferred `resultType`, and content digests (`sha256` hex over file bytes, the Library file row's `sha256`).
- The cockpit fingerprint shape (memql-cockpit `internal/worker/harness/record.go` `Fingerprint`): `{type, v, seq, takenAt, app{id,version,harness}, platform{os,arch}, tools[{name,version}], cwd, cwdDigest?, cwdEntries?, cwdTruncated?, variables[{name,set,digest?}], inputs[...]}`. Preconditions are learned from THIS shape (the step concept's `{tools, cwdDigest, variables, filesRead}` sentence is stale and is corrected in Task 3).
- An ABSENT measurement is never a match (step concept, D16): a precondition that cannot be measured on the target falls back to the app.
- Strings in MemQL call text go through `langparser.QuoteString` / `langparser.RenderCall`, never Go `%q` (`TestDSLCallStringsDoNotUseGoQuoting`).
- Stage files by explicit path; never `git add -A` / `git add .`. Commit messages: `Issue #<N>: <what>`; end with the `Co-Authored-By` trailer the session gives.
- No emojis anywhere (docs, UI copy, code comments).
- Never run prettier. OS code is hand-formatted.
- Canonical construct names are unique across the WHOLE DSL tree; grep before adding one.
- MemQL OS acceptance is RENDERED screenshots, both themes, empty and populated, desktop and narrow (clients/os/DESIGN.md, clients/os/SUPERVISED-VISUAL-COMPOSITION.md).

## Review Focus

1. A procedure recorded on a Mac replaying on the workbench: the platform predicate must NOT be compared on the workbench target (a portable footprint is platform-independent by D4), but tool versions the procedure's own commands use ARE; the test lives in Task 2 (`CheckPreconditions` target `workbench`).
2. A free parameter the goal's input cannot supply: a trusted replay must refuse to start and fall back (never run with a hole bound to ""); test in Task 5 (`TestTrustedReplayWithAnUnboundParameterFallsBackBeforeTheFirstStep`).
3. A construct demoted or re-lifted between compile and execution: `procedureReplay` re-reads the rung at execution and falls back when it is no longer canary/trusted; test in Task 5.
4. A promotion approved after the construct changed: the decide handler recomputes the artifact hash from the construct's CURRENT `procedureHash` and refuses with the existing artifact-changed error; test in Task 5 (`integrations/work`).
5. A divergence on a side-effecting step: the steps that already ran (with their idempotency keys) are named in the guidance handed to the app and in the replay run's outcome, and the same replay run never re-executes a step whose receipt exists; test in Task 5 and proven by `durability.duplicatedSideEffectsAcrossDivergence` in Task 7.

---

## 0. Ground truth this plan rests on (measured 2026-09-23 at `fa877627f`)

- `component/work` is a leaf module with NO requires. `DecideServe(ReplayContext) ServeVerdict` has one production caller, `integrations/work/modeljournal.go:85` (per model call, journal vs live). `component/memql` cannot import `component/work` (dependency fan-out; `component/memql/model_journal.go:10-23`).
- `component/procedure` is a leaf module with NO requires; `purity_test.go` allows only stdlib (the one permitted widening is `component/work`; this plan does NOT use it). It has no fitness, alignment, binding or materialization.
- Epic C's wiring is inert in the shipped binary. The gaps this epic's path runs through, all closed in Task 4:
  - G1 `integrations/procedure` is imported by no binary (absent from `app/plugins_core.go`).
  - G2 `goalSignature` is on no work concept; the corpus loader skips every run.
  - G3 `step.input` is never written; the app's arguments live on the `tool_result` observation (`data.args`, a JSON STRING, whole up to 256 KiB; `data.argsRef` when spilled).
  - G4 `updateWorkStep` does not accept `fingerprint`; the session writer's value is silently dropped (internal callers are lenient about unknown args).
  - G7 Gate 1 never runs: `pctx.Engine` (`*memql.MemQLEngine`) has no `CompileBundle`; `app.CognitionEngineAdapter` does.
  - G8 every rendered step is a comment line (no tool named `exec`/`fs_write`/... exists).
  - G9 `store.literal()` has no map case; `validationReport` renders as a quoted string.
  - G10 both procedure automations run as `system:automation:<name>` RoleReader: `workRunById` is `@serverOnly` + cluster-owner-filtered (zero rows) and the handler refuses a non-owner caller; the 6-hourly sweep never gets an owner.
  - G11 lifted constructs are never catalogued (this plan serves procedures through the LADDER, not the `catalogued` flag -- see Task 3 `procedureConstructsForGoalSignature`).
  - G15 every lift mints a fresh construct row under the same name.
  - Left as-is and named in the PR body: G6 (no data-flow holes at action level, since app results are digests), G12 (no `Deriver` implementation; a hole nothing derives is free), G13 (no `feedback` observation kind -- epic E #5416 adds it; this epic's gate reads it when present).
- The approval decide path (`integrations/work/approval.go` `handleDecideApproval`): only the owner decides; `currentArtifactHash` is a per-kind switch; `resumeParkedRun` sets the run `running` on approve and FAILS it on reject -- it must be skipped for `procedurePromotion`, whose run is a finished shadow run.
- `ValidateApprovalKind` (`component/work/approval_routing.go`) requires a runId for every kind but `routingReview`. `procedurePromotion` carries the runId of the shadow run whose comparison met the threshold.
- `loadWorkTemplate` (`app/work_template.go`) loads `loader.LoadByName(automationName)` from the EMBEDDED tree when the run's `templateConstructId` is empty. The served path therefore writes NO `templateConstructId` and names the embedded `replayLearnedProcedure`.
- Seeds: a global seed's id defaults to its name; the materializer calls `create<Concept>` with `<concept>Id`; a global seed is re-asserted every boot on every node (fields the seed declares win).
- `maintenanceAutomations` is the compiled Go map in `component/auth/maintenance_actor.go`; its gate test pins the sorted name list (`maintenance_actor_gate_test.go`). A cron firing carries NO args.
- `shippedAutomationCount` (`component/automations/strict_automation_boot_test.go`) is a hard-coded integer (64 today).
- The Work app is **Nexus**: `clients/os/src/apps/nexus/`. Its Automations section (`AutomationsSection.tsx`, `automations.ts`, `useAutomations.ts`) is the only construct view; approvals live in `ApprovalsSection.tsx`; kind vocabulary in `words.ts`; tests in `clients/os/test/nexus/` with `harness.tsx`.
- The proving lane (`ci.yml` job `proving`) runs `go run ./cmd/memql-bench --do=gate` against a real Postgres, then `--do=scorecard --check`. `checkNegativeControl` requires a control to read NON-ZERO; `CorpusControls` demands controls only for Lower+Blocking metrics; every claimed metric must belong to the scenario's own family.

---

## 1. Contracts (every stream codes against these; change one only by editing this section first)

### 1.1 `component/work` (Task 1)

```go
// ladder.go
type Rung string
const (
	RungNone      Rung = ""          // not a learned procedure
	RungCandidate Rung = "candidate"
	RungShadow    Rung = "shadow"
	RungCanary    Rung = "canary"
	RungTrusted   Rung = "trusted"
	RungRetired   Rung = "retired"
)
func ParseRung(s string) (Rung, bool)            // unknown -> (RungNone, false); callers treat unknown as NOT servable

type LadderPolicy struct {
	ShadowMatches        int // m
	DistinctBindings     int // k
	CanaryMatches        int
	FailuresToDemote     int
	InsufficientToDemote int
	RetireAfterDays      int
}
func DefaultLadderPolicy() LadderPolicy          // {5, 2, 5, 2, 1, 30}
func (p LadderPolicy) Normalize() LadderPolicy   // every non-positive field -> its default

type LadderState struct {
	Rung                Rung
	ShadowMatches       int
	CanaryMatches       int
	DistinctBindings    map[string][]string // free parameter id -> distinct binding DIGESTS in the current streak
	Failures            int
	Insufficient        int
	PromotionApprovalId string              // the open procedurePromotion approval, "" when none
	LastReplayAt        time.Time
}

type LadderEventKind string
const (
	EventShadowCompared   LadderEventKind = "shadowCompared"
	EventPromotionDecided LadderEventKind = "promotionDecided"
	EventReplayed         LadderEventKind = "replayed"      // a canary or trusted replay finished
	EventStartRefused     LadderEventKind = "startRefused"  // preconditions did not hold at start
	EventSweep            LadderEventKind = "sweep"         // demotion + retirement re-evaluation
)
type LadderEvent struct {
	Kind           LadderEventKind
	At             time.Time
	Match          bool              // shadowCompared: matched; replayed: succeeded
	Approved       bool              // promotionDecided
	Bindings       map[string]string // shadowCompared: free parameter id -> bound VALUE (digested by Advance)
	FreeParameters []string          // shadowCompared: every free parameter id of the procedure
	Insufficient   bool              // replayed: diverged although every precondition held
	LastUsedAt     time.Time         // sweep: newest of lastReplayAt and the construct's creation
}
type Transition struct {
	From, To Rung
	State    LadderState
	Propose  bool   // raise ONE procedurePromotion approval now
	Demoted  bool
	Retired  bool
	Reason   string // one sentence a person reads in Nexus
}
func Advance(s LadderState, e LadderEvent, p LadderPolicy) Transition
func BindingDigest(value string) string          // "sha256:" + hex(sha256(value)); what DistinctBindings stores

// verdict.go (D21 vocabulary, D23 certification half)
type Verdict string
const (
	VerdictUnseen  Verdict = ""
	VerdictLike    Verdict = "like"
	VerdictDislike Verdict = "dislike"
	VerdictNeutral Verdict = "neutral"
)
func ParseVerdict(s string) Verdict               // like|liked, dislike|disliked, neutral; anything else unseen
type StepVersions struct{ Verdicts []Verdict }    // one per version of ONE instance step, oldest first
type CandidateEvidence struct {
	Uses             int
	UnexplainedHoles int
	Instances        [][]StepVersions             // [instance][template step]
}
func CandidateGate(e CandidateEvidence) (ready bool, reason string)
func EntryRung(e CandidateEvidence) (Rung, string) // shadow when ready, else candidate

// reliability.go
func Reinforce(reliability float64, success bool) float64 // EMA, alpha 0.2: success r+0.2(1-r); failure 0.8r; clamped [0,1]

// replay.go (extended; existing fields and behaviour unchanged)
const ServeConstruct = "construct"
type ReplayContext struct {
	Mode, ReplayPolicy string
	JournalHit, SameGoal, BeforeForkPoint bool
	ConstructRung Rung // NEW: the rung of the construct compile's exact tier found; RungNone when none
}
type ServeVerdict struct {
	Source   string
	Diverged bool
	Reason   string
	Shadow   bool // NEW: the app serves, the construct replays beside it in a sandbox
	Standby  bool // NEW: the construct serves, the app stands by to take over on divergence
}
// DecideServe: when ConstructRung != RungNone AND Mode is "" or "live":
//   trusted -> {Source: ServeConstruct}
//   canary  -> {Source: ServeConstruct, Standby: true}
//   shadow  -> {Source: ServeLive, Shadow: true}
//   candidate / retired / anything else -> {Source: ServeLive, Reason: "..."}
// A replay or fork run keeps the journal decision (it must reproduce the recorded run).

// compare.go
type ContentDigest struct{ Op, Path, Digest string } // Op "read"|"write"; Path relative to the workspace; Digest sha256 hex
type StepObservation struct {
	IsError    *bool
	ExitCode   *int
	ResultType string          // object|array|string|number|boolean|null; "" unknown
	Contents   []ContentDigest
}
type StepExpectation struct {
	NoError    bool            // every recording reported isError=false
	ExitCode   *int            // every recording reported this same code
	ResultType string          // every recording agreed on this type
	Contents   []ContentDigest // one per (op,path) seen in EVERY recording; Digest "" when the recordings varied
	Exact      bool            // the recordings agreed on every observable they carried: a deterministic step
}
func ExpectationFrom(recordings []StepObservation) StepExpectation
func Compare(exp StepExpectation, got StepObservation) (bool, []string)              // a replay against the recordings
func CompareShadow(exp StepExpectation, app, replay StepObservation) (bool, []string) // exact where exp.Exact, by type where the recordings varied
func InferTextType(s string) string // the cockpit's rule: JSON-parse -> its type; otherwise "string"

// target.go (D4)
type ReplayTarget string
const (
	TargetWorkbench ReplayTarget = "workbench"
	TargetMachine   ReplayTarget = "machine"
)
var ErrMachineLocalOnWorkbench = errors.New("work: a machine-local procedure cannot replay on the workbench")
func ActionFootprint(tool, cwd string, paths []string) Footprint // Machine when a path is absolute and outside cwd; Files for fs_*; External for fetch
func ReplayTargetFor(fp Footprint) ReplayTarget                  // Machine -> TargetMachine; else TargetWorkbench
func CheckTarget(fp Footprint, target ReplayTarget) error        // ErrMachineLocalOnWorkbench before the first step

// compile.go (extended): CatalogCandidate gains
//   Rung Rung // RungNone for an authored construct; the ladder rung for a learned procedure
// Decide is unchanged; the planner filters procedure candidates through DecideServe BEFORE Decide
// and ranks trusted before canary.

// approval_procedure.go
const ApprovalKindProcedurePromotion = "procedurePromotion"
type PromotionProposal struct {
	OwnerUserId, ConstructId, ConstructName, ProcedureHash, ShadowRunId string
	ShadowMatches int
	DistinctBindings map[string]int // free parameter -> distinct bindings seen
	RecordedFrom map[string]any     // {app, model, effort, sessionIds, runIds}
	Title string                    // the goal statement the procedure serves
}
func ProcedurePromotionApproval(p PromotionProposal, requestedAt time.Time) ApprovalRequest
// subject = {constructId, constructName, procedureHash, title, shadowMatches, distinctBindings, recordedFrom}
// ArtifactHash = p.ProcedureHash (the construct version pin; see 1.3)
```

### 1.2 `component/procedure` (Task 2)

```go
// value.go: Node gains
//   Form string // rendering hint, IGNORED by Equal: "" | "argv" | "path" | "rootedPath" | "json"
// canonicalizeString sets it (commandKeys -> argv; JSON doc -> json; path -> path, or rootedPath when it began with "/").
// Clone copies it; AntiUnify keeps the left operand's Form.

// bind.go
func Bind(t Template, stepIndex int, a Action) (map[string]string, bool)          // hole id -> literal; false when a literal position differs or the shape differs
func BindInstance(t Template, instance []Action) (map[string]string, bool)
func Materialize(n *Node, values map[string]string) (any, error)                   // back to a Go value; argv -> one shell-quoted string; path -> "a/b" ("/a/b" rootedPath); json -> JSON text; hole -> values[id] (missing -> error naming the hole)
func LearnInputMap(t Template, instances [][]Action, inputs []map[string]any) map[string]string // FREE hole id -> goal input key whose canonical literal equals the hole's value in EVERY instance

// preconditions.go (D16)
type Preconditions struct {
	Platform       map[string]string `json:"platform,omitempty"`       // os, arch
	Tools          map[string]string `json:"tools,omitempty"`          // name -> version (only tools the procedure's exec steps invoke)
	Variables      map[string]string `json:"variables,omitempty"`      // name -> "unset" | digest
	EmptyWorkspace *bool             `json:"emptyWorkspace,omitempty"` // every recorded start had cwdEntries == 0
}
func LearnPreconditions(fingerprints []map[string]any, usedTools []string) Preconditions // a predicate is kept only when EVERY start agreed on it
type PreconditionReport struct {
	Held       bool
	Mismatches []string // "tools.node: recorded 22.1.0, found 20.3.0"
	Unmeasured []string // a learned predicate the observation does not carry -> Held=false
}
func CheckPreconditions(learned, observed Preconditions, target string) PreconditionReport // target "workbench" ignores Platform and Variables
func UsedTools(t Template) []string // argv[0] basenames of exec steps

// fitness.go / align.go (D16)
func TokenReplay(tree *ProcessTree, trace []string) FitnessResult
type FitnessResult struct {
	Fitness float64 // 0.5*(1-missing/consumed) + 0.5*(1-remaining/produced); 1 for a fitting trace
	Produced, Consumed, Missing, Remaining int
	Fits           bool
	FirstDeviation int // index into trace of the first event that needed a missing token; -1 when none
}
func PrefixFits(tree *ProcessTree, trace []string) (bool, int) // a running check: remaining tokens are expected mid-run; returns first deviating index or -1
type MoveKind string
const (MoveSync MoveKind = "sync"; MoveLog MoveKind = "log"; MoveModel MoveKind = "model")
type Move struct{ Kind MoveKind; Label string; TraceIndex int }
type Alignment struct{ Moves []Move; Cost int }
func Align(tree *ProcessTree, trace []string) Alignment
func (a Alignment) FirstDeviation() (Move, bool)
func AssignSymbol(symbols []Symbol, a Action, budget int) (string, bool) // the symbol an action belongs to, under Symbolize's own distance budget
func SequenceTree(symbols []string) *ProcessTree                          // the procedure's own model: seq(leaf...)
func MarshalTree(t *ProcessTree) ([]byte, error); func UnmarshalTree(b []byte) (*ProcessTree, error)
func MarshalTemplate(t Template) ([]byte, error); func UnmarshalTemplate(b []byte) (Template, error) // Node trees round-trip including Form, LitType, HoleId, HoleType
```

### 1.3 DSL surface (Task 3)

`dsl/authoring/concepts.memql`, concept `construct` gains (all optional, additive):

| field | type | meaning |
|---|---|---|
| `ladder` | `enum("candidate","shadow","canary","trusted","retired")` | the rung; ABSENT on every non-learned construct |
| `preconditions` | `object` | component/procedure.Preconditions JSON |
| `procedure` | `object` | what a replay executes: `{v:1, level, title, goalSignature, inputKeys, steps:[{tool, args(Node JSON), symbol}], holes:[...], expect:[StepExpectation JSON], inputMap:{holeId:inputKey}, freeParameters:[holeId], footprint:{...}, target, symbols:[{id,tool,template}], model:(ProcessTree JSON), recordedFrom:{app,model,effort,sessionIds,runIds}}` |
| `procedureHash` | `string` | `sha256:` over canonical JSON of `{source, procedure, preconditions}` -- the version a procedurePromotion approval pins |
| `shadowMatches` | `int` | consecutive shadow matches in the current streak |
| `canaryMatches` | `int` | consecutive clean canary replays |
| `distinctBindings` | `object` | `{holeId: [binding digest, ...]}` in the current streak |
| `failures` | `int` | consecutive failed replays on canary/trusted |
| `insufficient` | `int` | replays that diverged although every precondition held |
| `promotionApprovalId` | `string` | the open procedurePromotion approval, empty when none |
| `lastReplayAt` | `datetime` | last shadow/canary/trusted replay |
| `ladderReason` | `string` | why the ladder last moved, one sentence |
| `ladderChangedAt` | `datetime` | when the rung last changed |

New concept `ladderPolicy` in `dsl/authoring/concepts.memql`: fields `shadowMatches int!`, `distinctBindings int!`, `canaryMatches int!`, `failuresToDemote int!`, `insufficientToDemote int!`, `retireAfterDays int!`. Tier: readable by every signed-in person, written only by the seed (use `@rowAuthz(clusterOwner, rankFloor="viewer")` if `rankFloor` admits every rank at or above viewer; otherwise the nearest established read-for-all pattern -- verify against `component/memql/rowauthz_*` and record the choice in the concept doc comment). Seed `seed ladderPolicy primary { ... }` in a new `dsl/authoring/seeds.memql` -> id `v1:authoring:ladderPolicy:primary`, refreshed on every boot like the cluster singletons.

`dsl/authoring/mutations.memql` (all new constructs `@serverOnly` with a `///` doc saying why caller-scoping is not the fix):
- `createLadderPolicy { args { ladderPolicyId string!  shadowMatches int! distinctBindings int! canaryMatches int! failuresToDemote int! insufficientToDemote int! retireAfterDays int! } insert { ... stamp { id: args.ladderPolicyId } } }` -- the seed materializer's create.
- `recordProcedure { args { constructId string!  source string!  procedure object!  preconditions object  procedureHash string! } update {...} }` -- the lift's payload write (first lift AND re-lift).
- `recordConstructLadder { args { constructId string!  ladder enum(...)!  shadowMatches int  canaryMatches int  distinctBindings object  failures int  insufficient int  promotionApprovalId string  lastReplayAt string  ladderReason string  ladderChangedAt string } update {...} }`. `promotionApprovalId: ""` must be writable (clearing it) -- keep it in `accept`, never `?? ""`.
- `recordConstructReliability` unchanged; it gets its first production caller.

`dsl/authoring/queries.memql`:
- `procedureConstructsForGoalSignature(goalSignature)` `@actor @unbounded(...)`: `row.ownerUserId == actor.userId && row.goalSignature == args.goalSignature && row.ladder in ["shadow","canary","trusted"]`, shape `constructFull`. Compile's exact tier reads it beside `cataloguedConstructsForGoalSignature`.
- `learnedProceduresForOwner` `@actor`, paginated 100: `row.ownerUserId == actor.userId && row.targetNamespace == "procedure"`, shape `procedureCard`. The Nexus list.
- `procedureConstructByName(name)` `@actor`: owner + name + targetNamespace procedure. The idempotent lift.
- `ladderPolicyCurrent` `@actor`: pinned to `row.id == "v1:authoring:ladderPolicy:primary"`, shape `ladderPolicyFull`.
- `authoringConstructById` already exists; its shape must project the new fields.

`dsl/authoring/shapes.memql`: `constructFull` gains every new field; new `procedureCard` (id, name, kind, status, ladder, shadowMatches, canaryMatches, distinctBindings, failures, insufficient, promotionApprovalId, lastReplayAt, ladderReason, ladderChangedAt, reliability, reinforceCount, lastReinforced, goalSignature, procedure, preconditions, procedureHash, bundleId, row.createdAt); new `ladderPolicyFull`.

`dsl/work/concepts.memql`:
- `approval.kind` enum gains `"procedurePromotion"` (append; its description says it is raised by the ladder, carries the shadow run's id, and that its artifact hash is the construct's `procedureHash`).
- `run` gains `goalSignature string` (component/work.GoalSignature of the goal this run serves -- copied onto a delegated session's recording run) and `parentRunId string` (the run whose step delegated this session run).
- `step.fingerprint` description corrected to the cockpit's real shape.
`dsl/work/mutations.memql`: `updateWorkRun` and the run-opening mutation(s) the journal and `workjournal.Begin` use accept `goalSignature` and `parentRunId`; `updateWorkStep` accepts `fingerprint object` (G4).
`dsl/work/queries.memql`: `workRunsForOwnerGoalSignature(goalSignature)` `@actor`: owner + goalSignature + `status == "succeeded"`, sort `"row.createdAt","desc"`, paginate 200 -- the corpus read (replaces the 200-run scan).

`dsl/procedure/builtins.memql` (executors in `integrations/procedure`; no `@serverOnly` on builtins -- gates live in handlers):
- `procedureStep { step integer!  tool string!  args object }` -> `integration.procedure.step`. The statement every rendered step is; its handler REFUSES outside a replay ("procedureStep runs only inside replayLearnedProcedure"), because a learned procedure is served through the ladder and never activated as an ordinary automation.
- `procedureReplay { constructId string! }` -> `integration.procedure.replay`. Serves the current work run from the construct.
- `procedureLadderSweep { sweep string! @enum("demotion","retirement") }` -> `integration.procedure.ladderSweep`.
- `procedureDecidePromotion { approvalId string! }` -> `integration.procedure.decidePromotion`.

`dsl/procedure/automations.memql`:
- `learnFromSucceededRun` gains `@filter(row => row.status == "succeeded")`; its handler now also runs the SHADOW comparison.
- `replayLearnedProcedure { args { procedureConstructId string! }  replayed := builtin procedureReplay(constructId: args.procedureConstructId) }` -- no trigger; compile names it. (If the loader requires a trigger, give it the narrowest one the tree already uses for run-only templates and say why in its doc.)
- `demoteProcedures` `@trigger(schedule="0 */15 * * * *")` -> `procedureLadderSweep(sweep: "demotion")`.
- `retireProcedures` `@trigger(schedule="0 30 3 * * *")` -> `procedureLadderSweep(sweep: "retirement")`.
- `onProcedurePromotionDecided` `@trigger(event="node.updated", concept="v1:work:approval") @filter(row => row.kind == "procedurePromotion" && row.decision != "")` -> `procedureDecidePromotion(approvalId: args.id ?? "")`.
- `mineProcedureCorpusAcrossAutomations` unchanged in DSL; its handler iterates owners when called by the maintenance principal with a blank owner.

`component/auth/maintenance_actor.go`: entries (with reasons) for `learnFromSucceededRun`, `mineProcedureCorpusAcrossAutomations`, `demoteProcedures`, `retireProcedures`, `onProcedurePromotionDecided`; pin updated in `maintenance_actor_gate_test.go`.

`dsl/rbac/seeds.memql`: `read app:settings/procedures` seeded on the same roles as `app:settings/levels`.

### 1.4 `integrations/procedure` exported surface (Tasks 4-5)

```go
// dispatch.go
type Dispatcher interface {
	// Dispatch runs one materialized step on the target and reports what it did in executor-independent terms.
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)
}
type DispatchRequest struct {
	Target       work.ReplayTarget
	OwnerUserId  string
	RunId        string // the replay run; keys the workbench workspace
	StepKey      string
	IdempotencyKey string
	Tool         string         // exec | fs_write | fs_read | fetch | mcp
	Args         map[string]any // materialized
	Sandbox      bool           // shadow: never the person's machine, never a real MemQL write
}
type DispatchResult struct {
	Observation work.StepObservation
	Output      any    // the executor's result, kept for the replay run's step receipt
	Delivered   bool   // a side effect reached the world
}
func (i *Integration) SetDispatcher(target work.ReplayTarget, d Dispatcher)

// fallback.go
type AppFallback interface {
	Handover(ctx context.Context, req FallbackRequest) (FallbackOutcome, error)
}
type FallbackRequest struct {
	OwnerUserId, GoalId, RunId, StepId, App, Level string
	Statement string
	Guidance  Guidance
}
type Guidance struct {
	Procedure   string          // construct name
	Diagnosis   string          // one paragraph: what diverged, where, why
	Completed   []CompletedStep // steps that ran, with their idempotency keys -- the app must not redo them
	Alignment   []procedure.Move
	Prompt      string          // the rendered guidance text appended to the statement
}
type CompletedStep struct{ Index int; Tool, IdempotencyKey, Summary string; SideEffect bool }
type FallbackOutcome struct{ ChildRunId, Content string }
func (i *Integration) SetAppFallback(f AppFallback)

// probe.go
type Prober interface {
	Probe(ctx context.Context, target work.ReplayTarget, ownerUserId, runId string, learned procedure.Preconditions) (procedure.Preconditions, error)
}
func (i *Integration) SetProber(p Prober)
func (i *Integration) SetCompiler(c CompileGate) // G7; CompileGate = interface{ CompileBundle([]memql.SandboxConstruct) memql.SandboxReport }

// replay.go
type ReplayMode string // "shadow" | "canary" | "trusted"
type ReplayRequest struct {
	OwnerUserId, ConstructId string
	Mode       ReplayMode
	GoalRunId  string            // canary/trusted: the goal's run the replay serves; shadow: the recording run compared against
	Input      map[string]any    // canary/trusted: the goal's input (binds free parameters through inputMap)
	Bindings   map[string]string // shadow: hole id -> value, bound from the app's own actions
	AppActions []work.StepObservation // shadow: the app's observation per template step, aligned by Bind
}
type ReplayOutcome struct {
	Served        bool   // canary/trusted: the goal was answered by the procedure
	Match         bool   // shadow: every step compared equal
	Diverged      bool
	DivergedStep  int
	Diagnosis     string
	StartRefused  bool   // preconditions did not hold, or a parameter could not be bound: nothing ran
	Insufficient  bool   // diverged although every precondition held
	Completed     []CompletedStep
	Fitness       procedure.FitnessResult
	Alignment     procedure.Alignment
	FellBack      bool
	Fallback      FallbackOutcome
	ReplayRunId   string
	ModelCalls    int    // always 0 for the replay itself; the fallback's are the app's
	Transition    work.Transition
}
func (i *Integration) Replay(ctx context.Context, req ReplayRequest) (ReplayOutcome, error)

// lift entry points used by the proving driver and tests
func (i *Integration) LearnFromRun(ctx context.Context, runId string) (LearnResult, error)   // exported wrapper of the handler body
func (i *Integration) ShadowCompare(ctx context.Context, recordingRunId string) ([]ReplayOutcome, error)
func (i *Integration) DecidePromotion(ctx context.Context, approvalId string) (work.Transition, error)
func (i *Integration) Sweep(ctx context.Context, sweep string, now time.Time) (SweepResult, error)
```

---

## 2. Streams, ownership, order

| Task | Issue | Stream | Owns (edits nobody else makes) | Starts after |
|---|---|---|---|---|
| 1 | #5410 | pure ladder | `component/work/{ladder,verdict,reliability,compare,target,approval_procedure}.go` + `replay.go` + tests | now |
| 2 | #5411 (pure half) | pure learning | `component/procedure/{value,canonicalize,bind,preconditions,fitness,align,serialize}.go` + tests | now |
| 3 | #5409 | DSL + gates | `dsl/authoring/*`, `dsl/work/*`, `dsl/procedure/*`, `dsl/rbac/seeds.memql`, `component/auth/maintenance_actor*.go`, conformance pins, counts, `make sdk-gen` / `concept-snapshot` outputs | now |
| 4 | #5409/#5411 | C gaps + lift | `integrations/procedure/{corpus,learn,persist,store,render}.go`, `app/plugins_core.go`, `app/integrations_procedure.go`, `integrations/agent/worker/app_session_delegate.go` (child run fields), `integrations/work/session.go` (goalSignature on an opened recording run) | 1, 2, 3 merged |
| 5 | #5410/#5411 | runner, serve, sweeps | `integrations/procedure/{replay,shadow,ladder,serve,sweep,dispatch,fallback,probe}.go`, `integrations/planner/work_compile*.go`, `integrations/work/approval.go`, `app/integrations_procedure_agent.go` | 4 merged |
| 6 | #5412 | OS | `clients/os/src/apps/nexus/*`, `clients/os/src/apps/settings/*`, `clients/os/src/apps/registry.tsx`, `clients/os/src/styles/index.css` (own appended blocks), `clients/os/test/{nexus,settings}/*` | 3 merged (generated SDK) |
| 7 | #5413 | proving | `component/proving/**`, `cmd/memql-bench/**`, `test/proving/**`, `docs/public/overview/proving-scorecard.md` | 5 merged |
| 8 | all | docs + delivery | `CLAUDE.md` procedure section, `docs/public/...` certification page, design record "what shipped differently", plan deletion, PR | 5, 6, 7 merged |

Each stream works in its OWN worktree on its OWN branch cut from `epic/procedure-certification-replay`; the coordinator merges stream branches into the epic branch. Generated artifacts (`sdk/**/generated_*`, `component/architecture/embedded/*`, concept snapshot) are regenerated by whoever merges LAST, never hand-merged.

---

## Task 1: the ladder as a pure state machine (`component/work`, #5410)

**Files:**
- Create: `component/work/ladder.go`, `component/work/ladder_test.go`
- Create: `component/work/verdict.go`, `component/work/verdict_test.go`
- Create: `component/work/reliability.go`, `component/work/reliability_test.go`
- Create: `component/work/compare.go`, `component/work/compare_test.go`
- Create: `component/work/target.go`, `component/work/target_test.go`
- Create: `component/work/approval_procedure.go`, `component/work/approval_procedure_test.go`
- Modify: `component/work/replay.go`, `component/work/replay_test.go`
- Modify: `component/work/approval_routing.go` only if `ValidateApprovalKind` needs to know the new kind (it must accept procedurePromotion WITH a runId; add a table row to `approval_routing_test.go`).

**Interfaces:** Produces section 1.1 exactly. Consumes nothing new. The module stays requires-free.

- [ ] **Step 1: failing tests for the ladder.** In `ladder_test.go`, table-driven, behaviour-sentence names, a comment block per test saying why the property matters:
  - `TestShadowMatchingMTimesAcrossKBindingsProposes`: policy {m:3,k:2}; free params [s0.path]; three matches with bindings "a","b","a" -> third transition has `Propose=true`, `State.PromotionApprovalId` still "" (the caller fills it), `To==RungShadow`.
  - `TestShadowMatchingMTimesOnOneBindingDoesNotPropose` (#5410 acceptance): same policy, bindings "a","a","a" -> no transition proposes; `len(DistinctBindings["s0.path"])==1`.
  - `TestAProcedureWithNoFreeParameterProposesOnMMatches`.
  - `TestAShadowMismatchResetsTheStreak`: two matches, a mismatch, two matches -> no proposal at m=3; `ShadowMatches==2` after.
  - `TestAPendingPromotionIsNeverProposedTwice`: state with `PromotionApprovalId="appr1"` and m matches -> `Propose=false`.
  - `TestApprovalMovesShadowToCanaryAndResetsCanaryEvidence`; `TestRejectionKeepsShadowAndMakesItReEarnTheProposal` (streak reset, approval id cleared).
  - `TestPromotionDecidedOutsideShadowIsANoOp` (canary/trusted/candidate/retired unchanged, Reason names why).
  - `TestCanaryClimbsToTrustedOnItsOwnEvidence`: policy CanaryMatches 2 -> second clean replay moves to trusted.
  - `TestTwoFailedReplaysDemoteATrustedProcedureToShadow` (#5411 acceptance): policy FailuresToDemote 2; one failure stays trusted with Failures 1; second -> shadow, `Demoted=true`, every streak counter zero.
  - `TestOneInsufficientPreconditionDemotesImmediately`: Insufficient=true with InsufficientToDemote 1 -> shadow at once.
  - `TestACleanReplayResetsConsecutiveFailures`.
  - `TestAStartRefusalCountsAsAFailedReplay`: two `EventStartRefused` on trusted -> shadow.
  - `TestTheSweepDemotesOnStoredEvidenceUnderTheCurrentPolicy`: trusted with Failures 3 and policy 2 -> shadow.
  - `TestTheSweepRetiresAProcedureUnusedForTheWindow`: LastUsedAt 31 days before At, RetireAfterDays 30 -> retired, `Retired=true`; 29 days -> unchanged.
  - `TestRetiredIsTerminal`: every event on retired returns it unchanged.
  - `TestNormalizeReplacesEveryNonPositiveValueWithItsDefault`; `TestDefaultLadderPolicyIsTheRecordsValues` (5,2,5,2,1,30).
  - `TestParseRungRefusesAnUnknownValue`.
  - `TestBindingDigestIsSha256Prefixed` (`sha256:` + 64 hex).
- [ ] **Step 2:** `go test ./component/work/ -run 'Ladder|Shadow|Canary|Retire|Rung|Binding|Promotion|Sweep|Normalize|StartRefusal|Insufficient' -v` from `component/work` -- expect FAIL (undefined).
- [ ] **Step 3: implement `ladder.go`** exactly per 1.1 and these rules:
  - `Advance` never mutates its input; it copies `DistinctBindings` (deep) before editing.
  - shadowCompared on a non-shadow rung: unchanged, Reason "only a procedure in shadow is compared beside the app".
  - match: `ShadowMatches++`; for every id in `FreeParameters`, append `BindingDigest(Bindings[id])` to `DistinctBindings[id]` when absent, capping each list at `max(p.DistinctBindings, 8)`; `LastReplayAt=At`; propose when `PromotionApprovalId==""` and `ShadowMatches>=m` and every free parameter has `>=k` digests. Reason when proposing: `"matched the app %d times across %d distinct bindings; promotion to canary is proposed"`.
  - mismatch: zero the streak (`ShadowMatches=0`, `DistinctBindings` empty map), keep `PromotionApprovalId`, `LastReplayAt=At`.
  - promotionDecided: approved on shadow -> canary, zero `CanaryMatches`, `Failures`, `Insufficient`, clear `PromotionApprovalId`; rejected on shadow -> zero the streak, clear the approval id.
  - replayed on canary/trusted: success -> `Failures=0`, `Insufficient=0`, `LastReplayAt=At`; on canary `CanaryMatches++` and at `>=CanaryMatches` -> trusted (zero CanaryMatches). failure -> `Insufficient++` when `e.Insufficient` else `Failures++`; demote when either reaches its threshold (demotion zeroes every counter and the streak and clears the approval id).
  - startRefused on canary/trusted: `Failures++`, demote at threshold. On other rungs: no-op.
  - sweep: demotion re-evaluation first (canary/trusted over threshold -> shadow), then retirement when `!LastUsedAt.IsZero() && At.Sub(LastUsedAt) > RetireAfterDays*24h` on any rung but retired.
  - Every transition sets `From`, `To`, and a one-sentence `Reason`; `To==From` transitions still return the updated state.
- [ ] **Step 4:** run the tests -- PASS.
- [ ] **Step 5: verdicts.** Tests `TestADislikedInstanceStepHoldsACandidateUntilALikedVersionExists` (#5410 acceptance: instance 0 step 1 versions [dislike] -> not ready, reason names instance and step; add [dislike, like] -> ready), `TestANeutralVersionClearsADislike`, `TestAnUnseenVersionDoesNotClearADislike`, `TestFewerThanTwoUsesIsNotACandidate`, `TestAnUnexplainedHoleIsNotACandidate`, `TestEntryRungIsShadowOnlyWhenTheGatePasses`, `TestParseVerdictAcceptsThePastTenseEpicCWrote` (`disliked` -> dislike, `liked` -> like, `DISLIKE` -> dislike, `meh` -> unseen). Implement `verdict.go`. PASS.
- [ ] **Step 6: reliability.** Tests: success from 0 -> 0.2; failure from 1 -> 0.8; 50 successes approach but never exceed 1; NaN/negative input clamps to 0. Implement. PASS.
- [ ] **Step 7: compare.** Tests:
  - `TestExpectationIsExactWhenEveryRecordingAgreed` (same exitCode 0, same type, same write digest at `out/report.txt`).
  - `TestExpectationFallsBackToTypeWhereRecordingsVaried` (two exitCode 0, types string/string, write digests differ -> Contents entry with Digest "" and Exact false).
  - `TestCompareRefusesADifferentExitCode`, `TestCompareRefusesAnErrorWhereTheRecordingsHadNone`, `TestCompareRefusesAMissingWrite`, `TestCompareAcceptsAnyDigestWhereRecordingsVaried`.
  - `TestCompareTreatsAnAbsentObservationAsNotAMatch` (exp.ExitCode set, got.ExitCode nil -> false, reason "exit code not reported").
  - `TestCompareShadowIsExactForADeterministicStep` (app and replay write the same digest -> match; different -> mismatch).
  - `TestCompareShadowComparesByTypeWhereTheRecordingsVaried` (different digests but same type -> match).
  - `TestInferTextTypeMatchesTheCockpit` (`{"a":1}`->object, `[1]`->array, `1`->number, `true`->boolean, `null`->null, `hello`->string, `1 2`->string).
  Implement `compare.go`. PASS.
- [ ] **Step 8: target.** Tests: `TestAWorkspaceRelativeWriteIsPortable` (cwd `/w/run1`, path `/w/run1/out.txt` and `out.txt`), `TestAnAbsolutePathOutsideTheWorkspaceIsMachineLocal` (`/Users/x/notes.md`), `TestAFetchIsExternalAndPortable`, `TestAMachineLocalFootprintSentToTheWorkbenchIsRefusedBeforeTheFirstStep` (#5411 acceptance: `CheckTarget(Footprint{Machine:true}, TargetWorkbench)` is `ErrMachineLocalOnWorkbench`), `TestReplayTargetForAPortableFootprintIsTheWorkbench`. Implement `target.go`. PASS.
- [ ] **Step 9: DecideServe reads the ladder.** Add to `replay_test.go`: trusted -> ServeConstruct no standby; canary -> ServeConstruct + Standby; shadow -> ServeLive + Shadow; candidate/retired/"bogus" -> ServeLive not shadow; a `replay`-mode run with a trusted construct keeps the journal decision; every existing test unchanged. Implement. PASS.
- [ ] **Step 10: the approval.** `TestProcedurePromotionApprovalPinsTheProcedureHash` (ArtifactHash == ProcedureHash, Kind procedurePromotion, RunId == ShadowRunId, subject keys exactly the seven named), `TestValidateApprovalKindAcceptsAProcedurePromotionWithARun` and refuses one without. Implement. PASS.
- [ ] **Step 11:** `cd component/work && go vet ./... && go test -count=1 ./...` -- all PASS; `GOWORK=off go build ./... && GOWORK=off go mod tidy -diff` in `component/work` -- no diff (still a leaf).
- [ ] **Step 12: commit** `git add component/work/<each file>` then `git commit -m "Issue #5410: the certification ladder as a pure state machine, the serve decision, the comparison and the replay target"`.

## Task 2: binding, materialization, preconditions, fitness and alignment (`component/procedure`, #5411 pure half)

**Files:**
- Modify: `component/procedure/value.go` (Node.Form; Clone), `component/procedure/canonicalize.go` (set Form), `component/procedure/symbolize.go` (AntiUnify keeps Form)
- Create: `bind.go`, `preconditions.go`, `fitness.go`, `align.go`, `serialize.go` and a `_test.go` beside each
- Modify: `component/procedure/doc.go` (one paragraph naming the new functions)

**Interfaces:** Produces 1.2 exactly. Stays stdlib-only (`purity_test.go` unchanged and green).

- [ ] **Step 1: Form is a rendering hint.** Tests: `TestFormIsIgnoredByEqual` (two argv arrays with different Form are Equal), `TestCanonicalizeMarksTheFormItParsed` (command -> argv, `{"a":1}` -> json, `/tmp/a` -> rootedPath, `./a/b` -> path), `TestAntiUnifyKeepsTheLeftForm`. Every existing golden test must stay green unchanged (Form is not serialized into any existing golden). Implement. PASS.
- [ ] **Step 2: Bind.** Tests: `TestBindReadsEveryHoleOfAnInstanceTheTemplateFits` (template from two instances `echo hi > a.txt` / `echo hi > b.txt` binds a third `echo hi > c.txt` to `{"s0.command.3": "c.txt"}` -- use the hole ids Generalize actually produces), `TestBindRefusesAnInstanceWhoseLiteralDiffers` (`echo bye > c.txt` -> false), `TestBindRefusesADifferentTool`, `TestBindInstanceRequiresEveryStep`.
- [ ] **Step 3: Materialize.** Tests: `TestMaterializeRoundTripsARecordedCommand` (canonicalize `git commit -m "a b"` then materialize with no holes -> an argv string that `splitArgv` parses back to the same tree), `TestMaterializeRestoresTheLeadingSlash` (`/tmp/x/y` rootedPath), `TestMaterializeReEncodesJSON`, `TestMaterializeFillsAHoleWithItsBinding`, `TestMaterializeRefusesAnUnboundHoleNamingIt` (error text contains the hole id), `TestMaterializeTypesANumberLiteral` (LitType number -> float64 in the output map).
  Quoting rule for argv: an element containing whitespace, a quote or a backslash, or empty, is single-quoted with `'` escaped as `'\''`; everything else is bare.
- [ ] **Step 4: LearnInputMap.** Tests: `TestAFreeHoleEqualToAGoalInputInEveryInstanceMapsToThatKey`, `TestAFreeHoleEqualToAnInputInOnlySomeInstancesStaysUnmapped`, `TestOnlyFreeHolesAreMapped`.
- [ ] **Step 5: preconditions.** Fingerprint fixtures in the cockpit's shape (see Global Constraints). Tests:
  - `TestAPredicateIsLearnedOnlyWhenEveryStartAgreed` (node 22.1.0 in both -> kept; git 2.43 vs 2.44 -> dropped).
  - `TestOnlyToolsTheProcedureInvokesArePreconditions` (usedTools [node] -> git absent).
  - `TestEmptyWorkspaceIsLearnedFromCwdEntries` (both 0 -> true; one 3 -> absent, never false).
  - `TestPlatformAndVariablesAreNotComparedOnTheWorkbench` (learned darwin, observed linux, target workbench -> Held).
  - `TestPlatformIsComparedOnTheMachine` (target machine -> mismatch "platform.os").
  - `TestAToolVersionMismatchIsReported`.
  - `TestALearnedPredicateTheObservationDoesNotCarryIsUnmeasuredAndDoesNotHold`.
  - `TestUsedToolsReadsArgvZeroOfExecSteps`.
- [ ] **Step 6: token replay.** Build a workflow net from the tree (leaf -> labelled transition; seq -> chain; xor -> shared in/out places; and -> silent split/join with a place pair per child; loop(body, redo) -> body between p_in and p_mid, silent exit p_mid->out, redo p_mid->p_in; empty seq -> silent). Replay: for each event, if no transition with that label is enabled, try enabling it by firing silent transitions (BFS over silent firings, bounded at 256 markings); if still not enabled, add the missing tokens. At the end, fire silent transitions toward the final marking; count remaining tokens. Tests: `TestAFittingSequenceHasFitnessOne`, `TestAnExtraEventLowersFitnessAndNamesItsIndex`, `TestAMissingEventLeavesRemainingTokens`, `TestALoopReplaysAnyNumberOfIterations` (Structure over `[a b a b a]`-shaped retries), `TestAChoiceAcceptsEitherBranch`, `TestPrefixFitsWhileTheRunIsIncomplete` (`[s0 s1]` against seq(s0,s1,s2) fits; `[s0 x]` deviates at 1).
- [ ] **Step 7: alignment.** Dijkstra over (marking, trace index); sync cost 0, silent 0, log move 1, model move 1; state cap 100000 (return the best partial with Cost -1 when exceeded, and a test pins that). Tests: `TestAFittingTraceAlignsWithZeroCost`, `TestAnInsertedEventIsALogMove`, `TestASkippedStepIsAModelMove`, `TestASubstitutionIsOneLogAndOneModelMove`, `TestFirstDeviationNamesTheEarliestNonSyncMove`.
- [ ] **Step 8: AssignSymbol + SequenceTree + serialization.** Tests: an action is assigned the symbol it was clustered into by `Symbolize` on the same corpus; an unrelated tool assigns nothing; `MarshalTemplate`/`UnmarshalTemplate` round-trip every node field including Form, LitType, HoleId, HoleType and every hole field; `MarshalTree`/`UnmarshalTree` round-trip.
- [ ] **Step 9:** `cd component/procedure && go vet ./... && go test -count=1 ./...` (includes `purity_test.go`) -- PASS; `GOWORK=off go build ./...` -- OK.
- [ ] **Step 10: commit** `Issue #5411: binding, materialization, learned preconditions, token-replay fitness and alignment in the pure module`.

## Task 3: the ladder as rows, its values as rows, the approval kind (#5409)

**Files:** everything listed in 1.3, plus:
- `test/dslconformance/server_only_parsed_test.go` (`want` entries with written reasons), and `server_only_callers_stamp_test.go` if a Go caller list needs it.
- `component/automations/strict_automation_boot_test.go` (`shippedAutomationCount` + 4, with a history line).
- `component/auth/maintenance_actor_gate_test.go` (sorted pin).
- `embed_inventory_test.go` (a new `dsl/authoring/seeds.memql` moves the dsl count -- MEASURE from the failure message).
- `component/conceptfields/concept-fields.snapshot.json` (`make concept-snapshot`).
- `sdk/go/client/generated_*.go`, `sdk/ts/src/client/generated_*.ts` (`make sdk-gen`).
- `component/memql/app_resource_os_parity_test.go` stays green once Task 6 adds `requires: "app:settings/procedures"`; until then the seed row has no requirer -- so Task 3 ALSO adds the Settings manifest entry `{ id: "procedures", name: "Procedures", requires: "app:settings/procedures" }` in `clients/os/src/apps/registry.tsx` with a placeholder branch that Task 6 fills, OR defers the seed row to Task 6. Choose deferring the seed row to Task 6 (one owner per file); record it in the commit body.

- [ ] **Step 1:** Read `docs/public/language/authoring-rules.md` and `dsl/_reference/_concept.memql`, `_logic.memql`, `_automation.memql` before writing any construct. Grep every new construct name across `dsl/` for collisions (`procedureStep`, `procedureReplay`, `procedureLadderSweep`, `procedureDecidePromotion`, `replayLearnedProcedure`, `demoteProcedures`, `retireProcedures`, `onProcedurePromotionDecided`, `recordProcedure`, `recordConstructLadder`, `createLadderPolicy`, `ladderPolicyCurrent`, `learnedProceduresForOwner`, `procedureConstructByName`, `procedureConstructsForGoalSignature`, `workRunsForOwnerGoalSignature`, `procedureCard`, `ladderPolicyFull`).
- [ ] **Step 2:** Write the concept fields, the `ladderPolicy` concept, the seed, the mutations, the queries, the shapes, the work-domain changes and the procedure-domain builtins/automations exactly as 1.3 says. Every doc comment explains WHY (house style: the reason a reader needs, not the obvious). The `procedure` object field's description is the JSON contract in 1.3.
- [ ] **Step 3:** `go run ./cmd/memqlmigrate --rewrite=doc-comment-descriptions,accept-stamp,required-sigil,same-domain-use -w dsl/authoring/ dsl/work/ dsl/procedure/` then `git diff --stat` -- the codemods must leave nothing to change.
- [ ] **Step 4:** `go run ./cmd/memqllint dsl/` -- clean.
- [ ] **Step 5:** `make sdk-gen && make sdk-gen-check`; `make concept-snapshot` (must add, never retire); `make arch-model` only if Go call sites moved.
- [ ] **Step 6:** maintenance entries + gate pin; `shippedAutomationCount`; server-only `want` entries; embed count.
- [ ] **Step 7:** `go test -count=1 ./component/auth/ ./component/automations/ ./test/dslconformance/ ./component/conceptfields/ .` and `make test` (capture: `make test > /tmp/.../maketest.log 2>&1; echo MAKE_EXIT=$?`, then `grep -E '^(FAIL|--- FAIL)'`) -- green except the known shared-database drift set, which must be re-measured on `origin/main` before being waved through.
- [ ] **Step 8: a seed-materializer test** in `component/memql` (db-gated, beside the existing seed tests): the `ladderPolicy primary` seed materializes at `v1:authoring:ladderPolicy:primary` with the six values, and a second materialization after an edit re-asserts them (refresh on boot). `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql_pcr?sslmode=disable go test -count=1 -run LadderPolicy ./component/memql/`.
- [ ] **Step 9: commit** `Issue #5409: the ladder as construct fields, its values as a seeded singleton, and the procedurePromotion approval kind`.

## Task 4: close epic C's gaps on this epic's path, and lift onto the ladder (#5409, #5411)

**Files:** `integrations/procedure/{corpus,learn,persist,store,render,integration}.go` + tests; `app/plugins_core.go`; `app/integrations_procedure.go` (new: installs the compile gate and the workbench dispatcher/prober); `integrations/planner/work_compile_adapter.go` (write `goalSignature` on the run); `integrations/agent/worker/app_session_delegate.go` (child run carries `goalSignature`, `parentRunId`, and the parent run's `variables`); `integrations/work/session.go` (a run the writer opens itself carries the same when the request names them); `component/worker/recording.go` only if `RecordingRequest` needs the fields.

**Interfaces:** Consumes Task 1 (`EntryRung`, `CandidateEvidence`, `ParseVerdict`, `ExpectationFrom`, `ActionFootprint`, `ReplayTargetFor`, `Reinforce`), Task 2 (`Form`, `LearnInputMap`, `LearnPreconditions`, `UsedTools`, `SequenceTree`, `MarshalTemplate`, `MarshalTree`), Task 3 (DSL). Produces the stored `construct.procedure` / `preconditions` / `procedureHash` / ladder entry, and the `LearnFromRun` export.

- [ ] **Step 1 (G1, G7): the plugin ships and Gate 1 runs.** Blank-import `integrations/procedure` in `app/plugins_core.go`. Add `SetCompiler` and install `&CognitionEngineAdapter{Engine: engine}` (or the existing adapter instance) from a new `app/integrations_procedure.go`, found through `engine.IntegrationByName("procedure")`. Test `app` (untagged): `TestTheProcedureIntegrationShipsWithItsCompileGate` builds the app's integration set the way the other wiring tests do and asserts the integration is registered and `compiler != nil`. Fix `module_taxonomy_test.go` / call-origin allowlist only if they fail, with the reason.
- [ ] **Step 2 (G2): runs carry the goal signature.** `WorkCompiler.Compile` adds `goalSignature: out.Signature` to the run write. `beginChildRun` reads the parent run (owner actor) and writes `goalSignature`, `parentRunId`, `variables` onto the child. `SessionWriter.OpenRecording` does the same when its request names a parent. Tests: planner `TestCompileStampsTheGoalSignatureOnTheRun`; agent-tagged `TestADelegatedSessionRunCarriesItsParentsGoalSignature` (`go test -tags agent ./integrations/agent/worker/ -run GoalSignature`).
- [ ] **Step 3 (G3, G4): arguments and observables come from the observation.** `loadSteps` reads `workObservationsForOwnerRun(runId)` once per run, joins `tool_result` rows by `stepKey`, parses `data.args` (JSON string; a value that is not a JSON object is kept as `{"_raw": value}`), and builds a `work.StepObservation` per step: `isError`, `exitCode`, `resultType`, contents from `contentRefs` (read each file row's `sha256` and `name` under the owner; the path is recovered from the name the recorder composed -- read `contentFileName` in `component/worker` and invert it; add an exported inverse there if there is none) plus `contentOmitted` entries (`"<path>: ... (sha256 <hex>)"`). The first step's `fingerprint` is read from the step row (Task 3 made it writable). Corpus read switches to `workRunsForOwnerGoalSignature`. Tests with a fake engine answering those queries: `TestTheCorpusReadsArgumentsFromTheObservation`, `TestTheCorpusResolvesContentDigestsFromTheLibrary`, `TestARunWithNoActionsAtTheLevelIsSkipped`.
- [ ] **Step 4 (G9): `literal()` renders maps and slices as MemQL literals** (objects `{k: v}` with keys sorted and validated as MemQL names, arrays `[...]`), recursively; test `TestLiteralRendersAMapAsAnObject` and that `recordBundleValidation`'s call text parses (`langparser` parse of the rendered statement).
- [ ] **Step 5 (G8): the source renders every step as a real statement.** New `render.go`: `renderProcedureSource(name, title string, t proc.Template, freeParams []proc.Hole, prov planner.TemplateProvenance) string` emitting the provenance comment (reuse `planner`'s exported pieces or move `provenanceComment` behind an export), `@description`, `automation <name> {`, the args block for free parameters, and one line per step: `  call<N> := builtin procedureStep(step: <N>, tool: "<tool>", args: {<rendered args object>})`. Holes render as in `planner.templateValue` (free -> `args.<param>`, constant -> literal, data-flow -> `call<k>.<path>`); a hole with no spelling -> the step renders as a comment and `everyStepWritten=false`. Golden test in `render_test.go` over a two-step template; Gate 1 test with the real sandbox compile where available (skip with a named reason otherwise).
- [ ] **Step 6: the procedure payload.** In `persist.go`, after the template is accepted, build `construct.procedure` per 1.3: steps (tool, args as `MarshalTemplate` node JSON, symbol), holes, `expect` (per template step `work.ExpectationFrom` over that step's observation in every instance), `inputMap` (`proc.LearnInputMap` with each instance's goal input = the recording run's `variables`), `freeParameters`, `footprint` (union of `work.ActionFootprint` over every instance action, with paths from args and contents), `target` (`work.ReplayTargetFor`), `symbols`, `model` (`proc.Structure` over the corpus sequences -> `MarshalTree`), `recordedFrom` (app, model, effort, sessionIds from the recordings' `v1:worker:appSession` rows whose `sessionRunId` is the run, and the transcript file's `producedBy`; runIds), `title` (the parent goal's statement, via `parentRunId` -> run -> goal), `goalSignature`, `inputKeys`. `preconditions` = `proc.LearnPreconditions(fingerprints, proc.UsedTools(t))`. `procedureHash` = `"sha256:" + hex(sha256(canonical JSON of {source, procedure, preconditions}))` (canonical = sorted keys, no insignificant whitespace; one helper, tested).
- [ ] **Step 7 (G15) idempotent lift, and ladder entry.** Look the construct up with `procedureConstructByName(name)` under the owner. Absent -> create bundle + construct (as today) then `recordProcedure`. Present with the SAME `procedureHash` -> no write at all (a re-mine of an unchanged corpus is free and keeps every counter). Present with a different hash -> `recordProcedure` with the new payload and reset the ladder to the entry rung (D15: "a later change of the construct is a new candidate"); if a `promotionApprovalId` was open, leave the approval row alone (its hash no longer matches, so deciding it refuses). Then Gate 1, validation, goal signature (only when re-runnable), and `recordConstructLadder` with `work.EntryRung(evidence)` where evidence = uses, unexplained holes (0 after demotion to free), and per-instance per-step verdicts read from `feedback` observations on each recording run (`data.verdict` via `work.ParseVerdict`, `data.target.stepKey` / `data.target.version`; a feedback row naming the RUN excludes the recording from the corpus as epic C does, a row naming a STEP feeds the gate). `ladderReason` is the gate's reason; `ladderChangedAt` now.
  Tests (fake engine): `TestARelearnOfAnUnchangedCorpusWritesNothing`, `TestAChangedProcedureIsANewCandidate`, `TestALiftedProcedureEntersShadowWhenTheGatePasses`, `TestADislikedInstanceStepHoldsTheLiftAtCandidate`, `TestTheProcedureHashCoversSourceProcedureAndPreconditions`, `TestTheLiftWritesTheProcedurePayloadTheRunnerNeeds` (every 1.3 key present).
- [ ] **Step 8 (G10): the automations can act.** `handleLearnFromRun`: when the caller is a synthetic system actor (`auth.AccessFromContext(ctx).Synthetic`) it is not "another person"; read the run through a maintenance-safe read (the automation is now in `maintenanceAutomations`, so `workRunById` answers) and then do every owner read under the owner's actor. `handleMineCorpus` with a blank owner AND a synthetic caller iterates owners (`listUserIds` or the query the seed sweep uses) and mines each one's signatures; a blank owner from a person is still refused. Tests for both, including that a person naming another owner is still refused.
- [ ] **Step 9:** `go test -count=1 ./integrations/procedure/ ./integrations/planner/ ./app/` and `go test -count=1 -tags agent ./integrations/agent/worker/` -- PASS; `go vet -tags agent ./integrations/... ./app/...`.
- [ ] **Step 10: commit** in two commits: `Issue #5409: epic C's wiring ships -- the plugin, the goal signature on runs, arguments from observations, Gate 1, real statements` and `Issue #5409: the lift is idempotent and writes the procedure, its preconditions and its hash, and enters the ladder`.

## Task 5: the replay runner, shadow, serving, the approval, the sweeps (#5410, #5411)

**Files:** `integrations/procedure/{replay,shadow,ladder,serve,sweep,dispatch,fallback,probe}.go` + tests; `integrations/planner/work_compile.go`, `work_compile_adapter.go` + tests; `integrations/work/approval.go` + test; `app/integrations_procedure.go` (workbench dispatcher + prober), new `app/integrations_procedure_agent.go` (`//go:build agent`: machine dispatcher + app fallback) + tests.

**Interfaces:** Consumes 1.1, 1.2, 1.3, Task 4's payload. Produces 1.4.

- [ ] **Step 1: ladder IO** (`ladder.go`): `readLadder(ctx, owner, constructId) (work.LadderState, row, error)` and `writeLadder(ctx, owner, constructId, work.Transition)` -> `recordConstructLadder`; `readPolicy(ctx, owner)` -> `ladderPolicyCurrent`, absent/unreadable -> `work.DefaultLadderPolicy()`, always `.Normalize()`; `reinforce(ctx, owner, constructId, success)` -> `recordConstructReliability` with `work.Reinforce` (FIRST production caller; reinforceCount++ and lastReinforced on success only). Tests with a recording fake engine asserting the exact call text.
- [ ] **Step 2: the runner** (`replay.go`), in this order, each a named function with its own test:
  1. load the construct (owner actor) and its `procedure`/`preconditions`; refuse a construct with no procedure.
  2. rung gate: shadow mode needs rung shadow; canary/trusted mode re-reads the rung and, when it is no longer canary/trusted (demoted or re-lifted since compile), `StartRefused` with reason and FALL BACK (canary/trusted) -- `TestAProcedureDemotedSinceCompileFallsBackAtExecution`.
  3. target: `work.ReplayTargetFor(footprint)`; shadow of a machine-local procedure runs DRY (no dispatch: materialized args compared with the app's recorded args step by step); `work.CheckTarget` before the first step -- `TestAMachineLocalFootprintSentToTheWorkbenchIsRefusedBeforeTheFirstStep` (no Dispatch call recorded).
  4. bindings: shadow uses `req.Bindings`; canary/trusted resolves every free parameter through `inputMap` from `req.Input`; an unbound parameter is `StartRefused` + fallback -- `TestTrustedReplayWithAnUnboundParameterFallsBackBeforeTheFirstStep`.
  5. preconditions: `Prober.Probe` then `proc.CheckPreconditions(learned, observed, target)`; no prober with learned predicates -> unmeasured -> refuse; mismatch -> `StartRefused`, ladder `EventStartRefused`, fallback, and the mismatch recorded on the replay run's outcome -- `TestATrustedProcedureWhoseFingerprintMismatchesFallsBackToTheAppAndRecordsTheMismatch` (#5411 acceptance: fallback called once; outcome.StartRefused; the replay run row's `outcome` names the mismatch; ladder Failures+1).
  6. open the replay run (owner actor, internal origin): `automationName` = construct name, `mode` "live", `triggeredBy` `procedure:<mode>`, `goalSignature`, `parentRunId` = the goal run (canary/trusted) or recording run (shadow), `templateConstructId` = the construct id (it IS what ran). One step row per template step keyed `step<N>`, `kind` deterministic, `idempotencyKey` `work.IdempotencyKey(runId, key, 1)`, written `running` before dispatch and with a receipt after.
  7. per step: materialize (`proc.Materialize`); content-addressed input check (a step whose recorded args were identical across instances must materialize to the same canonical JSON digest); skip a step whose receipt already exists for this run (never re-dispatch -- `TestAStepWithAReceiptIsNeverDispatchedAgain`); dispatch; compare (`work.Compare` against `expect`, or `work.CompareShadow` against `req.AppActions[i]`); append symbol (or `symbol + "!"` on a mismatch) to the live trace; `proc.PrefixFits` on the procedure's `model`; on the first mismatch: `proc.Align` for the diagnosis, STOP.
  8. divergence (canary/trusted): outcome `Diverged`, `Insufficient` (every precondition held), `Completed` steps with idempotency keys and whether each had a side effect (`Footprint.IsSideEffect()` of that step, or `DispatchResult.Delivered`); ladder `EventReplayed{Match:false, Insufficient}`; `reinforce(false)`; FALL BACK with `Guidance` whose `Prompt` renders: the goal statement, then "A learned procedure already ran these steps -- do not repeat them:" with one line per completed step (tool, summary, idempotency key), then "It stopped at step N because: <diagnosis>". The fallback's `ChildRunId` is written onto the replay run's outcome as `repairRunId` (the repaired run is recorded by epic B as a new recording). `TestADivergenceHandsTheAppThePartialTraceAndNeverRedoesACompletedStep`.
  9. success (canary/trusted): outcome `Served`; ladder `EventReplayed{Match:true}`; `reinforce(true)`; run `succeeded` with `outcome {servedBy: "procedure", constructId, rung, steps}`.
  10. shadow: every step compared; outcome `Match`; ladder `EventShadowCompared{Match, Bindings, FreeParameters}`; `reinforce(Match)`; when the transition `Propose`s: raise `work.ProcedurePromotionApproval` through `integrations/work`'s approval writer (`RaiseApproval`, owner's borrowed actor) and write its id into `promotionApprovalId` -- `TestAShadowMatchingMTimesAcrossKBindingsRaisesExactlyOnePromotion`.
  Every test uses fake `Dispatcher`, `Prober`, `AppFallback` and a recording fake engine; none needs a database.
- [ ] **Step 3: shadow on a succeeded recording** (`shadow.go`, called from `handleLearnFromRun` AFTER the lift): read `procedureConstructsForGoalSignature(sig)` under the owner; for each construct on shadow: symbolize the recording's actions with the construct's `symbols` (`proc.AssignSymbol`), find the aligned instance (`proc.BindInstance` over the template), build `Bindings` and `AppActions` from the recording's observations, and `Replay(mode shadow)`. A recording the template does not fit is a MISMATCH (resets the streak) -- `TestARecordingTheProcedureDoesNotFitIsAShadowMismatch`. The recording that produced a relift is compared against the relifted version only when the hash is unchanged.
- [ ] **Step 4: serving from compile** (`integrations/planner/work_compile.go`): `cataloguedForSignature` also reads `procedureConstructsForGoalSignature`; for each procedure row, `work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})`; keep only `Source == ServeConstruct` rows as exact candidates carrying `Procedure: true` (add the field to `work.CatalogCandidate` in Task 1 if cleaner -- coordinate by editing 1.1 first) ranked trusted before canary, then reliability. `finishCompile` on a procedure candidate: `AutomationName = "replayLearnedProcedure"`, `ConstructId = ""`, `Variables = {"procedureConstructId": <id>}`; `WorkCompiler.Compile` merges `out.Variables` over `req.Input` into the run's `variables` and never writes `templateConstructId` for this route. Tests: `TestATrustedProcedureOnAnExactHitIsServedByTheReplayAutomationWithNoModel` (counting provider: zero calls), `TestAShadowProcedureIsNotServedAndTheAppRuns` (falls through to triage), `TestACandidateOrRetiredProcedureIsNeverServed`.
- [ ] **Step 5: the serve builtin** (`serve.go`): `handleProcedureReplay` reads the current run from `common.RunFromContext(ctx)` (refuse outside a work run), the run's owner and `variables` and goal (owner actor); refuses a construct owned by someone else; calls `Replay` with mode trusted/canary per the CURRENT rung (DecideServe), `GoalRunId` = the run, `Input` = the goal's `input`. Returns `{servedBy: "procedure"|"app", constructId, rung, diverged, repairRunId}`. `handleProcedureStep` always refuses with the sentence in 1.3. Tests for both, and that a person calling `procedureReplay` on another person's construct is refused.
- [ ] **Step 6: deciding the promotion.** `integrations/work/approval.go`: `currentArtifactHash` for `procedurePromotion` reads `authoringConstructById(subject.constructId)` under the owner and returns its current `procedureHash` (so a changed construct refuses with the existing artifact-changed error); `resumeParkedRun` is SKIPPED for this kind. `integrations/procedure` `DecidePromotion(approvalId)` (the `onProcedurePromotionDecided` automation's handler): read the approval and the construct under the owner; if `promotionApprovalId` matches and the rung is shadow, `Advance(EventPromotionDecided{Approved: decision == "approved"})` and write it; otherwise no-op (idempotent: a second delivery sees canary). Tests: `integrations/work` `TestApprovingAPromotionAfterTheConstructChangedIsRefused`, `TestDecidingAPromotionNeverTouchesTheShadowRun`; procedure `TestAnApprovedPromotionMovesShadowToCanaryOnce`, `TestARejectedPromotionKeepsShadow`.
- [ ] **Step 7: sweeps** (`sweep.go`): `Sweep(ctx, "demotion"|"retirement", now)`: refuse unless the caller is a synthetic maintenance actor or internal; iterate owners; per owner read `learnedProceduresForOwner` (all pages), `Advance(EventSweep{At: now, LastUsedAt: max(lastReplayAt, createdAt)})`, write only changed rows. Retirement leaves `status` alone and sets `ladder: retired` (the compile read excludes it). Tests: `TestTheDemotionSweepDemotesStoredEvidence`, `TestTheRetirementSweepRetiresAnUnusedProcedure`, `TestASweepReadsEveryOwnerUnderTheirOwnActor` (the fake engine records the actor per query), `TestAPersonCannotRunTheSweep`.
- [ ] **Step 8: production seams.**
  - Workbench `Dispatcher` + `Prober` (untagged, `app/integrations_procedure.go`): call the workbench integration's `dispatchHost` capability handler exactly as `app/integrations_skills.go` `dispatchHostHandler` does; tool -> action map `exec->exec`, `fs_write->fs_write`, `fs_read->fs_read`, `fetch->http_fetch`; `mcp` -> execute the named MemQL tool as the owner (`ExecuteToolByName`) -- refused in shadow (`Sandbox`) unless the tool is a query. Observation: exec -> exitCode + `work.InferTextType(stdout)` + isError(exitCode != 0); fs_write -> write content digest = sha256 of the bytes written; fs_read -> read content digest. Shadow runs use a DISTINCT workspace (the replay run id) so nothing the app did is visible to it. Prober: `platform` via `uname -s`/`uname -m` (lowercased to Go names), `tools.<n>` via `<n> --version` first line, `emptyWorkspace` via `fs_list` of the workspace root (0 entries). Tests with a fake handler.
  - Machine `Dispatcher` + `AppFallback` (`//go:build agent`, `app/integrations_procedure_agent.go`): machine dispatch through `agentworkerDispatchHost`'s handler with `agentId` = the owner's reasoning agent (the same resolution `integrations/planner` uses for `reasoningAgent`; expose it rather than copy it) and the run/step ids; the fallback through the router's session door (a `core/airoute.ResolveRequest` at level `reasoning`, tools modality, `RunId`, `StepId` = the CANONICAL journal step id of the replay statement, `UserId` owner, `AgentId` as above, messages = the guidance prompt) -- never a direct provider-registry accessor (the one-seam AST gate). When the router refuses, the outcome is `FellBack=false` with the refusal code and the run fails `procedure_fallback_unavailable` with the router's words. Wiring test `TestTheAgentNodeInstallsTheMachineDispatcherAndTheAppFallback` (agent tag).
- [ ] **Step 9:** full local lanes for the touched trees: `go test -count=1 ./integrations/procedure/ ./integrations/planner/ ./integrations/work/ ./app/`; `go test -count=1 -tags agent ./integrations/agent/worker/ ./app/`; `go vet` per tag (agent, planner, workbench, mcp, bff, edge, identity); `go test -count=1 -timeout=300s .` (root gates: internal-origin allowlist, plugin taxonomy, staged-read classification, DSL call strings, one-seam router gate); db-gated: `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... go test -count=1 ./integrations/work/`.
- [ ] **Step 10: commits** per concern: runner, shadow, serve+compile, approval, sweeps, production seams.

## Task 6: MemQL OS -- Nexus shows the ladder, the approval card, the provenance; Settings shows the values (#5412)

**Files:** `clients/os/src/apps/nexus/{AutomationsSection.tsx,automations.ts,useAutomations.ts,ApprovalsSection.tsx,words.ts,rows.ts,actions.ts}` and new `clients/os/src/apps/nexus/ProcedurePage.tsx`, `clients/os/src/apps/nexus/ladder.ts`; `clients/os/src/apps/settings/{SettingsApp.tsx,ProceduresSection.tsx,useLadderPolicy.ts}`; `clients/os/src/apps/registry.tsx`; `clients/os/src/styles/index.css` (append `.os-nexus-ladder*`, `.os-settings-procedures*` blocks only); `dsl/rbac/seeds.memql` (`read app:settings/procedures`); tests in `clients/os/test/nexus/` and `clients/os/test/settings/`.

**Before writing a line:** invoke `frontend-design:frontend-design`; read `clients/os/DESIGN.md`, `clients/os/SUPERVISED-VISUAL-COMPOSITION.md`, `clients/os/README.md` sections on live collections, absent-vs-zero, refusals, unseen changes; read `AutomationsSection.tsx`, `ApprovalsSection.tsx`, `words.ts`, `test/nexus/harness.tsx`.

Design (clean, minimal, full function):
- **Automations list** reads `cataloguedConstructsForOwner` AND `learnedProceduresForOwner`, one list in `RecordRow` language. A learned procedure's row: title = `procedure.title` (fallback: "Procedure learned from N runs"), origin fact "Learned" (authored rows "Authored"), state word = the ladder word (`Candidate`, `Shadow`, `Canary`, `Trusted`, `Retired`), one quiet evidence line ("3 of 5 matches beside the app", "Needs a second binding", "Promotion waiting for you", "2 clean replays of 5", "Replays without a model", "Unused for 30 days"), freshness from `lastReplayAt`. Origin is a Refine facet, never a heading (DESIGN.md "a subset is a filter"). The ladder is a WORD, never a percentage (README).
- **Procedure page** (`ProcedurePage.tsx`, replacing the list with `<- Automations` in the Head per rule 11): Head = title; `meta` = rung word. Panels (Panel + Subhead + Facts):
  1. **Ladder** -- a horizontal rail of the four climbing rungs (Candidate, Shadow, Canary, Trusted) with the current one accented and a text state ("Proposed", "Running beside the app"), retired shown as a separate quiet note when it applies; under it one sentence of what the next rung needs, computed from the policy row (`shadowMatches of m across k bindings`) -- the rail is keyboard-reachable text, distinguishes state by text/icon not colour alone, respects reduced motion (no pulses).
  2. **Evidence** -- Facts: Shadow matches, Distinct bindings per parameter, Canary replays, Failed replays, Last replay, Reliability as a word only if present ("Proven", never a number), Why the ladder last moved (`ladderReason`).
  3. **Recorded from** -- the provenance stamp (D9): App, Model, Effort, Sessions (each `CopyValue`), Recordings (links to their runs), Version (`procedureHash` short + `CopyValue`).
  4. **Preconditions** -- the learned initiation set as Facts ("node 22.1.0", "Starts in an empty workspace"); "Nothing learned" when empty.
  5. **Steps** -- one row per step: tool word + the argument skeleton with holes shown as named chips (a free parameter named by its goal input key when mapped).
  ActionBar (rule 12): state words left; acts legal from state only: `Review promotion` (primary, when `promotionApprovalId` is set; opens the approval in the Approvals section via the existing `openApproval`). No disabled acts; nothing else changes the procedure's state from here.
- **Approval card** for `procedurePromotion` in `ApprovalsSection.tsx`: kind word "Promotion", meaning sentence "A learned procedure matched the app often enough to run for real, with the app standing by."; a dedicated panel "What you are promoting": Procedure (link to its page), Construct version (`procedureHash`, `CopyValue`), Artifact hash (`artifactHash`, `CopyValue`) -- #5412 acceptance: both are ON the card --, Evidence (matches, bindings per parameter), Recorded from (app, model, effort). Approve / Reject stay on the one ActionBar; the server's refusal (artifact changed) shows verbatim in place.
- **Settings > Procedures** (`requires: "app:settings/procedures"`): Head "Procedures"; Panel "Certification ladder" with Facts in plain words: "Shadow matches before promotion is proposed: 5", "Distinct bindings each parameter needs: 2", "Clean canary replays before a procedure is trusted: 5", "Failed replays that demote it: 2", "Preconditions proved insufficient that demote it: 1", "A procedure unused this long retires: 30 days"; an `InfoDetail` explaining the ladder in four sentences; a quiet note that the values are the cluster's seeded policy and refresh on every boot. Absent row -> "This cluster has not published its ladder policy yet" (never invented numbers).
- `words.ts` also gains the missing `routingReview` word (it currently falls through to the raw value).
- Unseen changes: declare ONE `attentionChanges` marker on Nexus for the new procedure page (id `nexus:procedures`, revision 1, section automations, label "Learned procedures") with a reachable acknowledgment destination, per README "Unseen changes".

Steps:
- [ ] **Step 1: failing tests** in `test/nexus/automations.test.tsx`, `test/nexus/procedures.test.tsx`, `test/nexus/approvals.test.tsx`, `test/settings/procedures.test.tsx`: the merged list shows a learned row with its ladder word and origin; Refine origin facet filters; the page renders every panel from a fixture row, the rail names the current rung, the next-rung sentence uses the policy's numbers; `Review promotion` exists only with an open approval and opens it; the approval card shows the construct version and the artifact hash; Settings renders the six values and the absent-row sentence; the manifest `requires` and `roleOpens(role, "app:settings/procedures")` agree (owner/admin true, viewer per the seeded roles). Add `learnedProceduresForOwner` and `ladderPolicyCurrent` to `harness.tsx` / the settings harness.
- [ ] **Step 2:** implement; keep every existing act reachable (count buttons by label before and after, per the memory note on moved controls).
- [ ] **Step 3:** `cd clients/os && npx vitest run test/nexus test/settings` then ONE `make os-test`, `make os-typecheck`, `make os-build` at the end (serialise: `npm ci` runs in each).
- [ ] **Step 4: pixels.** Build a throwaway QA harness (`clients/os/qa/`, deleted before commit): `vite.qa.config.ts` with an `enforce: "pre"` `resolveId` plugin swapping `src/live/connection.tsx` on the RESOLVED path for a fake whose `executeNamed` answers the new queries from fixtures (a module-level singleton connection), `setRoleLadder(SEEDED_LADDER)` (never import `test/seededAccess.ts` in the browser), `?layout=desktop`, `?theme=light|dark`, fonts allowed through `server.fs.allow` of the repo root, its own `cacheDir`. Capture with `google-chrome --headless=new --disable-gpu --no-sandbox --hide-scrollbars --force-device-scale-factor=2 --user-data-dir=$(mktemp -d) --window-size=W,H --virtual-time-budget=16000 --screenshot=...`: Nexus Automations (empty and populated), the procedure page for each of shadow (with a pending promotion), canary, trusted and retired, the promotion approval card, and Settings > Procedures (populated and absent) -- light and dark, 1400x900 and 760x900 and one short 1400x640. Read every capture; fix what the pixels show (trail row, measure, ActionBar, contrast, truncation); re-capture. Save the final set under the scratchpad and list them in the PR body.
- [ ] **Step 5: commit** `Issue #5412: Nexus shows where a learned procedure stands and the one approval it needs; Settings shows the ladder's values` (+ the rbac seed row).

## Task 7: proving -- two headline figures with controls, and the program-wide scenario (#5413)

**Files:** `component/proving/figure/figure.go` (+ test), `component/proving/{figures.go,suite.go,runner.go}`, new `component/proving/procedure_lifecycle.go` (+ test), `component/proving/scenario/scenario.go` (+ test), `test/proving/scenarios/*.json` (new), `cmd/memql-bench/main.go` (only if it must import `integrations/procedure`), `docs/public/overview/proving-scorecard.md` (regenerated), `docs/public/overview/why-memql-harness.md` (the stale "does not make yet" row about replay, rewritten to what is now measured).

- [ ] **Step 1: metrics.** Register `amortizedCost.replaysServedWithoutModel` (unit replays, HigherIsBetter, Blocking, Means "Goals a trusted learned procedure answered with no model and no app call.") and `durability.duplicatedSideEffectsAcrossDivergence` (count, LowerIsBetter, Blocking, Means "Side effects delivered twice when a replay diverged and the app took over. Must be zero."). `figure_test.go` wording/blocking rules stay green.
- [ ] **Step 2: direction-aware controls.** `CorpusControls` demands a control for every Blocking metric: Lower -> a control reading NON-ZERO (unchanged), Higher -> a control reading ZERO. `checkNegativeControl` implements both; the existing controls keep passing (test). A zero-reading control for a Higher metric is measured on the PLATFORM arm (it asserts the platform does not claim a replay it did not make).
- [ ] **Step 3: the lifecycle driver** (`procedure_lifecycle.go`, package `proving`, selected by a new scenario field `"procedure": {...}`): with the real engine the lane already opens, a real `integrations/procedure.New(engine, logger)` with fake `Dispatcher` over the proving `world`, fake `Prober`, and a fake `AppFallback` that is the FIXTURE APP (it performs the goal's actions in the world, emits normalized action events recorded through `integrations/work.NewSessionWriter`, and counts one model reach per call). Phases, each asserted: record two sessions with two bindings -> `LearnFromRun` -> construct on shadow -> two more fixture-app goals with two new bindings, each followed by `ShadowCompare` (policy row set to m=2,k=2 for the scenario) -> promotion approval raised -> decide approved through the real `integrations/work` decide handler -> `DecidePromotion` -> canary -> one canary replay (policy CanaryMatches=1) -> trusted -> one trusted replay: served, `ModelCalls==0`, fallback never called. Arm result fields: `ReplaysServedWithoutModel`, `AppCalls`, `DuplicatedAcrossDivergence`. Baseline arm: no ladder; every goal goes to the fixture app.
- [ ] **Step 4: scenarios** (`test/proving/scenarios/`):
  - `amortizedCost.a-trusted-procedure-replays-without-a-model.json` -- claims `amortizedCost.replaysServedWithoutModel` (platform 1, baseline 0) and `amortizedCost.providerCalls` for the trusted goal (platform 0).
  - `amortizedCost.control-a-shadow-procedure-serves-no-goal.json` -- no approval is given; `negativeControlFor: amortizedCost.replaysServedWithoutModel`; platform reads 0.
  - `amortizedCost.control-a-fresh-goal-reaches-a-model.json` -- a goal with a different statement on the same fixture; `negativeControlFor: amortizedCost.providerCalls`; baseline and platform both reach the fixture app (>= 1) -- #5413 acceptance "a fresh goal on the same fixture reaches a model".
  - `durability.a-divergence-duplicates-no-side-effect.json` -- a trusted replay whose second step's world answer is injected to diverge after the first step delivered a side effect; claims `durability.duplicatedSideEffectsAcrossDivergence` (platform 0: the guidance names the completed step and the fixture app honours it).
  - `durability.control-a-divergence-without-guidance-duplicates.json` -- `negativeControlFor` that metric; the baseline arm's app redoes the delivered step -> 1.
  Every scenario that reaches the fixture app declares so where `needsModel` requires it, with cassettes only if the corpus tests demand them (the fixture app is not a provider; state which in the scenario's description).
- [ ] **Step 5:** `go test -count=1 ./component/proving/...` (db-free) -- PASS; with the epic database: `MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql_pcr?sslmode=disable go run ./cmd/memql-bench --do=gate --runner=local` -> one JSON line with `ok: true`; `go run ./cmd/memql-bench --do=scorecard` then `--do=scorecard --check` -> clean; `go test -count=1 -run 'Claims|Scorecard' .` -> PASS.
- [ ] **Step 6: commit** `Issue #5413: replaysServedWithoutModel and duplicatedSideEffectsAcrossDivergence with their controls, and the program-wide lifecycle scenario`.

## Task 8: documentation, the full sweep, delivery

- [ ] Root `CLAUDE.md` is over its ~150k-character budget and a peer is moving per-epic write-ups to `docs/internal/design/feature-notes.md`. Rebase first; if that file exists on main, the write-up goes THERE and CLAUDE.md gets only a few lines plus a link. Either way, the root text carries only the four rules that reach outside the epic: (1) the ladder is values in `v1:authoring:ladderPolicy:primary`, refreshed at boot; (2) a procedure is SERVED only through the ladder (`procedureConstructsForGoalSignature` + `replayLearnedProcedure`), never activated -- `procedureStep` refuses outside a replay; (3) comparisons use executor-independent observables only (the cockpit's result digest is not comparable across executors); (4) the one approval pins `procedureHash` over source, template and preconditions, so any change is a new candidate. Keep it tight; the gate `TestNoDatabaseProductClaims` and the doc gates scan this file.
- [ ] New public page `docs/public/operate/procedure-certification.md` (front-matter per `docs/DOCS_STANDARD.md`): the ladder, the values, what each rung does, what the person approves, demotion and retirement, what "falls back to the app" means, what is measured by the proving suite; link it from `GLOSSARY.md` and from `docs/public/operate/local-apps.md`.
- [ ] Design record: append "What shipped differently, recorded 2026-09-23 when epic D landed" (the executor-independent observables; shadow of a machine-local procedure is DRY; the served path through `replayLearnedProcedure`; `canaryMatches` as a policy value; epic C gaps closed here) and fill the delivery table's status.
- [ ] Delete this plan in the last commit before the PR merges.
- [ ] Full verification on the FINAL tree (after the last commit): `make test` (captured, MAKE_EXIT read), `go test -count=1 -timeout=900s $(scripts/ci/db-gated-packages.sh --complement-cacheable)`, `go test -count=1 -timeout=300s .`, db-gated trees on the epic database, `go run ./cmd/memqllint dsl/`, `go run ./cmd/harness-eval`, `make env-registry-check`, `make sdk-gen-check`, `make arch-model-check`, `make frontdoor-paths-check`, tagged vet/test sweep, module-boundaries loop, `make os-test os-typecheck os-build`, `go run ./cmd/memql-bench --do=gate` against the epic database, gitleaks.
- [ ] One PR against `main` with `Closes #5408.` `Closes #5409.` `Closes #5410.` `Closes #5411.` `Closes #5412.` `Closes #5413.` each on its own line (a comma list links only the first); assert `closingIssuesReferences` lists all six.
