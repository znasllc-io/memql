---
title: Tool ↔ Knowledge Domain Pattern
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Tool ↔ Knowledge Domain Pattern

**Status:** Established. Pattern shipped 2026-04-x with the product
pack's operator-UI domain; extended 2026-05-08 to `computer-use`;
further extended 2026-05-17 to the `workbench` domain (the sandboxed
first-choice headless surface).

## Problem

Capabilities (operator UI control, Computer Use, the sandboxed
Workbench, and whatever ships next) all carry sizeable
operational knowledge: which tool surfaces exist, when to reach for
each, how the per-task approval gate works, how to recover from
failure modes, what to never do.

The naive place to put that knowledge is the agent's prompt template
(the `agentReply` prompt's `.tmpl`, delivered in the product's runtime
DSL bundle via `MEMQL_DSL_PATH`). That doesn't scale:

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

Four places to touch when adding the pattern for a new
capability `<cap>`:

1. **Knowledge domain catalog**
   `dsl/knowledge/catalog/automations.memql` — append a domain
   entry with `RequiredByToolSlugs: ["<cap>"]`. Same
   `RelevantForRoles` shape as the existing entries. Mark
   `Category: "internal"` for capability-knowledge domains
   (vs. `"product"` / `"business"` etc. for subject-matter
   domains).

2. **Seed corpus**
   Add `SourceRef` + `Text` entries to the shipped DSL catalog and expose
   the corpus through `integrations/knowledge/seed.go`, like
   `computerUseSeedCorpus`. Keep entries short; larger entries are split
   by the normal ingestion chunker:

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
   `seedStandardDomainsHandler` and the exact-content registry in
   `canonicalSeedChunks`. Pack-registered corpora enter both automatically.
   Registration describes content; installation still requires ingestion as
   described under Publication and review below.

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
   `operatorEnabled`). Mirror what the `computer-use` domain does
   for `computerUseStatus`. (Operator UI control is an engine-generic
   capability now: its domains are engine-owned and force-attached the
   same way whenever `operatorEnabled` is truthy. A product that ships
   its own operator domains delivers them through its runtime DSL
   bundle, not a compile-time `AppProfile` registration.)

   Also add the new domain to `appStructureDomainIds` near the
   bottom of the file — that registry tells the citation pipeline
   to treat the chunks as operator/internal documentation (not
   audibly cited as "your X training" in agent replies).
   These are engine-owned domains; operator UI domains classify the
   same way now that operator control is an engine-generic capability.

4. **Strip prompt template**
   the `agentReply` prompt template (in the product's runtime DSL
   bundle) — keep ONLY the per-turn-dynamic
   capability block:

   - The capability gate (`{{if .computerUseStatus}}` etc.)
   - One short sentence pointing the agent at its `<cap>` knowledge
     domain.
   - Any state field that genuinely changes per turn (CONNECTED /
     DISCONNECTED and the current operation's approval state).

   Move EVERYTHING else (tool-surface descriptions, scope tiers,
   approval flows, failure modes, things-you-must-never-do) into
   the seed corpus. The template gets lighter; the knowledge gets
   retrievable.

### What stays in the template

The template is for **per-turn dynamic state** the agent can't get
from RAG:

- Capability availability (the `{{if .computerUseStatus}}` gate
  itself).
- Current state values (CONNECTED / DISCONNECTED and the durable run's
  current permission and execution state).
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
| operator UI control (engine-generic) | operator-UI domain(s) (engine-owned) | engine-owned seed | `operatorEnabled` truthy |
| `computer-use-*` (split)     | `computer-use`   | `computerUseSeedCorpus`       | `computerUseStatus` non-empty           |
| `workbench-use`              | `workbench`      | `workbenchSeedCorpus`         | `workbenchAvailable` truthy (i.e. agent's expanded tool list carries `workbenchHost`) |

## Why it works

- **Template stays agnostic.** New capabilities don't bloat the
  template. The same template renders for every agent + every turn.
- **Knowledge stays curatable.** The registered corpus supplies stable
  chunk IDs from domain + sourceRef + sequence + text. Reinstalling the
  same content is idempotent; changed content gets a new ID. Earlier
  approved revisions remain until an explicit review or supersession
  decision, as described under Publication and review below.
- **RAG does the right work.** Only chunks relevant to the user's
  current message land in the prompt. A "what's the weather?" turn
  doesn't pay for `computer-use` / `workbench` knowledge tokens; an "open Mail
  and ..." turn does.
- **Per-agent overrides have a clean home.** Training pipeline
  attaches private domains; the standard seed isn't touched.
- **Decoupled lifecycle.** Knowledge can be attached without the
  capability (research agent talks about Computer Use), capability
  cannot be granted without the knowledge (force-include guarantees
  the agent always has the manual when it needs it).

## When NOT to use this pattern

- **Truly per-turn data** — current cluster topology, current
  user's preferences, the plan this turn belongs to. Those are
  template data fields, not knowledge.
- **Capabilities that have no operational knowledge worth
  retrieving** — a one-line tool whose description in the tool
  registry suffices. The pattern earns its keep when the capability
  has 5+ paragraphs of operational nuance to teach.
- **Subject-matter knowledge** — accounting, HR, legal, etc. Those
  are user-attached knowledge domains via the Knowledge panel, not
  capability-bundled internal documentation.


## Publication and review

Agent retrieval uses only the latest version of each document chunk whose
`validationStatus` is `validated` and whose `superseded` flag is not true.
Pending, missing-status, rejected and superseded chunks remain visible to
Training review reads but cannot supply retrieved agent context or the Trainer's
refresh prompt. The filter runs before the similarity result limit, so pending
matches do not displace approved material. A review on another replica takes
effect on the next retrieval; cached query embeddings do not cache approval.

General ingestion, including file uploads and caller-supplied `appStructure`
labels, does not approve content. The existing Trainer writer explicitly stamps
validated output. The server-selected canonical corpus writer may publish only
exact registered content, and preserves existing rejection and supersession.

### Versioned release knowledge

`dsl/knowledge/training/manifest.json` explicitly selects the reviewed
computer-use, workbench and work-guidance manuals, two software-development
lessons and a research-methodology lesson. The original lesson files, their
hashes and the revised literal-data lesson are versioned beside the manifest.
Each selected source has a stable
source reference and a SHA-256 content revision. Startup refuses an inconsistent
manifest rather than silently approving altered bytes. Adding a catalog entry
or a pack does not automatically add it to this release selection.

Each node publishes this exact selection in one bounded startup pass, before an
owner or embedding service is required. These are shared application-provided
chunks, not fabricated owner uploads. They appear in Training's existing domain
and review views, including unvalidated, rejected and superseded revisions.
Their content identity and source reference retain provenance; no full domain
catalog or generated prose is created. Existing user uploads remain separate.
The legacy repair pass still repairs only exact existing registered chunks and
bare vector keys.

The installed `reconcileReleaseKnowledge` schedule retries every two minutes on
BFF. Its two-minute pass publishes first, then attempts at most eight missing
embeddings in the active embedding space. Independent source locks and fresh
vector reads prevent concurrent replicas or an explicit capability installation
from duplicating completed embedding work. Failed provider/storage calls remain
retryable and may require another embedding; this is not an exactly-once billing
guarantee after an uncertain external outcome. Missing configuration or an
unavailable embedder is reported as deferred indexing. Ownerless scheduled work
does not manufacture a user or a forwarding assertion to reach private fleet
models; those bindings await an authorized owned turn.

Optional Training similarity lookups record `embedding-timeout` when their own
short deadline expires. The Training recipe may reuse that availability receipt
for five minutes, recording `embedding-backoff` and the original retry time.
This is unavailable context, not evidence that no lessons match. Repeated reads
do not extend the delay. Each lookup rechecks the owned run, current source heads,
review/publication state and embedding binding; a changed query, source or binding
permits a fresh attempt. Successful vectors continue to use the existing shared
embedding cache, while selected passages always undergo current authorization
and source validation. Oversized source inventories disable availability reuse.

Before an owned turn retrieves manuals and optional lessons, its Training recipe
rechecks the current run and goal and indexes at most eight missing release
vectors in one two-minute pass. Only exact, validated, non-superseded manifest
chunks qualify. Inference retains the caller’s identity, accepted forwarding
authority, run budget and cancellation across the serving-node hop. Publication
remains a separate server operation; a binding grants no new machine access.
Missing binding or unavailable selection defers indexing, while cancellation,
ceilings, storage errors and failed model calls remain failures. The same source
locks prevent overlapping first-use, scheduled and explicit-install passes from
duplicating completed work. Reviewable content alone
does not mean a lesson is indexed or has been retrieved.

Unchanged revisions reuse rows and vectors. New revisions retain old audit rows,
but ordinary retrieval selects only exact current manifest bytes for these
release sources. Old revisions remain visible in Training. A rejected
predecessor withholds automatic approval of a new revision. Superseded content
with no accepted replacement also withholds approval; it cannot reappear simply
because a release changes its text. A withheld revision remains pending for an
explicit Training review. An already accepted current replacement remains
usable when an older revision was explicitly superseded. Decisions apply to
source/chunk lineage, not the whole domain. Existing current rejection and
supersession are never reset. Concurrent writes are fenced against the current
chunk version.

### Explicit capability corpus installation

A cluster owner can install selected shipped capability manuals through the
ordinary authenticated query surface:

```memql fragment
builtin knowledgeInstallCapabilityCorpora(domainIds: ["computer-use", "workbench", "work-guidance"])
```

This explicit operation creates only registered document chunks and their
vectors in the cluster's active embedding space. It does not create knowledge
domain catalog rows, generate prose or grant tools. The release lifecycle above
selects its own explicit manifest independently of this owner operation. Pack corpora
qualify when registered with source `appStructure`, a nonempty
`RequiredByToolSlugs`, and content. The caller supplies domain IDs only; uploads,
source labels and caller text cannot become canonical publication authority.
Work-run invocations are refused, even with inherited owner credentials.

The compiled installation recipe stops on the first publication or embedding
failure. Repeating the same selection on any replica skips existing vectors
and repairs missing ones, without changing recorded rejection or supersession.
Requests accept 1–16 distinct domains and at most 256 registered chunks, with a
five-minute execution bound. The three domains above currently contain 25
chunks: at most 25 missing-vector embedding calls, with no generative calls.

The result reports `domains`, with `domainId`, `chunks`, `ready`, `preserved`,
`embedded` and `olderPublished` for each selected domain, plus the embedding
`provider`. `ready` counts currently validated, nonsuperseded registered chunks
with vectors; `preserved` counts existing review states that prevent their
publication. `embedded` counts vectors written by this invocation. Errors remain
errors, so an incomplete invocation never reports full readiness.

`olderPublished` counts other still-approved chunks for the same registered
source references. They retain their content and review history. Current release-manifest sources
are restricted to their current exact revisions during retrieval; unselected pack
corpora keep their existing retrieval semantics. The count is an audit fact, not
a claim that every older row remains retrievable. Installation does not rewrite
older review or supersession decisions.

## Owned task lessons

Owned work turns can also retrieve reviewed lessons taught from the owner's
Library files and the explicit release-owned lessons, without adding a
specialist agent or assigning the lesson's
domain to an agent. The authored `agentWorkTrainingContext` recipe searches by
the current section and overall goal. It selects at most three passages with
cosine similarity of at least 0.60, within a conservative 1,200-token context
budget. Existing required tool domains remain separate and unchanged.

Research source retrieval, evidence ledgers, research-to-drafting checks and
extraction alternatives live in the versioned `training:research-evidence`
lesson under `research-methodology`. The shared work prompt retains general
truthfulness, evidence, authority and completion constraints, without imposing
research procedures on every task. The same section-and-goal retrieval recipe
selects this lesson; it is not attached by a task-name classifier. Research
sections still carry their authored evidence-output contracts. Semantic
selection is bounded and does not guarantee inclusion on every research turn.

The native reader derives the owner, run and goal from the execution context.
It folds current rows before ranking, then rechecks each candidate's approval,
supersession, owned Library artifact and ready file, taught domain and exact
source text. Unknown provenance, other owners' sources and changed sources do
not qualify. A review change on another replica takes effect on the next turn;
query-embedding caches do not cache publication decisions. If there are no
eligible lessons, this path makes no embedding request.

Release lessons use a separate provenance branch that admits only exact current
manifest chunks; they do not borrow Library ownership. Both sources share the
same relevance, count and context limits. Exact duplicate text occupies one slot,
with valid owned provenance preferred on equal scores. Similar but different
lessons remain independent. Returned passages include their domain, chunk,
source, approved chunk version time and similarity; release passages also carry
the source revision, while uploaded passages retain their Library identities. Only domains actually returned are added to the turn's recorded
knowledge footprint. The prompt labels the passages as reference material;
they grant no tools or permissions and do not replace the current instructions.
This is retrieval of reviewed knowledge, not model training or automatic
publication of completed work. User-authored lessons must first be saved to the Library, taught into a domain
and approved through Training review. Release-owned lessons follow the reviewed
manifest publication above. Neither path guarantees a semantic match or forces
prompt inclusion.
