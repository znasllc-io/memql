---
title: Memory search
audience: public
status: stable
area: operate
sinceVersion: 0.31.41
owner: znas
description: The one embedding model memory search uses, whose machines embed for it, and how an owner chooses it, switches it and switches back.
---

# Memory search

Memory search finds items by meaning: a Library file's passages, a knowledge
domain's chunks, what a run observed, a conversation's evidence, the authoring
catalog and the Training lessons. Each item's text is turned into a vector by an
**embedding model**, a search's words are embedded by the same model, and the
nearest vectors answer it.

It is built on the time-series memory graph. Every item it finds is a row of the
graph; the index beside it keeps one vector per item for each model, in a table
of that model's own (`node_vectors_<dims>_<16 hex>`), and never the item's text,
which is rebuilt from the row whenever it has to be embedded again. What each
person may see in a result is decided by the row, as everywhere else.

Owners see and change it in **Cluster > Settings > Memory search**. Anyone else
they grant it to sees it there and changes nothing.

## The model

Vectors from two models cannot be compared, so the whole cluster uses ONE
memory-search model at a time, named on one row (`v1:platform:embedderBinding`,
id `active`). It is either:

- **a model on a machine**, such as `fleet:qwen3-embedding:0.6b`: embedded on
  an owner's machines, and no vendor account pays; or
- **a vendor model**, such as `embedding3Small` (OpenAI's
  text-embedding-3-small): the cluster's vendor account pays for every item and
  every search, whoever wrote or runs it. Only an active owner's choice binds
  one, never the automatic choice.

Every memory-search embedding names this model. It never falls back to another
and never degrades ([AI routing](ai-routing.md)): a vector from another model
would land in a space no search reads. When the model cannot be reached, nothing
is embedded with a different one: indexing waits (below), and search by meaning
has nothing to compare until the model answers again.

## The indexer

A model on a machine has an **indexer**: the cluster owner whose machines, and
the machines lent to them, embed everything for memory search -- every item
indexed and every search -- whoever wrote the item or runs the search. A person
with no machine is indexed, and searches, like anyone else. Only the embedding
step changes hands: reading and writing the item keep their usual authority.
The row names the indexer (`indexedBy`) and who chose the model (`chosenBy`),
and any signed-in person can read it, so anybody can see whose machines receive
their memory text.

**Where the text goes.** An item's text, and a search's words, go only to the
indexer's machines and the machines lent to them or, for a vendor model, to the
vendor whose model the cluster uses.

**The indexer must stay an active owner**: an active user whose role is
`owner`, which each replica checks again within a minute. When they stop being
one, or none of their machines offering the model is online, indexing waits:
nothing is lost, no item is counted failed, the backlog grows, and Memory search
says why ("37 items waiting: none of Ana Owner's machines offering
qwen3-embedding:0.6b is online").

**An owner takes over by choosing the model again.** When the indexer is
missing, or is no longer an active owner, or none of their machines (their own
or lent to them) offers the model while one of yours does, or none of theirs
offering it is online while one of yours is, Memory search offers an active
owner **Index on my machines**, and choosing it makes them the indexer. It is
offered only where it changes something: an indexer with a machine online that
offers the model is never replaced. While the indexer's laptop sleeps, an owner
whose machine is awake can take over, rather than every search waiting for it
to wake.

A vendor model has no indexer.

## The automatic first choice

A cluster with no model binds its first by itself on a readiness pass: an
embedding model that an ONLINE machine of an active owner offers (their own, or
one lent to them), indexed by that owner and chosen by nobody. When several
qualify they are tried in model-id order, and the first that answers is bound.
It never binds a vendor model, and never one that only a non-owner's machine
offers. With no candidate the cluster stays without a model, memory search
embeds nothing, the next pass tries again, and first-run setup says "Add an
embedding model" ([What an owner sees first](first-run.md)).

A cluster bound to a vendor model nobody chose -- by an earlier version's
fallback, or by hand -- keeps working, and Memory search asks an owner once:
"This cluster uses text-embedding-3-small, paid by the cluster's vendor
account. Keep it, or choose a model on your machines." **Keep** records the
owner who answers as its chooser. A model on a machine that was bound before
indexers existed is given one at boot: the first active owner, by id, whose
machines offer it.

## Choosing and switching

Memory search lists what can be chosen:

- every embedding model your machines, and the machines lent to you, offer, with
  how many offer it. One with none of those machines online is shown with the
  reason and cannot be chosen now, because the model is checked before anything
  starts.
- every vendor embedding model the cluster's own vendor account is set up for,
  marked "Paid by the cluster's vendor account".

Choosing asks first, in place. On a cluster with no model yet, the model is
bound at once: one sentence is embedded to measure its vector width, its table
is made, and every item is indexed with it from then on. Any other choice
starts a **switch**, and the question says what will happen: everything is
re-indexed in the background, on your machines or, for a vendor model, on the
cluster's vendor account, with about how many items; new items are indexed the
same way from then on; search keeps the current model until the rebuild
finishes, then switches by itself; and the current index is kept for 30 days in
case you switch back.

One switch at a time: until a switch's flip, no other model can be chosen, and
Memory search offers **Cancel**. Every act on this page is an active owner's:
choosing a model of either kind, a take-over, Keep, Cancel and Switch anyway.
Whoever chooses a model on a machine becomes its indexer.

## Re-indexing

One job does the rebuilding: every minute, for up to 45 seconds, on an agent
replica, and on one replica at a time (a Postgres advisory lock). It saves its
place and its counts after every batch of 32 items that embedded anything, so a
restart, or another replica, carries on where it stopped and embeds nothing
twice.

The job walks every kind of item memory search indexes, reads each with the
cluster's own authority, rebuilds its text by the same rule its writer uses, and
embeds it as the indexer, or at the vendor. An item already in the index, and
not older than its row, is skipped, so a walk can always be run again; an item
rewritten after it was embedded is embedded again. An item that fails three
times is counted failed and the walk goes on; the next walk tries it again.
When the model cannot be reached at all, the job waits with the reason and
counts nothing failed.

A switch, step by step:

1. The new model's index is filled while every search uses the current one.
   Memory search shows "Re-indexing 1,240 of 8,310 items. Search uses
   qwen3-embedding:0.6b until this finishes." The total is an ESTIMATE, the
   current index plus its backlog, because counting the whole cluster on every
   look would cost a walk of it; the figure done never passes it.
2. A catch-up walk picks up what was written meanwhile.
3. **The flip**: search moves to the new model in one write, the replaced
   model's index is dated to be kept 30 days, and the answer cache (below) is
   emptied.
4. A last walk catches anything written while every replica learned of the
   flip.

The flip happens by itself only when nothing failed and every entry of the
current index has an item to rebuild it from. Otherwise the switch waits for an
owner, saying how many items and why ("5 items could not be indexed (3 failed,
2 cannot be rebuilt)."), with **Switch anyway** -- those items drop out of
search until they are written again or a later walk indexes them -- or
**Cancel**.

**Cancel** stops the switch before its flip. The new model's partial index is
kept for 30 days, so choosing that model again resumes the rebuild instead of
starting over. After the flip the switch has happened: Cancel is no longer
offered (the engine refuses it), and going back is a new switch.

A switch to a model on a machine embeds as its own indexer, the owner who
started it. If they can no longer index the new model, the switch waits, and
another owner can carry it on with **Re-index on my machines**, which chooses
the new model again. **Index on my machines**, beside the model in use, takes
over the CURRENT model's indexing instead, which is what search and new items
wait on until the flip.

## The backlog

Every ten minutes the job also walks every item and fills what the current
model's index is missing: an item its writer could not embed when it was
written, or one rewritten since. While indexing waits it still counts what is
missing, at the same pace and embedding nothing, so the number is there when it
matters: Memory search shows "37 items waiting: none of your machines offering
qwen3-embedding:0.6b is online" under the index's size. The count is as fresh
as its last walk, and is not taken again while a switch runs.

## Retention

A replaced model's index is kept for 30 days after the flip, and a cancelled
switch's partial index for 30 days after the cancel. Once a day a cleanup drops
each kept index past its date, unless it belongs to the current model or to a
running switch's target. After that, choosing the model again rebuilds its
index from nothing.

## Switch back

While the replaced model's index is kept, Memory search says "The previous
model's index is kept until <date>." with **Switch back**, which asks the same
question as choosing that model. The switch reuses the kept index and embeds
only what is missing from it: what was written, or rewritten, since. For a
vendor model the question's item count is still the whole index, so it states
more than the cluster's account pays for.

It is offered for the model the current one replaced, while that model can be
chosen (one of your machines offering it is online, or the vendor account is
set up), and not while another switch is being rebuilt. A cancelled switch's
partial index is kept too, but is not offered here: it was never in use.
Choosing that model again reuses it all the same.

## The answer cache

The semantic answer cache
([AI-call caching](../ai/llm-cost-control.md#ai-call-caching-cost-reduction-not-just-bounding))
follows the memory-search model: it embeds every lookup and every stored answer
with the model memory search uses, as the indexer, and its entries are scoped by
model, so a lookup is never compared with an answer stored under another model.
The flip empties it.

With a model on a machine, every answer-cache namespace an operator enables
(`MEMQL_SI_SEMANTIC_CACHE_NAMESPACES`; none is enabled by default) has its
prompt text embedded on the indexer's machines.

## Who sees and changes it

Memory search is the part `app:cluster/memorysearch`, seeded on the `owner` role
alone; [Settings > Access](auth/access-model.md#grants-to-people-and-groups)
can grant it to others.

| Who | Sees and does |
|---|---|
| an active owner holding `read` and `execute` | everything on this page |
| anyone else holding `read`: somebody granted the part who is not an owner, or an owner without `execute` | the model, its indexer, the backlog and any switch. No act is offered, and every act is refused: where everyone's memory text goes, and what the cluster's vendor account pays for, are an owner's choice |
| neither | no Memory search in the Cluster app |

Fleet's model setup keeps the **Search & memory** task on the model memory
search uses. Its board says where to change it to whoever can open Memory
search, and to anybody else that a cluster owner can change it.

## From code

The page is six builtins, callable from the SDKs:

| Builtin | Needs | Does |
|---|---|---|
| `memorySearchStatus` | `read` | the one status row: the model, its indexer, whether you may act (`mayAct`), `takeOver`, the index's size, the backlog and why it waits, any switch (with its own `takeOver`), the kept indexes |
| `memorySearchChoices` | `read` | the models you may choose |
| `memorySearchSwitch(provider)` | `execute`, as an active owner | choose a model: bind the first, start a switch, or take over |
| `memorySearchCancel` | `execute`, as an active owner | cancel the switch before its flip |
| `memorySearchSwitchAnyway` | `execute`, as an active owner | flip a held switch, at the job's next tick |
| `memorySearchConfirm` | `execute`, as an active owner | keep a vendor model nobody chose |

Each act answers the status after it. "memory search changed meanwhile, try
again" means somebody else, or the job, moved it since you read it; "memory
search's model is being changed elsewhere right now; try again in a moment"
means another change is under way. Neither is retried for you. "only an active
cluster owner can change memory search, and you are not one" is the answer to
anybody else.

## Limits

- A switch's total is an estimate (above).
- The backlog is as fresh as its last count, up to ten minutes old, and is not
  counted again while a switch runs.
- Memory-search embeddings write no router decision (`v1:router:call`), so the
  router's decision log does not show them.
- The answer cache stores vectors 1536 wide only (`embedding3Small`'s width).
  Under a model of any other width, such as `qwen3-embedding:0.6b` at 1024, an
  enabled namespace stores nothing and every lookup falls through to a fresh
  call.
