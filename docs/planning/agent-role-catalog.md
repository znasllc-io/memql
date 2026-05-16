# Agent Role Catalog -- Phase 2 plan

**Status:** Phase 1 shipped on `feature/role-and-knowledge-catalog`
(catalog + concept + lock semantics). This doc covers Phase 2:
GA-driven auto-creation, server-side lock enforcement, and the
cockpit / CoPresent surfaces that consume the catalog. Deletes
itself when Phase 2 ships, per the no-stale-docs convention.

---

## Where Phase 1 landed

- New first-class concept `v1:agents:agentRole` (global scope) -- the
  spine of agent identity. Carries `lockedDomainIds`, `defaultDomainIds`,
  `availableDomainIds`, `lockedToolSlugs`, `defaultToolSlugs`, `tier`,
  `recommendedPolicySlug`, and `systemPromptHints`.
- `v1:common:knowledgeDomain` gains `lockedForRoles []string` --
  reserved as a future domain-side index of the role catalog's
  `lockedDomainIds`. Not populated by the seeder today; enforcement
  reads from the role row directly. A startup hook can fan out the
  inversion if retrieval-side queries on `lockedForRoles` become hot.
- **Role catalog is seed-driven.** 98 predefined roles authored as
  `seed <slug> { ... }` blocks under `dsl/agents/roles/` (12 files
  grouped by category: professional, medical, legal, trades, creative,
  education, scientific, hospitality, civic, transportation,
  agriculture, personal). The `SeedMaterializer` (PR #13) materializes
  one `v1:agents:agentRole` row per seed at startup; idempotent.
- Knowledge-domain catalog expanded 253 -> 509 entries (Go-side
  `seedStandardDomains` capability) to back the new role set. Domain
  seeding stays Go-side because the catalog pipeline does heavy work
  (LLM content generation, Wikipedia fetch for Tier C); the catalog
  capability is the right shape for that.
- DSL surface: `dsl/agents/mutations.memql` carries
  `mutationCreateAgentRole` (the seed materializer's write path);
  `dsl/agents/queries.memql` carries `queryAgentRoleBySlug` and
  `queryActiveAgentRoles`; `dsl/agents/shapes.memql` carries
  `agentRoleFull`.

What is NOT in Phase 1:

- The GA-driven auto-create flow (the agent factory that consumes the
  catalog when no fitting specialist exists).
- Server-side lock enforcement in `mutationUpdateAgent`.
- The cockpit / CoPresent UI surfaces that render locked vs default
  vs available with the right affordances.
- The "expand the agent's knowledge / capabilities" flow that
  enforces the same locks on add-ons.

---

## Phase 2 -- what we build next

### 1. GA-driven auto-creation flow

The general assistant is the gatekeeper. When a conversation reveals
that no existing agent fits the user's need, the GA proposes (and on
approval, creates) a new specialist by walking the role catalog.

**Trigger:** During cognition's per-turn routing, the conductor
already computes `fitScore` per candidate agent. When the
best-candidate score falls below a `roleGapThreshold` (proposed
default: 0.4) for two consecutive turns on the same topic, the GA
takes over with a `proposeSpecialist` turn mode.

**Conversation shape:** the GA reads the conversation transcript +
the active user's existing agents and prompts the role catalog (via
the existing `agent("name", args)` builtin -- here we'd call a new
`roleSuggest` agent declared in `dsl/agents/`). The result is one
or more candidate `agentRole` rows ranked by fit. The GA then asks
the user: "It sounds like you're after X. I can create a `{role.name}`
agent for you -- they ship with `{N}` mandatory knowledge areas and
`{M}` mandatory tools. Should I?" The user says yes / no / pick a
different role.

**Materialization:** on confirmation, a new `mutationCreateAgentFromRole`
(forthcoming, lives in `dsl/agents/mutations.memql`) writes the
`v1:agents:agent` row using the role catalog as the template:

- `roleSlug` = `agentRole.slug`
- `capabilities.domains` = union(`lockedDomainIds`, `defaultDomainIds`)
- `capabilities.tools` = union(`lockedToolSlugs`, `defaultToolSlugs`)
- `providerConfig.llm.policyName` = `recommendedPolicySlug`
- `gender` = `recommendedGender` (or fallback to whichever bucket has
  more unused canonical voices for this owner)
- `systemPrompt` = the role's `systemPromptHints` prepended to the
  generic specialist prompt template

**Naming:** the GA picks a name from the canonical-voice catalog
(`voicePickForGender`) and asks the user "Their voice will be `<voice>`.
Sound good? Or pick another name?" The agent is created on confirm.

**Bookkeeping:** the agent gets `lineage.extendedFromAgentId =
generalAssistant`, `lineage.extensionGoals = [the conversational
need that triggered the create]`, so the Q10 layered dedupe path
can see why it was minted.

### 2. Server-side lock enforcement

`mutationUpdateAgent` runs a new pre-insert validator that loads the
agent's `agentRole` row by `roleSlug` and checks:

- Every id in `agentRole.lockedDomainIds` is present in the
  proposed `capabilities.domains`. If one is missing, reject with a
  typed error `LockedDomainRemoval{ slug, missing }`.
- Every slug in `agentRole.lockedToolSlugs` is present in the
  proposed `capabilities.tools`. Same shape of error.
- Every id in `agentRole.lockedLiveKnowledgeIds` is present in the
  proposed live-knowledge attachments.

The enforcement runs on every write path -- the cockpit edit modal,
the CoPresent edit modal, the GA's `extendAgent` tool, mutations
issued by automations. There is no "force override" flag at this
layer: a locked id is locked. Adding new locked ids to the role
catalog at seed time has the effect of growing every existing
agent in that role on the NEXT mutation; we don't retroactively
mutate-and-write, but we do reject any write that doesn't include
the new locks.

Concept-side: the `v1:common:knowledgeDomain.lockedForRoles` field
is reserved for a domain-side inversion of the role catalog. If
retrieval-side queries on `lockedForRoles` become hot, a startup hook
walks the materialized `agentRole` rows and updates each
`knowledgeDomain` row's `lockedForRoles`. Not populated by the seeder
in Phase 1; enforcement reads from the role row directly.

### 3. Cockpit + CoPresent UI affordances

The agent-creation modal (CoPresent's `CreateAgentModal`) and the
Training panel render the locked / default / available split:

- **Locked** rows: greyed out with a lock icon. Hovering says
  "Required for this role." Cannot be deselected.
- **Default** rows: selected by default; can be deselected.
- **Available** rows: unselected by default; can be selected.

Same UI for tools.

On the cockpit side, the architecture-model navigator (shipped on
`feature/auto-generated-diagrams`) gets a parallel "role catalog"
view: drill into a role -> see its locked / default / available
domains + tools as a tree. Same right-pane chrome, X-toggled like
the architecture navigator.

### 4. Agent-driven expansion (not user-driven)

Per the user direction: end-users don't add knowledge / capabilities
to agents directly. The flow is always:

1. User asks the GA: "Can you teach `<agent>` about `<topic>`?"
2. GA decides whether `<topic>` maps to an existing domain. If yes,
   GA proposes adding it to the agent. If no, GA proposes creating
   a new (user-scoped) domain first, then attaching it.
3. On confirm, GA calls a new `mutationExtendAgentDomains` (writes
   to `agent.capabilities.domains`).
4. Locked ids stay locked; everything else can be added.

The CoPresent UI's "manual edit" path stays available for power
users but routes through the same validator, so the locks apply
regardless of entry surface.

### 5. Sunset of `roleDomainMap`

`roleDomainMap` in `seed.go` is kept in Phase 1 for backward-compat
with the older `rolesForDomain` derivation. Phase 2 removes it; the
role catalog becomes the sole source of truth, and `rolesForDomain`
is rewritten to read `rolesForDomainFromCatalog` (already shipped
in role_seed.go).

---

## Open questions for Phase 2

1. **What constitutes "no fitting agent"?** The threshold is a
   simple fitScore floor in the proposal; we may need a more
   nuanced signal (multi-turn dissatisfaction, explicit user
   request) before triggering an auto-create. Start with the floor;
   measure; tune.

2. **Tier C role creation gating.** Tier C roles (clinical medicine,
   legal practice, etc.) ship without authoritative content -- the
   seeder writes a placeholder telling the user to upload their own.
   Should the GA refuse to mint a Tier C agent without first
   confirming the user understands the advisory framing? Proposed:
   yes -- a one-time consent dialog the first time a Tier C role
   is selected per user.

3. **Cross-role agents.** Today an agent has one `roleSlug`. Some
   real specialists span two roles ("HR + payroll", "operations +
   project management"). Proposed for v3: add a `secondaryRoleSlug`
   field; the validator unions locks across both rows. Defer until
   we see real demand.

4. **Role versioning.** If a predefined role's locked set changes
   (we add a domain), every existing agent of that role inherits
   the new lock on next mutation. Is that surprising? Probably yes
   for power users. Proposed: stamp `roleVersion` on the agent at
   creation; the validator checks against the stamped version, not
   the current catalog row, until an explicit "upgrade role"
   action is taken.

---

## Files that change in Phase 2 (preview)

| Path | What |
|---|---|
| `dsl/agents/mutations.memql` | + `mutationCreateAgentFromRole`, + `mutationExtendAgentDomains`, + `mutationExtendAgentTools` |
| `dsl/agents/prompts.memql` + templates | + `roleSuggest` prompt, + agent factory prompt |
| `dsl/agents/automations.memql` | + `enforceRoleLocksOnAgentWrite` |
| `component/memql/` mutation validator | + lock-enforcement validator path |
| `memql-bff-copresent/dsl/copresent/mutations.memql` | wire createAgent through createAgentFromRole |
| `memql-cockpit/cli/cluster/architecture.go` | + role catalog drill-down (parallel to arch model) |
| (CoPresent repo, not in this workspace) | CreateAgentModal + Training panel UI surfaces |
| `dsl/agents/roles/` | continue expanding the catalog as gaps surface; one seed per role, grouped by category |

---

## Where to read more

- Phase 1 role catalog: `dsl/agents/roles/` (12 per-category seed files).
- Phase 1 domain catalog: `integrations/knowledge/seed.go` (the
  expanded domain block).
- Concept surfaces: `dsl/agents/concepts.memql`,
  `dsl/knowledge/concepts.memql` (lockedForRoles).
- Agents-as-DSL-primitive context: `docs/planning/agents-dsl-primitive.md`.
- Seed primitive context: `component/memql/seed_parser.go`,
  `component/memql/seed_materializer.go`.
