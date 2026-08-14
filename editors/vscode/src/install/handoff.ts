// What happens the moment an install finishes: the cluster the operator just
// built becomes a real registry entry, and they are handed a way in.
//
// WITHOUT THIS the install is a success nobody can use. The graph creates a k3d
// cluster, a hosts entry and a trust-store CA, and then the run ends -- and the
// Clusters tree, which reads ~/.memql/clusters.yaml and nothing else, shows
// exactly what it showed before. The machine changed; the editor did not.
//
// PURE, and free of `vscode` imports, because every effect is injected. That is
// not ceremony: the ordering below is the whole content of this module, and an
// ordering is only worth writing down if it can be tested.
//
// Refs: #3477 #3463 #3401

import { composeEndpointFromDomain } from "../connection/endpoint.js";
import type { ClusterUpdate } from "../clusters/file.js";
import type { ClusterConfig } from "../clusters/model.js";

export interface InstalledCluster {
  /** The domain the install was given. `api.<domain>` is the front door. */
  domain: string;
  /** The registry name. Derived from the domain when the caller has none. */
  name?: string;
}

/**
 * The registry name for a freshly installed cluster.
 *
 * The domain's first label, because that is what an operator calls it: a
 * cluster at `lab.example.com` is "lab", and one at the default
 * `memql.localhost` is "memql". Falls back to "local" for a domain with nothing
 * usable in front, so this never yields an empty name -- `upsertCluster`
 * rejects one outright, and it would orphan the entry.
 */
export function defaultClusterName(domain: string): string {
  const first = trimDomain(domain).split(".")[0] ?? "";
  return first === "" ? "local" : first;
}

function trimDomain(domain: string): string {
  return domain.trim().replace(/^\.+|\.+$/g, "");
}

/**
 * The entry an install writes.
 *
 * `local: true` is DELIBERATE and is the whole reason this composes an entry
 * rather than reusing the add-cluster form's. It is what makes the tree render
 * the row as a local cluster and what earns it the uninstall action -- a remote
 * registration never sets it, so neither kind can claim the other's status by
 * accident.
 *
 * No `issuer`. `identityBaseUrlFor` derives `https://identity.<domain>` from
 * the domain by the same convention that composes the endpoint, so writing one
 * would be storing a value the tree can already compute -- and storing it wrong
 * would override the operator's own discovery document later.
 */
export function installedClusterEntry(params: InstalledCluster): ClusterUpdate {
  const domain = trimDomain(params.domain);
  const name = (params.name ?? "").trim() === "" ? defaultClusterName(domain) : params.name!.trim();
  return {
    name,
    domain,
    // THE ONE SPELLING of the `api.<domain>` convention (memql#3475). This was
    // a private copy, which is the duplication that function exists to
    // prevent: a front door whose port or prefix changed in one place and not
    // the other is a cluster the tree dials at an address nothing serves.
    endpoint: composeEndpointFromDomain(domain),
    local: true,
  };
}

export type HandoffResult =
  | {
      ok: true;
      cluster: ClusterConfig;
      /** Whether "Sign in as owner" can be offered at all. */
      canSignIn: boolean;
    }
  | {
      ok: false;
      /** Where the cluster the operator just built actually answers. */
      reachableAt: string;
      message: string;
    };

export interface HandoffEffects {
  /**
   * Writes the entry. `upsertCluster`, NOT `addCluster`.
   *
   * Re-running an install over a cluster that is already registered must UPDATE
   * the entry, not fail: a repair, or a second run after a partial one, is a
   * completely ordinary thing to do, and `addCluster` refuses a duplicate name
   * by design.
   */
  write: (update: ClusterUpdate) => Promise<void>;
  /** Drops the presence memo. */
  invalidatePresence: () => void;
  /** Repaints the Clusters tree. */
  refreshTree: () => void;
  /** Makes the new cluster the selected one. */
  select: (name: string) => Promise<void>;
}

/**
 * Registers the cluster and reports what the operator can do next.
 *
 * ORDER, and why each step is where it is:
 *
 *   1. WRITE first, because everything after it describes a registry that
 *      contains this cluster.
 *   2. INVALIDATE PRESENCE ON BOTH PATHS -- including a failed write. This is
 *      the subtle one. The memo answers "is there a local cluster on this
 *      machine", and a machine that just had one built now HAS one whether or
 *      not the registry records it. Leaving a stale `absent` verdict would keep
 *      the "+" offering to install a second cluster over the top of the one
 *      that just succeeded, which is the exact destruction the evidence pass
 *      exists to prevent.
 *   3. REFRESH, then SELECT. Selecting a row the tree has not drawn yet is a
 *      no-op the operator sees as "it installed and then nothing happened".
 *
 * A FAILED WRITE IS NOT A FAILED INSTALL. The cluster exists; only the registry
 * entry is missing. So the failure carries where it answers, which is enough
 * for the operator to add it by hand or simply dial it -- rather than a bare
 * error that implies ten minutes of work were wasted.
 */
export async function completeInstallHandoff(
  params: InstalledCluster,
  effects: HandoffEffects,
): Promise<HandoffResult> {
  const entry = installedClusterEntry(params);

  try {
    await effects.write(entry);
  } catch (err) {
    effects.invalidatePresence();
    return {
      ok: false,
      reachableAt: entry.endpoint ?? "",
      message:
        // "its front door is" rather than "it is reachable at": this path is
        // now also reached by a RECONNECT (memql#3741), which is offered for a
        // cluster that is installed and NOT answering. The endpoint is a fact
        // about the entry that was composed; reachability is not.
        `The cluster is on this machine and its front door is ${entry.endpoint}, but it could ` +
        `not be added to your cluster list: ${err instanceof Error ? err.message : String(err)}`,
    };
  }

  effects.invalidatePresence();
  effects.refreshTree();
  await effects.select(entry.name);

  const cluster: ClusterConfig = {
    name: entry.name,
    endpoint: entry.endpoint ?? "",
    domain: entry.domain,
    local: true,
  };
  // A domain is all `identityBaseUrlFor` needs to name the identity host, so a
  // cluster written by this module can always be signed in to. Computed rather
  // than hardcoded true, because that stops being the case the moment the entry
  // is composed differently.
  return { ok: true, cluster, canSignIn: trimDomain(params.domain) !== "" };
}
