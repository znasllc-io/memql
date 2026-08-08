// Tracking what has been session-defined, per cluster, on the CURRENT stream.
//
// This module exists for one failure that is otherwise invisible.
//
// A session-defined construct lives on the WebSocket. When the stream ends --
// a dropped socket, a cluster switch, a reconnect -- every injected construct
// is gone. The engine does not report that, and it does not have to: the next
// `ExecuteQueryMsg` naming that construct resolves happily against the
// DEPLOYED definition and returns a perfectly good result. So a developer who
// edits a buffer, runs it, loses the connection for a moment, and runs again
// gets a green result computed by code they are not looking at. Nothing on
// screen is wrong; the answer just is not theirs.
//
// The fix is to remember what was injected and re-inject before honouring a
// re-run. That needs two facts, and this class holds both:
//
//   - WHAT was last injected per cluster, so an unchanged buffer does not pay
//     for a redundant round-trip on every run;
//   - WHICH STREAM it was injected on, so "unchanged buffer" cannot be
//     confused with "still injected". Sources equality alone is precisely the
//     wrong test -- after a reconnect the sources are identical and the
//     registration is gone.
//
// The stream identity is an EPOCH COUNTER rather than a connection object.
// Holding the connection would keep a dead one alive and would not survive the
// ConnectionManager replacing it; a counter the adapter bumps on every
// connection-state change is cheap, and its failure mode is a redundant
// re-inject rather than a missed one.
//
// Deliberately free of `vscode` imports -- tested under bare `node --test`.

interface Injection {
  sources: string;
  epoch: number;
}

export class SessionRegistry {
  // Monotonic, never reset. Every connection-state change bumps it, so a
  // record minted under an earlier value is by definition stale. Starting at
  // 0 with no records means the first run always injects.
  private epoch = 0;
  private readonly injected = new Map<string, Injection>();

  /** The current stream generation. Exposed for tests and for diagnostics. */
  get streamEpoch(): number {
    return this.epoch;
  }

  /**
   * noteStreamReset marks every existing injection as belonging to a stream
   * that is no longer current.
   *
   * Called on EVERY connection-state change, not only on a drop. A transition
   * to "connected" is a new stream; "connecting" and "error" both mean the old
   * one is going or gone. Bumping on all of them is deliberately conservative:
   * the cost of an unnecessary bump is one extra session-define, and the cost
   * of a missed one is a run that silently executes the deployed construct.
   */
  noteStreamReset(): void {
    this.epoch++;
  }

  /**
   * needsInjection reports whether `sources` must be session-defined on
   * `clusterName` before a call can be trusted to hit the buffer.
   *
   * True when there is no record, when the record was made on an earlier
   * stream (the reconnect case), or when the buffer has changed since -- which
   * is what makes an unsaved edit take effect on the very next run with no
   * save and no redeploy.
   */
  needsInjection(clusterName: string, sources: string): boolean {
    const record = this.injected.get(clusterName);
    if (record === undefined) return true;
    if (record.epoch !== this.epoch) return true;
    return record.sources !== sources;
  }

  /** recordInjection notes a SUCCESSFUL session-define against the current stream. */
  recordInjection(clusterName: string, sources: string): void {
    this.injected.set(clusterName, { sources, epoch: this.epoch });
  }

  /**
   * forget drops the record for a cluster.
   *
   * Called when a session-define FAILS. Leaving the previous record in place
   * would let the next run decide nothing needs injecting and invoke against a
   * registry that no longer holds what the record claims -- the same silent
   * wrong-code failure, arrived at from the other direction.
   */
  forget(clusterName: string): void {
    this.injected.delete(clusterName);
  }

  /** lastInjected returns the sources currently believed live on `clusterName`, if any. */
  lastInjected(clusterName: string): string | undefined {
    const record = this.injected.get(clusterName);
    if (record === undefined || record.epoch !== this.epoch) return undefined;
    return record.sources;
  }
}
