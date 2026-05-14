# Multi-Repo BFF Architecture — Migration Plan

> **Status:** Brainstorm done (2026-05-04). Architectural decision locked: separate Go modules per client + `go.work` for development. Migration not yet started.
>
> This document is the handoff for whoever (developer or AI agent) executes the migration. It captures the architectural decision, the repo layout, the phase-by-phase migration steps, and the conventions that survive afterward.
>
> **Once Phase 8 lands, this document gets deleted** (per the project's no-stale-docs convention). Until then it's the canonical reference.

---

## Table of contents

0. [Pre-flight: how this user works and how to collaborate](#0-pre-flight-how-this-user-works-and-how-to-collaborate)
1. [Goal and target architecture](#1-goal-and-target-architecture)
2. [Locked design decisions](#2-locked-design-decisions)
3. [Current state and what's wrong with it](#3-current-state-and-whats-wrong-with-it)
4. [Concept audit — initial classification](#4-concept-audit--initial-classification)
5. [Phase 1: Lock the concept audit](#5-phase-1-lock-the-concept-audit)
6. [Phase 2: Define memql's public API surface](#6-phase-2-define-memqls-public-api-surface)
7. [Phase 3: Extract memql-cockpit (TUI client)](#7-phase-3-extract-memql-cockpit-tui-client)
8. [Phase 4: Extract copresent-bff](#8-phase-4-extract-copresent-bff)
9. [Phase 5: Establish `go.work` for parallel development](#9-phase-5-establish-gowork-for-parallel-development)
10. [Phase 6: Scaffold memql-cockpit-bff](#10-phase-6-scaffold-memql-cockpit-bff)
11. [Phase 7: Cleanup memql repo](#11-phase-7-cleanup-memql-repo)
12. [Phase 8: Documentation rewrite + cutover](#12-phase-8-documentation-rewrite--cutover)
13. [Cross-cutting conventions and gotchas](#13-cross-cutting-conventions-and-gotchas)
14. [Quick-start: bringing up the multi-repo dev environment](#14-quick-start-bringing-up-the-multi-repo-dev-environment)

---

## 0. Pre-flight: how this user works and how to collaborate

> The conventions in this section are the same ones documented across the active planning + handoff documents in the repo. Skim if you've seen them; read if this is your first hand-off.

### 0.1 The user

- **Name:** Jose Sanz. **Email:** `jsanz@visionarys.io`.
- **Company:** Visionarys.
- **Role:** product owner / lead engineer for the whole stack. Reviews everything before push.
- **Communication:** voice-to-text dictation often produces transcription artifacts; read for intent.
- **Preferences:** **no emojis** in any output. Professional, concise language.

### 0.2 The repos (current and target)

**Today:**

```
~/projects/memql       Go monorepo: core + copresent-specific concepts + cmd/memql-cockpit
~/projects/copresent   React/Vite frontend (separate already)
```

**After this migration:**

```
~/projects/memql                Pure core Go library + non-BFF node-type binaries
~/projects/copresent            React/Vite frontend (unchanged)
~/projects/copresent-bff        Backend for copresent (NEW — extracted from memql)
~/projects/memql-cockpit        Go TUI client (NEW — extracted from memql)
~/projects/memql-cockpit-bff    Backend for cockpit (NEW)
~/projects/portal               React/Vite frontend (FUTURE)
~/projects/portal-bff           Backend for portal (FUTURE)
~/projects/go.work              Development workspace tying the Go modules together
```

All Go repos use `main` as their single long-lived branch. **No feature branches** unless explicitly asked. Stage files with `git add <path>` per file. Never `git add -A`.

### 0.3 The four-phase rule

Every non-trivial change follows: **familiarize → brainstorm → plan → execute**. Brainstorming is one-question-at-a-time format. The brainstorm that produced this document was the brainstorm phase. This is the plan output. Phases 1–8 below are execute.

### 0.4 Triage every request

Name explicitly which repo(s) the change touches before coding. After this migration there will be more repos — naming "this is a memql core change" vs "this is a copresent-bff change" matters more than ever.

### 0.5 Pre-production deletion

Both repos are pre-production. When code is superseded, **delete it**. No `@deprecated`, no fallback shims. Stale docs are worse than missing docs.

### 0.6 Memory capture

Save user-stated rules to the persistent memory directory immediately. The architectural decision in this document is captured at `<memory>/project_bff_architecture.md`.

### 0.7 Execution mode

When the user says "execute the plan / get this going / no questions, no pausing":

- Run uninterrupted, parallel agents aggressively.
- At the end, **commit changes**. Use explicit `git add <path>` per file.
- **DO NOT push.** User validates locally before authorizing push.
- If you genuinely can't complete in one session, stop at a clean phase boundary, commit transparently, report.

### 0.8 Makefile is canonical

Every build/run/test command is a Makefile target in the relevant repo. Multi-step logic extracts to `scripts/<area>/<name>.sh` with the standard shell-script structure. After the migration, **each repo has its own Makefile**. There is no shared Makefile across repos.

### 0.9 Memory pointers

| File | What it covers |
|---|---|
| `<memory>/MEMORY.md` | Index — read first. |
| `<memory>/project_bff_architecture.md` | Long-form rationale for THIS migration. |
| `<memory>/project_repos.md` | Repo layout, main-branch rule, `git add` conventions. |
| `<memory>/feedback_pre_prod_deletion.md` | Delete dead code. |
| `<memory>/feedback_documentation_hygiene.md` | Prune stale docs. |
| `<memory>/feedback_makefile_convention.md` | Makefile is canonical. |
| `<memory>/feedback_execute_endtoend.md` | Execution mode. |
| `<memory>/feedback_brainstorm_first.md` | Workflow. |

---

## 1. Goal and target architecture

### 1.1 The three-tier model

```
┌───────────────────────────────────────────────────────────────┐
│ Frontends (one repo each, browsers + TUIs)                    │
│   copresent        React/Vite, browser                        │
│   memql-cockpit    Go TUI, terminal                           │
│   portal           React/Vite, browser (future)               │
└───────────────────────────────────────────────────────────────┘
                          │
                          │ gRPC / HTTP
                          ▼
┌───────────────────────────────────────────────────────────────┐
│ Per-client BFFs (one repo each, all Go memql binaries)        │
│   copresent-bff       imports memql, registers copresent      │
│                       concepts/mutations/automations/tools    │
│   memql-cockpit-bff   imports memql, registers cockpit-      │
│                       specific concepts                       │
│   portal-bff          imports memql, registers portal         │
│                       concepts (future)                       │
└───────────────────────────────────────────────────────────────┘
                          │
                          │ Go module dependency
                          ▼
┌───────────────────────────────────────────────────────────────┐
│ Core memql (one repo)                                         │
│   github.com/visionarys-io/memql                              │
│   - Engine, gRPC server, plug-in registry                     │
│   - Identity service, agent / voice / cognition / planner     │
│     node-type binaries                                        │
│   - Generic concepts: identity, cluster, platform, common,    │
│     data, router, memql                                       │
│   - NO client-specific concepts                               │
│   - Published as a versioned Go module                        │
└───────────────────────────────────────────────────────────────┘
```

### 1.2 What changes vs today

- **memql becomes a clean core library.** Today it contains both engine/infrastructure code AND copresent-specific concepts. After migration, memql contains only generic platform code.
- **The `bff` node-type goes away.** Currently a `make bff` target produces a generic BFF binary. After migration, each client's BFF is its own binary built from its own repo (`copresent-bff`, `memql-cockpit-bff`, etc.).
- **memql-cockpit moves out of memql.** Currently `cmd/memql-cockpit/` lives inside memql. After migration, it's its own repo.
- **Client-specific concepts move to client-specific BFF repos.** Concepts like `v1:copresent:plan`, `v1:copresent:agent`, etc. relocate to `copresent-bff`. Same for mutations, queries, automations, tools, prompts, providers, shapes, and integrations.
- **`go.work` ties the modules together for development.** A single workspace file at `~/projects/go.work` lets you edit core memql and a BFF in tandem without bumping versions.

### 1.3 What stays the same

- **The plug-in registry pattern.** `memql.RegisterPlugin(name, factory)` already exists in core for registering client-specific Go integrations. Each BFF uses this from its `init()` to bring its concepts and Go integrations into the binary at build time.
- **DSL files (.memql).** Concepts, mutations, queries, automations, etc. continue to live as `.memql` files. They just live in different repos after migration.
- **Build tags.** Core memql still uses build tags to compile node-type binaries (voice, cognition, agent, planner, identity). The `bff` tag retires; per-client BFFs use their own approach.
- **gRPC wire protocol.** Defined once in core memql. All BFFs and frontends consume it.
- **Identity service.** Lives in core memql (already done in Phase 1 of identity work).
- **Cluster topology, partition model, audit infrastructure.** All in core.

---

## 2. Locked design decisions

These are settled in the brainstorm. Don't relitigate without explicit user approval.

### 2.1 Architectural model

**Separate Go modules per client + `go.work` for development.** Rejected alternatives:

- **Branches** (`bff/copresent`, `bff/cockpit`) — tried, retired 2026-04-20, would scale worse with more clients.
- **Submodules** — operational fiddliness, historical resistance.
- **Single-repo with directory separation** — doesn't enforce module boundaries; "is this core or client?" remains unclear.

### 2.2 Repo names and module paths

| Repo | Go module path | Type |
|---|---|---|
| `memql` | `github.com/visionarys-io/memql` | Core library + non-BFF node binaries |
| `copresent` | n/a (TS) | Frontend |
| `copresent-bff` | `github.com/visionarys-io/copresent-bff` | BFF for copresent |
| `memql-cockpit` | `github.com/visionarys-io/memql-cockpit` | TUI client |
| `memql-cockpit-bff` | `github.com/visionarys-io/memql-cockpit-bff` | BFF for cockpit |
| `portal` | n/a (TS) | Frontend (future) |
| `portal-bff` | `github.com/visionarys-io/portal-bff` | BFF for portal (future) |

### 2.3 Plug-in registration as the integration mechanism

Each BFF's `cmd/main.go` imports `github.com/visionarys-io/memql` as the application framework, plus its own client-specific packages that register at `init()` via `memql.RegisterPlugin`. The BFF binary is a thin wrapper that wires everything together.

Memql core exposes:

- The plug-in registry API (`RegisterPlugin`, `PluginContext`).
- A function to register additional DSL directories (`RegisterDSLFS(fs.FS)` or similar) so BFFs can embed their own `.memql` files.
- A function to register additional integrations explicitly when they don't fit `PluginContext` (parallels the existing `app/integrations_*.go` mechanism, but the BFF owns its own `app/integrations_<client>.go` file in its own repo).

### 2.4 `go.work` for development

A single `~/projects/go.work` file:

```go
go 1.26.1

use (
    ./memql
    ./copresent-bff
    ./memql-cockpit
    ./memql-cockpit-bff
    ./portal-bff   // when it exists
)
```

Local edits to memql core are immediately visible to BFFs without `go get`. CI and production use pinned go.mod versions; the workspace file is `.gitignore`d in each individual repo.

### 2.5 No `bff` node-type tag

The retired `bff` build-tag goes away entirely. There is no generic BFF binary anymore. Each BFF is its own binary built from its own repo with its own Makefile target (typically `make build` produces `bin/copresent-bff`, etc.).

### 2.6 Concept namespace ownership

The `v1:` prefix on concept names stays. Namespace ownership maps to repo ownership:

- Generic namespaces (identity, cluster, platform, common, data, router, memql) → memql core.
- Client namespaces (copresent, cognition, knowledge, copresent-worker) → relevant client BFF repo.

This is the boundary the concept audit (Phase 1) verifies and corrects.

### 2.7 DSL embedding strategy

Each repo embeds its own DSL files via `embed.FS`:

- memql core embeds `concepts/v1/identity/`, `concepts/v1/cluster/`, etc.
- copresent-bff embeds `concepts/v1/copresent/`, plus its own queries / mutations / automations / tools / prompts / providers / shapes.
- The plug-in registration at init time hands the embedded FS to the engine for loading.

Memql core exposes a function the BFF calls during plug-in registration:

```go
// In memql core
func RegisterDSLFS(name string, fsys fs.FS, root string)

// In each BFF's plug-in init
//go:embed concepts/v1/copresent/**/*.memql
//go:embed mutations/v1/copresent/**/*.memql
//go:embed automations/v1/copresent/**/*.memql
//go:embed tools/v1/copresent/**/*.memql
//go:embed prompts/v1/copresent/**/*.memql
//go:embed providers/v1/copresent/**/*.memql
//go:embed shapes/v1/copresent/**/*.memql
//go:embed queries/v1/copresent/**/*.memql
var copresentDSL embed.FS

func init() {
    memql.RegisterPlugin("copresent", func(ctx memql.PluginContext) {
        memql.RegisterDSLFS(ctx, "copresent", copresentDSL, ".")
        // ... other registrations: integrations, route handlers, etc.
    })
}
```

---

## 3. Current state and what's wrong with it

### 3.1 What memql contains today

Pulling from the existing CLAUDE.md:

```
memql/
├── app/                    Phased service bootstrap
│   ├── build_*.go          Per-tag binaries (bff, voice, cognition, agent, planner, identity)
│   ├── integrations_*.go   Per-tag integration wiring
│   └── ...
├── automations/            Event-driven workflows (.memql)
│   └── v1/
│       ├── identity/       CORE
│       └── copresent/      COPRESENT-SPECIFIC ← needs to move
├── component/              Core Go components
│   ├── memql/              Engine
│   ├── grpc/               gRPC server
│   ├── identity/           Identity service
│   ├── server/             HTTP server
│   ├── auth/               Access middleware
│   └── ...                 (and many more)
├── concepts/v1/
│   ├── identity/           CORE
│   ├── cluster/            CORE
│   ├── platform/           CORE
│   ├── common/             CORE (mostly)
│   ├── data/               CORE
│   ├── router/             CORE
│   ├── memql/              CORE
│   ├── copresent/          COPRESENT-SPECIFIC ← move
│   ├── cognition/          AMBIGUOUS — likely COPRESENT-SPECIFIC ← move (audit)
│   └── knowledge/          AMBIGUOUS — likely COPRESENT-SPECIFIC ← move (audit)
├── queries/v1/             ditto — split by namespace
├── mutations/v1/           ditto
├── tools/v1/               ditto
├── prompts/v1/             ditto
├── providers/v1/           ditto
├── shapes/v1/              ditto
├── integrations/           Go integrations
│   ├── auth/               CORE
│   ├── identity/           CORE
│   ├── email/              CORE
│   ├── files/              CORE
│   ├── gcs/                CORE
│   ├── knowledge/          AMBIGUOUS ← audit
│   ├── similarity/         CORE? ← audit
│   ├── training/           AMBIGUOUS ← audit
│   ├── liveavatar/         AMBIGUOUS ← audit (probably copresent)
│   ├── router/             CORE
│   ├── embedding/          CORE
│   ├── stt/                CORE (used by voice node)
│   ├── cognition/          COPRESENT-SPECIFIC ← move (it's the cognition pipeline tied to copresent)
│   ├── agent/              CORE
│   ├── openaiVoice/        CORE
│   └── copresent/          COPRESENT-SPECIFIC ← move
├── cmd/
│   ├── memql-cockpit/      ← extract to its own repo
│   ├── bridge-agent/       CORE
│   └── healthcheck/        CORE
├── proto/                  gRPC defs — CORE
├── core/                   Shared utilities — CORE
├── docs/                   Mixed — see Phase 7
├── docker/                 Mixed — see Phase 7
└── deploy/                 Mixed — see Phase 7
```

### 3.2 The problems this causes

- A new contributor to memql core has to know which concepts under `concepts/v1/` they're allowed to touch and which are copresent's territory. There's no enforcement.
- A copresent concern (e.g., adding a new `v1:copresent:agent` field) shows up as a memql change in the git log, conflating product changes with platform changes.
- The single Makefile carries targets for both core (voice, cognition, identity) and copresent-specific tools (claw, agent reply integrations).
- Adding a second client (cockpit) means more conflation. Adding a third (portal) makes it worse.
- The retired `bff/copresent` branch was a symptom of this — the user tried to physically separate copresent code from main, but the dual-branch overhead was too much. With proper module separation, the same goal is achieved without the merge pain.

---

## 4. Concept audit — initial classification

This is the concrete starting point for Phase 1. Each line below is a hypothesis that needs user confirmation before files move.

### 4.1 Definitely CORE (stay in memql)

```
concepts/v1/identity/*       user, identity, authSession, partitionAccess, invitation,
                             delegation, magicLinkRequest, accessRequest, auditEvent,
                             clusterSettings (when added)
concepts/v1/cluster/*        node, nodeType, spawnEvent, cluster, database, identityProvider
concepts/v1/platform/*       partition, globalSecret, globalVariable,
                             partitionSecret, partitionVariable
concepts/v1/data/*           log, policy, record
concepts/v1/router/*         call, budget, policycatalog, modelcatalog
concepts/v1/memql/*          checkpoint
```

Plus the corresponding `mutations/`, `queries/`, `automations/`, `tools/`, `prompts/`, `providers/`, `shapes/` under those same namespaces.

Plus core Go: `component/`, `core/`, `proto/`, `app/build_*.go` for non-BFF tags, `integrations/` minus client-specific ones, `cmd/healthcheck/`, `cmd/bridge-agent/`.

### 4.2 Definitely COPRESENT-SPECIFIC (move to copresent-bff)

```
concepts/v1/copresent/*      All of it: agent, group, plan, task, taskState,
                             agentAuthorization, canvasState, attachment, config,
                             curriculum, guardrail, media, segment, unmet,
                             onboarding, operatormemory, standingtask, trainingresult
mutations/v1/copresent/*
queries/v1/copresent/*
automations/v1/copresent/*
tools/v1/copresent/*
prompts/v1/copresent/*
providers/v1/copresent/*
shapes/v1/copresent/*
integrations/copresent/*
integrations/cognition/*     ← yes, even though it's named "cognition", it's
                             the cognition pipeline tied to copresent's UX
                             (turn-taking, conductor, agent reply pipeline).
                             Confirm with user.
```

### 4.3 PROBABLY COPRESENT-SPECIFIC (audit needed)

These have generic-sounding names but are actually used in copresent's product context:

```
concepts/v1/cognition/*      space, session, participant, presence, turn, utterance,
                             client/tool/request, client/tool/response, space/context,
                             text/chunk
                             ← These model copresent's session UX. Move to copresent-bff
                             unless cockpit / portal will reuse them.

concepts/v1/knowledge/*      document, spreadsheetRow, imageRegion, validationEvent,
                             domainEntitySchema, entityIndex
                             ← These model copresent's knowledge ingestion. Move
                             to copresent-bff unless explicitly shared.

concepts/v1/common/*         knowledgeDomain, knowledgeBridge, documentChunk
                             ← Despite the "common" name, these are tied to
                             copresent's knowledge feature. Move to copresent-bff
                             unless re-confirmed as truly common.

integrations/knowledge/*     Knowledge ingestion — copresent-specific.
integrations/training/*      Training pipeline — copresent-specific.
integrations/liveavatar/*    Avatar rendering — copresent-specific (UI feature).
```

### 4.4 What about cockpit?

The current `cmd/memql-cockpit/` has no `.memql` files of its own — it's a Go-only TUI that talks to memql via gRPC. So Phase 3 just moves the Go code to a new repo; no concept extraction needed for cockpit at this stage. Cockpit-specific concepts (if any get added in the future) live in `memql-cockpit-bff` once that repo exists.

### 4.5 What about ambiguous packages

Where my guess is wrong, the user corrects in Phase 1. Each correction adjusts the plan.

---

## 5. Phase 1: Lock the concept audit

### 5.1 Goal

Get user-confirmed yes/no on every concept namespace and every `integrations/` subdirectory: stays in core or moves to a client BFF? After this phase, the migration plan is ground-truth.

### 5.2 Deliverables

- A document at `docs/architecture/concept-audit.md` listing every namespace and integration with the user's decision (CORE vs `<client>-bff`).
- For each contested namespace, a short rationale.
- The user signs off before Phase 2.

### 5.3 How to run the audit

Walk through the lists in §4 above with the user, one section at a time. For each ambiguous item:

1. Read the concept files and integration code briefly to understand what they actually do.
2. Ask: "Does this exist for copresent specifically, or could cockpit / portal also need it?"
3. Lock the answer.

The user has explicitly flagged concept misclassification as a concern. Don't speed-run this phase; it sets the boundary for everything else.

### 5.4 Phase 1 commit boundary

Single commit on memql/main: `docs: concept audit for multi-repo migration`. No code moves yet — this phase is purely the audit document.

---

## 6. Phase 2: Define memql's public API surface

### 6.1 Goal

Decide what memql exports as a Go module and what stays internal. After this phase, BFFs can import core memql with confidence that the API won't churn under them.

### 6.2 Deliverables

- Move private packages under `internal/` so they can't be imported from outside the module.
- Document the public API in `docs/architecture/public-api.md`: which packages and types are stable, which are experimental, which are off-limits.
- Add `go.mod` directives to ensure version compatibility.

### 6.3 What needs to be public

For BFFs to register concepts and integrate, memql must export:

- `memql.RegisterPlugin(name, factory)` and `memql.PluginContext` (already exist).
- `memql.RegisterDSLFS(ctx, name, fsys, root)` (NEW — function for BFFs to register their embedded DSL files).
- The `app.Build()` function or equivalent — so a BFF's `cmd/main.go` can construct an app instance using memql core's bootstrap. Likely renamed to `memql.NewApp()` or similar.
- The `Logger`, error types, and other shared utilities used in plug-in factories.
- Audit logger interface (for client BFFs that want to emit audit events).
- gRPC types from `proto/` that BFFs need to inspect.

### 6.4 What should be internal

- The phased bootstrap implementation details (`app/config.go`, `app/database.go`, etc.) — BFFs use the high-level `app.Build()`, not the phases.
- Engine internals (`component/memql/`).
- gRPC server implementation details (`component/grpc/`).
- Database internals (`component/database/`).
- Bus / wiring internals.

### 6.5 Phase 2 commit boundary

Single commit on memql/main: `architecture: define public API surface; move internals to /internal/`. This is a substantial move because every package currently importing from `component/...` may need to switch to importing from `internal/component/...`. Touch every file that needs updating.

---

## 7. Phase 3: Extract memql-cockpit (TUI client)

### 7.1 Goal

Move the cockpit TUI to its own repo so it can evolve independently. After this phase, memql no longer ships a `make cockpit` target — cockpit is built from `~/projects/memql-cockpit/`.

### 7.2 Why cockpit first

It's a strict Go-only client with no `.memql` files of its own. The extraction is purely Go code movement + a new go.mod that imports memql core. No concept audit complexity, no DSL embedding. It's the cleanest first move and proves the architecture before we tackle copresent-bff.

### 7.3 Steps

1. Create the new repo: `~/projects/memql-cockpit/`. `git init`, `go mod init github.com/visionarys-io/memql-cockpit`.
2. Copy `cmd/memql-cockpit/` and `cli/` (the TUI library) from memql to the new repo.
3. Add `require github.com/visionarys-io/memql v<current>` to the new go.mod, plus all the transitive deps.
4. Update import paths in the moved files: `github.com/visionarys-io/memql/cli/...` becomes either still `memql/cli/...` (if cli stays in core — probably no, it's TUI-specific) or `github.com/visionarys-io/memql-cockpit/cli/...`.
5. Move the Makefile cockpit targets to the new repo.
6. Move CI workflow for cockpit binaries to the new repo.
7. Move `deploy/launchd/` and `deploy/systemd/` cockpit-related files.
8. In memql, delete `cmd/memql-cockpit/` and `cli/` and the related Makefile targets.
9. Update memql's CLAUDE.md to remove cockpit references (point at the new repo).
10. Test: build cockpit from its new repo, connect to a running memql cluster, verify it works.

### 7.4 Phase 3 commit boundary

Two commits, one per repo:

- `memql`: `cockpit: extract cmd/memql-cockpit to its own repo`. Removes cockpit from memql.
- `memql-cockpit`: initial commit `cockpit: extracted from memql`. Brings everything in.

Push memql first (since cockpit depends on it). Then push cockpit.

---

## 8. Phase 4: Extract copresent-bff

### 8.1 Goal

Move all copresent-specific code (Go integrations + DSL files) from memql to its own repo. After this phase, memql has zero concepts under `v1:copresent:*`, zero integrations under `integrations/copresent/`, zero copresent-specific anything. The copresent-bff binary is built from `~/projects/copresent-bff/`.

### 8.2 The big move

Based on the locked concept audit from Phase 1, move all client-specific items. Probable list:

```
FROM memql/                          TO copresent-bff/
─────────────────────────────────────────────────────────────────────
concepts/v1/copresent/**             concepts/v1/copresent/**
concepts/v1/cognition/**             concepts/v1/cognition/**       (per audit)
concepts/v1/knowledge/**             concepts/v1/knowledge/**       (per audit)
concepts/v1/common/**                concepts/v1/common/**          (per audit)
mutations/v1/copresent/**            mutations/v1/copresent/**
mutations/v1/cognition/**            mutations/v1/cognition/**
mutations/v1/knowledge/**            mutations/v1/knowledge/**
queries/v1/copresent/**              queries/v1/copresent/**
queries/v1/cognition/**              queries/v1/cognition/**
queries/v1/knowledge/**              queries/v1/knowledge/**
automations/v1/copresent/**          automations/v1/copresent/**
automations/v1/cognition/**          automations/v1/cognition/**
automations/v1/knowledge/**          automations/v1/knowledge/**
tools/v1/copresent/**                tools/v1/copresent/**
tools/v1/claw/**                     tools/v1/claw/**               (claw is copresent-specific)
prompts/v1/copresent/**              prompts/v1/copresent/**
prompts/v1/cognition/**              prompts/v1/cognition/**
prompts/v1/agent/**                  prompts/v1/agent/**            (per audit — likely copresent)
providers/v1/copresent/**            providers/v1/copresent/**
shapes/v1/copresent/**               shapes/v1/copresent/**
integrations/copresent/**            integrations/copresent/**
integrations/cognition/**            integrations/cognition/**
integrations/knowledge/**            integrations/knowledge/**
integrations/training/**             integrations/training/**
integrations/liveavatar/**           integrations/liveavatar/**
app/build_bff.go                     app/build.go                   (renamed; this becomes the BFF main)
app/integrations_bff.go              app/integrations.go            (the integrations-wiring file)
```

### 8.3 The new copresent-bff structure

```
copresent-bff/
├── go.mod                          require github.com/visionarys-io/memql v<X>
├── go.sum
├── Makefile                        with `build`, `dev`, `test` targets
├── cmd/
│   └── copresent-bff/
│       └── main.go                 thin wrapper: imports memql, calls app.Build(), runs
├── app/
│   └── integrations.go             registers copresent plug-ins via memql.RegisterPlugin
├── concepts/v1/
│   ├── copresent/...
│   ├── cognition/...               (per audit)
│   └── knowledge/...               (per audit)
├── mutations/v1/...
├── queries/v1/...
├── automations/v1/...
├── tools/v1/...
├── prompts/v1/...
├── providers/v1/...
├── shapes/v1/...
├── integrations/
│   ├── copresent/...
│   ├── cognition/...
│   └── ...
├── docker/
│   ├── docker-compose.full.yml     for copresent-bff dev environment
│   └── Dockerfile                  for copresent-bff binary
├── docs/                           copresent-bff docs
├── scripts/                        per-repo shell scripts
├── CLAUDE.md                       per-repo agent context
└── embed.go                        //go:embed concepts/**/*.memql ...
```

### 8.4 The `cmd/copresent-bff/main.go` shape

```go
package main

import (
    "log/slog"
    "os"

    "github.com/visionarys-io/memql"
    _ "github.com/visionarys-io/copresent-bff/app"  // side-effect: registers plug-ins
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    app, err := memql.NewApp(memql.AppConfig{
        Logger:  logger,
        Version: version,
        // BFF-specific overrides if any
    })
    if err != nil {
        logger.Error("failed to construct app", "error", err)
        os.Exit(1)
    }
    if err := app.Run(); err != nil {
        logger.Error("app exited with error", "error", err)
        os.Exit(1)
    }
}
```

The plug-in registration in `copresent-bff/app/integrations.go` calls `memql.RegisterPlugin("copresent", ...)` at init time, which then registers the embedded DSL files and any Go integrations.

### 8.5 Steps

1. Create `~/projects/copresent-bff/`. `git init`, `go mod init`.
2. Move all client-specific files per the audit (Phase 1) using `git mv` from inside memql, then re-add in copresent-bff.
3. Update import paths inside the moved Go files: `github.com/visionarys-io/memql/integrations/copresent/...` → `github.com/visionarys-io/copresent-bff/integrations/copresent/...`.
4. Rewrite the moved files' package declarations if they need to change.
5. Add `embed.go` with the appropriate `//go:embed` directives.
6. Add `app/integrations.go` that calls `memql.RegisterPlugin` at init.
7. Add `cmd/copresent-bff/main.go` as the entry point.
8. Build: `make build` in copresent-bff produces `bin/copresent-bff`.
9. Update memql: remove the moved files. Update Makefile to drop the `bff` target. Update CLAUDE.md to reflect the new architecture.
10. Test: bring up memql core (voice + cognition + agent + planner + identity) and copresent-bff together; verify copresent's UI still works against the new BFF.

### 8.6 Phase 4 commit boundary

Two commits, one per repo:

- `memql`: `architecture: extract copresent-specific code to copresent-bff repo`. The big delete from memql.
- `copresent-bff`: initial commit `bff: extracted from memql core`. Brings everything in.

Push memql first. Then push copresent-bff.

---

## 9. Phase 5: Establish `go.work` for parallel development

### 9.1 Goal

Make it trivial to edit memql core and a BFF together without bumping go.mod versions during development.

### 9.2 Deliverables

- `~/projects/go.work` file:

  ```
  go 1.26.1

  use (
      ./memql
      ./copresent-bff
      ./memql-cockpit
  )
  ```

- Add `go.work` and `go.work.sum` to each repo's `.gitignore` (the workspace file is local-dev only; CI and production use pinned go.mod).

- Document the workflow in each repo's CLAUDE.md: "to work on memql + copresent-bff together, ensure `~/projects/go.work` includes both directories. Any local edit to memql is immediately picked up by copresent-bff's build without a `go get`."

### 9.3 Steps

1. Create `~/projects/go.work`.
2. Test: edit memql core; rebuild copresent-bff; confirm the change took effect without a `go.mod` bump.
3. Document.

### 9.4 Phase 5 commit boundary

Three commits (one per repo) just adding `go.work` to `.gitignore` and documenting the workflow. The `go.work` file itself is not committed.

---

## 10. Phase 6: Scaffold memql-cockpit-bff

### 10.1 Goal

Create the BFF that serves the cockpit TUI. Currently cockpit talks to a `bff`-tagged memql binary; after this phase it talks to its own dedicated `memql-cockpit-bff`.

### 10.2 Initial scope

memql-cockpit-bff starts thin. It wraps memql core and exposes the gRPC surfaces cockpit needs. Initially it has no client-specific concepts — just the wrapper. As cockpit grows features (per-cluster settings, ops dashboards, debug surfaces), those concepts land here.

### 10.3 Steps

1. `~/projects/memql-cockpit-bff/`. `git init`, `go mod init`.
2. Mirror the copresent-bff structure but with empty `concepts/`, `mutations/`, etc.
3. `cmd/memql-cockpit-bff/main.go` is a tiny wrapper around `memql.NewApp`.
4. `app/integrations.go` may register no plug-ins initially — just exists.
5. Add to `~/projects/go.work`.
6. Build, verify it runs (does nothing client-specific yet, just core memql).
7. Update cockpit (the TUI) to point at the new BFF endpoint.
8. Document in `memql-cockpit-bff/CLAUDE.md`.

### 10.4 Phase 6 commit boundary

Single commit in memql-cockpit-bff: initial commit, scaffold-only. May also need a copresent / cockpit docs update if the URLs / endpoints change.

---

## 11. Phase 7: Cleanup memql repo

### 11.1 Goal

Memql is now a clean core-only library. Remove leftovers from the migration: dead Makefile targets, stale CI workflows, dead Docker compose files, retired README sections.

### 11.2 Likely cleanup items

- Drop `app/build_bff.go` and `app/integrations_bff.go` (the bff tag is gone).
- Drop `bff` from any Makefile target lists.
- Drop `docker/docker-compose.full.yml`'s copresent-bff sections (those move to copresent-bff repo).
- Drop docs that referenced copresent or cockpit — they're not in this repo anymore.
- Update memql's CLAUDE.md to be a clean "this is a core Go library" doc.
- Update memql's README to point at the per-client BFF repos.

### 11.3 Phase 7 commit boundary

Single commit on memql/main: `cleanup: remove bff tag, copresent references, retired Docker stack`. Last big change in memql for this migration.

---

## 12. Phase 8: Documentation rewrite + cutover

### 12.1 Goal

The architectural shift is reflected everywhere. Operators, developers, and AI agents know where to look for what.

### 12.2 Per-repo documentation

Each repo gets its own CLAUDE.md, README, and `docs/`. Specifically:

- **memql**: "This is the core memQL platform library. Generic concepts, the engine, gRPC server, identity service. Imported by per-client BFF repos. See `<repo>-bff` for client-specific code."
- **copresent-bff**: "Backend for the copresent product. Imports memql core. Registers copresent's concepts, mutations, automations, integrations."
- **memql-cockpit**: "TUI client for memQL operations. Talks to memql-cockpit-bff."
- **memql-cockpit-bff**: "Backend for the memql-cockpit TUI."
- **copresent**: existing CLAUDE.md updated to reflect the BFF split (the URL it talks to may not change, but the team behind that URL is now copresent-bff).

### 12.3 Top-level architecture doc

Create `memql/docs/architecture/overview.md` (or similar) that describes the three-tier model and points to all the repos. This is the high-level "how the system fits together" doc.

### 12.4 Production cutover

- Deploy copresent-bff as the new BFF for copresent. Switch DNS / ingress / service mesh to point copresent's traffic at the new binary.
- Retire the old `memql-bff` deployment.
- Validate end-to-end. Same for cockpit-bff once cockpit traffic is routed through it.

### 12.5 Delete this document

Per the project's no-stale-docs rule, this migration plan deletes itself in Phase 8 once the work is done. The architecture overview doc replaces it as the steady-state reference.

---

## 13. Cross-cutting conventions and gotchas

### 13.1 What stays the same across all repos

- **Branch model**: single `main` branch per repo. No feature branches unless explicitly requested.
- **Commit conventions**: `git add <path>` per file, never `-A`. Commit message style as in the existing repos.
- **Pre-prod deletion**: delete-don't-deprecate rule applies to all repos.
- **Doc hygiene**: each repo's CLAUDE.md and `docs/` get pruned in the same commit as code that obsoletes them.
- **Makefile-canonical**: every operational command is a Make target. Multi-step logic to `scripts/<area>/<name>.sh`.
- **No emojis** in code, commits, or docs.

### 13.2 Cross-repo dependency rule

A BFF imports memql. Memql does NOT import any BFF. There is no circular dependency. If you're tempted to import from a BFF into memql core, the thing being imported should probably be in memql core to begin with — promote it, don't reach.

### 13.3 Versioning

memql uses semver. Tag releases as `v0.x.y` during pre-production, `v1.0.0` once stabilized. BFFs pin specific versions in their go.mod; they upgrade explicitly when a memql release is ready.

### 13.4 Cross-repo PRs

When a change spans memql + a BFF (e.g., adding a new core feature that copresent-bff needs to use):

1. Make the memql change first. Tag a new memql version (or use a pseudo-version during development via go.work).
2. Update copresent-bff's go.mod to the new memql version.
3. Make the BFF change.
4. Both commits land on their respective repos; the BFF's commit message references the memql version it depends on.

During development you can use `go.work` to skip the version-bump step temporarily.

### 13.5 Plug-in registration ordering

If a BFF has multiple plug-ins (e.g., copresent-bff registers a copresent plug-in, a cognition plug-in, and a knowledge plug-in), they're registered via `init()` order. Memql core processes them in registration order. If one plug-in depends on another (e.g., the cognition plug-in needs concepts from the knowledge plug-in), declare the dependency explicitly in the plug-in factory rather than relying on `init()` order.

### 13.6 Docker compose for development

Each BFF repo carries its own `docker-compose.full.yml` for spinning up that BFF + memql core + dependencies (Postgres, etc.). For multi-BFF integration testing, a separate docker-compose at `~/projects/docker/all-bffs.yml` (or similar) ties multiple BFFs together against a shared cluster.

### 13.7 Identity service cluster boundary

The identity service work that's already in flight assumes per-cluster identity. With multi-BFF setups, the cluster boundary is "one memql cluster + multiple BFF binaries serving different products against the same cluster." The identity service runs once per cluster, every BFF authenticates against it, every BFF shares the same user pool. Confirm this model still holds when you look at the identity-implementation-plan.md.

### 13.8 Audit trail across repos

`v1:identity:auditEvent` is in core memql. Both copresent-bff and memql-cockpit-bff write audit events through the core memql audit logger. All cluster-wide activity flows through one audit log regardless of which BFF generated it. Same for `v1:worker:invocation` once the worker initiative ships.

---

## 14. Quick-start: bringing up the multi-repo dev environment

After Phase 5 lands, the workflow looks like this.

### 14.1 Initial setup

```bash
cd ~/projects

# Clone all the repos (URLs as appropriate)
git clone <url> memql
git clone <url> copresent-bff
git clone <url> memql-cockpit
git clone <url> memql-cockpit-bff
git clone <url> copresent

# Create the workspace file
cat > go.work <<'EOF'
go 1.26.1

use (
    ./memql
    ./copresent-bff
    ./memql-cockpit
    ./memql-cockpit-bff
)
EOF

# Per-repo deps
cd memql && go mod download && cd ..
cd copresent-bff && go mod download && cd ..
cd memql-cockpit && go mod download && cd ..
cd memql-cockpit-bff && go mod download && cd ..
cd copresent && npm install && cd ..
```

### 14.2 Daily workflow

```bash
# Edit memql core, see the change reflected in copresent-bff immediately
cd ~/projects/memql
# ... edit some core file
cd ~/projects/copresent-bff
make build           # picks up the local memql change via go.work
./bin/copresent-bff  # runs against the modified core

# Run the full stack
cd ~/projects/copresent-bff
make dev             # docker compose up: postgres + memql core nodes + copresent-bff
cd ~/projects/copresent
make dev             # frontend at localhost:8080
```

### 14.3 Daily make-target reference (per repo)

| Repo | `make build` | `make test` | `make dev` |
|---|---|---|---|
| memql | builds core node-type binaries (voice, cognition, agent, planner, identity) | runs Go tests in memql core | docker compose for memql core + postgres |
| copresent-bff | builds copresent-bff binary | runs Go tests in copresent-bff | docker compose for copresent-bff + memql core + postgres |
| memql-cockpit | builds the cockpit TUI binary | runs Go tests | (n/a — no docker; just runs against a cluster) |
| memql-cockpit-bff | builds memql-cockpit-bff binary | runs Go tests | docker compose for cockpit-bff + memql core |
| copresent | runs Vite + Express dev servers | runs frontend tests | (combined with copresent-bff via separate workflow) |

### 14.4 What to read once you're set up

In order:

1. `<memory>/MEMORY.md` and the `project_*` / `feedback_*` files it references.
2. memql's `CLAUDE.md` (refreshed in Phase 8).
3. `<repo>-bff/CLAUDE.md` for whichever BFF you're working on.
4. The per-repo `docs/architecture/` if any.
5. This document, end to end (until Phase 8 deletes it).

Then proceed to whichever phase is in flight.

---

## Final checklist for the next implementer

Before starting Phase 1:

- [ ] Read `<memory>/project_bff_architecture.md`.
- [ ] Read `<memory>/project_repos.md` and the relevant `feedback_*.md` files.
- [ ] Read this document end to end.
- [ ] Look at memql's current state — read CLAUDE.md, the `concepts/v1/` tree, the Makefile, the `app/build_*.go` files. Understand what's there before classifying.
- [ ] Begin Phase 1 (the concept audit) with the user.

For each phase:

- [ ] Update task list at start (in_progress) and end (completed) of each phase.
- [ ] Stage files explicitly with `git add <path>`. Never `git add -A`.
- [ ] Commit at phase boundary; do NOT push without user validation.
- [ ] Update CLAUDE.md and docs in the same change as the code.
- [ ] Test build + run end-to-end before declaring the phase complete.

Good luck.
