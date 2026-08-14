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

## Seeded and trained

| | Where it lives | How it got there | To change it |
|---|---|---|---|
| **Seeded** | the embedded `dsl/` tree, or a product bundle mounted at `MEMQL_DSL_PATH` | loaded from disk when the node booted | build an image or a bundle, and roll it out |
| **Trained** | `v1:authoring:construct` rows in the database | promoted against the running cluster | promote again; live in seconds |

Both are real, and a cluster normally runs a mix. Every construct in the
engine's own `dsl/` tree is seeded. Every construct a product ships in its DSL
bundle is seeded. A construct you promote from your editor is trained.

The two are not ranked. Seeded is how a platform ships a contract it intends
to keep; trained is how a domain arrives — usually as **nouns**, because a
customer's world is made of concepts before it is made of queries over them.

Neither can shadow the other. The engine's core-first invariant is one-way: a
promoted construct may never take over a name core already owns, so training
can add to what a cluster knows and can never redefine it.

---

## Saving is not promoting

Writing a `.memql` file to disk changes **nothing** about what the cluster
runs. It is the single thing people get wrong, so it is worth stating flatly:

- Saving a file does not promote anything.
- There is no promote-on-save and no train-on-save. Nothing in the editor runs
  or promotes automatically; every action is a click.
- One file may hold trained, untrained and drifted constructs **at the same
  time**, and saving it leaves all three exactly as they were.

The editor's status bar reads `3 untrained - 1 drifted` for the active
document precisely so that this is impossible to miss. It reports only what
needs attention: a file whose constructs are all trained shows nothing.

---

## The four states

A construct in a file you are editing is in one of five states with respect to
the cluster you are connected to.

| State | Meaning |
|---|---|
| **untrained** | the cluster has no record of this construct |
| **drifted** | the cluster knows it, but not this version of it — the local source no longer matches what was promoted |
| **trained** | promoted, persisted, and live |
| **seeded** | loaded from disk at boot. The cluster has it, but it was never promoted, and it cannot be changed without a rollout |
| **unknown** | there is no connected cluster, so the question has no answer |

`seeded` is distinct from `trained` and the distinction is the point: there is
nothing to demote on a seeded construct, and no promote will change it.

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

## The four actions

Each action is a distinct commitment. They are offered as a CodeLens above a
construct's signature, beside the Run lens.

| Action | What it does | What it commits you to |
|---|---|---|
| **Dry-run** | compiles and binds the bundle in an isolated sandbox against a read-only clone of the live registry | nothing. Zero engine mutation; safe against production |
| **Try in session** | registers the bundle into your own stream-scoped registry, so it is callable by name | this connection only. It is dropped at disconnect, and nobody else ever sees it |
| **Promote** | persists the construct, registers it into the shared registry, and broadcasts it to every node | the cluster, durably. It survives restart and is visible to every caller |
| **Demote** | the inverse of promote | withdrawing it. For a concept, see the retire rule below |

Two properties of **Try in session** matter enough to state separately,
because mistaking it for a promote is the most expensive confusion available
here:

- It is **not durable**. Nothing is persisted.
- Its loss on reconnect is **silent**. After a reconnect the definition is
  simply gone, and the next call by that name resolves to the deployed
  construct instead. A tool that re-runs across a reconnect has to re-inject.

**Promote and demote are owner-only.** Dry-run and try-in-session are
owner-or-developer. That asymmetry is deliberate: a session definition affects
one stream, a promotion affects the cluster.

### The bundle is the closure, not the construct

A promote carries the construct **and what it depends on**. Promoting a query
whose spec is untrained must not half-land, so the closure is resolved and
shown before anything is committed — what you are about to teach the cluster
is what you see, not just the construct your cursor was on.

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

A retired concept survives a restart still retired.

The row count that decides between the two outcomes is read under an actor
that can see every row. A per-user count would make the outcome depend on who
asked, which is not a property an operation like this may have.

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

A refusal names the field, the number of rows affected, and the constructs
that reference it:

```
BREAKING - refused
  - sku                      removed; 1,284 rows carry it, 3 queries reference it
  ~ price  string -> number  old rows hold strings
  + region string!           required; existing mutations do not supply it
```

An explicit override lands the change anyway, and is audited as an override.
It exists for when the break is meant; it is not a way to make the message
stop.

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
  construct's origin (`core`, `bundle`, `promoted`) is the engine's own answer
  to which tree it came from. Guessing it by matching a directory called `dsl/`
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

## Every environment is trained separately

A promoted construct is a row in a database, under that environment's schema.
Staging and production are two namespaces on two schema search paths, so they
are **trained independently**.

Promoting to staging does not promote to production. There is no propagation
between them, and this is correct rather than missing: an environment boundary
in memQL is the connection, not a filter, and a promote follows the connection
it was made on.

If a construct is meant to exist in both, promote it in both — or ship it as a
seeded construct in the bundle, which is what a rollout is for.

---

## See also

- [MemQL in VS Code](vscode.md) — the extension and language server
- [VS Code Runtime Panel](vscode-runtime-panel.md) — Deployments, Clusters, and the concept browser
- [Authoring Rules](authoring-rules.md) — read before writing `.memql`
- [MemQL Language](memql.md) — the DSL reference
