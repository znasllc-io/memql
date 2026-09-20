# App sessions as full doors, recorded, learned from, and replayed without a model -- the five-epic program

- **Date:** 2026-09-13
- **Status:** agreed with the owner in the 2026-09-13 brainstorm. Every fork below was put
  to the owner as selectable options and answered (the program shape, the atom, the
  certification rule, the replay target, the recording scope, the approach); the
  derived decisions follow from those answers and were presented section by section and
  approved. Three requirements the owner added during the same brainstorm -- intervention
  on any step with versions and branches, feedback and validation on the AI Fluency
  framework, and decomposition into reusable automations -- were presented as three more
  sections, approved, and are D18 to D24 and epic E. Issues were filed on
  2026-09-13; the table at the end of section 8 names them.
- **What it is:** the index and the design for a program of five epics. A coding-agent
  app on a fleet machine (Claude Code or Codex, headless, with MemQL's tools reachable
  over MCP) becomes a full door that a tool-needing step can be handed to (A); every
  action the app takes is recorded as rows of the work spine (B); recurring, validated
  step sequences are generalized into parameterized constructs by algorithms that spend
  no model (C); a construct climbs a certification ladder until it replays without a
  model, demoting itself when the world drifts (D); and a person can step into any run,
  re-run a step with a different intelligence or prompt, branch, and say what was wrong
  in the vocabulary of the AI Fluency framework, while long work is cut into automations
  that are labelled and reused (E).
- **Why it is pivotal:** it is the platform's own premise applied to delegated work. A
  person gives a goal; the system works it out once, records the steps as atomic
  variable-taking units, and from then on replays them without a model unless
  intelligence is genuinely needed. Today the one place that premise stops is the
  boundary of an app session, which returns artifacts and an answer and keeps everything
  it did inside a 256 KiB string. Most of what is done in programming is done over and
  over; the energy and the tokens spent once are worth recording so they are not spent
  again.
- **Repositories:** the engine half in `memql` (this record; `component/work`,
  `component/worker`, `component/router`, `component/mcp`, `component/memql`'s authoring
  pipeline, `integrations/agent/worker`, `integrations/work`, `integrations/planner`,
  `component/grpc/worker.proto`, `dsl/work`, `dsl/authoring`, `dsl/worker`, and the OS
  Work app under `clients/os`); the cockpit half in `memql-cockpit`
  (`internal/worker/harness`, `internal/worker/appsession`, `internal/worker/tools` for
  the policy file). The halves share one proto change per epic that needs one and land
  proto first, cockpit second, engine use third, as the fleet-inference record did.
- **Predecessors:** the work spine (`2026-09-05-work-spine-design.md`), the fleet
  inference and app door record (`2026-09-06-fleet-inference-and-app-door-design.md`),
  local apps as execution surfaces (`2026-08-22-local-apps-as-execution-surfaces-design.md`),
  the proving suite (`2026-09-06-proving-suite-design.md`).

---

## 1. Problem

Six things the owner wants, in the owner's words made precise:

1. **An app can do far more than answer a prompt.** Through Claude Code or Codex the
   platform reaches every tool those apps have -- a shell, a filesystem, the web, MemQL's
   own tools over MCP -- and every model and effort level they expose. Today the router
   uses an app only as a model-call door for chat and structured calls and passes it
   over for a call that needs tool turns, so the OS's Ask stays dark on a cluster whose
   only door is a signed-in Claude Code. The design of 2026-09-06 (D3) drew that line
   deliberately, because on a tool turn MemQL drives and an app is an agent that drives
   itself. The line was right for a model call and wrong for a step: the step is what an
   app should be handed.
2. **What the app does must be captured.** MemQL is repeatable by design: an automation
   is a combination of constructs, and a construct is a thing that can be run again.
   Whatever Claude Code or Codex did inside a session should be recorded so the same
   steps can be reconstructed as constructs, with provenance saying where they came
   from, and so the app does not have to be called again for the same or a similar thing.
   The recording is also the starting point for improving the procedure.
3. **MemQL learns.** A step that is small enough, has been tested enough and whose
   feedback is good needs no intelligence to run again. Composing such steps gives the
   results the person wants. The models behind an app are always the vendor's; what
   MemQL keeps is the procedure, in its own vocabulary, translated at the door into each
   app's dialect of model and effort.
4. **A person can step into any run.** After a goal ran through the fleet, every step is
   visible. A person who dislikes one step's answer goes back to it, picks a different
   model or effort or edits the prompt, runs it again, and gets a new version while the
   previous one stays; or branches from that step into something different. A step that
   is a conversation between agents is intervened on the same way, with the person
   between the two agents.
5. **Feedback is structured and it feeds the system.** A like, a dislike that asks why,
   and neutral when nobody said anything. The vocabulary of "why" is the AI Fluency
   framework's, and intelligence is used, in some cases, to validate a response before a
   person sees it, under the same vocabulary.
6. **Long work is many automations, cut for reuse.** A goal that runs for days is many
   automations; intelligence should be used correctly to cut it so the pieces are
   atomic and reusable, labelled as reusable or unique to a goal or a client, with the
   unique ones the minority. The library grows with actions and with automations, and
   both are replayed before intelligence is spent again.

What is NOT asked for: a smaller model trained on the trajectories. Trajectory
distillation certifies the same way MemQL should (a test passed) but yields a policy that
samples at replay. The artifact here stays a construct with provenance.

## 2. What the tree already has

The design builds on seams that exist. Verified on 2026-09-13 against `main` at
`8a063ec3f`; file references are the map a future session should re-check first.

### 2.1 The work spine is the recording model, and most of the vocabulary

- `v1:work:step` (`dsl/work/concepts.memql`) already carries `kind` (deterministic /
  reasoning / decision / human / loop / subrun), `call`, `input`, `result` and
  `resultFingerprint`, `binding` (skills, provider, model, surface, machine labels,
  worker and node ids), `expectedFootprint` / `actualFootprint`, `postcondition`,
  `symptom`, `idempotencyKey`, `childRunId`, `attempt`, timing, tokens and cost.
- `v1:work:observation` carries `kind` (tool_result / error / note / decision), a named
  `data` object, a `content` sentence that is the embedding source, and `embedding`.
  `integrations/work/observation.go` writes one per tool call the agent replier makes,
  with the arguments bounded at 8 KiB and truncation recorded, and deliberately without
  the tool's result.
- `component/work` is a leaf module of pure decisions: `DeriveKind` (a call is
  deterministic unless it transitively reaches a prompt), `DerivePostcondition` (a
  mutation owes `rowWritten`, a query owes its schema, and a deterministic step without
  a postcondition is refused), `UnionFootprint`, the idempotency key
  `runId:stepKey:attempt`, the three replay modes (`live`, `replay`, `fork`) decided by
  `DecideServe`, and the compile order (`GoalSignature` exact catalog hit, near match at
  0.82 with a gap list, then one triage call that answers complexity and sectionability
  together).
- The journal (`component/automations/journal.go`, `component/workjournal`) opens a run,
  writes every step at `running` before its body and again with a receipt after, never
  fails the run, and resumes from the row at `running` with no receipt. A step's row id
  is `runId-stepKey`, so a retry is a new VERSION of the same row, and rows are
  append-only: versions exist already, they are only not surfaced.
- `v1:work:approval` is the one concept for every human gate, with an artifact hash so
  an approval can never carry to a modified artifact.

### 2.2 The catalog and the lift exist; their certification half has no writer

- `v1:authoring:construct` carries `goalSignature`, `catalogued`, `catalogMatchText`,
  `reliability`, `reinforceCount` and `lastReinforced`; the exact tier of compile reads
  `cataloguedConstructsForGoalSignature`. The mutations that would write the signature
  and the reliability (`recordConstructGoalSignature`, `recordConstructReliability`)
  have no Go caller anywhere in the tree, and neither does `v1:skills:skill.status`.
  The ladder was declared and never climbed.
- The runtime authoring pipeline has four gates (isolated compile, dry run, approval and
  activation, the owner-scoped live registry) and two generators: the model path
  (`authoringEmit` / `authoringRepair`) and the deterministic path
  (`component/emailrules/generate.go`).
- `integrations/planner/agent_loop_authoring_capture.go` and
  `agent_loop_authoring_transcript.go` already lift a succeeded run's `tool_result`
  observations into an automation, one step per call, with no model, persist it as a
  validated bundle with `sourceRunId`, run the compile gate for a `reRunnable` verdict,
  and never auto-activate. It drops failed calls and renders truncated arguments as an
  empty object, so the largest calls lose their arguments silently.

### 2.3 The app session is a black box on purpose, and the wire is not

- `AppSessionStart` carries session id, app, kind, prompt, inputs, workspace,
  credential, MCP endpoint, limits, run id, step id, app session ref and
  `response_schema_json`; `AppSessionChunk` carries `stream` (stdout, stderr, event)
  and a monotonic `seq`; `AppSessionEnd` carries exit code, usage, produced artifact ids,
  error and `result_json` (`component/grpc/worker.proto`).
- `component/worker/runner.go` drains chunks into one flattened `transcript` string on
  `v1:worker:appSession`, bounded at 256 KiB, discarding stream and sequence. Event
  chunks are structured JSON from the harness and reach only a transient progress event.
- The MCP node (`component/mcp/tool_surface.go`) exposes the engine's DSL tools 1:1 plus
  `submit` and `next_task` for an app-session bearer, and writes no observation: the
  recorder wraps the agent replier only, and nothing on the MCP path stamps a run.
- One session maps to at most one step (`appSession.runId`, `appSession.stepId`);
  `binding`, footprints, postcondition and symptom stay empty for delegated work.

### 2.4 The Cockpit's harness protocol is already per app

`internal/worker/harness/claudeheadless.go` drives Claude Code as one process per turn
under stream-json output with `--json-schema` for a structured final answer and
`--resume` across turns; `codexappserver.go` and `codexmcp.go` drive Codex. Neither
passes a model or an effort. Claude Code's headless output emits one JSON object per
line: assistant messages whose content blocks are text, thinking or `tool_use` with an
id, a name and an input; user messages carrying `tool_result` with the matching id, the
content and an error flag; and a terminal result with `structured_output`, usage, cost,
turn count and session id. Codex's non-interactive mode emits items with ids and a
completed status: `command_execution` (command, aggregated output, exit code),
`file_change` (paths and kinds), `mcp_tool_call` (server, tool, arguments, result),
`agent_message`, `reasoning`. Both vendors declare their on-disk transcripts internal;
the streamed events are the contract.

### 2.5 What the research found

Three lanes were run on 2026-09-13 (a codebase map, the agent-memory literature, the
algorithms for generalizing traces). The findings that shaped the decisions:

- **No published system does the whole loop, and the pieces exist.** Agent Workflow
  Memory (Wang, Mao, Fried, Neubig, 2024; arXiv:2409.07429) induces workflows with
  placeholders from successful trajectories and gains 24 to 51 percent on web
  benchmarks, but the workflow is guidance for a model and its online certification is
  a model judge that the authors admit produces wrong workflows. Voyager (2023;
  arXiv:2305.16291) and SkillWeaver (2025; arXiv:2504.07079) store executable skills
  that replay without a model and verify them by execution; SkillWeaver keeps
  preconditions on each skill and filters by them at selection. Agent Skill Induction
  (2025; arXiv:2504.06821) certifies a skill by rewriting the source trajectory to call
  it, re-executing, and requiring correctness, usage and validity before it enters the
  action space. CRAFT (2023; arXiv:2309.17428) abstracts a solved instance into a
  parameterized tool and re-solves the original with it. The most complete certification
  regime found is GSE (Yang et al., 2026; arXiv:2608.06153): replay the originating case
  with the proposal applied, then replay every historical case the skill would have
  claimed, and merge only if performance holds.
- **The literature is unambiguous that a model judge is a pre-filter, not a
  certifier.** Reflexion documents self-written tests passing on wrong solutions;
  SkillLearnBench (2026; arXiv:2604.20087) documents recursive drift under
  self-feedback; the personalized-skills study (Huang, Du, Lan, 2026;
  arXiv:2608.10319) shows per-user induction over-generalizing from thin history, with
  benefit only after six or more prior sessions and a rule that a candidate needs at
  least two independent turns behind it. Misevolution (Shao et al., 2025;
  arXiv:2509.26354) shows learned tools and memories eroding safety. CoALA (2023;
  arXiv:2309.02427) names writing to procedural memory the riskiest kind of learning.
  ExpeL (2023; arXiv:2308.10144) and ACE (2025; arXiv:2510.04618) keep helpful and
  harmful counters on what they learned, which is the shape feedback takes here.
- **The algorithms are old, small and well understood.** Robotic process mining (Leno,
  Polyvyanyy, Dumas, La Rosa, Maggi, 2021) is the closest analogue of the whole
  pipeline: a UI log's parameters are split into context parameters, stable across
  executions, and data parameters that vary; the log is normalized, segmented at the
  back-edges of its directly-follows graph, mined with a closed sequential pattern
  miner, and ranked by frequency, length, coverage and cohesion. Anti-unification
  (Plotkin, 1970; Cerna and Kutsia survey, 2023; arXiv:2302.00277) is the operation "two
  concrete steps become one template with variables", and Bulychev and Minea's
  generalization-distance budget (2008) is what stops it collapsing to a bare variable.
  The Inductive Miner (Leemans, Fahland, van der Aalst, 2013) turns many traces into a
  sound block-structured model with choices and loops; token-based replay gives a
  per-trace fitness cheaply and alignments name the deviation. Stitch (Bowers et al.,
  2023; arXiv:2211.16605) scores an abstraction by the compression it buys across its
  uses minus the cost of its body and its arguments, which is the one-line statement of
  "compression equals reuse". PrefixSpan, BIDE+, SEQUITUR and Re-Pair find recurring
  sub-sequences in linear or near-linear time. Skill chaining (Konidaris and Barto)
  learns a skill's initiation set as a classifier over the states it succeeded from,
  which is the precondition of a procedure. MACROPS (Fikes, Hart, Nilsson, 1972) is the
  original: generalize a successful plan by replacing constants with parameters, store
  it, execute it under per-step precondition monitoring.
- **Determinism has three primitives:** an environment fingerprint over the observed
  inputs, content-addressed step inputs so a replay can check "same input as recorded"
  before acting, and idempotency keys on side-effecting steps so a divergence-triggered
  retry cannot double-apply. MemQL already has the third.
- **The AI Fluency framework** (Dakan and Feller, with Anthropic; the course "AI
  Fluency: Framework and Foundations") names four competencies: Delegation (problem
  awareness, platform awareness, task delegation), Description (product, process and
  performance description), Discernment (product, process and performance discernment,
  and the description-discernment loop), and Diligence (creation, transparency and
  deployment diligence). Its Discernment axes are the vocabulary of feedback here, and
  the other three competencies turn out to be structural in the program already.

## 3. Decisions

The first six were put to the owner and answered; D7 to D17 follow from them; D18 to
D24 record the three requirements the owner added and the sections approved for them.

### D1 -- One program record, five epics, shipped A to E, one PR each

Recording is useless without the door, learning is useless without recordings,
certification is meaningless without something to certify, and intervention, feedback
and decomposition act on rows the earlier epics create. Each epic is one PR, as the
owner asked for every epic since 2026-09-07. Chosen over two larger epics (door, then
learning) because a two-epic cut lands learning only when the whole loop is proven, and
over designing B to D first because the door decides what a session is and therefore
what a recording is. The program was agreed as four epics; E was added in the same
brainstorm when the owner added its three requirements.

### D2 -- Tool calls are the atoms; sessions compose them

Every tool call the app makes is a step row with its arguments and a result digest.
Recurring sequences become procedures; a whole session becomes a procedure of
procedures. Chosen over the session as the atom (nothing inside reusable, any change
means running the app again) and over the app's inferred sub-tasks as atoms (inferring
intent from the app's prose is the weakest signal in the transcript).

### D3 -- Thresholds propose, a person promotes once, demotion is automatic

A procedure certifies itself through shadow replays and asks a person to approve
exactly one transition, shadow to canary. After that yes it climbs to trusted on its
own evidence, and a failed replay or a precondition that proved insufficient demotes
it without asking. This keeps the work spine's D5 (never a silent edit) and still gets
hands-off after the first yes. Chosen over fully automatic (a bad generalization
replays unattended until it fails) and over always-a-person (the approval queue becomes
the bottleneck of learning).

### D4 -- The footprint decides where a procedure replays

A procedure whose footprint is portable (workspace files, MemQL tools, the network)
replays in the sandboxed workbench in the cluster. One whose footprint names a
machine's own files or apps replays on that machine through the worker. The
environment fingerprint is compared before either, and a mismatch falls back to the
app. Chosen over always-the-machine (the laptop has to be on, the procedure is tied to
one owner's hardware) and always-the-workbench (anything needing the machine's files,
credentials or installed tools can never replay).

### D5 -- Actions verbatim, contents content-addressed in the Library, prose as a pointer

Every action row keeps its arguments whole and its result as a digest plus an inferred
JSON type. File contents the app read or wrote go into the Library as content-addressed
files owned by the machine's owner and are referenced by hash from the step. The
model's own text is one transcript artifact per session, not rows, so the spine stays
about actions. Chosen over everything-as-rows (the spine fills with prose nobody
replays, the owner's file contents sit inline in rows) and over actions-only-with-hashes
(a procedure that has to re-create a file cannot, so those steps always fall back).

### D6 -- Rows in, constructs out, algorithms in a leaf Go module

Induction spends no model. A new pure module, `component/procedure`, holds the
algorithms as functions over values; `integrations/procedure` is the wiring that reads
rows and writes constructs through the existing lift and pipeline. Approach 2 (a prompt
writes the constructs, execution certifies) is allowed for exactly one thing, deriving a
hole the rules cannot explain, and never for certification. Approach 3 (an external
mining sidecar with Python libraries) is kept as a research harness for validating the
Go implementation against reference algorithms, never as product: it would put a
second runtime into product-agnostic engine images and make the learning something
other than rows.

### D7 -- A tool-needing call resolved to an app door becomes a session subrun

When the router's chain resolves a call whose modality needs tool turns to an `app:`
entry, the step is handed to an app session instead of the door being passed over. The
step gets a `childRunId`; the session gets the prompt, inputs, workspace, MCP endpoint,
limits and response schema from the step; the step's result is the session's structured
answer plus its produced artifacts. Chat and structured calls keep the model-call shape
of the 2026-09-06 record. The 2026-09-06 D3 sentence survives for what it was about: a
model call with tools is never proxied through an app, because two agents cannot drive
one loop. A step is not a model call.

### D8 -- The level rides the app wire; the Cockpit owns the per-app translation

The engine puts the call's level on `AppSessionStart` and on `ModelCallStart`. The
Cockpit holds one table per app, level to knobs: for Claude Code `--model` and
`--effort` (low, medium, high, xhigh, max, verified on the installed 2.1.270); for Codex
its model flag and its reasoning-effort configuration key, whose exact spelling is
verified against the pinned Codex version at implementation time. The table lives on
the machine side because the knob names are the app's, and the machine owner may
override it in `policy.yaml` beside `apps.allow`. A policy may pin an app model with an
entry `app:<id>:<model>`, the way `fleet:<modelId>` pins a fleet model; the entry
grammar gains that one form and nothing else.

### D9 -- Provenance is a stamp on the decision record and on every artifact

`v1:router:call` gains the model and effort the app reported. Every artifact a session
produces, and the transcript artifact itself, is stamped with app, model, effort and
session id. A lifted construct's source reads that stamp, so its provenance says
"recorded from claude-code, model X, effort Y, session Z", and the OS can say so.

### D10 -- Embeddings never go through an app; native image generation is not an app door

An embedding must come from the same embedder as the index it is written into, and
neither app exposes one. A native image call belongs to a local image runtime or to
federation with its own level. An app may still produce an image by code or through a
tool, which is a session artifact like any other.

### D11 -- Vision through an app is a harness feature, not a router change

Claude Code reads images natively. A vision call through the app door writes the image
into the session workspace and references it from the prompt. It is in scope for epic A
only if it costs no wire change; otherwise it is the first follow-up.

**Shipped as the follow-up, memql#5523.** The WIRE test passes: `AppSessionStart.inputs`
already carries Library artifact ids the cockpit pulls into the workspace, and the landing
filename is the engine's to choose, because the cockpit reads `Content-Disposition` and
`GET /artifacts/{id}/content` sets it from the file row's `name`. The cost is elsewhere and
this decision did not anticipate it: `inputs` takes Library artifact ids and NOTHING ELSE,
so every image in every vision turn becomes a permanent, owned, listed `v1:library:file`
plus its index row, and the model call cannot start until the asynchronous
`indexFileOnCreate` promotion has produced that index row -- the artifact id is deliberately
not derivable in Go. A person's Files app filling with transient inputs, and an async
automation on the critical path of every vision turn, are product decisions rather than
implementation details, and neither was put to the owner.

### D12 -- One recording format for both apps, keyed by the app's own ids

Claude Code's `tool_use` / `tool_result` pairs and Codex's completed items both carry
stable ids. The observation keeps the app's id, the session id, the sequence, the
working directory, the exit code and the error flag, and the raw event. The Cockpit
emits one `event` chunk per completed action, normalized to a single shape across apps,
so the engine never parses vendor formats; the engine's session runner writes the rows.

### D13 -- The hole classification order is data flow, then constant, then free parameter

A hole in a generalized template is explained in this order: derived from an earlier
step's result in every instance (a data-flow reference); constant across every instance
(a context parameter, kept literal); varying with no derivation (a free parameter of the
procedure). A free parameter is priced above a data-flow hole in the abstraction score,
because a free parameter is something the caller has to supply. Only a hole that none
of the three explain may go to the one bounded model call of D6, and its proposal must
hold on every recorded instance before it is kept.

### D14 -- Compression is the score, two uses is the floor, and unused procedures retire

An abstraction is accepted when the compression it buys across its uses exceeds the
cost of its body and its arguments, and it has at least two uses. The library grows one
abstraction at a time, the corpus is rewritten, and the loop repeats, so abstractions
reference earlier ones and the hierarchy emerges. A procedure that no goal has used
inside a retention window retires from the catalog, because a library whose
applicability checks cost more than they save is the utility problem Minton described
in 1990 and TroVE trims for.

### D15 -- The ladder is candidate, shadow, canary, trusted, and deny wins

Recorded on the construct's own reliability fields, which exist today with no writer.
Candidate: at least two uses, every hole classified. Shadow: on the next matching goal
the app still runs, the procedure replays in a sandbox beside it, and step results are
compared, exactly for deterministic actions and by inferred type where the successful
recordings themselves varied; promotion needs `m` consecutive matches across at least
`k` distinct bindings of every free parameter. Canary: the procedure runs for real with
the app on standby and every step's postcondition checked as it goes. Trusted:
model-free replay under the same postconditions. The one human approval is a
`v1:work:approval` of kind `procedurePromotion` whose artifact hash pins the construct
version; a later change of the construct is a new candidate. The record proposes
`m = 5` and `k = 2` as VALUES, in the ladder's own configuration rows, never as constants
in code. Feedback enters the ladder as D23 says.

### D16 -- Preconditions are learned initiation sets, and drift is caught three ways

A procedure's preconditions are the environment predicates that were true at every
successful start: tool versions, the working-directory listing digest, the relevant
variables, the content digests of the files it read. Before a replay the fingerprint
and each step's content-addressed inputs are compared to the recording; after each
step its postcondition; and a running token-replay fitness of the live trace against
the goal's process model catches a deviation cheaply, with an alignment computed only
then to name the log move or model move where it diverged. On divergence the replay
stops, the idempotency key prevents a double side effect, and the partial trace plus the
diagnosis go to the app as guidance, which is repair rather than resample; the repaired
run is a new recording. Two failed replays, or one precondition that proved
insufficient, demote a trusted procedure to shadow.

### D17 -- Owner-scoped, composite tier, no new concept for the recording

Everything stays on `@rowAuthz(owner="ownerUserId", clusterOwner)`. Procedures are
owner-scoped in this program; sharing a certified procedure across people is a later
decision, never a default. The recording adds fields to `v1:work:step`,
`v1:work:observation` and `v1:worker:appSession` and adds no concept; the library is
`v1:authoring:construct`; the approval kind is new. Every field is additive, so no
stored row is bricked (memql#5199).

### D18 -- Every step version is kept; re-running in place moves the run's head

A step re-run with overrides is a new version of the same step row (the journal's
`runId-stepKey` id, append-only rows). Every step after it becomes stale and re-runs
from its new upstream, each as a new version. The run carries a head that names the
current version of every step; going back is a mutation that moves the head to an
earlier version and marks the downstream receipts stale. Nothing is ever deleted, and
"the previous version is still there" is a property of the store, not a feature.

### D19 -- Branching from a step is a fork run; a session step branches into a new session

A branch is a new run in the existing `fork` replay mode: the shared prefix is served
from the journal by reference, the fork step runs live with the person's changes, and
the original run is untouched. When the fork step is a delegated session step, the
branch is a new app session started with the edited prompt against a snapshot of the
workspace as it was before that step, which the Library holds content-addressed. That is
what a person standing between two agents does in rows.

### D20 -- Overrides are per version, per step, and authored

An override names a level, a model, an effort, a prompt or inputs, and applies to one
version of one step; it never leaks into the next step. The level, model, effort and
author of every version are on the row, so a procedure lifted later says which version
it came from, and a version a person authored is marked as such and is never a
recording of the app.

### D21 -- Feedback is an observation on Discernment's three axes, and it never overwrites

A verdict on a step version or on a run is a `v1:work:observation` of kind `feedback`:
like, dislike or neutral, absent meaning unseen. A dislike asks one question before it
is saved, whose answers are the framework's product, process and performance
discernment, plus a free-text reason. Feedback is owner-scoped and versioned; a new
verdict is a new row, never a rewrite of an earlier one.

### D22 -- AI-assisted discernment is a pre-filter, never a certifier

One bounded validation call at the run's level may apply the same three axes to a
step's answer against its description (prompt, schema, postcondition) before a person
sees it, and record a `decision` observation with a verdict per axis and a reason. It
may hold a result for review, raise a repair with the reason as guidance, or annotate.
It never promotes a procedure and never counts as a like; the person's verdict outranks
it; a disagreement between the two is kept as the signal that the validator prompt
needs work. This is the literature's rule (Reflexion, SkillLearnBench, AWM) made
structural.

### D23 -- Feedback feeds four places, and text reaches only a model call

Certification: a procedure whose corpus contains a disliked step version stays a
candidate until a liked or neutral version of that step exists in every instance, and a
like on a replayed step is a reinforcement. Repair: a dislike's axis and reason ride as
guidance when the step is re-run, branched or handed back to the app. Learning: disliked
recordings leave the generalization corpus and liked ones rank higher. Description:
dislike reasons accumulate on the goal signature as description guidance and are
injected only when a model is genuinely used for that goal again, never into a replay,
because a replay reads rows and text is not a row it can act on.

### D24 -- Decomposition spends intelligence once, and reusability is decided by evidence

The compile order's triage produces a decomposition: named sections with inputs,
outputs, a one-line purpose and a reuse intent. Before any section is planned live the
catalog is asked for it: an exact hit on the section's signature, a near match against
reusable automations only, then intelligence. A section boundary must coincide with a
footprint boundary and a postcondition; a section that ends mid-effect is refused.
`construct.reuse` is a closed enum, `reusable` / `goalSpecific` / `accountSpecific`;
the decomposer proposes, evidence decides (two or more distinct goal signatures within
the owner's scope make it reusable; one goal keeps it goal-specific; an account tie
makes it account-specific), a person may override, and the override is a version while
the evidence keeps counting. The mining corpus carries symbols at both levels, an
action inside a session and an automation invocation as a subrun step, so the one
pipeline lifts repeated automation sequences into higher automations by the same
compression rule.

## 4. The five epics

Each epic names its change by path, its wire change, its failure modes, its tests and
its delivery. Numbers written as values are values.

### Epic A -- The app door completion

**Scope.** D7, D8, D9, D10, D11. Codex parity from the first PR: both harnesses exist,
the translation table and the recording shape are per app.

**Engine (`memql`).**

- `component/router`: at the point where `servesModality` passes an app door over for a
  tool-needing modality, return a winner of a new door shape, `session`, instead of
  continuing; the chain winner carries the app id. `component/memql`'s call site that
  receives a `session` winner opens the delegated step through
  `integrations/agent/worker`'s `CockpitAppExecutor.Run`, which today has no production
  caller on this path, with the step's prompt, inputs, workspace, MCP endpoint, limits
  and response schema, and records `childRunId` on the step.
- `component/grpc/worker.proto`: `level` on `AppSessionStart` and `ModelCallStart`;
  `model` and `effort` reported on `AppSessionEnd` and `ModelCallEnd`.
- `component/router` and `dsl/policies`: the entry form `app:<id>:<model>`, refused at
  load for an app id outside the closed runnable set, with the message naming the set.
- `v1:router:call`: `servedModel` and `servedEffort`. `v1:library:artifact` and the
  Library file row: a `producedBy` object (app, model, effort, sessionId), stamped by
  the session runner at end.
- `core/airoute`: the vocabulary stays the four levels; nothing new is declared at a
  call site.

**Cockpit (`memql-cockpit`).**

- `internal/worker/harness`: `Spec` gains `Level`; `claudeArgv` maps it through the
  table to `--model` and `--effort`; the Codex harnesses map it to the model flag and the
  reasoning-effort key. The reported model and effort come back from the result event
  (`modelUsage` on Claude Code; the turn usage on Codex) onto `AppSessionEnd`.
- `internal/worker/tools/policy.go`: an `apps.levels` block, per app, level to knobs,
  overriding the built-in table; default-deny does not apply here, an absent block means
  the built-in table.

**Failure modes.** An app that ignores the effort flag is reported as served at the
level requested with `effort` empty, never guessed. A `session` winner whose machine is
asleep parks the step with the existing `inferenceUnavailable` approval rather than
falling to federation silently, which is the 2026-09-06 park rule. A `session` winner on
a call with no step (a bare model call from Go) is refused at resolution, naming the
modality, because there is no step to hand over.

**Tests.** A router test that a tool-needing request resolving to `app:claude-code`
yields a `session` winner and never a federation entry when one is behind it in the
chain. A harness test that each level maps to the documented knobs on each app and that
an unknown level refuses. A decision-record test that `servedModel` is the app's
report and not the request's pin. An artifact test that `producedBy` is present on every
artifact of a session and absent on one uploaded by a person.

**Delivery.** One PR in `memql` after the proto change, one in `memql-cockpit`, the
cockpit's pin moved in the same commit as its go.mod, as the pin file requires.

### Epic B -- Recording

**Scope.** D2, D5, D12, D17.

**Engine (`memql`).**

- `dsl/work/concepts.memql`: `step.stepType` gains the action names `exec`, `fs_write`,
  `fs_read`, `fetch`, `mcp`, `app_answer`; `step.fingerprint` (an object: tool versions,
  cwd digest, variables named by the harness, files-read digests) written once on the
  session's first step; `observation.data` gains `appActionId`, `sessionId`, `seq`, `cwd`,
  `exitCode`, `isError`, `argsRef`, `resultDigest`, `resultType`, `contentRefs[]`
  (Library file ids), and the 8 KiB cap on `args` goes: arguments are stored whole, with
  the same `argsTruncated` marker kept only for the pathological case above a hard
  ceiling that is a value.
- `component/worker/runner.go`: the transcript collector stops flattening. `event`
  chunks are decoded into action rows through a new `integrations/work` writer that
  opens the session subrun (`childRunId` on the delegating step), writes one step and
  one observation per action under the owner's borrowed authority with internal origin
  stamped, and closes the subrun at `AppSessionEnd` with the structured answer as the
  final `app_answer` step. stdout and stderr go to the transcript artifact, one Library
  file per session, referenced from the run; `v1:worker:appSession.transcript` and
  `transcriptBytes` are retired in favour of `transcriptArtifactId`.
- `component/mcp`: the app-session bearer's credential label already names the session;
  `callMCPTool` stamps a run context from it so an MCP call the app makes lands as an
  `mcp` step with the tool's arguments and result digest, written by the same writer.
  The `submit` and `next_task` tools are excluded from recording, being the protocol
  rather than the work.
- File contents: an `fs_write` action's content and an `fs_read` action's content are
  stored by the session runner as content-addressed Library files owned by the machine's
  owner, `source` `appSession`, with `producedBy` from epic A; the observation references
  them by id. A content above the Library's per-file cap is referenced by digest only,
  with `contentOmitted` recorded. The workspace snapshot before each session step that
  D19 branches from is the set of those content-addressed files, so a branch costs no
  copy.

**Cockpit (`memql-cockpit`).**

- `internal/worker/harness`: one normalized action event per completed tool call, for
  both apps: `{id, seq, tool, args, cwd, exitCode, isError, resultDigest, resultType,
  contentInline?}`, emitted as an `event` chunk; the fingerprint emitted once as the
  first event. The transcript prose stays on stdout.

**Wire.** No new message. `AppSessionChunk.stream` and `seq` are already there; the
change is that they are honoured.

**Failure modes.** A session whose owner cannot be resolved is refused before it starts,
not recorded under a blank actor (the workbench's `workspace_owner_unresolved` rule).
An action event arriving out of order is dropped, as chunks already are, and the gap is
recorded on the run so a lifted procedure knows it is incomplete. A Library write that
fails records `contentOmitted` on the observation and never fails the session.

**Tests.** A runner test that a session of N actions writes N action steps and N
observations with monotonic `seq` and one transcript artifact. An MCP test that a tool
call under an app-session bearer writes an `mcp` step and that `submit` writes none. A
Library test that an `fs_write` content is content-addressed (two identical writes, one
file) and owned by the machine's owner. A cross-node test that the rows written on the
agent replica reach a subscription on the bff replica, in the existing forward hop
harness.

### Epic C -- Learning

**Scope.** D6, D13, D14, and the two-level corpus of D24.

**The module.** `component/procedure`, pure, importing only the standard library and
`component/work`, asserted by the same build-graph test that pins the proving
sub-packages. Its functions, in pipeline order:

1. `Canonicalize(steps) []Action` -- each step becomes a tree of tool, argument tree,
   result digest and effect digest; argv, JSON and paths are parsed into trees; pure
   reads whose result nothing later consumed are dropped as noise. An automation
   invocation (a subrun step) canonicalizes to its construct name and its bound
   arguments, so it is a symbol like any action.
2. `Symbolize(actions, budget) []Symbol` -- steps with the same tool are anti-unified
   pairwise (objects paired by key, arrays by longest common subsequence, a typed hole
   per mismatch); a step joins a cluster when the generalization distance stays under
   the budget; the cluster id is its symbol.
3. `Mine(sequences, minSupport, gap) []Pattern` -- closed frequent sub-sequences with a
   gap tolerance, ranked by coverage and cohesion before frequency, removed and re-mined
   until nothing clears the bar. `Structure(sequences, noise) ProcessTree` -- the
   inductive miner per goal signature, so a retry loop is a loop node.
4. `Generalize(instances) Template` -- anti-unify the candidate's instances into a
   template with typed holes; `Classify(template, instances) []Hole` -- D13's order.
5. `Score(template, corpus) Utility` -- D14's compression utility; `Select(candidates)`
   accepts the max-utility abstraction, rewrites the corpus, repeats.

**The wiring.** `integrations/procedure` runs on `graph.node.updated.v1:work:run`
reaching `succeeded` for a session subrun, and on a schedule over the corpus of
recorded sessions and runs per goal signature, with disliked recordings excluded and
liked ones weighted (D23). It renders the selected template through the existing
deterministic lift (`renderTranscriptAutomation`, extended to take a template with
holes) into an automation whose steps call `workbenchDispatchHost` or `workerHost`,
MemQL's own tools, or lower automations, with the free parameters as its `args` and the
data-flow holes as step references, persists it as a validated bundle with
`sourceRunId`, `goalSignature` (finally written), and the provenance stamp from D9, and
runs the compile gate. Nothing auto-activates; the construct enters D15 as a candidate.
The one bounded model call of D6 is a prompt at level `reasoning` whose output schema
is a derivation expression, checked against every instance before it is kept.

**Failure modes.** A corpus of one session yields no procedure (two uses is the floor);
the session is still lifted as a one-off validated bundle, as today. Symbolization that
collapses everything into one cluster (a budget too loose) is caught by a test that two
unrelated tools never share a symbol. A pattern whose holes are all free parameters
scores below the floor by construction. The model fallback that proposes a derivation
holding on some instances and not others is rejected, and the hole stays free.

**Tests.** Golden tests on fixtures for each function, with a NEGATIVE control per
claim: a corpus with no repeats yields no pattern; two traces differing only in a
literal yield one template with one hole classified free; a trace whose literal equals
an earlier result yields a data-flow hole; a retry loop yields a loop node and not a
long pattern; two runs sharing a sequence of three automations yield one higher
automation. A parity harness (the research sidecar of D6) that runs the same fixtures
through reference implementations and asserts the Go results agree, kept under
`component/procedure/reference` and skipped when the references are absent.

### Epic D -- Certification and replay

**Scope.** D3, D4, D15, D16, D14's retirement, and D23's certification half.

**Engine (`memql`).**

- `dsl/authoring/concepts.memql`: `construct.ladder` (a closed enum: candidate, shadow,
  canary, trusted, retired), `construct.preconditions` (the learned initiation set),
  `construct.shadowMatches`, `construct.distinctBindings`, `construct.failures`,
  `construct.lastReplayAt`; the existing `reliability`, `reinforceCount` and
  `lastReinforced` are written at last, by `recordConstructReliability`. A
  `v1:authoring:ladderPolicy` singleton row at a literal id carries `m`, `k`, the
  demotion counts and the retirement window as values.
- `v1:work:approval`: kind `procedurePromotion`, artifact hash over the construct's
  source and its preconditions.
- `component/work`: `DecideServe` gains the ladder as an input: a trusted construct on
  an exact goal signature hit serves the steps from the construct; a shadow one serves
  the app and runs the construct beside it in a sandbox; a canary one serves the
  construct with the app on standby. The comparison function (exact for deterministic
  actions, by inferred type where the recordings varied) is pure. The candidate gate
  reads feedback: a disliked step version in the corpus holds the construct at
  candidate (D23).
- `integrations/procedure`: the replay runner chooses the target by footprint (D4),
  compares the fingerprint and content-addressed inputs before each step, checks the
  postcondition after, keeps the running replay fitness, computes the alignment on a
  drop, and on divergence stops, hands the partial trace and the diagnosis to the app
  session as guidance, and records the repaired run as a new recording. Demotion and
  retirement are its two sweeps, both in `maintenanceAutomations`, because the
  composite tier answers zero rows and no error under the default reader.
- The OS: the Work app shows the ladder state on a construct, the approval card for
  `procedurePromotion`, and a procedure's provenance stamp; the Settings screen shows the
  ladder policy values.

**Failure modes.** A precondition that never held on a later start is a demotion, not a
retry. A replay on the wrong target (a machine-local footprint sent to the workbench)
is refused before the first step by the footprint check. A side effect after a
divergence is prevented by the idempotency key; the proving suite's figure for it must
read zero. A canary that diverges on a side-effecting step reports the step and its
idempotency key so the person can see what did and did not run.

**Tests.** The ladder transitions as a pure state machine with every guard. A shadow
run that matches `m` times across `k` bindings proposes and one that matches `m` times
on one binding does not. A trusted procedure whose fingerprint mismatches falls back to
the app and records the mismatch. A candidate with a disliked instance step does not
propose until a liked version of that step exists. The proving suite gains
`amortizedCost.replaysServedWithoutModel` paired with the negative control that a
shadow-only construct serves none, and `durability.duplicatedSideEffectsAcrossDivergence`
that must read zero.

### Epic E -- Intervention, feedback and reusable decomposition

**Scope.** D18 to D24. Its intervention and feedback tasks depend on B (the rows exist);
its decomposition tasks depend on C (the two-level corpus) and D (the ladder). It ships
after D as one PR; its plan may start the intervention and feedback tasks beside C.

**Engine (`memql`).**

- `dsl/work/concepts.memql`: `run.head` (an object naming the current version of every
  step, by step key); `step.version` (the attempt number surfaced as a first-class
  field), `step.override` (level, model, effort, prompt, inputs, all optional) and
  `step.authoredBy` (a user id when a person wrote the version's input); `run.forkedFrom`
  (run id and step key) on a branch. `observation.kind` gains `feedback`, with
  `data.verdict` (like / dislike / neutral), `data.axes` (product, process, performance,
  each a boolean), `data.reason`, `data.target` (a step version or the run).
- `component/work`: `MoveHead(run, stepKey, version) (head, stale)` and
  `ForkAt(run, stepKey, override) RunSpec` as pure decisions; `DecideServe` reads the
  head so a re-run serves the prefix from the current versions; the existing
  `BeforeForkPoint` is the branch. `Decide` (the compile order) gains the decomposition
  tier: sections with reuse intent, catalog-first per section, the boundary rule
  (footprint boundary and postcondition, refused mid-effect).
- `integrations/work`: `rerunStep` (a new version with the override, downstream marked
  stale and re-run in order), `branchRun` (a fork run; for a session step a new app
  session against the content-addressed workspace snapshot), `recordFeedback`, and the
  validation call of D22 as an automation step at the run's level with the schema
  `{product, process, performance, reason}`.
- `dsl/authoring/concepts.memql`: `construct.reuse` (closed enum: reusable,
  goalSpecific, accountSpecific), `construct.reuseEvidence` (distinct goal signatures
  seen, account ties), `construct.reuseOverride` (a person's label, versioned).
- `integrations/procedure`: the reuse sweep that decides the label from evidence, in
  `maintenanceAutomations`; the description-guidance store on the goal signature (D23),
  read by the prompt assembly only when a model call is made for that goal.
- The OS Work app: the step timeline with every version selectable, "Run again with"
  (level, model, effort, prompt, inputs), "Branch from here", the like and dislike
  controls with the three-axis question and a reason on dislike, the validator's
  verdict shown beside the person's, the reuse label with an override, and the ratio of
  reusable to goal-specific constructs on the Work app's overview.

**Cockpit (`memql-cockpit`).** None beyond epic A's level and epic B's events: a re-run
or a branch of a session step is a new `AppSessionStart`.

**Failure modes.** Moving the head to a version whose downstream receipts are stale
re-runs only what is stale, never the whole run. A branch of a session step whose
workspace snapshot has a `contentOmitted` file is refused, naming the file, because a
branch from a partial snapshot would diverge silently. A dislike saved without an axis
is refused by the mutation: the question is the point. The validator disagreeing with
the person is recorded, never resolved automatically. A reuse override contradicting
the evidence is kept as the person's label and the evidence keeps counting, so the OS
can show both.

**Tests.** A head-move test that only stale steps re-run. A fork test that the prefix is
served and the fork step runs live with the override on that version only. A feedback
test that a dislike without an axis is refused and that a later verdict is a new row. A
validator test that its verdict never changes the ladder. A decomposition test that a
section ending mid-effect is refused and that a section with a catalogued reusable
automation spends no model. A reuse-label test that two goal signatures make a
construct reusable and an override is a version.

## 5. Cross-cutting rules

- **Cross-node.** Rows are written on the agent node and read on the bff; the new fields
  ride the routing rules that already carry the work spine's events, and the recording
  writer stamps internal origin the way the journal does. The replay runner runs on the
  agent for a worker target and on the workbench path for a workbench target; neither
  holds state a sibling replica would need. Feedback is written on the bff by the person
  and read on the agent by the sweeps, the same hop the other way.
- **Authorization.** Composite owner tier everywhere; the MCP-recorded step is owned by
  the session's owner, resolved from the credential label, never from the call. Feedback
  is the owner's; account members' feedback is a later decision.
- **No environment branching.** Nothing here names a deploy tier; the replay target is a
  footprint fact, and the log-only decisions of other subsystems are untouched.
- **Vocabulary.** A procedure is a construct. It is not a fourth extension word; a
  library of procedures is the catalog. The literature's word "skill" is not adopted for
  it, because `v1:skills:*` already means something else in this tree. "Intelligence"
  in this record means whatever answers a step that rows cannot: a model, an app, or a
  person; the premise is that it is spent only when necessary.
- **Values, not constants.** `m`, `k`, the demotion counts, the retention window, the
  symbolization budget, the mining support and gap, the argument ceiling and the reuse
  evidence threshold are rows or manifest values with the defaults this record names.

## 6. Failure modes of the program

- **Over-generalization from thin evidence.** Two uses is the floor, every free
  parameter needs `k` distinct bindings in shadow, and the personalized-skills finding
  says the benefit of induction appears only with several prior sessions; the ladder is
  what makes a thin corpus harmless rather than wrong.
- **Stale procedures under drift.** Fingerprints, content-addressed inputs and the
  per-step postcondition catch it at the step; demotion is automatic; the app is the
  fallback, never a blind continuation.
- **The certifier is the same model that produced the trace.** It is not: certification
  is execution and comparison, the one model call in learning proposes a derivation
  that must hold on every instance, the validator of D22 is a pre-filter, and the human
  approval pins a hash.
- **The utility problem.** Retirement by disuse and the compression floor keep the
  library smaller than the traces it explains.
- **Safety erosion by learned tools.** A procedure runs under the same safety gate,
  footprints and approvals as the steps it was recorded from; it gains no verb by being
  learned.
- **A library that is mostly goal-specific.** The reuse ratio on the Work app is the
  gauge; a decomposer cutting in the wrong places shows there before it shows as cost.

## 7. Testing and proving

Every epic's tests are named in its section. Program-wide: a scenario in the proving
suite that records a session on a fixture app (a fake harness emitting normalized
events), lifts it, replays it in shadow twice with two bindings, promotes it through an
approval, replays it trusted, and measures that the trusted replay reached no model,
with the negative control that a fresh goal on the same fixture does; and a second
scenario that dislikes one step, re-runs it with a different level, and asserts that the
lifted procedure came from the liked version and that the first version is still
readable.

## 8. Delivery

| # | Epic | Repository | Depends on |
|---|---|---|---|
| A | The app door completion | memql (engine) + memql-cockpit (cockpit) | the fleet inference record; one proto change |
| B | Recording | memql (engine) + memql-cockpit (cockpit) | A; no proto change |
| C | Learning | memql | B |
| D | Certification and replay | memql | C |
| E | Intervention, feedback and reusable decomposition | memql | B for intervention and feedback; C and D for decomposition |

One PR per epic; the cockpit half of A and B is its own PR in `memql-cockpit` with the
pin moved in the same commit as its go.mod. The first epic's plan files the issues under
the `claude` label and `epic:<name>`, one epic issue and its task sub-issues each, and
records their numbers in this index. Plans are written by the session that picks the
epic up and deleted in the epic's merge.

### Issues, filed 2026-09-13

| Priority | Epic | Repository | Epic issue | Task issues |
|---|---|---|---|---|
| P07 | A The app door completion (engine half) | memql | #5391 | #5392-#5395 |
| -- | A's first follow-up: vision through the app door (D11) | memql | -- | #5523 |
| P02 | A The app door completion (cockpit half) | memql-cockpit | #436 | #437-#439 |
| P08 | B Recording (engine half) | memql | #5396 | #5397-#5401 |
| P03 | B Recording (cockpit half) | memql-cockpit | #440 | #441-#443 |
| P09 | C Learning | memql | #5402 | #5403-#5407 |
| P10 | D Certification and replay | memql | #5408 | #5409-#5413 |
| P11 | E Intervention, feedback and reusable decomposition | memql | #5414 | #5415-#5420 |

Every task is a GitHub sub-issue of its epic; every epic body names its priority, its
PR grouping, this record and its branch; every task body carries its deliverable,
acceptance and files and opens with its epic, its PR number and the record section. All
carry the `claude` label and `epic:<name>`. The priority label `priority:Pnn` orders the
epics across each repository, lower first: in `memql` the six DSL freeze epics (P01 to
P06, record `2026-09-13-dsl-v1-language-freeze-program-design.md`) precede this
program, and in `memql-cockpit` the in-flight worker-stream-hardening epic (#425, P01)
precedes its two halves here.

## 9. Out of scope

- Distilling trajectories into model weights.
- Running the apps inside the workbench; its credential question stays open in the
  local-apps record.
- Native image generation through an app (D10).
- Cross-owner procedure sharing (D17) and feedback from account members.
- Codex before its harness parity is verified on the pinned version; the recording
  format is per app from the first PR so nothing is redone.
- A runtime arrangement surface for procedures in the OS beyond the Work app's ladder
  state, the approval card, the step timeline and the reuse label.

## 10. Facts to re-verify before starting

- That `CockpitAppExecutor.Run` still has no production caller on the delegation path
  and that `app_inference.go` is the reachable initiator.
- That `AppSessionChunk.stream` and `seq` are still discarded at
  `transcriptCollector.append` and no per-chunk concept has appeared.
- That `recordConstructGoalSignature` and `recordConstructReliability` still have no
  Go caller.
- The installed Claude Code's flag set (`--model`, `--effort`, `--json-schema`,
  stream-json event types) and the pinned Codex version's model and reasoning-effort
  spellings and event item shapes.
- The 8 KiB observation argument cap and the 256 KiB transcript bound.
- Which `v1:work:*` events carry broadcast routing rules today.
- How earlier versions of a step row are read: the standard queries collapse an
  append-only concept to one row per id, so the step timeline needs the version-history
  read, whose name and tier must be confirmed.
- Whether the triage prompt's sectionability answer already carries section boundaries
  or only a yes.

## 11. Sources

Cited by title and identifier; the research lanes' full reports are in the brainstorm
session that produced this record.

- Wang, Mao, Fried, Neubig. Agent Workflow Memory. 2024. arXiv:2409.07429.
- Wang et al. Voyager: an open-ended embodied agent with large language models. 2023.
  arXiv:2305.16291.
- Zheng et al. SkillWeaver. 2025. arXiv:2504.07079.
- Agent Skill Induction. 2025. arXiv:2504.06821.
- Yuan et al. CRAFT. 2023. arXiv:2309.17428.
- Cai et al. LLMs as Tool Makers. 2023. arXiv:2305.17126.
- Wang, Fried, Neubig. TroVE. 2024. arXiv:2401.12869.
- Yang et al. Learning globally reusable skills for coding agents. 2026. arXiv:2608.06153.
- Zhang et al. Workflow-to-Skill. 2026. arXiv:2606.06893.
- Huang, Du, Lan. Do personalized skills help coding agents? 2026. arXiv:2608.10319.
- Zhong et al. SkillLearnBench. 2026. arXiv:2604.20087.
- Shao et al. Misevolution. 2025. arXiv:2509.26354.
- Sumers, Yao, Narasimhan, Griffiths. Cognitive architectures for language agents. 2023.
  arXiv:2309.02427.
- Shinn et al. Reflexion. 2023. arXiv:2303.11366.
- Zhao et al. ExpeL. 2023. arXiv:2308.10144.
- Zhang et al. ACE: agentic context engineering. 2025. arXiv:2510.04618.
- Su et al. Learn-by-Interact. 2025. arXiv:2501.10893.
- Dakan, Feller, with Anthropic. AI Fluency: Framework and Foundations. 2025. The 4D
  framework: Delegation, Description, Discernment, Diligence.
- Leno, Polyvyanyy, Dumas, La Rosa, Maggi. Robotic process mining. Business and
  Information Systems Engineering, 2021; and Discovering executable routine
  specifications from user interaction logs. 2021. arXiv:2106.13446.
- Cerna, Kutsia. Anti-unification and generalization: a survey. 2023. arXiv:2302.00277.
- Bulychev, Minea. Duplicate code detection using anti-unification. 2008.
- Leemans, Fahland, van der Aalst. Discovering block-structured process models from
  event logs. 2013. Berti, van der Aalst. Improved token-based replay. 2020.
  arXiv:2007.14237.
- Bowers et al. Top-down synthesis for library learning (Stitch). 2023. arXiv:2211.16605.
  Ellis et al. DreamCoder. 2021. Grand et al. LILO. 2024. arXiv:2310.19791.
- Pei et al. PrefixSpan. 2001. Nevill-Manning, Witten. SEQUITUR. 1997.
- Konidaris, Barto. Skill discovery in continuous reinforcement learning domains using
  skill chaining. 2009.
- Fikes, Hart, Nilsson. Learning and executing generalized robot plans (MACROPS). 1972.
- Minton. Quantitative results concerning the utility of explanation-based learning.
  1990.
- Lau, Wolfman, Domingos, Weld. Programming by demonstration using version space
  algebra. 2003.
