---
title: Skills v1 (memql#157 / #158 / #159)
audience: internal
status: historical
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Skills v1 (memql#157 / #158 / #159)

Status: Phase 1 shipped 2026-05-21 in memql#157. Phase 2 (consumer
migration) and Phase 3 (`mintSkill` planner authority) tracked separately.

## Problem

Capability bundling on agents is spread across three parallel flat
lists on `v1:agents:agent.capabilities` (`domains[]`, `tools[]`,
`liveSources[]`) and six parallel lists on `v1:agents:agentRole`
(`lockedDomainIds[]`, `defaultDomainIds[]`, `availableDomainIds[]`,
`lockedToolSlugs[]`, `defaultToolSlugs[]`, `forbiddenToolSlugs[]`,
`lockedLiveKnowledgeIds[]`). The agent factory, the cognition scoring
engine, the cockpit role manager, and the planner-driven
`extendSpecialist` / `createSpecialist` flows each reason across
those surfaces independently. The same conceptual "bundle"
(`product-ui knowledge + UI-driving tools`) lives in three
different shapes; keeping them aligned has been ad-hoc.

A **skill** is the single bundling unit the platform reasons about
once Phase 2 lands: one named row that carries `domainIds[] +
toolSlugs[] + liveSourceIds[]`. Roles point at skill ids; agents
inherit skills from their role + per-agent skill attachments. The
Planner Agent's `mintSkill` authority (Phase 3) lets it propose a
new skill row mid-Plan when no existing bundle covers the gap, with
a user-approval canvas card before the row lands.

## Phase 1 -- the concept + the catalog (this PR)

Concepts: `v1:agents:skill`, `v1:agents:skillChangeEvent`.
Validation: load-time `skill.tier >= max(tier across domainIds[])`.
13 predefined seed rows organized in four sections.

| Section | Skill | Tier | Domains (bundled) | Tools (expanded slugs) |
|---|---|---|---|---|
| Foundational | `workbench-baseline` | A | `workbench` | `workbench-use` |
| Foundational | `operator-computer-use` | B | `computer-use` | `workerHost`, `workerComputer`, `workerStatus`, `requestComputerUseScope`, `canvasPublish` |
| Engineering | `go-backend-engineering` | A | `software-development`, `eng-software-architecture`, `api-design` | -- |
| Engineering | `memql-dsl-author` | A | `software-development`, `eng-software-architecture` (*see prerequisite*) | -- |
| Engineering | `grpc-services` | A | `api-design`, `software-development` | -- |
| Engineering | `cockpit-cli-dev` | A | `software-development`, `eng-software-architecture` | -- |
| Product (pack-owned) | `product-ui` | A | `product-ui`, `frontend-development` | -- |
| Product (pack-owned) | `product-api` | A | `product-ui`, `api-design` | -- |
| Product (pack-owned) | `product-canvas` | A | `product-ui` | -- |
| Product (pack-owned) | `product-voice-agents` | A | `product-ui` (*see prerequisite*) | -- |
| Cross-cutting | `knowledge-ingestion` | A | `research-methodology` (*see prerequisite*) | -- |
| Cross-cutting | `web-research` | A | `research-methodology`, `user-research` | -- |
| Cross-cutting | `customer-support-ops` | A | `customer-relations` | -- |

Empty `toolSlugs[]` for most engineering / product skills is intentional
in Phase 1: those skills currently compose through `workbench-baseline`
(universally locked), and the agent factory has no other agent-callable
tool to bind at this layer. Phase 2 layers domain-specific tools (e.g.
`memql-cli`, `cockpit-pty`) on the engineering skills once the tool
registry grows them.

### Tier validation

`component/memql/skill_tier_validation.go` runs the consistency check
over the in-memory SeedRegistry **before** any rows materialize. The
rule is simple: a skill's declared tier must be at least as strict
as the highest-tier domain it bundles. `A < B < C`; missing tier
defaults to A (matches the `knowledgeDomain` default). A violation
is fatal -- the materializer returns an error and app boot fails.

Unknown domain ids (skills referencing a `domainIds[]` entry with no
corresponding seed) are emitted as warnings in Phase 1 and do NOT
fail boot. This is the deliberate carve-out documented inline: Phase 1
ships catalog rows whose domain prerequisites some namespaces don't
yet seed, and hard-failing on those would block this PR from landing
additively. Phase 2 closes the gap by running the same check at
`createSkill` time, where the universe of valid ids includes
user-created domains.

## Knowledge domain prerequisites (Phase 2 backlog)

Three skill catalog entries currently lean on placeholder domain ids
that the knowledge seeder doesn't ship yet. Tracked here so the
follow-up PR knows what to land:

| Skill | Missing domain | Current stand-in | Why it matters |
|---|---|---|---|
| `memql-dsl-author` | `memql-dsl-authoring` | `software-development` | The DSL has rules + gotchas (struct form, import model, trait filters) that a generic engineering domain doesn't cover. |
| `product-voice-agents` | `voice-audio-pipeline` | `product-ui` | LiveKit, the voice-agent worker, the STT/LLM/TTS/avatar chain, the audio + video override family. |
| `knowledge-ingestion` | `knowledge-pipeline` | `research-methodology` | Document validation lifecycle, supersession, per-format item concepts. |

Phase 2's `dsl/agents/skills/*.memql` update swaps these placeholder
ids for the real ones once the knowledge seeder ships them.

## Phase 2 -- consumer migration (memql#158)

- `agentRole` schema: replace the six parallel `*DomainIds[]` /
  `*ToolSlugs[]` lists with `lockedSkillIds[] + defaultSkillIds[]`.
  `availableSkillIds[]` either composes from the picker UX or
  drops (TBD at #158 design time).
- `agent.capabilities`: replace `domains[] + tools[] + liveSources[]`
  with `skillIds[]`. The agent factory resolves skills to the
  flattened tool / domain / liveSource lists at attach time.
- The agent-creation modal + the cockpit role manager rewrite to
  pick skills, not raw domains + tools.
- `skillChangeEvent` rows start landing on every attach /
  reconfigure (Phase 1 ships the concept; Phase 2 writes through it).
- The `domains` / `tools` getters on existing read paths
  (cognition scoring, prompt context, retrieval) stay as
  computed views over the resolved skill bundle so the cognition
  surface doesn't churn.

## Phase 3 -- `mintSkill` planner authority (memql#159)

- Planner Agent gains a `mintSkill` tool. When the planner concludes
  no existing skill covers a Plan's needs, it proposes a new skill
  composition (domains + tools + a name).
- The proposal lands as a canvas card carrying the bundle preview.
  The user clicks Approve / Reject / Edit-then-approve.
- On approval, a new `v1:agents:skill` row materializes (predefined
  = false) plus a `skillChangeEvent` row attributing it to the
  planner + the originating Plan. The planner then attaches it
  to the relevant specialist via the same path as Phase 2's
  `extendSpecialist`.

## Out of scope (forever, or much later)

- **Detach.** The `skillChangeEvent.changeKind` enum reserves
  `detached` but Phase 1 ships only `attached` + `skillReconfigured`
  because no consumer currently reads the negative path. Add when
  there's a real use case.
- **Skill versioning.** Phase 1 keeps skills as live-edited rows
  (predefined = true means the catalog re-seeds them on every
  startup, so the latest source slice wins; user-created = false
  means they're freely editable). Phase 4+ could layer immutable
  versioned skill rows if the audit story needs replayable
  snapshots, but the `skillChangeEvent` log already provides
  per-mutation provenance.
- **Per-partition skills.** Skills stay global (matches the
  `agentRole` decision). Workspace / private scopes get added at
  the row-creation surface if a Phase 5+ need shows up.

## References

- Concept binding pattern: [agentRole](../../dsl/agents/concepts.memql)
- Seed materializer + tier-validation hook: [seed_materializer.go](../../component/memql/seed_materializer.go), [skill_tier_validation.go](../../component/memql/skill_tier_validation.go)
- Knowledge domain tier source: [knowledgeDomain concept](../../dsl/knowledge/concepts.memql), [seed.go](../../integrations/knowledge/seed.go)
- Tool ↔ Knowledge Domain pattern: [architecture/tool-knowledge-domain-pattern.md](../../public/concepts/tool-knowledge-domain-pattern.md)
