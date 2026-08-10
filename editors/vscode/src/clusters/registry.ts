// Removing a cluster COMPLETELY.
//
// Three things carry a cluster's existence, and deleting one of them is not
// deleting the cluster:
//
//   the ENTRY       -- ~/.memql/clusters.yaml, shared with the memQL Cockpit
//   the CREDENTIAL  -- SecretStorage, keyed per cluster name, thirty days
//   the CONNECTION  -- the live stream, if this happens to be the one dialled
//
// removeCluster (./file.ts) does the first alone, which is the right shape for
// a document operation and the wrong shape for an operator action. Left at
// that, "delete this cluster" would strand a thirty-day refresh token under a
// key SecretStorage cannot enumerate to find again, and leave a stream running
// against a cluster nothing in the registry can name.
//
// This module is the whole operation. It is the REVERSIBLE half of the pair the
// Clusters tree offers: it removes what this editor knows about a cluster and
// touches no machine. Uninstalling a local cluster -- k3d, /etc/hosts, the
// mkcert CA -- is a different command with a different confirmation, and the
// two are kept apart deliberately (docs/superpowers/specs/
// 2026-08-09-vscode-clusters-surface-design.md, D1).
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3465 #3464 #3463

import { ClusterCredentialStore, type SecretStore } from "../auth/store.js";
import { readClustersFile, removeCluster } from "./file.js";
import type { ClusterConfig } from "./model.js";

export interface RemoveClusterDeps {
  /**
   * Absent in a host with no SecretStorage.
   *
   * The purge then degrades to a no-op rather than an error, exactly as every
   * other secret operation in this extension does: a locked keyring is a normal
   * Linux condition and must not make a cluster undeletable.
   */
  secrets?: SecretStore;
  /**
   * The cluster the live connection is dialled to, if any.
   *
   * Supplied rather than read, because connection state lives in the
   * ConnectionManager and this module must stay importable from plain Node.
   */
  connectedClusterName?: string;
  /** Drops the live connection. Called only when it is this cluster's. */
  disconnect?: (clusterName: string) => Promise<void> | void;
}

/**
 * Removes a cluster's entry, its stored credential, and its live connection.
 *
 * THE EXISTENCE CHECK COMES FIRST, before any of the three steps. Every step
 * after it is destructive and none is conditional on the next one succeeding,
 * so a name that is not there has to be refused while nothing has happened yet
 * -- otherwise "remove a cluster that isn't there" would still disconnect a
 * session and purge a credential on the way to failing. Deriving that from the
 * caller's own state instead (they surely aren't connected to a cluster that
 * doesn't exist) would be an argument rather than a guarantee, and the tests
 * assert the guarantee.
 *
 * The re-read this costs is the same one `addCluster` performs for the same
 * reason, and carries the same caveat: no read here is authoritative for longer
 * than the call that made it, because the memQL Cockpit writes this file too.
 * Caching it would not make the check any less racy, and the real refusal
 * arrives from removeCluster below regardless.
 *
 * ORDER IS THEN LOAD-BEARING: disconnect, purge, rewrite.
 *
 * The connection goes first because it is the only one of the three that cannot
 * be cleaned up after the fact. A failure between the file write and the
 * disconnect would leave a stream running against a cluster no longer in the
 * registry -- and nothing left could name it to close it. The credential is the
 * opposite case: an orphaned secret is recoverable, because
 * `ClusterCredentialStore.reconcile` sweeps credentials whose cluster is gone,
 * and removing the entry is exactly what makes this one eligible. So the
 * irrecoverable step runs while everything still exists to undo it.
 *
 * Returns the removed entry so the caller can say what went, and can tell
 * whether it was `local` -- which is what decides whether to follow up by
 * offering an uninstall.
 */
export async function removeClusterCompletely(
  clustersPath: string,
  name: string,
  deps: RemoveClusterDeps,
): Promise<ClusterConfig> {
  const registry = await readClustersFile(clustersPath);
  if (!registry.clusters.some((c) => c.name === name)) {
    throw new Error(`no cluster named "${name}" in ${clustersPath}`);
  }

  if (deps.connectedClusterName === name && deps.disconnect !== undefined) {
    await deps.disconnect(name);
  }

  // clear() swallows keyring failures by design (see auth/store.ts), so this
  // cannot be what stops a removal. That is the intended trade: an operator
  // whose keyring is locked can still delete a cluster, and the secret it left
  // behind is swept by the next reconcile.
  await new ClusterCredentialStore(deps.secrets).clear(name);

  return removeCluster(clustersPath, name);
}
