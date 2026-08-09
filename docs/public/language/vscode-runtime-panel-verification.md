---
title: VS Code Runtime Panel -- Manual Verification Checklist
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# VS Code Runtime Panel -- Manual Verification Checklist

The artifact a human works through before calling a change to the VS Code
runtime panel done.

## Why this exists even though there is an automated host lane

There are two verification lanes for this extension, and they answer different
questions.

| Lane | Command | Answers |
|---|---|---|
| Unit | `make vscode-test` | Does the logic compute the right answer? Fast, Electron-free, covers only modules that do not import `vscode`. |
| Host smoke | `make vscode-test-host` | Does the extension survive a real VS Code? Activation, command registration, the activity-bar contributions, the host runtime's WebSocket story, watching a path outside the workspace, webview creation. Runs against both the declared `engines.vscode` floor and current stable. |
| This checklist | A human, `F5` | Does it *work*, and does it *look right*, against a live cluster? |

The host smoke lane (memql#3302) exists because a whole class of defect passes
every unit test and fails only in a host -- an unguarded global dereference on
a runtime that lacks it, a file watcher that silently never fires. It caught
three instances of that class. What it deliberately does **not** do is dial a
cluster: it never connects, so everything downstream of a connection -- real
rows, a real run, a real deployment -- is unverified until someone runs it.
That is this document's job, and it is the reason the list below is longer than
the smoke lane, not shorter.

## Setup

Five steps, in order. Each one has stopped a reader before section 1 at least
once (memql#3386), so none of them is optional and none of them can be guessed
at from the outside.

### 1. Build the extension

```bash
make vscode-deps                              # NOT optional on a clean checkout
cd editors/vscode && npm ci && npm run compile
```

`make vscode-deps` builds `sdk/ts` and `sdk/ts-viewkit`. The extension consumes
both as `file:` dependencies whose `main` / `types` point into a `dist/` that
does not exist until they are built, so skipping this leaves the symlinks
resolving to nothing and the compile fails (memql#3340).

Then open `editors/vscode` in VS Code and press **F5**. In the Extension
Development Host, open a folder containing `.memql` files (the repo's `dsl/`
tree is the obvious choice).

The language features additionally need a `memql-lsp` binary: run
`make vscode-install` first so a platform binary is bundled, or set
`memql.lsp.serverPath` in **User Settings**. A workspace-scoped value
(`.vscode/settings.json`) is deliberately ignored, with a warning saying it was
-- an opened folder is not trusted to name an executable this extension then
runs. With no binary at all the extension still activates and every runtime
view on this checklist still works; only highlighting, diagnostics, completion,
hover and signature help are lost (memql#3387). The Run CodeLens is the one
run-surface item that does need it, because it reads the runnable constructs
from the server.

### 2. Bring up a cluster

```bash
make up
```

A live cluster is required from checklist section 2 onward.

### 3. Trust the local CA

The k3d front door (`cockpit.local.znas.io`, `identity.local.znas.io`)
terminates TLS with a `*.local.znas.io` wildcard signed by your machine's
mkcert CA. `make up` / `make secrets` issue and seed that certificate
automatically (memql#3384). Creating the CA itself is a separate one-time step,
because it writes to the system trust store:

```bash
brew install mkcert                                              # macOS; apt/dnf on Linux
bash scripts/install/mkcert-setup.sh --confirm=install-memql-ca  # only if you have no mkcert CA yet
```

**Node clients need the CA separately.** Node -- and therefore the VS Code
extension host -- does not read the OS trust store. Installing the CA
system-wide is necessary for browsers but not sufficient for the extension:

```bash
export NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem"
```

For the extension host this must be set in the environment VS Code was
**launched from**. A shell `export` after launch does not reach an
already-running window; relaunch VS Code from that shell.

WARNING: dropping a CA file into `/usr/local/share/ca-certificates/` is not
enough. On Debian/Ubuntu that directory is only staging until
`sudo update-ca-certificates` compiles the file into the system bundle -- and
even then Node still ignores the system store, so `NODE_EXTRA_CA_CERTS` remains
required. A CA "installed" by file placement alone fails for both
system-store consumers and Node, in two different ways, for two different
reasons.

Verify before debugging anything else:

```bash
echo | openssl s_client -connect cockpit.local.znas.io:443 -servername cockpit.local.znas.io 2>/dev/null \
  | openssl x509 -noout -issuer
# want: issuer=O = mkcert development CA, ...
# CN = TRAEFIK DEFAULT CERT means local-znas-tls is missing -- run `make secrets`
```

### 4. Get a credential

The extension dials with an **identity-issued JWT access token** -- the
`access_token` from `POST <issuer>/oauth/token`.

A Personal Access Token (`mql_pat_...`) or a worker token (`mql_wkr_...`)
cannot work here and is refused by name before the dial. PAT verification is a
database lookup wired only into the identity binary, so every mesh node rejects
one *before* looking anything up: a valid PAT fails exactly like a forged one
(memql#3383).

Sign in against identity to get the pair. The sign-in page is
`https://identity.<domain>/login` -- a cluster with no owner yet redirects to
`/setup`, which mints the first one. The browser sign-in is what authorizes the
code exchange; what you need out of the `/oauth/token` response is
`access_token` and `refresh_token`.

The cluster's own discovery document names every connection fact the entries
below need, so none of them has to be guessed:

```bash
curl -s https://identity.local.znas.io/.well-known/memql-config.json
# {"identityUrl":"https://identity.local.znas.io","grpcEndpoint":"cockpit.local.znas.io:443","clientId":"cockpit","clusterName":"local"}
```

`identityUrl` is `issuer`, `clientId` is `client_id`, and `grpcEndpoint` is
`endpoint` -- a bare `host[:port]` naming the front door, which is exactly the
form the extension accepts. It was not always: until memql#3399 this field read
`https://bff.local.znas.io`, a URL at a host with no ingress, and the two wrong
guesses in the note below are the ones it handed the reader. Both halves are now
pinned to one shared statement of the contract
(`test/fixtures/discovery-endpoint-contract.json`).

### 5. Write two cluster entries

Two, not one. Checklist section 3 asks you to verify that running a mutation raises a
modal confirmation, and a cluster marked `local: true` never prompts -- the
flag exists to let disposable data be written without a dialog. Verifying that
item against a local k3d cluster therefore needs a second entry pointing at the
same cluster *without* the flag.

`~/.memql/clusters.yaml` is shared with the memQL Cockpit. Add:

```yaml
clusters:
  - name: vscode-local
    display_name: local.znas.io (local)
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    issuer: https://identity.local.znas.io   # optional -- derived from domain when absent
    client_id: cockpit                       # optional -- sent as client_id on refresh
    token: <the access_token from /oauth/token>   # REQUIRED. A JWT, not a PAT.
    refresh_token: <the refresh_token from the same response -- ingest only>
    local: true
  - name: vscode-nonlocal
    display_name: local.znas.io (not local)
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    issuer: https://identity.local.znas.io
    client_id: cockpit
    token: <a SECOND access_token -- see below>
    refresh_token: <its matching refresh_token>
selected_cluster: vscode-local
```

Three things about those entries are not guessable, and each is a wrong guess
somebody has already made:

- **`endpoint` is a bare `host[:port]`, never a URL.** `connection/endpoint.ts`
  rejects a scheme outright: *endpoint scheme must be ws:// or wss://, got
  "https://"*. The natural thing to paste -- the `https://` domain the Cockpit
  uses, sitting in this same file -- is exactly what fails.
- **The host is `cockpit.<domain>`, not `bff.<domain>`.** There is no
  `bff.local.znas.io` ingress. The front door is `cockpit-front-door`, which
  routes `/memql/ws` and `/portal` to `bff-http:8085` and `/` to `bff:50051`.
  Targeting "the bff" gets a 404.
- **Absent `local` means NOT local.** Omit the key on the second entry rather
  than writing `local: false`; the Cockpit declares the field `omitempty` and
  drops a false on its next write, so the two tools would churn the file
  against each other. A quoted `local: "true"` is not accepted as true either.

**Give each entry its own token pair.** Sign in twice. The refresh exchange
*rotates* the refresh token -- the presented one is consumed, with only a
30-second grace window on the previous value -- so a pair shared between two
entries survives the first exchange and then stops working on the other.

### Credential expiry and renewal

Access tokens carry a **900-second TTL**. Renewal is not manual: set
`refresh_token` once and the extension renews the access token proactively
before each connect, and in place on a live stream via the SDK's re-auth hook,
so a long session is never re-credentialed by hand (memql#3385).

`refresh_token` then **disappears from the file, by design**. It is a 30-day
credential and this file is plaintext and shared, so the key is an ingest path
only: on the first successful exchange the rotated token moves into VS Code's
`SecretStorage` and the plaintext key is deleted. The access token stays in the
file -- it is short-lived, and the Cockpit needs to see it. A `refresh_token`
that has vanished after your first connect was taken into custody, not lost.

An expired credential renders as a **yellow key** with a `CREDENTIAL EXPIRED:`
tooltip -- deliberately a different picture from the red dot an unreachable
cluster gets, because "your token ran out" and "the cluster went away" have
completely different next actions.

Full narrative: [VS Code Runtime Panel](vscode-runtime-panel.md).

### Record what you verified against

"It worked" means little without these:

- [ ] VS Code version: ____________
- [ ] Extension commit: ____________
- [ ] Cluster: ____________

---

## 1. Panel basics (B1)

- [ ] The memQL icon appears in the activity bar and reads cleanly at 24x24
- [ ] Clusters lists both entries from `~/.memql/clusters.yaml`
- [ ] Selecting one connects, and the icon turns to a filled green circle
- [ ] Concepts lists domains, and expanding one lists its concepts
- [ ] Clicking a concept opens a tab, rows render, and **Load more** pages
      correctly
- [ ] Clicking a row shows its full nested detail -- payload, provenance and
      intrinsics, unflattened
- [ ] Inserting a row elsewhere (the Cockpit, or `psql`) updates the list with
      no manual refresh
- [ ] Editing `~/.memql/clusters.yaml` externally refreshes the Clusters tree
- [ ] In an **untrusted** workspace: language features still work, and the
      runtime views do not appear

WARNING: the external-edit item is worth doing deliberately rather than
skimming. It is the item that was broken twice, both times silently, and both
times while every automated test was green.

## 2. Cluster registry editing (B1)

- [ ] **Add Cluster** collects name, domain, endpoint, access token, refresh
      token and the local flag, and the new cluster appears in the tree
- [ ] The local-flag step is a two-option pick that says what the flag DOES,
      and a new cluster defaults to "not local"
- [ ] Adding a cluster whose name collides with an existing one is refused,
      not silently turned into an edit
- [ ] **Edit Cluster** with the name field changed renames the existing entry
      rather than appending a second one
- [ ] Clearing the token field in **Edit Cluster** removes the key from
      `clusters.yaml` rather than leaving the old credential on disk
- [ ] Comments and unknown fields already in `clusters.yaml` survive a write
      (the Cockpit shares this file)
- [ ] A cluster with no endpoint shows the yellow warning "not configured"
      state; one with no credential, or a `mql_pat_` / `mql_wkr_` value in
      `token`, shows the yellow key and names the wrong class BEFORE any dial
- [ ] A failed connection shows the red error icon with the message on hover,
      and an expired credential shows the yellow key with `CREDENTIAL EXPIRED:`
      -- the two are different icons
- [ ] After the first successful connect, `refresh_token` is gone from
      `clusters.yaml` and the session keeps working (custody moved to
      SecretStorage)
- [ ] A session left running past 900 seconds does not drop -- the access token
      is renewed without a reconnect
- [ ] **Disconnect** returns the icon to the hollow circle and empties Concepts

### Sign-in, sign-out and the "+" menu (memql#3401)

These items exercise the browser sign-in the extension now drives itself, so
they are the one part of this checklist you can run **without** hand-pasting a
token into `clusters.yaml`. Take one cluster entry through them with its `token`
and `refresh_token` keys deliberately blank.

- [ ] **memQL: Sign In** (the cluster's context menu, or the palette) opens a
      cancellable progress notification reading `signing in to <cluster>` and
      opens a browser at the cluster's `identity.<domain>` login page
- [ ] Signing in there returns to VS Code without a manual paste, and the tree's
      yellow key clears
- [ ] `clusters.yaml` now carries a `token:` and a `client_id:` for that cluster,
      and does **not** carry a `refresh_token:` -- custody moved to SecretStorage
- [ ] Cancelling the progress notification mid-flow shows nothing louder than a
      cancellation message: no red error toast, and no credential is written
- [ ] Selecting a cluster whose dial fails **on the credential** offers a
      **Sign in** button on the error toast; one that fails because it is
      unreachable, or which names neither an `issuer` nor a `domain`, offers
      only the message -- a button whose sole outcome is another error is not
      shown
- [ ] **memQL: Sign Out** removes both `token:` and `refresh_token:` from
      `clusters.yaml`, drops a live connection to that cluster, and says
      `Run "memQL: Sign In" to authenticate again`
- [ ] After Sign Out the tree shows the yellow key ("no credential"), not the red
      error dot
- [ ] **Refresh after expiry:** with a signed-in cluster connected, wait past the
      access token's 900-second TTL (or shorten
      `MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS` on the identity Deployment and
      re-`make dev NODE=identity`). The stream stays up and the views keep
      loading -- the token is renewed in place, with no reconnect and no prompt
- [ ] Delete the cluster's refresh-token secret (sign out, then hand-write only a
      long-expired `token:` back into `clusters.yaml`) and connect: the tree
      shows the yellow key with `CREDENTIAL EXPIRED:` and the offered recovery is
      Sign In
- [ ] The **"+"** in the Clusters view title branches on evidence: with no
      install receipt and no `local: true` cluster it offers **Install a local
      cluster...** alongside **Connect to an existing cluster...**; with a
      local cluster present and answering it goes straight to the connect form
      with no picker; with one present but not answering it offers **Repair
      local cluster...** as well
- [ ] Choosing Install or Repair shows the installer's CLI command with a **Copy
      Command** button and says an in-editor wizard is not wired up yet -- it
      does not silently do nothing
- [ ] **memQL: Sign In With a Device Code** (palette only) shows an
      `XXXX-XXXX` code and a verification URL, and approving at
      `https://identity.<domain>/device` completes the sign-in on the editor side

WARNING: two things this section deliberately does not ask you to verify,
because they are not wired up and the row would fail:

- The automatic loopback-to-device-code fallback exists in the codebase
  (`signInWithDeviceCodeFallback`) but is **not** reached from **memQL: Sign
  In**, which runs the loopback flow alone. The device grant is reachable only
  through the explicit **Sign In With a Device Code** command. Verify it that
  way; do not wait for a failed loopback to hand you one.
- **Renaming a signed-in cluster does not move its refresh token.**
  `renameClusterCredentials` and `reconcileClusterCredentials` both exist and
  neither is called from the extension, so a rename leaves the secret orphaned
  under the old key and nothing sweeps it. Sign in again after a rename.

## 3. Running a construct (B2, memql#3309)

Open a `.memql` file with a runnable construct (a query is the easiest).

- [ ] A **Run** CodeLens renders above the construct's signature, and a
      **Run with...** lens beside it
- [ ] The lens tooltip names what will actually run
- [ ] A `@disabled` construct's lens reads **Run (@disabled)**, and its tooltip
      names the remedy rather than only the condition (memql#3333)
- [ ] Nothing runs on open and nothing runs on save -- the lens is an
      affordance, and only a click fires it
- [ ] **Run** on a no-argument construct executes and opens a result tab
- [ ] Result rows render through each concept's own `@displayCard`, and a
      concept with none falls back to the row id
- [ ] Clicking a result row opens it in the Concepts surface
- [ ] **Run with...** opens the argument form, with a field per declared arg
      and the declared types enforced
- [ ] An `@autoInjected` field is marked individually, with a per-field caption
      -- there is no blanket form-level notice disclaiming the whole form
      (memql#3333)
- [ ] A required arg left blank is reported in the form rather than sent
- [ ] Running a **mutation** against `vscode-nonlocal` raises a modal
      confirmation naming the cluster and the construct, and dismissing it
      cancels the run
- [ ] The same mutation against `vscode-local` runs with no prompt -- that is
      what the `local` flag is for, and it is why the two entries exist
- [ ] Editing the construct in the buffer and re-running runs the EDITED
      definition, not the deployed one (the result banner says which)
- [ ] Editing a shape in one buffer and running a query that imports it from
      another runs against the EDIT -- the transitive dependency is walked via
      the `memql/imports` LSP request, not a regex (memql#3335)
- [ ] The same, with the `use` line inside a `/* */` block comment: the
      commented-out import is NOT followed (the old regex scan got this wrong)
- [ ] Disconnecting and reconnecting, then re-running, still runs the edited
      definition rather than silently falling back to the deployed one

### Diagnostics mapping

- [ ] Introduce a syntax error in the construct, then run: the engine's
      diagnostics land in the **Problems** panel against the right file and the
      right line
- [ ] A diagnostic the engine could not position lands as a file-level problem
      on the active file, not parked on line 1 of some unrelated dependency
- [ ] Fixing the error and re-running clears them
- [ ] Typing in the buffer does NOT clear a run's diagnostics (they are a
      separate collection from the language server's)

### Saved run configurations

- [ ] Saving a named configuration from the arg form writes
      `.memql/runs.json` in the workspace
- [ ] The **Runs** view lists it
- [ ] Its inline play button re-runs it with the saved arguments
- [ ] Its inline delete button removes it, from the view and from the file
- [ ] **Open Run Configurations File** opens `.memql/runs.json`
- [ ] Editing that file by hand and hitting Refresh shows the change
- [ ] Re-running a saved configuration whose construct no longer exists fails
      with a legible message rather than a stack trace

## 4. Running an automation (B3, memql#3310)

Open a file containing an `automation`.

- [ ] A **Run automation...** CodeLens renders above it
- [ ] The form opens on the mode the trigger implies: `schedule` for a
      `@trigger(schedule=...)`, `row` for a concept-triggered one, `json`
      otherwise
- [ ] The form states in one sentence what the run will fire
- [ ] In **row** mode the picker lists real rows of the trigger concept, and
      **Load more** pages
- [ ] Picking a row and running fires the automation against it
- [ ] In **json** mode a malformed payload is reported in the form rather than
      sent
- [ ] An automation whose trigger names no concept does not offer the row
      picker at all

### The step trace

- [ ] Running opens the trace tab beside the form without stealing focus
- [ ] Steps appear as they complete, ordered by sequence, with per-step timing
- [ ] A failing automation shows the failing step, and the timeline stays
      intact
- [ ] A REFUSED run (unknown name, `@disabled`, a `@filter` miss, wrong role)
      reads as a refusal -- "it never started" -- and not as a failed run
- [ ] Refusing a run of an automation the language server reported as
      `@disabled` says so OUTRIGHT, and says the `@filter` was never consulted
      -- it does not offer `@disabled` and a `@filter` miss as both-possible
      (memql#3333, memql#3339). The engine answers both with the same code, so
      this is the buffer's knowledge being used, not the engine's
- [ ] The raw toggle shows the underlying frames
- [ ] Toggling raw mid-run is not undone by the next step landing
- [ ] Saving the automation run as a configuration, then re-running it from the
      Runs view, refills the form and fires the same payload

## 5. The Cluster tab (B4, memql#3312)

Open it from the Clusters tree's inline action, or from the palette
(**memQL: Open Cluster Topology and Deployments**).

- [ ] The tab opens against the selected cluster and is titled with its name
- [ ] **Topology** shows one tile per registered node: label, short id,
      advertised address, version, deployment, health
- [ ] The replica tally shows one row per node type, and a short tier is
      flagged as under-replica
- [ ] A tier with no declared replica count reads "N running", never "N of 0"
- [ ] An orphaned node is marked as such, with its reason
- [ ] **Deployment history** lists deployments newest first, with status and
      shortened digests
- [ ] Selecting a deployment shows its per-node-type specs
- [ ] The view updates live as a deployment progresses -- no manual refresh
- [ ] **Actions** shows only what your role permits (Cut version / Deploy /
      Promote / Roll back / Rollout promote-abort), and the hidden ones say why
- [ ] A destructive action requires typing its confirmation phrase, and a
      mismatch refuses
- [ ] Disconnecting shows the "not connected" state and the live-updates-offline
      notice rather than stale data
- [ ] Switching to a different cluster repaints the tab from the new cluster
      rather than leaving the old one's data on screen

## 6. Cross-cutting

- [ ] Open every tab type at once, then **Developer: Reload Window**: the
      extension comes back clean with no errors in the Extension Host log
- [ ] Close every tab: no "disposed" errors in the log
- [ ] Switch between `vscode-local` and `vscode-nonlocal` with several tabs
      open: every tab repaints against the new cluster, and none shows the
      previous cluster's rows
- [ ] Stop the cluster mid-run: the failure is reported, and the panel does not
      wedge
- [ ] Nothing anywhere renders a credential -- not the access token, not the
      refresh token, not in a tab, a tooltip, or the Extension Host log

## When something on this list fails

If the failure is of the host-only class -- nothing throws in the unit tests,
the feature is just silently dead -- consider whether the automated host lane
could have caught it, and add the case if so. That lane lives in
`editors/vscode/test-host/`; the three defects it already guards against are
documented in its header, and each of them was found the same way: a human
working through a list like this one.
