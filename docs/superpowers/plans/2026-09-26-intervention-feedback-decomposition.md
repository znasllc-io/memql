# Intervention, feedback and reusable decomposition -- implementation plan (epic memql#5414)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person can step into any run -- re-run a step with a different level, model, effort, prompt or inputs as a new version, go back by moving the run's head, or branch into a fork run -- say what was wrong on the AI Fluency framework's three Discernment axes, see an AI validator's pre-filter verdict beside their own, and have that feedback change what the system does next in four named places; long work is cut at compile into sections that ask the catalog before intelligence, and every learned or authored automation is labelled reusable / goal-specific / account-specific by evidence with a versioned override.

**Architecture:** Every DECISION stays pure in `component/work` (head moves, re-run plans, fork prefixes, workspace snapshots, override and feedback validation, section boundaries, section routing, reuse labels). `component/automations` gains a re-run mode of `ResumeFrom` and a fork-by-reference mode (the prefix is REHYDRATED from the source run, never re-executed). `integrations/work` holds every person-facing act as an `@sdk` builtin (`rerunStep`, `branchRun`, `moveRunHead`, `recordFeedback`, `workStepVersions`) plus the validator; the acts ride the run row to the agent node (cross-node plumbing is explicit: `run.rerun`). Overrides reach the model seam through `common.RunContext` for ONE step only. Decomposition extends the triage answer and the planner's bundle synthesis; the reuse sweep lives in `integrations/procedure`. Nexus draws versions on its existing step spine.

**Tech Stack:** Go 1.26 (leaf modules `component/work`, `component/procedure`), MemQL DSL (`dsl/work`, `dsl/authoring`, `dsl/procedure`, `dsl/planner`), protobuf (`component/grpc/worker.proto`), React/TS (MemQL OS, `clients/os`), the proving suite.

**Spec:** `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md` -- decisions D18-D24, section 4 epic E, section 7.

**Issues:** epic #5414; tasks #5415 (versions and the head), #5416 (feedback), #5417 (feedback consumers), #5418 (decomposition), #5419 (Nexus), #5420 (tests and the proving scenario). ONE PR closes all seven.

**Base:** branched off `origin/epic/procedure-certification-replay` (epic D, #5408) at `084362329`, merged forward as D's streams land. The PR opens only after D has merged into `main`; `main` is then merged in so the diff is E alone.

---

## Global Constraints

- Values, not constants: the reuse evidence threshold (`2` distinct goal signatures) and the validator switch (`validateAnswers: true`) are ROWS on the seeded singleton `v1:work:feedbackPolicy:primary`; Go carries the same values only as `DefaultFeedbackPolicy()` for an absent/unreadable row, and a non-positive threshold normalizes to its default.
- Owner-scoped, composite tier (D17): every read of a person's rows runs under `auth.ContextWithUserActor(owner)`; every `@serverOnly` write goes through the package's ONE internal-origin stamp site (`integrations/work/store.go executeInternal`, `integrations/procedure/store.go writeInternal`); a sweep iterates owners and borrows each one's actor.
- Every field is ADDITIVE. No field removed, no enum value dropped, no `@required` added to a concept with rows (memql#5199). `make concept-snapshot` passes with no `retired` entry.
- A new version's intent RESETS every per-version field (`result: {}`, `resultFingerprint: ""`, `binding: {}`, `errorCode: ""`, `errorMessage: ""`, `childRunId: ""`, `override: {}` or this version's, `authoredBy: ""` or this version's). The engine turns an insert on an existing id into a SHALLOW read-merge and drops JSON null, so an unwritten key keeps the previous version's value.
- An override applies to ONE version of ONE step (D20). It rides `common.RunContext` for that step's execution only; the next step's context never carries it.
- Journal serving never crosses goals, and a replay never reads description guidance (D23).
- No `if env == ...` anywhere (`TestNoEnvironmentBranchingInEngineCode`).
- Strings in MemQL call text go through `langparser.QuoteString` / `parser.RenderCall`, never Go `%q` (`TestDSLCallStringsDoNotUseGoQuoting`).
- Builtin replies are ONE id-keyed map on the wire (memory: builtin-reply-is-one-id-keyed-map); a builtin answering several rows gives each a DISTINCT id (`<stepId>@v<version>` for versions).
- Stage files by explicit path; never `git add -A` / `git add .`. Commit messages `Issue #<N>: <what>`, ending with the session's `Co-Authored-By` trailer.
- No emojis anywhere (docs, UI copy, code comments). Never run prettier; OS code is hand-formatted.
- Construct names are unique across the WHOLE DSL tree; grep before adding one.
- MemQL OS acceptance is RENDERED screenshots, both themes, empty and populated, desktop and narrow (clients/os/DESIGN.md, clients/os/SUPERVISED-VISUAL-COMPOSITION.md).

## Review Focus

1. A re-run requested while the run is still running, waiting or compiling must be refused (`run_not_finished`) rather than racing the live execution; test in Task 3 (`TestARerunOfARunningRunIsRefused`).
2. A re-run whose target step key is nested (`decide/a`, `for_x/0/touch`) or not in the run's step order must be refused naming the step; test in Task 3 (`TestARerunOfANestedStepIsRefused`).
3. Moving the head to a version whose downstream already has matching versions must re-run NOTHING (instant go-back), and to one with no matching downstream must re-run only from the first unmatched step; test in Task 1 (`TestMovingTheHeadBackRestoresMatchingDownstreamVersions`, `TestMovingTheHeadReRunsOnlyFromTheFirstUnmatchedStep`).
4. A node dying mid-re-run: the sweep re-dispatches the run and the re-run spec on the row is honoured (the override still reaches its step if that step had not finished); test in Task 2 (`TestAnInterruptedRerunResumesWithItsOverride`).
5. A branch or re-run of a session step whose earlier recordings omitted a file's content is refused naming the file; an earlier `exec` whose effects were never recorded is REPORTED (`unrecordedCommands`) and never silently restored; test in Task 1 (`TestASnapshotWithAnOmittedFileIsRefusedNamingIt`) and Task 3.

---

## 0. Ground truth this plan rests on (measured 2026-09-26 at `084362329`)

### The journal, resume and fork
- Step row id is `memql.BareShortId(runId) + "-" + sanitize(key)` (`component/automations/journal.go:152-171`); a retry is a new VERSION of that row with `attempt` incremented. `createWorkStep` is an insert the engine turns into a shallow read-merge (`component/memql/executor_mutation.go:645-672`). The step rows' `runId` is BARE for trigger runs and after `ResumeFrom`, CANONICAL for adopted goal runs -- a reader matches both.
- `stepRunning` (journal.go:465-482) writes the intent; `stepFinishedRowOnly` (:508-536) the receipt; `stepFinished` (:491-502) also writes `updateWorkRun{runId, heartbeatAt, chainHead, stepOrder}`. `dependsOn` has no writer.
- `attemptBase` (`component/automations/sequence.go:88-93`) is the resume point's last attempt, 0 for every other step: a resume re-runs later steps at attempt 1 with their OLD idempotency keys.
- `ResumeFrom(ctx, journal, automation, *ResumeOptions{FromStep, AllowSideEffects})` (`component/automations/resume.go:311`) refuses a journal with no failed or running step (`ValidateRunJournal`, resume.go:246-262). A done step before the resume point binds its recorded value and does not run (`sequence.go:228-236`, `resumedList`).
- The agent's `Dispatch` (`app/integrations_work_dispatch.go:122-217`) runs `ExecuteAdopted` for a run with no step rows, otherwise `ResumeFrom`. `HandleRunEvent` (`integrations/work/dispatch.go:201-244`) needs status running, a goalId and an automationName.
- The existing `forkRun` (`integrations/work/fork.go:40-83`, `deriveRun` :178-234) opens a fork run that RE-EXECUTES every step from step 1; only model calls before the fork point are served from the source journal (`integrations/work/modeljournal.go`, `DecideServe` fork arm). Its only caller is Nexus.
- `v1:work:{goal,run,step,approval}` broadcast; `observation` and `modelCall` never cross nodes (`component/node/routing.go:263-274`, `routing_reach.go:215-233`).

### Model choice
- `requestForPrompt` (`component/memql/ai_request.go:49-89`) sets the level from the prompt's `@level` and `ExplicitProvider` from `AIInvocation.ProviderOverride` (via `memql.WithProviderOverride(ctx, name)`); `InvokeAIStructured` passes a nil invocation and ignores the override. `AIInvocation.ModelOverride` has no reader.
- The router walks an explicit provider as a ONE-ENTRY CHAIN (`component/router/router.go:375-393`), so any entry of the closed grammar is a valid model pin: a provider name, `fleet:<modelId>`, `app:<id>`, `app:<id>:<model>`, `federation:<name>`.
- Agent turns (`runAgentTurn` -> `integrations/agents/agent_turn.go:98`) build their messages in `workTurnHistory` (`integrations/agents/work_context.go:17-61`), which already returns nil in replay mode; the replier fixes the level at `strong` (`integrations/agent/replier.go:271-337`).
- Effort cannot be requested anywhere; it is only reported (`servedEffort`). `ModelCallStart` carries `model` and `level` (`component/grpc/worker.proto:627`); `AppSessionStart` carries `level` and NO model or effort (:450-508). The cockpit resolves a level to `harness.Knobs{Model, Effort}` (memql-cockpit `internal/worker/harness/levels.go:57`, `appsession/session.go:612-660`).

### App sessions
- The session door (`component/router/session_door.go:37-64`) hands the step to `integrations/agent/worker/app_session_delegate.go RunStep` with `StepId = run.StepKey` -- a step KEY. `stampParentStep` (:273-287) runs `updateWorkStep(stepId: <key>)`, which names no row, so the parent step never gets its `childRunId`; the child goal id derives from the key alone and collides across runs. BOTH are defects Task 4 fixes (E reads the step -> recording link).
- There is no workspace snapshot. Recorded contents are `contentRefs` (Library FILE ids) and `contentOmitted` (flat strings `"<path>: <why>"`, optional `" (sha256 <hex>)"`) on `tool_result` observation data (`integrations/work/session.go:523-567`); `integrations/procedure/corpus.go parseOmitted`/`contentDigests` already parse them. `AppSessionStart.inputs` takes Library ARTIFACT ids the cockpit lands FLAT under the file row's name (`<base>.<session12>.<action12>`); `libraryArtifactBySourceConceptRef` maps a file to its artifact.

### Reading every version
- There is no DSL all-versions read (`TestDeploymentAllVersionsInOneReadIsStillAbsent`, and the comments at `dsl/deployment/queries.memql:77-97` name a BUILTIN as the prior art). `workTrace` reads every version but is not row-scoped. The gated raw-read pattern is `integrations/work/sweep.go:774-836` (`selectAdmitted`, `pctx.AdmitSourceRow`); the index `memory_nodes_work_journal_lookup_idx` covers `((payload->>'runId'), concept, id, "createdAt" DESC)` for steps.

### Feedback (agreed with epic D's session on 2026-09-26)
- D's `component/work/verdict.go` (`ParseVerdict`, `CandidateGate`, `EntryRung`) and `integrations/procedure/corpus.go readFeedback` read observations `kind == "feedback"` with `data.verdict`, `data.target.stepKey` (string) and `data.target.version` (int); a target with no stepKey is the RUN, and a run-level dislike excludes the recording. The newest row per version wins. E writes EXACTLY that shape and adds `data.axes {product, process, performance}` (booleans), `data.reason`, `data.goalSignature`.
- `observation.kind` has no `feedback` value today; a write would fail schema validation.

### Compile
- Triage (`goalComplexityTriage`, `@level("fast")`, `dsl/planner/prompts.memql:34-52`, parsed by `integrations/planner/agent_loop_sectionable.go:92-128`) answers a flag plus `sections: [{label, instruction}]` -- independent, no inputs, outputs, purpose or reuse intent. `synthesizeWorkReasoningBundle` (`integrations/planner/work_compile_draft.go:48-141`) turns them into ONE `@template automation` with one `runAgentTurn` per section and an assemble step.
- The goal-level near tier is not owner-scoped and cannot match an automation (automations are never embedded; `CatalogKey` refuses kind automation). Nothing writes `catalogued`/`goalSignature` on an authored automation. The `compileGoal` prompt has no Go caller.
- Builtin effects are not modelled and the journal writes no step footprint or postcondition, so the boundary rule is decided on what the SECTION declares.

### Sweeps and gates a new automation trips
- `maintenanceAutomations` (`component/auth/maintenance_actor.go:88-212`) + its sorted pin (`component/automations/maintenance_actor_gate_test.go:102-156`); `shippedAutomationCount` (`component/automations/strict_automation_boot_test.go:181`, 71 at this base); the corpus golden per automation (`go test -count=1 -run 'TestAutomationCorpusRuns$' ./component/automations/steps/ -update`); `make arch-model`; the `@serverOnly` pin list (`test/dslconformance/server_only_parsed_test.go`); the prompt level pin (`test/dslconformance/prompt_levels_test.go:49`); `TestCapabilityNamesMatchTheDSL` (`integrations/work/capabilities_test.go:27`); every new concept field reds `make sdk-gen-check`; a field no shape projects is invisible to every read.

---

## 1. Contracts (every stream codes against these; change one only by editing this section first)

### 1.1 DSL surface (Task 0 lands all of it)

`dsl/work/concepts.memql`, additive:

| Concept.field | Type | Meaning |
|---|---|---|
| `run.head` | `object` | `{"<stepKey>": {"version": int, "runId": string?}}` -- the current version of every top-level step. `runId` is present only when that version lives in another run (a fork's shared prefix). ABSENT on runs written before E: a reader treats each step's newest row as current. |
| `run.staleSteps` | `[]string` | Step keys whose head version was computed against an upstream version no longer current; written by a head move, cleared as a re-run gives them new versions. |
| `run.rerun` | `object` | The pending re-run request, `{requestId, reason: "rerun"|"headMove"|"branch", stepKey, override, snapshot?, workspace?, requestedBy, requestedAt}`. Written on the bff by the act, served on the agent, cleared to `{}` when the run closes. |
| `run.validation` | `object` | `{verdict: "pass"|"flag", stepKey, version, observationId, level, at}` -- the validator's summary; the detail is the `decision` observation. |
| `step.version` | `int` | The version number (equals `attempt` for every journal-written row). |
| `step.basis` | `object` | The head of every EARLIER top-level step when this version started, same shape as `run.head`. OMITTED in the pristine case (every earlier entry is version 1 in this run), so an ordinary run writes no O(n^2) basis; `ParseHead` of an absent basis plus the step order reconstructs it. |
| `step.override` | `object` | `{level, model, effort, prompt, inputs, guidance: {axes: {product, process, performance}, reason, feedbackId}, requestedBy}` -- empty `{}` on a version nobody overrode. |
| `step.authoredBy` | `string` | The person's user id when they wrote this version's prompt or inputs (D20); empty otherwise. |
| `observation.kind` | enum += `"feedback"` | |

New concept `v1:work:feedbackPolicy` (singleton, literal id `v1:work:feedbackPolicy:primary`), `@rowAuthz(clusterOwner, rankFloor="reader")`: `validateAnswers bool!`, `reusableAfterSignatures int! @minimum(1)`. Seeded in `dsl/work/seeds.memql` by `seed feedbackPolicy feedbackPolicyPrimary` (seed names are global and `primary` is ladderPolicy's) with an explicit `id: "primary"` and `true` / `2`. Read by `feedbackPolicyCurrent` (`@requiresRank("reader")`, no `@actor`).

`dsl/authoring/concepts.memql`, `construct` additive:

| Field | Type | Meaning |
|---|---|---|
| `reuse` | `enum("reusable", "goalSpecific", "accountSpecific")` | What the EVIDENCE says (D24). Absent until the sweep has looked. |
| `reuseEvidence` | `object` | `{goalSignatures: [string] (distinct, at most 20 kept), signatureCount: int, accountIds: [string], uses: int, decidedAt}` |
| `reuseOverride` | `object` | `{label, by, at, version}` -- a person's label; `version` counts every override write; `label: ""` means "follow the evidence". |

Mutations (all `@serverOnly` unless marked; each pinned in `server_only_parsed_test.go` with its reason and a `///` doc saying why caller-scoping is not the fix):

- `updateWorkRun` gains args `head object`, `staleSteps []string`, `rerun object`, `validation object` (no defaults: an omitted arg leaves the stored value alone; `{}` / `[]` clear).
- `createWorkRun` gains `head object`.
- `createWorkStep` gains `version int`, `basis object`, `override object`, `authoredBy string`, plus the reset fields it did not accept before: `result object`, `resultFingerprint string`, `binding object`, `errorCode string`, `errorMessage string`, `childRunId string` (bound with NO default, so the journal sends them only on a version > 1).
- `updateWorkStep` gains `version int`, `basis object`, `override object`, `authoredBy string`.
- NEW `reassertWorkStepVersion` (update): every per-version field (`status, result, resultFingerprint, binding, postcondition, symptom, attempt, version, basis, override, authoredBy, childRunId, idempotencyKey, startedAt, finishedAt, durationMs, tokens, cost, errorCode, errorMessage`) -- the head move's pointer write (section 1.4).
- (No new feedback mutation: `recordFeedback` writes through the existing `createWorkObservation` with `kind: "feedback"`, now a legal enum value.)
- NEW `recordConstructReuse` (update on construct): `constructId!, reuse!, reuseEvidence object!`.
- NEW `recordConstructReuseOverride` (update on construct): `constructId!, reuseOverride object!`.
- NEW `createFeedbackPolicy` (seed materializer only).

Queries:

- `workDescriptionGuidance(goalSignature!)` `@actor`: owner, `kind == "feedback"`, `row.?data.goalSignature == args.goalSignature`, `row.?data.verdict == "dislike"`; sort createdAt desc; paginate 20; shape `workObservationFull`.
- `workSignedRunsForOwner()` `@actor`: owner, `status == "succeeded"`, `goalSignature != nil`; sort createdAt desc; paginate 1000; shape `workRunFull`.
- `workAutomationStepsForOwner()` `@actor`: owner, `stepType == "automation"`; sort createdAt desc; paginate 2000; shape `workStepFull`.
- `feedbackPolicyCurrent()` as above.
- Shapes: `workRunFull` projects `head, staleSteps, rerun, validation`; `workStepFull` projects `version, basis, override, authoredBy`; every construct shape behind `cataloguedConstructsForOwner`, `learnedProceduresForOwner`, `authoringConstructById`, `procedureConstructByName` projects `reuse, reuseEvidence, reuseOverride`.

Builtins, `dsl/work/builtins.memql`, `@executor("integration.work.<name>")`:

| Builtin | `@sdk` | Fields | Reply (id-keyed map, one entry unless noted) |
|---|---|---|---|
| `rerunStep` | yes | `runId string!`, `stepKey string!`, `level string @enum("fast","strong","reasoning")`, `model string`, `effort string @enum("low","medium","high","xhigh","max")`, `prompt string`, `inputs object` | `{runId, stepKey, version, staleSteps}` |
| `branchRun` | yes | same as rerunStep | `{runId (the fork), forkedFromRunId, forkAtStepKey}` |
| `moveRunHead` | yes | `runId string!`, `stepKey string!`, `version int!` | `{runId, stepKey, version, staleSteps}` |
| `recordFeedback` | yes | `runId string!`, `stepKey string`, `version int`, `verdict string! @enum("like","dislike","neutral")`, `product boolean`, `process boolean`, `performance boolean`, `reason string` | `{observationId, verdict, validatorDisagrees}` |
| `workStepVersions` | yes | `runId string!` | one entry PER VERSION, id `<stepId>@v<version>`: the folded step row plus `current: bool` |
| `workValidateAnswer` | no | `runId string!` | `{observationId, verdict}` or `{skipped: reason}` |

`forkRun` is RETIRED (its builtin, its handler and its Nexus caller): `branchRun` is the fork with overrides, serving the prefix by reference (D19). `replayRun` stays.

`dsl/procedure/builtins.memql`: `setConstructReuse` (`@sdk`, executor `integration.procedure.setReuse`; fields `constructId string!`, `label string! @enum("reusable","goalSpecific","accountSpecific","evidence")` -- `evidence` clears the override) and `procedureReuseSweep` (executor `integration.procedure.reuseSweep`; field `dryRun boolean`).

Prompts:
- `validateStepAnswer` (`dsl/work/prompts.memql`, `@level("strong")`, `@templateFile("prompts/validateStepAnswer.tmpl")`): `goal string!`, `stepKey string`, `description object` (`{prompt, purpose, responseSchema, postcondition}` as far as known), `answer string!`, `now string!`. Structured output `{product: boolean, process: boolean, performance: boolean, reason: string}` -- `true` means a PROBLEM on that axis.
- `goalComplexityTriage` gains input `guidance []object` and its template asks each section for `{label, instruction, purpose, inputs, outputs, reuseIntent, effects, postcondition}`; `authoringDesign` gains input `guidance []object`.

Automations:
- `dsl/work/automations.memql` `validateGoalAnswer`: `@trigger(event="node.updated", concept="v1:work:run")`, `@filter(row => row.status == "succeeded" && args.oldStatus != "succeeded" && row.goalId != nil)`, one statement `builtin workValidateAnswer(runId: args.id)`.
- `dsl/procedure/automations.memql` `sweepConstructReuse`: `@trigger(schedule="0 20 */6 * * *")`, `builtin procedureReuseSweep(dryRun: false)`; a `maintenanceAutomations` entry.

### 1.2 Wire (Task 0)

`component/grpc/worker.proto`: `AppSessionStart` gains `string model = 15;` and `string effort = 16;` (EMPTY means the level decides); `ModelCallStart` gains `string effort = 17;`. `component/worker.RunSpec` gains `Model, Effort string`, carried onto `AppSessionStart`. The cockpit honouring them is a follow-up PR in `memql-cockpit` (Task 7) pinned to the engine version that carries the fields; until then the served model and effort the app REPORTS on `AppSessionEnd` are what the version shows.

### 1.3 `component/work` (Task 1)

```go
// head.go
type HeadEntry struct {
	Version int    `json:"version"`
	RunId   string `json:"runId,omitempty"` // set only when the version lives in another run
}
type Head map[string]HeadEntry
func ParseHead(v any) Head                        // tolerant decode of a stored object; ints arrive as float64
func (h Head) Object() map[string]any              // the stored form; nil-safe
func (h Head) Equal(o Head) bool
func BasisFor(order []string, head Head, key string) Head // entries for the keys BEFORE key in order
func PristineBasis(order []string, key string) Head        // every earlier key at version 1, no RunId
func IsPristine(basis Head, order []string, key string) bool // the journal omits a pristine basis
// A StepVersion whose stored basis was absent carries PristineBasis(order, key).

type StepVersion struct {
	Key     string
	Version int
	Basis   Head
	Status  string // done, failed, running, skipped, ...
}

type RerunPlan struct {
	From     string         // the first key that executes
	Versions map[string]int // key -> the version it executes as (max recorded + 1)
	Stale    []string       // every key from From on, in order
}
func PlanRerun(order []string, versions map[string][]StepVersion, key string) (RerunPlan, error)
// ErrStepNotInRun (naming the key), ErrNestedStep (a key containing "/")

func MoveHead(order []string, head Head, versions map[string][]StepVersion, key string, version int) (Head, []string, error)
// head[key] = version; then for each later key in order: the NEWEST version whose Basis equals the new
// head's entries for every earlier key (and whose status is done) becomes current; the first key with
// no such version and EVERY key after it are stale (their current entry is left as it was until the
// re-run replaces it). ErrVersionNotFound names key and version.

func ForkAt(order []string, source Head, sourceRunId, key string) (Head, error)
// the fork's initial head: every key BEFORE key, pointing at the source's version with RunId set

// override.go
type Guidance struct {
	Axes       Axes   `json:"axes"`
	Reason     string `json:"reason,omitempty"`
	FeedbackId string `json:"feedbackId,omitempty"`
}
type Override struct {
	Level       string         `json:"level,omitempty"`
	Model       string         `json:"model,omitempty"`
	Effort      string         `json:"effort,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Inputs      map[string]any `json:"inputs,omitempty"`
	Guidance    *Guidance      `json:"guidance,omitempty"`
	RequestedBy string         `json:"requestedBy,omitempty"`
}
func (o Override) Authored() bool     // Prompt != "" || len(Inputs) > 0
func (o Override) Empty() bool        // nothing that changes the call (RequestedBy alone is empty)
func ValidateOverride(o Override) error
// level in {fast, strong, reasoning} (embeddings is refused: D10); effort in {low, medium, high, xhigh, max};
// prompt at most 20000 bytes; model trimmed and at most 200 bytes; inputs keys non-empty.
// Each refusal is a *OverrideError{Field, Code} with Code override_level_invalid | override_effort_invalid |
// override_prompt_too_long | override_model_invalid | override_input_key_invalid.

// snapshot.go
type FileEffect struct {
	Order   int    // global action order across the recordings, oldest first
	Path    string // workspace-relative, cleaned
	FileId  string // v1:library:file id; empty when omitted
	Omitted string // why the content is missing ("" when present)
}
type SnapshotFile struct {
	Path   string `json:"path"`
	FileId string `json:"fileId"`
}
type Snapshot struct {
	Files              []SnapshotFile `json:"files"` // sorted by path
	UnrecordedCommands int            `json:"unrecordedCommands"`
}
func SnapshotOf(effects []FileEffect, unrecordedCommands int) (Snapshot, error)
// the NEWEST effect per path wins; a path whose newest effect is omitted refuses with
// *SnapshotOmittedError{Path, Why} whose message names the file.

// feedback.go
type Axes struct {
	Product     bool `json:"product"`
	Process     bool `json:"process"`
	Performance bool `json:"performance"`
}
func (a Axes) Any() bool
func (a Axes) Names() []string // "product", "process", "performance" in that order
func ValidateFeedback(v Verdict, axes Axes, reason string) error
// ErrFeedbackAxisRequired for a dislike with no axis; ErrFeedbackVerdictInvalid for unseen; a reason over 2000 bytes refuses
type ValidatorVerdict struct {
	Axes   Axes   `json:"axes"`
	Reason string `json:"reason"`
}
func (v ValidatorVerdict) Flagged() bool
func Disagrees(person Verdict, validator ValidatorVerdict) bool // like vs flagged, or dislike vs passed; neutral never disagrees

// sections.go
type Section struct {
	Name          string
	Label         string
	Instruction   string
	Purpose       string
	Inputs        []string
	Outputs       []string
	ReuseIntent   string    // reusable | goalSpecific | accountSpecific (the decomposer's proposal)
	Effects       Footprint // what the section changes
	Postcondition string    // how its end is checked; "" when none was declared
}
func SectionSignature(s Section) string // GoalSignature(s.Purpose, s.Inputs)
func CheckSectionBoundaries(sections []Section) error
// *SectionBoundaryError{Section, Reason}: a section with a side effect (Effects.IsSideEffect()) and no
// postcondition ENDS MID-EFFECT; a section with neither outputs nor a postcondition has no end at all.
type SectionCandidate struct {
	ConstructId string
	Name        string
	Signature   string
	Reuse       string // EFFECTIVE label
	Text        string // purpose/title words the near tier compares
	Args        []string
	Rung        Rung
}
type SectionRoute string // catalogExact | catalogNear | intelligence
type SectionDecision struct {
	Section   Section
	Route     SectionRoute
	Candidate *SectionCandidate
}
type SectionPlan struct {
	Sections   []SectionDecision
	NeedsModel bool // any section routed to intelligence
}
func DecideSections(sections []Section, exact map[string][]SectionCandidate, reusable []SectionCandidate, nearThreshold float64) SectionPlan
// per section: exact hit on SectionSignature first; else the best REUSABLE candidate whose Similarity >=
// threshold AND whose Args cover the section's Inputs; else intelligence. A candidate whose effective
// reuse is not "reusable" is never a near hit.
func SectionSimilarity(a, b string) float64 // Dice coefficient over NormalizeStatement content words
const DefaultSectionNearThreshold = 0.6

// reuse.go
type ReuseLabel string
const (
	ReuseReusable        ReuseLabel = "reusable"
	ReuseGoalSpecific    ReuseLabel = "goalSpecific"
	ReuseAccountSpecific ReuseLabel = "accountSpecific"
)
type ReuseEvidence struct {
	GoalSignatures []string
	AccountIds     []string
	Uses           int
}
func DecideReuse(e ReuseEvidence, reusableAfter int) ReuseLabel
// >= reusableAfter distinct signatures -> reusable; otherwise exactly one distinct account tie -> accountSpecific;
// otherwise goalSpecific (including zero uses)
func EffectiveReuse(evidence, override ReuseLabel) ReuseLabel // a non-empty override wins

// policy.go
type FeedbackPolicy struct {
	ValidateAnswers         bool
	ReusableAfterSignatures int
}
func DefaultFeedbackPolicy() FeedbackPolicy // {true, 2}
func (p FeedbackPolicy) Normalize() FeedbackPolicy
```

### 1.4 Re-run, head move and branch -- the execution contract (Tasks 2-4)

- `core/common.RunContext` gains (LANDED at `d1845f2ed`, `core/common/modelcall.go`):
  ```go
  Override  *StepOverride      // ONLY on the context of the one step the re-run or branch targets
  Workspace string             // a fresh workspace for this execution's app sessions; "" = the default
  Snapshot  *WorkspaceSnapshot // ONLY on the targeted step, when it is a session step

  type StepOverride struct {
  	Level, Model, Effort, Prompt string
  	Inputs                       map[string]any
  	GuidanceAxes                 []string // "product", "process", "performance"
  	GuidanceReason, FeedbackId   string
  	RequestedBy                  string
  }
  func (o *StepOverride) Empty() bool
  type WorkspaceSnapshot struct{ Files []SnapshotFile }
  type SnapshotFile struct{ Path, FileId string }
  ```
- A **re-run** (`rerunStep`): the bff handler validates, then writes the run `{status: "running", rerun: {requestId, reason: "rerun", stepKey, override, snapshot?, workspace?, requestedBy, requestedAt}, staleSteps: plan.Stale}` through the internal stamp under the owner. The agent's `Dispatch` sees `journal.Rerun` and calls `ResumeFrom(journal, auto, &ResumeOptions{FromStep: rerun.stepKey, AllowSideEffects: true, Rerun: &RerunSpec{...}})`. With `Rerun` set: `ValidateRunJournal`'s failed-step requirement is waived; every step from `FromStep` on executes as `maxRecordedAttempt(step) + 1`; the targeted step alone runs with `RunContext.Override` (and `Snapshot`); every step of the execution carries `RunContext.Workspace` when set. The run closes with `rerun: {}` and `staleSteps: []`.
- A **head move** (`moveRunHead`): the bff handler reads every version (`StepVersions`), calls `work.MoveHead`, re-asserts each step whose current version CHANGED with `reassertWorkStepVersion` (so every collapsed read -- the timeline, the corpus, `LoadRunJournal` -- keeps returning the head), writes `head` and `staleSteps`, and when stale is non-empty also writes `status: running` with `rerun: {reason: "headMove", stepKey: stale[0], override: {}}`.
- A **branch** (`branchRun`): the bff handler creates a new run (`mode: "fork"`, `forkedFromRunId`, `forkAtStepKey`, `triggeredBy: "branch:<src>"`, the source's template identity, variables and goal, `head: work.ForkAt(...)`, `rerun: {reason: "branch", stepKey, override, snapshot?}`, status running). The agent's `Dispatch` builds the fork's journal from the SOURCE: `Steps` = the source's done results for every key before the fork step (the source's newest rows ARE its head, by the re-assertion invariant), then `ResumeFrom(FromStep: forkAtStepKey, Rerun: ...)`. The prefix NEVER executes in the fork; only the fork step and what follows run, live.
- The **journal**: the intent write carries `version` (= attempt), `basis` (= `BasisFor(stepOrder, head, key)`), `override` and `authoredBy` for the targeted step (`{}` / `""` otherwise), and the reset fields on any version > 1; the run write at each receipt carries the FULL `head` map (read-merge is shallow, so a partial map would erase the rest). Top-level steps only: a nested key belongs to the version of the step that called it.
- The **model seam** (Task 4): for the step whose `RunContext.Override` is set, `requestForPrompt` and the structured path apply `Level` (the requested level), `Model` (as `ExplicitProvider` -- a one-entry chain), `Effort` (`ResolveRequest.Effort`, carried to `ModelCallStart.effort` for an app door and ignored with a debug log by a door that has no such knob), `Prompt` (appended as a user message `Instructions for this step from its owner:` + text) and guidance (appended `What was wrong with the previous version (<axes>): <reason>`); `workTurnHistory` applies the same two messages and the level/model to an agent turn. A session step's delegate uses `Prompt` as the WHOLE session prompt (the session prompt is recorded on `v1:worker:appSession.prompt`, so the person edited the real text), puts `Model`/`Effort` on the wire, and when `Snapshot` is set passes the snapshot's artifact ids as `Inputs`, a fresh `Workspace`, and a restore preamble mapping each landed name to its original path.
- **Description guidance** (D23): `workTurnHistory` (live runs only; it already returns nil in replay) and the compile-time triage/design calls read `workDescriptionGuidance(goalSignature)` for the run's goal signature and add at most 5 reasons as one message/input. Nothing on a replay, a fork prefix or a construct-served goal reads it.

### 1.5 Feedback, the validator and the corpus (Tasks 3 and 5)

- `recordFeedback` handler (`integrations/work/feedback.go`): the caller must own the run; `ValidateFeedback`; a step target must name a top-level key with that version recorded (`feedback_target_not_found`); `data = {verdict, axes, reason, target: {stepKey, version} | {}, goalSignature, validatorDisagrees?, validatorObservationId?}`; `content` = one sentence ("Disliked version 2 of draft (product, process): the totals are missing."); a fresh observation id every call (a later verdict is a new row).
- `workValidateAnswer` handler: runs only under internal origin or the `system:automation:validateGoalAnswer` actor with the run's owner resolved from the run (the D pattern `learnFromSucceededRun` uses); skips (and says why) when the policy is off, the run is not a goal run requested via `nexus` or `api`, the run made no model call, or a `decision` observation from the validator already names this run's answer version; otherwise builds the description from the answer step (the last done step whose kind called a model), calls `validateStepAnswer` at the run's level (the answer step's recorded `binding` level when present, else the prompt's), writes a `decision` observation `data = {validator: {axes, reason, verdict: "pass"|"flag"}, target: {stepKey, version}, level, model}` and `run.validation`. It NEVER writes a `feedback` observation, never touches a construct, and never calls anything in `integrations/procedure` -- which is what "never changes the ladder" means structurally, and a test asserts it.
- Corpus (`integrations/procedure/corpus.go`): a recording run whose PARENT step version (the parent run's version of the step whose `childRunId` is this run) is not the parent's head version is excluded (`superseded`); one whose parent step version's newest verdict is a dislike is excluded (`disliked`); a liked parent step version or a run-level like gives the recording weight 2 (else 1). Weights RANK only: `component/procedure.Mine` gains `MineWeighted(sequences, weights, minSupport, gap)` where the two-use floor counts DISTINCT recordings unweighted and weighted support breaks ranking ties after coverage and cohesion; instance order is unchanged, so an unchanged selection keeps its `procedureHash` (a like never resets a ladder).
- App hand-back (after D merges): D's `renderGuidance` appends the dislike reasons on the handed-back step's version.

---

## Task 0: the foundation (coordinator)

**Files:** `dsl/work/{concepts,mutations,queries,shapes,builtins,prompts,automations,seeds}.memql`, `dsl/work/prompts/validateStepAnswer.tmpl`, `dsl/authoring/{concepts,mutations,queries,shapes}.memql`, `dsl/procedure/{builtins,automations}.memql`, `dsl/planner/prompts.memql` (inputs only), `component/grpc/worker.proto` + generated, `component/worker/runner.go` (RunSpec fields + wire), `integrations/work/integration.go` (capability stubs), `integrations/procedure/integration.go` (capability stubs), `component/auth/maintenance_actor.go`, test pins, generated artifacts.

- [ ] Write the DSL of section 1.1 exactly. Anchor every insertion on the neighbouring construct's `///` doc, never its declaration line.
- [ ] Register every new capability as a STUB returning `work: <name> is not wired yet` / `procedure: <name> is not wired yet`; add `TestTheStubsSayTheyAreNotWiredYet` (each stream replaces its stubs; Task 8 swaps the test for `TestNoCapabilitySaysItIsNotWiredYet`).
- [ ] Delete `forkRun` (builtin, handler, capability entry); the Nexus caller moves to `branchRun` in Task 6.
- [ ] Proto fields of section 1.2; `make proto-gen`; commit; then `make proto-gen-check` (it diffs against the COMMITTED tree).
- [ ] Pins: `server_only_parsed_test.go` (four mutations), `prompt_levels_test.go` (`validateStepAnswer: strong`), `maintenance_actor.go` + `maintenance_actor_gate_test.go` (`sweepConstructReuse`), `shippedAutomationCount` +2, corpus goldens (`-update`), `TestCapabilityNamesMatchTheDSL`.
- [ ] Regenerate: `go run ./cmd/memqllint dsl/`, `make concept-snapshot`, `make sdk-gen`, `make arch-model`; `go test -count=1 ./component/automations/... ./test/dslconformance/... ./integrations/work/... ./integrations/procedure/...` and `go test -count=1 -run 'Rowauthz|ServerOnly|Capability|Snapshot' ./component/memql/`.
- [ ] Commit `Issue #5415: the epic E surface -- run head, step versions, feedback, reuse, the validator and the wire fields, every capability a stub`.

## Task 1: the pure decisions (`component/work`) -- #5415, #5416, #5418

**Files:** Create `component/work/{head,override,snapshot,feedback,sections,reuse,policy}.go` and a `_test.go` beside each.

- [ ] `head_test.go`: `TestPlanRerunExecutesFromTheStepAtMaxPlusOne`, `TestPlanRerunRefusesAnUnknownOrNestedKey`, `TestMovingTheHeadBackRestoresMatchingDownstreamVersions` (versions a1,b1,c1 then a2,b2,c2; move a->1 restores b1,c1; stale empty), `TestMovingTheHeadReRunsOnlyFromTheFirstUnmatchedStep` (a1,b1,c1; b re-run to b2 then c2; a re-run to a2,b3,c3; move b->2 -> b's basis {a:1} does not match head a:2 -> stale [b, c]... write the table carefully), `TestAFailedVersionIsNeverRestored`, `TestForkAtPointsThePrefixAtTheSource`, `TestTheHeadRoundTripsThroughItsStoredForm` (float64 versions).
- [ ] `override_test.go`: every refusal code, `TestEmbeddingsIsNeverAnOverrideLevel`, `TestAnOverrideOfOnlyTheLevelIsNotAuthored`.
- [ ] `snapshot_test.go`: `TestTheNewestEffectPerPathWins`, `TestASnapshotWithAnOmittedFileIsRefusedNamingIt`, `TestAnOmittedFileLaterRewrittenIsFine`, `TestUnrecordedCommandsAreCountedNotRefused`.
- [ ] `feedback_test.go`: `TestADislikeWithoutAnAxisIsRefused`, `TestALikeNeedsNoAxis`, `TestNeutralNeverDisagrees`, `TestALikeOnAFlaggedAnswerDisagrees`, `TestADislikeOnAPassedAnswerDisagrees`.
- [ ] `sections_test.go`: `TestASectionEndingMidEffectIsRefused`, `TestASectionWithNoEndIsRefused`, `TestAReadOnlySectionWithOutputsPasses`, `TestASectionWithACataloguedReusableAutomationSpendsNoModel` (every section exact -> NeedsModel false), `TestAGoalSpecificConstructIsNeverANearHit`, `TestANearHitMustCoverTheSectionInputs`, `TestSectionSimilarityIsSymmetricAndBounded`.
- [ ] `reuse_test.go`: `TestTwoGoalSignaturesMakeAConstructReusable`, `TestOneGoalKeepsItGoalSpecific`, `TestOneAccountTieMakesItAccountSpecific`, `TestAnOverrideWinsAndTheEvidenceStillDecides`.
- [ ] `go test -count=1 ./component/work/...`; commit `Issue #5415: head moves, re-run plans, fork prefixes and workspace snapshots as pure decisions` and `Issue #5418: section boundaries, section routing and reuse labels as pure decisions`.

## Task 2: the executor and the journal -- #5415

**Files:** `core/common/modelcall.go` (RunContext fields of 1.4), `component/automations/{resume,resume_statements,sequence,journal,executor,types}.go`, `app/integrations_work_dispatch.go`, tests beside each.

- [ ] `ResumeOptions.Rerun *RerunSpec{RequestId, Reason, StepKey string; Override *common.StepOverride; Snapshot *common.WorkspaceSnapshot; Workspace string}`; `RunJournal` gains `Head`, `Rerun` (decoded from the run row) and per-key `MaxAttempt`.
- [ ] With `Rerun`: waive the failed-step requirement; `attemptBase` = `MaxAttempt[step]` for every step at or after the resume point; `withRunContext` sets `Override`/`Snapshot` only for `Rerun.StepKey`, and `Workspace` for every step.
- [ ] Journal: `version`, `basis`, `override`, `authoredBy`, the reset fields on version > 1, and the full `head` on each receipt's run write; the run's close writes `rerun: {}` and `staleSteps: []` when a re-run was served.
- [ ] `Dispatch`: a run with `rerun.reason in {rerun, headMove}` -> `ResumeFrom(FromStep: rerun.stepKey, Rerun: ...)`; a fork run (`mode == "fork"`) -> the source-prefix journal of 1.4 -> `ResumeFrom(FromStep: forkAtStepKey, Rerun: ...)`; an interrupted re-run (a running step with no receipt AND `rerun` present) resumes from that step carrying the spec.
- [ ] Tests (package `automations`, no database; fakes for the journal executor): `TestARerunExecutesFromTheStepAsNewVersions`, `TestARerunNeverReExecutesThePrefix`, `TestTheOverrideReachesOnlyTheTargetedStep`, `TestANewVersionResetsTheOldResult`, `TestEveryReceiptWritesTheWholeHead`, `TestAForkRehydratesThePrefixFromTheSourceAndRunsTheForkStepLive` (#5415 acceptance), `TestAnInterruptedRerunResumesWithItsOverride`.
- [ ] `go test -count=1 ./component/automations/... ./core/common/...` and the app package's dispatch tests; commit `Issue #5415: re-runs and forks by reference in the executor, versions and the head in the journal`.

## Task 3: the person-facing acts (`integrations/work`) -- #5415, #5416

**Files:** Create `integrations/work/{rerun,branch,head,versions,feedback,validator,snapshot}.go` + tests; modify `integrations/work/integration.go` (replace stubs), `store.go` (writers).

- [ ] `versions.go`: `(*Integration).StepVersions(ctx, runId string) ([]map[string]any, error)` -- a raw read of every `v1:work:step` row-version whose `payload->>'runId'` is the bare OR canonical run id, gated through `pctx.AdmitSourceRow` exactly as `sweep.go selectAdmitted`, folded to the newest row-version per `(id, version)`; the `workStepVersions` handler requires the caller to own the run (`readOwnRun`) and marks `current` from `run.head` (or the newest version when the head is absent).
- [ ] `rerun.go`, `branch.go`, `head.go`: the three acts of 1.4 with refusals `run_not_found`, `run_not_finished` (running, waiting, compiling), `step_not_in_run`, `step_nested`, `version_not_found`, `override_*` (from `ValidateOverride`), `snapshot_content_omitted` (naming the file). Each attaches the newest dislike on the targeted head version as `override.guidance` (D23 repair) unless the caller sent a prompt that already contains its reason.
- [ ] `snapshot.go`: for a SESSION step (its head version has a `childRunId` whose run's `automationName == "appSession"`), collect the file effects of every EARLIER session step's recording on the head (their `tool_result` observations: `contentRefs` with the path read from `args`, `contentOmitted` via the corpus parser moved to a shared helper), count earlier `exec` actions as unrecorded commands, and `work.SnapshotOf`; the snapshot and a fresh workspace (`<runId>-v<version>` for a re-run; the fork run's own id for a branch) ride `run.rerun`.
- [ ] `feedback.go`, `validator.go`: section 1.5.
- [ ] Tests (fake engine where the package allows it; the db-gated `openWorkTestEngine` for the raw read): `TestARerunOfARunningRunIsRefused`, `TestARerunOfANestedStepIsRefused`, `TestARerunWritesTheRequestOnTheRunForTheAgent`, `TestARerunAfterADislikeCarriesTheReasonInItsGuidance` (#5417 acceptance), `TestAHeadMoveReassertsTheChosenVersionAndMarksOnlyTheUnmatchedStale`, `TestABranchCreatesAForkRunPointingThePrefixAtTheSource`, `TestABranchFromASessionStepWithAnOmittedFileIsRefusedNamingIt` (#5415 acceptance), `TestStepVersionsReturnsEveryVersionOnceAndMarksTheHead`, `TestStepVersionsRefusesSomebodyElsesRun`, `TestADislikeWithoutAnAxisIsRefusedByTheAct` (#5416 acceptance), `TestALaterVerdictIsANewRow` (#5416 acceptance), `TestAFeedbackRowHasTheShapeTheCorpusReads` (round-trips through a copy of `readFeedback`'s parse), `TestTheValidatorNeverWritesFeedbackOrTouchesAConstruct` (#5416 acceptance: the recording engine sees exactly one `decision` observation and one run write), `TestTheValidatorDisagreementIsKept`, `TestTheValidatorSkipsWhenThePolicyIsOff`.
- [ ] Commit per act.

## Task 4: overrides at the model seam and in app sessions -- #5415, #5417

**Files:** `core/airoute/request.go` (`Effort`), `component/memql/{ai_request,ai_structured_result,engine_ai}.go`, `integrations/agents/{agent_turn,work_context}.go`, `integrations/agent/replier.go`, `component/router` (effort to the app client's `ModelCallStart`), `integrations/agent/worker/app_session_delegate.go`, `component/memql/app_session_provider.go`, `integrations/agent/worker/cockpitapp.go`, `component/worker/runner.go`, tests.

- [ ] The model seam and agent turns of 1.4, including `InvokeAIStructured` honouring the override; a test per knob (`TestALevelOverrideChangesTheRequestedLevel`, `TestAModelOverrideIsAOneEntryChain`, `TestPromptAndGuidanceRideAsTwoMessages`, `TestAnOverrideOnAnotherStepIsIgnored`).
- [ ] Description guidance in `workTurnHistory` and a test that replay mode never executes `workDescriptionGuidance` (#5417 acceptance: "a replay never reads description guidance").
- [ ] The delegate: stamp the REAL step id (`<runShort>-<sanitized key>` through the journal's id function, exported for it), derive the child goal key from the step id, apply the override (prompt replaces, model and effort on the wire, level), apply the snapshot (artifact ids via `libraryArtifactBySourceConceptRef`, a fresh workspace, the restore preamble). Tests: `TestTheDelegateStampsTheRealStepRow`, `TestTwoRunsWithTheSameStepKeyGetDifferentChildGoals`, `TestASessionOverrideReplacesThePromptAndSetsTheKnobs`, `TestASnapshotLandsAsInputsWithARestorePreamble`.
- [ ] Commit.

## Task 5: decomposition, reuse and the corpus -- #5418, #5417

**Files:** `dsl/planner/prompts/goalComplexityTriage.tmpl`, `integrations/planner/{agent_loop_sectionable,goal_complexity,work_compile,work_compile_draft,agent_loop_authoring}.go`, `integrations/procedure/{reuse_sweep,reuse,corpus,learn}.go`, `component/procedure/mine.go`, tests.

- [ ] Triage parse gains the section fields; `CheckSectionBoundaries` refuses a mid-effect decomposition (the goal then compiles on the author route, and the outcome records `decomposition_refused: <reason>`).
- [ ] Catalog-first per section: exact candidates by `SectionSignature` from `cataloguedConstructsForGoalSignature` and `procedureConstructsForGoalSignature` (servable rungs only) under the owner; the reusable near set from the owner's constructs whose EFFECTIVE reuse is reusable; `work.DecideSections`; the synthesized bundle calls `automation <name>(<inputs>)` (or `replayLearnedProcedure` for a procedure) for a catalog section and keeps `runAgentTurn` for an intelligence section; `CompileOutcome.ModelCalls` counts only what ran. Test `TestADecomposedGoalWhoseSectionsAreAllCataloguedSpendsOnlyTheTriageCall` (#5418 acceptance) and `TestAMidEffectDecompositionFallsBackToAuthoring`.
- [ ] Compile-time description guidance into triage and design inputs, only on the branches that call them.
- [ ] `reuse_sweep.go`: owners via `activeUserIds`; per owner `workSignedRunsForOwner`, `workAutomationStepsForOwner`, `workGoalsForOwner` (account ties), the owner's constructs; evidence = distinct goal signatures of runs whose `templateConstructId` is the construct, runs of `replayLearnedProcedure` whose `variables.procedureConstructId` is it, and runs with an automation step calling it by name; `work.DecideReuse` with the policy row; write `recordConstructReuse` only when the label or the evidence changed. `setConstructReuse`: owner-gated, writes `reuseOverride` with `version = previous + 1` (`evidence` writes `label: ""`). Tests: `TestTwoGoalSignaturesMakeAConstructReusable` (#5418 acceptance, through the sweep), `TestAnOverrideIsAVersionAndTheEvidenceKeepsCounting` (#5418 acceptance), `TestTheSweepWritesNothingWhenNothingChanged`.
- [ ] Corpus of 1.5 with `MineWeighted`. Tests: `TestASupersededRecordingLeavesTheCorpus`, `TestADislikedParentStepVersionExcludesItsRecording`, `TestALikedRecordingRanksHigherWithoutChangingTheHash`, `TestOneLikedRecordingIsStillOneUse`.
- [ ] Commit per piece.

## Task 6: Nexus -- #5419

**Design (frontend-design, inside the OS language).** The palette and type are the theme's tokens; nothing new is invented. Boldness is spent in ONE place: versions drawn ON the existing spine -- a step with more than one version carries a quiet stack of ticks beside its node (one per version, the current one inked), read with the same ink-not-hue vocabulary as the thread. Everything else is kit and quiet:

```
Runs > weekly-report run r-7f3                                   (TrailRow, published by Head)
Weekly report                                               Done
For "Summarise last week's sales"
[What this run is made of: band + spend]
 o  1  fetch        Deterministic   Done
 *  2  draft        Reasoning  |||  Done     v3 current   (ticks: v1 v2 v3)
 o  3  publish      Deterministic   Done
------------------------------------------------------------------
 draft                              Version  [v1] [v2] [v3 Current]
 Facts: Status, Level/Model/Effort asked, Served, Changed (what the override changed),
        Written by (authoredBy), Started, Duration, Tokens, Cost
 Your verdict   [Like] [Dislike] [Neutral]      Validator: Flagged - process (why)
                 on Dislike: What was wrong?  [The result] [The approach] [The behaviour]
                             Why (optional) [..................]   Save
------------------------------------------------------------------
 Step draft, version 3 of 3               Make current  Branch from here  [Run again]   (ActionBar)
```

- The run page uses kit `Head` with `back` (the hand-built header is replaced; DESIGN.md "one trail row").
- The timeline groups rows by key (the newest version in the list, the others in the step detail) instead of one row per attempt.
- The ActionBar follows the selection (rule 12): no step selected -> the run's acts (Answer it, Replay); a step selected -> `Make current` (only when the selected version is not current), `Branch from here`, `Run again` (primary). Illegal acts are absent.
- `Run again` and `Branch from here` open ONE composer dialog (the kit gains `Dialog`, promoted from `apps/deployables/page/DetailDialog.tsx` on its second use): Level (`Select` over fast/strong/reasoning, current level named), Model (a text `Input` with the entry grammar in an `InfoDetail`), Effort (`Select`), Prompt (for a session step: the recorded session prompt, editable) or Instructions (for a model step: added to the prompt), Inputs (key/value rows), and, after a dislike, the reason shown as what will be passed on. Its floor: the consequence in words ("Runs as version 4. The 2 steps after it run again.") and `Cancel` (text) + the act (button).
- Feedback: three choice pills; Like and Neutral save at once; Dislike opens the three-axis question (labels "The result", "The approach", "The behaviour", each with the framework's name in an `InfoDetail`) and a reason; `Save` is absent until an axis is chosen, and the words on the left say so. The newest verdict shows as the selected pill; the server's refusal shows verbatim in place.
- The validator: one quiet line beside the person's verdict ("Validator: passed" / "Validator: flagged the approach -- <reason>") with "It disagrees with you" when `validatorDisagrees`; the line links to `Run again` with the reason prefilled.
- Automations: a reuse fact on every construct row ("Reusable", "For one goal", "For one account"; "Your label" when overridden) and, on the construct's page, a Reuse panel (evidence facts: goals it served, accounts, uses; the override as a `Select` with "Follow the evidence").
- Overview: a new first section (`kit/Overview` + `OverviewBreakdown`) over the reads Nexus already makes: goals open, runs in flight, approvals waiting, and the constructs by reuse label with the ratio "N reusable to M for one goal" (`Figure` absence when nothing is labelled yet, never zero).
- Attention: one `attentionChanges` marker, `nexus:interventions` revision `interventions-1`, section `runs`, label "Re-run, branch and feedback", with a reachable acknowledgment on the run page's step detail.

- [ ] Tests (`test/nexus/*`, harness gains `workStepVersions`, `rerunStep`, `branchRun`, `moveRunHead`, `recordFeedback`, `setConstructReuse`, `feedbackPolicyCurrent` in the WIRE shape): every act reaches its builtin with the exact args ("every act survives", counted by label before and after), the Save rule, absent acts, the composer's consequence sentence, the validator line and disagreement, version selection, the reuse label and override, the overview ratio and its absence.
- [ ] Rendered pass through a throwaway Vite QA harness (deleted before commit), both themes, empty and populated, 1400x900 and 760x900; read every capture and fix what the pixels show.
- [ ] `make os-typecheck os-test os-build`; commit.

## Task 7: after epic D merges

- [ ] Merge `origin/main`; resolve conflicts (derived files are regenerated, never hand-merged).
- [ ] D's `renderGuidance`: append the dislike reasons on the handed-back step version (#5417 app hand-back); test.
- [ ] Nexus: the reuse panel on D's `ProcedurePage` and the reuse fact on learned rows.
- [ ] #5420 proving scenario in D's lifecycle format: extend `ProcedureGoal` with `feedback {step, verdict, axes, reason}` and `rerun {step, level, target}` entries, the fixture app answering a re-run with the corrected action, named checks `aLiftedProcedureCameFromTheLikedVersion` and `theDislikedVersionIsStillReadable`, a scenario claiming from the lifecycle claims, its control, and the driver's `WorkSpine` gaining `RecordFeedback`, `RerunStep`, `StepVersions`.
- [ ] memql-cockpit follow-up PR: honour `AppSessionStart.model`/`effort` and `ModelCallStart.effort` over the level table, pinned to the engine version that carries them.

## Task 8: documentation, the full sweep, delivery

- [ ] Root `CLAUDE.md` gets only the cross-cutting rules (an override is one version of one step; the head is re-asserted so collapsed reads see it; feedback is a `feedback` observation in D's shape; the validator never certifies) with the write-up in `docs/internal/design/feature-notes.md` if that file exists on main, else a short CLAUDE.md section. `go test -count=1 .` after editing it.
- [ ] New public page `docs/public/operate/intervention-and-feedback.md` (front matter per `docs/DOCS_STANDARD.md`), linked from `GLOSSARY.md`.
- [ ] Design record: "What shipped differently, recorded when epic E landed" (run.forkedFrom is the existing `forkedFromRunId` + `forkAtStepKey`; the head re-assertion; basis; the snapshot's restore preamble; effort needing the cockpit; section near matching is lexical and spends no model; the validator's cases).
- [ ] File follow-up issues for the defects found and not in scope: `ActRetry` is unreachable (attempt = 1 + RetryCount), a retry wait is never served on the agent, `replan`/`repair` are never wired (`wireWorkFailurePath` has no caller), the failure-path approval hash covers different fields than its recompute, `workTrace` applies no row authorization.
- [ ] Delete this plan in the last commit before the PR merges; `Closes #5414.` ... `Closes #5420.` each on its own line.
- [ ] Full verification on the FINAL tree: `make test` (captured, exit read), `MEMQL_REQUIRE_DB=1` db-gated trees against a real Postgres, `go test -count=1 -timeout=300s .`, `go run ./cmd/memqllint dsl/`, `make sdk-gen-check arch-model-check frontdoor-paths-check proto-gen-check env-registry-check concept-snapshot-check`, the module-boundaries loop, `make os-typecheck os-test os-build`, `go run ./cmd/memql-bench --do=gate` against the database, gitleaks.

---

## 2. Stream plan

| Stream | Tasks | Worktree | Starts |
|---|---|---|---|
| coordinator | 0, merges, 7, 8 | the epic worktree | now |
| S1 pure | 1 | `wt-e-pure` | after Task 0 |
| S2 executor | 2 | `wt-e-exec` | after Task 0 |
| S3 acts | 3 | `wt-e-acts` | after Task 0 (codes against 1.3 and 1.4; merges after S1) |
| S4 seam | 4 | `wt-e-seam` | after Task 0 |
| S5 decomposition | 5 | `wt-e-decomp` | after Task 0 (merges after S1) |
| S6 Nexus | 6 | `wt-e-os` | after Task 0 |

Each stream works only in its files, commits `Issue #<N>: ...`, and reports what shipped differently from section 1 (the coordinator records it in section 3 before the next merge).

## 3. Stream notes

### Task 0 (coordinator), landed at `626351aee` + `d1845f2ed`
- Every capability is a STUB in the file its stream owns: `integrations/work/{rerun,branch,head,feedback,versions,validator}.go` and `integrations/procedure/{reuse,reuse_sweep}.go`. Replace the stub function IN PLACE; never edit `integrations/work/integration.go`'s or `integrations/procedure/integration.go`'s capability list (the coordinator owns both).
- Executor names: `integration.work.{rerunStep, branchRun, moveRunHead, recordFeedback, stepVersions, validateAnswer}`, `integration.procedure.{setReuse, reuseSweep}`.
- `workValidateAnswer` takes `runId` AND `ownerUserId` (the owner hint the completion event carries, re-verified by an owner-filtered read -- `learnFromSucceededRun`'s pattern).
- `forkRun` is still present; it is retired when the Nexus stream moves to `branchRun` (coordinator, at that merge).
- A fresh worktree needs `bash scripts/identity/build-css.sh` before the root package builds.
- Gates already satisfied by Task 0: maintenance pin, automation count 73, goldens, server-only pins, prompt level pin, row-authz adjudication of `feedbackPolicyCurrent`, embed inventory 433, SDK, snapshot, arch model, proto.
