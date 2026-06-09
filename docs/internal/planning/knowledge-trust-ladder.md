---
title: Knowledge Trust Ladder + Validation UX
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Knowledge Trust Ladder + Validation UX

Status: planning. Branch: `feature/knowledge-trust-ladder`. Five
architectural decisions locked in the originating brainstorm; this
doc is the single source of truth for the implementation.

---

## Goal

Extend the existing knowledge surface (knowledge domains + document
chunks + live knowledge dispatcher) with:

1. An explicit **4-tier trust ladder** the agent retrieval path honors.
2. A **validation UX** that lets users sign off on knowledge with
   workload proportional to how much they actually want to validate.
3. A **reframe of Live Knowledge** from "agent-facing data surface" to
   "integration broker" -- external data flows through Live Knowledge
   into native concept rows; agents read native rows uniformly.

Most of the schema scaffolding already exists in the tree
(`documentChunk.validationStatus`, `validationEvent` audit log,
`source` enum on `documentChunk`, `liveSource` + `liveConnector`
registry, `v1:knowledge:document` container). This initiative
connects the pieces and adds the missing fields.

---

## The trust ladder

| Tier | What it is | Maps to |
|------|------------|---------|
| T0 | LLM pretraining; no graph rows | nothing in the graph |
| T1 | Seeded chunks, unreviewed | `documentChunk` with `source ∈ {llmSeeded, crossDomainBridge, augment}` and `validationStatus = unreviewed` |
| T2 | User-validated chunks | `validationStatus = validated`; recorded on the existing `validationEvent` audit row |
| T3 | Live data, fetched at query time | `liveSource` query results; trust comes from the confirmed binding contract, not per-row review |

T0 has no concept rows. T1/T2 are chunks in the graph. T3 is on-demand.
Agents see T1/T2 vs T3 with different prompt-level treatment per Q1.

---

## Locked decisions

### Q1 -- T1/T2 retrieval and T3 fetch are SEPARATE prompt blocks

Agents receive trained-knowledge chunks (T1/T2) and live-data results
(T3) as distinct context blocks, not merged into one ranked stream.

Why:

- **Different epistemic status.** Chunks are "what we have learned"
  (possibly stale background); live results are "what is true right
  now" (system of record). When they disagree the agent should reason
  about the conflict, not have one silently outrank the other.
- **Different scoring spaces.** Cosine similarity on embeddings does
  not compare meaningfully to a deterministic SQL/REST match.
- **Different cost and intent.** T3 calls hit external systems,
  sometimes paid. The agent decides whether to consult them based on
  the question; they do not flow into every similarity search.
- **Provenance lines up cleanly.** Trained-chunk citation chips
  (`{domainId, matchedPhrase}` on the existing reply envelope) stay
  as-is. Live-data citations get a distinct "from Source X, fetched
  Ns ago" badge.

Implementation: the prompt-context builder emits two named sections
(`trainedKnowledge` + `liveData`), each with its own citation
provenance. The agent reply envelope grows a second citation list for
live data (or a tagged variant on the existing list).

### Q2 -- Trust threshold per (agent, domain) attach, with per-domain floor

Two knobs:

- **Per-attach `trustThreshold`** on the agent-domain attach record.
  Default: T1. An agent attached to a high-stakes domain can be
  configured to read only T2+ from it.
- **Per-domain `minTrustLevel`** on `v1:common:knowledgeDomain`.
  Default: T1. The domain owner stamps a floor (e.g. "no agent may
  quote T1 from this HR domain"). Enforced regardless of any attach
  config -- `max(attachThreshold, domainFloor)` wins.

Cold-start gotcha: when the threshold filter returns zero chunks, the
agent prompt directive (`agentReply.tmpl`) must instruct "say you do
not have validated information on that" rather than fall back to T0
(model prior). Confident-wrong is worse than "I do not know."

### Q3 -- Validation granularity

Document-level validation is the only **proactive** validation
surface. Per-chunk validation exists in the data model (the
`cascadeValidationToItems` automation already propagates Document
signoff to chunks) but is not exposed as a primary UI affordance.

The escape valve is **spot-rejection from a citation chip in chat**:
when an agent cites a passage and the user spots an error, a
thumbs-down on the citation chip fires a mutation that:

- drops the chunk's `validationStatus` to T1,
- inserts a `validationEvent` row capturing the user's reason,
- emits a graph event so the agent's next turn re-fetches without
  that chunk.

The spreadsheet per-row drawer pattern stays as-is for tabular data
(rows are meaningful in isolation). Free-text chunks do NOT copy that
pattern -- a 1800-char chunk read out of context is not a reviewable
unit.

### Q4 -- v1 Live Knowledge connectors + architectural reframe

**Reframe.** Live Knowledge is no longer the agent-facing data
surface. It is the integration broker between external systems and
native concept rows. Agents read native concepts via tools; sync
automations translate external data through Live Knowledge into
native rows.

```
External system (Postgres, web URL, ...)
       <->
   Live Knowledge (translation / dispatch layer)
       <->
   Native concepts (Calendar, Contacts, ...)  <- agent reads here
       |
   Agent tools (calendar.*, contacts.*, ...)
```

**v1 external connectors:**

- **web fetch** -- HTTP GET with TTL cache. Universal, no per-org
  config. Wraps the existing `rest` connector kind with a fixed-shape
  named query ("fetch this URL, return text").
- **Postgres** -- the `postgres` connector kind slot is declared
  today; needs implementation. Highest enterprise leverage -- one
  connector unlocks the long tail of org-internal databases.

**Not in v1:** Google Calendar (the Calendar Initiative builds the
native concept directly instead), Slack, Notion, Gmail, Drive. Each
is its own quarter of integration work; pick them up when specific
user demand surfaces.

The `memql` connector (queries against the local engine itself)
ships today and stays. Agents can ask graph questions in T3 mode via
the broker without RAG.

### Q9 -- T3 binding verification

T3 is "always validated" because the *binding* contract promises
authoritative freshness. The binding itself must be confirmed:

1. **Create-then-confirm.** A new `liveSource` row starts in
   `status = draft`. Agents cannot read from draft sources.
2. **Test query at confirmation time.** An admin+ opens a
   confirmation panel, runs the source's primary named query, looks
   at the rows/response that come back, and clicks **Confirm**.
3. **Persistent fields:** `confirmedBy`, `confirmedAt`, `configHash`
   (sha256 of binding fields + named-query body).
4. **Config-change voids signoff.** Any mutation touching the binding
   recomputes `configHash`; a mismatch flips the row back to draft
   and emits a `validationEvent` for the invalidation reason.
5. **Audit via existing `validationEvent`.** One audit table covers
   chunk validation, Document signoff, liveSource confirmation, and
   liveSource invalidation. Single retention/query/UI surface for the
   whole knowledge-trust audit.

Only `owner` and `admin` roles can confirm. Writers can draft
sources but not promote them.

---

## Schema deltas

### `v1:common:knowledgeDomain`

Add:
- `minTrustLevel` -- enum {`t1`, `t2`, `t3`}. Default `t1`. The
  domain owner's floor.

### Agent-domain attach record

Currently keyed by `(roleSlug, sortedDomainIds)` per
`ensureKnowledgeBridge`. Add:
- `trustThreshold` -- enum {`t1`, `t2`, `t3`}. Default `t1`. The
  effective threshold at retrieval time is `max(trustThreshold,
  domain.minTrustLevel)`.

### `v1:knowledge:liveSource`

Add:
- `status` -- enum {`draft`, `confirmed`}. Default `draft`.
- `confirmedBy` -- user id, nullable.
- `confirmedAt` -- timestamp, nullable.
- `configHash` -- sha256 over binding fields + named-query body.

### `v1:knowledge:validationEvent`

Extend `subject` enum (or string set) to include:
- `liveSource.confirmed`
- `liveSource.invalidated`

Schema-compatible since `subject` is already a free-form string.

### `v1:common:documentChunk`

No schema change. The existing `validationStatus` field is what
spot-rejection writes to.

---

## Retrieval pipeline changes

1. **Trust filter at chunk lookup.** The cosine-similarity query in
   `integrations/similarity` takes the agent's effective threshold
   (`max(trustThreshold, domainFloor)`) as a filter on
   `documentChunk.validationStatus`. Only rows at or above the
   threshold are returned. Source enum (`source ∈ {...}`) is unused
   for trust filtering -- the tier is determined entirely by
   `validationStatus`.
2. **Separate T3 context block.** When the agent (or the prompt
   author) wants live data, a tool call to
   `integration.liveknowledge.query` returns results that land in a
   `liveData` block in the prompt, separate from `trainedKnowledge`.
3. **Cold-start directive in `agentReply.tmpl`.** Instructs the
   agent to say "I do not have validated information on that" when
   the filter returns empty, rather than fall back to T0 silently.

---

## UX surfaces

UI work lives on the CoPresent side (separate brainstorm when we get
there). For planning purposes:

1. **Citation spot-rejection chip.** Thumbs-down on the citation chip
   in chat fires a mutation that drops chunk validation and inserts a
   `validationEvent` row.
2. **Live source confirmation panel.** Two-step ceremony: create
   row (draft) -> run test query -> render response -> Confirm
   (admin+ only). Re-render on any binding edit until reconfirmed.
3. **Rejected-chunk review** (follow-on; not blocking v1): a list on
   the Document detail panel showing chunks that have been
   spot-rejected, with options to re-validate or remove.

---

## Out of scope (v1)

- Bidirectional sync for any external source -- v1 is one-way pull
  only; bidirectional is a separate phase.
- Slack / Notion / Gmail / Drive / Calendar SaaS connectors.
- Per-chunk proactive validation UI -- data model supports it, but
  v1 surfaces only Document-level + spot-rejection.
- Trust-threshold editing UI on the agent edit surface -- v1 ships
  schema fields and sensible defaults; the editing UI is follow-on.

---

## Open follow-ons

Real questions but downstream of v1 ship:

- **Test-query result rendering.** The confirmation panel needs a UI
  surface to render rows (SQL/memql kinds) and bodies (REST/GraphQL).
  Small CoPresent-side component.
- **Spot-rejection volume threshold.** A Document with many
  spot-rejections should at some point auto-de-validate. What is
  "many"? Defer until we see real data.
- **Trust-level visualization in the agent reply.** Should the user
  see "this answer used 3 T2 chunks + 1 T3 live source"? Probably
  yes; ship without it first and add when value is clear.

---

## Implementation phases

Rough sequence; not committed to specific commits yet.

1. **Schema.** Add new fields on `knowledgeDomain`, the agent-domain
   attach concept, and `liveSource`. Extend `validationEvent`
   subjects. Concept files + struct-form mutations to set the new
   fields.
2. **Retrieval filter.** Plumb the effective threshold through the
   `similarity` lookup path; pass it from the agent's prompt-context
   builder (`integrations/agent/prompt_data.go` and callers).
3. **Separate prompt blocks.** Update `agentReply.tmpl` and
   prompt-data builders to emit `trainedKnowledge` and `liveData`
   sections separately. Update the reply envelope's citation shape if
   live citations need their own tagged variant.
4. **liveSource confirmation.** Mutations
   (`mutationConfirmLiveSource`, `mutationInvalidateLiveSource`),
   the `configHash` recompute on edit, and the agent-read filter that
   excludes `status != confirmed` sources.
5. **Spot-rejection.** `mutationRejectChunk` + `validationEvent`
   insert. Frontend chip is a CoPresent ticket.
6. **Web fetch + Postgres connectors.** Self-registering plug-ins
   under `integrations/liveknowledge/connectors/` (or whatever the
   connector-kind layout looks like at that point).
7. **Cold-start prompt directive.** Update `agentReply.tmpl`; verify
   with an end-to-end smoke test.
8. **End-to-end smoke through CoPresent.** Verify retrieval +
   spot-rejection + binding confirmation paths.
