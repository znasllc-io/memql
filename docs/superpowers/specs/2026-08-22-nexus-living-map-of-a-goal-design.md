# Nexus -- the living map of a goal: Map, Constructs, Replay

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project I of nine); the map prototyped live in the visual companion and corrected with the owner
**Owner:** `clients/portal` (a new feature directory), a handful of DSL reads; hard prerequisite memql#4366

Sub-project I of the 2026-08-22 backlog brief. A portal section where a
goal's world materializes in 3D as the system works on it -- you, the goal,
the planner, the specialists it raises, the tasks by phase, the artifacts
produced and the MemQL constructs authored along the way -- replayable
afterwards from the engine's own history. Built on sub-project B (epic
#4308, shipped in #4368) and blocked on its carve-out #4366.

---

## 1. What the brief asks, and what the engine already gives it

The owner's words: a minimalistic but complete, production-grade 3D graph
map of the agents working on a task, materializing slowly as the goal is
worked -- the goal, the planner that takes it as input, the specialists,
the queries / mutations / automations created along the way -- click for
detail through the portal's reusable UI; enjoyable and rewarding in a
personal, professional, technical way, not as gamification.

At `fd6b140f`:

- **Everything Nexus draws already emits live events.** Row-level CDC for
  `v1:planner:plan`, `task`, `v1:agents:agent`, `v1:authoring:bundle` /
  `construct` / `dependencyEdge`, `v1:library:artifact`; plus the
  concept-registry follow the portal already consumes (`useConcepts.ts`),
  which is how "an agent authored a new concept" shows up live today.
- **The plan -> DSL loop is modelled**: `bundle.sourcePlanId`,
  `agent.lineage.originatingPlanId`, `artifact.producedByPlanId`,
  `task.planId` / `phase` / `dependsOn[]`. Runtime authoring is real
  (`dsl/authoring`; define / stage / promote; durable promotion writes
  ordinary rows), gated by `MEMQL_AUTHORING_CAPTURE_MODE=author`, off by
  default.
- **Three landmines in the feed**: `graph.node.created` fires on every
  write including updates (dedup by id); delivery order is not guaranteed
  (order by the payload's timestamps); CDC has no replay (seed from queries,
  then follow).
- **The subscription seam is authorized now (#4368)**: admission at
  fan-out mirrors the read path; a `granted`-tier row arrives as an
  **id-only notification with `payload_omitted`** and the client re-reads
  it through the authorized path -- the portal's live band already does
  this (#4310).
- **The declarations are not in (#4366)**: `plan`, `task`, `taskState` are
  undeclared, so today a subscription to `plan` delivers every user's
  plans. `plan` cannot take an owned floor (`plansForSpace` reads
  collaborators' rows by design, `cmd/memqlmigrate/rowauthz_infer.go:371`);
  its right tier is `granted` via space participation, which is the
  id-only case. Engine-internal reads over these concepts run with no
  actor and would be refused by an owned tier until the internal actor is
  characterised. **Nexus does not ship before #4366 resolves** -- a map
  that subscribes to everyone's plans is not a map of yours.
- **MemQL is append-only**: every row the map draws carries the
  timestamps that say when it existed and changed (`createdAt`,
  `startedAt`, `completedAt`, `phases[].startedAt/completedAt`,
  `activatedAt`), so a goal's history can be replayed from the rows with
  no new backend.
- **Constraints**: no 3D or graph library in the portal
  (`clients/portal/package.json`); the edge CSP allows WebGL and
  same-origin workers and blocks WASM (`wasm-unsafe-eval` absent,
  `component/edge/csp.go:58-72`, one policy for every hosted site);
  reduced motion is CSS-only today (`src/styles/index.css:121-127`) and
  cannot reach a canvas; detail views are routes + `RowDetailDialog`
  (`routes.tsx:54-78`); `src/views/` may not hand-render rows
  (`portal_view_composition_test.go`), so Nexus is a feature directory;
  no concept-id literal in the generic dirs (`portal_render_path_test.go`);
  a busy plan is low hundreds of rows, thousands across a user's history.

---

## 2. Decisions

### D1 -- One goal at a time, yours

Chosen over an operator's cluster-wide view and over both-with-a-switch.
Nexus opens on a plan and shows its world; a picker moves between your
goals. Bounded (hundreds of nodes) and the view where the effects earn
their keep. The cluster view is a different product (a monitoring surface
that needs aggregation before effects) and is not precluded: the scene is
data-driven.

### D2 -- Semantic grain, activity as motion

Nodes: you, the goal, the planner, each specialist, each semantic task by
phase, each produced artifact, each authored construct and the bundle.
Tool invocations are **not** nodes: they are pulses and a counter on the
task they belong to. A retried step re-lights its node. Chosen over
everything-is-a-node (a hairball after one busy plan) and over no activity
(a scene that is dead between arrivals).

### D3 -- The timeline constellation, with the goal at the end

Prototyped live and corrected with the owner: **you at the start, the goal
at the far end as a faint beacon**, the planner taking the request, the
phases materializing left to right toward the goal, agents above the work,
artifacts and constructs below it; the goal fills in as tasks complete
and lights when the last one lands. Deterministic positions from
`(kind, phase, index-in-phase)` so a replay looks the same twice and a node
always lands where you expect it. Chosen over orbital (hierarchy without
time) and force-directed (things move after they have arrived).

### D4 -- Three pages: Map, Constructs, Replay

Each page is one question -- what is happening, what did it build, how did
it get here. Chosen over map-only-with-panels and over deferring
Constructs; Replay is nearly free on an append-only engine.

### D5 -- Every node and every moment is a URL; the camera is not

`/nexus/:planId`, `/nexus/:planId/node/:nodeId`, `/nexus/:planId/constructs`,
`/nexus/:planId/replay?at=<rfc3339>`. A deep link lands on the thing; the
camera re-frames it. The portal's rule ("every destination is a URL, not a
piece of component state") holds; camera position is the one state it has
never had to accommodate and is deliberately not a URL.

### D6 -- The feed handles payload and id-only events identically

Every live event is resolved through the authorized read (`useRowDetail`'s
fresh read), whether it arrived with a payload or `payload_omitted`. One
code path, no trust in a payload the seam may later withhold, and the
`granted` tier `plan` will carry is already the common case.

### D7 -- Reduced motion is the scene's job

`prefers-reduced-motion` is read by the render loop: no particles, no
drift, no overshoot, fades only, frame loop on demand. The portal's CSS
rule cannot reach a canvas; the scene carries the same guarantee itself.

### D8 -- The reward is the receipt

No points, no streaks. The arrival animations, the lit goal, and a
**completion card** that materializes under the goal when it succeeds:
elapsed time, tasks, agents raised, artifacts produced, constructs
authored, tokens spent and -- once sub-project H lands -- what the
subscription covered. A professional's reward is seeing what got built and
what it cost.

---

## 3. The section

Rail group **Nexus**; feature directory `clients/portal/src/nexus/` with a
splat router (`nexus/*`) on the `src/admin/` and `src/artifacts/` pattern;
`Container` page bodies; icons through `src/ui/icons.ts`; the scene chunk
lazy-loaded so the rest of the portal pays nothing for it.

- `/nexus` -- the Map for the most recent goal, with the goal picker (your
  plans newest first, the running one on top) and a "recent goals" strip.
- `/nexus/:planId` -- the Map for one goal; `/node/:nodeId` opens the detail.
- `/nexus/:planId/constructs` -- Constructs.
- `/nexus/:planId/replay` -- Replay; `?at=` pins a moment.

Tabs under the goal's PageHeader switch between the three (the admin-console
tab strip).

---

## 4. The Map

### 4.1 Stack

three.js through react-three-fiber (v9, React 19) and drei, in one lazy
chunk; WebGL only (no WASM, D7 above; physics is not needed); labels as
CSS2D elements in the portal's type and tokens; brand colours from the
existing tokens (`--memql-accent` for agents, `--memql-fg` for you and the
goal, `--memql-warn` for a running task, `--memql-danger` for a failed one,
the data tones for constructs and artifacts). Dark and light grounds from
`--memql-bg`; the scene reads the theme the way the rest of the portal
does.

### 4.2 Layout (pure, tested without WebGL)

`layout(rows) -> positions`: x from `(phase, index-in-phase)` with the goal
at the end and you at the start; y by lane (agents above, tasks on the
road, artifacts and constructs below); z spreads siblings. Many tasks in a
phase wrap into rows; beyond a threshold (~150 semantic nodes) a phase
collapses into a cluster node that expands on click and never overlaps its
neighbours. The layout is a pure function of the rows, so a replay and a
deep link produce the same scene.

### 4.3 Materialization

Seed, then follow:

- **Seed** reads under the caller's actor: `planById`, `tasksForPlan`
  (semantic kinds only), agents for the plan (by
  `lineage.originatingPlanId` plus the plan's `ownerAgentId`), the bundle
  by `sourcePlanId` with its constructs and dependency edges, artifacts by
  `producedByPlanId`. Two of those reads do not exist yet as named queries
  (`agentsForPlan`, `artifactsForPlan`) and are added, classified for the
  per-row authz bucket.
- **Follow**: one `subscribeGraph` per concept (`plan`, `task`, `agent`,
  `bundle`, `construct`, `dependencyEdge`, `artifact`) plus the registry
  follow, all on the one connection `ClusterProvider` holds; events are
  resolved through the authorized read (D6), deduplicated by id, ordered
  by the payload's timestamps, and dropped when the read refuses.
- **Arrival**: the prototyped animation -- particles condense into the
  node, it scales in with a small overshoot, its edges draw toward nodes
  that already exist; a task pulses while `running` (tool invocations tick
  its counter), settles on `succeeded`, turns to the danger tone on
  `failed`; a retry re-lights the node; the goal fills with
  `completedTasks / tasks` and lights on `succeeded`, with the completion
  card (D8).
- **Ambient life**: slow breathing on agents, the road from you to the
  goal brightening with progress, nothing that moves a node after it has
  arrived.

### 4.4 Interaction

Hover lights the path from you to the node and its label; click navigates
to `/node/:nodeId`, which opens `RowDetailDialog` for that row (a fresh
authoritative read), with the actions the portal has today -- open in the
concept browser, open the artifact in the Library, open the construct in
Constructs -- and the runnable actions from the construct catalog once
epic #4274 lands. Keyboard: the Replay page's event list doubles as a
linear, focusable index of every node (accessibility is structural here,
not a second UI). The camera re-frames a node on deep link and otherwise
never moves on its own.

### 4.5 Performance

Frame loop on demand (idle under 5% CPU when nothing changes), instanced
meshes per kind, labels culled by distance, particles pooled, the chunk
lazy; a budget test renders a 300-node fixture and asserts the frame time
and the idle behaviour.

---

## 5. Constructs

The goal's authored bundle: status (`draft -> validated -> dryRunPassed ->
active -> paused -> retired | failed`), validation and dry-run results;
each construct with kind, name, status (`draft -> staged -> active ->
retired`), its source in a read-only code block, and the dependency edges
drawn as a small 2D graph (the same rows the map draws); the two actions
the engine already has -- stage and promote -- behind `ConfirmDialog`
through the existing authoring handlers (`component/grpc/authoring_handlers.go`,
`dsl/authoring/mutations.memql`). When authoring capture is off, an honest
banner: "this goal produced no constructs because authoring capture is
off", linking the operator doc.

---

## 6. Replay

`events(rows) -> timeline` is a pure function over the seed rows' timestamps:
plan created, each task created / started / completed / failed, each agent
raised, the bundle and each construct created and activated, each artifact
produced, the plan succeeded. The scrubber filters the same scene by time
(`scene(rows, at)`), with the arrival animations replaying when played
forward; speeds 1x / 4x / 16x; a moment is a URL (`?at=`). The event list
beside the scrubber is the map's accessible index. No new backend: rows
that exist carry their own history; a row that was deleted is not in a
plan's world by construction (the concepts here are append-only).

---

## 7. Security and prerequisites

| Concern | Handling |
|---|---|
| Subscribing to `plan` / `task` receives everyone's rows today | **#4366 is the gate**: Nexus ships after `plan` / `task` declare a tier the engine's internal actor can live with (`granted` via space participation is the ruling the issue points at). The map's feed already treats every event as id-only + authorized re-read (D6), so the tier's choice changes nothing in the client. |
| `agent`, `bundle`, `construct`, `artifact` | `construct`, `dependencyEdge`, `artifact` declare owner tiers already; `agent` and `bundle` are in the long tail and admit everyone on reads as today; the client filters by their plan pointers, and the residual is recorded in the undeclared gate, not hidden here. |
| A payload the seam withholds | never rendered; D6. |
| Authoring actions | the existing handlers' own gates (owner-scoped stage, owner-gated promote). |

---

## 8. Testing

1. Pure functions: `layout` (determinism, wrapping, the cluster threshold,
   the goal at the end), `events` (ordering, retries re-light, missing
   timestamps), `scene(rows, at)` (scrub boundaries).
2. The feed: duplicate `created` events dedupe; out-of-order `updated`
   resolve by timestamp; a `payload_omitted` event is re-read; a refused
   read drops the node; a seed-then-follow race leaves exactly one node.
3. Routes: picker, deep link to a node opens the dialog, `?at=` pins the
   scrubber, tabs.
4. Constructs: the banner when capture is off; stage and promote confirm
   and call the handlers.
5. Reduced motion: no particles, no drift, frame loop idle.
6. Budget: the 300-node fixture.
7. Repo-root guards: page frame, render path, view composition, brand
   source.
8. A visual smoke on the parity cluster against a real plan, recorded as
   a screenshot in the PR.

---

## 9. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the scene and the Map | the pure scene library, the R3F scene, the feed, the picker, the routes, the nav group | #4366 on `main` |
| 2 -- Constructs and Replay | both pages, the two new queries if PR 1 did not need them yet | PR 1 |
| 3 -- the receipt and polish | the completion card (subscription-covered spend once epic #4358 lands), the cluster node, the budget test, docs | PR 1 |

One `Closes #N` line per issue. Eight tasks.

---

## 10. Out of scope

- The cluster-wide operator view (D1; the scene is data-driven, so it is
  a later mode, not a rewrite).
- Tool invocations as nodes (D2).
- Server-side subscription predicates ("only this plan"); the per-goal
  event rate does not need them.
- Ordered / replayable live delivery (`DeliverySubstrate`); Replay reads
  rows, it does not replay the bus.
- Declaring `agent` and `bundle` (the row-authz long tail, #4366's
  successor work).
- Sound, haptics, achievements.

---

## 11. References

- Prototype: `nexus-layout-v4.html` in the brainstorm session directory
  (`.superpowers/brainstorm/`, local, git-ignored); the corrected timeline
  screenshot is attached to the epic.
- Code: `clients/portal/src/{cluster,concepts,components,admin,artifacts}/`,
  `sdk/ts/src/client/{subscriptions,query}.ts`, `dsl/planner/{concepts,queries}.memql`,
  `dsl/agents/concepts.memql`, `dsl/authoring/*.memql`, `dsl/library/queries.memql`,
  `component/edge/csp.go`, `component/memql/{concept_registry_broadcast,executor_mutation}.go`.
- Specs: `2026-08-22-subscription-row-authz-design.md` (epic #4308),
  `2026-08-22-local-apps-as-execution-surfaces-design.md` (epic #4358, the
  subscription-covered figure).
- Issues: #4366 (the declarations prerequisite), #4274 (dynamic views --
  actions from the construct catalog), #2460 (structured graph
  subscriptions), #4310 (id-only re-read in the portal).
