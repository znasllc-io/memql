---
title: VS Code Runtime Panel
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# VS Code Runtime Panel

The memQL extension's activity-bar panel connects VS Code to a running
cluster: pick a cluster, browse every registered concept, and inspect
rows without leaving the editor.

Verifying a change to this panel: [VS Code Runtime Panel -- Manual Verification
Checklist](vscode-runtime-panel-verification.md), which also states what the
automated `make vscode-test-host` smoke lane covers and what it deliberately
leaves to a human.

## Requirements

- A **trusted** workspace. Language features (highlighting, diagnostics,
  completion, hover, signature help) work in an untrusted workspace; the
  runtime panel does not. It reads credentials and opens a network
  connection, which a malicious workspace must not be able to trigger.
- A cluster in `~/.memql/clusters.yaml` with an endpoint and an
  identity-issued JWT access token. A Personal Access Token does not work
  here and cannot -- see [Authentication](#authentication) below. If there is
  no cluster yet, the **+** installs one (see
  [Installing a local cluster](#installing-a-local-cluster)).

## Clusters

The panel reads the same `~/.memql/clusters.yaml` the memQL Cockpit uses,
so a cluster added in either tool appears in both. The file is watched:
an external edit refreshes the view.

Click a cluster to make it the working cluster. The selection persists to
`selected_cluster`, so the cockpit resumes on the same cluster.

| Icon | Meaning |
|---|---|
| Filled green circle | Connected |
| Spinner | Connecting |
| Red error icon | The cluster is the problem -- unreachable, or the connection died |
| Yellow key | The CREDENTIAL is the problem -- expired, missing, or the wrong class |
| Yellow warning | Not configured -- no endpoint |
| Hollow circle | Configured, not connected |

The key and the red dot are deliberately different pictures. "Your token
ran out" and "the cluster went away" have completely different next
actions, and rendering both as a red dot made the first one unreadable
(memql#3385).

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

The panel dials with the `token` field, which must hold an
**identity-issued JWT access token** -- the `access_token` from
`POST <identity>/oauth/token`.

**A Personal Access Token does not work here, and cannot.** PAT
verification is a database lookup wired only into the identity binary, so
every mesh node (bff, voice, cognition, agent, planner) rejects an
`mql_pat_...` bearer *before* looking anything up. The panel detects one
in the `token` field and refuses it by name rather than letting it fail as
an unexplained handshake error (memql#3383).

Access tokens are short-lived -- identity issues them with a 900-second
TTL. Set `refresh_token` alongside the token and the panel renews it
itself: proactively before each connect, and in place on a live stream via
the SDK's re-auth hook, so a long session never has to be re-credentialed
by hand (memql#3385).

The refresh token is a 30-day credential, so the panel does not leave it
in `clusters.yaml`. The `refresh_token` key is an **ingest path only**: on
the first successful exchange the rotated token is moved into VS Code's
`SecretStorage` and the plaintext key is deleted from the file. The access
token stays in the file, because it is short-lived and because the memQL
Cockpit shares this registry and needs to see it.

A cluster entry therefore looks like this:

```yaml
clusters:
  - name: local
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    issuer: https://identity.local.znas.io   # optional; derived from domain
    client_id: cockpit                       # optional
    token: <the access_token from /oauth/token>   # REQUIRED. A JWT, not a PAT.
    refresh_token: <ingest only -- moved to SecretStorage on first use>
    local: true
selected_cluster: local
```

`issuer` is where the refresh exchange is POSTed. When it is absent the
panel derives `https://identity.<domain>` (or the `identity.` sibling of a
`cockpit.<host>` endpoint); a cluster with neither is told which field to
supply rather than having a host guessed for it.

## Cluster lifecycle

Four verbs, reached from the Clusters view. Two of them sound alike and are
deliberately kept apart.

| Action | Where it lives | What it touches |
|---|---|---|
| **Add** | the **+** in the view title | Opens the Add a cluster page: install a local cluster, or register one that already exists elsewhere. |
| **Repair** | the same page, and the cluster panel's primary control when the cluster is not answering | Re-runs the install. Every step verifies before it acts and skips what is already satisfied, so a repair is the install graph and not a second one. |
| **Remove** | the trash can beside a row, inline | Drops the registry entry, the stored credential, and the live connection. Nothing on the machine is touched. |
| **Uninstall** | the row's context menu, on local clusters only | Reverses the install receipt: the k3d cluster, the hosts-file block, the mkcert CA, the pinned tools. |

**Remove and Uninstall are different commands on purpose.** Remove is a
routine, reversible act of editing a list: the cluster keeps running, its
data is untouched, and adding it back is a matter of supplying its endpoint
and a credential again. Uninstall takes a cluster off the machine, and there
is no undo -- a k3d cluster that has been deleted is gone with everything in
its database. Folding both into one action that then asks which you meant
would put the irreversible one click away from the routine one, so they are
separate commands with separate labels, separate menus and separate
confirmations. Remove confirms with a modal whose second line says what is
*not* happening; Uninstall confirms against an itemised dry run.

Uninstall is contributed only on rows the editor installed (`local: true`
with a receipt to reverse), and never as an inline icon. Aiming at the trash
can cannot land on it.

**The uninstall preview is the confirmation.** Before anything is removed
the page lists every artifact the receipt names, what will happen to it, and
what removing it will ask of you -- a step needing elevation says so before
you consent rather than surprising you with a password prompt mid-run.
Anything the install *found* rather than created is listed as preserved and
is not touched: an mkcert CA that predated the install belongs to the
machine, not to memQL, and guessing otherwise would break every other
locally-trusted certificate on it.

## Installing a local cluster

**The Add a cluster page is the supported path.** Press **+** in the Clusters
view and choose *Install a local cluster*. The page collects what an install
needs before any work starts -- the domain, who owns the cluster, and which AI
provider the key belongs to plus where that key is -- because a wizard that
stops to ask a question nine minutes in is a wizard people abandon. The
provider is a **choice** rather than a box: `anthropic` and `openai` are what
the installer can verify a key against, and it is pre-answered, so an operator
with no preference accepts the defaults and types four things. It then runs the install graph step by step, with
each step's state visible and each failure carrying the exit status and the
script's own stderr rather than a summary of it. A failed step can be retried
in place; cancelling still leaves a valid receipt, so a cancelled install is
still uninstallable. Where several steps in one wave fail -- which the executor
allows deliberately, so you see every failure rather than the first one and a
shrug -- each is explained on its own terms, because the exit codes ask for
genuinely different things: a refusal is the script protecting something, a
missing prerequisite is something to go and install.

Whatever the operator types, what reaches the install is a **file** and never
a command-line argument. Process arguments are world-readable in `ps`, so a
`--provider-key` flag would publish the key to every process listing on the
machine for as long as the install ran. That is why the CLI below takes
`--provider-key-file` and there is deliberately no flag that carries the key
itself.

**The CLI is the scripted alternative, and it is not deprecated.** For CI, an
unattended provisioning job, or any situation with no editor in it, the same
install runs from a terminal:

```bash
cd editors/vscode
npm run install-cli -- install --domain=<domain> --owner-email=<email> \
  --provider-key-file=<path>
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
([reproduce-staging-locally.md](../operate/reproduce-staging-locally.md)),
while the page installs onto a machine that need not have one.

## Concepts

The Concepts view lists every registered concept on the connected
cluster, grouped by domain, read from the engine's own registry. A
concept added to the DSL appears with no extension update.

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

## Beyond browsing

Executing constructs from a CodeLens, running automations with a step
trace, and driving deployments from the Cluster tab have since landed
alongside the views above. Each has its own section in the
[manual verification checklist](vscode-runtime-panel-verification.md),
which is where the behaviour is written down in the detail a reader
checking one of them needs.
