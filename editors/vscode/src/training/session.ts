// What is defined on THIS stream, and how the editor says so.
//
// A session-defined construct is callable by name and looks exactly like a
// promoted one from the call site. That resemblance is the risk the whole
// training design is built around -- "try in session" mistaken for "promote" is
// a developer who believes a cluster is carrying something it will lose at the
// next reconnect -- so the editor has to keep saying which one happened, after
// the modal that said it once has gone.
//
// THE RECORD DIES WITH THE STREAM, and that is not bookkeeping hygiene: it is
// the only honest model. The engine drops every session definition when the
// WebSocket ends and does not report it -- the next call by that name silently
// resolves against whatever the cluster deployed. So a record that outlived the
// stream would be this editor asserting something false about the cluster, which
// is worse than saying nothing. `noteStreamReset` clears, unconditionally, on
// EVERY connection-state change, exactly as run/session.ts bumps its epoch on
// all of them: the cost of an unnecessary clear is a lens that stops claiming
// something true, and the cost of a missed one is a lens that keeps claiming
// something false.
//
// It is deliberately NOT run/session.ts. That class answers "must I re-inject
// before this run can be trusted", which is a caching question with an epoch and
// a sources comparison; this one answers "what should the editor be showing",
// which needs the NAMES and needs nothing else. Sharing one class would have
// meant a cache whose invalidation is also a rendering decision.
//
// Free of `vscode` imports; the CodeLens adapter lives in extension.ts.
//
// Refs: #3763 #3745

import type { LspRange } from "../constructs/runnable.js";
import type { TrainingConstruct } from "../state/training.js";

/** One construct a session-define registered. */
export interface DefinedConstruct {
  kind: string;
  name: string;
}

export class SessionDefinitions {
  // Keyed by cluster name: switching cluster and switching back within one
  // stream is not possible (a switch resets the stream), but two clusters can
  // hold different definitions across the life of one editor window and a
  // name-keyed set would merge them.
  private readonly byCluster = new Map<string, Map<string, DefinedConstruct>>();
  // Monotonic, never reset. It identifies the stream a record belongs to for
  // anything that wants to reason about generations; the records themselves are
  // simply dropped, because a stale one has no use at all.
  private epoch = 0;

  /** The current stream generation. Exposed for tests and diagnostics. */
  get streamEpoch(): number {
    return this.epoch;
  }

  /** Called on EVERY connection-state change. See the module comment. */
  noteStreamReset(): void {
    this.epoch += 1;
    this.byCluster.clear();
  }

  /** Record a SUCCESSFUL session-define. */
  record(clusterName: string, constructs: readonly DefinedConstruct[]): void {
    let bucket = this.byCluster.get(clusterName);
    if (bucket === undefined) {
      bucket = new Map<string, DefinedConstruct>();
      this.byCluster.set(clusterName, bucket);
    }
    for (const c of constructs) bucket.set(c.name, { kind: c.kind, name: c.name });
  }

  /** Is `name` currently defined on this stream, on this cluster? */
  isDefined(clusterName: string, name: string): boolean {
    return this.byCluster.get(clusterName)?.has(name) === true;
  }

  /** Everything defined on this stream for a cluster, in insertion order. */
  defined(clusterName: string): DefinedConstruct[] {
    return [...(this.byCluster.get(clusterName)?.values() ?? [])];
  }

  /**
   * Forget a cluster's records.
   *
   * Called when a session-define FAILS: the registration state is then unknown,
   * the request may have been applied before the transport failed, and claiming
   * a definition this editor cannot vouch for is the one thing this class must
   * not do.
   */
  forget(clusterName: string): void {
    this.byCluster.delete(clusterName);
  }
}

/** A lens saying a construct is live for this session only. */
export interface SessionLensPlan {
  name: string;
  signatureRange: LspRange;
  title: string;
  tooltip: string;
}

/**
 * sessionLensPlans is the lens that keeps "temporary" on screen.
 *
 * It is a SEPARATE lens from the state lens rather than a fifth state, because
 * it is not a state: the construct is still `untrained` on that cluster and
 * still wants a Promote offered beside it. What changed is that a copy of it is
 * answering calls right now, on this connection, and will stop without notice.
 *
 * NO COMMAND. Clicking it would have to do something, and there is nothing to
 * do: undefining is not a thing the engine offers, and the way to make it
 * permanent is the Promote lens already sitting on the same line.
 */
export function sessionLensPlans(
  constructs: readonly TrainingConstruct[],
  defined: SessionLookup,
): SessionLensPlan[] {
  const out: SessionLensPlan[] = [];
  for (const construct of constructs) {
    if (!defined(construct.name)) continue;
    out.push({
      name: construct.name,
      signatureRange: construct.signatureRange,
      title: "defined for this session only",
      tooltip:
        "This version is answering calls on the current connection and nowhere else. It is not persisted, no other caller can see it, and it is dropped when the connection drops or you switch cluster. Promote is what makes it outlive the session.",
    });
  }
  return out;
}

/** Whether a construct name is currently session-defined. */
export type SessionLookup = (name: string) => boolean;
