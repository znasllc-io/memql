# MemQL editor extensions

MemQL separates editor connections, productivity, and machine operations. Both extend Visual Studio
Code and compatible editors such as Cursor. Both have useful offline features;
connecting to MemQL adds shared state, authorization, and execution.

| Product | Responsibility | Offline | Connected |
|---|---|---|---|
| **MemQL** (`vscode/`) | Cluster lifecycle, connections, authentication, and development | MemQL language support, local source editing | Install a local cluster where the editor host supports it; select and sign in to a cluster; inspect, author, and run MemQL constructs |
| **MemQL Productivity Tools** | Documents, campaign templates, PDFs, and review | View and edit supported local files; compose templates | Open and save MemQL files with revisions; create and edit campaign templates; store comments and suggestions; request human approval before starting work to apply suggestions |

The existing extension owns the selected cluster and its authenticated
connection. Productivity Tools consumes that connection through a versioned
extension API. It must not keep a competing cluster selection, duplicate
credentials, install clusters, or become a second MemQL language extension.
Both extensions use the same MemQL icon and have distinct names and packages.

## Cockpit owns the machine

[MemQL Cockpit](https://github.com/znasllc-io/memql-cockpit) is the fleet
worker runtime and cluster CLI. It owns machine enrollment, the worker
connection, local tool execution, local app/model access, watched-folder
backup, its local transfer spool, and its backup ledger. The extensions must
not install a second worker, scan folders, create a background backup loop,
or mutate worker enrollment as a side effect of selecting a cluster.

The main MemQL extension owns the **editor's** selected connection. Selecting
another cluster in VS Code changes editor requests; it does not re-enroll a
machine or redirect an existing Cockpit worker. Desktop cluster installation
creates/manages the cluster; Cockpit enrollment connects a machine to it.
These are separate operations with explicit user actions.

On desktop the core extension and Cockpit share cluster definitions in
`~/.memql/clusters.yaml`. Registry changes use the shared `core/filetxn`
kernel-lock and atomic-replacement contract. The extension's bundled native
helper performs compare-and-swap; a conflict reapplies the edit to the latest
YAML document. Cockpit holds the same lock for its read-modify-write. Unknown
keys survive both writers. The lock file is permanent; deleting it would let
two writers lock different inodes. This requires both updated clients.
Browser VS Code uses profile metadata because it cannot read the machine's
registry. Productivity always consumes the core extension's connection API.

Authentication is **per client**. VS Code uses its SecretStorage and its own
OAuth client. Cockpit uses its own credential store and OAuth client. Neither
client rotates, revokes, or copies the other's credentials. New editor sign-ins
never put access or refresh tokens in the shared YAML. A locked editor secret
store produces an actionable refusal. Old editor token fields can be ingested
and are cleared after custody; Cockpit PATs and credentials are left alone.

| File operation | Owner and behavior |
|---|---|
| Open/save a remote `memql-file:` document | Productivity through the core editor API; one versioned MemQL save |
| Save a local `file:` document | VS Code writes the local file; Productivity does not additionally upload it |
| Back up a watched local folder | Cockpit's existing one-way host-to-cluster pipeline; never writes a remote edit back to the original |
| Browse, organize, download, configure backup | MemQL OS Files app; editing opens VS Code |
| Revision checks, authorization, history, review, jobs | MemQL backend, shared by all clients |

An editor save and a backup share the same version precondition. Large uploads
retain the version observed at initialization and check it again at completion.
A remote edit during upload produces a conflict, not an overwrite. A completion
retry returns the version that upload actually wrote, even after later edits.
No extension starts its own backup queue or bypasses the server's row access.

## Supported hosts

Both extensions must ship a desktop and a browser entry point. Supported
operating systems are **macOS and Linux**. Windows is not supported at this
time. Browser support includes VS Code for the Web on those operating systems;
desktop support includes compatible Cursor installations. This is a support
commitment, not a claim that a web browser can execute native desktop tools.

Cluster installation and other native machine operations belong to the desktop
MemQL extension. Its browser version connects to existing clusters and uses
their remote capabilities. Productivity Tools supports local/offline file
work and connected MemQL work on both hosts, within the browser's file-access
permissions. Packaging, installation instructions, and verification must cover
the two hosts for both extensions; a desktop-only VSIX is not completion.

MemQL owns the underlying files, versions, permissions, campaign records,
comments, proposals, approvals, and jobs. The extension is a client. Saving
must reach the currently selected cluster under the signed-in person's
authority, with conflict detection. Selecting another cluster must invalidate
loaded state and must never redirect a dirty document's save to that cluster.

## MemQL OS boundary

Files remains the browser for files, folders, uploads, and backups. Campaigns
remains the organizer for client organizations, audiences, sender identities,
campaigns, schedules, and results. These apps hand content editing to the
editor; they do not own embedded document, PDF, or email-body editors.
Names, organization selectors, filters, and other record settings remain
ordinary OS controls.

The requested default is **VS Code in a separate browser tab**. A setting can
select installed VS Code or Cursor instead. A ZIP is downloaded intact to the
user's machine: opening it never extracts it or loads it as an editor folder.
PDFs belong to Productivity Tools' viewing and editing surface, including
versioned saves to MemQL when connected.

## Delivery status

This is the owner-approved responsibility boundary, recorded October 1, 2026.
The existing `vscode/` extension is implemented. Productivity Tools and the
browser-first file workflow are being implemented; this document does not
claim that they are installed, published, or ready for use yet. The new artifact handoff opens a versioned file through Productivity Tools.
Markdown supports source, reading, and split modes with revision-bound feedback. Campaigns
still contains its previous inline template editor until the replacement is
working and that editor is removed in the same delivery.

The implementation and acceptance requirements are recorded in
[Productivity and campaign workflows](../docs/internal/planning/productivity-and-campaigns.md).
