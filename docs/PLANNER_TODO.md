# Planner integration: remaining work

Single-source-of-truth todo list for what's left of the v1 planner build-out. Every item below has its **schema, types, prompts, helpers, and UI surfaces already in place** -- what's missing is the orchestration code that wires them into a production loop.

Each item carries: what it is, why it matters, what already exists to support it, what's missing, where to start, rough effort estimate.

**Scope reminder**: today the synchronous file-drop pipeline runs end-to-end (drop file → upload → goroutine extracts + summarizes + lifecycle stamps + canvas card). The items below extend that to the broader v1 surface (chat-triggered Plans, dedup, lazy embedding, container-executor work, real budget enforcement).

---

## 1. Live wire of `cognitionPlanTriage` into cognition handler

**What:** Per Q10, every chat message that arrives gets classified by Cognition for `needsPlan` + `planHint` *in addition to* the existing `nextSpeaker` routing decision. When `needsPlan = true`, the chat agent's reply is the fast "Got it, I'll get back to you" pattern AND a Plan is created with the planHint as the routing tip.

**Why:** Closes the gap between "user drops a file" (works today) and "user asks a Plan-worthy question in chat" (currently answered inline).

**What's in place:**
- Prompt template: `prompts/v1/cognition/cognitionPlanTriage.{memql,tmpl}`
- Plan creation mutation: `mutationCreatePlan` accepts `triggerSource: "user.implicit"`
- Frontend chat completion line already renders for any new Plan via `usePlans` subscription

**What's missing:**
- Cognition handler in `integrations/cognition/cognition_handler.go` needs to call `cognitionPlanTriage` alongside the existing `cognitionRouting` call
- When `needsPlan = true`, the handler emits `mutationCreatePlan` with the message author as `requestedBy`, `triggerSource: "user.implicit"`, and the planHint as `authorizedBy` / `recommendationCardId` source
- The chat agent's reply prompt (cognitionReply.tmpl) needs a branch for "this turn is acknowledging a Plan; respond with 'Got it, I'll get back to you' and stop" so the prompt doesn't try to actually answer the question

**Where to start:** `integrations/cognition/cognition_handler.go` — find where `cognitionRouting` is invoked, add a parallel call to `cognitionPlanTriage`, branch the reply path on `needsPlan`.

**Effort:** ~2-3 hours. Risk: medium — touching the established turn-taking flow needs careful testing against existing scenarios.

---

## 2. Lazy embedding pipeline

**What:** Per Q14, when an agent runs a semantic query against a knowledge domain that has attached `Document`s with `embeddingStatus != 'complete'`, kick off an `embedDomainItems` Plan that takes typed items (`SpreadsheetRow`, `ImageRegion`) and writes embedded `documentChunk` rows into the domain.

**Why:** Closes the loop on knowledge-domain attachment. Today attaching a Document marks it attached; the semantic-retrieval surface still doesn't have embeddings for typed items, so RAG only sees text-style chunks.

**What's in place:**
- `Document.embeddingStatus` enum ('none' / 'partial' / 'complete') + `embeddedItemCount` field
- `SpreadsheetRow.embeddedAsChunkId` / `ImageRegion.embeddedAsChunkId` back-references
- `documentChunk` concept already accepts the `documentId` back-ref (added in v1 expansion)
- Existing embedding integration: `integration.embedding.store` with `vectorField='content'` (used by knowledge corpus seeding)

**What's missing:**
- New Go capability `lazyEmbedDocument(documentId, targetDomainId)` in either `integrations/knowledge/` or a new `integrations/embedder/` package:
  - Reads typed items for the Document (`querySpreadsheetRowsForDocument` / `queryImageRegionsForDocument`)
  - For each, generates a synthetic text representation:
    - `SpreadsheetRow`: either cheap `"key1=val1, key2=val2"` concatenation OR LLM-generated sentence ("Jane Doe is a Director in Engineering"). Q14 recommended starting cheap and tightening if retrieval quality demands.
    - `ImageRegion`: use the `caption` field directly
  - Calls embedding provider, writes `documentChunk` row pointed at target domain, sets `embeddedAsChunkId` back on the source item
  - Updates `Document.embeddingStatus` + `embeddedItemCount` incrementally
- Plan kind `embedDomainItems` + an automation that triggers it on first semantic query against an unembedded domain (or on user request)
- Retrieval capability checks `Document.embeddingStatus` for attached documents and kicks the Plan if needed

**Where to start:** `integrations/knowledge/seed.go` already has the chunk-write pattern (`chunkIdFor`, `purgeChunksForSource`); extend with a per-item-batch version that takes typed items as input. New plan-kind handler in the planner integration (or as an automation that calls the capability directly).

**Effort:** ~3-4 hours.

---

## 3. Entity-schema inference Plan orchestrator

**What:** Per Q17 Option D: when the **second** validated Document lands in a knowledge domain that doesn't yet have an entity schema, fire an `inferEntitySchema` Plan that calls the inference prompt and surfaces a domain-setup canvas card for user confirmation.

**Why:** Unlocks cross-file dedup (Q26). Without an entity schema, the dedup pipeline (item 4 below) can't compute keyHashes.

**What's in place:**
- Prompt template: `prompts/v1/copresent/inferEntitySchema.{memql,tmpl}`
- Concept: `v1:knowledge:domainEntitySchema` with `inferredFromDocumentId` / `inferredFromTaskId` provenance + `confirmedBy` / `confirmedAt` for the user-confirmation step
- Mutation: `mutationCreateDomainEntitySchema`

**What's missing:**
- Automation that triggers on Document validation: queries how many validated Documents this domain has; if >= 2 AND no active schema exists, creates an `inferEntitySchema` Plan.
- Plan handler that calls the prompt with each validated Document's metadata (columnSchema + sample rows), parses the result, writes the proposed schema rows (active=false until user confirms).
- New canvas card variant `domain.entitySchemaProposal` that surfaces the proposal with [Confirm] / [Adjust ▾] / [Skip — no entity model] actions.
- Confirm action flips `confirmedBy` + `confirmedAt`, sets `active=true`, dispatches a domain-changed event.

**Where to start:** New automation in `automations/v1/copresent/inferEntitySchemaTrigger/`. Plan kind handler in the planner integration. New card variant in the front-end (mirrors the existing plan.* variants).

**Effort:** ~2-3 hours.

---

## 4. Dedup-aware analyzer (entityIndex lookup at attach time)

**What:** Per Q26: when the user attaches a Document to a domain that has an entity schema, the analyzer pass runs entityIndex lookups on each typed item, populates `dedupStatus` + `matchesEntityIndexId` + `diffFromMatched`, and surfaces the multi-step card state with counts (213 new, 25 duplicates, 5 updates).

**Why:** Closes the load-bearing v1 use case the user explicitly asked for ("you upload new-hires.xlsx and 30 employees already exist").

**What's in place:**
- `EntityIndex` concept + `mutationCreateEntityIndexEntry` + `queryEntityIndexLookup`
- `SpreadsheetRow.dedupStatus` / `matchesEntityIndexId` / `diffFromMatched` fields
- PlanCompletedCard `dedupResolution` state machine ('idle' / 'checking' / 'resolving' / 'attached')
- The card's attach handler already advances state through these phases (currently short-circuits to 'attached' since dedup pipeline isn't running)

**What's missing:**
- Go capability `runDedupAgainstDomain(documentId, targetDomainId)` that:
  - Loads the active entity schema for the target domain
  - For each typed item in the Document, computes `keyHash = sha256(normalize(keyFields))`
  - Calls `queryEntityIndexLookup` for each hash
  - Updates the item via `mutationUpdateSpreadsheetRow` with the appropriate `dedupStatus` + `matchesEntityIndexId` + `diffFromMatched`
  - Returns aggregate counts so the card can render them
- The PlanCompletedCard's `handleAttach` calls this capability via a new Plan (kind=`dedupCheck`) and reads the counts to populate `dedupResolution.counts`
- The 'resolving' phase UI: rows table with side-by-side existing-vs-incoming for 'update' rows, [Add new + skip duplicates] / [Review updates...] / [Add anyway (force)] / [Cancel attach] action buttons
- After resolution, validated 'new' items get an `entityIndex` row stamped via `mutationCreateEntityIndexEntry`

**Where to start:** Capability lives next to the lazy-embedding pipeline (item 2) — they share the per-item iteration pattern. Front-end work is in `PlanCompletedCard.tsx` + a new `DedupResolutionDrawer.tsx` component.

**Effort:** ~3-4 hours (depends on item 3 shipping first; the schema needs to exist before dedup can run).

---

## 5. NemoClaw `init()` registration as ContainerExecutor

**What:** Wire NemoClaw as the first registered backend in the container-executor registry so Tasks with `executionSurface: containerExecutor` and `executorBackend: "nemoclaw"` route through the NemoClaw webhook tools.

**Why:** Unblocks long-running Plans that need a sandboxed container (file-system access, CLI work, builds). Current Tasks all use `executionSurface: inProcess` since no executor is registered.

**What's in place:**
- `component/planner/executor.go` ContainerExecutor interface + registry
- NemoClaw is invoked today via the webhook tools in `tools/v1/claw/` (`clawExecuteTask`, `clawReadFile`, `clawListFiles`, `clawSearchCode`); there is no Go integration package yet.

**What's missing:**
- A new `integrations/nemoclaw/` package that implements the ContainerExecutor interface against the NemoClaw HTTP gateway and calls `planner.RegisterContainerExecutor("nemoclaw", ...)` from `init()`
- Mapping from Task kinds (e.g. `runCommand`, `browseUrl`) to NemoClaw tool invocations (the `tools/v1/claw/*` definitions are the existing surface to wrap)
- Progress-event streaming wired into the registered executor's `Run` method

**Where to start:** Create `integrations/nemoclaw/` -- the existing webhook-tool definitions in `tools/v1/claw/` document the gateway contract. The init() registration is one line.

**Effort:** ~1-2 hours.

---

## 6. Live token-budget enforcement integration

**What:** Per Q6: agent's tool-call wrapper invokes `EngineTokenBudget.CheckCall(planId, estimatedCallTokens)` before each LLM/tool call; if it returns an error, the Task transitions to `failed` with reason `tokenBudgetExceeded`.

**Why:** Without this, `tokenBudget` is informational only. The user can't actually cap a runaway plan.

**What's in place:**
- `component/planner/budget.go` TokenBudget interface + EngineTokenBudget implementation
- `tokenBudgetSoftWarning` automation already fires at 75/90% spend
- Plan fields: `tokenBudget` / `tokenSpent` / `tokenAllocatedToChildren` / `tokenCapDisabled`

**What's missing:**
- `PlanLookup` implementation that reads `Plan.tokenBudget` etc. via the engine (small adapter — wraps `queryPlanById`)
- Wiring `EngineTokenBudget.CheckCall` into the agent's tool-call dispatcher in `integrations/agent/` (or wherever the LLM call site is)
- Token-spent accounting: after each successful call, increment `Plan.tokenSpent` via `mutationUpdatePlanStatus`
- Sub-plan allocation: when a child Plan is spawned, parent must carve `tokenAllocatedToChildren` out of remaining budget

**Where to start:** Find the agent's central LLM-call site (probably wraps the SI provider call). Inject the TokenBudget check + the post-call accounting.

**Effort:** ~2-3 hours.

---

## 7. Community-tier permission gating for role categories

**What:** Filter the Create Agent role picker by user tier — community tier sees only `personal`-category roles; paid tier sees `business` + `personal` + `universal`. Knowledge-domain picker filters the same way.

**Why:** Core monetization gate. Personal-tier users get the everyday-life agents (personal finance, parenting, household, etc.); business roles + their integrated tools are paid.

**What's in place:**
- `RoleCategory` union type (`universal | business | personal`) on every entry of `AGENT_ROLES` in `copresent/src/utils/agentDefaults.ts`.
- `BUSINESS_ROLES` / `PERSONAL_ROLES` selector arrays exported.
- 15 personal-category roles + 30+ personal-category knowledge domains seeded with role mappings.
- Knowledge domains carry `category` enum already (`core` / `business` / `technical` / `product` / `internal`); the new personal domains use `product` as a stopgap until a `personal` category enum value is added.

**What's missing:**
- A `tier: 'community' | 'paid'` field on `v1:identity:user` (or `User.preferences`).
- Filter logic in CreateAgentModal's role picker that branches on the user's tier and only shows the matching category.
- Two-tab visual on the role picker: when both categories visible, picker becomes [Personal | Business] sub-tabs (mirrors the existing Personality tab's sub-category pattern).
- Backend gate: when a `community`-tier user tries to create a `business`-category agent, `mutationCreateAgent` returns a permission error.
- Same tier-filter applied to knowledge-domain pickers and Knowledge page.
- Add a `personal` value to the `KnowledgeDomain.category` enum (currently personal domains use `product` as a stopgap).

**Where to start:** `copresent/src/components/agents/CreateAgentModal.tsx` for the role-picker filter + tab UI; `copresent/src/utils/permissions.ts` for the tier-check helper; `mutations/v1/copresent/createAgent.memql` for the backend gate.

**Effort:** ~2-3 hours.

---

## Overall

Total remaining: ~15-22 hours of focused Go + frontend integration work. Items 2, 3, 4 are tightly coupled (entity inference → dedup analyzer → embedding) and best done together. Items 1, 5, 6, 7 are independent and can ship in any order.

When picking one up, the supporting infrastructure is already in place — schema, prompts, helpers, mutations, queries, UI surfaces. The work is the orchestration code that connects them.

For the latest decision rationale on each, see the brainstorm transcript in the conversation history (Q1-Q26).
