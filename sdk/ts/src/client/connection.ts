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
import { wsAuthSubprotocols } from "./wsauth.js";
import {
  uploadAttachment,
  type AttachmentRef,
  type UploadAttachmentParams,
} from "./attachments.js";
import { QueryClient } from "./query.js";
import { SubscriptionManager } from "./subscriptions.js";
import { newShortId } from "./id.js";
import { readServerPayload } from "./wire.js";
import { WS_OPEN } from "./wsReadyState.js";

export interface ConnectionAuth {
  // Bearer JWT issued by the identity service.
  bearer?: string;
  // Guest invitation token (used for the unauthenticated /join/<token>
  // flow). Mutually exclusive with bearer.
  guestToken?: string;
  // Worker token (mql_wkr_<...>) for worker-paired flows.
  workerToken?: string;
  // Called when a fresh bearer is needed. The hook should return a
  // fresh bearer (or null to give up); resolution is awaited. Invoked
  // (a) proactively by the SDK's auto-rotation timer shortly before the
  // current bearer expires, and (b) reactively if the server reports the
  // current bearer is no longer valid. Supplying this hook is all a
  // consumer needs for in-place WS re-auth -- the SDK owns the expiry
  // timer + rotateAuth round-trip (#1110); consumers no longer reimplement
  // it. Guest/worker tokens don't carry an exp and are never auto-rotated.
  onTokenExpired?: () => Promise<string | null>;
  // Legacy transport opt-in (#2524). When true, `bearer` / `guestToken` are
  // stamped onto the dial URL as the deprecated `?bearer_token=` /
  // `?guest_token=` query params instead of riding the WebSocket subprotocol
  // channel. The credential then leaks into ingress/proxy access logs, so this
  // is ONLY for talking to an older front door that doesn't negotiate the
  // subprotocol scheme; default (unset/false) is subprotocol carry. In-place
  // auto-rotation is unaffected -- it is driven by the auth source, not the
  // transport, so it works under either carry.
  legacyUrlToken?: boolean;
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
  // `protocols` carries the auth subprotocol pair (["bearer"|"guest",
  // token], #2511); a factory MUST forward it to its WebSocket
  // constructor or authenticated dials will fail.
  webSocketFactory?: (url: string, protocols?: string[]) => WebSocket;
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
  // The endpoint this connection dialed. Retained so HTTP helpers
  // (uploadAttachment, #2523) can derive the front-door origin the same way
  // resolveEndpoint derives the WebSocket URL.
  private readonly endpoint: string;
  private closed = false;
  // Auto-rotation (#1110): the SDK decodes the bearer's exp and rotates
  // in place shortly before TTL so a steady-state stream is never torn
  // down + redialed on token refresh. Null when there's nothing to rotate
  // (guest/worker token, no onTokenExpired hook, or a bearer with no exp).
  private rotateTimer: ReturnType<typeof setTimeout> | null = null;
  private currentBearer: string | undefined;

  private constructor(socket: WebSocket, opts: ConnectOptions) {
    this.socket = socket;
    this.logger = opts.logger ?? null;
    this.auth = opts.auth;
    this.endpoint = opts.endpoint;
    // Seed the current bearer so HTTP helpers and rotation share one source of
    // truth; rotateAuth advances it on every successful swap.
    this.currentBearer = opts.auth?.bearer;
    this.dispatcher = new Dispatcher({ socket, logger: this.logger });
    this.query = new QueryClient(this.dispatcher);
    this.subscriptions = new SubscriptionManager(this.dispatcher);
  }

  static async dial(opts: ConnectOptions): Promise<Connection> {
    const url = resolveEndpoint(opts.endpoint, opts.auth);
    const factory = opts.webSocketFactory ?? defaultWebSocketFactory;
    const socket = factory(url, wsAuthSubprotocols(opts.auth));
    await waitForOpen(socket);
    const conn = new Connection(socket, opts);
    try {
      await conn.handshake(opts);
    } catch (err) {
      conn.close();
      throw err;
    }
    conn.startAutoRotate();
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
    const ok = payload.value.ok === true;
    // Advance the current bearer on success so any subsequent HTTP helper
    // (uploadAttachment, #2523) sends the rotated token -- both the SDK's
    // auto-rotation timer and a consumer's manual rotateAuth flow through here.
    if (ok) this.currentBearer = trimmed;
    return ok;
  }

  // uploadAttachment POSTs a file to the space's attachment endpoint on this
  // connection's front door, authenticated with the connection's CURRENT
  // bearer (post-rotation), and returns the created attachment reference
  // (memql#2523). Only bearer-authenticated connections can upload; a
  // guest/worker connection has no bearer and this rejects.
  async uploadAttachment(params: UploadAttachmentParams): Promise<AttachmentRef> {
    return uploadAttachment(
      {
        authToken: () => this.currentBearer,
        attachmentBaseUrl: () => this.attachmentBaseUrl(),
      },
      params,
    );
  }

  // attachmentBaseUrl derives the HTTP(S) base of the front door from the
  // dialed WebSocket endpoint (wss -> https, ws -> http), resolving a relative
  // endpoint against the document origin exactly like resolveEndpoint. The
  // bridge's `/memql/ws` suffix is stripped so any deployment base-path prefix
  // (sanitizeBaseURLFromEnv registers `/{prefix}/memql/ws` and
  // `/{prefix}/spaces/...` together) is preserved for the attachments path;
  // with no prefix this is just the origin.
  private attachmentBaseUrl(): string {
    const wsUrl = resolveWsUrl(this.endpoint);
    const httpProto = wsUrl.protocol === "wss:" ? "https:" : "http:";
    const prefix = wsUrl.pathname.replace(/\/memql\/ws\/?$/, "").replace(/\/+$/, "");
    return `${httpProto}//${wsUrl.host}${prefix}`;
  }

  // startAutoRotate arms the in-place re-auth timer (#1110). No-op unless a
  // bearer (with a decodable exp) AND an onTokenExpired hook are present --
  // guest/worker tokens and exp-less bearers are left alone.
  private startAutoRotate(): void {
    if (!this.auth?.onTokenExpired || !this.auth.bearer) return;
    this.scheduleAutoRotate(this.auth.bearer);
  }

  private scheduleAutoRotate(bearer: string): void {
    this.clearRotateTimer();
    if (this.closed) return; // never arm a timer on a closed connection
    this.currentBearer = bearer;
    const exp = decodeJwtExp(bearer);
    if (exp == null) return; // no exp -> nothing to schedule against (skip)
    const delay = computeRotateDelayMs(exp, Date.now());
    this.rotateTimer = setTimeout(() => {
      void this.performAutoRotate();
    }, delay);
  }

  // performAutoRotate fetches a fresh bearer via onTokenExpired and rotates it
  // in place. On success it reschedules against the new token's exp; on
  // failure it retries within the remaining TTL window so a transient refresh
  // hiccup doesn't fall through to a reconnect.
  private async performAutoRotate(retriesLeft = 2): Promise<void> {
    if (this.closed) return;
    const hook = this.auth?.onTokenExpired;
    if (!hook) return;

    let next: string | null = null;
    try {
      next = await hook();
    } catch (err) {
      this.logger?.warn?.("memql sdk: onTokenExpired threw during auto-rotate", err);
    }
    if (this.closed) return;

    const trimmed = next?.trim();
    if (trimmed) {
      try {
        const ok = await this.rotateAuth(trimmed);
        if (this.closed) return; // closed during the rotate round-trip
        if (ok) {
          this.scheduleAutoRotate(trimmed); // reschedule on the fresh token
          return;
        }
        this.logger?.warn?.("memql sdk: rotateAuth refused the rotated bearer");
      } catch (err) {
        this.logger?.warn?.("memql sdk: rotateAuth threw during auto-rotate", err);
      }
    }

    // Failed to rotate: retry partway through whatever TTL remains, so we get
    // another shot before the bearer actually expires.
    if (retriesLeft > 0 && !this.closed) {
      const exp = this.currentBearer ? decodeJwtExp(this.currentBearer) : null;
      const remainingMs = exp != null ? exp * 1000 - Date.now() : 0;
      if (remainingMs > 0) {
        const retryDelay = Math.max(1_000, Math.floor(remainingMs / (retriesLeft + 1)));
        this.rotateTimer = setTimeout(() => {
          void this.performAutoRotate(retriesLeft - 1);
        }, retryDelay);
      }
    }
  }

  private clearRotateTimer(): void {
    if (this.rotateTimer != null) {
      clearTimeout(this.rotateTimer);
      this.rotateTimer = null;
    }
  }

  // close shuts down the stream + subscriptions. Idempotent.
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.clearRotateTimer();
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

// computeRotateDelayMs returns how long to wait before rotating a bearer that
// expires at `expSeconds` (unix seconds), given `nowMs`. It fires at `fraction`
// of the remaining TTL (default 70%) so there's headroom to retry before the
// hard expiry; never negative, and 0 when the token is already past that point.
export function computeRotateDelayMs(
  expSeconds: number,
  nowMs: number,
  fraction = 0.7,
): number {
  const ttlMs = expSeconds * 1000 - nowMs;
  if (ttlMs <= 0) return 0;
  return Math.max(0, Math.floor(ttlMs * fraction));
}

// decodeJwtExp reads the `exp` (unix seconds) claim from a JWT WITHOUT
// verifying the signature -- it only needs the expiry to schedule rotation.
// Returns null for a non-JWT, an unparseable payload, or a missing/non-numeric
// exp (callers treat null as "don't auto-rotate this token").
export function decodeJwtExp(jwt: string): number | null {
  const payload = jwt.split(".")[1];
  if (!payload) return null;
  try {
    const claims = JSON.parse(base64UrlDecode(payload)) as { exp?: unknown };
    return typeof claims.exp === "number" && Number.isFinite(claims.exp)
      ? claims.exp
      : null;
  } catch {
    return null;
  }
}

function base64UrlDecode(segment: string): string {
  let b64 = segment.replace(/-/g, "+").replace(/_/g, "/");
  const pad = b64.length % 4;
  if (pad) b64 += "=".repeat(4 - pad);
  // Prefer atob (browsers + Node 16+); decode bytes as UTF-8.
  if (typeof atob === "function") {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  }
  // Node fallback without an @types/node dependency -- probe globalThis for
  // Buffer rather than referencing the bare identifier (mirrors src/si/speech.ts).
  const g = globalThis as {
    Buffer?: { from(s: string, enc: string): { toString(enc: string): string } };
  };
  if (g.Buffer) return g.Buffer.from(b64, "base64").toString("utf-8");
  throw new Error("memql sdk: no base64 decoder available (need atob or Buffer)");
}

function defaultWebSocketFactory(url: string, protocols?: string[]): WebSocket {
  if (typeof WebSocket === "undefined") {
    throw new Error(
      "memql sdk: no global WebSocket available -- pass webSocketFactory or run in a browser / Node 22+",
    );
  }
  return new WebSocket(url, protocols);
}

function waitForOpen(socket: WebSocket): Promise<void> {
  if (socket.readyState === WS_OPEN) return Promise.resolve();
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

// resolveWsUrl parses the dial endpoint into a URL WITHOUT stamping any
// credential. An absolute wss:// / ws:// endpoint is taken verbatim; a
// relative path resolves against the current document origin in a browser.
// Shared by the WS dial and the HTTP attachment-origin derivation (#2523).
function resolveWsUrl(endpoint: string): URL {
  if (/^wss?:\/\//i.test(endpoint)) return new URL(endpoint);
  if (typeof globalThis.location !== "undefined") {
    const loc = globalThis.location;
    const proto = loc.protocol === "https:" ? "wss:" : "ws:";
    return new URL(endpoint, `${proto}//${loc.host}`);
  }
  throw new Error(`memql sdk: cannot resolve relative endpoint ${endpoint} without a window.location`);
}

// resolveEndpoint resolves the dial URL. By default bearer and guest
// credentials travel as WebSocket subprotocols (#2511, see wsauth.ts), NOT on
// the query string -- the URL stays free of live tokens. The `legacyUrlToken`
// opt-in (#2524) restores the deprecated `?bearer_token=` / `?guest_token=`
// carry for older front doors that don't negotiate the subprotocol scheme.
// Worker tokens remain on the query string until the worker surface migrates.
function resolveEndpoint(endpoint: string, auth: ConnectionAuth | undefined): string {
  const url = resolveWsUrl(endpoint);
  if (auth?.legacyUrlToken) {
    if (auth.bearer) url.searchParams.set("bearer_token", auth.bearer);
    else if (auth.guestToken) url.searchParams.set("guest_token", auth.guestToken);
  }
  if (auth?.workerToken) url.searchParams.set("worker_token", auth.workerToken);
  return url.toString();
}
