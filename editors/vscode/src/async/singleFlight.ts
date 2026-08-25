// A keyed join: while a flight for `key` is in the air, callers board it
// instead of launching another.
//
// The consumer this exists for is sign-in (memql#4596). Four surfaces reach
// signInToCluster -- the dial-failure toast's "Sign in" action, the palette
// command, the ownership walk, "Sign In With a Device Code" -- and nothing stopped two
// of them running the flow for one cluster at once: two loopback listeners,
// two browser tabs or device codes, two progress notifications, and the
// second credential overwriting the first. Joining is deliberately chosen
// over cancel-and-restart: the person triggering the second request is almost
// always the same person mid-way through answering the first flow's browser
// page, and killing that flow under them re-creates exactly the dead-tab
// failure memql#4594 removed.
//
// Named and shared for the same reason src/async/latest.ts is: "which async
// paths are guarded, and how" should be a grep, not a per-module rediscovery.
// Latest answers "only the newest caller may publish"; SingleFlight answers
// "everyone gets the one in-flight result".
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

export class SingleFlight<T> {
  private readonly inFlight = new Map<string, Promise<T>>();

  /**
   * run returns the in-flight promise for `key` when one exists, otherwise
   * starts `fn` and tracks it until it settles -- resolution and rejection
   * alike release the slot, so a failed attempt never haunts the next one.
   *
   * A joiner's `fn` is NOT invoked. That is the contract: the underlying
   * flow runs once, and everyone observes its outcome.
   */
  /**
   * has reports whether a flight for `key` is currently in the air. The
   * consumer uses it to TELL a joiner they are joining -- a silently absorbed
   * request reads as the command doing nothing, worst when the joiner asked
   * for a different grant than the one flying.
   */
  has(key: string): boolean {
    return this.inFlight.has(key);
  }

  run(key: string, fn: () => Promise<T>): Promise<T> {
    const existing = this.inFlight.get(key);
    if (existing !== undefined) return existing;
    // The async wrapper converts a synchronous throw into a rejection, so a
    // flow that dies before returning a promise still settles (and releases)
    // like any other failure.
    const flight = (async () => fn())().finally(() => {
      this.inFlight.delete(key);
    });
    this.inFlight.set(key, flight);
    return flight;
  }
}
