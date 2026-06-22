# MemQL ↔ CoPresent — Current-State Architecture Map

Snapshot of where every concept, integration, tool, and service **currently
lives**, and a first-pass classification, so we can design the cleanest
decoupling. Grounded in the repos as they stand (June 2026).

**Legend (classification):**
- **CORE** — product-agnostic platform/engine. Stays in `memql`. May still
  need `spaceId → partitionId` cleanup to remove CoPresent meaning.
- **COPRESENT** — product-specific. Moves to the CoPresent plugin repo's
  embedded `dsl/copresent` pack (and/or its Go).
- **SPLIT** — currently one unit, but part is engine (core) and part is
  product (CoPresent). Requires surgical separation.

**Key correction:** the **polyphon multi-agent voice engine** and the
**cognition conversation engine** are CORE. CoPresent is the `space` concept
and the product behaviors layered onto them. Rooms are named
`polyphon-<spaceId>` — core engine, CoPresent parameter.

---

## A. Repos / services overview

| Repo / service | What it is | Classification | Notes |
|---|---|---|---|
| `memql` | Engine, DSL core, integrations, deploy | **CORE** (with CoPresent concepts to extract) | The harness IP |
| `memql-bff-copresent` | Thin Go carrier + `dsl/copresent` pack + BFF gRPC surface | **COPRESENT (plugin repo)** | Target home for extracted CoPresent concepts |
| `memql-cockpit` | Terminal-native operator IDE/console for clusters | **CORE (generic)** | ~1 file couples to space/copresent; basically clean |
| `copresent` | React SPA (presence, canvas, chat, avatars) | **COPRESENT (frontend)** | Already separate; consumes the BFF gRPC surface |

---

## B. Core DSL concept areas (`memql/dsl/*`)

| Area | Key concepts | Classification | Evidence / note |
|---|---|---|---|
| memql | checkpoint | **CORE** | engine |
| cluster | cluster, database, node, nodeType, spawnEvent | **CORE** | infra/topology |
| identity | user, oauth*, authSession, invitation, delegation | **CORE** | auth (16 spaceId leaks → partition) |
| platform | secrets, variables, policyTrace | **CORE** | runtime (4 leaks) |
| providers | providers | **CORE** | model/integration providers |
| router | budget, call, modelCatalog, policyCatalog | **CORE** | model routing (4 leaks) |
| safety | classification, approvalRequest, outputScreening | **CORE** | guardrails |
| worker | invocation, registration | **CORE** | workers |
| observability | codeProfile, invocation, codeMetric | **CORE** | telemetry |
| data | log, policy, record | **CORE** | data layer (48 leaks → partition) |
| authoring | bundle, construct, dependencyEdge | **CORE** | DSL authoring |
| harness | plan, step, observation, semanticMemory | **CORE** | agent eval harness |
| common | attachment, media | **CORE** | shared (25 leaks) |
| agents | agent, agentRole, skill, operatorMemory, avatarPersona | **CORE** (avatarPersona?) | agent framework (12 leaks) |
| planner | plan, task, taskState, responsibility | **CORE** | space-coupled (17 leaks → partition) |
| **cognition** | session, utterance, turn, presence, audio/videoOverride, client-tool relay | **CORE (engine half)** | the conversation/voice engine |
| **cognition** | **space**, space:context, micState, privateUtterance, misrouteFeedback, greetSuppression | **COPRESENT (product half)** | **SPLIT** — descriptions cite copresent#124/#44/#252 |
| cognition | unmetCapability, guardrailHealth | **SPLIT (recommend)** | raw signal core; product rollup → CoPresent |
| **knowledge** | knowledgeDomain, document, knowledgeBridge, entityIndex, liveSource | **COPRESENT** | knowledge-domains product (namespace anomaly: tagged `common`) |
| **guide** | guide, scene | **COPRESENT** | guided walkthroughs; references space |
| **curriculum** | curriculum, segment | **COPRESENT** | onboarding/learning content |
| calendar | calendarEvent | **CORE (shared feature)** | assistant feature; generic |
| notes | note | **CORE (shared feature)** | assistant feature; generic |
| todos | todo | **CORE (shared feature)** | assistant feature; generic |
| library | artifact, generatedOutput, documentVersion, memory | **SPLIT (review)** | 30 leaks; artifacts may be shared, space-binding is product |
| actions | candidate, action, surface | **CORE (review)** | agent action surface |
| forge | project, request, requestEvent | **CORE (review)** | approval/mentoring tooling (own MCP surface) |
| workbench | workspace | **CORE** | dev workbench (1 leak) |

---

## C. Core Go integrations (`memql/integrations/*`)

Coupling = # of Go files referencing `spaceId`/`copresent`/`knowledgeDomain`/`polyphon`.

| Integration | Purpose | Coupling | Classification |
|---|---|---|---|
| openai, openairealtime | LLM + realtime voice-to-voice | low | **CORE** |
| router | model routing | low | **CORE** |
| audio, stt, embedding, similarity | media + retrieval primitives | low | **CORE** |
| auth, identity, database, azureblob | infra | low | **CORE** |
| fileprocessor, email, timeutil | utilities | low | **CORE** |
| training, harnessrecall, harnesstrace | harness | low | **CORE** |
| agentdef, agents, agent | agent framework | agent 8/31 | **CORE** (partition cleanup) |
| actionsearch | action retrieval | 0/3 | **CORE** |
| workbench | dev workbench | 1/12 | **CORE** |
| **voice** | realtime voice agent (room↔Realtime bridge, polyphon) | **38/64** | **CORE** (heavy spaceId leak → partition; telephony attaches here) |
| **planner** | planning engine | 18/79 | **CORE** (partition cleanup) |
| liveknowledge | live retrieval | 0/3 | **SPLIT (review)** — clean but knowledge-flavored |
| **cognition** | conversation routing/handlers | **30/41** | **SPLIT** — engine core, product behaviors → CoPresent |
| **knowledge** | knowledge-domain ingestion/retrieval | **8/11** | **COPRESENT** |
| **dailyspace** | daily/scheduled spaces | **4/5** | **COPRESENT** (space-based) |
| **avatardirect** | direct video avatars | **3/3** | **COPRESENT** |
| **avatarvendor** | avatar vendor integration | 3/10 | **COPRESENT** |
| **chat** | chat surface | **2/2** | **COPRESENT** |
| library | artifacts/outputs | 2/3 | **SPLIT (review)** |

---

## D. Tools / MCP surface

| Tool group | Where it lives | Classification |
|---|---|---|
| `calendar`, `notes`, `todos`, `memql` tools | core `dsl/*/tools.memql` | **CORE (shared)** |
| `forge` tools (projects/approval/mentoring) | core `dsl/forge/tools.memql` | **CORE (review)** |
| `canvasPublish`, `claw*` (file/code), `worker*`, `workbenchHost`, `requestComputerUseScope` | BFF `dsl/copresent/tools.memql` | **COPRESENT** (already in pack) |
| `ui*` operator/teach tools (uiClick, uiNarrate, uiHighlight, uiWaitFor…) | BFF `dsl/copresent/operator_tools.memql` | **COPRESENT** (already in pack) |
| Telephony tools (planned: `place_call`, etc.) | new, core `integrations/telephony` | **CORE** (attach to partition, not space) |

---

## E. CoPresent plugin repo (`memql-bff-copresent`) today

| Piece | Content | Note |
|---|---|---|
| `dsl/copresent/concepts.memql` | canvasState, config, onboarding | Thin today — **target for the extracted CoPresent concepts** |
| `dsl/copresent/{tools,operator_tools}.memql` | canvas, claw, worker, ui* | Already CoPresent-correct |
| `component/bff/` (~585 LOC) | gRPC BFF service (server, adapters, ids, subscription) | Frontend-facing surface for the SPA |
| `component/grpc/gen/` (~2,390 LOC) | generated protobuf/gRPC | Derived from `.proto` |
| `main.go` | thin carrier; wraps core `app.Run`; `RegisterTree("copresent")` | "every line of meaningful behaviour belongs in memql" |

---

## F. The connective-tissue task (applies across B & C)

`space` is CoPresent, but `spaceId` is used as a scope key throughout CORE
(cognition engine, voice, planner, data, library, identity, agents, router,
platform). For `space` to leave core cleanly, core needs a generic
**`partitionId`** (tenant/scope) that those concepts reference instead, with
CoPresent's `space` mapping onto a partition. This is what makes the same core
engine run unchanged across the Anaquim, Visionaries/CoPresent, and CNATs
clusters — only the pack differs.

---

## G. Open classification questions (for your "different idea")

> **RESOLVED (3.1 sign-off, 2026-06-22)** in the authoritative move-list
> [`07-copresent-move-list.md`](07-copresent-move-list.md): library = SPLIT
> (spine core / space-facet pack); calendar/notes/todos = CORE; liveknowledge =
> CORE; forge = CORE (+ actions = CORE); unmetCapability/guardrailHealth = CORE
> telemetry. Knowledge and the whole agents framework are also CORE. The
> questions below are retained as the original first-pass framing.

1. **library** — are artifacts/generated-outputs a shared core capability, or
   a CoPresent product surface? (30 spaceId refs.)
2. **calendar / notes / todos** — keep as a shared core "assistant features"
   layer, or push to product packs?
3. **liveknowledge** — core live-retrieval primitive, or part of the CoPresent
   knowledge product?
4. **forge / actions** — core platform governance/action surface, or product?
5. **unmetCapability / guardrailHealth** — split raw-signal (core) from
   product rollup (CoPresent), or keep whole on one side?
