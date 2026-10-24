# Cockpit and the editor extensions

Cockpit owns machine enrollment, worker sessions, local tools and apps, local
models, watched-folder backups, transfer spools, and the backup ledger. It is
the sole background worker for those operations. It does not own a document
editor or campaign template designer.

The **MemQL** VS Code extension owns the editor's cluster connection, sign-in,
cluster lifecycle tools, and language tooling. **MemQL Productivity Tools**
consumes that connection for documents, PDFs, templates, and review. It has no
independent registry, credentials, worker enrollment, or sync daemon. Both
extensions support desktop and web VS Code on macOS and Linux; Windows is
outside the current support matrix. Browser instances connect to existing
clusters and cannot install a cluster or run a native worker.

Desktop clients share connection definitions in `~/.memql/clusters.yaml`.
Cockpit registry edits hold `core/filetxn`'s kernel lock across the read and
write. VS Code's bundled helper uses the same lock and compares the exact
previous bytes before atomic replacement. Unknown fields survive both writers.
Use updated versions of both clients: older clients that ignore the lock cannot
participate in this guarantee. Do not delete the permanent `.lock` sibling.

Logins remain separate. Cockpit's OAuth client and keyring/file credentials are
not VS Code's credentials. VS Code keeps both tokens in SecretStorage. Selecting
an editor connection does not change worker enrollments, and editor sign-out
does not revoke Cockpit's session. Shared metadata deletion affects discovery
of a connection but does not uninstall or re-enroll an existing worker.

Saving a remote virtual document creates one MemQL revision through the editor
API. Saving a local file writes locally; the existing watcher may then back it
up. Productivity must not additionally upload that same local save. Backups
remain one-way: a remote edit never silently writes back to the original file.

Cockpit sends `expectedVersion` for both small and chunked uploads. MemQL must
check the base at initialization and completion, and use a shared database
lock for the final version write. A remote edit creates a conflict for the
backup. A completion retry must report its original upload version, not a
newer head, so the ledger cannot adopt a version it did not write.

MemQL is the authority for organization access, file history, comments,
approvals, and jobs. The clients provide their respective interfaces and do
not maintain competing business logic.
