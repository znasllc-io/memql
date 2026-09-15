# Research briefs from your Files

A complete edition-2026 domain that turns a question into a saved research draft:

1. A mutation writes a `researchBrief` request.
2. Its create event starts `prepareResearchBrief`.
3. `librarySimilarArtifacts` finds up to five matching files from the caller's indexed Library (the **Files** app).
4. A configured agent receives the question, titles, snippets, and artifact IDs.
5. A mutation saves its reply and sources under the same brief ID.
6. `researchBriefs` reads the caller's drafts with a five-minute result-cache TTL.

The example saves text for review. It does not publish or send a finished report.

## Source and offline validation

- [brief.memql](research/brief.memql) contains every authored definition and the two core builtin imports.
- [memql.toml](research/memql.toml) declares the domain's language edition.

From the repository root:

```bash
go run ./cmd/memqllint examples/research-desk
```

Use **directory mode**: it overlays the domain on the embedded core DSL and runs
the engine's load-time checks, including imported symbols and statement bodies.
It makes no inference calls and does not connect to your cluster.

For a downloaded copy, keep this layout and lint the directory containing `research/`:

```text
research-desk/
└── research/
    ├── memql.toml
    └── brief.memql
```

The domain directory supplies the `research` namespace. The concept ID is
`v1:research:researchBrief`; no `@namespace` annotation is needed.

## Prerequisites for a real run

- A development cluster with the core **Library** and **agents** integrations,
  database/vector support, and an account authorized to load this domain and
  run its constructs.
- Files uploaded by that account, with successful analysis and embeddings.
  Check `status` and `embeddingStatus`; an unindexed file is not a searchable
  source. Index and query embeddings must use compatible model/vector settings.
  See [Files and semantic search](../../docs/public/operate/library.md#search-by-meaning).
- An existing agent available to that account, with a compatible model route.
  Supply its actual `agentId`. Use an agent configured for drafting, with no
  external-action tools needed for this example. Its system instructions,
  routing, and tool permissions remain in effect.
- Execution on an **agent node**: `runAgentTurn` is synchronous and refuses on a
  node without an agent runtime. The sample does not provision or route nodes.
  See [AI routing](../../docs/public/operate/ai-routing.md).
- An authenticated execution context. The search and saved rows are scoped to
  the actor; an unowned scheduler/system event is not a substitute for the
  authenticated request's create event.

Load the `research/` domain through your development cluster's DSL bundle
workflow (`MEMQL_DSL_PATH`). The directory must be visible inside the running
engine, alongside its embedded core domain tree. Loading registers the schema,
helpers, mutations, query, and event subscription together. Merely opening or
saving the file in an editor does not load it. Use the deployment process for
your cluster; this example does not change a running deployment for you.

## Run from Visual Studio Code or Cursor

1. Install the [MemQL extension](../../editors/vscode/README.md), open this example,
   trust the workspace, add your development cluster, and sign in.
2. After the domain is loaded, inspect `researchBrief` and the helper definitions
   in **Constructs**. Resolve any load or permission errors first.
3. Run `requestResearchBrief` with a fresh `briefId`, your configured `agentId`,
   and a question answered by your uploaded documents—for example,
   `What does our incident guide say about database failover?` This writes a row
   and can trigger real embedding and model calls.
4. Run `researchBriefs`. A successful draft has `status: "draft"`, an `answer`,
   and the source IDs/snippets. Inspect it and verify citations before using it.
   The automation is asynchronous: the new draft may not exist on the first read.
5. Inspect the brief in **Data** and the cluster's automation execution records
   if no draft appears. `no_sources` means retrieval returned no files and the
   model step was skipped; that status is intentionally outside the draft query.

A model or retrieval failure stops the automation before the final save and
leaves the requested row available for investigation. The sample does not
promise automatic recovery or exactly-once inference. Manually rerunning an
automation can make another model call. Reusing a brief ID appends a version;
use a fresh ID to request a new brief. Saving the draft emits an update rather
than another create event, so this trigger does not loop on its own output.

## Core constructs in this file

| Construct | Its job here |
|---|---|
| `concept researchBrief` | Declares typed stored fields and row ownership. |
| `shape researchBrief researchBriefSummary` | Selects the fields returned to a draft reader. |
| `trait hasDraftStatus` | Names a reusable row condition; fields are checked against the concept where it is applied. |
| `spec researchBrief hasResearchAnswer` | Binds the nonempty-answer condition to this concept. |
| `query researchBrief researchBriefs` | Composes the predicates and ownership filter, then returns the chosen shape. |
| `mutation researchBrief requestResearchBrief` | Writes the request; `saveResearchBrief` appends its outcome. |
| `logic` | Composes calls and returns a value for another step to use. |
| `automation prepareResearchBrief` | Starts on a create event and connects the calls into a workflow. |

A trait has no fixed concept binding; a spec binds exactly one concept or
shape. Both are side-effect-free predicates. Neither replaces the explicit
ownership check in the query. Draft reads require a nonempty answer.

[Specs and traits](../../docs/public/language/specifications.md) ·
[Shapes](../../docs/public/language/memql.md#shapes) ·
[Queries and mutations](../../docs/public/language/functions.md).

## What each capability does

**Similarity.** `librarySimilarArtifacts` uses vector similarity over indexed
file chunks, applies ownership checks, and folds matches to files. It is an
approximate bounded search: owner filtering happens after candidate retrieval,
so an eligible match outside the candidate pool can be missed. It returns short
snippets, not whole-document evidence. The sample deliberately uses this
owner-aware wrapper instead of the lower-level concept-scoped `similarTo`.

**AI.** `builtin runAgentTurn(...)` invokes a configured agent and returns a reply
envelope. `turn.first().reply` extracts the text. This is an actual registered
DSL builtin; the sample does not invent a direct prompt-call spelling. The
prompt asks for source IDs and treats snippets as data, but that is not a
citation validator or an enforcement mechanism for the agent's tools.

**Caching.** `@cache(300)` sets the query-result TTL in seconds. Writes to the
read concept invalidate dependent cached results across nodes. The query reads
saved drafts; it does **not** cache the embedding or agent calls. Pure reads
already have a default TTL; `@cache(0)` opts out.

**Composition.** The automation binds typed event fields, calls two named logic
functions, handles an empty search result, and persists the outcome. The
extension helps author, diagnose, inspect, and explicitly execute those
constructs. The engine owns retrieval, inference, caching, events, and storage.

[Walkthrough](../../docs/public/language/research-workflow.md) ·
[First program without a model](../../docs/public/language/first-program.md) ·
[Statement bodies](../../docs/public/language/memql.md#bodies).
