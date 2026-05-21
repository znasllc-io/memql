// Connection opens a WebSocket to `/memql/ws`, performs the
// ClientHello / ServerHello handshake, and exposes the Dispatcher /
// QueryClient / SubscriptionManager triad described in the spec.
//
// Browser-first: the default transport is the `/memql/ws` JSON bridge
// (the only path browsers can use; raw gRPC requires the future
// Node-native build). The endpoint may be an absolute `wss://` URL or
// a relative path -- relative paths resolve against the current
// document origin when run in a browser.

import { Dispatcher, type DispatcherOptions } from "./dispatcher.js";
import { QueryClient } from "./query.js";
import { SubscriptionManager } from "./subscriptions.js";
import { newShortId } from "./id.js";
import { readServerPayload } from "./wire.js";

export interface ConnectionAuth {
  // Bearer JWT issued by the identity service.
  bearer?: string;
  // Guest invitation token (used for the unauthenticated /join/<token>
  // flow). Mutually exclusive with bearer.
  guestToken?: string;
  // Worker token (mql_wkr_<...>) for worker-paired flows.
  workerToken?: string;
  // Called when the server reports the current bearer is no longer
  // valid (e.g. rotateAuth returns ok=false). The hook should return
  // a fresh bearer (or null to give up). Resolution is awaited.
  onTokenExpired?: () => Promise<string | null>;
}

export interface ConnectOptions {
  // The full WebSocket URL (wss://host/memql/ws) OR a path that will
  // be resolved relative to the current document origin in a browser.
  endpoint: string;
  auth?: ConnectionAuth;
  // SDK identification surfaced in the ClientHello envelope. Logs +
  // audit trails on the server side use these.
  clientId?: string;
  sdkName?: string;
  sdkVersion?: string;
  // WebSocket factory override. The default uses globalThis.WebSocket.
  // Exposed for tests + Node consumers that ship their own ws impl.
  webSocketFactory?: (url: string) => WebSocket;
  logger?: DispatcherOptions["logger"];
  // Override the handshake timeout (default 5_000 ms).
  handshakeTimeoutMs?: number;
}

const DEFAULT_HANDSHAKE_TIMEOUT_MS = 5_000;

export class Connection {
  readonly dispatcher: Dispatcher;
  readonly query: QueryClient;
  readonly subscriptions: SubscriptionManager;

  // Server-stamped identity from ServerHello.
  nodeId = "";
  serverVersion = "";

  private readonly socket: WebSocket;
  private readonly logger: DispatcherOptions["logger"];
  private readonly auth: ConnectionAuth | undefined;
  private closed = false;

  private constructor(socket: WebSocket, opts: ConnectOptions) {
    this.socket = socket;
    this.logger = opts.logger ?? null;
    this.auth = opts.auth;
    this.dispatcher = new Dispatcher({ socket, logger: this.logger });
    this.query = new QueryClient(this.dispatcher);
    this.subscriptions = new SubscriptionManager(this.dispatcher);
  }

  static async dial(opts: ConnectOptions): Promise<Connection> {
    const url = resolveEndpoint(opts.endpoint, opts.auth);
    const factory = opts.webSocketFactory ?? defaultWebSocketFactory;
    const socket = factory(url);
    await waitForOpen(socket);
    const conn = new Connection(socket, opts);
    try {
      await conn.handshake(opts);
    } catch (err) {
      conn.close();
      throw err;
    }
    return conn;
  }

  // rotateAuth swaps the bearer on the live stream (mirrors sdk/go's
  // Dispatcher.RotateAuth). Returns true on success; false when the
  // server refused the new token. Transport / protocol failures
  // throw.
  async rotateAuth(accessToken: string): Promise<boolean> {
    const trimmed = accessToken.trim();
    if (!trimmed) throw new Error("rotateAuth: empty access token");
    const resp = await this.dispatcher.sendAndWait({
      rotateAuth: { accessToken: trimmed },
    });
    const payload = readServerPayload(resp);
    if (payload?.kind !== "rotateAuthResult") {
      throw new Error("rotateAuth: server reply missing rotateAuthResult payload");
    }
    return payload.value.ok === true;
  }

  // close shuts down the stream + subscriptions. Idempotent.
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.subscriptions.stop();
    this.dispatcher.stop();
    try {
      this.socket.close(1000, "client closing");
    } catch {
      // socket may already be closed
    }
  }

  // done resolves on any termination of the underlying stream.
  done(): Promise<void> {
    return this.dispatcher.done();
  }

  private async handshake(opts: ConnectOptions): Promise<void> {
    const timeoutMs = opts.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS;
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), timeoutMs);
    try {
      const resp = await this.dispatcher.sendAndWait(
        {
          messageId: newShortId(),
          clientHello: {
            clientId: opts.clientId ?? "memql-sdk-ts",
            sdkName: opts.sdkName ?? "memql-sdk-ts",
            sdkVersion: opts.sdkVersion ?? "0.1.0",
          },
        },
        ac.signal,
      );
      const payload = readServerPayload(resp);
      if (payload?.kind === "serverHello") {
        this.nodeId = payload.value.nodeId ?? "";
        this.serverVersion = payload.value.version ?? "";
      }
    } finally {
      clearTimeout(timer);
    }
  }
}

function defaultWebSocketFactory(url: string): WebSocket {
  if (typeof WebSocket === "undefined") {
    throw new Error(
      "memql sdk: no global WebSocket available -- pass webSocketFactory or run in a browser / Node 22+",
    );
  }
  return new WebSocket(url);
}

function waitForOpen(socket: WebSocket): Promise<void> {
  if (socket.readyState === WebSocket.OPEN) return Promise.resolve();
  return new Promise<void>((resolve, reject) => {
    const onOpen = () => {
      cleanup();
      resolve();
    };
    const onError = (ev: Event) => {
      cleanup();
      reject(new Error(`websocket open failed: ${(ev as ErrorEvent).message ?? "error"}`));
    };
    const onClose = (ev: CloseEvent) => {
      cleanup();
      reject(new Error(`websocket closed before open: code=${ev.code} reason=${ev.reason}`));
    };
    const cleanup = () => {
      socket.removeEventListener("open", onOpen);
      socket.removeEventListener("error", onError);
      socket.removeEventListener("close", onClose);
    };
    socket.addEventListener("open", onOpen);
    socket.addEventListener("error", onError);
    socket.addEventListener("close", onClose);
  });
}

// resolveEndpoint stamps the auth credential onto the URL. Browsers
// can't send custom headers on the WebSocket upgrade, so guest tokens
// go on the query string (matching the server-side
// `?guest_token=<token>` accepted path in component/grpc/...) and
// bearer JWTs piggyback as the `bearer_token` query string. The
// gRPC interceptor admits both forms.
function resolveEndpoint(endpoint: string, auth: ConnectionAuth | undefined): string {
  let url: URL;
  if (/^wss?:\/\//i.test(endpoint)) {
    url = new URL(endpoint);
  } else if (typeof globalThis.location !== "undefined") {
    const loc = globalThis.location;
    const proto = loc.protocol === "https:" ? "wss:" : "ws:";
    url = new URL(endpoint, `${proto}//${loc.host}`);
  } else {
    throw new Error(`memql sdk: cannot resolve relative endpoint ${endpoint} without a window.location`);
  }
  if (auth?.guestToken) url.searchParams.set("guest_token", auth.guestToken);
  if (auth?.workerToken) url.searchParams.set("worker_token", auth.workerToken);
  if (auth?.bearer) url.searchParams.set("bearer_token", auth.bearer);
  return url.toString();
}
