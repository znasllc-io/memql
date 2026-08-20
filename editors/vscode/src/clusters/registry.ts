// Removing a cluster COMPLETELY.
//
// Three things carry a cluster's existence, and deleting one of them is not
// deleting the cluster:
//
//   the ENTRY       -- ~/.memql/clusters.yaml, shared with the MemQL Cockpit
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

import { ClusterCredentialStore, renameClusterCredentials, type SecretStore } from "../auth/store.js";
import { readClustersFile, removeCluster, upsertCluster } from "./file.js";
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
 * than the call that made it, because the MemQL Cockpit writes this file too.
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

/**
 * Saves an edited cluster COMPLETELY, moving its credentials when the name
 * changed (memql#3515).
 *
 * The same three-things problem as removal, from the other side. A cluster's
 * credentials are keyed by NAME and only half of them live in the file: #3404
 * puts the access token on the entry (so the file write carries it along
 * automatically) and the refresh token plus the expiry in SecretStorage (so
 * nothing carries them at all). Renaming through `upsertCluster` alone left the
 * thirty-day credential stranded under the old name -- invisible, because
 * SecretStorage cannot be enumerated, and then deleted by the next reconcile as
 * belonging to a cluster that does not exist. The operator's symptom was a
 * cluster they had signed into asking them to sign in again after a rename.
 *
 * `renameClusterCredentials` existed for this, correct and tested, with no
 * callers -- which is the same defect as the unreachable device-code fallback
 * #3515 was filed about, in a second place. It is what this calls (memql#3529):
 * the first fix reached past it to `ClusterCredentialStore.rename` directly,
 * which closed the operator-visible bug while leaving the named function still
 * importable and still called by nothing -- two spellings of one decision, and
 * a reader who found the free function had no way to know which one ran. Its
 * sibling `reconcileClusterCredentials` is already used in its free-function
 * form; going through this one makes the pair symmetric and lets the wiring
 * guard (test/authWiring.test.ts) cover both.
 *
 * ORDER: the file write runs FIRST. It is the step that can fail (a
 * clusters.yaml the Cockpit is mid-write, a read-only home directory), and a
 * secret moved to a name no entry carries is exactly the orphan this exists to
 * prevent. The secret move cannot fail loudly -- rename() swallows keyring
 * failures like every other secret operation here -- so running it second means
 * a failure leaves the credential under the OLD name, which is where the entry
 * still is.
 *
 * A no-op rename (same name) skips the move rather than renaming a key onto
 * itself.
 */
export async function saveClusterEdit(
  clustersPath: string,
  previousName: string,
  edited: ClusterConfig,
  deps: Pick<RemoveClusterDeps, "secrets">,
): Promise<void> {
  await upsertCluster(clustersPath, edited, previousName);
  if (edited.name !== previousName) {
    await renameClusterCredentials(deps, previousName, edited.name);
  }
}

/**
 * What has to happen once a local cluster is off the MACHINE (memql#3476).
 *
 * The uninstall graph takes back the k3d cluster, the hosts block, the mkcert
 * CA and the pinned checkout. Three things this editor believes are then false,
 * and each is false for its own reason:
 *
 *   the ENTRY    -- clusters.yaml still names a cluster that no longer exists,
 *                   and every command that reads the registry would offer to
 *                   connect to it. Removing it goes through the whole operation
 *                   above, so the credential and the live connection go too.
 *   the MEMO     -- ClusterPresence caches its verdict for thirty seconds, and
 *                   an uninstall is exactly the event presence.ts documents as
 *                   requiring invalidate(): the answer changed deterministically
 *                   and the operator must not be shown the menu for a machine
 *                   that still has a cluster.
 *   the TREE     -- it renders the registry, which just changed.
 *
 * THE ORDER IS LOAD-BEARING, and so is the fact that the last two are
 * unconditional. Dropping the entry is the one step that can fail (a
 * clusters.yaml the Cockpit is mid-write, a read-only home directory), and it
 * runs first so a failure is reported before anything says the surface is up to
 * date. But the memo and the tree are refreshed whether it succeeded or not:
 * the MACHINE has changed either way, and leaving a thirty-second memo claiming
 * a healthy local cluster because a YAML write failed would compound one problem
 * with a second, unrelated lie.
 *
 * Returns the problem sentence, or "" when there was none -- rather than
 * throwing, because by the time this runs the uninstall itself has SUCCEEDED,
 * and a rejection here would present a clean machine as a failed removal.
 */
export interface UninstallFollowUp {
  /**
   * The registry name of the local cluster, or undefined when none is
   * registered. Absent is an ordinary case: an operator can install a cluster
   * without ever adding it to clusters.yaml.
   */
  clusterName: string | undefined;
  /** Drops the entry, its stored credential and its live connection. */
  removeEntry: (name: string) => Promise<unknown>;
  /** Drops the presence memo. */
  invalidatePresence: () => void;
  /** Repaints the Clusters view. */
  refreshTree: () => void;
  /**
   * Removes the install receipt -- the record of the install that has just been
   * reversed (memql#3544).
   *
   * THE THIRD THING A REMOVAL LEAVES BEHIND, and the one nothing used to take.
   * `detectPresence` reads the receipt, so a machine whose every artifact had
   * just been removed still answered `installed-*`: the Install card stayed
   * withdrawn, Repair re-ran a graph over a cluster that was gone, and
   * Uninstall reported nothing left to remove. Permanently -- no control in the
   * extension could clear it.
   */
  deleteReceipt: () => Promise<void>;
}

export async function completeLocalUninstall(follow: UninstallFollowUp): Promise<string> {
  const problems: string[] = [];
  const name = follow.clusterName;
  if (name !== undefined && name !== "") {
    try {
      await follow.removeEntry(name);
    } catch (err) {
      problems.push(
        `"${name}" could not be removed from the cluster list: ` +
          `${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }

  // ATTEMPTED EVEN IF THE REGISTRY WRITE FAILED. They are independent records of
  // the same departed cluster, and the receipt is the one that decides what the
  // wizard offers next -- so a failure to tidy clusters.yaml must not also cost
  // the operator the Install card.
  try {
    await follow.deleteReceipt();
  } catch (err) {
    problems.push(
      `the record of the install could not be removed, so MemQL will go on ` +
        `reporting a cluster on this machine: ${err instanceof Error ? err.message : String(err)}`,
    );
  }

  follow.invalidatePresence();
  follow.refreshTree();
  // The cluster IS gone either way; these are follow-ups, and the caller
  // renders them as their own news rather than as a failed removal.
  return problems.length === 0 ? "" : `the cluster is off this machine, but ${problems.join("; ")}`;
}
