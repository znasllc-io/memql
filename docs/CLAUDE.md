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
├── core/              Core concepts and architecture
├── api/               API references
├── guides/            Operational + how-to guides
├── auth/              Authentication and authorization
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

## Planning (`planning/`)

Active planning docs only -- removed when work ships. Currently:

- `portal-ai-router-handoff.md` -- product spec for the Portal team
  building admin surfaces for the AI Router (BYOK, budgets, usage
  dashboard).
- `SHAPE_DRIFT_HARDENING.md` -- proposed (not shipped) hardening for
  the class of bug where a concept field added without updating the
  shape silently vanishes on the next full-payload update.
- `PARTICIPANT_VIDEO_SPECS.md` -- coordination doc for the
  CoPresent + Polyphon video extension.
- `llm-driven-decisions.md` -- proposed phasing for replacing keyword
  routing in cognition with structured-output LLM classification.
  Phase 0 (cache audit + instrumentation) shipped; Phases 1-2 next.
- `knowledge-seeder.md` -- pipeline shipped, first authoritative seed
  run pending API-spend approval.
- `struct-form-rewriter-retirement.md` -- deferred design proposal
  for collapsing the rewriter passes into grammar + AST changes.
- `agents-dsl-primitive.md` -- Phases 0-5 shipped (commit 26a8d65);
  Phase 6 (tighten capabilities.tools) deferred.

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
- [handoff-computer-use-scope-elevation.md](handoff-computer-use-scope-elevation.md) --
  per-task scope-elevation card flow. Feature is absent from the current
  tree; needs a product call (deferred, replaced, or rebuild?) before
  anyone resumes.

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
