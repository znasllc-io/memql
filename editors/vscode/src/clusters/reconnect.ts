// Getting back to a local cluster that is already here, with nothing typed.
//
// `memql.clusters.remove` correctly removes only the REGISTRY ROW -- the
// cluster keeps running -- and until now the only way back was to walk the
// add-a-cluster form and re-enter values the machine already knows. That is
// four boxes to fill in about a cluster this editor installed itself.
//
// `clusters/presence.ts` has computed the evidence all along
// (`installed-healthy` / `installed-unreachable`) and nothing consumed it.
// This is the consumer.
//
// TWO EVIDENCE SOURCES, EITHER SUFFICIENT -- the same rule presence.ts applies
// to the verdict itself. The install receipt names the domain the cluster was
// built for; a machine whose receipt is gone but whose cluster answers is the
// hand-built `make up` case, and it gets the installer's own default. Both are
// better than a form.
//
// AND THE DEFAULT IS NOT A GUESS. `DEFAULT_LOCAL_DOMAIN` is what `make up`
// serves with no `DOMAIN=` override and what the installer writes into the
// hosts file, so it is the domain a hand-built cluster actually has -- not a
// plausible value chosen to fill the field.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3741 #3733

import type { ClusterUpdate } from "./file.js";
import { installedClusterEntry } from "../install/handoff.js";
import { DEFAULT_LOCAL_DOMAIN } from "../install/stackPin.js";
import { recordedDomain, type Receipt } from "../install/receipt.js";

export interface ReconnectPlan {
  /** The entry to write. Composed, never collected. */
  entry: ClusterUpdate;
  /** The domain it was composed from. */
  domain: string;
  /** Whether the receipt supplied it, as opposed to the installer's default. */
  fromReceipt: boolean;
}

/**
 * The registry entry for the local cluster on this machine.
 *
 * Goes through `installedClusterEntry`, which is what an INSTALL writes -- so a
 * cluster reconnected to is registered identically to one just built, including
 * the `local: true` flag that earns it the uninstall action. Two spellings of
 * the same entry would be two answers to what a local cluster's row looks like.
 */
export function planLocalReconnect(receipt: Receipt | null): ReconnectPlan {
  const recorded = recordedDomain(receipt);
  const domain = recorded !== "" ? recorded : DEFAULT_LOCAL_DOMAIN;
  return {
    entry: installedClusterEntry({ domain }),
    domain,
    fromReceipt: recorded !== "",
  };
}
