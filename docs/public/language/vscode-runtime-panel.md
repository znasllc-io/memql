---
title: Visual Studio Code and Cursor Runtime Panel
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# Visual Studio Code and Cursor Runtime Panel

The MemQL extension's activity-bar panel connects Visual Studio Code or Cursor to a running
cluster: pick a cluster, browse what that cluster has **defined**
(Constructs) and what rows **exist** (Data), and run either without
leaving the editor.

Verifying a change to this panel: [Visual Studio Code and Cursor Runtime Panel -- Manual Verification
Checklist](vscode-runtime-panel-verification.md), which also states what the
automated `make vscode-test-host` smoke lane covers and what it deliberately
leaves to a human.

## Requirements

- A **trusted** workspace. Language features (highlighting, diagnostics,
  completion, hover, signature help) work in an untrusted workspace; the
  runtime panel does not. It reads credentials and opens a network
  connection, which a malicious workspace must not be able to trigger.
- A cluster in `~/.memql/clusters.yaml`, and a sign-in to it. **MemQL: Add
  Cluster** writes the first from a name and a domain, and **Sign In** gets the
  second through your browser; nothing needs editing by hand. A Personal Access
  Token does not work here and cannot -- see [Authentication](#authentication)
  below. If there is no cluster yet, the **+** opens the Add a cluster page,
  which also installs one locally (see
  [Installing a local cluster](#installing-a-local-cluster)).

The registry and token details below are reference for troubleshooting and
migration, not prerequisites you must hand-edit.

## Clusters

The panel reads the same `~/.memql/clusters.yaml` the MemQL Cockpit uses,
so a cluster added in either tool appears in both. The file is watched:
an external edit refreshes the view.

Click a cluster to use it: it connects, or, when it is already connected or
needs a sign-in, opens its page. The selection persists to `selected_cluster`,
and the cluster in use is marked on its row. Each row says its state in a word
-- Connected, Sign in, Connecting, Not running (a local cluster), Can't reach,
Not set up -- beside an icon:

| Icon | Meaning |
|---|---|
| Filled green circle | Connected |
| Spinner | Connecting, or retrying a dropped connection |
| Red error icon | The cluster is the problem -- unreachable, or the connection died and the retries stopped |
| Yellow key | The CREDENTIAL is the problem -- expired, missing, or the wrong class |
| Yellow warning | Not set up -- no address |
| Filled circle | The cluster in use, not connected yet |
| Hollow circle | Configured, not connected |

The key and the red dot are deliberately different pictures. "Your token
ran out" and "the cluster went away" have completely different next
actions, and rendering both as a red dot made the first one unreadable
(memql#3385).

Each row carries one inline act, the next thing to do in its state: **Sign in**
when it needs a sign-in, **Open MemQL OS** when it is connected. Everything else
is in the row's context menu, by state: Show Cluster Details, Open MemQL OS,
Sign In, Sign In With a Code, Create Owner Passkey (a first run on a local
cluster), Disconnect, Sign Out, Edit Cluster, and, in a group of its own, Remove
Cluster From List.

**The cluster page** (**Show Cluster Details**) shows the address, MemQL OS,
whether the cluster is installed on this computer, and who you are signed in as
while connected. Its bar carries the state in words and the acts legal from it:
**Sign in** (with **Sign in with a code**) when a sign-in is needed, **Open
MemQL OS** when connected, **Retry** and **Repair** when a local cluster is not
running. **Edit** and **Remove from list** sit at its head.

**The connection comes back on its own.** The cluster in use is connected again
when a window opens, whenever a stored session can do it, and a dropped
connection is retried a few times over about two minutes before the row says
it cannot reach the cluster and a notification offers **Reconnect**.

The **+** in the view title opens the **Add a cluster** page. It is a page
and not a menu: what belongs on it depends on what is already on this
machine, and the answer an operator gives is followed by ten minutes of
work. Neither fits in a list of three sentences. The page reads the
evidence first -- an install receipt, a `local: true` entry, and whether
that cluster answers -- and offers only the actions that evidence
supports.

**Edit Cluster** collects a name, domain, endpoint, access token and
(optionally) refresh token through the editor's own inputs. Writes
preserve comments and any field a newer cockpit wrote, because the file is
shared.

### Authentication

**Sign In** is one click from every place that says it is needed: the row, the
cluster page and the notification. It signs in as the editor's own built-in
client and opens your browser; when MemQL OS is already signed in there, it
finishes without typing. A remote window, or a host with no browser, uses a
short code you approve on another device, and after 30 seconds without the
browser coming back the editor offers **Use a code instead** beside it.
[Connecting an Editor](../operate/auth/connecting-editors.md) covers both
flows and the built-in client.

The panel dials with an **identity-issued JWT access token** -- the
`access_token` from `POST <identity>/oauth/token`.

**A Personal Access Token does not work here, and cannot.** PAT
verification is a database lookup wired only into the identity binary, so
every mesh node (bff, agent, planner, workbench) rejects an
`mql_pat_...` bearer *before* looking anything up. The panel detects one
and refuses it by name rather than letting it fail as an unexplained
handshake error (memql#3383).

Access tokens are short-lived -- identity issues them with a 900-second
TTL. The panel renews them itself with the refresh token: proactively before
each connect, and in place on a live stream via the SDK's re-auth hook, so a
long session never has to be re-credentialed by hand (memql#3385).

**The editor keeps its credentials in VS Code's `SecretStorage`**, never in
`clusters.yaml`: the access token, the 30-day refresh token, the expiry, and
the client the refresh token was issued to, which it presents on refresh. A
`token` or `refresh_token` key in the file (pasted by hand, or left by an older
extension) is an **ingest path only**: it is used, and moves into
`SecretStorage` at the first refresh, leaving the file. The Cockpit keeps its own
sign-in, and signing out of the editor leaves it alone.

A cluster entry therefore looks like this:

```yaml
clusters:
  - name: local
    domain: memql.localhost
    endpoint: api.memql.localhost:443          # or https://api.memql.localhost
    issuer: https://identity.memql.localhost   # optional; derived from domain
    local: true
selected_cluster: local
```

`endpoint` may be a bare `host:port` or an `https://` address naming the same
front door (the Cockpit writes the local cluster that way). `issuer` is where
the refresh exchange is POSTed. When it is absent the panel derives
`https://identity.<domain>` (or the `identity.` sibling of a `api.<host>`
endpoint); a cluster with neither is told which field to supply rather than
having a host guessed for it. A `client_id` in an entry belongs to the tool
that wrote it (the Cockpit writes `cockpit`), and the editor never signs in or
refreshes with it.

## Cluster lifecycle

Four verbs, in two places. The Add a cluster page and the Deployments view own
what changes this machine; Clusters owns which clusters you can reach. Two of
the four sound alike and are deliberately kept apart.

| Action | Where it lives | What it touches |
|---|---|---|
| **Install Local Cluster...** | The Add a cluster page (**Install MemQL on this computer**); the Clusters and Deployments title menus and welcomes when no local cluster is present | The full install graph |
| **Repair Local Cluster** | The Add a cluster page; the Clusters and Deployments title menus; the cluster page when a local cluster is not running | Re-runs the install. Every step verifies before it acts and skips what is already satisfied, so a repair is the install graph and not a second one -- and so is **Change Version...**, which moves an installed cluster to another release tag. |
| **Remove Cluster From List** | Clusters, the last group of a row's context menu; **Remove from list** on the cluster page | Drops the registry entry, the stored credential, and the live connection. Nothing on the machine is touched, and for a local cluster the confirmation says so. |
| **Uninstall Local Cluster...** | The Add a cluster page; the Deployments title menu with the local cluster selected | Reverses the install receipt: the k3d cluster, the hosts-file block, the downloaded MemQL files, and whichever shared tools you switch on. |

**Registering a cluster** is the other job of the Clusters **+**: a remote
one through **Connect to a cluster**, and a local one that is already on the
machine through **Connect to it**, which composes the entry from what the
install recorded and asks for nothing.

**Remove and Uninstall are different commands on purpose.** Remove is a
routine, reversible act of editing a list: the cluster keeps running, its
data is untouched, and adding it back is a matter of supplying its domain and
signing in again. Uninstall takes a cluster off the machine, and there is no
undo -- a k3d cluster that has been deleted is gone with everything in its
database. Folding both into one action that then asks which you meant would put
the irreversible one click away from the routine one, so they are separate
commands with separate labels, separate menus and separate confirmations.
Remove confirms with a modal whose second line says what is *not* happening;
Uninstall confirms on its own page.

Uninstall is never an inline icon, and it is not on any Clusters row at all, so
aiming at a cluster's context menu cannot land on it.

**The uninstall page is the confirmation.** Before anything is removed it
lists, under **Will be removed**, every artifact the receipt names and which of
them will ask for your password, so a step needing elevation says so before you
consent rather than surprising you with a password prompt mid-run. The shared
tools -- k3d, kubectl, the local certificate authority and mkcert -- are
switches under **Also remove**, all off. Anything the install *found* rather
than created is listed as **Kept** and is not touched: an mkcert CA that
predated the install belongs to the machine, not to MemQL, and guessing
otherwise would break every other locally-trusted certificate on it.

A cluster this editor did not create -- one brought up with `make up`, or one an
install adopted -- is kept too, unless you turn on **Delete the cluster's
data** and type `delete memql data`. Only then does the button read
**Uninstall and delete data**. That is the one removal of something MemQL did
not make, and the only one behind a phrase.

## Installing a local cluster

**MemQL: Install Local Cluster...**, or **Install MemQL on this computer** on
the Add a cluster page, opens the install form. It collects what an install
needs before any work starts -- your name and email for the owner account, and
under **More options** the domain and the version -- because a wizard that stops
to ask a question nine minutes in is a wizard people abandon. **Checks** below
the form say whether the installer is ready and whether your password will be
needed. **Install** asks for that password once, before anything runs;
dismissing the prompt runs nothing.

The run is one progress screen: the MemQL mark, a bar that only moves forward
(weighted by how long each step usually takes), the running step in a few words
("Creating the cluster", "Starting services 5 of 9"), `Step n of m · m:ss`, and
**Show logs** for the live log, each step's lines under its label, with
**Copy** and **Open in Output**. **Cancel** stops after the current step, and
**Resume** carries on; a cancelled install still leaves a valid receipt, so it
is still uninstallable. A failed step turns the bar red and names the step; one
notice says what the script said and what to do, with the fix as a command you
can **Run in terminal**, and the log opens at that step. Where several steps
fail -- which the executor allows deliberately, so you see every failure rather
than the first one and a shrug -- each is explained on its own terms, because
the exit codes ask for genuinely different things: a refusal is the script
protecting something, a missing prerequisite is something to go and install.
**Retry** appears once every other step has finished.

**The CLI is the scripted alternative, and it is not deprecated.** For CI, an
unattended provisioning job, or any situation with no editor in it, the same
install runs from a terminal:

```bash
cd editors/vscode
npm run install-cli -- install --domain=<domain> --owner-email=<email> \
  --owner-first-name=<first> --owner-last-name=<last>
npm run install-cli -- repair
npm run install-cli -- uninstall --receipt=<path>
```

The two front ends are not two implementations. The orchestration lives in
`editors/vscode/src/install/session.ts`; the page and `cli.ts` are both
callers of it, and `cli.ts` does nothing beyond parsing argv and printing.
That is what makes "it worked from the terminal but not from the editor"
impossible to introduce here -- there is no second run path to drift. Which
steps exist, what each one verifies, and what an uninstall may touch are
pinned in the graph documents and the receipt, not in either front end.

Both paths install the same cluster the repo's `make up` brings up. The
difference is what they assume: `make up` is for a checkout of this
repository and the inner development loop
([reproduce-the-cloud-locally.md](../operate/reproduce-the-cloud-locally.md)),
while the page installs onto a machine that need not have one.

## Constructs

The Constructs view lists everything the connected cluster has **loaded**,
grouped by kind and then by namespace, read from the engine's own
registry. It answers what a `.memql` file cannot: what is actually
running there, which is not necessarily what is in your checkout.

A construct added to the DSL appears with no extension update, and a
kind this extension has never heard of still renders -- under its own
name, at the end of the list. A client outlives several engine releases,
and a view that silently dropped what it did not recognise would
disagree with the cluster it claims to describe.

Selecting one opens its detail page: kind, namespace, origin, bound
concept, and its arguments with types and flags. **Nothing on that page
edits.** Editing happens in a `.memql` file, where the language server
owns it; a second authoring path here would be a second answer to a
question that already has one.

### Origin, and finding the source

| Origin | Meaning | Jump to source |
|---|---|---|
| `core` | The engine's embedded DSL tree | Opens the file, revealed at the signature -- when the file is in this workspace |
| `bundle` | A product's DSL, mounted at `MEMQL_DSL_PATH` | Same, and the same caveat |
| `promoted` | Lives in the cluster's database; **there is no file** | Nothing to open. The source is rendered on the page from what the cluster holds |
| `staged` | The same place as `promoted`, with a different audience: **only its author can call it** until it is trained | The same answer, for the same reason |

The catalog reports a path relative to the **cluster's** tree, which is
not obliged to be the checkout you have open -- for a remote cluster it
usually is not. When the path does not resolve here, the page's
**Open source** still works: the cluster that loaded the construct also serves
the file, over its pack browser, so the source opens as a read-only
`memql-cluster://` document at the signature. Highlighting and
nothing else -- a file that is not on this machine gets no diagnostics,
because the imports it names resolve against the cluster's tree rather
than yours.

The `promoted` case is where an operator first meets the seeded-versus-trained
distinction: a construct that exists on the cluster and in no repository.

### Which kinds run, and why the rest do not

**query, mutation, logic and tool** run from the detail page, through the
same path a CodeLens run takes -- the same argument form, preflight,
Result view and write confirmation. A mutation against a non-local
cluster still asks for confirmation; browsing a catalog is not a quieter
way to write to production.

**An automation runs from here too, through its own form.** It is fired
by an event, so what its form needs is not arguments -- an automation has
none -- but a trigger: which payload modes to offer, and which concept's
rows the picker browses. The catalog carries that trigger, so the detail
page opens the same automation form a `.memql` file's CodeLens does.

The button says **Run...** rather than **Run**, and the ellipsis is the
point: for the other four kinds a click invokes, while here it opens a form
that ends in firing a real event on a real cluster. An automation the cluster
reports no trigger for is manual-run, which is a real form rather than a
missing one, so it is offered as such.

One detail worth knowing, because it makes the catalog's answer better
than the file's: an automation written in the pre-structured trigger form
(`@trigger(event="graph.node.created.v1:x:y")`) reads to the language
server as one opaque event with no concept, because that is literally
what the file says. The cluster stores the composed topic and the catalog
decomposes it, so the catalog-sourced form gets a row picker the
file-sourced one does not.

**The other eight kinds are not runnable, and that is settled rather than
missing.** Each would need an execution semantic decided before a Run
control could mean anything:

| Kind | What "run it" would have to mean first |
|---|---|
| `spec`, `trait` | A predicate compiling to a SQL `WHERE` fragment or evaluating against the auth envelope. Running one alone means choosing rows to run it against -- which is a query. |
| `shape` | A projection with no inputs and no return. |
| `concept` | A schema. Browsing its rows is the Data view; "executing" it is undefined. |
| `prompt` | A model call, so it needs a cost decision before it needs a control. |
| `provider` | A vendor + model + auth record. |
| `seed` | Writes fixture rows -- a mutation with different intent, and the intent is what would need deciding. |
| `builtin` | The Go implementation behind a DSL name; reached through the tool or function that declares it. |

The absence of a Run control is the statement. Nothing is drawn disabled.

Design: [the Constructs view](../../superpowers/specs/2026-08-14-vscode-constructs-view-design.md).

## Data

The Data view lists every registered concept on the connected cluster,
grouped by domain, read from the engine's own registry. A concept added
to the DSL appears with no extension update.

> **This view was called Concepts** until memql#3754, and the old name was
> wrong from the start: it never showed a concept's definition, it showed
> rows. Definitions are the Constructs view above -- and a concept is
> itself a construct, which is what made the two names collide. Its
> commands moved with it: `memql.concepts.refresh` and
> `memql.concepts.open` are now `memql.data.refresh` and
> `memql.data.open`. There is no alias, so a keybinding or workspace
> `.vscode/` file naming the old ids needs updating.

Click a concept to open its browser tab: rows on the left, detail on the
right.

- Rows are labelled using whatever `@displayCard` slots the concept
  declares. A concept that declares none falls through to the stated
  fallback contract -- a title inferred from a `name` / `title` / `label`
  field, a status inferred from a lifecycle field, the row id when neither
  is present. See
  [display-cards.md](../concepts/display-cards.md).
- **Load more** pages through the keyset cursor; a concept larger than one
  page is fully walkable.
- Selecting a row shows its full nested shape -- payload, provenance and
  intrinsics -- with no flattening, so the intrinsics stay visible.
- The list updates live as rows are created, updated or deleted.

There is no concept-specific rendering anywhere in the panel. That is
deliberate: it is what makes a newly declared concept work the day it is
declared.

### One value viewer, everywhere

A row's detail, a run's result, and an automation trace's raw payload and
per-step output all render through the **same** viewer -- collapsing,
type-badged, filterable, and bounded so a payload too large to draw
renders as much as its budget allows and says it stopped. None of these
surfaces prints stringified JSON into a `<pre>`, and a guard test
(`editors/vscode/test/surfaceGuards.test.ts`) fails the build if one
starts to.

It lives in `sdk/ts-viewkit`, and the extension is its one consumer. The kit
is shared with nothing else today, so its element-fitting layer has no caller
(epic memql#4984).

## Beyond browsing

Executing constructs from a CodeLens or from the **Constructs** catalog,
running automations with a step trace, and driving deployments from the
**Deployments** view have since
landed alongside the views above. (Deployments replaced the Cluster tab in
memql#3733: topology is cluster state and belongs to MemQL OS, while what
you operate and what you can reach belong here. The extension's detailed
reference states that boundary and the table it produces.) Each has its own section in the
[manual verification checklist](vscode-runtime-panel-verification.md),
which is where the behaviour is written down in the detail a reader
checking one of them needs.
