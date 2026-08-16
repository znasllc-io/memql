---
title: Training Constructs Into a Running Cluster
audience: public
status: stable
area: language
sinceVersion: 0.15.0
owner: znas
---

# Training Constructs Into a Running Cluster

A memQL cluster can be **taught** a construct while it is running. The
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

## The six states

A construct in a file you are editing is in one of six states with respect to
the cluster you are connected to.

| State | Meaning |
|---|---|
| **untrained** | the cluster has no record of this construct |
| **drifted** | the cluster knows it, but not this version of it — the local source no longer matches what was promoted |
| **trained** | promoted, persisted, and live for everyone |
| **staged** | persisted and live **for you**. The cluster has it and no other caller can reach it |
| **seeded** | loaded from disk at boot. The cluster has it, but it was never promoted, and it cannot be changed without a rollout |
| **unknown** | there is no connected cluster, so the question has no answer |

`seeded` is distinct from `trained` and the distinction is the point: there is
nothing to demote on a seeded construct, and no promote will change it.

`staged` is distinct from `trained` for the mirror reason: it is the only state
with a **Train** action, and collapsing it into `trained` would claim the
cluster runs something only one person can call.

A staged construct you have edited stays `staged` rather than becoming
`drifted`, and that is deliberate. Drift is defined against a **promotion**, and
a staged construct has no promoted version to have drifted from — the same
argument that keeps an edited seeded construct `seeded`. Re-staging is how you
update it.

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

There is **no migration**, ever. Every row in memQL lives in one generic
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

---

## Read-only files

Some `.memql` files are marked read-only in the editor. One rule produces all
of it:

> A file is read-only exactly when editing it **cannot change what the cluster
> runs.**

| File | Connected cluster | Editable |
|---|---|---|
| core engine `dsl/` | any | no |
| product bundle `dsl/` | local | yes |
| product bundle `dsl/` | remote | no |
| a new file | any | yes — this is the training path |

Core constructs are sealed by the engine's core-first invariant, so an edit to
one changes nothing on any cluster. A remote cluster loads its bundle from its
own image, so editing a local checkout of that bundle changes nothing *there*
— which is why the verdict moves when you select a different cluster.

A **new file is never blocked**. Adding one is how training starts.

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
installation whose database it was written to. memQL ships one installation
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

## See also

- [MemQL in VS Code](vscode.md) — the extension and language server
- [VS Code Runtime Panel](vscode-runtime-panel.md) — Deployments, Clusters, and the concept browser
- [Authoring Rules](authoring-rules.md) — read before writing `.memql`
- [MemQL Language](memql.md) — the DSL reference
