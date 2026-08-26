// The callback route that survives a remote host (memql#4623).
//
// -----------------------------------------------------------------------------
// WHY LOOPBACK IS NOT ENOUGH
// -----------------------------------------------------------------------------
//
// The loopback listener binds 127.0.0.1 ON THE MACHINE THE EXTENSION HOST RUNS
// ON. Under Remote-SSH, Codespaces or a dev container that is the SERVER, while
// the browser opens on the USER's machine and redirects to THEIR 127.0.0.1,
// where nothing is listening.
//
// Nothing detected it. The bind succeeded, `openExternal` succeeded, and
// `timeout` was deliberately removed as a fallback trigger in memql#4600 -- so
// neither fallback fired and the user watched a 600-second spinner, then read
// advice about a palette command. `asExternalUri` did not rescue it either: it
// tunnels loopback authorities, and it was being applied to the AUTHORIZE url
// rather than to the redirect, so an `https://identity...` URL came back
// unchanged and no tunnel was ever created for the callback port.
//
// -----------------------------------------------------------------------------
// WHAT THIS DOES INSTEAD
// -----------------------------------------------------------------------------
//
// A `vscode://znasllc.memql/callback` redirect is resolved by the user's OWN VS
// Code client, which forwards it to the extension wherever that runs -- across
// the remote boundary, over the connection that already exists. No port, no
// tunnel, nothing to bind.
//
// The waiter is keyed on `state`, which the flow already generates and already
// verifies. That keying is not decoration: two sign-ins can be in flight (two
// windows, two clusters), and delivering a callback to the wrong one would hand
// an authorization code to a flow that will then correctly reject it as a state
// mismatch -- while the flow it belonged to waits out its deadline.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the extension host calls `deliverUriCallback` from its own URI handler.

import { AuthFlowError } from "./errors.js";
import type { CallbackParams } from "./loopback.js";

/** The path segment of the registered vscode:// redirect. */
export const URI_CALLBACK_PATH = "/callback";

interface Pending {
  resolve: (params: CallbackParams) => void;
  reject: (err: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
}

const pending = new Map<string, Pending>();

export interface UriCallbackWaiter {
  /** The redirect_uri to send to /authorize. */
  readonly redirectUri: string;
  waitForCallback(): Promise<CallbackParams>;
  close(): void;
}

export interface UriCallbackOptions {
  /** The registered redirect URI, e.g. `vscode://znasllc.memql/callback`. */
  redirectUri: string;
  /** The `state` this sign-in will send, and the key the callback is routed by. */
  state: string;
  timeoutMs: number;
  signal?: AbortSignal;
}

/**
 * awaitUriCallback arms a waiter for one sign-in.
 *
 * Shaped like a LoopbackListener on purpose -- `redirectUri`, `waitForCallback`,
 * `close` -- so the flow chooses a transport and nothing downstream of that
 * choice has to know which one it got.
 */
export function awaitUriCallback(options: UriCallbackOptions): UriCallbackWaiter {
  const { state, timeoutMs, signal } = options;

  let settle: Pending | undefined;
  const promise = new Promise<CallbackParams>((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(state);
      reject(
        new AuthFlowError(
          "timeout",
          `No sign-in callback arrived within ${Math.round(timeoutMs / 1000)} seconds. ` +
            `The browser page was never completed, or this editor did not receive the ` +
            `${options.redirectUri} handoff from it.`,
        ),
      );
    }, timeoutMs);
    // THE TIMER IS NOT unref'd, deliberately. An unref'd timer does not keep
    // the event loop alive, so if nothing else is pending the process exits and
    // the deadline NEVER FIRES -- the waiter simply never settles. That is the
    // failure this whole file exists to remove, reintroduced one layer down.
    // A pending sign-in is a foreground action the user just started; holding
    // the loop for at most one deadline is the correct cost, and `close()`
    // releases it the moment the flow is done either way.
    settle = { resolve, reject, timer };
    pending.set(state, settle);
  });

  const abort = () => {
    const held = pending.get(state);
    if (held === undefined) return;
    pending.delete(state);
    clearTimeout(held.timer);
    held.reject(new AuthFlowError("cancelled", "The sign-in was cancelled."));
  };
  signal?.addEventListener("abort", abort, { once: true });

  return {
    redirectUri: options.redirectUri,
    waitForCallback: () => promise,
    close: () => {
      signal?.removeEventListener("abort", abort);
      const held = pending.get(state);
      if (held === undefined) return;
      pending.delete(state);
      clearTimeout(held.timer);
      held.reject(new AuthFlowError("cancelled", "The sign-in was cancelled."));
    },
  };
}

/**
 * deliverUriCallback routes an incoming `vscode://` callback to whichever
 * sign-in is waiting on its `state`.
 *
 * Returns true when it was consumed. FALSE IS NOT AN ERROR: the same URI
 * handler serves the portal's own handoff links, so a uri this does not
 * recognise belongs to somebody else and must fall through untouched.
 */
export function deliverUriCallback(query: string, path: string): boolean {
  if (path !== URI_CALLBACK_PATH) return false;

  const params = new URLSearchParams(query.startsWith("?") ? query.slice(1) : query);
  const state = params.get("state") ?? "";
  // NO STATE MEANS NO ROUTE. It is not rejected here as a mismatch -- the flow
  // owns that judgement and has the sentence for it -- but there is nothing to
  // deliver it to, so it is not ours.
  if (state === "") return false;

  const held = pending.get(state);
  if (held === undefined) return false;

  pending.delete(state);
  clearTimeout(held.timer);
  held.resolve({
    code: params.get("code") ?? "",
    state,
    error: params.get("error") ?? "",
    errorDescription: params.get("error_description") ?? "",
  });
  return true;
}

/** Test seam: how many sign-ins are armed. A leak here is a leak of timers. */
export function pendingUriCallbackCount(): number {
  return pending.size;
}
