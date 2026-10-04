# MemQL for Visual Studio Code and Cursor

Write `.memql` where you write code. Inspect and run it where it lives.

The extension gives you MemQL language support that works offline, connects to
your MemQL clusters when you want to run something, and installs a local
cluster on your computer (and removes it again). It is part of the alpha MemQL
platform. It runs on **macOS and Linux**; Windows is not supported.

## Write MemQL offline

- Syntax highlighting, live diagnostics and cross-reference checks.
- Completion, hover, signature help and go to definition.
- Snippets, and quick fixes that rewrite older forms.
- **MemQL: Show Language Reference** shows the connected cluster's grammar and
  vocabulary in one searchable tab, with Copy for handing either to a model.
  With no cluster it names the language this extension was built for.

Open a folder of `.memql` files. The bundled language server needs no cluster
and no sign-in.

## Connect to a cluster

1. Open a trusted workspace and run **MemQL: Add Cluster**, or press **+** in
   the Clusters view.
2. Choose **Connect to a cluster**, type a name and the cluster's domain, and
   press **Connect**. The addresses are worked out from the domain, and the
   cluster is checked before it is added.
3. Press **Sign in**. Your browser opens, and if you are already signed in to
   that cluster's MemQL OS, sign-in finishes without typing anything.

**Sign in is one click** wherever the extension says you need it: the cluster's
row, its page, or the notification. The editor signs in as itself, so a sign-in
another MemQL tool saved for the same cluster never gets in its way. If the
browser hasn't come back after 30 seconds, **Use a code instead** lets you
approve the sign-in on another device. Remote windows (Remote SSH, dev
containers, Codespaces) use a code from the start.

**It reconnects on its own.** The cluster you use is connected again when a
window opens, and after the connection drops, for example when a laptop wakes or
a VPN reconnects. If the cluster is still away after about two minutes, a
notification says so, with **Reconnect**.

## Install a local cluster

You need Docker running, on Linux x64 or Apple Silicon macOS.

1. Run **MemQL: Install Local Cluster...**, or press **+** and choose **Install
   MemQL on this computer**.
2. Enter your name and email. **More options** holds the domain
   (`memql.localhost`) and the version, which is the latest release unless you
   pick another.
3. Press **Install**. The extension asks for your computer password once, to
   update the hosts file and trust a local certificate.

**One progress screen** follows the install: a bar, the step running now
("Creating the cluster"), the step count and the time so far. **Show logs**
opens the live log, with **Copy** and **Open in Output**. **Cancel** stops after
the current step, and **Resume** picks up from there. When a step fails, the
screen names it and says why, gives the fix as a command you can **Run in
terminal**, and offers **Retry**.

When it is done, save the recovery key it shows you (it is shown only once),
then press **Sign in**, or **Set up a passkey** for the owner account.

**MemQL: Repair Local Cluster** runs the install again and fixes only what is
missing.

## Uninstall a local cluster

Run **MemQL: Uninstall Local Cluster...**. The page lists exactly what will go,
and nothing is removed until you press **Uninstall**:

- **Will be removed:** the cluster, the downloaded MemQL files, and the local
  addresses in `/etc/hosts` (which asks for your password).
- **Also remove:** switches for k3d, kubectl, the local certificate authority
  and mkcert. They start off, because other projects on your computer may use
  them. mkcert can only be removed together with the certificate authority.
- **Data:** a cluster this extension didn't create, such as one made with
  `make up` in a MemQL checkout, is kept unless you turn on **Delete the
  cluster's data** and type `delete memql data`. The button then reads
  **Uninstall and delete data**.

The uninstall runs on the same progress screen as an install.

**MemQL: Remove Cluster From List** is a different thing: it takes the cluster
off your list and deletes your saved sign-in. The cluster keeps running.

## The views

The MemQL panel in the activity bar has five views.

| View | Shows |
|---|---|
| **Clusters** | The clusters you can reach, each with its state: Connected, Sign in, Connecting, Not running or Can't reach. Click a row to use that cluster; the one in use is marked. |
| **Deployments** | The history of the cluster in use: installs, updates, rebuilds and deploys. Open a row for its details. |
| **Constructs** | What the cluster has loaded, by kind and namespace. Open one to see its arguments and source, and to run it. |
| **Data** | The rows your account may read, by concept. |
| **Runs** | Runs you saved from a Run form, ready to run again. |

Runnable queries, mutations, logic, tools and automations get a **Run** lens
in the editor. A run can change real data. Each construct also gets a lens
saying where it stands on the cluster: Not on cluster, Differs from cluster,
Staged, Live, Built in or Edited. Click it for the actions that state allows,
such as Dry run, Try in this session, Stage, Promote or Demote. Saving a file
never changes the cluster. The [training guide](https://github.com/znasllc-io/memql/blob/main/docs/public/language/training.md)
explains each action.

**Open MemQL OS**, on a connected cluster's row, opens that cluster's MemQL OS
in your browser: Fleet, Files, Deployables, Nexus and the other apps.

## Commands

Every command is in the Command Palette under **MemQL:**. The ones you will use
most:

| Command | What it does |
|---|---|
| **MemQL: Add Cluster** | Install a local cluster, or connect to one somewhere else |
| **MemQL: Connect to Cluster...** | Choose the cluster to use |
| **MemQL: Connect to Local Cluster** | Add the cluster on this computer to your list and sign in |
| **MemQL: Sign In** | Sign in to a cluster in your browser |
| **MemQL: Sign In With a Code** | Approve the sign-in on another device |
| **MemQL: Sign Out** / **MemQL: Disconnect** | End the session, or just close the connection |
| **MemQL: Show Cluster Details** | The cluster's page: its address, MemQL OS, and who you are signed in as |
| **MemQL: Open MemQL OS** | Open the cluster's MemQL OS in your browser |
| **MemQL: Install Local Cluster...** | Install MemQL on this computer |
| **MemQL: Repair Local Cluster** | Run the install again to fix what is missing |
| **MemQL: Uninstall Local Cluster...** | Remove MemQL from this computer |
| **MemQL: Show Deployments** | The cluster's deployments page |
| **MemQL: Change Version...** | Move the local cluster to another release |
| **MemQL: Rebuild From Checkout...** | Build the local cluster's services from its source folder |
| **MemQL: Pull and Rebuild...** | Pull the source folder's branch, then rebuild |
| **MemQL: Check for Updates** | Look for a newer MemQL release |
| **MemQL: Show Constructs Not Live on Cluster** | List the constructs in the open file that the cluster doesn't run yet |
| **MemQL: Show Language Reference** | The cluster's grammar and vocabulary |
| **MemQL: Edit Cluster** / **MemQL: Remove Cluster From List** | Change a cluster's details, or take it off your list |

## In the browser

In VS Code for the Web, MemQL connects to clusters you already have: add one
with **MemQL: Add Cluster**, then approve the sign-in with a code. Installing a
local cluster and the offline language server need the desktop. Highlighting
and the MemQL themes work on both. **MemQL Productivity Tools** adds documents,
templates and PDFs, and uses this extension's connection rather than a sign-in
of its own.

## Appearance

Choose **MemQL Light** or **MemQL Dark** with **Preferences: Color Theme**. The
extension never changes your theme by itself. The `memql.appearance` setting
picks the palette for the MemQL pages: `system` follows your editor.

## Install the extension from source

From a checkout of this repository:

```bash
make vscode-install                   # Visual Studio Code
make vscode-install EDITOR_CMD=cursor # Cursor
```

Then run **Developer: Reload Window**. You need VS Code 1.91 or later (or a
compatible Cursor), Go, Node.js 20 or later, npm, Make and unzip.
`make vscode-package` builds the `.vsix` without installing it. The
[installation guide](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode.md#get-the-extension)
has the details.

## More

- [Your first MemQL program](https://github.com/znasllc-io/memql/blob/main/docs/public/language/first-program.md):
  a small program to write, validate offline, and run against a cluster.
- [Detailed extension reference](REFERENCE.md): every view, page and setting,
  and how the extension handles credentials.
- [Runtime panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md)
  and [language server](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode.md#architecture).
- Contributors: run `make vscode-deps` once on a clean checkout, then
  `make vscode-test` and `make vscode-test-host`. See
  [Development](REFERENCE.md#development).
