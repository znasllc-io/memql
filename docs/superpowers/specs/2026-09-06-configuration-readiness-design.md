# Configuration readiness -- the platform knows what is set up, and the OS says so

- **Date:** 2026-09-06
- **Status:** approved in the 2026-09-06 brainstorm. The eight forks below
  (D1-D8) were put to the owner as selectable options and each was answered;
  they are not open questions. Everything else is a recommendation with its
  rationale and what it rejected.
- **Program:** epic 1 of a five-part program agreed in the same brainstorm,
  in shipping order: 1 this record; 2 OpenAI workload identity federation,
  the migration to the official OpenAI Go SDK, and the removal of manual API
  keys everywhere; 3 fleet inference completion; 4 model runtime install and
  model pull in the cockpit; 5 the first-run core wizard. Epics 2 to 5 get
  their own records. Section 10 carries the facts established today that
  they will need.
- **Owner areas:** `component/envregistry` (the declaration),
  `component/memql/readiness` (new leaf package, the fold), `component/memql`
  (the writer, the evaluators, the builtin), `component/node` (the routing
  rule), `dsl/platform` (the concept), `clients/os` (the shell).

---

## 1. Problem

A fresh cluster boots with no owner, an owner claims it, and from then on
every MemQL OS app opens as though the modules behind it were configured.
Nothing tells the shell otherwise. Campaigns opens and refuses at the
preflight; Materializer opens and fails on its one model call; Fleet's
Apps section offers a delegation that no environment supports. A person
without the owner or developer role cannot learn that the app is merely
unconfigured rather than broken, and a person with the role has no list of
what to set up and where.

The engine already answers "is this configured" five different ways
(section 2.2), none of them readable by an ordinary signed-in user, none of
them cluster-wide, and only one of them able to say "partly".

## 2. What the tree already has

### 2.1 The shell

- An app manifest (`clients/os/src/system/registry.ts`, `OsAppManifest`)
  is static: id, name, icon, roles, sections, a required `settingsSection`
  and a required `logsSection`. Nothing in it varies at runtime. Every app
  has its own Settings section, backed by a versioned localStorage store;
  Deployables is the one app whose Settings section holds real cluster
  configuration (the GitHub connection).
- Role gating is "hidden and refused": an app the actor cannot open is
  absent from the launcher (`appsForRole`), `openApp` refuses it, gated
  sections are absent from the rail (`sectionsForRole`), and a window
  already open on a now-refused app keeps its rail and title bar and
  renders `SurfaceRefused` (`src/kit/RankStates.tsx`) in place of the body.
  That surface's doc is the tone precedent: not an error, not styled as
  one, and the copy names the fix rather than the failure.
- There is no disabled or badged navigation entry anywhere, and three code
  comments refuse the pattern (`kit/ActionBar.tsx`, `chrome/Dock.tsx`, the
  interface rules' rule 12). There is no shared empty-state component; the
  dominant pattern is `LiveList`'s `emptyText`, rendered only when the feed
  is live and empty.
- The Accounts first-run card (`src/apps/accounts/FirstRunCard.tsx`) is the
  one sanctioned first-run moment (interface rule 8). Its covenant matters
  here: no modal, no banner, no badge in other apps; the gate is a field on
  the row so the answer lives in the cluster; and while the feed is still
  seeding neither the card nor the list renders, because an unconfigured
  row and a feed that has not arrived look identical.
- Cluster's Readiness section (`src/apps/cluster/readiness/`) reads
  `inferenceStatus` and the owner's passkeys and says of itself that it is
  a signpost, not a gate: the portal's gate was client-only and
  session-latched, and enforced nothing while behaving like a wall.
- The dot language (`ProvenanceDot`, `src/kit/index.tsx`): green is
  reachable now, amber is not reachable, unknown draws no dot. Exactly
  three CSS tones exist: `reachable`, `unreachable`, `off`.
- The live-collection contract (`clients/os/README.md`): `retain()` is the
  only thing that starts a feed; a concept broadcasts only under a routing
  rule in `component/node/routing.go`; a heartbeat is not news.

### 2.2 The engine's five vocabularies

| Signal | Where | Says | Granularity | Readable by |
|---|---|---|---|---|
| `providerAuthStatus` | `component/memql/provider_auth_status_read.go` | `available`, `authSource` in {federation, globalSecret, globalVariable, env, unresolved} | provider entry, per node | owner only |
| `inferenceStatus` | `component/memql/fleet_catalog_read.go` | `eligible`, `doorsOpen` in {local, federation, apiKey} | the cluster, scoped to the caller's fleet | everyone |
| `integrationStatus` | `integrations/email/status.go`, `configmanifest.go` | `needs_configuration`, `configured`, `unhealthy`, or not reported; slots, lanes, reasons | integration | owner or developer |
| module registry | `component/memql/module_registry.go` | `built_in`, `active`, `opted_out`, `running`, `scaled_to_zero`, `credential_gated`, `not_deployed` | component, integration, pack, node type | admin and above |
| Shopify store | `integrations/shopify/store.go` | `configured`, `backfilling`, `live`, `paused`, `error` | one store | store owner |

Email is the only integration with a real self-report, and the only place
anywhere that can say "partial": its config manifest declares slots grouped
into lanes, a lane resolves wholesale from one source, and every required
slot resolving across sources is the `split_lane` reason. Everything else
registered reports "not reported". The env manifest
(`scripts/secrets/manifest.yaml`, embedded as
`component/envregistry/manifest.yaml`) tags 470 entries with an owning
`component` across 26 values, and only three entries carry `required`.
`v1:platform:missingCapability` has no writer, no caller and no reader.

### 2.3 What each app needs

Three classes, from the per-app inventory:

- **Zero configuration beyond a signed-in owner:** Bin, Concepts, Users,
  Cluster, Logs, Settings, Accounts.
- **A real partial state:** Files (bytes need storage, browsing does not),
  Fleet (Workbenches and Apps each need their own environment), Deployables
  (publish needs storage; private sources need a credential), Training
  (embeddings degrade without a provider), Campaigns (authoring works,
  sending is all-or-nothing).
- **All or nothing:** Stores, Materializer, Nexus, the Ask widget.

The substrate several apps share, and therefore the wizard's future scope:
identity, AI providers, blob storage, the email sender, the domain, the
GitHub App.

## 3. Decisions

### D1 -- Five epics, foundations first, the wizard last

Chosen over "wizard second, offering only what exists" (an honest thin
wizard rebuilt as each later epic lands) and "wizard second at full scope"
(one epic absorbing everything). The wizard is the core modules' Set up
groups walked in order on first run, so it consumes the readiness model,
the federation doors, the fleet doors and the cockpit's runtime setup. The
Set up groups from this epic give owners a guided path in the meantime.

### D2 -- Readiness is computed on the engine, per module; the OS maps apps to modules

Chosen over "engine, per OS app id" (the product-neutral engine would carry
the OS's app ids, and a product bundle's own apps could not be described)
and "OS only, folded from today's signals" (most of those reads are
owner-only, so a viewer's window could never learn it is unconfigured, and
the five vocabularies would stay). One source of truth, readable by every
signed-in person, and the wizard walks the same list.

### D3 -- Every node reports; the read folds them

Chosen over "the answering node, labelled" (cheaper, and wrong for any
module whose environment lives on another node type: the GitHub App's six
values are on identity, workbench peers on the agent, storage on the bff)
and "ask the mesh on demand" (a new cross-node request, no live feed, slow
on a large fleet). Costs a concept, a routing rule and a per-node writer.

### D4 -- The rail stays; every section shows the setup surface

Chosen over "sections absent until set up" (the literal reading of "absent,
never disabled" applied to places: the app collapses to one section, its
shape is invisible until it works, and it grows back piece by piece) and
"entries listed but greyed and inert" (the owner's original proposal: a new
idiom the shell refuses for acts, needing its own accessibility story, and
both rail renderers change). The chosen shape is the refused surface's:
nothing disabled, the rail still teaches what the app will do, one
component serves partial apps per section, and the phone shell needs no
special case.

### D5 -- Red when not set up, amber when partly, nothing when set up

Chosen over "red, amber and green in every state" (a permanent green dot on
Settings in every window is standing chrome, and green already means
"reachable now") and "a word beside Settings" (cramped in the rail, no home
on the gear or the phone strip). The mark appears only while a person is
needed, following the shell's own rule that unknown draws no dot.

### D6 -- One place per module; the app's Settings gets a Set up group that points there

Chosen over embedding the module's editor inside every app that needs it
(one module editable from several windows) and over linking out with no
group (the app's Settings would say nothing about what it needs). Shared
modules are configured once; nothing is said twice.

### D7 -- Manual API keys are removed everywhere, the local cluster included

Recorded here because it shapes the `ai` evaluator and the providers
surface; executed in epic 2, because the OpenAI provider has no credential
source until OpenAI federation exists. Chosen over "refuse keys in the
cloud, keep the env key local-only" and "remove only the OS key entry".
Consequences the owner accepted: a developer on a local cluster reaches
cloud models only through a fleet machine, and streaming transcription and
whisper lose their bearer on a local cluster until a fleet machine can
transcribe. The Anthropic federation record's D4 and D6 ("local keeps the
key") are reversed by this.

### D8 -- Lanes are declared in a `modules` block of the env manifest, not per entry

A recommendation, not a fork the owner was asked. Section 1 of the
brainstorm said "each entry gains `lane` and `requiredInLane`"; the
concrete shape is a top-level `modules` list instead, for one reason found
while writing this: a lane spans components. The GitHub App's six values
sit under `component: identity`, and local apps need one value each from
`mcp`, `cluster` and `identity`. A per-entry key cannot express a module
that owns none of its variables. The decision it implements is unchanged:
readiness is declared as data, in the one registry that already knows
every variable.

## 4. The model

### 4.1 Vocabulary

A **module** is a named set of lanes declared in the env manifest, or one
of the named evaluators (4.3). Its id may coincide with a component id
(`storage`, `email`) or name a feature (`localApps`, `githubApp`). A
module's readiness on one node is one of:

| State | Meaning |
|---|---|
| `configured` | one lane has every required slot present |
| `partial` | some required slot is present, no lane is complete |
| `unconfigured` | no required slot is present |
| `notApplicable` | this node does not host the module |

The folded, cluster-wide verdict (4.5) adds `unreported`: no live node
reported it. The client adds `unknown`: the feed has not loaded. Neither
is ever spelled `unconfigured`.

### 4.2 The declaration

```yaml
# scripts/secrets/manifest.yaml (authored); embedded copy regenerated by
# `make env-registry-sync`, drift caught by `make env-registry-check`.
modules:
  - name: storage
    core: true
    description: "Files, materialized outputs, deploy bundles and log archives live in blob storage."
    hostedBy:
      integrations: [storage]
    lanes:
      - name: azure-blob
        configurableFrom: deployment
        slots: [MEMQL_AZURE_BLOB_CONTAINER, MEMQL_AZURE_STORAGE_CONNECTION_STRING]
        # Present slots count toward "touched", never toward completeness.
        optionalSlots: []
  - name: email
    core: true
    description: "Sending mail needs a mailbox this cluster can send from."
    # An evaluator, NOT lanes -- see the amendment note below 4.2.
    evaluator: "integration:email"
  - name: githubApp
    description: "Connecting a source through the GitHub App needs the app registered on identity."
    hostedBy:
      nodeTypes: [identity]
    lanes:
      - name: app
        configurableFrom: deployment
        slots: [MEMQL_GITHUB_APP_ID, MEMQL_GITHUB_APP_SLUG, MEMQL_GITHUB_APP_CLIENT_ID,
                MEMQL_GITHUB_APP_CLIENT_SECRET, MEMQL_GITHUB_APP_PRIVATE_KEY_B64, MEMQL_GITHUB_APP_WEBHOOK_SECRET]
  - name: campaigns
    description: "Sending a campaign needs a one-click unsubscribe secret and a reachable unsubscribe address."
    lanes:
      - name: unsubscribe
        configurableFrom: deployment
        slots: [MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET, MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL]
  - name: workbench
    description: "Workbenches need a workbench node this agent can reach."
    lanes:
      - name: remote
        configurableFrom: deployment
        slots: [MEMQL_WORKBENCH_REMOTE, MEMQL_WORKER_PEERS]
  - name: localApps
    description: "Running a task in Claude Code or Codex on your machine needs the agent to mint a session credential."
    lanes:
      - name: default
        configurableFrom: deployment
        slots: [MEMQL_MCP_PUBLIC_URL, MEMQL_NODE_BOOTSTRAP_TOKEN, MEMQL_IDENTITY_VERIFIER_BASE_URL]
  - name: ai
    core: true
    description: "Inference needs a provider: a machine on your fleet serving a model, or a federated cloud vendor."
    evaluator: inferenceStatus
```

Rules:

- Every slot names an entry that exists in `secrets` or `variables`; a
  lane naming an unknown entry refuses boot the way an unknown
  relationship type does.
- `configurableFrom` is `os` (a Settings surface writes it as a global
  secret or variable) or `deployment` (set in the environment). This is
  the unshipped C4 of the integration-config record, at the lane rather
  than the entry, because the Set up group offers a button or names a
  variable per lane.
- `core` marks the modules the wizard (epic 5) walks. This epic marks
  `ai`, `storage` and `email`; epic 5 adds identity and the domain with
  their own evaluators.
- `description` is operator-facing prose in the interface's voice: what
  the module is for, one sentence, no variable names. The setup surface
  renders it verbatim.
- The manifest decoder drops unknown keys silently today (the unshipped
  C5). The `modules` block is decoded strictly, and a test asserts every
  lane the manifest names is one the evaluator read.
- `hostedBy` names the integrations or node types that report on a
  module; a node matching neither reports `notApplicable`. Naming
  neither means every node evaluates it.
- `optionalSlots` count toward presence, not completeness: a lane whose
  only present slots are optional is untouched, and a lane missing only
  optional slots is complete. They exist so a person can see the whole
  lane, not so a lane can be half-satisfied by the part that did not
  matter.

> **AMENDED while planning (2026-09-06).** `email` is an EVALUATOR, not
> lanes, and the parity test this bullet used to describe does not exist
> because the duplicate it would have guarded does not exist.
>
> The SMTP lane's variables (`SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`,
> `SMTP_PASSWORD`, and the from-address pair) are not env-registry
> entries, so a lane naming them would violate the first rule above --
> every slot names an entry. Declaring only the Graph lane would have
> been worse than a rule violation: it would report an SMTP-configured
> cluster as unconfigured, which is a wrong answer rather than a missing
> one. So the module declares `evaluator: integration:email` and the
> engine asks the email integration's own status capability in-process,
> which already knows both lanes. Email's Go `ConfigManifest` remains the
> single source of that knowledge; nothing restates it.

### 4.3 Evaluators

The generic evaluator resolves each slot's presence through the ladder
email already uses (`integrations/email/configmanifest.go`): environment
first, then the row tier the slot's own kind names -- `v1:platform:globalSecret`
for an entry declared under `secrets`, `v1:platform:globalVariable` for one
declared under `variables`. A secret slot does NOT fall through to the
variable tier: a plaintext row written under a secret's name would satisfy
a falling-through check while the decrypting reader still finds nothing, so
the report would say configured about a lane that cannot work. A slot
reports `present` and `source`, never a value. A node evaluates every
module in the manifest and reports `notApplicable` for one it does not
host, decided by `hostedBy` above.

An `integration:<name>` evaluator asks `integration.<name>.status`
in-process: `configured` or `unhealthy` is configured, `needs_configuration`
with any slot present is partial, otherwise unconfigured, and an
integration not registered on the node -- or one whose probe errors -- is
not applicable. `unhealthy` is deliberately CONFIGURED: somebody did the
setup and the send is failing for another reason, so sending them back to
a form they already filled in correctly would be the wrong repair.

`ai` is the one module whose truth is not environment presence. Its
evaluator asks the provider registry through the same reading
`inferenceStatus` makes: any open door means configured, no door means
unconfigured. The doors are those that exist when this epic ships; epic 2
removes the key door, epic 3 adds a signed-in subscription app as one
(section 10, "Epic 3 direction"). A half-configured federation cannot appear, because boot refuses
it. Later modules with non-environment truths (identity's keyset agreement,
the domain's certificate) register evaluators the same way.

### 4.4 The rows

```memql
/// One node's verdict on one module. Rewritten as a new version of the same
/// id on every boot and after a providers reload or an integration
/// configure. Carries presence and source only, never a value.
@rowAuthz(public, requiresIdentity)
concept moduleReadiness {
  module      string!
  nodeId      string!
  nodeType    string!
  state       enum("configured", "partial", "unconfigured", "notApplicable")!
  core        boolean
  /// One entry per lane: name, configurableFrom, complete, and its slots
  /// as { name, present, source }.
  lanes       []object
  reportedAt  datetime!
}
```

- Id derived from `(module, nodeId)`, so a rewrite is a new version of one
  logical row and a read collapses to the latest (the singleton-literal-id
  pattern the cluster infra rows use).
- Written under the maintenance actor by a `@serverOnly` mutation, after
  plug-ins and providers have materialized at boot, and again from the
  `providersReload` and `integrationConfigure` paths. No client-reachable
  mutation exists for the concept.
- Read tier `public, requiresIdentity`: any signed-in person, nobody
  anonymous.
- Routing: `graph.node.created` and `graph.node.updated` for the concept
  broadcast (`TargetType: ""`), beside the worker and workbench rules in
  `component/node/routing.go`, so the OS keeps a live feed on every
  replica.

### 4.5 The fold

> **AMENDED on 2026-09-14.** The fold now sets two kinds of row aside before
> the worst state wins: a row whose node could not evaluate (`unknown`, named
> in `unknown`), and a row whose cluster-scoped lanes lag the freshest row's
> (named in `stale`). See `2026-09-14-readiness-convergence-design.md`, D1 and
> S1; rows are also restated on change and by a safety-net pass (its D2), not
> only at boot and after a providers reload or configure.

> **AMENDED on 2026-09-07.** This record and the plan both placed the package
> at `component/readiness`, in the ROOT module, on the reasoning that its one
> importer "already depends on the root". IT DOES NOT: the root requires
> `component/memql`, not the reverse. Workspace mode resolves the import
> anyway, so every local run and every other CI lane was green; only the
> `module-boundaries` lane, which runs `GOWORK=off`, saw it -- and it failed
> for SEVENTEEN modules at once, since each one that replaces `component/memql`
> by relative path inherits its unsatisfiable import. The package is a sibling
> of its importer now. A nested module of its own was the alternative and is
> worse: a new `go.mod` trips a dozen gates, three of which no local run sees.

`component/memql/readiness` is a leaf package: no engine, no database, no
provider. Its one exported function takes the rows and the cluster's node
rows and returns one verdict per module:

1. Keep rows whose node's `v1:cluster:node` row is healthy and seen within
   the online window; drop the rest, so a dead replica's stale row cannot
   pin a verdict.
2. Drop `notApplicable`.
3. Worst state wins: `unconfigured` over `partial` over `configured`.
4. If the kept rows disagree, the verdict is `partial` and `disagreement`
   lists the node ids by state.
5. A module with no kept row is `unreported`.

The same function is the `moduleReadiness` builtin (request and reply, for
the wizard, the Modules section and any agent) and is mirrored in
TypeScript over the live feed, held equal by fixture parity tests
(section 7, item 1).

## 5. The OS

### 5.1 Manifest

```ts
export interface OsAppSection {
  id: string; name: string; roles?: RoleRequirement;
  requires?: readonly string[];   // module ids; unmet = this section shows the setup surface
  wants?: readonly string[];      // module ids; unmet = amber mark and a Set up row, never a gate
}
export interface OsAppManifest {
  /* as today */
  requires?: readonly string[];   // unmet = every section but Settings and Logs shows the setup surface
  wants?: readonly string[];
}
```

A contract test beside `settingsSectionProblem` fails when a manifest
names a module id the engine does not declare, checked against a fixture
generated from the manifest with a Go test that fails on drift.

| App | Requires | Section requires | Wants |
|---|---|---|---|
| Campaigns | email, campaigns | | |
| Materializer | ai, storage | | |
| Nexus | | Goals, Runs, Approvals: ai | |
| Fleet | | Workbenches: workbench; Apps: localApps | |
| Deployables | | | storage, githubApp |
| Files | | | storage |
| Training | | | ai |
| Users | | | Invites: email |
| Logs | | | storage |
| Ask widget | ai | | |

Stores, Accounts, Bin, Cluster, Concepts and Settings declare nothing. A
store row, a source, a deployable and a folder are content, not
configuration, and keep the empty states they have. `OsWidgetManifest`
gains the same `requires`, which is how the Ask widget declares `ai`; a
widget whose requirement is unmet renders the setup surface's sentence in
its own body. Deployables is listed
as `wants` rather than `requires` on purpose: composing and addressing work
without storage, and the publish refusal already names what is missing.

### 5.2 The feed

`SessionScope` (`src/chrome/Shell.tsx`) retains one live collection over
`v1:platform:moduleReadiness` and one over `v1:cluster:node`, folds them
with the mirrored function, and provides `readiness` through
`SessionProvider` beside `access` and the ladder:

```ts
type Verdict = { state: "configured" | "partial" | "unconfigured" | "unreported"; disagreement: string[] };
type Readiness = { loaded: boolean; of(module: string): Verdict | null };
```

While `loaded` is false nothing is gated and nothing is drawn, so a
configured cluster never flashes a setup screen for a frame. A degraded or
disconnected feed keeps its last verdict. Any memo that filters by
readiness names `readiness.loaded` in its deps, the rule the ladder
already enforces (`src/chrome/state.tsx`, the ladder-reactivity note).

### 5.3 The setup surface

`SurfaceUnconfigured` in `src/kit/RankStates.tsx`'s file or beside it,
rendered by `WindowFrame` and `PhoneShell` in place of the app body when
the current section's requirement is unmet, the settings and logs sections
exempt. Quiet chrome, centred like `SurfaceRefused`: the `off` dot
enlarged as the mark, never an error tone.

- Headline: `Campaigns is not set up yet` for an app, `Workbenches is not
  set up yet` for a section.
- One sentence: the unmet module's `description` from the manifest. Two
  unmet modules render two sentences.
- Owner or developer: the primary act `Set up Campaigns`, which navigates
  to the app's Settings section. Everyone else: `An owner or developer can
  set it up in Settings.`

### 5.4 The mark

Two dot tones added to the kit's vocabulary, named by meaning:
`needsSetup` (`--os-error`) and `partlySetUp` (`--os-warn`). Drawn on the
rail's Settings entry and on the title-bar gear in `WindowFrame`, and on
the Settings entry of the phone strip in `PhoneShell`; nothing when
configured, unreported or unknown. Accessible name: `Campaigns is not set
up` or `Campaigns is partly set up`.

### 5.5 The Set up group

`SetupGroup`, a kit piece every app with `requires` or `wants` renders at
the top of its Settings section, visible to owners and developers as a set
(the Integrations gate, C9 of the integration-config record). One row per
module: name, state dot and word, and the act:

- `configurableFrom: os`: `Open AI providers` or `Open Integrations`,
  which opens the Settings app at that section through the shell's intent
  mechanism. The module-to-section map lives in the OS.
- `configurableFrom: deployment`: the lane's slot names and the sentence
  `Set in the deployment`, no button.
- `partial` with disagreement: the nodes named, so a mid-rollout state
  reads as one.

### 5.6 Cluster, Modules

The Modules section (`src/apps/cluster/modules/`) gains a readiness column
from the same feed, so the operator's inventory and the apps' marks are one
reading.

## 6. Data flow and failure modes

- **Boot.** A node materializes plug-ins and providers, evaluates every
  module, writes its rows as new versions of the same ids. Until the write
  lands, the previous boot's version stands with its own `reportedAt`.
- **A change at runtime.** A providers reload or an integration configure
  re-evaluates and rewrites on that node; the broadcast reaches every
  replica; the mark and the surface flip within the arrival cue. Readiness
  reports the applied state only, so a federation id saved but not yet
  applied stays unconfigured, keeping the providers section's "a save is
  not an apply" rule honest.
- **Disagreement.** Mid-rollout one replica reports configured and another
  not yet; the verdict is partial and names both; it resolves as the
  rollout completes.
- **A dead node.** Excluded by the fold's first rule.
- **A failing feed.** Not loaded gates nothing and draws nothing. Degraded
  keeps the last verdict. The setup surface appears only from a reported
  `unconfigured`, never from an error; the builtin returns an error as an
  error.
- **Trust.** Rows are readable by any signed-in person and writable only by
  the engine, so no client can forge readiness.
- **Volume.** Modules times nodes, rewritten on boot and on a change; old
  versions collapse on read.

## 7. Testing

1. **The fold** (`component/memql/readiness`): table tests on values for worst
   state, disagreement, dead-node exclusion, `unreported`; the same JSON
   fixtures drive the TypeScript mirror, and a Go test fails when either
   side drifts, as `TestFleetOnlineWindowMatchesTheClients` does for the
   online window.
2. **The evaluator:** fake resolvers for the three sources; complete lane,
   some slots but no lane, none; a written row carries presence and source
   only (modelled on `TestModuleEnvSurfaceNeverCarriesASecretValue`); the
   consumed-lanes gate; a lane naming an unknown entry refuses boot; the
   `integration:<name>` evaluator's five cases, including a probe error
   reading as `notApplicable` rather than `unconfigured`.
3. **The writer:** database-gated on the shared throwaway Postgres: boot
   writes one row per module per node, a reload rewrites the same ids, a
   read collapses to the latest. Routing-rule presence for created and
   updated.
4. **Authorization:** the classification gate sees the declared tier; the
   write is `@serverOnly`; a test asserts no client-reachable mutation
   touches the concept.
5. **The OS contract:** every `requires` and `wants` id is a declared
   module; the loaded-flag reactivity test in the style of
   `test/system/ladderReactivity.test.tsx`, both renderers; the surface
   renders the act for owners and developers and the sentence for others;
   marks draw only for `needsSetup` and `partlySetUp`.
6. **Acceptance is screenshots**, both modes, empty and populated: the
   setup surface, a partial rail, the marks, the Set up group, in the Vite
   QA harness. The interface rules' own standard.

## 8. Scope and delivery

**In:** everything in sections 4 to 7, the mapping table in 5.1, the
Modules column in 5.6.

**Out, with a home:** the wizard (epic 5); OpenAI federation, the SDK
migration and key removal (epic 2); fleet inference completion and the
subscription-app door (epic 3); runtime install and model pull in the
cockpit (epic 4). **Out, no home yet:** a badge on the launcher or dock;
editing deployment-only slots from the OS beyond naming them; the dead
`missingCapability` concept, which is unrelated and can go in its own
cleanup.

**Delivery:** ONE PR, closing the epic issue and all seven task issues.

> **AMENDED on 2026-09-07** (owner instruction). This record and the plan
> both specified two PRs -- the engine, then the OS branched from main
> after it merged. The owner asked for a single PR covering every issue in
> the epic, so the split is retired. What the split was buying was CI on
> PR 2 against a base that already carried PR 1; one PR gets that for free,
> since both halves are on the same branch. The plan's Task 7 ("open PR 1")
> and Task 14's PR 2 step collapse into one open at the end.

The engine half: the manifest block and decoder, the evaluators, the
concept and routing rule, the writer, the fold and the builtin, the Modules
column's data. The OS half: the manifest verbs and contract test, the feed,
the three kit pieces, both renderers, the mapping table, the tests and the
screenshots. All issues carry the `claude` label and the epic label.

The email configure hook runs the `readinessRecompute` builtin through the
integration's existing writer; the builtin carries no `@sdk` and refuses any
origin that is not internal or a cluster owner. It therefore admits an
owner and refuses a developer, whose write still lands and whose mark
catches up on the next providers reload or restart (since 2026-09-14, on the
node's next safety-net pass at the latest) -- the recompute gate
exists so no signed-in person can make every node rewrite rows in a loop,
and widening it to fit one caller would trade that away for a mark that
refreshes sooner.

## 9. Out of scope, explicitly

- Making a deployment-only lane editable from the OS. The Set up group
  names the variables; moving a lane to `configurableFrom: os` is a
  per-module decision with its own write path, and the wizard epic will
  make several.
- Counting a signed-in Claude Code or Codex as an inference door. Today
  they are task-execution surfaces reached only through the planner's
  delegation policy; no router path sends an ordinary inference call to
  them. Epic 3 builds that path (section 10); this epic does not count
  them.
- A cluster-wide health verdict. `cluster.status` was deleted on purpose
  (`dsl/cluster/concepts.memql`); readiness is about configuration, not
  liveness, and says when it looked.

## 10. Facts for the later epics, established today

**OpenAI workload identity federation exists and is generally available**
for the API. The design record of 2026-08-22 and the Anthropic runbook both
say the opposite, and the refusal string in
`component/memql/provider_auth_check.go` says it to operators; OpenAI's own
changelog dates the release to 2025-05-26. The exchange is RFC 8693 token
exchange, a JSON `POST` to OpenAI's auth host at `/oauth/token` with
`grant_type`, `subject_token_type`, `subject_token`,
`identity_provider_id` and `service_account_id`, answering a `Bearer`
access token of at most one hour that never outlives the subject token,
with no refresh token. A Platform service account in a project is the
principal; Admin API endpoints are excluded; at most 50 identity providers
per organization and 50 mappings per provider. The Kubernetes guide uses a
projected token with audience `https://api.openai.com/v1` and matches the
mapping on `sub` = `system:serviceaccount:<namespace>:<name>`; the Azure
guide accepts an AKS projected token directly, the same shape as the
Anthropic path. Self-hosted clusters must upload a JWKS; AKS uses
discovery. The official Go SDK (`openai-go` v3.56.0, 2026-09-03) has
`option.WithWorkloadIdentity` with a Kubernetes token-file provider and
reads `OPENAI_IDENTITY_PROVIDER_ID` and `OPENAI_SERVICE_ACCOUNT_ID`. The
engine pins the community `sashabaranov/go-openai` v1.42.0, which has no
token-provider seam of any kind; the migration surface is 52 call sites in
`component/memql/ai_providers.go` plus `integrations/stt/openai_whisper.go`.
The exchange observer and `provider-auth check` are Anthropic-typed today.
Whether a federated token is accepted on the Realtime WebSocket is not
stated in OpenAI's docs and must be verified in the runbook.

**Removing manual API keys** touches about two dozen surfaces, most of them
deletes: the Settings key entry and `providerKeySet`, the `vendor_api_key`
rows for the AI vendors, the four env names and four legacy aliases, the
`apiKey` auth entries in `dsl/providers/providers.memql`, the install
graph's `providerKey` node and its eight `dependsOn` references, the
`--key-file` branch of `scripts/install/verify-provider-key.sh`, and the
docs. Two need replacements: the OpenAI provider's credential source, and
the bearer for streaming transcription and whisper
(`app/integrations_stt.go`, `integrations/openai/asr.go`). `providerVerify`
and the keyless-boot tests stay.

**Local models on the fleet** shipped their engine half in the 2026-08-26
record: a Fleet provider type, `fleet:<model>` names in policies, the
model-call stream, park-not-fallback, owner-scoped machine selection,
cross-replica forwarding. Four gaps: a fleet model implements no
tool-calling surface, so a turn with tools cannot land on it; embeddings
never route to the fleet (`EmbeddingProvider` reads the registry directly),
so the seeded local embedding policy has no consumer; `InvokeAIStructured`
with a fleet default provider probably falls through to a cloud structured
provider because `isNonStreamingType` does not know `Fleet`, which would be
a silent paid call on a path that must park, unverified and needing a test
first; and no call site names any of the four local-first policies.

**The cockpit discovers, gates and serves, and nothing more.** It finds
Ollama or a declared OpenAI-compatible endpoint, applies the hardware
floor and `models.allow`, advertises on the heartbeat, serves model calls,
detects Claude Code and Codex with `apps.allow` and a signed-in probe. It
never installs a runtime, never pulls a model, and has no container
orchestration; `setup` is only the permissions re-check. Runtime install
and model download are new work in that repo.

### Epic 3 direction, decided 2026-09-06

The owner asked whether the whole loop with Claude Code and Codex could
run over MCP instead of the cockpit. Researched and decided:

- **MCP cannot start the loop from the cluster's side.** Under the MCP
  specification of 2026-07-28 a server may make a request of a client
  only while that client is in the middle of calling it; sampling and
  elicitation ride inside a client call as a multi-round-trip result and
  standing bidirectional streams are gone. Claude Code additionally does
  not implement MCP sampling at all. Our MCP node speaks revision
  2025-06-18 over streamable HTTP at `/mcp`, declares tools, resources
  and prompts, sends no server-to-client request of any kind, and the
  engine is never an MCP client.
- **Each app has a harness protocol, and the cockpit will drive it.**
  Codex ships `codex mcp-server` (a `codex` tool that runs a session and
  `codex-reply` that continues it) and `codex app-server`, a JSON-RPC
  protocol over stdio, WebSocket or a socket with bearer auth that its
  own VS Code extension and desktop app use. Claude Code offers
  resumable headless sessions (`claude -p`, `--resume`,
  `--output-format stream-json`, `--json-schema`, `--mcp-config`) and the
  Agent SDK; `claude mcp serve` exposes its file and shell tools, not
  "run a task". Automated use of a Claude subscription is permitted and
  draws from the plan's limits (Anthropic cancelled the planned split on
  2026-06-15).
- **What the cockpit does today, and what changes.** A run is one prompt
  in via argv with stdin closed (`claude -p <prompt> --output-format
  stream-json --verbose`; `codex exec <prompt>`), the only controls are
  cancel and renew, the end carries no final text (transcript and
  artifacts only), a Codex run yields no usage, Codex's MCP config is
  marked unverified in the cockpit's own source, and Claude Code reads
  its MCP config at startup so a mid-run credential renewal never reaches
  the running process. Epic 3 replaces the argv runner with the harness
  protocols above, adds a follow-up control so a session can take another
  turn, adds a `submit` tool so the app hands back a structured result
  with the session end as the fallback, and keeps the worker stream as
  the request path and MCP as the app's channel back.
- **A router door.** A `SubscriptionApp` provider type with models named
  `app:claude-code` and `app:codex`, selectable by a policy or a labelled
  request, billed as `subscription`, serving chat and structured calls and
  not MemQL's own tool-calling turns, counted by inference status once it
  exists. An app the person started themselves and connected to our MCP
  server without a cockpit may pull work through a `nextTask` and
  `submit` pair as a non-default option; every door stays available
  whether or not it is the best one.
- **The default routing order, and a reversal of park-not-fallback.**
  Local fleet models first, and among them the STRONGEST eligible model
  rather than the cheapest, since local models cost nothing either way;
  then a fleet app; then federation, automatically, with the federation
  hop governed by the existing dollar ceiling and loop caps. Work parks
  only when every door is shut. This replaces D2 of the local-models
  record as the DEFAULT; an authored policy may still pin any order, and
  fine-grained routing rules a person can tune are a later epic.
- **"Strongest" needs a ranking the fleet does not advertise yet.** A
  machine reports context window, structured output, embeddings and
  concurrency, nothing about size. The cockpit will advertise parameter
  count and quantization as Ollama reports them; the engine ranks by
  parameter count then context window by default; a routing policy may
  override the order.

## 11. References

- Shell: `clients/os/src/system/registry.ts`, `src/system/roles.ts`,
  `src/chrome/WindowFrame.tsx`, `src/chrome/PhoneShell.tsx`,
  `src/chrome/Shell.tsx`, `src/chrome/access.tsx`, `src/kit/RankStates.tsx`,
  `src/kit/index.tsx`, `src/live/useLiveCollection.ts`,
  `src/apps/accounts/FirstRunCard.tsx`,
  `src/apps/cluster/readiness/ReadinessSection.tsx`,
  `src/apps/settings/providerFacts.ts`,
  `src/apps/settings/integrationsReport.ts`, `clients/os/DESIGN.md`,
  `clients/os/README.md`.
- Engine: `component/envregistry/manifest.go`,
  `scripts/secrets/manifest.yaml`, `integrations/email/configmanifest.go`,
  `integrations/email/status.go`, `component/memql/module_registry.go`,
  `component/memql/fleet_catalog_read.go`,
  `component/memql/provider_auth_status_read.go`,
  `component/memql/provider_config_write.go`, `component/node/routing.go`,
  `component/worker` (the online window), `dsl/platform/concepts.memql`,
  `dsl/skills/concepts.memql` (the `public, requiresIdentity` precedent).
- Records: `2026-08-22-anthropic-workload-identity-federation-design.md`,
  `2026-08-23-zero-key-install-design.md`,
  `2026-08-26-local-models-on-the-fleet-design.md`,
  `2026-09-01-integration-config-design.md`,
  `2026-09-06-portal-removal-design.md`.
