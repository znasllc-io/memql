---
title: Routes -- Auto, Local, Apps and your own, chosen per message in Ask
audience: internal
status: draft
area: operate
sinceVersion: 0.30.0
owner: znas
---

# Routes: Auto, Local, Apps and your own

Owner brief, 2026-10-18. A **route** decides which model does each job. The
person picks the route for each message in Ask. Four kinds ship: **Auto**,
**Local**, **Apps**, and routes people create and name themselves. This record
covers the engine, the Ask message field and Fleet's Routing page. The profile
work is in `2026-10-18-profile-design.md`.

## What the owner asked for

- **Auto** is the default. For every call it picks the best balance of speed,
  reliability and cost: embeddings go to a local model because that is free,
  and a strong job goes where it is fastest and most reliable for its price.
  - Without a budget it uses MemQL's recommended balance.
  - With a monthly budget, it stays within it.
  - Its balance is tuned with sliders on an advanced page, never in Ask, and
    "Reset to recommended" returns MemQL's defaults.
- **Local** uses local models for everything, on your machines or on fleet
  machines shared with you.
- **Apps** uses Claude Code or Codex for everything except embeddings.
  - Embeddings come from whatever source has an embedding model: a machine of
    yours, a shared fleet machine, or a vendor.
  - Without one, Apps is not usable, and says so.
- **Your own** routes have a name you choose ("Emergency", "Out of credits"),
  are saved, and are picked in Ask like the built-in ones.
  - You choose the model for each job.
  - Local and Apps come pre-set, but their jobs can be changed. Only Auto
    decides on its own.
- **Setup is not complete without an embedding model.** Apps cannot work alone,
  so the first-run choice keeps three options (Local, Anthropic, OpenAI). The
  readiness verdict for inference requires an embedding source as well as a
  source that answers.

## Vocabulary

The screens use these words. The engine keeps its own names in ids.

| Word | Means | Engine |
|---|---|---|
| Route | A named way of serving every job | `route:<id>`, new |
| Job | What a call is for | a level |
| Source | Something that can answer | a chain entry (`fleet:`, `app:`, `federation:`) |

There are four jobs, the four levels, named for what people recognise:

| Job (screen) | Level | What declares it |
|---|---|---|
| Quick tasks | `fast` | Triage, classification, summaries, suggestions |
| Main work | `strong` | Agent replies, conductor turns, authoring |
| Deep reasoning | `reasoning` | Emitting or repairing constructs, re-planning |
| Search and memory | `embeddings` | Every embedding call |

Vision, speech, transcription and image generation are unchanged and follow
Auto in every route. A route shows them as one quiet line, "Images and voice
follow Auto", and nothing more.

## Search and memory is the cluster's, not the route's

Every route serves **Search and memory** from the cluster's bound embedder
(`embedder:active`, `platform.activeEmbedderBinding`). A route cannot pick a
different one.

The index holds vectors from exactly one model. A different embedder answers in
a different vector space, so its vectors do not belong in that index, and every
later similarity read would come back plausible and wrong.

The job is shown in every route as a read-only row naming the bound model. It
offers "Change embedding model", which goes to the existing activation flow
(`embedder_activation.go`), because re-binding plans a re-index.

**First binding is automatic.** When no embedder is bound and an embedding
model becomes reachable (pulled onto a machine, shared with the person, or a
vendor embedding model configured), the engine binds it. There are no vectors
yet, so binding plans no re-index. Today's Local install always pulls
`qwen3-embedding:0.6b`, so a Local setup ends with an embedder bound.

## Built-in routes

| Route | Quick tasks | Main work | Deep reasoning | Search and memory |
|---|---|---|---|---|
| Auto | ranked per call | ranked per call | ranked per call | bound embedder |
| Local | `fleet:fastest` | `fleet:strongest` | `fleet:strongest` | bound embedder |
| Apps | `app:*` | `app:*` | `app:*` | bound embedder |

- `fleet:` includes models shared with the person through machine Sharing,
  exactly as the router already expands it for that person.
- **Apps maps each job to the app's own model and effort**, through the
  machine's `apps.levels`: Haiku, Sonnet or Opus for Claude Code, and low,
  medium or high effort for Codex. A tool-needing step becomes an app session,
  as today.
- **Local and Apps can be edited cluster-wide** by an owner or developer:
  - each job holds one to four sources, in order;
  - **Restore default** removes the override;
  - the overrides live in the routing document (`PolicyDocument`), under a new
    `routes` map, sharing its revision and CAS so a stale write is refused the
    way routes and rules are today;
  - Auto has no job lists to edit.
- **No source outside the route.** A route is walked job by job, trying its
  sources in order. When the job's sources are exhausted the call parks, or for
  an interactive turn refuses, naming the route and why each source was shut:
  "Apps: Claude Code is signed out on studio-mac; Codex is not allowed on this
  cluster". Nothing falls through to sources the route does not name. The
  existing degrade rule (`reasoning` to `strong` to `fast`) still applies
  inside the route.

## Your own routes

A personal route is a row of a new concept `v1:router:route`:

```
concept route {
  ownerUserId  string!   // stamped from the caller
  name         string!   // 1..40 chars, unique per owner, case-insensitive
  fast         []string  // 1..4 chain entries
  strong       []string  // 1..4 chain entries
  reasoning    []string  // 1..4 chain entries
  createdAt    datetime
  updatedAt    datetime
}
@rowAuthz(owner="ownerUserId")
```

- **Validation.** Every entry must parse as a source the router accepts: a
  `fleet:`, `app:` or `federation:` entry, never `policy:` and never
  `embedder:`. An app id is held to the runnable set.
- **New routes start from a copy** of Local, Apps or the blank Auto rows. A new
  route copies the source rows, so later edits to Local or Apps never change it.
- **Private by design.** A route is the owner's alone. Sharing routes is out of
  scope.

## Choosing a route

- **The source field.** Ask sends the choice in the existing field, which needs
  no new wire field: `AiChatMsg.provider = "route:<id>"`, where `<id>` is
  `local`, `apps` or a personal route's bare id. Empty, or `route:auto`, is
  Auto.
- **Parsing.** `ParseRouteChoice` accepts `route:<id>`.
- **Checking.** `CheckRouteChoice` refuses an id this person cannot see with
  `route_policy_unknown`, and refuses a route that is not ready with a new code,
  `route_not_ready`. The reason travels in metadata.
- **Resolution.** `router.ResolveFor` (through `memql.ApplyRunRouting`) replaces
  the matched rule's chain with the route's chain for the call's level. It
  keeps embeddings on `embedder:active` and leaves vision, audio and image calls
  to the rules.
- **Scope.** The choice binds every model call of the turn's goal on every node,
  as `RouteChoice.Source` does today, including compile-time calls.
- **Picking Apps implies Computer.** In Ask, choosing Apps sets **Where** to
  Computer, visibly. Apps run on the person's own machine, so their sessions
  need the Computer destination. The person can change Where afterwards. No
  gate is relaxed.
- **Background work keeps Auto.** System work and calls with no person behind
  them (sweeps, scheduled automations) always use Auto. A route is a person's
  choice for their own messages.

## Auto

Auto keeps today's rule evaluation. That keeps the locked prompt-specific rules
(`compilerLocalOnly`, `documentAppResearch`, `embeddingsBound`) intact. What
changes is the order inside a chain. When the matched rule names one of the
general shipped policies (`localFirst`, `fastLocalFirst`,
`federationStrongest`), Auto expands the chain to concrete candidates and
**ranks them** before walking. Every other policy is walked as written.

**Signals per candidate, all measured or declared, never invented:**

| Signal | Source | Missing value |
|---|---|---|
| Speed | median latency of that source over the last 50 decisions for this level (`v1:router:call`) | neutral (0.5) |
| Reliability | success ratio of that source over the same window, plus whether it is online and ready now | neutral (0.5); a source not ready now is dropped, as today |
| Cost | estimated USD for a typical call at this level. Local and subscription apps are 0; vendors are priced from the provider record | a vendor with no price sorts last, as `federation:cheapest` already does |
| Quality | a fixed, level-aware rank: at quick tasks every source is nearly equal (a strong vendor model slightly ahead); at main work a strong vendor model leads, then an app and the strongest local model together; at reasoning a strong vendor model, then an app, then local models | from the rank alone |

**The score.** Each signal is normalised to 0..1 across the candidates, and the
score is `wSpeed*speed + wCost*(1-cost) + wReliability*reliability +
wQuality*quality`.

**Weights.** Cluster-wide **Auto balance** settings with three sliders, each
0..100: Speed, Cost, Quality. Reliability is always weighted at the mean of the
three, so nothing can trade reliability away to zero.

- The recommended balance is 50, 50, 50: equal weights. It is stored as the
  shipped default, not as a value, so it can be re-tuned in a release from
  measured history.
- **Reset to recommended** clears the override.
- Owner or developer only.
- Stored in the routing document under `auto`, sharing its revision.

**Budget.** A person's monthly AI budget is a `v1:router:budget` row with a new
scope value `user` and `scopeId` = the user id, `periodType` "monthly". The
concept already declares the limit; nothing enforced it until now.

- **Spend.** Spend this month is the sum of that person's `v1:router:call` cost
  where billing is metered. Subscription and local spend never count, as with
  the run ceilings.
- **Under 80% of the limit:** weights as set.
- **From 80%:** the cost weight is raised to the maximum, and vendors sort after
  every free source.
- **At or over the limit:** vendors are excluded for that person's calls, as
  `NoPaidProviders` excludes them. A call with only vendor sources then parks,
  saying the monthly budget is reached and when it resets.
- **No budget:** recommended balance only.

**Ties keep the chain's own order**, which is local first: a cluster nobody has
measured yet routes exactly as it did before Auto.

**The cost ceiling still holds.** The guard asks the ceiling before a vendor
hop only when a local door came first, because a chain that starts at a vendor
is a decision an author made. A chain Auto reordered is not, so every vendor
hop in it asks the ceiling.

**Explainable.** The decision record keeps one `(auto)` entry in `considered`
with every candidate's score and its four signals, the balance, and the budget
state, so History can show why Auto chose what it did.

## Fleet capacity: the layer under every route

Owner, 2026-10-18: "part of the whole routing is also the measurement of
resources as a whole based on the machines that are part of the fleet ... it's
like an underneath layer ... before you even do that, you need to make sure
that the resources are available to send a request to one of these fleet
machines". It applies to every route: Auto, Local, Apps (for its embeddings)
and personal routes.

**Admission, before any fleet machine is chosen for a call.** A machine takes
a call only when all of these hold:

- it is online;
- the model has a free concurrency slot on it (`FleetMachine.Busy()`, as
  today);
- it can serve the model without overcommitting memory: the model is already
  loaded, or free memory is at least the model's `memoryNeedBytes` plus 10%
  headroom (at least 1 GiB);
- it is not degraded: over the last 20 decisions it served for this model, the
  error ratio is under 50% and the median latency is under three times the
  model's own median across the fleet.

A machine that fails is passed over with a precise reason in `considered`, for
example "not enough free memory to load qwen3.8:27b (needs 20 GB, 9 GB free)",
and the next machine or source in the route is tried. A call is never sent to
a machine that cannot take it.

**Signals.** The cockpit reports live memory (total and free, unified or
VRAM) and the models its runtime has loaded (Ollama `/api/ps`) on its
heartbeat. When a machine reports none of these, admission falls back to the
concurrency check alone and says so in `considered` ("memory not reported;
admitted on concurrency only").

**Measurement.** `fleetCapacity()` (`@sdk`) returns, per machine the caller can
route to:

- memory total and free, and whether it is unified or VRAM;
- the models loaded now;
- in-flight calls against slots, per model;
- recent tokens per second and median latency, per model, from
  `v1:router:call`.

Auto's ranking reads the same measurement for speed and availability, and the
routing page's "What Auto picks now" shows the reason.

## Readiness

- **An embeddings lane.** `readiness.InferenceLanes` gains a fourth lane,
  `embeddings`: an embedder is bound, or an embedding-capable model is
  reachable and will be bound.
- **The `ai` verdict.** `ai` is configured only when an answering lane (local,
  app or federation) AND the embeddings lane are configured.
- **The gate's message when the embedder is missing.** The core gate's
  Inference stop says "Add an embedding model" and offers the same Local act,
  which pulls the recommended set, the embedder included.
- **Route readiness for the person**, through the `routesForActor` builtin
  below. A route is ready when every job's chain has a source that can serve
  this person now, and an embedder is bound. Otherwise one short reason, for
  example:
  - "Needs an embedding model"
  - "No signed-in app on your machines"
  - "No local model reachable"

## Ask and the Apps route

Ask streams. Today an app is never counted as a streaming source
(`streamingChatEligible`), so Ask says "unavailable" on a cluster whose only
source is an app.

- **Streaming through apps.** The app provider implements chat streaming. It
  relays partial text when the session reports it (Claude Code's
  `stream-json`, Codex's turn events) and otherwise delivers the answer as one
  chunk when it completes.
- **Counting apps for Ask.** `streamingChatEligible` counts an app door.
- **Pinned test.** The test that pins "an app never streams"
  (`inference_streaming_readiness_test.go`) is updated to the new contract.

## Decision records and History

- `v1:router:call` gains `route`: the route id a person chose, or empty for
  Auto.
- History shows it as "Route: Apps", and it can be refined by route.

## Builtins (the contract the OS builds on)

All are `@sdk` builtins. Shapes are JSON.

```
routesForActor() -> { routes: RouteSummary[], revision: number }

RouteSummary {
  id: string                  // "auto" | "local" | "apps" | personal route id
  name: string                // "Auto" | "Local" | "Apps" | the person's name
  kind: "auto" | "local" | "apps" | "custom"
  scope: "cluster" | "personal"
  description: string         // one line, e.g. "Local models for everything"
  ready: boolean
  reason: string              // "" when ready
  customized: boolean         // a built-in with a cluster override
  jobs: RouteJob[]            // empty for auto
}
RouteJob {
  job: "fast" | "strong" | "reasoning" | "embeddings"
  sources: string[]           // chain entries; embeddings: ["embedder:active"]
  editable: boolean           // false for embeddings and for Auto
  servesNow: string           // label of the first ready source, "" when none
  ready: boolean
}

routeSave(id?: string, name?: string, fast: string[], strong: string[],
          reasoning: string[], revision?: number) -> RouteSummary
    // no id: create a personal route (name required)
    // id = local|apps: cluster override (owner or developer; revision required)
    // personal id: update the caller's own route
routeDelete(id: string) -> {}            // personal routes only
routeReset(id: "local" | "apps", revision: number) -> RouteSummary

autoBalance() -> { speed, cost, quality, recommended: { speed, cost, quality },
                   customized: boolean, revision }
autoBalanceSave(speed, cost, quality, revision) -> same shape  // owner/developer
autoBalanceReset(revision) -> same shape                       // owner/developer

myBudget() -> { monthlyLimitUSD: number | null, spentThisMonthUSD: number,
                resetsAt: string }
myBudgetSave(monthlyLimitUSD: number | null) -> same shape     // null removes
```

Errors follow the router's refusal codes, with `code` and `field` metadata.
Every write that names a revision is refused when the document changed since
that revision ("Routing changed since this page read it").

## Ask's message field

One control, drawn as one bordered surface:

- **Text area.** It starts two text rows tall, above a control row inside the
  same frame.
  - It grows upward as the text needs more rows, up to 40% of the Ask surface's
    height, then scrolls inside.
  - The control row never moves: it stays at the bottom edge of the frame.
  - Enter sends and Shift+Enter starts a new line, as today.
- **The control row**, left to right:
  - **Attach** (paperclip icon button). It opens the file picker. Each file
    uploads to the person's Library (the existing chunked upload) and shows as
    a removable chip inside the frame, above the text. The turn carries the
    artifact ids, so the agent reads them through its Library tools. Over-quota
    and failed uploads are refused on the chip in a few words.
  - **Where**, a compact menu: Auto, Workbench, Computer. This is the existing
    `ExecutionDestination`.
  - **Route**, a compact menu:
    - Auto, Local, Apps, then a "Yours" group with the person's routes;
    - a route that is not ready is shown dim, with its reason, and cannot be
      chosen;
    - "Manage routes" opens Fleet, Routing, Routes;
    - choosing Apps sets Where to Computer, as above.
  - **Send**, at the right end. It is absent while there is nothing to send and
    becomes Stop while a turn runs, as today.
- **Persistence.** The route and Where choices are kept per conversation and
  remembered as the person's last choice for new conversations (the per-viewer
  preference store Ask already uses for its own state).
- **Look.** The frame follows the kit: straight selection edges, theme tokens,
  the control line height (`--os-control-h`), no decorative chrome. The two
  menus read as quiet text-plus-chevron controls, not boxed selects, so the
  whole thing reads as one element.

## Fleet, Routing, Routes

The Routes tab is rebuilt around routes. Rules, History and Machines stay.

- **The list.** Auto, Local and Apps, then a "Yours" section with the person's
  own routes and the Add control (`AddButton` on the section heading). Each row
  shows the name, one line ("Local models for everything", "Claude Code or
  Codex, with your embedding model") and readiness.
- **A route's page.** It shows the four jobs as targets beside a tray of
  sources: your models, models shared with you, apps and vendors.
  - Each source is marked ready or not, with the reason.
  - Assigning works by click or drag, and the keyboard equivalents, exactly as
    the machine Model setup assignment board does (DESIGN.md, "Machine Model
    setup uses an assignment board"). Reuse that board's pieces where they fit.
  - A job holds its ordered sources: first is tried first.
  - Search and memory is the read-only row described above.
  - Acts follow the state: Save; Discard changes; Restore default (Local and
    Apps when customized, asking first); Delete (personal routes, asking first).
- **Adding a route** runs the shared Wizard:
  - Name ("Name and purpose");
  - Start from ("Copy Local, Apps or start empty");
  - Jobs ("Choose a model for each job");
  - then Save.
- **Auto's page.** It shows Balance and Budget.
  - **Balance:** three sliders, Speed, Cost and Quality, with the recommended
    marks shown, and Reset to recommended. Owner or developer; others see the
    values without controls.
  - **Budget:** this person's monthly budget and this month's spend. The budget
    is edited in the Profile, which is linked here.
  - Below them, "What Auto picks now", one line per job, read from the router's
    current ranking.
- **The old chain composer.** The policy-chain composer that the Routes tab used
  to hold remains reachable under Rules as "Chains" for owners and developers.
  It is unchanged, because rules still name policies.

## Who can do what

| Act | Who |
|---|---|
| Pick a route in Ask | anyone who can use Ask |
| Create, edit or delete own routes | anyone who can use Ask |
| Edit Local or Apps (cluster) | owner or developer |
| Edit Auto balance | owner or developer |
| Set own monthly budget | the person (Profile) |

## Testing

- **Engine units:**
  - `ParseRouteChoice` and `CheckRouteChoice` for `route:`;
  - per-level resolution under each built-in route and a personal route;
  - no fall-through outside a route;
  - embeddings always on the bound embedder;
  - Auto ranking with each weight extreme, missing signals, and budget states
    (below 80%, at 80%, over);
  - automatic first binding;
  - readiness requiring the embeddings lane;
  - apps streaming.
- **Cross-node.** A route chosen on the bff must bind calls the planner and
  agent make (`test/clustere2e` or the in-process hop pattern): the choice
  rides the run row, as `RouteChoice` does today.
- **OS:**
  - vitest for the message field: growth, a fixed control row, attach chips,
    picker readiness, and Apps setting Where;
  - vitest for the Routes list, route page assignment, the add wizard and the
    Auto page;
  - QA harness views and screenshots in light and dark, at 1400x900 and
    820x760.

## Out of scope

- Sharing personal routes.
- Per-route embedders, which would need a second index.
- Routing vision, voice and image generation by route.
- Auto learning its weights by itself.
