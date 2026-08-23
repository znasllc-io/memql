---
title: Training Constructs Into a Running Cluster
audience: public
status: stable
area: language
sinceVersion: 0.15.0
owner: znas
---

# Training Constructs Into a Running Cluster

A MemQL cluster can be **taught** a construct while it is running. The
construct is validated, persisted, registered on every node within seconds,
and replayed at the next boot. No image build, no rollout, no restart.

That is one of two ways a construct can come to exist on a cluster, and the
distinction between them is the mental model this page exists to install.

---

## Seeded, staged, and trained

| | Where it lives | How it got there | Who can call it | To change it |
|---|---|---|---|---|
| **Seeded** | the embedded `dsl/` tree, or a product bundle mounted at `MEMQL_DSL_PATH` | loaded from disk when the node booted | everyone | build an image or a bundle, and roll it out |
| **Staged** | `v1:authoring:construct` rows, status `staged` | staged against the running cluster | **its author, and nobody else** | stage again; live immediately, for you |
| **Trained** | `v1:authoring:construct` rows, status `active` | promoted against the running cluster | everyone | promote again; live in seconds |

All three are real, and a cluster normally runs a mix. Every construct in the
engine's own `dsl/` tree is seeded. Every construct a product ships in its DSL
bundle is seeded. A construct you promote from your editor is trained. A
construct you stage is durable and yours alone until you train it.

They are not ranked. Seeded is how a platform ships a contract it intends to
keep; trained is how a domain arrives — usually as **nouns**, because a
customer's world is made of concepts before it is made of queries over them;
staged is how you keep one while you are still working it out.

None can shadow the other. The engine's core-first invariant is one-way: a
promoted construct may never take over a name core already owns, and a staged
one may take neither a core name nor a trained one. So training can add to what
a cluster knows and can never redefine it, and staging can add to what *you* can
call and never redefine what the cluster runs.

### Staged is promote with one step removed

    promote:  compile -> register SHARED -> persist row (active) -> broadcast
    stage:    compile -> register OWNER   -> persist row (staged) -> .

There is no separate staging pipeline, no second compile, no second store, and
no staged event. **The omission is the tier.** A staged construct is not a
different kind of thing from a trained one; it is the same thing that has not
been shown to anybody yet — which is why training it is not a new operation but
a state transition: the same row flips to `active`, the construct registers into
the shared registry, and the same broadcast fires.

**A concept cannot be staged**, and the refusal says so by name. A concept
registers into the one shared concept registry and its merge rebuilds
relationship state cluster-wide, so there is no owner-scoped form of it to
stage — a staged concept would be a row nothing ever reads. Train the concept,
then stage the constructs bound to it. That is the ordinary shape anyway: the
noun is the part you want everyone to agree on.

### Two things are called staged, and they disagree about the subject

Everything above is the **staged construct** tier (epic memql#3928). There is a
second tier that shares the word and means something else: **staged data** (epic
memql#3974), which is the subject of
[Staged data](#staged-data-rows-can-arrive-before-the-concept-is-trained) under
Concepts below. Read this table before you read either.

| | Staged **construct** (memql#3928) | Staged **data** (memql#3974) |
|---|---|---|
| What is staged | the construct's **resolution scope** | the **visibility of a concept's rows** |
| Recorded as | `v1:authoring:construct.status` = `staged` | `v1:authoring:construct.conceptDataStaged`, a **sibling boolean** |
| Can a concept be it? | **no** — refused by name | **only** a concept can |
| Registered cluster-wide? | no. It resolves for its author alone and is not broadcast | **yes** — registered, resolvable, and it accepts writes |
| Who can reach it | its author | its ROWS: nobody on the ordinary read path, and the **cluster owner** through an explicitly staged-scoped read (memql#4040). See below |
| What makes it live | Promote / Train | training the concept |

They are not two flavours of one idea. The difference is the **subject**. A
staged construct is a definition the cluster has not shown to anybody — the
omitted broadcast *is* the tier. Staged data is a concept the cluster has fully
published, whose rows are held back. So a concept is the one thing that cannot
be staged in the first sense and the only thing that can be staged in the
second, and a reader who carries one meaning into the other section will
conclude the opposite of the truth in both directions.

The collision is also why staged data is a sibling boolean rather than another
`status` value, and that would be true even if the name were free. The boot
re-hydration routes on `status`; a value it skips leaves the concept
unregistered; rows addressed by a name the engine no longer knows are not
withheld, they are **lost**. That is the argument `conceptRetired` already made
(memql#3756), arriving at the same answer for the same reason.

---

## Saving is not promoting

Writing a `.memql` file to disk changes **nothing** about what the cluster
runs. It is the single thing people get wrong, so it is worth stating flatly:

- Saving a file does not promote anything.
- There is no promote-on-save and no train-on-save. Nothing in the editor runs
  or promotes automatically; every action is a click.
- One file may hold trained, untrained and drifted constructs **at the same
  time**, and saving it leaves all three exactly as they were.

The editor's status bar reads `3 untrained · 1 drifted` for the active
document precisely so that this is impossible to miss. It reports only what
needs attention: a file whose constructs are all trained shows nothing.

---

## The seven states

A construct in a file you are editing is in one of seven states with respect
to the cluster you are connected to.

| State | Meaning |
|---|---|
| **untrained** | the cluster has no record of this construct |
| **drifted** | the cluster knows it, but not this version of it — the local source no longer matches what was promoted |
| **trained** | promoted, persisted, and live for everyone |
| **staged** | persisted and live **for you**. The cluster has it and no other caller can reach it |
| **seeded** | loaded from disk at boot. The cluster has it, but it was never promoted, and it cannot be changed without a rollout |
| **edited** | loaded from disk at boot, and your source no longer matches what the cluster loaded. Not drifted -- nothing was promoted. A rollout applies it: on a local cluster, **Rebuild from checkout** in the Deployments view |
| **unknown** | there is no connected cluster, so the question has no answer |

`seeded` is distinct from `trained` and the distinction is the point: there is
nothing to demote on a seeded construct, and no promote will change it.

`staged` is distinct from `trained` for the mirror reason: it is the only state
with a **Train** action, and collapsing it into `trained` would claim the
cluster runs something only one person can call.

A staged construct you have edited stays `staged` rather than becoming
`drifted`, and that is deliberate: drift is defined against a **promotion**,
and a staged construct has no promoted version to have drifted from.
Re-staging is how you update it. An edited **seeded** construct is `edited`
for the mirror reason -- there is nothing to promote over, and the editor
says so rather than reporting it current.

`unknown` is distinct from `untrained` for a blunter reason. A disconnected
editor must never report that the cluster does not have your work — it does not
know, and it says so by showing nothing at all.

Drift is decided by a **source hash**: the construct's source is normalized
(comments outside `///` doc blocks stripped, insignificant whitespace
collapsed) and hashed. The language server computes it for the open buffer and
compares against the hash the cluster stored at promote time. Because it reads
the **buffer** rather than the file, a construct you have edited and not saved
reports `drifted` — what you are looking at is what the comparison is about.

The hash is defined once and pinned by a parity test that runs both
implementations over the same corpus. A divergence there would silently mark
every construct drifted, which reads as "everything is broken" rather than as
the bug it is, so the parity gate is not optional.

---

## The five actions

Each action is a distinct commitment, and they escalate. They are offered as a
CodeLens above a construct's signature, beside the Run lens.

| Action | What it does | What it commits you to |
|---|---|---|
| **Dry-run** | compiles and binds the bundle in an isolated sandbox against a read-only clone of the live registry | nothing. Zero engine mutation; safe against production |
| **Try in session** | registers the bundle into your own stream-scoped registry, so it is callable by name | this connection only. It is dropped at disconnect, and nobody else ever sees it |
| **Stage** | persists the construct and registers it into your own durable tier | yourself, durably. It survives restart and reconnect, and no other caller can reach it |
| **Promote** | persists the construct, registers it into the shared registry, and broadcasts it to every node | the cluster, durably. It survives restart and is visible to every caller |
| **Demote** | the inverse of stage or promote, whichever the construct is | withdrawing it. For a concept, see the retire rule below |

Two properties of **Try in session** matter enough to state separately,
because mistaking it for a promote is the most expensive confusion available
here:

- It is **not durable**. Nothing is persisted.
- Its loss on reconnect is **silent**. After a reconnect the definition is
  simply gone, and the next call by that name resolves to the deployed
  construct instead. A tool that re-runs across a reconnect has to re-inject.

**Stage is the durable alternative to Try in session**, and it is the answer to
that silent loss: same owner-scoping, same "nobody else sees it", and it does
not die with the connection. Reach for Try in session when you want a
throwaway; reach for Stage when you would be annoyed to lose it.

**Training a staged construct is Promote.** The lens over a staged construct
labels it *Train (make it live for everyone)*, because that is the consequence,
but there is no separate operation on the wire and no separate command: the
engine sees the construct is staged for you and flips the same persisted row
rather than writing a second one. One construct, one lifecycle, one row.

**Promote and demote are owner-only.** Dry-run, try-in-session and **stage** are
owner-or-developer. That asymmetry is deliberate, and stage sits on the lower
side of it for the reason the tier exists: a session definition affects one
stream, a stage affects one person, and a promotion affects the cluster.

### The bundle is the closure, not the construct

A promote carries the construct **and what it depends on**. Promoting a query
whose spec is untrained must not half-land, so the closure is resolved and
shown before anything is committed — what you are about to teach the cluster
is what you see, not just the construct your cursor was on.

A **staged** dependency travels in the closure too, and the asymmetry with a
trained one is deliberate: the closure carries whatever the cluster does not
serve to *everyone*, and a staged construct resolves for one author. Omitting it
would let a promote land a shared construct bound to a private one — which
compiles for its author and resolves for nobody else.

---

## Concepts

Concepts are trainable, and they are the reason the rest of this exists: a
customer's domain arrives as nouns first, and every other construct kind binds
to one.

There is **no migration**, ever. Every row in MemQL lives in one generic
hypertable keyed by a `concept TEXT` column, so a new concept is a new *string
value* in that column. There is no per-concept table to create and no DDL to
run.

Two things behave differently for a concept than for a function, and both are
about the rows.

### Demote retires; it removes only when the concept is empty

Withdrawing a query makes it uncallable and that is the whole story.
Withdrawing a concept would strand rows already written under it.

| Rows under the concept | Outcome |
|---|---|
| any | **retired** — it stays in the registry marked retired, the name stays claimed, existing rows stay readable, new writes are refused with a message naming the retirement, and re-promoting un-retires it |
| zero | **removed** — the registry entry is gone and the name is free to claim again |

The rule underneath: data is never made unreachable by an operation whose name
suggests it affects only a definition. And a concept promoted by a typo, with
nothing written to it, must still be cleanly withdrawable — otherwise the
misspelling owns that name on that cluster forever.

A retired concept survives a restart still retired. The refusal a write gets
names the state and the way out, rather than failing as a generic validation
error:

```
concept "v1:acme:widget" is RETIRED: it was demoted while rows already existed
under it, so the rows stay readable and new writes are refused. Re-promote the
concept to resume writing to it.
```

A node with **no database** cannot take the count, and a demote there is
**refused** rather than guessed. The two wrong guesses are not symmetric:
guessing "retired" costs writes that one more demote can refuse again, while
guessing "removed" frees a name that rows are still addressed by.

The row count that decides between the two outcomes is taken **against
storage, under no actor at all**. That is deliberate, not a shortcut: per-row
authorization filters reads by the caller, so a count taken under the demoting
owner would answer "how many rows can *I* see". An owner who wrote nothing but
whose colleague wrote a thousand rows would get zero and remove the definition
— the outcome would depend on who asked, which is not a property an operation
like this may have.

### A changed schema is classified: additive lands, breaking is refused

Re-promoting a concept whose schema changed is a migration wearing a different
hat. It is helped considerably — not solved — by `schema` being stored **per
row**: every row carries the schema it was written under, so prior rows stay
valid and a query written against the new shape sees absent fields rather than
corruption.

So the promote path diffs the candidate against the promoted version and
classifies the change.

**Additive — lands:**

- a new optional field
- a new `@relationship`
- an edited `@description`
- a widened `@enum`

**Breaking — refused:**

- a removed field
- a changed field type
- a **new required field**. Existing mutations do not supply one, and a
  concept-field `@default` is never applied on insert (see
  [authoring rules](authoring-rules.md)), so `??` in the mutation body is the
  only mechanism that would fill it
- a narrowed `@enum`

Two changes are **reported in the diff but not classified breaking**: removing
a `@relationship`, and changing a node type. Removing a relationship makes no
row unreadable — and if the field it pointed through went too, that is caught
as a removed field — while the engine's own registry derivation is already the
authority on whether a node type is legal.

A refusal names the field, the number of rows affected, and the constructs
that reference it:

```
BREAKING - refused
  - sku                      removed; 1,284 rows carry it, 3 queries reference it
  ~ price  string -> number  1,284 rows hold string values
  + region string!           required; 1,284 rows lack it, 2 mutations do not supply it

ADDITIVE - lands
  + notes  string            optional; nothing to backfill
```

The breaking changes come first, because they are what the heading is about;
the additive ones follow so the whole change is visible rather than only the
part that stopped it. Row counts are real counts taken against the live table,
never estimates — and on a node with no database they are omitted rather than
reported as zero, because zero would be a claim the node is not entitled to
make.

An explicit override lands the change anyway. The heading then reads
`BREAKING - landed by explicit override`, and the engine writes an audit event
carrying the concept and the whole classified diff — an override leaves no
other record of what was overridden. It exists for when the break is meant; it
is not a way to make the message stop.

A **first** promote — one with no prior version to diff against — is not run
through the classifier at all.

This follows the same house rule the tool-declaration gates set: a silent
degrade is not permissive, it is confidently wrong.

### Staged data: rows can arrive before the concept is trained

> **This is not the staged tier from the top of the page.** See
> [Two things are called staged](#two-things-are-called-staged-and-they-disagree-about-the-subject).
> Here, the concept is fully published and only its ROWS are withheld.

A cluster could already be taught a concept while running. What it could not do
is accept **data** ahead of that moment — rows written against a concept nobody
had trained yet had nowhere to be. Epic memql#3974 gives them somewhere.

A promoted concept can carry `conceptDataStaged` on its `v1:authoring:construct`
row. While it does, the rows written under it are **present, addressable, and
withheld from the ordinary read path**, and training the concept makes them live
in place. Nothing is copied and no id is remapped: every node of an installation
already shares one PostgreSQL + TimescaleDB substrate, so becoming live is a
change of visibility, not of location.

Note what is **not** withheld. The concept registers into the one shared concept
registry, resolves for every caller, and keeps accepting writes exactly as any
other promoted concept does.

#### Visibility is a property of the CONCEPT, and of nothing smaller

There is **no marker on the rows at all**. That is a measured ruling
(memql#3977), taken against a real TimescaleDB container reproducing the live
DDL — `compress_segmentby = 'concept'` included — with every chunk compressed
before anything was timed, at 10,000 staged rows:

| Model | Transition | Decompresses? | Later reads |
|---|---|---|---|
| **concept-grain (chosen)** | **11 ms**, one row written elsewhere | **no** | unchanged, 7–12 ms |
| row marker, `UPDATE` | 116–142 ms | **yes** | unchanged |
| row marker, append a new version | 74 ms | no | **29–36 ms, permanently** |

Two results decided it, and neither is the millisecond column.

**Every row-marked model has to decompress.** That is proven rather than
inferred: with `timescaledb.enable_dml_decompression` set to false the `UPDATE`
does not run slowly, it hard-errors with *"UPDATE/DELETE is disabled on
compressed chunks"*. `compress_segmentby = 'concept'` puts every row of one
concept in one compressed segment, so marking that concept's rows means
rewriting the segment. Afterwards the affected chunks sit **partially**
compressed with roughly 46x heap inflation — and `chunk_compression_stats`
reports them `Compressed` throughout, so the view an operator would naturally
check cannot see the state at all.

**Concept-grain is the only candidate whose cost does not scale with the number
of staged rows.** The transition is one row written to the concept's own
`v1:authoring:construct` record; 10,000 staged rows and 10,000,000 cost the
same. Every other model is O(rows). The epic's premise is that data arrives
*before* the concept is trained, so the row count at transition time is
unbounded by construction.

The append-a-new-version model is the near-miss worth naming, because it avoids
decompression entirely and a reasonable person proposes it. It loses on the read
side: it doubles the version history permanently, so the latest-version collapse
for that concept stays 3–5x slower forever, and it rewrites every row's
`createdAt`, which leaves the staged version permanently reachable through an
`asOf` read positioned before the transition — the opposite of what the tier is
for.

#### A staged row is currently reachable by NOTHING

This is the sharpest thing to know about the tier as it stands, and it is
written down rather than left to be discovered.

The audience ruling (memql#3976) has two halves. The first — a staged row is
invisible on the ordinary read path to **every** caller, its own author included
— shipped. The second — that it stays reachable through a read which
**explicitly asks** for staged rows, with that scope set by the caller on the
read and never inferred from who is asking — did not.

The reason is the constraint that ruling turns on. The visibility predicate is
injected at a point that has no `context.Context`, and the ruling forbids giving
it one: a predicate derived from caller identity would have to be threaded into
every read seam, including the reads a Go executor issues directly, each of
which then gets its own opportunity to spell it wrong. So the predicate is a
**constant**, and a constant has no scope to widen. The choice on the table was
an absent escape hatch versus a partial one that appears to work and silently
does not, and the absent one is the honest answer.

**That constraint turned out to be narrower than it read, and memql#4040 built
the escape hatch without loosening the ruling.** What the parse path refuses is a
`context.Context`, not a *value that came from one*: `executeWith` already reads
the call origin (memql#2800) and the ambient envelope (memql#3024) off the
request once and hands them down as parameters, precisely so the injectors stay
ctx-free. The staged scope rides the same way. The predicate is still a constant
— now a constant of the *resolved scope* rather than of the staged set alone —
and no injector gained a context.

So an operator can inspect what is queued before training it:

- **The caller declares it, per read.** `memql.ContextWithStagedScope(ctx, ids…)`
  names the concepts whose staged rows this read may see. Nothing is inferred
  from identity: the cluster owner's *ordinary* read still sees no staged rows.
- **The cluster owner may use it.** Authorization is identity-derived and
  resolved once, up front, where the actor exists — deliberately separated from
  the predicate, which never sees an identity. An unauthorized declaration
  resolves to the empty scope wherever it is read, and `Execute` refuses it
  explicitly rather than returning an ordinary read the caller cannot tell apart
  from "nothing is staged".
- **The row gate honours it too**, through the same function the pushdown uses. A
  scope honoured by one seam and not the other would return *some* staged rows
  and not others.

It is Go-level, which is exactly as far as the staging side reaches — nothing on
the wire stages a concept either (see the table below). Training remains one-way:
there is no un-train that re-hides rows.

#### Rows persist. There is no TTL, and that absence is defended

A concept nobody ever trains keeps its staged rows indefinitely (memql#3978).

That is a ruling, not an oversight. Graph rows carry no retention policy at all
today, and the absence is deliberate and already guarded — the version-history
migration states outright that versions are never evicted on a timer, and a test
fails if a retention policy is ever added to it. A staged-row TTL would be the
first timer that deletes graph rows, and it would arrive by the side door.

It would also discard data somebody staged on purpose. Load a dataset, get
pulled onto something else for a quarter, come back to train it, find it
silently gone. Staging exists to *hold* data until someone is ready; a tier that
discards what it is holding has inverted its own purpose.

The lifecycle already has the right verb for a concept nobody wants, and it is
the demote rule above. Staged rows need no separate disposal rule, because they
are rows and that rule is about rows.

#### Demote does not re-stage

Withdrawing a concept that has rows **retires** it, exactly as described above.
It does not push its rows back into staging (memql#3978).

Staging is a *pre-publication* state; un-publishing is a different operation and
it already has a ruling — a demoted-with-rows concept stays registered and its
rows stay readable, precisely so a demote does not strand data. Re-hiding them
would be an outage wearing a lifecycle transition's clothes, and it would look
reversible while being irreversible in practice: with no row-grain marker,
nothing records which rows were live before the demote.

#### Staged rows COUNT, and they must

Two decisions above turn on a row count — whether a demote **retires** the
concept or **removes** it, and whether a schema change is refused as breaking.
Staged rows are inside both counts.

They are today, for free, because both counters read **storage** rather than the
query path, under no actor at all. That predates this epic and has its own
reason: a count taken through the query path answers *"how many rows can I
see"*, which would make the outcome depend on who asked.

The change to guard against is a future one, and it will look like a cleanup —
routing those counters through the query path "for consistency", or having a
land-time check mechanically require the staged predicate on every read of the
row table. Either would make a concept holding 10,000 staged rows count as
**empty**, and two things follow immediately: the concept becomes *removable*
rather than retirable, freeing a name ten thousand rows are still addressed by,
and a breaking schema change lands unrefused against rows that are about to be
made live under a schema they do not satisfy.

So these counters are not merely exempt from the staged predicate — they are
**prohibited** from carrying it, with the prohibition enforced rather than
commented (memql#3984). A check that mechanically required the predicate
everywhere would not prevent that bug. It would create it.

#### What is built, and what is not

Shipped: the tier itself (memql#3980) — the durable flag, the in-memory marker,
the promote-side write and the boot replay — and read enforcement (memql#3983),
which is what makes a staged concept's rows actually stop coming back.

Not yet, so do not assume it:

| Task | What is missing today |
|---|---|
| memql#3985 | The **write chokepoint**. A write to a staged concept still publishes its `node.created` event, and that event carries the whole row payload flattened into it. Suppression has to happen at the publish site — the event bus has no actor and no authorization hook, so withholding the read while emitting the event publishes the row to every in-process subscriber and calls it hidden (ruled on memql#3979) |
| memql#3986 | The **transition and its cross-node propagation**. The marker is set on the node that promoted and re-derived by every node at *its own* next boot; it is not broadcast, so a peer that has not restarted since does not have it. There is also no train-this-concept's-data operation yet: the only un-staging path is a re-promote writing a new construct row with the flag false, which the "a live row wins" fold resolves as live |
| memql#3984 (partial) | The **repack blind spot** is closed for `integration.chat.recentChat` (memql#4029) and `PluginContext.AdmitSourceRow` is the door any other direct reader uses, but the wider inventory of hand-rolled `MemoryNodes` reads is still open |
| memql#3984 | The **direct-SQL reads**. A read issued inside a Go executor never passes through a plan, so neither the injected predicate nor the row gate sees it |

There is also no editor action, no MCP tool and no wire message that stages a
concept's data. The mutation exists and the durable promote path takes the
intent; nothing operator-facing passes it.

The full read-side mechanism — where the predicate is injected, why the row gate
rather than the predicate is the correctness mechanism, and what it does not
cover — is in
[Per-row authorization audit](../operate/auth/per-row-authz-audit.md#staged-data-visibility-memql3974).

---

## Read-only files

Some `.memql` files are marked read-only in the editor. One rule produces all
of it:

> A file is read-only exactly when editing it **cannot change what the cluster
> runs.**

| File | Connected cluster | Editable |
|---|---|---|
| any origin, core included | **local** | yes |
| core engine `dsl/` | remote | no — badge `C` |
| product bundle `dsl/` | remote | no — badge `R` |
| promoted or staged | any | yes — it lives in the database, not in a tree |
| a new file | any | yes — this is the training path |

**A local cluster locks nothing.** It is rebuilt from a checkout on your own
machine — **Rebuild from checkout**, in the Deployments view — so an edit to any
file it loaded can change what it runs, core included.

**Which clone it rebuilds from is a hint, not a lock.** The install receipt
records one directory. With a local cluster selected and a *different* clone of
the same repository open, every file stays editable — it is your file — and the
ones the cluster loaded carry an `L` badge whose hover says this is not the
folder that cluster rebuilds from. Locking them instead would be the editor
deciding which of your checkouts is the real one.

Against a **remote** cluster the two read-only reasons have different ways out.
A remote cluster loads its bundle from its own image, so editing a local
checkout of that bundle changes nothing *there* — select the local cluster and
open its checkout. Core constructs are additionally sealed against promotion by
the engine's core-first invariant, so on a cluster nothing here is rebuilt into,
an edit to one changes nothing it runs.

A **new file is never blocked**. Adding one is how training starts.

**A cluster document is read-only by construction**, which is a different
mechanism rather than a stricter setting. When a construct's file is not on this
machine at all, the editor can serve it from the cluster that loaded it
(**View source from cluster**, on the construct's detail page) as a
`memql-cluster://` document — bytes from the cluster's own pack browser, badged
`RO`. There is no file to write back to, so nothing forbids the edit; there is
simply nowhere for it to go. `files.readonlyInclude` is not involved.

Two clarifications that are easy to get backwards:

- **The classification comes from the cluster, not from the path.** A
  construct's origin (`core`, `bundle`, `promoted`, `staged`) is the engine's
  own answer to which tree it came from. Guessing it by matching a directory called `dsl/`
  would be wrong the first time a product bundle also lives in one — which is
  the convention.
- **The marking is a courtesy, not the control.** The editor manages
  `files.readonlyInclude` in workspace settings and you can override it. What
  actually refuses is the engine: a promote may not shadow a core name. The
  editor explains; the engine enforces.

When there is no connected cluster, every file stays editable. There is no
"what the cluster runs" for an edit to fail to change, so the condition for
read-only cannot be met — and the alternative locks a developer out of their
own checkout because their laptop went to sleep.

---

## Every installation is trained separately

A promoted construct is a row in a database, so it exists in exactly the
installation whose database it was written to. MemQL ships one installation
shape (epic memql#3943): a second environment is a second install, with its own
database, and the two are **trained independently**.

Promoting on one does not promote on the other. There is no propagation
between them, and this is correct rather than missing: a promote follows the
connection it was made on, and two installs share no connection.

If a construct is meant to exist in both, promote it in both — or ship it as a
seeded construct in the bundle, which is what a rollout is for.

Staged constructs follow the same rule for the same reason: a staged row is a
row in one installation's database, so staging against one install tells another
nothing.

---

## Watching a goal author its own constructs

When a construct is authored by a **goal** rather than by a person -- the
runtime authoring path, gated by `MEMQL_AUTHORING_CAPTURE_MODE=author` -- the
portal has a page for it: `/nexus/:planId/constructs` shows the bundle that
goal produced, every construct in it with its source, and the dependency
edges between them, with **stage** and **promote** behind confirmations. The
Map beside it draws the same constructs materializing as the goal works.

**Capture is off by default, and the page says so rather than looking
empty.** A goal that succeeded and left no bundle is the signature of capture
being off, and Constructs states that plainly -- an empty list would read as
"this goal built nothing", which is a claim about the goal rather than about
the cluster.

See [MemQL Portal -- operator guide](../operate/portal.md).

---

## See also

- [MemQL in VS Code](vscode.md) — the extension and language server
- [VS Code Runtime Panel](vscode-runtime-panel.md) — Deployments, Clusters, and the concept browser
- [Authoring Rules](authoring-rules.md) — read before writing `.memql`
- [MemQL Language](memql.md) — the DSL reference
- [MemQL Portal](../operate/portal.md) — Nexus: a goal's constructs, and the
  map of the goal that authored them
