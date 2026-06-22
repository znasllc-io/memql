# Epic 3 — CoPresent Move-List (3.1) — SIGNED OFF

Authoritative inventory of what **moves** to the CoPresent pack
(`memql-bff-copresent`), **stays** in core `memql`, or **splits**. Produced by
issue #1898 (3.1) off post-rename `main` (`715e8db`); **owner-ratified 2026-06-22**.
This supersedes the first-pass classification calls in
[`05-current-state-map.md`](05-current-state-map.md) §G. Executed against by 3.4
(#1901, DSL areas), 3.5 (#1902, Go integrations), 3.6 (#1903, split-cases), and
referenced by 3.2 (#1899, partition re-point) + 3.3 (#1900, cognition split).

**Guiding principle (owner-stated).** memQL core is the LLM **harness + platform**.
CORE = anything a non-CoPresent deployment reuses (the `mcp` node, telephony/Epic 4,
Anaquim / CNATs clusters) — including the whole agent framework, knowledge, planner,
and forge. COPRESENT = only what is tied to the CoPresent **`space`** concept and
its direct product behaviors/surfaces. SPLIT = keep the engine primitive in core,
move just the `space`-binding/product-rollup to the pack.

> **Owner sign-off corrections (2026-06-22)** vs the first-pass map:
> - **forge → CORE** (it is part of core memql; its "NOT core" docstring is wrong and is fixed here).
> - **agents → CORE in full** — `agent`/`agentRole`/`skill`/`agentAuthorization` are all core platform (there is an `agent` *service*). Only `space`-coupled refs get the partition cleanup (3.2).
> - **knowledge → CORE** — knowledge domains/documents/bridge/entityIndex are a core platform RAG capability, not a CoPresent product. (Fix the `@namespace("common")` anomaly *in core*.)
> - **liveknowledge → CORE** — live-retrieval is a core primitive.
> - **library → SPLIT** — provenance/version spine core; `space`-faceting → pack.

---

## What actually moves to the pack (the whole COPRESENT set)
1. **cognition product-half** (3.3): `space`, `space:context`, `micState`, `privateUtterance`, `misrouteFeedback`, `greetSuppression` + their queries/mutations/logic/automations.
2. **guide**, **curriculum** DSL areas (3.4).
3. **Go integrations** (3.5): `avatardirect`, `avatarvendor`, `dailyspace`, `chat`.
4. **library space-faceting** (3.6 split): the `space`-scoped Library-panel queries only.

Everything else is CORE (with `spaceId → partitionId` cleanup handled by 3.2, which is a *re-point, not a move*).

---

## A. DSL concept areas (`memql/dsl/*`)

| Area | Disposition | Note |
|---|---|---|
| memql, cluster, observability, safety, policies, providers, router | CORE | infra/engine; zero coupling |
| identity, platform, data, authoring, harness | CORE | infra/data; spaceId leaks → partition (3.2), not moves |
| **agents** (agent, agentRole, skill, agentAuthorization) | **CORE** | full agent framework is core platform; only space-coupled refs → partition |
| **knowledge** (knowledgeDomain, document, documentChunk, knowledgeBridge, entityIndex, spreadsheetRow, imageRegion, validationEvent, domainEntitySchema, liveSource/liveConnector/liveSnapshot) | **CORE** | core RAG/knowledge platform. **Fix `@namespace("common")` anomaly → stays in core `knowledge` ns** |
| planner (plan, task, taskState, responsibility) | CORE | generic; 18 spaceId leaks → partition (3.2) |
| **cognition** | **SPLIT → 3.3** | CORE: session, utterance, turn:state, participant, presence, audio/videoOverride, client-tool relay. PACK: space, space:context, micState, privateUtterance, misrouteFeedback, greetSuppression |
| cognition: unmetCapability / guardrailHealth | CORE | routing-engine telemetry; spaceId→partition (3.2). (Not in the 3.3 product-half move.) |
| common | SPLIT | attachment/media stay CORE (storage; spaceId→partition). `knowledgeDomain` mis-tagged here → move tag to core `knowledge` ns (still CORE) |
| **guide** | COPRESENT → 3.4 | CoPresent Control walkthroughs; reference space |
| **curriculum** | COPRESENT → 3.4 | onboarding/learning content |
| **library** (artifact, generatedOutput, documentVersion, memory) | **SPLIT → 3.6** | CORE: provenance + version spine (any agent produces files). PACK: `space`-faceted Library-panel queries only |
| calendar, notes, todos | CORE | generic assistant primitives; no space coupling; `@mcp` tools |
| **forge** (project, request, requestEvent) | **CORE** | core memql team-ops backbone; partition-scoped, own MCP surface. **Docstring fixed** |
| actions (candidate, action, surface) | CORE | planner replay/action library (#1734); engine capability |
| workbench (workspace) | CORE | per-Plan sandbox; 1 leak → partition |
| worker (registration, invocation) | CORE | execution audit; computer-use scope semantics already in `dsl/copresent` |

## B. Go integrations (`memql/integrations/*`)

| Integration | Coupling | Disposition | Note |
|---|---|---|---|
| openai, openairealtime, stt, audio, voice(catalog), router, embedding, similarity, auth, identity, database, azureblob, fileprocessor, email, timeutil, training, harness*, actionsearch, agentdef | low | CORE | infra/engine; telephony reuses the voice/Realtime lane |
| agents (4/5), agent (6/23) | med | CORE | agent framework; spaceId/copresent refs → partition + envelope cleanup (3.2) |
| **knowledge** (7/11) | — | **CORE** | knowledge-domain seeding/bridge is core RAG; spaceId→partition only |
| **liveknowledge** (0/3) | — | **CORE** | clean retrieval dispatcher; core primitive |
| **voice** (38/64) | high | CORE | heavy spaceId → partition (3.2); telephony attaches here |
| **planner** (15/80) | — | CORE | planning engine; spaceId/copresent → partition (3.2) |
| **cognition** (30/41) | high | SPLIT → 3.3 | engine routing/conductor/relay stay; product behaviors → pack |
| **dailyspace** (4/5) | — | COPRESENT → 3.5 | space lifecycle |
| **avatardirect** (3/3) | — | COPRESENT → 3.5 | direct/Guide avatar |
| **avatarvendor** (3/10) | — | COPRESENT → 3.5 | shared by voice-agent + avatardirect; pack |
| **chat** (2/2) | — | COPRESENT → 3.5 | space-scoped chat tool |
| **library** (2/3) | — | SPLIT → 3.6 | version/edit engine → CORE; space facet → pack |
| workbench (1/12) | — | CORE | sandbox; partition cleanup |

## C. Tools / MCP surface
Already pack-correct in `memql-bff-copresent/dsl/copresent`: canvasPublish, claw*,
worker*, workbenchHost, requestComputerUseScope, ui* operator/teach tools.
Stay CORE: calendar/notes/todos/memql tools (all `@mcp`), actions tools, **forge tools**.

## D. Registration mechanics (for 3.5)
The four COPRESENT integrations (avatardirect, avatarvendor, dailyspace, chat)
become build-tag-gated `//go:build copresent` plugins registered from the pack via
`RegisterPlugin` + `RegisterTree`, with cross-node behavior re-homed onto
`node.RegisterRoutingRule` / `RegisterConceptOwnership`. Engine-only core (the `mcp`
pattern) then drops them at compile time. Knowledge/liveknowledge/forge/agents stay
unconditionally compiled in core.

---

## Downstream issue rescoping (from this sign-off)
- **3.4 (#1901):** moves **guide + curriculum** DSL only (id-preserving via pack `RegisterTree`). Two tails were spun out during execution: the knowledge `@namespace` fix is a **breaking id migration** → **#1960** (knowledge stays core); the live `guide` suggest path rides the core `AiSuggest` gRPC handler, a CoPresent-coupled surface the inventory missed → **#1959** (decouple via an extension point; blocks G3). `guide_suggest.go` stays in core until #1959.
- **3.5 (#1902):** moves **avatardirect, avatarvendor, dailyspace, chat** only. Knowledge integration **stays core**.
- **3.6 (#1903):** the one real split is **library** (spine core / space-facet pack). All other former "split-case" concepts (forge, actions, calendar/notes/todos, liveknowledge, unmetCapability/guardrailHealth) are **confirmed CORE** — no move.
- **3.2 (#1899):** unchanged — `spaceId → partitionId` across voice/cognition/planner/data/common/identity/agents/router/platform.
- **3.3 (#1900):** unchanged — extract the cognition product-half listed above.
