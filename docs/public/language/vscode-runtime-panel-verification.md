---
title: Visual Studio Code and Cursor Runtime Panel -- Manual Verification Checklist
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# Visual Studio Code and Cursor Runtime Panel -- Manual Verification Checklist

The artifact a human works through before calling a change to the MemQL
runtime panel in Visual Studio Code and Cursor done.

## Why this exists even though there is an automated host lane

There are two verification lanes for this extension, and they answer different
questions.

| Lane | Command | Answers |
|---|---|---|
| Unit | `make vscode-test` | Does the logic compute the right answer? Fast, Electron-free, covers only modules that do not import `vscode`. |
| Host smoke | `make vscode-test-host` | Does the extension survive a real VS Code? Activation, command registration, the activity-bar contributions, the host runtime's WebSocket story, watching a path outside the workspace, webview creation. Runs against both the declared `engines.vscode` floor and current stable. |
| Host smoke, live half | `make vscode-test-host` with `MEMQL_HOST_SMOKE_CLUSTERS_FILE` set | Does the half downstream of a connection work at all? Dials a real cluster and does the READ-ONLY things: connect, list concepts, page rows, resolve a row's detail, validate a bundle and map its diagnostics, read the deployment and node-spec concept sets. Skips, loudly, when no registry is configured. |
| This checklist | A human, `F5` | Does it *work*, and does it *look right*, against a live cluster? |

The host smoke lane (memql#3302) exists because a whole class of defect passes
every unit test and fails only in a host -- an unguarded global dereference on
a runtime that lacks it, a file watcher that silently never fires. It caught
three instances of that class.

Its live half (memql#3337) then dials a cluster and exercises the read-only
part of what comes after, so that "the connection, the paging or the
diagnostics mapping is broken" is found by a command rather than by a human an
hour into this list. It is **read-only on purpose**: it runs no mutation,
session-defines nothing and deploys nothing, because an automated lane pointed
at whatever cluster an operator had selected must not be able to write to it.

What neither lane can do is look at anything. "The icon turns to a filled green
circle", "rows render through each concept's `@displayCard`", "the trace tab
opens beside the form without stealing focus" are not assertions a process can
make about itself, and neither is any item that needs a write. That is this
document's job, and it is why the list below is longer than both lanes, not
shorter.

Run the live half before you start:

```bash
MEMQL_HOST_SMOKE_CLUSTERS_FILE=~/.memql/clusters.yaml \
MEMQL_HOST_SMOKE_CLUSTER=vscode-local \
NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem" \
  make vscode-test-host
```

## Setup

### The short path

```bash
scripts/vscode/verification-setup.sh --install-tools
```

That is steps 1 to 5 below, in order: the pinned tools, the local CA, the
extension and `memql-lsp` built, `make up`, the TLS check that memql#3384 was
filed over, the discovery read that memql#3399 was filed over, two device-code
sign-ins, and both cluster entries written through the extension's own
`clusters.yaml` writer. It verifies nothing on this checklist -- it ends where
your work begins.

Read the five steps anyway if it fails: each one says what it is for, and the
script's failure messages assume you have.

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

The k3d front door (`api.memql.localhost`, `identity.memql.localhost`)
terminates TLS with a `*.memql.localhost` wildcard signed by your machine's
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
echo | openssl s_client -connect api.memql.localhost:443 -servername api.memql.localhost 2>/dev/null \
  | openssl x509 -noout -issuer
# want: issuer=O = mkcert development CA, ...
# CN = TRAEFIK DEFAULT CERT means memql-front-door-tls is missing -- run `make secrets`
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
curl -s https://identity.memql.localhost/.well-known/memql-config.json
# {"identityUrl":"https://identity.memql.localhost","grpcEndpoint":"api.memql.localhost:443","clientId":"cockpit","clusterName":"local"}
```

`identityUrl` is `issuer` and `grpcEndpoint` is `endpoint` -- a bare
`host[:port]` naming the front door. `clientId` names the Cockpit's own client;
the editor always signs in as `memql-vscode` and does not read it. It was not
always so: until memql#3399 this field read `https://bff.memql.localhost`, a URL
at a host with no ingress, and the wrong host in the note below is the one it
handed the reader. Both halves are now pinned to one shared statement of the
contract (`test/fixtures/discovery-endpoint-contract.json`).

### 5. Write two cluster entries

Two, not one. Checklist section 3 asks you to verify that running a mutation raises a
modal confirmation, and a cluster marked `local: true` never prompts -- the
flag exists to let disposable data be written without a dialog. Verifying that
item against a local k3d cluster therefore needs a second entry pointing at the
same cluster *without* the flag.

`~/.memql/clusters.yaml` is shared with the MemQL Cockpit. Add:

```yaml
clusters:
  - name: vscode-local
    display_name: memql.localhost (local)
    domain: memql.localhost
    endpoint: api.memql.localhost:443
    issuer: https://identity.memql.localhost   # optional -- derived from domain when absent
    token: <the access_token from /oauth/token>   # A JWT, not a PAT -- ingest only
    refresh_token: <the refresh_token from the same response -- ingest only>
    local: true
  - name: vscode-nonlocal
    display_name: memql.localhost (not local)
    domain: memql.localhost
    endpoint: api.memql.localhost:443
    issuer: https://identity.memql.localhost
    token: <a SECOND access_token -- see below>
    refresh_token: <its matching refresh_token>
selected_cluster: vscode-local
```

Three things about those entries are not guessable, and each is a wrong guess
somebody has already made:

- **`endpoint` is a `host[:port]` or an `https://` address, and nothing
  else.** Both name the same front door: `connection/endpoint.ts` maps
  `https://` (and `http://`) to its socket twin on the same origin, which is
  why the `https://api.memql.localhost` the Cockpit writes in this same file
  dials. Any other scheme is refused in plain words. Until 0.6.2 a URL form was
  refused outright, so a cluster the Cockpit had written could be signed in to
  and then not connected to.
- **The host is `api.<domain>`, not `bff.<domain>`.** There is no
  `bff.memql.localhost` ingress. The front door is `api-front-door`, which
  routes the HTTP paths -- `/memql/ws`, `/healthz` -- to
  `bff-http:8085` and `/` to `bff:50051`. Targeting "the bff" gets a 404.
- **Absent `local` means NOT local.** Omit the key on the second entry rather
  than writing `local: false`; the Cockpit declares the field `omitempty` and
  drops a false on its next write, so the two tools would churn the file
  against each other. A quoted `local: "true"` is not accepted as true either.

**Give each entry its own token pair.** Sign in twice. (Signing in through the
extension instead writes nothing here: the tokens go to `SecretStorage`, keyed
by the entry's name, and the hand-pasted keys are not needed.) The refresh exchange
*rotates* the refresh token -- the presented one is consumed, with only a
30-second grace window on the previous value -- so a pair shared between two
entries survives the first exchange and then stops working on the other.

### Credential expiry and renewal

Access tokens carry a **900-second TTL**. Renewal is not manual: set
`refresh_token` once and the extension renews the access token proactively
before each connect, and in place on a live stream via the SDK's re-auth hook,
so a long session is never re-credentialed by hand (memql#3385).

`token` and `refresh_token` then **disappear from the file, by design**. The
editor keeps its credentials in VS Code's `SecretStorage` -- the refresh token
is a 30-day credential and this file is plaintext and shared -- so both keys
are an ingest path only: on the first successful exchange the rotated tokens
move into `SecretStorage` and the plaintext keys are deleted. Keys that have
vanished after your first refresh were taken into custody, not lost. The
Cockpit keeps its own sign-in and is not affected.

An expired credential renders as a **yellow key** with *Your session ended.* in
its tooltip -- deliberately a different picture from the red dot an unreachable
cluster gets, because "your token ran out" and "the cluster went away" have
completely different next actions.

Full narrative: [Visual Studio Code and Cursor Runtime Panel](vscode-runtime-panel.md).

### Record what you verified against

"It worked" means little without these:

- [ ] Editor and version (Visual Studio Code or Cursor): ____________
- [ ] Extension commit: ____________
- [ ] Cluster: ____________

---

## 1. Panel basics (B1)

- [ ] The MemQL icon appears in the activity bar and reads cleanly at 24x24
- [ ] Clusters lists both entries from `~/.memql/clusters.yaml`
- [ ] Clicking one connects, the icon turns to a filled green circle, and the
      row is marked as the cluster in use
- [ ] Data lists domains, and expanding one lists its concepts
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

- [ ] The **"+"** opens the **Add a cluster** page (its own subsection below),
      and registering a cluster that already exists is its *Connect to a
      cluster* choice -- the fields are collected in that page's own form, not
      by a chain of editor prompts
- [ ] Registering a cluster whose name collides with an existing one is
      refused, not silently turned into an edit
- [ ] **Edit Cluster** collects name, domain, endpoint, access token, refresh
      token and the local flag through the editor's own inputs, and the edited
      cluster keeps its place in the tree
- [ ] Edit's local-flag step is a two-option pick that says what the flag DOES
- [ ] **Edit Cluster** with the name field changed renames the existing entry
      rather than appending a second one
- [ ] Clearing the token field in **Edit Cluster** removes the key from
      `clusters.yaml` rather than leaving the old credential on disk
- [ ] Comments and unknown fields already in `clusters.yaml` survive a write
      (the Cockpit shares this file)
- [ ] A cluster with no endpoint shows the yellow warning and **Not set up**;
      one with no credential, or a `mql_pat_` / `mql_wkr_` value in `token`,
      shows the yellow key and names the wrong class BEFORE any dial
- [ ] A failed connection shows the red error icon with the message on hover,
      and an expired credential shows the yellow key with *Your session
      ended.* -- the two are different icons
- [ ] After the first refresh, `token` and `refresh_token` are gone from
      `clusters.yaml` and the session keeps working (custody moved to
      SecretStorage)
- [ ] Stop the cluster (or drop the network) under a live connection: the row
      reads **Connecting** while the extension retries, for about two minutes,
      then **Can't reach** (or **Not running**) with a notification offering
      **Reconnect**. Bring the cluster back within the window and it reconnects
      with no click
- [ ] Reload the window with a signed-in cluster in use: it connects again with
      no click
- [ ] A session left running past 900 seconds does not drop -- the access token
      is renewed without a reconnect
- [ ] **Disconnect** clears the green icon and empties Constructs and Data

### Sign-in and sign-out (memql#3401)

These items exercise the browser sign-in the extension now drives itself, so
they are the one part of this checklist you can run **without** hand-pasting a
token into `clusters.yaml`. Take one cluster entry through them with its `token`
and `refresh_token` keys deliberately blank.

- [ ] **Sign in** (the row's inline act, its context menu, the cluster page or
      the palette) puts no dialog in front of the browser: one cancellable
      progress notification, **MemQL: Signing in to <cluster>**, whose line
      moves through *Opening your browser*, *Waiting for you in the browser*
      and *Finishing sign-in*, and a browser at the cluster's sign-in page.
      With MemQL OS already signed in there, it finishes without typing
- [ ] The browser tab says **You're signed in** only once the editor has the
      session; a sign-in that fails after the callback says **Sign-in didn't
      finish** instead
- [ ] With a `client_id: cockpit` line in the entry (the Cockpit writes one),
      sign-in and refresh still work -- the editor signs in as itself and never
      uses another tool's client
- [ ] Signing in there returns to the editor without a manual paste, and the tree's
      yellow key clears
- [ ] `clusters.yaml` carries no `token:`, `refresh_token:` or `client_id:` for
      that cluster afterwards -- the editor's sign-in lives in SecretStorage
- [ ] Cancelling the progress notification mid-flow shows nothing louder than a
      cancellation message: no red error toast, and no credential is written
- [ ] Clicking a cluster whose credential is missing or expired opens its page
      with **Sign in** as the button, and the row carries **Sign in** inline; a
      cluster that is unreachable offers **Retry**, and one that names neither
      an `issuer` nor a `domain` offers **Edit** -- a button whose sole outcome
      is another error is not shown
- [ ] **MemQL: Sign Out** removes what the editor stored for that cluster,
      drops a live connection to it, and says *Signed out of <cluster>.*
- [ ] After Sign Out the tree shows the yellow key and **Sign in**, not the red
      error dot
- [ ] **Refresh after expiry:** with a signed-in cluster connected, wait past the
      access token's 900-second TTL (or shorten
      `MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS` on the identity Deployment and
      re-`make dev NODE=identity`). The stream stays up and the views keep
      loading -- the token is renewed in place, with no reconnect and no prompt
- [ ] Delete the cluster's refresh-token secret (sign out, then hand-write only a
      long-expired `token:` back into `clusters.yaml`) and connect: the tree
      shows the yellow key with *Your session ended.* and the offered recovery
      is **Sign in**
- [ ] **MemQL: Sign In With a Code** (the palette, the row's context menu, or
      **Sign in with a code** on the cluster page) opens the approval page with
      the `XXXX-XXXX` code pre-filled, keeps the code and verification URL on
      the progress line, and shows exactly ONE action message (**Copy code** /
      **Open page**) that does NOT reappear after a button is clicked
      (memql#4595). Approving at
      `https://identity.<domain>/device` completes the sign-in on the editor
      side
- [ ] **MemQL: Sign In** falls back to a device code on a host that cannot do
      loopback (memql#3515). The trigger set is the pre-browser-open
      limitations only -- a refused loopback bind, or no browser at all
      (memql#4594); a firewall that refuses the bind is the cheapest
      arrangement. The switch shows on the progress line
      (*Switching to a code*) and the ONE action message explains it
      (memql#4595) -- there is no separate "falling back" toast -- and then the
      same `XXXX-XXXX` code appears. A host that *can* do loopback must still
      open a browser: the fallback firing unconditionally would be its own
      defect
- [ ] **A slow browser sign-in is NOT abandoned** (memql#4594): start
      **MemQL: Sign In** and leave the browser page unfinished. After 30
      seconds a notification (*Still waiting for your browser...*) and the
      cluster page offer **Use a code instead**; NO device code appears unless
      you take it, the progress notification stays, and completing the page at
      minute nine still signs the editor in. Taking the offer runs the code
      beside the browser, and whichever finishes first signs you in. Only past
      ten minutes does the browser wait end, as a warning offering **Use a code
      instead**
- [ ] Against a cluster that does not accept the editor's client (an engine
      older than the built-in client), sign-in fails within seconds, before a
      browser page is left on "Bad Request", with a sentence saying the cluster
      doesn't accept sign-in from VS Code
- [ ] Renaming a signed-in cluster (**MemQL: Edit Cluster**, change the name)
      leaves it signed in (memql#3515). Rename it, then reconnect: no
      credential prompt. The stranded half is invisible by construction --
      SecretStorage cannot be enumerated and the access token rides on the
      entry -- so the check that bites is reconnecting **after the access token
      expires** (15 minutes), where a lost refresh token surfaces as a
      re-authorization the rename should not have caused

WARNING: one thing this section deliberately does not ask you to verify:

- The credential **sweep** (`reconcileClusterCredentials`) runs at activation
  and deletes SecretStorage entries whose cluster is no longer in
  `clusters.yaml`. It is correct that you cannot observe it -- SecretStorage
  cannot be enumerated, so there is no surface that shows an orphan before or
  after. Its behaviour is covered by unit tests
  (`editors/vscode/test/authStore.test.ts`); what you can check here is only
  that a rename does **not** trip it, which is the row above.

### The Add a cluster page and the cluster lifecycle (memql#3463)

The **"+"** opens one page -- its tab reads **Add a cluster**, and **Install
MemQL**, **Repair MemQL** or **Uninstall MemQL** while it is doing that. Every
screen is the page kit: a head, the body, and a bar at the bottom with the
state in words and at most three acts, one of them a button.

The landing wants four machine states; run them in the order below and you get
them from one cluster.

- [ ] While the page is looking at the machine the landing is the grey shape of
      a list, with no sentence and no choice -- never "installed but not
      answering" before it has looked
- [ ] With nothing local: **Install MemQL on this computer** and **Connect to a
      cluster**, and nothing else. On an unsupported computer: one sentence
      and **Connect to a cluster**
- [ ] Pressing **"+"** a second time reveals the page already open rather than
      opening a second one
- [ ] **Connect to a cluster** asks for a name and a domain; the line under the
      domain reads `Connects to api.<what-you-typed>:443` as you type.
      **More options** holds the endpoint (prefilled) and the access token
- [ ] Typing `memql.localhost` (or `localhost`, or `127.0.0.1`) as the domain is
      refused, and the message sends you back to the local cluster
- [ ] Escape leaves an EMPTY connect form; in a half-filled one it does nothing
      and what you typed is still there. **Cancel** discards
- [ ] **Connect** on an unreachable domain warns with the address and the
      reason, writes nothing, and the button becomes **Add anyway**
- [ ] A cluster that is added stays on the page with its address and **Sign
      in** as the one button -- no toast
- [ ] **Install MemQL** shows First name, Last name and Email; Domain and
      Version sit behind **More options**, whose summary reads
      `memql.localhost · Latest (vX.Y.Z)`. Checks below list Installer (and
      Your password, when one will be asked) with one word each
- [ ] Version, under More options, lists releases newest first with the first
      reading `Latest (vX.Y.Z)` and selected; the last reads **Build from
      source (main, slower)**. With no network it is a text box prefilled with
      the pinned release
- [ ] Install asks for your computer password ONCE, titled **MemQL needs your
      password**. Pressing Escape on that prompt returns to the form with
      everything you typed and runs nothing
- [ ] The run screen is the mark, **Installing MemQL**, a bar that keeps moving
      through the cluster step, one short status line ("Creating the
      cluster", "Starting services 5 of 9") and `Step n of m · m:ss`. No step
      checklist. The bar at the bottom reads **Installing** with **Cancel**
- [ ] **Show logs** opens a live log in the order lines arrived, each step's
      label once where its lines begin; scrolling up stops the follow and
      reaching the bottom resumes it. **Copy** and **Open in Output** sit on
      its header. Typing and selecting text are not interrupted while it runs
- [ ] **Cancel** changes the bar to **Stopping after the current step** with
      nothing to press, and only once the step finishes to **Not finished**
      with **Back** and **Resume**
- [ ] Break a step (stop Docker): the status reads **Couldn't check Docker**,
      one notice says what the script said and what to do, the log opens AT
      that step, and -- only once every other step has finished -- the bar
      offers **Cancel** and **Retry**. A step that needs the password it was
      refused shows the command with **Run in terminal**, which types it into
      a terminal without pressing Enter
- [ ] The done screen: **MemQL is installed**, Address and MemQL OS, the
      recovery key masked with **Show** and **Copy** (Copy works without
      Show, and the button then reads **Copied**), and **Sign in** as the one
      button, with **Set up a passkey** beside it when the owner has no
      passkey yet, and **Back** as a text act. No modal appears over it
- [ ] **Back** on the done screen with the key not copied asks first; leaving
      lands on the landing, which offers **Sign in** / **Repair** /
      **Uninstall** for the cluster just built -- never Install again
- [ ] Closing the tab with the key not copied warns that it can't be shown
      again and that an owner can replace it later -- it names no screen
- [ ] With a local cluster installed and in the list, the landing offers **Sign
      in** (or **Open MemQL OS** when signed in), **Repair** and **Uninstall**,
      and never Install. Stop it (`k3d cluster stop memql`): Repair moves first
- [ ] Remove it from the list: the landing offers **Connect to it**, which adds
      it with nothing typed and opens the sign-in
- [ ] A `make up` cluster with no install record offers **Connect to it** and
      **Uninstall**, and -- listed or not -- no Repair
- [ ] **Repair Local Cluster** from the Clusters or Deployments menu on a
      machine with nothing installed opens the landing, not a repair form

Remove and Uninstall are the pair to check most carefully, because the risk in
this surface is reading one as the other:

- [ ] **Remove Cluster From List**, in the last group of a row's context menu,
      is **Remove**, and its confirmation spends its one line on what does NOT
      happen: the cluster keeps running on this computer (a local one), or
      nothing on the cluster changes (a remote one)
- [ ] Remove drops the entry from `clusters.yaml`, deletes the stored
      credential, and disconnects if that cluster was the live connection
- [ ] Removing the selected cluster clears `selected_cluster` rather than
      leaving it pointing at a name that no longer exists
- [ ] **Uninstall** appears in the Deployments view's title menu with a
      `local: true` cluster selected, is NOT an inline icon anywhere, and does
      NOT appear with a remote cluster selected -- nor on any Clusters row
      (memql#3742 moved it off Clusters; memql#4426 moved it off the Deployments
      instance row, which is gone)
- [ ] Uninstall opens **Uninstall MemQL**: what will be removed, by name
      (The cluster, Downloaded MemQL files, Local addresses -- "Asks for your
      password"), what is kept and why, and no path with your home directory
      in it
- [ ] k3d, kubectl, the local certificate authority and mkcert are SWITCHES,
      all off. mkcert is unavailable, with a line saying so, until the
      certificate authority is on; turning the authority off turns mkcert off
- [ ] On a cluster MemQL did not create (a `make up` one, with or without a
      list entry), the cluster is listed as kept and **Delete the cluster's
      data** is a red switch. Turning it on shows the `delete memql data`
      field, which says **Doesn't match yet** as you type; the bar offers
      **Uninstall and delete data** only once it matches, and the cluster moves
      to **Will be removed**
- [ ] **Cancel** and reopening Uninstall brings every switch back OFF and the
      phrase empty
- [ ] Make `~/.memql/install-receipt.json` unreadable (a copy with a stray
      character): Uninstall says **Couldn't work out what would be removed**
      with **Back**, **Open in Output** and **Try again** -- never "No local
      cluster was found", and never **Remove from list**. Put the file back
      and **Try again** shows the list; an Install against the same broken
      file says **The install couldn't start** with the detail in its log,
      rather than sitting on "Starting"
- [ ] Escape on the password prompt removes nothing and returns to the list
- [ ] Anything the install FOUND rather than created is listed as kept and is
      still there afterwards
- [ ] After the uninstall the cluster is gone from the tree as well as from
      the machine

One property is worth checking rather than assuming:

- A wave with several failures explains EACH of them (memql#3474): one notice
  per failed step, each opening with its own "Couldn't ..." -- and the status
  line leads with the earliest, on the ground that the others may be
  consequences of it.

## 3. Running a construct (B2, memql#3309)

Open a `.memql` file with a runnable construct (a query is the easiest).

- [ ] One run CodeLens renders above the construct's signature: **Run** for a
      construct with no arguments, **Run...** (which opens the argument form)
      for one with any
- [ ] The lens tooltip names what will actually run
- [ ] A `@disabled` construct's lens reads **Run (disabled)**, and its tooltip
      names the remedy rather than only the condition (memql#3333)
- [ ] Nothing runs on open and nothing runs on save -- the lens is an
      affordance, and only a click fires it
- [ ] **Run** on a no-argument construct executes and opens a result tab
- [ ] Result rows render through each concept's own `@displayCard`, and a
      concept with none falls back to the row id
- [ ] Clicking a result row opens it in its concept's rows page (the one the
      Data view opens)
- [ ] **Run...** opens the argument form, with a field per declared arg and the
      declared types enforced
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
- [ ] **Delete Saved Run...** in its context menu asks first, then removes it,
      from the view and from the file
- [ ] **Edit Saved Runs** (the view's title bar) opens `.memql/runs.json`
- [ ] Editing that file by hand shows the change in the view with no refresh
- [ ] Re-running a saved configuration whose construct no longer exists fails
      with a legible message rather than a stack trace

## 4. Running an automation (B3, memql#3310)

Open a file containing an `automation`.

- [ ] A **Run...** CodeLens renders above it
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

## 5. Deployments, and the cluster page (memql#3733)

The Cluster tab is gone. Topology -- the pod grid, the replica tally, the
orphan verdicts -- is cluster state, and MemQL OS owns it; **Open MemQL OS**,
on the Clusters row and on the cluster page, is one click away. What replaced
it is split in two: Deployments answers "what do I operate", and the cluster
page answers "what does this editor dial, and as whom".

### 5a. The Deployments view (rewritten by memql#4426)

The view is the SELECTED cluster's runs, flat. There is no `local` wrapper row
and no instance row of any kind -- the instance's facts are the view's
description and its actions are in the title menu.

- [ ] With **no cluster selected**: the view is empty and shows the welcome
      -- *Not connected to a cluster.* -- carrying **Install Local Cluster**
      (**Show Local Cluster** when a local cluster is on this computer) and
      **Connect to a Cluster**, and the view has no description
- [ ] Selecting a cluster populates it; the description reads
      `<name> · Connected · <version>`, and the version is the tag the install
      recorded
- [ ] With TWO clusters registered, switching the selection in Clusters switches
      this view with it, and none of the other cluster's runs remain
- [ ] A local cluster that is installed and not answering keeps its rows and its
      description reads `· Not running ·` (`· Can't reach ·` for a remote
      one) -- selected-but-unreachable is NOT the empty state and must not show
      the welcome. A cluster that needs a sign-in shows *Sign in to see this
      cluster's history.* with **Sign In**
- [ ] A version that cannot be worked out is left out of the description,
      never printed as `unknown` or a blank between separators
- [ ] A newer release available adds `· <version> available` to the
      description of a cluster running released images, and never to one
      running your own build
- [ ] A selected cluster with no runs shows its description and no rows -- not
      the welcome, and not an empty-state placeholder
- [ ] Run rows are newest first and carry the verb in the past tense
      (**Installed**, **Updated**, **Rebuilt**, **Update failed**), the version
      transition and a relative time
- [ ] A run started from this editor appears in the tree BEFORE its first step
      reports, and its steps fill in live
- [ ] Kill the editor mid-run, reopen: the run is still listed, and names exactly
      the steps that had completed
- [ ] Disconnect (or sign out): the rows go, the description clears, and the
      welcome returns

### 5a-i. Where the instance went (memql#4426)

Every action the `local` row used to carry must still be reachable. All of them
are now in the view's **title menu** (the `...` in the view's header), each
shown only when it is legal.

- [ ] With an INSTALLED local cluster selected, the title menu offers
      **Change Version...**, **Repair Local Cluster** and **Uninstall Local
      Cluster...**, plus **Rebuild From Checkout...** and **Open Checkout
      Folder** when a checkout is recorded, and **Pull and Rebuild...** when
      that checkout is on a branch. Uninstall is in a group of its own, last
- [ ] With a machine that has NO local cluster selected, it offers
      **Install Local Cluster...** and none of the above
- [ ] With a REMOTE cluster selected, it offers none of the local actions -- an
      action whose only outcome is a refusal must not be drawn
- [ ] With nothing selected, it offers none of them
- [ ] **Show Deployments** in the view's title bar opens the page for the
      SELECTED cluster -- check this with a remote cluster selected, since
      opening the local one instead is the failure that looks right

### 5a-ii. Opening one deployment (memql#4427)

- [ ] Clicking a run row opens its detail page -- it is not inert
- [ ] The page states the kind, the version transition, the start and the
      duration, and its bar carries the outcome in a word
- [ ] A run still in flight shows NO duration and NO finish time, rather than
      zeros
- [ ] A failed run names what went wrong and the step it failed in, and that
      step's log opens beneath it
- [ ] A local run's items are headed **Steps**; a remote deployment's are headed
      **Services**, each with its version and replicas
- [ ] A run with no recorded items says so (*No steps were recorded.*), rather
      than showing an empty list
- [ ] A local run that failed or was interrupted offers **Retry**, which runs
      the act it came from (the same version change, rebuild or repair) -- and
      offers nothing when that act is not legal now. A run that succeeded
      offers no acts at all
- [ ] On a remote cluster, the deployment **Roll back** would return to offers
      **Roll back to <version>...** on its own page, and no other deployment's
      page does
- [ ] While another run is going on this machine, the page offers **Show
      current run** and nothing that could start a second run
- [ ] The head's back link returns to the cluster's page

### 5b. Changing a local cluster's version

- [ ] **Change Version...** on an installed cluster opens **Change version**:
      the current version, and a list of the published MemQL releases, newest
      first, the newest marked **Latest** and the running one **Current**,
      with **nothing pre-selected**. **Other...** takes a typed tag
- [ ] With no network, the list is absent and a **Version** box, with
      *Couldn't load the list of versions.* under it, still accepts a tag
- [ ] A mistyped tag (`0.18.0`, `latest`) is refused under the box, before
      anything runs
- [ ] Choosing the version the cluster is already on is allowed, and says
      *Already on <version>. This re-applies it, the same as Repair.*
- [ ] Choosing a version lists, under **What changes**, the steps that will do
      something (switching the source, applying the version) and says how many
      others are checked and left as they are; the button reads **Change to
      <version>**
- [ ] A newer release than the running one is also offered on the cluster's page
      as **Update to <version>...**
- [ ] The run uses the progress screen (*Changing version*, or *Updating
      MemQL* for **Update to <version>...**), and the steps already satisfied
      are skipped
- [ ] Docker not running fails at the first step, **in the page with its
      guidance** -- not as a notification toast
- [ ] A failed step offers **Retry** (when retrying could help) and no guided
      mode

### 5c. Repair and uninstall, from Deployments

- [ ] Repair and Uninstall appear in the Deployments view TITLE menu with an
      installed local cluster selected (memql#4426 moved them off the instance
      row, which no longer exists), and on **no** Clusters row
- [ ] Uninstall opens the **Uninstall MemQL** page, the same one the Add a
      cluster page opens, and nothing is removed until its button is pressed
- [ ] An artifact the install FOUND rather than created is listed as **Kept**,
      is left on the machine, and appears as `preserved` in the run record
      afterwards

### 5cc. Rebuild from checkout on a wizard-installed cluster (memql#4246)

Needs a local cluster the wizard installed, so there is a recorded checkout and
the cluster is running RELEASED images. A rebuild takes minutes and changes
which images the cluster runs -- do not run it against a parity cluster
somebody else is using.

- [ ] **Rebuild From Checkout...** appears in the Deployments title menu with an
      installed local cluster selected, and with nothing else selected; a
      machine with no recorded checkout does not offer it
- [ ] The **Rebuild from checkout** page reads *Checking the source* briefly,
      then shows the source folder and the commit (with the uncommitted count),
      and says *The cluster switches to your own build.* on a released-lane
      cluster
- [ ] Editing something under `deploy/` in the checkout adds a notice that
      changes under `deploy/` aren't applied, and only code is rebuilt
- [ ] Stop Docker: a notice says *Docker isn't running.* with **Check again**,
      the bar reads *Can't rebuild yet*, and nothing runs
- [ ] Leaving **Services** empty rebuilds every app node; typing `bff, agent`
      rebuilds those two, and the toast afterwards names what was actually built
- [ ] The run uses the same progress screen an install does
- [ ] Afterwards the Deployments view description reads
      `<name> · Connected · Your build <commit>` with no `available` clause,
      and the cluster's page Details say **Built from your checkout**
- [ ] A construct you edited before the rebuild stops reading
      `Edited · needs rebuild` without touching the file -- the catalog
      refreshed
- [ ] Now open **Repair**: its checks list **Images** as **Replaced**, saying
      your checkout build is replaced by the release and that Rebuild from
      checkout brings it back. **Change version** on the same cluster says
      *Your own build is replaced with released <version> images.*
- [ ] A failed rebuild lands on the failure screen with the step's own reason,
      and **Retry** re-runs the REBUILD -- not a version change
- [ ] **Pull and Rebuild...** on a checkout that is on a branch shows the
      branch, how many commits there are to pull, your own commits not on the
      origin, and uncommitted files; **Merge with my commits** is a switch,
      off. A merge in progress in the folder blocks it with *Can't pull yet*

### 5d. A remote instance

- [ ] Its runs read from the cluster, newest first, and their items are labelled
      **Services** -- never "Steps" -- with version and replicas
- [ ] A remote this editor is not connected to still lists in the CLUSTERS view,
      with its state word, rather than hidden; the Deployments view narrowed to
      the selection in memql#4426 and shows only the one you are on
- [ ] Exactly one of the three pipeline states renders: the actions,
      *Deployments aren't set up for this cluster.*, or *Your role can't view
      deployment status.*, with the engine's own words under **Details**
- [ ] Only the actions your role permits are drawn -- **Deploy** and **Roll
      back to <version>...** on the bar, **Promote** and **Abort...** on each
      rollout's row under **Rollouts**, **Prepare <version>** under **Next
      version** -- and a refusal from the engine names the role required,
      verbatim, with its audit id
- [ ] Abort and Roll back require typing their target, and a mismatch refuses

### 5e. The cluster page

- [ ] Clicking a Clusters row that is connected, or needs a sign-in, opens the
      cluster page, titled with the cluster's name; **Show Cluster Details**
      opens it from the context menu or the palette
- [ ] It shows the address and MemQL OS, **Installed: On this computer** for a
      local cluster, and its version in the head when it says something
- [ ] Signed in and connected: **Signed in as** shows the email and role from
      the live session, and appears only while a connection is up
- [ ] The bar carries the state in words with at most three acts:
      **Sign out**, **Disconnect**, **Open MemQL OS** when connected; **Sign in
      with a code** and **Sign in** when a sign-in is needed (with **Create
      owner passkey** on a local cluster's first run); **Show details**,
      **Repair**, **Retry** for a local cluster that is not running
- [ ] Signing in moves the bar to **Signing in** with *Waiting for you in the
      browser*, and after 30 seconds **Use a code instead** appears beside
      **Cancel**
- [ ] **Open MemQL OS** opens the cluster's MemQL OS -- its own site row when
      connected, the composed `os.<domain>/` when not
- [ ] **Remove from list** on a LOCAL cluster says the cluster keeps running on
      this computer and can be added back from **+**
- [ ] After removing it, the page says the cluster is no longer in your list,
      and the **+** offers **Connect to it**, which asks for nothing at all
      before the cluster is back in the list

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

Which of the two files depends on what the case needs:

- `index.ts` -- needs no cluster. Activation, contributions, the host runtime,
  watchers, webview creation.
- `live.ts` -- needs a connection, and must be **read-only**. A case that has
  to write, or that can only be judged by looking, belongs on this checklist
  instead. Say so in the case's comment when you leave it here.
