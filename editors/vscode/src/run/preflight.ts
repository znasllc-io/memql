// The write confirmation, and what counts as "once".
//
// Reads run freely. A MUTATION against a cluster not marked `local` prompts,
// naming the cluster and the construct -- the same instinct behind the
// cockpit's type-to-confirm on rollback, sized to a smaller risk. The engine's
// per-row authorization remains the actual authority; this is friction against
// the WRONG WINDOW, not a second permission system.
//
// "Prompts once" is the part worth being precise about. Once per (cluster,
// construct) for the life of the session:
//
//   - Not once per run: an iterate-fix-rerun loop would fire the dialog every
//     few seconds, and a dialog that appears that often is a dialog nobody
//     reads. The prompt only works if it is rare.
//   - Not once per session outright: acknowledging "yes, write to staging"
//     for one mutation must not silently pre-authorise a different mutation
//     the developer has not thought about. The construct's name is half of
//     what makes the prompt informative.
//   - Not persisted across sessions: the acknowledgement is about the working
//     session's intent, and a stored one would silently outlive the reason
//     for it.
//
// Deliberately free of `vscode` imports -- the adapter owns the modal; this
// owns only the "should we ask" decision. Tested under bare `node --test`.

import { isWriteKind, type RunnableKind } from "../constructs/runnable.js";

export interface WriteConfirmationRequest {
  clusterName: string;
  clusterLabel: string;
  constructName: string;
  constructKind: RunnableKind;
}

export class WriteConfirmationGate {
  private readonly acknowledged = new Set<string>();

  /**
   * required reports whether this run needs a confirmation prompt.
   *
   * `local` is read as "explicitly marked local in clusters.yaml". An ABSENT
   * flag means NOT local, which is the safe default and the only one
   * compatible with the field being new: every cluster already in an
   * operator's clusters.yaml predates it, and defaulting those to "local"
   * would silently disable the confirmation on the exact clusters -- staging,
   * production -- it exists for.
   */
  required(kind: RunnableKind, local: boolean, clusterName: string, constructName: string): boolean {
    if (!isWriteKind(kind)) return false;
    if (local) return false;
    return !this.acknowledged.has(key(clusterName, constructName));
  }

  /** acknowledge records a confirmation the user granted. */
  acknowledge(clusterName: string, constructName: string): void {
    this.acknowledged.add(key(clusterName, constructName));
  }

  /**
   * reset clears every acknowledgement.
   *
   * The adapter calls this when clusters.yaml changes: an operator who just
   * edited the registry may have re-pointed a cluster name at a different
   * endpoint, and an acknowledgement keyed on the NAME would carry over to a
   * cluster the user never confirmed anything about.
   */
  reset(): void {
    this.acknowledged.clear();
  }
}

function key(clusterName: string, constructName: string): string {
  // "\u0000" cannot appear in either a cluster name or a construct name, so
  // no pair of distinct inputs can collide onto one key -- a plain ":" join
  // would let ("a:b", "c") and ("a", "b:c") share an acknowledgement.
  return `${clusterName}\u0000${constructName}`;
}

/**
 * writeConfirmationMessage is the modal's text.
 *
 * It NAMES THE CLUSTER AND THE CONSTRUCT, which is the entire content of the
 * warning: "are you sure?" tells the developer nothing they can check, while
 * "run mutationCreateSpace against staging" is a claim they can immediately
 * recognise as right or wrong. The consequence follows in one clause: what is
 * true of a cluster that is not local, never the file that records it.
 */
export function writeConfirmationMessage(req: WriteConfirmationRequest): string {
  return `Run "${req.constructName}" on ${req.clusterLabel}? It isn't a local cluster, so this writes real data.`;
}
