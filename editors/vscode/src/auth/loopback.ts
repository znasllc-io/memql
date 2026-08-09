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
// (`http://127.0.0.1/callback`, see register.ts) and identity's RFC 8252
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
// listener keeps waiting. The FIRST request on the callback path is served, the
// flow resolves, and the server closes -- there is no second chance to redeem,
// which is what "one-shot" buys.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { AuthFlowError, errorText } from "./errors.js";

/** The only interface this listener is ever allowed to bind. */
export const LOOPBACK_HOST = "127.0.0.1";

/** The path the callback arrives on. Must match the registered redirect URI. */
export const CALLBACK_PATH = "/callback";

/**
 * How long the listener waits for the callback before giving up.
 *
 * Two minutes is long enough for a person to read an authorization page,
 * switch to a password manager, and click; short enough that an abandoned
 * sign-in does not leave a port open for the rest of the session.
 */
export const DEFAULT_CALLBACK_TIMEOUT_MS = 120_000;

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
  /** Closes the server. A still-pending wait rejects as `cancelled`. */
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

// The page the browser lands on. Deliberately tiny and self-contained: it is
// served from a throwaway loopback port that is gone a moment later, so it can
// reference no stylesheet, no font and no image.
const SUCCESS_PAGE = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Signed in to memQL</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f5f6f8;color:#1a1a1a;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid #e2e4e9;border-radius:10px;padding:2rem;max-width:24rem;margin:1rem;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.06)}
h1{font-size:1.15rem;margin:0 0 .5rem}
p{color:#555;line-height:1.5;margin:0}
</style>
</head>
<body>
<div class="card">
<h1>Signed in to memQL</h1>
<p>You can close this tab and return to VS Code.</p>
</div>
</body>
</html>`;

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

  const teardown = (): void => {
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
    // Every response this server writes carries `Connection: close`, so a
    // socket that has been answered ends on its own. This sweeps the other
    // case -- a connection opened that never sent a complete request, which
    // would otherwise hold the server handle (and the event loop) open. It is
    // deferred by one tick so it can never race a response still flushing, and
    // it is an optional call because closeAllConnections only exists from Node
    // 18.2; the extension targets node20, but the cast says so rather than
    // assuming it.
    setImmediate(() => {
      (server as { closeAllConnections?: () => void }).closeAllConnections?.();
    });
  };

  const succeed = (params: CallbackParams): void => {
    if (settled) return;
    settled = true;
    resolveCallback(params);
    teardown();
  };

  const fail = (err: AuthFlowError): void => {
    if (settled) return;
    settled = true;
    rejectCallback(err);
    teardown();
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
    if (url.pathname !== path) {
      // A favicon fetch, a prefetch, a stray tab. Answered and IGNORED: the
      // flow is still waiting for the real callback.
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
    res.writeHead(200, {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      // No keep-alive: this server is about to stop existing, and a browser
      // holding the socket open would keep the handle (and the event loop)
      // alive well past the flow it belongs to.
      connection: "close",
    });
    // Settle only once the page has flushed: teardown() drops open sockets, so
    // resolving first would race the response the person is waiting to see.
    res.end(SUCCESS_PAGE, () => succeed(params));
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
