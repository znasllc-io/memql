// The one-shot loopback listener that catches the OAuth callback.
//
// -----------------------------------------------------------------------------
// WHY 127.0.0.1 AND NOT 0.0.0.0
// -----------------------------------------------------------------------------
//
// For the few seconds this server is up it will hand whatever arrives on the
// callback path to the token exchange. Binding 0.0.0.0 publishes that on every
// interface the machine has -- a coffee-shop network, a corporate LAN, a
// container bridge -- so anyone who can reach the port can deliver a callback.
// The `state` check downstream is what makes a forged one useless, but a
// listener nobody off-machine can reach never has to win that argument. RFC 8252
// §7.3 assumes a loopback bind for exactly this reason, and the host is a
// constant here rather than an option so no caller can widen it.
//
// PORT 0 asks the kernel for an ephemeral port, which is what makes this work
// without reserving anything: the redirect URI is REGISTERED portless
// (`http://127.0.0.1/callback`, see wellKnownClient.ts) and identity's RFC 8252
// matcher accepts any port on a loopback URI whose scheme, host and path agree
// (component/identity/config.go, matchesLoopbackAnyPort).
//
// -----------------------------------------------------------------------------
// ONE REQUEST, ONE PATH
// -----------------------------------------------------------------------------
//
// A browser sends more than the callback. A favicon fetch, a prefetch, a stray
// tab pointed at the port -- each is a request this server sees. Resolving on
// "the first request" would let any of them end the flow with no code at all,
// so only the callback path resolves; everything else gets a 404 and the
// listener keeps waiting. The FIRST request on the callback path resolves the
// flow and the server stops accepting -- there is no second chance to redeem,
// which is what "one-shot" buys.
//
// -----------------------------------------------------------------------------
// THE BROWSER IS ANSWERED AFTER THE EXCHANGE, NOT BEFORE
// -----------------------------------------------------------------------------
//
// The callback's response is HELD until the flow calls `finish` with how the
// code exchange went, and then the browser gets "You're signed in" or "Sign-in
// didn't finish" (loopbackPage.ts). It used to be answered "Signed in" on
// arrival, before the code had been redeemed, so a refused exchange left the
// browser claiming a sign-in the editor did not have. A flow that never calls
// `finish` is covered twice: `close()` answers a held response with the
// failure page, and a bound (HOLD_LIMIT_MS) answers it if nothing else does.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { AuthFlowError, errorText } from "./errors.js";
import { loopbackPage, type CallbackOutcome } from "./loopbackPage.js";

export type { CallbackOutcome } from "./loopbackPage.js";

/** The only interface this listener is ever allowed to bind. */
export const LOOPBACK_HOST = "127.0.0.1";

/** The path the callback arrives on. Must match the registered redirect URI. */
export const CALLBACK_PATH = "/callback";

/**
 * How long the listener waits for the callback before giving up.
 *
 * Sized for the MAGIC-LINK round trip, which is the identity service's
 * primary human factor: enter an email, wait for the mail to arrive, click
 * the link, approve, and let the requesting tab finish -- routinely longer
 * than the two minutes this used to be. Two minutes abandoned people
 * mid-sign-in (memql#4594): the deadline fired, the then-automatic device
 * fallback closed this listener, and the browser's eventual redirect hit a
 * dead port. Ten minutes matches the magic-link TTL and the
 * device-authorization window (both 600s defaults), and the progress
 * notification carrying the wait is cancellable the whole time, so the
 * longer deadline is a bounded backstop rather than a trap.
 */
export const DEFAULT_CALLBACK_TIMEOUT_MS = 600_000;

/**
 * The longest a delivered callback's response is held for the flow's verdict.
 * The exchange is one POST; anything past this is a flow that will not answer,
 * and the browser is told the sign-in did not finish rather than left loading.
 */
export const HOLD_LIMIT_MS = 60_000;

/** The raw query parameters the callback carried. Interpreting them is flow.ts's job. */
export interface CallbackParams {
  code?: string;
  state?: string;
  error?: string;
  errorDescription?: string;
}

export interface LoopbackListener {
  /**
   * The address actually bound, read back off the socket.
   *
   * Exposed rather than assumed so a test can assert the real bind is
   * LOOPBACK_HOST and not something wider -- the difference between 127.0.0.1
   * and 0.0.0.0 is invisible in a redirect URI, which is composed from a
   * constant either way.
   */
  readonly host: string;
  /** The ephemeral port the kernel assigned. */
  readonly port: number;
  /** The redirect_uri to send to /authorize -- this one carries the port. */
  readonly redirectUri: string;
  /**
   * Resolves with the callback's query parameters, or rejects with an
   * AuthFlowError of kind `timeout` or `cancelled`.
   *
   * The deadline starts when the listener starts, not when this is called, so
   * a caller cannot extend it by waiting to await.
   */
  waitForCallback(): Promise<CallbackParams>;
  /**
   * Answers the browser that delivered the callback, once the exchange has
   * settled. Idempotent; a no-op before a callback has arrived.
   *
   * Optional so a test double need not implement it; the real listener does.
   */
  finish?(outcome: CallbackOutcome): void;
  /**
   * Closes the server. A still-pending wait rejects as `cancelled`, and a held
   * callback response is answered with the failure page.
   */
  close(): void;
}

export interface LoopbackOptions {
  /** Overrides the callback path. Defaults to CALLBACK_PATH. */
  path?: string;
  /** Overrides the deadline. Defaults to DEFAULT_CALLBACK_TIMEOUT_MS. */
  timeoutMs?: number;
  /** Aborts the wait -- rejects as `cancelled` and closes the server. */
  signal?: AbortSignal;
}

/**
 * startLoopbackListener binds 127.0.0.1 on an ephemeral port and waits for one
 * callback.
 *
 * Rejects with an AuthFlowError of kind `bindFailed` when the socket cannot be
 * bound -- distinguishable from every later failure, because nothing has been
 * opened in a browser yet and no code exists anywhere.
 */
export async function startLoopbackListener(
  options: LoopbackOptions = {},
): Promise<LoopbackListener> {
  const path = options.path ?? CALLBACK_PATH;
  const timeoutMs = options.timeoutMs ?? DEFAULT_CALLBACK_TIMEOUT_MS;
  const signal = options.signal;

  let settled = false;
  let timer: NodeJS.Timeout | undefined;
  let abortListener: (() => void) | undefined;
  // The callback's response, held until `finish` says how the exchange went.
  let held: ServerResponse | undefined;
  let holdTimer: NodeJS.Timeout | undefined;
  let answered = false;

  let resolveCallback!: (params: CallbackParams) => void;
  let rejectCallback!: (err: unknown) => void;
  const callbackPromise = new Promise<CallbackParams>((resolve, reject) => {
    resolveCallback = resolve;
    rejectCallback = reject;
  });
  // The promise is handed out by waitForCallback(), but close() may reject it
  // before anyone has asked for it. An unobserved rejection crashes a Node
  // process under `node --test`, so a no-op handler is attached up front; real
  // awaiters still see the rejection.
  callbackPromise.catch(() => {});

  const server: Server = createServer();

  // Drops every socket still open. Deferred by one tick so it can never race a
  // response still flushing, and an optional call because closeAllConnections
  // only exists from Node 18.2; the extension targets node20, but the cast
  // says so rather than assuming it. It sweeps a connection that opened and
  // never sent a complete request, which would otherwise hold the server
  // handle (and the event loop) open.
  const dropSockets = (): void => {
    setImmediate(() => {
      (server as { closeAllConnections?: () => void }).closeAllConnections?.();
    });
  };

  // Stops listening and clears the wait's own bookkeeping. The held response,
  // if any, is left for `answer` -- dropping its socket here would cut off the
  // page the person is waiting to see.
  const stopWaiting = (): void => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
    if (signal !== undefined && abortListener !== undefined) {
      signal.removeEventListener("abort", abortListener);
      abortListener = undefined;
    }
    // Stops accepting immediately, so the port is unusable from here on.
    server.close();
  };

  // Writes the verdict page to the held response and then releases the sockets.
  const answer = (outcome: CallbackOutcome): void => {
    if (answered) return;
    answered = true;
    if (holdTimer !== undefined) {
      clearTimeout(holdTimer);
      holdTimer = undefined;
    }
    const res = held;
    held = undefined;
    if (res === undefined) {
      dropSockets();
      return;
    }
    res.writeHead(200, {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      // No keep-alive: this server is about to stop existing, and a browser
      // holding the socket open would keep the handle (and the event loop)
      // alive well past the flow it belongs to.
      connection: "close",
    });
    res.end(loopbackPage(outcome), () => dropSockets());
  };

  const succeed = (params: CallbackParams): void => {
    if (settled) return;
    settled = true;
    stopWaiting();
    resolveCallback(params);
  };

  const fail = (err: AuthFlowError): void => {
    if (settled) {
      // Already delivered: the only thing left to close is a held response.
      answer("failure");
      return;
    }
    settled = true;
    stopWaiting();
    rejectCallback(err);
    answer("failure");
  };

  timer = setTimeout(() => {
    fail(
      new AuthFlowError(
        "timeout",
        `No sign-in callback arrived within ${Math.round(timeoutMs / 1000)} seconds. The browser page was never completed, or it could not reach ${LOOPBACK_HOST}.`,
      ),
    );
  }, timeoutMs);

  server.on("request", (req: IncomingMessage, res: ServerResponse) => {
    let url: URL;
    try {
      url = new URL(req.url ?? "/", `http://${LOOPBACK_HOST}`);
    } catch {
      respondNotFound(res);
      return;
    }
    if (url.pathname !== path || settled) {
      // A favicon fetch, a prefetch, a stray tab. Answered and IGNORED: the
      // flow is still waiting for the real callback (or already has it).
      respondNotFound(res);
      return;
    }
    const q = url.searchParams;
    const params: CallbackParams = {
      code: q.get("code") ?? undefined,
      state: q.get("state") ?? undefined,
      error: q.get("error") ?? undefined,
      errorDescription: q.get("error_description") ?? undefined,
    };
    // HELD, not answered: the page says how the exchange went, so it is
    // written by `finish` once the flow knows (see the header).
    held = res;
    holdTimer = setTimeout(() => answer("failure"), HOLD_LIMIT_MS);
    succeed(params);
  });

  const bound = await listen(server).catch((err: unknown) => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
    throw err;
  });

  if (signal !== undefined) {
    abortListener = () => {
      fail(new AuthFlowError("cancelled", "Sign-in was cancelled before the browser returned."));
    };
    if (signal.aborted) abortListener();
    else signal.addEventListener("abort", abortListener, { once: true });
  }

  return {
    host: bound.host,
    port: bound.port,
    redirectUri: `http://${LOOPBACK_HOST}:${bound.port}${path}`,
    waitForCallback: () => callbackPromise,
    finish: (outcome) => {
      // Before a callback there is nobody to answer.
      if (settled) answer(outcome);
    },
    close: () => {
      fail(
        new AuthFlowError(
          "cancelled",
          "The sign-in listener was closed before the browser returned.",
        ),
      );
    },
  };
}

function respondNotFound(res: ServerResponse): void {
  res.writeHead(404, {
    "content-type": "text/plain; charset=utf-8",
    "cache-control": "no-store",
    connection: "close",
  });
  res.end("not found");
}

// listen resolves with the address actually bound, or rejects `bindFailed`.
function listen(server: Server): Promise<{ host: string; port: number }> {
  return new Promise<{ host: string; port: number }>((resolve, reject) => {
    server.once("error", (err) => {
      reject(
        new AuthFlowError(
          "bindFailed",
          `Could not open a local sign-in listener on ${LOOPBACK_HOST}: ${errorText(err)}`,
          { cause: err },
        ),
      );
    });
    server.listen({ host: LOOPBACK_HOST, port: 0 }, () => {
      const address = server.address() as AddressInfo | string | null;
      if (address === null || typeof address === "string") {
        server.close();
        reject(
          new AuthFlowError(
            "bindFailed",
            `The local sign-in listener bound ${LOOPBACK_HOST} but reported no port, so there is no redirect URI to authorize against.`,
          ),
        );
        return;
      }
      resolve({ host: address.address, port: address.port });
    });
  });
}
