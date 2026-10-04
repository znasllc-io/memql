// "Use a code instead", without giving up on the browser.
//
// THE PROBLEM. A browser sign-in waits on a page the person may never finish:
// the tab was closed, the magic link has not arrived, the page cannot reach
// this machine. The only way out used to be Cancel and then a palette command
// named in a hint after a quiet minute. And the device-code fallback could not
// simply take over, because closing the loopback listener under a live tab is
// the memql#4594 failure: the tab's redirect then lands on a dead port.
//
// THE SHAPE. After a while without a callback the editor offers a code
// (CODE_OFFER_AFTER_MS). Taking it starts the device grant BESIDE the browser
// flow, not instead of it: both run, the first to produce tokens wins, and the
// other is stopped. So a late callback still completes the sign-in, and a
// person who switched to a code on their phone is not stranded by a tab that
// never comes back. The toast that makes the offer, and any page button that
// takes it, both call `useCode()`; everything here is free of `vscode` so the
// race itself is testable.
//
// FAILURE. One side failing does not end the sign-in while the other is still
// running. Only when both have settled is the sign-in a failure, reported with
// the device grant's error when the person chose it (the more recent intent),
// else the browser's. An outer cancel stops both.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { AuthFlowError, isAuthFlowError } from "./errors.js";
import type { AuthFlowTokens } from "./flow.js";

/** How long the browser has before a code is offered beside it. */
export const CODE_OFFER_AFTER_MS = 30_000;

export interface BrowserOrCodeDeps {
  /** The browser sign-in (loopback, with its own automatic device fallback). */
  runBrowser: (signal: AbortSignal) => Promise<AuthFlowTokens>;
  /** The device grant, started only when the person takes the offer. */
  runDeviceCode: (signal: AbortSignal) => Promise<AuthFlowTokens>;
  /** The sign-in's own cancellation (the progress notification's Cancel). */
  signal?: AbortSignal;
}

/** One sign-in that may finish in the browser or with a code, whichever comes first. */
export class BrowserOrCodeSignIn {
  readonly result: Promise<AuthFlowTokens>;

  private readonly browserAbort = new AbortController();
  private readonly deviceAbort = new AbortController();
  private browserDone = false;
  private browserError: unknown;
  private deviceStarted = false;
  private deviceDone = false;
  private deviceError: unknown;
  private done = false;
  private resolve!: (tokens: AuthFlowTokens) => void;
  private reject!: (err: unknown) => void;
  private readonly deps: BrowserOrCodeDeps;

  constructor(deps: BrowserOrCodeDeps) {
    this.deps = deps;
    this.result = new Promise<AuthFlowTokens>((resolve, reject) => {
      this.resolve = resolve;
      this.reject = reject;
    });
    const outer = deps.signal;
    if (outer !== undefined) {
      const stop = (): void => {
        this.browserAbort.abort();
        this.deviceAbort.abort();
      };
      if (outer.aborted) stop();
      else outer.addEventListener("abort", stop, { once: true });
    }
    deps.runBrowser(this.browserAbort.signal).then(
      (tokens) => this.win(tokens, "browser"),
      (err: unknown) => {
        this.browserDone = true;
        this.browserError = err;
        this.maybeFail();
      },
    );
  }

  /** Whether the sign-in has finished, one way or the other. */
  get settled(): boolean {
    return this.done;
  }

  /** Whether the device grant has been started beside the browser. */
  get codeStarted(): boolean {
    return this.deviceStarted;
  }

  /**
   * Take the offer: start the device grant beside the browser flow. Returns
   * false (and does nothing) once the sign-in has settled, when a code is
   * already running, or after the browser flow has already given up.
   */
  useCode(): boolean {
    if (this.done || this.deviceStarted || this.browserDone) return false;
    if (this.deps.signal?.aborted === true) return false;
    this.deviceStarted = true;
    this.deps.runDeviceCode(this.deviceAbort.signal).then(
      (tokens) => this.win(tokens, "device"),
      (err: unknown) => {
        this.deviceDone = true;
        this.deviceError = err;
        this.maybeFail();
      },
    );
    return true;
  }

  private win(tokens: AuthFlowTokens, side: "browser" | "device"): void {
    if (this.done) return;
    this.done = true;
    // Stop the loser. Aborting the browser closes its listener (the tab, if it
    // ever comes back, is told the sign-in did not finish there); aborting the
    // device grant stops the polling.
    if (side === "browser") this.deviceAbort.abort();
    else this.browserAbort.abort();
    this.resolve(tokens);
  }

  private maybeFail(): void {
    if (this.done) return;
    const browserSettled = this.browserDone;
    const deviceSettled = !this.deviceStarted || this.deviceDone;
    if (!browserSettled || !deviceSettled) return;
    this.done = true;
    this.reject(this.reportedError());
  }

  // The device grant's error when the person chose it and it failed on its own
  // terms; otherwise the browser's. A `cancelled` from either side is only the
  // answer when both were cancelled.
  private reportedError(): unknown {
    const meaningful = (err: unknown): boolean =>
      err !== undefined && !(isAuthFlowError(err) && err.kind === "cancelled");
    if (this.deviceStarted && meaningful(this.deviceError)) return this.deviceError;
    if (meaningful(this.browserError)) return this.browserError;
    return this.browserError ?? this.deviceError ?? new AuthFlowError("cancelled", "Sign-in was cancelled.");
  }
}
