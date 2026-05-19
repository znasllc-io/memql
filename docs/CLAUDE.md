# Documentation Directory

**Purpose:** memQL documentation organized by topic area.
**Quick access:** see [GLOSSARY.md](../GLOSSARY.md) for a complete index.

---

## Layout

```
docs/
├── CLAUDE.md          This file
├── ROADMAP.md         Future-work tracker (deferred items, not abandonment list)
├── SERVICE_ACCOUNT_SETUP.md  GCP service account for Cloud Run deploys
├── polyphon-architecture.md  Voice + video architecture (voice-agent / LiveKit Agents 1.5)
├── PLANNER_TODO.md    Planner integration: remaining v1 build-out work
├── dsl-import-model-refactor.md  In-flight DSL import-model design + migration plan
├── handoff-*.md       Cross-session handoff notes (see "Handoff docs" below)
├── core/              Core concepts and language reference
├── architecture/      Architecture audits, diagrams, and cross-cutting patterns
├── api/               API references
├── auth/              Authentication and authorization
├── voice/             Voice-pipeline tuning notes
├── workbench/         Workbench (sandboxed per-Plan headless surface) ops
├── workers/           Workers (computer-use) ops
├── guides/            Operational + how-to guides
└── planning/          Active planning docs (added during multi-phase work, removed when shipped)
```

---

## Core Concepts (`core/`)

| File | What |
|------|------|
| `arch.md` | System architecture overview |
| `memql.md` | MemQL language reference |
| `memql-functions.md` | Function system (Query / Mutation / Spec / Builtin / Prompt / Provider / Shape) |
| `memql-authoring-rules.md` | **Read before writing `.memql` files.** Numbered list of gotchas (19+) every author hits. |
| `memql-naming-conventions.md` | Naming rules (function-name prefix vs filename-no-prefix, etc.) |
| `memql-specifications.md` | Specs (`@spec`) reference |
| `attribute-matrix.md` | Attribute schema reference |
| `events.md` | Event system + subscription protocol |
| `identifiers.md` | Canonical node id format `{partition}:{concept}:{shortId}`, dispatch-site composition, anti-patterns |
| `permissions_and_access_control.md` | Permissions model |
| `concept_seeding.md` | Seeding concepts into the database |
| `concept-versioning.md` | How v1/v2 versioning works |
| `data-validation.md` | Draft / checked / confirmed lifecycle, policies, identity requirements |
| `build-tags.md` | Build-tag-based binaries (bff / voice / cognition / agent / planner) |

---

## Architecture (`architecture/`)

| File | What |
|------|------|
| `auto-generated-diagrams.md` | Auto-generated arch model + observe runtime + cockpit Topology drill-down. Walks Go source, builds a typed graph (cluster / service / package / type / function), embeds it in the binary, lets the cockpit render it with live observability overlays. Shipped 2026-05-15. |
| `dsl-engine-audit.md` | One-time cleanup audit of `component/memql/**`, `component/language/**`, and the concept parser (~50 KLOC). Identifies tiered cleanup targets. |
| `dsl-engine-before-after.md` | Visual companion to `dsl-engine-audit.md` showing each tier's before / after. |
| `tool-knowledge-domain-pattern.md` | When a capability has operational knowledge (CoPresent Control, Computer Use, Workbench), put it in a knowledge domain the tool requires -- not in the agent prompt template. Read before adding capability-bundled documentation. |

---

## API References (`api/`)

| File | What |
|------|------|
| `audio-streaming.md` | Audio WebSocket (`/memql/audio`) for browser STT/TTS in spaces, plus the streaming-transcription gRPC path on `MemqlService.Stream`. |

The cognition-API frontend reference doc was retired in 2026-04 -- it
was a snapshot of the cognition wire surface that drifted faster than
it could be maintained. The wire is now the proto in
`component/grpc/memql.proto`; query/mutation function names live in
`queries/v1/cognition/` and `mutations/v1/cognition/` and are the
authoritative source.

---

## Guides (`guides/`)

| File | What |
|------|------|
| `env-vars.md` | **Definitive env-var reference.** Bootstrap envelope vs. concept storage; how to add / rotate / override; resolution chain; per-tenant BYOK. |

---

## Auth (`auth/`)

| File | What |
|------|------|
| `access-model.md` | Identity / user / partitionAccess data model. Roles (owner/admin/writer/reader). Stream lifecycle + middleware enforcement. Per-node verifier (JWKS) and PAT path. Cockpit "My Access" panel. |
| `user-provisioning.md` | Registration modes, magic-link flow, invitations, personal partitions, account-deletion cooldown. |
| `identity-service.md` | Operator-side narrative: env vars, deployment topology, key management + rotation, anti-abuse tuning, email delivery. |

---

## Voice (`voice/`)

| File | What |
|------|------|
| `eou-tuning.md` | Voice end-of-utterance (EOU) tuning -- Deepgram `endpointing_ms` / `utterance_end_ms` knobs + a design seed for per-user adaptive endpointing. Read before re-tuning Deepgram or starting on the adaptive layer. |

---

## Workbench (`workbench/`)

| File | What |
|------|------|
| `runbook.md` | Operational guide for the workbench capability -- sandboxed per-Plan Linux working environment; the default first choice for any HEADLESS work an agent needs to do. Covers the in-process MVP. |
| `production.md` | Workbench production deployment plan (multi-node Cloud Run topology). Deferred -- gated on the broader production rollout. |

---

## Workers (`workers/`)

| File | What |
|------|------|
| `runbook.md` | Operator reference for the worker subsystem (computer-use feature). Single source of truth post-implementation. Covers headless + embodied modes, the scope-grant model, kill switch, token mint flow, and install. |

---

## Planning (`planning/`)

Active planning docs only -- removed when work ships. Currently:

- `agent-role-catalog.md` -- Phase 2 plan. Phase 1 (catalog + concept
  + lock semantics) shipped on `feature/role-and-knowledge-catalog`;
  Phase 2 partial on `feature/agent-factory`. Cockpit / CoPresent UI
  for the locked-vs-default-vs-available split and the end-to-end
  async `agentInvocation` Plan dispatch are the remaining gaps.
- `agents-dsl-primitive.md` -- Phases 0-5 shipped on
  `feature/agents-dsl-primitive`; Phase 6 (tighten `capabilities.tools[]`
  to typed reference collection) deferred. Self-deletes when the
  deferred follow-ups land and memql is in steady state.
- `cache-audit-phase-0.md` -- Phase 0 of `llm-driven-decisions.md`:
  cache audit + Ristretto/SI cache instrumentation shipped. Baseline
  numbers pending a week of dev usage.
- `knowledge-seeder.md` -- pipeline shipped, first authoritative seed
  run pending API-spend approval.
- `knowledge-trust-ladder.md` -- 4-tier trust ladder + validation UX
  + Live Knowledge reframe as integration broker. Planning; branch
  `feature/knowledge-trust-ladder`.
- `llm-driven-decisions.md` -- proposed phasing for replacing keyword
  routing in cognition with structured-output LLM classification.
  Phase 0 (cache audit + instrumentation) shipped; Phases 1-2 next.
- `native-calendar.md` -- Native memQL Calendar (concepts + agent
  tool surface + eventual external-calendar mirroring via Live
  Knowledge). Planning; branch `feature/native-calendar`.
- `PARTICIPANT_VIDEO_SPECS.md` -- coordination doc for the
  CoPresent + Polyphon video extension.
- `planner-observability.md` -- Per-Plan token + cost rollup for the
  cockpit. Items 1 and 4 (planFull token/cost fields + cockpit
  display) shipped via memql#32 + memql-cockpit#26; items 2, 3, 5-7
  (`UsageReporter` interface, `siRuntime` plumbing, per-phase timers,
  optional billing concept) outstanding.
- `portal-ai-router-handoff.md` -- product spec for the Portal team
  building admin surfaces for the AI Router (BYOK, budgets, usage
  dashboard). Under product review.
- `SHAPE_DRIFT_HARDENING.md` -- proposed (not shipped) hardening for
  the class of bug where a concept field added without updating the
  shape silently vanishes on the next full-payload update.
- `struct-form-rewriter-retirement.md` -- deferred design proposal
  for collapsing the rewriter passes into grammar + AST changes.

---

## Top-level active docs

Top-level docs that aren't handoffs, planning entries, or reference
material. Listed here so the index is exhaustive.

- [PLANNER_TODO.md](PLANNER_TODO.md) -- single-source-of-truth todo
  list for the remaining v1 planner build-out (Cognition triage
  wire-up, lazy embedding, container-executor work, real budget
  enforcement). Each item carries scope + dependencies + effort.
- [dsl-import-model-refactor.md](dsl-import-model-refactor.md) --
  the import-model pivot landed via PRs #47 / #48 / #49 on 2026-05-19.
  File-top `use <module>.{ names }` imports + concept-in-signature
  signatures (`query <Concept> <name>`, etc.) replaced the legacy
  `@useConcept` / `@use*` family. Doc serves as the shipped-state
  reference now; the migration scripts live under
  `scripts/dsl-imports/`.

---

## Handoff docs

Cross-session handoff notes for ongoing refactors. Removed when the
work fully lands.

- [handoff-prompt.md](handoff-prompt.md) -- meta-template: the briefing
  prompt for onboarding a new implementer (agent or developer). Points
  at CLAUDE.md, the memory directory, and the active handoff list below.
- [handoff-ctx-purge.md](handoff-ctx-purge.md) -- ctx-envelope purge
  (F.1-F.7). Shipped end-to-end; kept as the historical record of the
  migration. Deletable once the repo has cut a release with the
  purged form.

---

## Roadmap

[ROADMAP.md](ROADMAP.md) -- future-work tracker. Items here are
deliberately deferred, not abandoned. Update as scope clarifies or
work moves into a `planning/` doc.

---

## Conventions

- Lowercase-with-hyphens filenames (`auth-troubleshooting.md`).
- Cross-reference with relative paths.
- When a feature ships, **delete** the planning doc and update the
  affected reference docs in the same commit. Don't leave stale
  "deprecated" stubs -- every line of docs eats prompt context.
- No emojis (per global convention).
