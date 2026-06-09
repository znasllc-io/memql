---
title: Tool ↔ Knowledge Domain Pattern
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Tool ↔ Knowledge Domain Pattern

**Status:** Established. Pattern shipped 2026-04-x with `copresent_ui`;
extended 2026-05-08 to `computer_use`; further extended 2026-05-17
to the `workbench` domain (the sandboxed first-choice headless
surface).

## Problem

Capabilities (CoPresent Control, Computer Use, future skills like
voice-pipeline orchestration, claw, etc.) all carry sizeable
operational knowledge: which tool surfaces exist, when to reach for
each, how the per-task approval gate works, how to recover from
failure modes, what to never do.

The naive place to put that knowledge is the agent's prompt template
(`prompts/v1/agent/agentReply.tmpl`). That doesn't scale:

- Templates become long and capability-laden — every new skill adds
  a section, every operational nuance is template prose.
- The template is rendered every turn for every agent regardless of
  whether the agent has the capability or whether the user's current
  request is even related to it.
- Per-agent customization (good/bad examples curated by an
  operator) ends up either hardcoded in the template or impossible
  to inject cleanly.
- The same prose is also expensive to keep agnostic — temptation to
  drop concrete examples ("open Safari", "list Downloads") creeps
  in; those leak agent-specific assumptions into the global
  template.

## Pattern

For every capability that ships meaningful operational knowledge,
add a **knowledge domain** that the capability auto-attaches when
the agent picks the tool.

```
Tool   --requires-->   KnowledgeDomain
```

The arrow goes one way:

- **Tool requires domain** — picking the capability flag implies
  the domain is attached (no separate step in the create-agent
  flow).
- **Domain does NOT require tool** — an agent can attach the
  domain WITHOUT holding the capability. A research / training /
  documentation agent might want to *talk* about Computer Use
  without being able to drive it themselves.

### Wiring

Three places to touch when adding the pattern for a new
capability `<cap>`:

1. **Knowledge domain catalog**
   `integrations/knowledge/seed.go` — append a `StandardDomain`
   entry with `RequiredByToolSlugs: []string{"<cap>"}`. Same
   `RelevantForRoles` shape as the existing entries. Mark
   `Category: "internal"` for capability-knowledge domains
   (vs. `"product"` / `"business"` etc. for subject-matter
   domains).

2. **Seed corpus**
   `integrations/knowledge/seed.go` — declare a
   `<cap>SeedCorpus []struct{SourceRef, Text string}` near the
   existing `copresentUISeedCorpus`. Each entry is one chunk; rules:

   - Lead with the topic anchor in the first sentence — it's what
     the embedding picks up most strongly.
   - One concept per chunk. Multiple sub-topics get split into
     separate `SourceRef`s.
   - Keep chunks under ~2 KB. RAG ranks chunks individually; fat
     chunks dilute relevance.
   - **No agent-specific or task-specific examples.** Pattern
     shape only. If a specific agent needs curated examples,
     those come through per-agent training, not the standard
     seed.
   - Tool-name references stay verbatim
     (`workerHost`, `workerComputer`, `requestComputerUseScope`).

   Wire the corpus into the ingestion loop in
   `seedStandardDomainsHandler` — extend the existing
   `ingestCorpus(...)` helper call list to cover the new domain.

3. **Auto-attach in the retrieval domain set**
   `integrations/agent/replier.go` — in the block that builds
   `domains` before RAG retrieval, add a force-include when the
   per-turn signal indicates the agent has the capability:

   ```go
   if cap, _ := data["<capabilityFlag>"].(string); cap != "" {
       domains = ensureDomain(domains, "<cap>")
   }
   ```

   The `<capabilityFlag>` is the per-turn template-data key the
   capability already injects (e.g. `computerUseStatus`,
   `operatorEnabled`). Mirror what `copresent_ui` does for
   `operatorEnabled`.

   Also add the new domain to `appStructureDomainIds` near the
   bottom of the file — that registry tells the citation pipeline
   to treat the chunks as operator/internal documentation (not
   audibly cited as "your X training" in agent replies).

4. **Strip prompt template**
   `prompts/v1/agent/agentReply.tmpl` — keep ONLY the
   per-turn-dynamic capability block:

   - The capability gate (`{{if .computerUseStatus}}` etc.)
   - One short sentence pointing the agent at its `<cap>` knowledge
     domain.
   - Any state field that genuinely changes per turn (CONNECTED /
     DISCONNECTED / scope value, planApprovedTrigger flag).

   Move EVERYTHING else (tool-surface descriptions, scope tiers,
   approval flows, failure modes, things-you-must-never-do) into
   the seed corpus. The template gets lighter; the knowledge gets
   retrievable.

### What stays in the template

The template is for **per-turn dynamic state** the agent can't get
from RAG:

- Capability availability (the `{{if .computerUseStatus}}` gate
  itself).
- Current state values (CONNECTED / DISCONNECTED, current scope,
  whether THIS turn is a post-approval execution dispatch via
  `planApprovedTrigger`).
- Pointers ("consult your `<cap>` knowledge domain") to the
  retrievable knowledge.

### What goes into the domain

The domain is for **general operational knowledge** that's the same
for every turn and every agent that has the capability:

- What the capability is, what tool surfaces it fans out into.
- When to reach for each surface.
- Scope tiers and how they map to surfaces.
- The approval flow (request → user clicks Allow → re-dispatched
  turn).
- Post-approval execution semantics (what to do, what NOT to do).
- Plan-outcome semantics (how the planner decides succeeded vs
  failed).
- Failure-recovery patterns and never-do lists.

## Per-agent customization

The standard seed is agnostic. When a specific agent needs curated
examples (good cases / bad cases the operator wants to reinforce),
those land via per-agent training:

- Operator opens **Training** panel → drops the agent into the
  Studio → adds a private knowledge domain with the curated
  examples → Train.
- That private domain is attached to the agent record's
  `capabilities.domains`; RAG retrieval picks chunks from it
  alongside the standard `<cap>` chunks.

**Don't put examples in the standard seed.** The standard seed is
for every agent that ever holds the capability. Examples belong in
training, where they can be tuned per agent and per workspace.

## Concrete instances

| Capability slug              | Domain id        | Seed corpus var               | Auto-attach signal in replier.go      |
|------------------------------|------------------|-------------------------------|----------------------------------------|
| `copresent_control`          | `copresent_ui`   | `copresentUISeedCorpus`       | `operatorEnabled` truthy                |
| `computer_use_*` (split)     | `computer_use`   | `computerUseSeedCorpus`       | `computerUseStatus` non-empty           |
| `workbench_use`              | `workbench`      | `workbenchSeedCorpus`         | `workbenchAvailable` truthy (i.e. agent's expanded tool list carries `workbenchHost`) |

## Why it works

- **Template stays agnostic.** New capabilities don't bloat the
  template. The same template renders for every agent + every turn.
- **Knowledge stays curatable.** Edits land in a `.go` slice
  literal, embed re-runs idempotently (chunk id is a sha256 of
  domain + sourceRef + seq + text — same text, same id; new text,
  new id, old version purged on next re-ingest).
- **RAG does the right work.** Only chunks relevant to the user's
  current message land in the prompt. A "what's the weather?" turn
  doesn't pay for `computer_use` / `workbench` knowledge tokens; an "open Mail
  and ..." turn does.
- **Per-agent overrides have a clean home.** Training pipeline
  attaches private domains; the standard seed isn't touched.
- **Decoupled lifecycle.** Knowledge can be attached without the
  capability (research agent talks about Computer Use), capability
  cannot be granted without the knowledge (force-include guarantees
  the agent always has the manual when it needs it).

## When NOT to use this pattern

- **Truly per-turn data** — current cluster topology, current
  user's preferences, the active spaceId. Those are template
  data fields, not knowledge.
- **Capabilities that have no operational knowledge worth
  retrieving** — a one-line tool whose description in the tool
  registry suffices. The pattern earns its keep when the capability
  has 5+ paragraphs of operational nuance to teach.
- **Subject-matter knowledge** — accounting, HR, legal, etc. Those
  are user-attached knowledge domains via the Knowledge panel, not
  capability-bundled internal documentation.
