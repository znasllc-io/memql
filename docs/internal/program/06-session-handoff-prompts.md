---
title: Session Handoff Prompts
audience: internal
status: historical
area: internal
owner: znas
---

# Session Handoff Prompts

Copy-paste prompts to run the four epics as separate sessions with explicit
gates. **S1 starts now. S2/S3 start at G1. S4 starts at G3.** Gate definitions
and the parallel plan: [`00-master-plan.md`](00-master-plan.md).

> **Historical.** All four epics shipped (Epic 4 #1906 last). These prompts are
> kept as a record of how the program was sequenced.

> **How gates work between sessions.** A session that depends on a gate must
> not start its gated work until the producing session confirms the gate is
> open (merged + green). Each prompt states which gate it waits on and which it
> produces. Post gate-open status where your sessions coordinate (PR label,
> tracking issue, or shared channel).

---

## MASTER PROMPT (orchestrator / your reference)

```
We are running a 4-epic platformization program on the znasllc-io/memql
codebase (plus the product carrier repo, memql-cockpit, and the product frontend).
Authoritative plan: memql/docs/internal/program/00-master-plan.md, with one
file per epic (01–04), the current-state map (05), and these prompts (06).

Epics and order: (1) AI→AI rename → (2) platform/plugin → (3) decouple
the product → (4) telephony. Gates: G1 = rename landed; G2 = partition foundation
landed; G3 = core decoupled (engine-only build green).

Locked decisions: AI→AI rename runs first and includes wire/proto names in one
coordinated breaking sweep; `partition` is the canonical tenant scope (already
exists — do NOT invent one); services stay core, packs are product; telephony
attaches to a partition/room, never a the product `space`.

Run each epic as a separate session using the per-epic prompts in 06. Within an
epic, issues tagged [P] run in parallel; [G:x] waits on gate/issue x. Create
GitHub issues from each epic file, link dependencies, and gate cross-session
work as specified.
```

---

## SESSION S1 — Epic 1: SI → AI rename  (starts NOW · produces G1)

```
You own Epic 1 (AI→AI rename) of the memql platformization program. Read, in
order: memql/docs/internal/program/00-master-plan.md and 01-epic-si-to-ai-
rename.md. Work the repos: znasllc-io/memql, the product carrier repo, memql-cockpit,
and the product frontend.

Goal: rename "SI / synthetic intelligence" → "AI" across DSL, Go, wire/proto,
and frontend in ONE coordinated sweep, including the breaking proto names.
Centerpiece: the DSL construct si("prompt", args) → ai(...), and SIExpression →
AIExpression.

CRITICAL denylist — never rename: SIP* (SIP telephony), TSInterface*/TSImport*/
CSS*/CJS* (TS/CSS AST), and English words (POSIX, VERSION, MISSING, INSIDE,
OUTSIDE, ANALYSIS, DECISIONS, SILENTLY, PERSISTS, EPSILON, UNSIGNALED, SID).

Steps: create GitHub issues 1.1–1.7 from the epic file with their dependencies.
Build the curated allowlist/denylist renamer first (1.1, dry-run + diff). Then
core DSL keyword (1.2) and internal Go (1.3) in parallel; proto rename +
regenerate (1.4) which gates BFF (1.5) and frontend (1.6); finish with docs +
full verification (1.7). Open a PR per issue, keep builds green at each step.

When 1.7 passes — all four repos build + test green and a repo-wide scan shows
zero AI-as-synthetic-intelligence identifiers outside the denylist — announce
G1 OPEN. Sessions S2 and S3 are waiting on it.
```

---

## SESSION S2 — Epic 2: Platform / plugin  (waits for G1 · produces G2)

```
You own Epic 2 (platform/plugin architecture) of the memql program. DO NOT START
until G1 is open (Epic 1 rename fully landed). Read: memql/docs/internal/program/
00-master-plan.md and 02-epic-platform-plugin.md. Repo: znasllc-io/memql.

Goal: formalize the existing plugin contract (RegisterPlugin + PluginContext +
RegisterTree + routing rules) as a versioned Plugin SDK, and adopt the EXISTING
`partition` primitive (v1:platform:partition*, ResolvePartitionFromContext) as
the canonical tenant scope. Do NOT invent a new partition concept. Do NOT
implement runtime (non-compiled) pack loading — packs stay embedded via build
tags, like the product carrier repo.

Steps: create issues 2.1–2.5. Land 2.1 (contract/version) and 2.2 (partition
adoption + mapping docs + new-spaceId lint) FIRST — when both merge, announce
G2 OPEN (Session S3's core re-pointing depends on it). Then 2.3 (extension-point
audit for cognition/voice/planner) [P] 2.4 (pack model/validation), then 2.5
(reference pack + developer guide). You may run concurrently with S3's prep.
```

---

## SESSION S3 — Epic 3: Decouple the product  (prep at G1, core work at G2 · produces G3)

```
You own Epic 3 (decouple the product from core) of the memql program. PREP may
start once G1 is open; the core re-pointing (3.2) must wait for G2. Read:
memql/docs/internal/program/00-master-plan.md, 03-epic-decouple-the product.md,
and 05-current-state-map.md. Repos: znasllc-io/memql (extract from),
the product carrier repo (move into).

Goal: move ALL the product concepts/logic/integrations out of core into the
the product pack, and re-point core services off spaceId onto the generic
`partition`. End state: core service nodes build engine-only (zero the product
refs), like the mcp node does today.

Steps: create issues 3.1–3.7. After G1: freeze the move-list (3.1) and start the
pure moves that don't need partition — knowledge/guide/curriculum DSL (3.4),
the product Go integrations avatar*/dailyspace/chat/knowledge (3.5), split-cases
(3.6) — in parallel. After G2: re-point core spaceId→partitionId (3.2), then
split cognition extracting space/micState/privateUtterance/misrouteFeedback/
greetSuppression to the pack while keeping the engine concepts (3.3).

Finish with 3.7: build core with NO the product pack and confirm zero the product
symbols, then build a the product cluster (core + pack) and pass an end-to-end
space + knowledge flow. When green, announce G3 OPEN. Session S4 is waiting.
```

---

## SESSION S4 — Epic 4: Telephony  (waits for G3)

```
You own Epic 4 (telephony into core) of the memql program. DO NOT START until G3
is open (core decoupled, engine-only build green). Read: memql/docs/internal/
program/00-master-plan.md, 04-epic-telephony.md, and the shipped operator docs
at memql/docs/public/operate/telephony.md. Repo: znasllc-io/memql.

Goal: inbound + outbound PSTN calling via self-hosted livekit/sip + a carrier-
agnostic CarrierProvider abstraction (Telnyx first), driven by the OpenAI
Realtime voice path. No Twilio.

Two amendments vs the original drafts: (A) attach calls to a generic
partition/room, NEVER a the product `space` — v1:telephony:* carry partitionId;
inbound DID→partition routing; outbound place_call to a partition-scoped room.
(B) use post-rename AI vocabulary (ai(), *AIProvider) — no si-named identifiers.

Steps: create the 8 telephony issues with Amendments A/B applied. Start with a
thin M0 spike (livekit/sip locally + one hand-bought Telnyx DID, prove one
inbound and one outbound call), then build the CarrierProvider + concepts +
inbound + outbound + provisioning + cost controls + compliance. Honor the cost
guardrails: caching on, VAD-gate silence, trim long-call context.
```
