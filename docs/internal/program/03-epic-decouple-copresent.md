---
title: Epic 3 — Decouple CoPresent from core
audience: internal
status: historical
area: internal
owner: znas
---

# Epic 3 — Decouple CoPresent from core

Move all CoPresent-specific concepts, logic, and integrations out of core
`memql` into the CoPresent plugin repo's pack, and re-point core services off
`spaceId` onto the generic `partition`. Leaves core a clean, product-agnostic
engine. **Session: S3. Prep starts at G1; core re-pointing at G2. Produces G3.**

**Repos:** `memql` (extract from), `memql-bff-copresent` (move into).
Full inventory: [`05-current-state-map.md`](05-current-state-map.md).

## North star
The `mcp` node already builds engine-only with **no CoPresent DSL**. Goal: the
core service nodes (cognition, voice, voice-agent, planner, agent) can build the
same way, with CoPresent concepts supplied only by the CoPresent pack.

---

## Issue 3.1 — Freeze the move-list inventory [foundation, G:G1]
**Approach:** From the current-state map, produce the authoritative list of what
moves vs stays vs splits, and resolve the open classification calls (library,
liveknowledge, unmetCapability/guardrailHealth, calendar/notes/todos,
forge/actions). Output a checklist the other 3.x issues execute against.
**Acceptance:** Signed-off move-list; every CoPresent symbol has a target.

## Issue 3.2 — Re-point core off `spaceId` → `partitionId` [G:G2] (the big one)
**Scope (coupling counts):** voice integration (38/64 files), cognition engine
(30/41), planner (18/79), data (48 DSL refs), library (30), common (25),
identity (16), agents (12), router (4), platform (4).
**Approach:** Replace `spaceId` scope usage in **core** with the partition API
from Epic 2.2 (`ResolvePartitionFromContext`/`partitionId`). Core stops knowing
about `space`.
**Acceptance:** Core has zero `spaceId` references in engine/service code; tests
green; behavior preserved under a `partition` derived from the old space.

## Issue 3.3 — Split cognition: extract CoPresent concepts to the pack [G:3.2, 2.3]
**Move to pack:** `space`, `space:context`, `micState`, `privateUtterance`,
`misrouteFeedback`, `greetSuppression` (descriptions cite copresent#124/#44/
#252). **Keep in core engine:** session, utterance, turn:state, participant,
presence, audio/videoOverride, client-tool relay.
**Approach:** Move the CoPresent concepts + their queries/mutations/logic/
automations into `memql-bff-copresent/dsl/copresent`; rewire as automations/
routing hooking the core cognition events (per Epic 2.3).
**Acceptance:** Cognition service builds engine-only; `space` etc. live only in
the pack and work when the pack is loaded.

## Issue 3.4 — Move knowledge / guide / curriculum DSL areas to the pack [G:3.1] [P with 3.3]
**Scope:** `dsl/knowledge` (knowledgeDomain, document, knowledgeBridge — fix the
`@namespace("common")` anomaly), `dsl/guide`, `dsl/curriculum`.
**Acceptance:** These areas removed from core; present + functional in the pack.

## Issue 3.5 — Move CoPresent Go integrations to the pack [G:3.1] [P with 3.3,3.4]
**Scope:** `integrations/{avatardirect,avatarvendor,dailyspace,chat,knowledge}`
(plus the CoPresent half of `cognition`/`liveknowledge` per 3.1).
**Approach:** Relocate as pack-registered plugins (`RegisterPlugin`, build-tag
`copresent`).
**Acceptance:** Core builds without these packages; pack registers them.

## Issue 3.6 — Resolve split-case concepts [G:3.1] [P]
**Scope:** library, liveknowledge, unmetCapability/guardrailHealth,
calendar/notes/todos, forge/actions — per the 3.1 decisions (raw engine signal
stays core; product rollups → pack).
**Acceptance:** Each split-case concept lands on its decided side; both sides
build.

## Issue 3.7 — Engine-only verification [G:3.2,3.3,3.4,3.5,3.6] (produces G3)
**Approach:** Build core service nodes with **no CoPresent pack** (the `mcp`
node pattern); confirm zero CoPresent references. Then build a CoPresent
cluster (core + pack) and confirm `space`/knowledge/guide flows work.
**Acceptance (G3):** Engine-only core build green with zero CoPresent symbols;
CoPresent cluster passes an end-to-end space + knowledge flow. Run via a fresh
verification sub-session.

---

## Parallelization within S3
After **G1**: `3.1` (inventory) + start `3.4 [P] 3.5 [P] 3.6` (pure moves not
needing partition). After **G2**: `3.2` (re-point), then `3.3` (cognition
split). `3.7` closes the epic and **opens G3**.
