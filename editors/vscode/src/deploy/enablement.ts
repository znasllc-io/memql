// Whether the cluster panel's deploy actions can be pressed, and what to offer
// when they cannot.
//
// The panel used to ask ONE question -- what is the caller's role -- and drew
// its buttons from the answer alone. So a cluster that was unreachable, or
// whose credential had expired, rendered a full set of live-looking deploy
// buttons directly beneath a warning saying it was not connected. Every one of
// them was certain to fail.
//
// TWO QUESTIONS, AND THEY STAY SEPARATE:
//
//   ROLE decides what EXISTS. A caller who may never cut a version should not
//   see a Cut Version button at all; that is `visibleActions` in ./actions.ts
//   and it is unchanged.
//   CONNECTION STATE decides what is PRESSABLE. A button that is momentarily
//   unusable greys out and names the one thing that fixes it.
//
// Collapsing the two would lose the difference between "you cannot do this" and
// "you cannot do this yet", which are not the same message to an operator.
//
// NOTHING HERE IS A GATE. ./actions.ts says it at length and it holds just as
// much for this file: the engine is the authority, every refusal arrives from
// the same service gate the unary path runs, and the worst a mistake here can
// do is grey out a button that would have worked. It can never permit one that
// should have been refused.
//
// The verdict is the one src/clusters/status.ts already computes for the tree
// row, rather than a second classification. memql#3385 introduced that split --
// an expired token and a cluster that went away used to render identically, and
// they have completely different next actions -- and a panel that re-derived it
// would be free to disagree with the icon sitting next to it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3467 #3463 #3385

import type { ClusterRowIcon } from "../clusters/status.js";
import type { ClusterConfig } from "../clusters/model.js";

/**
 * The single control offered alongside disabled actions.
 *
 * One, never a menu: the panel is telling the operator what is wrong, and the
 * value of that is lost if it then asks them to choose how to respond. Each id
 * maps to a command the extension already contributes.
 */
export type PrimaryControlId =
  /** Dial the cluster. */
  | "connect"
  /** Get a fresh credential. */
  | "signIn"
  /** Re-run the install graph over a local cluster that stopped answering. */
  | "repair"
  /** Supply an endpoint, for a cluster that has none. */
  | "edit";

export interface ActionEnablement {
  /** Whether the deploy buttons may be pressed. */
  enabled: boolean;
  /** Why not, as a sentence a tooltip shows verbatim. Empty when enabled. */
  disabledReason: string;
  /** The one thing to click. Absent when there is nothing useful to offer. */
  primaryControl?: PrimaryControlId;
}

const ENABLED: ActionEnablement = { enabled: true, disabledReason: "" };

/**
 * Maps a cluster's resting/live verdict onto what the panel should do.
 *
 * `local` matters in exactly ONE case -- an unreachable cluster. A remote
 * cluster that does not answer is somebody else's outage and all this editor
 * can do is retry; a local one is on this machine, installed by this extension,
 * and re-running the install graph over it IS the repair (every step verifies
 * first and skips when satisfied). It deliberately does NOT redirect the
 * credential case: a local cluster that is up but whose token expired needs a
 * sign-in, and repairing it would rebuild a working cluster to fix a token.
 */
export function actionEnablement(
  icon: ClusterRowIcon,
  cluster: Pick<ClusterConfig, "name" | "local">,
): ActionEnablement {
  const name = cluster.name;
  switch (icon) {
    case "connected":
      return ENABLED;

    case "connecting":
      // No control. Offering Connect during a connect invites the operator to
      // make it worse, and the state resolves on its own.
      return {
        enabled: false,
        disabledReason: `Connecting to "${name}". Deploy actions become available once the handshake completes.`,
      };

    case "credential":
      return {
        enabled: false,
        disabledReason: `The credential for "${name}" is missing or no longer accepted. Sign in to run deploy actions.`,
        primaryControl: "signIn",
      };

    case "unconfigured":
      // An empty endpoint. Connect cannot succeed and Sign in has nowhere to
      // go; the only thing that fixes it is supplying an address.
      return {
        enabled: false,
        disabledReason: `"${name}" has no endpoint, so there is nothing to dial. Edit the cluster to add one.`,
        primaryControl: "edit",
      };

    case "failed":
      return cluster.local === true
        ? {
            enabled: false,
            disabledReason: `The local cluster "${name}" is not answering. Repair it to run deploy actions.`,
            primaryControl: "repair",
          }
        : {
            enabled: false,
            disabledReason: `"${name}" is not answering. Connect to run deploy actions.`,
            primaryControl: "connect",
          };

    case "idle":
      return {
        enabled: false,
        disabledReason: `Deploy actions need a live connection to "${name}". Connect to run them.`,
        primaryControl: "connect",
      };
  }
}
