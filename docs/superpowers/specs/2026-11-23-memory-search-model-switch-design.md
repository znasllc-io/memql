# Memory search: choosing and switching its model

Design record, 2026-11-23/24. Status: proposed, for the owner's approval.
Follows the vendor accounts work
([2026-11-21-vendor-accounts-and-api-keys-design.md](2026-11-21-vendor-accounts-and-api-keys-design.md)).

## Intent

MemQL never pays for anybody's inference unless the cluster owner grants it.
Model access is bring-your-own: a person's machines, apps and keys, and what
others lend them; the cluster's vendor account pays only where the owner said
so. Memory search follows the same rule, with one difference: its index is
one for the whole cluster, so the OWNER provides it.

Today it falls short in four ways:

- The cluster picks its memory-search (embedding) model automatically the
  first time, and if no machine offers one it falls back to a vendor model
  that the cluster's account then pays for, which the owner never chose.
- Each item is embedded on a machine of the person who wrote it (theirs or
  lent to them). Somebody with no machine offering the model is simply not
  indexed, and nothing ever catches up.
- The owner can see the model only on a Fleet machine's board.
- The owner cannot change it: a switch is refused, because the re-index it
  needs was never built.

The owner's decisions (2026-11-23 and 24):

1. The model is a machine model by default. Nothing ever falls back to a
   vendor automatically.
2. The owner may deliberately choose a vendor's embedding model in settings.
   By that choice, the cluster's account pays for all indexing.
3. The model is shown, and changed, in the Cluster app's settings.
4. A switch RE-INDEXES FIRST: everything is rebuilt with the new model in the
   background while search keeps using the current one, and search switches
   over by itself when the rebuild is complete.
5. The old index is kept for 30 days after a switch, so switching back is
   quick, and is deleted after that.
6. The re-index runs on the cluster; only the embedding step (text to
   numbers) goes out, to the owner's machines or to the vendor the owner
   chose.
7. Everyday indexing works the same way: every item is embedded on the
   owner's machines (and machines lent to the owner), or by the vendor the
   owner chose, whoever wrote the item.

## Goals, non-goals, anti-goals

Goals: the seven decisions above, working across replicas, surviving
restarts, visible to the owner while they run, and catching up on anything
missed while the owner's machines were offline.

Non-goals: a model per person (one model per cluster is what makes search
work: vectors from two models cannot be compared); re-indexing the semantic
answer cache (it refills on its own; its rows are dropped at a switch);
changing any chat model.

Anti-goals, each of which makes the work a failure:

- A vendor model in use that the owner did not choose.
- Search answering from a half-built index, or from two models at once.
- An item silently missing from search: missed now must mean caught up
  later, with the backlog visible to the owner.
- A re-index that dies with the replica running it, or runs twice at once.
- The cluster's vendor account paying for a re-index the owner did not
  confirm.
- Anybody's item text sent anywhere but the owner's machines, machines lent
  to the owner, or the vendor the owner chose.

## How it works today

- One binding row, `v1:platform:embedderBinding:active`
  (`component/memql/embedder_binding.go`): `providerRef` (a vendor record or
  `fleet:<model>`), `dimensions`, `activatedAt`, `previousRef`,
  `reembedRunId`. Production: `fleet:qwen3-embedding:0.6b`, 1024 wide, since
  2026-10-22.
- Every model's vectors live in their own table,
  `node_vectors_<width>_<hash of providerRef>` (`EmbeddingVectorTable`), with
  columns `id, concept, vector_field, embedding, created_at, updated_at`. The
  item's TEXT is not stored: each writer builds it from its own row and calls
  `integration.embedding.store` (work observations, memory beliefs, Library
  file chunks, knowledge items, the authoring catalog, work memory and
  acknowledgements, Library training). Readers resolve the active binding's
  table, and embed a search query with the same pinned model.
- An embedding call is a fleet or vendor call like any other: a fleet model
  resolves against the ACTING person's machines and the ones lent to them.
  Since September a machine can only be private or lent to named people and
  groups; the old "Everyone in this cluster" share cannot be created.
- `autoBindEmbedder` (`embedder_runtime.go`) binds the first model: models
  registered machines offer (bound as that machine's owner), then
  `federationEmbedders()` (the vendor fallback this design removes).
  `bindEmbedder` is cluster-owner only and refuses any change of an active
  binding.
- `embedder_activation.go` already describes the safe order (new table, plan,
  re-embed, compare, flip) but the re-embed was never built, and its count
  comparison still uses the older width-only table names.
- Binding changes are broadcast to every replica
  (`component/node/routing.go`), which invalidates each replica's cached
  binding.
- Embedding calls pin the bound model and never degrade or fall back.
- A failed embed is logged ("until backfilled") and nothing backfills.
- Fleet's model board shows the bound model locked: "Memory search keeps
  <model>. Switching requires a reindex."

## Approaches

- **A. Re-index first (chosen by the owner).** Build the new index beside the
  old one, keep serving search from the old one, flip when complete.
- **B. Switch right away (declined).** Flip at once; items indexed so far
  drop out of search until switched back or re-indexed.
- **C. Dual-write during the rebuild (not taken, my call).** Writers would
  embed every new item with both models while a rebuild runs. It doubles the
  work on the write path and on the owner's machines; catch-up passes before
  and after the flip give the same result off the write path.
- **Indexing per person (declined by the owner).** Keep embedding each item
  on its writer's machines: bring-your-own, but whoever has no machine with
  the model is never searchable.

## The design

### 1. Who provides the index: the indexer

The binding records its INDEXER: the cluster owner who chose the model (a new
`indexedBy`, alongside a new `chosenBy`). For a machine model, every embedding
call for memory search (indexing an item, and embedding a search query)
resolves as the indexer, so it runs on the indexer's machines and the ones
lent to them, whoever wrote the item or runs the search. For a vendor model,
every such call is paid by the cluster's account. Only the embedding step
changes hands: reading and writing the item itself keeps its usual authority.

If the indexer stops being an active cluster owner, or no machine of theirs
offers the model, indexing pauses (the backlog grows, nothing is lost), and
the Memory search page says so and lets an owner take over by choosing again.

### 2. The automatic first choice is a machine model of an owner

`autoBindEmbedder` considers only models offered by a cluster owner's machines
(or machines lent to one), and binds with that owner as indexer. With none,
the cluster stays unbound, as the setup screen already says ("Add an embedding
model"). A vendor model is bound only by an owner choosing it.

A cluster already bound to a vendor model (by the fallback this removes, or by
hand) keeps working; the Memory search page asks an owner once to confirm it
or choose a machine model, and the confirmation records `chosenBy`.

### 3. Choosing a model

An owner chooses from: machine embedding models offered by their machines and
those lent to them, with how many offer each; and vendor embedding models the
cluster has an account for, each marked "paid by the cluster's vendor
account". A first binding on a cluster with no vectors activates at once. Any
other choice starts a switch.

### 4. The index job

One mechanism serves three uses: a switch's rebuild (filling a new table), the
everyday backfill (filling what the active table is missing, on a schedule),
and the catch-up passes around a flip.

- It runs on the cluster, on one replica at a time, claimed under a Postgres
  advisory lock; its cursor and counts are saved after every batch, so a
  restart or another replica resumes where it stopped.
- It walks the registered SOURCES (section 5), reading every indexable item
  with the cluster's own authority, and for each item missing from the target
  table it rebuilds the text, embeds it as the indexer (or on the vendor) and
  writes it. An item already in the target is skipped, so the job is safe to
  re-run.
- A failing item is retried a few times, then counted as failed; the job goes
  on, and the next pass tries it again.
- If the indexer's machines are offline it pauses and waits, rather than
  failing the items.

### 5. Sources: how an item's text is rebuilt

A registry in `component/memql` keyed by `(concept, vector_field)`: each
writer registers which of its rows are indexed and how their text is built.
Most are one payload field (a Library chunk's text, a memory belief's
content); a work observation reuses the rules `embedWorkObservation` applies
(summary over content, the excluded administrative rows). The writers'
everyday path and the job use the same recipe, so the two never disagree. A
test pins that every concept with rows in a seeded index has a recipe; an
entry in an old table with no recipe is counted as `unsourced` at a switch.

### 6. A switch

One switch at a time, recorded on a plan row: target `providerRef` and width,
its table, who started it and when, state (`indexing`, `catchingUp`,
`switching`, `done`, `cancelled`, `failed`), cursor, and counts (total, done,
skipped, unsourced, failed).

The job fills the new table while search keeps using the active one. When the
walk ends, a catch-up pass picks up what arrived meanwhile. Then the flip, in
one transaction under the binding's lock: the target becomes active, and the
previous binding is kept with `retireAt` = now + 30 days. A last catch-up
after the flip covers anything written in between: a replica can index with
the old binding for a few seconds after the broadcast, and the old table is
still there, so nothing is lost.

The flip is automatic only when every item is done. If some are unsourced or
failed, the switch waits, and the owner sees how many and of which kind, with
**Switch anyway** (those items drop out of search until written again or
picked up by a later pass) or **Cancel** (my call).

Cancel stops the job; the partial new table is kept under the same 30-day
retention, so starting the same switch again resumes rather than restarts
(my call).

### 7. Keeping and deleting old indexes

Each retired binding keeps its table until `retireAt`. A daily cleanup on the
leader drops tables past their date that are neither active nor the target of
a switch. Switching back within the window reuses the old table, so the job
only fills what was written since.

### 8. Interface

Cluster > Settings > **Memory search**, owners only, under the OS interface
rules (`clients/os/DESIGN.md`):

- the model in use, machine or vendor, whose machines index (or "the
  cluster's vendor account"), how many items are indexed, and any backlog
  ("37 items waiting: none of your machines offering qwen3-embedding:0.6b is
  online");
- **Change model**, listing the choices of section 3;
- a confirmation that says what will happen: everything is re-indexed in the
  background (on your machines, or, for a vendor model, on the cluster's
  vendor account, with the number of items); from then on every new item is
  indexed the same way; search keeps using the current model until the
  rebuild finishes, then switches by itself; the current index is kept for
  30 days in case you switch back;
- while a switch runs: "Re-indexing 1,240 of 8,310 items. Search uses
  qwen3-embedding:0.6b until this finishes." and **Cancel**;
- afterwards, while a previous index is kept: "The previous model's index is
  kept until <date>." and **Switch back**;
- the one-time confirmation of a vendor model picked before this change.

Fleet's board keeps the bound model locked and says where to change it.

### 9. Across replicas and restarts

What survives: the binding (with indexer), the plan row with its cursor and
counts, every table and the retained list, all in Postgres. What does not
need to: each replica's cached binding (refreshed by the broadcast) and a
batch in flight (redone from the cursor). One job per cluster at a time, by
the advisory lock.

## Tests and acceptance

- The automatic choice never binds a vendor model, and only an owner's
  machine model.
- Everyday indexing embeds as the indexer whoever writes the item: an item
  written by a person with no machine is indexed on the owner's machine.
- A switch rebuilds every item and flips only when complete; search answers
  from the old index until then (db-gated, two replicas over one database).
- An interrupted job resumes from its cursor on another replica.
- Items written during the rebuild, and between the last pass and the flip,
  end up in the new index; an item missed while the indexer was offline is
  picked up by the next backfill.
- Unsourced or failed items hold the automatic flip; Switch anyway and
  Cancel behave as described.
- A retired table is kept until `retireAt` and dropped after; switching back
  within the window embeds only what is missing.
- The interface: owners only, the confirmation's wording (vendor cost line
  included), progress, backlog, Cancel, Switch back, the one-time vendor
  confirmation.

## Left to the builder

Batch size, concurrency, retry counts and the backfill's interval; whether
the plan lives on `embedderBinding` rows or a concept of its own; the
cleanup's hour; the exact layout within the design rules.

## Calls made without the owner (to check)

- 30 days, owners only, one switch at a time.
- No dual-write; catch-up passes and a scheduled backfill instead.
- Unsourced or failed items hold the automatic flip, with Switch anyway.
- Cancel keeps the partial index under the 30-day retention.
- Search queries are embedded the same way as items (as the indexer), since
  they must use the same model and the searcher may have no machine.
- The job reads every indexable item with the cluster's own authority, to
  rebuild its text; only the embedding step runs as the indexer.
- An existing vendor binding keeps working until an owner confirms or
  changes it.
