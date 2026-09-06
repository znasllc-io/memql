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
import { QueryClient } from "./query.js";
import { SubscriptionManager } from "./subscriptions.js";
import { newShortId } from "./id.js";
import { readServerPayload } from "./wire.js";
import { WS_OPEN } from "./wsReadyState.js";

export interface ConnectionAuth {
  // Bearer JWT issued by the identity service.
  bearer?: string;
  // Worker token (mql_wkr_<...>) for worker-paired flows.
  workerToken?: string;
  // Called when a fresh bearer is needed. The hook should return a
  // fresh bearer (or null to give up); resolution is awaited. Invoked
  // (a) proactively by the SDK's auto-rotation timer shortly before the
  // current bearer expires, and (b) reactively if the server reports the
  // current bearer is no longer valid. Supplying this hook is all a
  // consumer needs for in-place WS re-auth -- the SDK owns the expiry
  // timer + rotateAuth round-trip (#1110); consumers no longer reimplement
  // it. Worker tokens don't carry an exp and are never auto-rotated.
  onTokenExpired?: () => Promise<string | null>;
  // Lower bound on the auto-rotation timer, in milliseconds. Defaults to
  // DEFAULT_ROTATE_FLOOR_MS (30s). The floor is what stops a pathological
  // schedule -- a clock-skewed browser, or a token whose whole lifetime is
  // shorter than a round trip -- from rotating at network speed (memql#4326).
  // Lower it only in a harness driving deliberately short-lived tokens;
  // lowering it in a browser re-opens the storm this floor exists to close.
  rotateFloorMs?: number;
  // Legacy transport opt-in (#2524). When true, `bearer` is stamped onto the
  // dial URL as the deprecated `?bearer_token=` query param instead of riding
  // the WebSocket subprotocol channel. The credential then leaks into
  // ingress/proxy access logs, so this
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
  // Auto-reconnect (memql#4537). Omitted / disabled keeps the historic
  // one-shot behaviour exactly.
  reconnect?: ReconnectOptions;
}

// ReconnectOptions configures the SDK's auto-reconnect (memql#4537).
//
// OPT-IN. A consumer that manages its own lifecycle off done() /
// unexpectedClose() keeps exactly the behaviour it had; enabling this moves
// that job into the SDK, once, for every consumer -- portal, product SPAs,
// site surfaces, Go tools.
export interface ReconnectOptions {
  enabled: boolean;
  // First backoff delay. Default 1s.
  initialDelayMs?: number;
  // Backoff ceiling. Default 30s.
  maxDelayMs?: number;
  // Give up after this many consecutive failed attempts. Default: never.
  maxAttempts?: number;
  // How long a stream must SURVIVE before the backoff resets (default 10s).
  //
  // Resetting on "a dial succeeded" instead would be wrong in the case that
  // matters: a server accepting connections and dropping them immediately
  // would look like success every time, and the client would hammer it at the
  // initial delay forever. Resetting on SURVIVAL means a flapping server
  // converges to the ceiling, and a healthy one that blips comes back at full
  // speed.
  stableAfterMs?: number;
}

// ConnectionStatus is what a UI renders (memql#4537).
//
//   connected     -- a live stream
//   reconnecting  -- the stream is down and the SDK is retrying
//   disconnected  -- FINAL: close() was called, or the attempt budget is spent
export type ConnectionStatus = "connected" | "reconnecting" | "disconnected";

export interface ConnectionStatusEvent {
  status: ConnectionStatus;
  // Consecutive failed dial attempts since the last live stream. 0 while
  // connected. What "Reconnecting (attempt 3)..." reads.
  attempt: number;
  // The failure that ended (or is preventing) the stream. Empty when healthy.
  error: string;
}

const DEFAULT_HANDSHAKE_TIMEOUT_MS = 5_000;
const DEFAULT_RECONNECT_INITIAL_MS = 1_000;
const DEFAULT_RECONNECT_MAX_MS = 30_000;
const DEFAULT_RECONNECT_STABLE_MS = 10_000;

export class Connection {
  readonly dispatcher: Dispatcher;
  readonly query: QueryClient;
  readonly subscriptions: SubscriptionManager;

  // Server-stamped identity from ServerHello.
  nodeId = "";
  // The wire protocol version the node speaks ("v1"), not its release.
  serverVersion = "";
  // The release the node's binary was cut from -- "v0.18.1", or "dev+<12 hex>"
  // when it was not cut from a release (memql#3998, memql#4575). Stays ""
  // against a node that predates the field, which is how a caller tells "older
  // than this contract" apart from "not a release build".
  engineVersion = "";
  // The revision that binary was built from, abbreviated to 12 hex characters
  // ("-dirty" when the tree was modified). Stays "" when the node cannot
  // establish it OR predates the field; both mean "render this as unknown",
  // which is why they are not told apart here (memql#4575).
  engineCommit = "";

  private socket: WebSocket;
  private readonly logger: DispatcherOptions["logger"];
  private readonly auth: ConnectionAuth | undefined;
  // The endpoint this connection dialed.
  private readonly endpoint: string;
  private closed = false;
  // Auto-rotation (#1110): the SDK decodes the bearer's exp and rotates
  // in place shortly before TTL so a steady-state stream is never torn
  // down + redialed on token refresh. Null when there's nothing to rotate
  // (guest/worker token, no onTokenExpired hook, or a bearer with no exp).
  private rotateTimer: ReturnType<typeof setTimeout> | null = null;
  private currentBearer: string | undefined;
  // When the current bearer ARRIVED, by this machine's clock. The rotation
  // schedule is measured from here (memql#4326).
  private bearerReceivedAtMs = 0;

  // ---- auto-reconnect (memql#4537) ----------------------------------------
  private readonly opts: ConnectOptions;
  private readonly reconnectCfg: Required<ReconnectOptions> | null;
  private statusValue: ConnectionStatus = "connected";
  private attemptCount = 0;
  private lastError = "";
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private stableTimer: ReturnType<typeof setTimeout> | null = null;
  private cycleCount = 0;
  private readonly statusListeners = new Set<(ev: ConnectionStatusEvent) => void>();
  private readonly cycleListeners = new Set<(cycle: number) => void>();
  private readonly finalResolvers: Array<() => void> = [];
  private finished = false;

  private constructor(socket: WebSocket, opts: ConnectOptions) {
    this.socket = socket;
    this.logger = opts.logger ?? null;
    this.auth = opts.auth;
    this.endpoint = opts.endpoint;
    this.opts = opts;
    this.reconnectCfg = resolveReconnect(opts.reconnect);
    // Seed the current bearer so HTTP helpers and rotation share one source of
    // truth; rotateAuth advances it on every successful swap.
    this.currentBearer = opts.auth?.bearer;
    this.dispatcher = new Dispatcher({
      socket,
      logger: this.logger,
      // Supervision is what keeps `done()` from firing on every node roll:
      // with reconnect on, only THIS class decides the connection is over.
      supervised: this.reconnectCfg !== null,
    });
    this.query = new QueryClient(this.dispatcher);
    this.subscriptions = new SubscriptionManager(this.dispatcher);
    if (this.reconnectCfg !== null) {
      this.dispatcher.onTransportClose(() => this.onTransportLost());
      this.armStableTimer();
    }
  }

  static async dial(opts: ConnectOptions): Promise<Connection> {
    const socket = await openSocket(opts, opts.auth);
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

  // ---- connection status --------------------------------------------------

  // status is what a UI renders. Without auto-reconnect it is "connected"
  // until close(), then "disconnected" -- consumers on done() are unaffected.
  get status(): ConnectionStatus {
    return this.statusValue;
  }

  // attempt counts consecutive failed dials since the last live stream.
  get attempt(): number {
    return this.attemptCount;
  }

  // onStatusChange subscribes to connection-state transitions. Fires
  // immediately with the current state, so a late subscriber does not sit on
  // a stale render waiting for the next drop.
  onStatusChange(handler: (ev: ConnectionStatusEvent) => void): () => void {
    this.statusListeners.add(handler);
    handler(this.statusEvent());
    return () => this.statusListeners.delete(handler);
  }

  // onConnectionCycle fires once per successful RECONNECT, after every
  // subscription has been replayed on the new stream (memql#4537).
  //
  // This is the seam a store re-seeds on. It fires AFTER the replay, never
  // before, because a re-seed racing its own subscription is exactly the
  // read-then-subscribe hole the ordering contract closes (memql#4536).
  //
  // Not fired for the FIRST connection: a caller that just dialled is about
  // to seed anyway, and firing here would make every consumer read twice on
  // startup.
  onConnectionCycle(handler: (cycle: number) => void): () => void {
    this.cycleListeners.add(handler);
    return () => this.cycleListeners.delete(handler);
  }

  // retryNow collapses the current backoff and dials immediately. It is what
  // a "Retry" button should call once auto-reconnect is on: the SDK is
  // already retrying, so the button ACCELERATES rather than being the only
  // mechanism. No-op unless reconnecting.
  retryNow(): void {
    if (this.reconnectCfg === null || this.finished) return;
    if (this.statusValue !== "reconnecting") return;
    this.clearReconnectTimer();
    this.attemptCount = 0;
    void this.attemptReconnect();
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
    // sends the rotated token -- both the SDK's auto-rotation timer and a
    // consumer's manual rotateAuth flow through here.
    if (ok) this.currentBearer = trimmed;
    return ok;
  }

  // startAutoRotate arms the in-place re-auth timer (#1110). No-op unless a
  // bearer (with a decodable exp) AND an onTokenExpired hook are present --
  // guest/worker tokens and exp-less bearers are left alone.
  private startAutoRotate(): void {
    // currentBearer, not auth.bearer: a reconnect re-resolves the credential,
    // so re-arming against the ORIGINAL token would schedule the next
    // rotation off an expiry that has already passed.
    const bearer = this.currentBearer ?? this.auth?.bearer;
    if (!this.auth?.onTokenExpired || !bearer) return;
    this.scheduleAutoRotate(bearer);
  }

  private scheduleAutoRotate(bearer: string): void {
    this.clearRotateTimer();
    if (this.closed) return; // never arm a timer on a closed connection
    this.currentBearer = bearer;
    // RECEIPT TIME, stamped here and nowhere else. Every later arithmetic is
    // measured from it, so the browser's clock only ever measures ELAPSED
    // time against itself and never gets compared with the server's.
    this.bearerReceivedAtMs = Date.now();
    const lifetime = decodeJwtLifetime(bearer);
    if (lifetime == null) return; // no exp -> nothing to schedule against (skip)
    const delay = computeRotateDelayMs(
      lifetime,
      this.bearerReceivedAtMs,
      this.bearerReceivedAtMs,
      DEFAULT_ROTATE_FRACTION,
      this.rotateFloorMs(),
    );
    this.rotateTimer = setTimeout(() => {
      void this.performAutoRotate();
    }, delay);
  }

  // rotateFloorMs resolves the configured floor, refusing a non-positive or
  // non-finite override rather than letting it disarm the guard.
  private rotateFloorMs(): number {
    const configured = this.auth?.rotateFloorMs;
    if (typeof configured === "number" && Number.isFinite(configured) && configured > 0) {
      return configured;
    }
    return DEFAULT_ROTATE_FLOOR_MS;
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
      const lifetime = this.currentBearer ? decodeJwtLifetime(this.currentBearer) : null;
      const remainingMs =
        lifetime != null
          ? remainingLifetimeMs(lifetime, this.bearerReceivedAtMs, Date.now())
          : 0;
      if (remainingMs > 0) {
        // FLOORED at the same bound as the scheduled rotation (memql#4326).
        // This used to be a 1s floor, so a refresh outage against a
        // short-lived token became three requests a second -- the retry path
        // amplifying the very storm the scheduled path was fixed to stop.
        const retryDelay = Math.max(
          this.rotateFloorMs(),
          Math.floor(remainingMs / (retriesLeft + 1)),
        );
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
  //
  // A DELIBERATE close never reconnects, whatever the reconnect config says.
  // That distinction is the reason the SDK can own reconnect at all: "the
  // socket died" and "the app is done with this connection" arrive at the
  // same listener otherwise.
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.clearRotateTimer();
    this.clearReconnectTimer();
    this.clearStableTimer();
    this.subscriptions.stop();
    this.dispatcher.stop();
    try {
      this.socket.close(1000, "client closing");
    } catch {
      // socket may already be closed
    }
    this.finish("disconnected", "");
  }

  // done resolves on FINAL termination.
  //
  // Without auto-reconnect that is any close, exactly as before. With it,
  // done() waits for close() or an exhausted attempt budget -- a transport
  // drop the SDK is going to recover from is not the end of the connection,
  // and telling consumers it was is how a self-healing stream still produced
  // a "disconnected" screen.
  done(): Promise<void> {
    if (this.reconnectCfg === null) return this.dispatcher.done();
    if (this.finished) return Promise.resolve();
    return new Promise<void>((resolve) => this.finalResolvers.push(resolve));
  }

  // ---- the reconnect loop -------------------------------------------------

  private onTransportLost(): void {
    if (this.closed || this.finished) return;
    this.clearStableTimer();
    this.setStatus("reconnecting", this.lastError || "stream closed");
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    const cfg = this.reconnectCfg;
    if (cfg === null || this.closed || this.finished) return;
    if (this.attemptCount >= cfg.maxAttempts) {
      this.finish("disconnected", this.lastError || "reconnect attempts exhausted");
      return;
    }
    const delay = backoffDelayMs(this.attemptCount, cfg.initialDelayMs, cfg.maxDelayMs);
    this.clearReconnectTimer();
    this.reconnectTimer = setTimeout(() => {
      void this.attemptReconnect();
    }, delay);
  }

  private async attemptReconnect(): Promise<void> {
    const cfg = this.reconnectCfg;
    if (cfg === null || this.closed || this.finished) return;
    this.attemptCount++;
    this.setStatus("reconnecting", this.lastError);

    let socket: WebSocket | null = null;
    try {
      // Re-resolve the bearer through the EXISTING auth seam. A stream that
      // has been down for a while may be holding a bearer that expired while
      // it was down, and dialing with it just fails again -- the in-place
      // rotation contract (memql#4326) covers healthy streams only.
      const auth = await this.authForDial();
      socket = await openSocket(this.opts, auth);
      if (this.closed || this.finished) {
        socket.close(1000, "connection closed during reconnect");
        return;
      }
      this.socket = socket;
      this.dispatcher.rebind(socket);
      await this.handshake(this.opts);
      if (this.closed || this.finished) return;
    } catch (err) {
      this.lastError = err instanceof Error ? err.message : String(err);
      try {
        socket?.close();
      } catch {
        // already gone
      }
      this.scheduleReconnect();
      return;
    }

    // Live again. REPLAY FIRST, then tell consumers -- a store that re-seeds
    // on the cycle notification must already be subscribed when its read goes
    // out, or the re-seed reintroduces the read-then-subscribe hole the
    // ordering contract closes (memql#4536).
    this.subscriptions.replay();
    this.lastError = "";
    this.armStableTimer();
    this.startAutoRotate();
    this.setStatus("connected", "");
    this.cycleCount++;
    for (const handler of [...this.cycleListeners]) {
      try {
        handler(this.cycleCount);
      } catch (err) {
        this.logger?.warn?.("memql sdk: connection-cycle listener threw", err);
      }
    }
  }

  // authForDial resolves the credential for a FRESH dial. The bearer is
  // re-read through onTokenExpired when there is one; otherwise the
  // connection re-presents whatever it currently holds.
  private async authForDial(): Promise<ConnectionAuth | undefined> {
    if (!this.auth) return undefined;
    const hook = this.auth.onTokenExpired;
    if (!hook || !this.currentBearer) return { ...this.auth, bearer: this.currentBearer };
    try {
      const next = (await hook())?.trim();
      if (next) {
        this.currentBearer = next;
        this.bearerReceivedAtMs = Date.now();
        return { ...this.auth, bearer: next };
      }
    } catch (err) {
      this.logger?.warn?.("memql sdk: onTokenExpired threw before a redial", err);
    }
    return { ...this.auth, bearer: this.currentBearer };
  }

  // armStableTimer resets the backoff once a stream has SURVIVED long enough
  // to count as healthy. See ReconnectOptions.stableAfterMs for why survival
  // rather than dial success is the trigger.
  private armStableTimer(): void {
    const cfg = this.reconnectCfg;
    if (cfg === null) return;
    this.clearStableTimer();
    this.stableTimer = setTimeout(() => {
      this.attemptCount = 0;
    }, cfg.stableAfterMs);
  }

  private clearStableTimer(): void {
    if (this.stableTimer != null) {
      clearTimeout(this.stableTimer);
      this.stableTimer = null;
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer != null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private statusEvent(): ConnectionStatusEvent {
    return { status: this.statusValue, attempt: this.attemptCount, error: this.lastError };
  }

  private setStatus(status: ConnectionStatus, error: string): void {
    this.statusValue = status;
    this.lastError = error;
    const ev = this.statusEvent();
    for (const handler of [...this.statusListeners]) {
      try {
        handler(ev);
      } catch (err) {
        this.logger?.warn?.("memql sdk: status listener threw", err);
      }
    }
  }

  private finish(status: ConnectionStatus, error: string): void {
    if (this.finished) return;
    this.finished = true;
    this.clearReconnectTimer();
    this.clearStableTimer();
    // An EXHAUSTED budget is as terminal as a close(), so tear the transport
    // down here too. Leaving a supervised dispatcher alive with nothing left
    // to supervise would park every later request on a socket nothing is
    // going to revive, which reads to a consumer as a hang rather than a
    // failure.
    if (!this.closed) {
      this.clearRotateTimer();
      this.subscriptions.stop();
      this.dispatcher.stop();
    }
    this.setStatus(status, error);
    for (const resolve of this.finalResolvers) resolve();
    this.finalResolvers.length = 0;
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
        this.engineVersion = payload.value.engineVersion ?? "";
        this.engineCommit = payload.value.engineCommit ?? "";
      }
    } finally {
      clearTimeout(timer);
    }
  }
}

// DEFAULT_ROTATE_FRACTION is how much of a token's lifetime elapses before the
// SDK rotates it. 70% leaves headroom for the bounded retries below.
export const DEFAULT_ROTATE_FRACTION = 0.7;

// DEFAULT_ROTATE_FLOOR_MS is the shortest interval the SDK will ever schedule a
// rotation at.
//
// memql#4326. Without a floor, ANY arithmetic that can produce a small number
// produces a request loop: the old formula compared the identity service's
// `exp` against the browser's `Date.now()`, so a browser running ahead by a
// little under the TTL saw every fresh token as nearly expired and rotated
// every few seconds, forever -- one refresh-token rotation, and one audit row,
// per rotation. `exp - iat` removes the skew; the floor is what makes a
// *pathological token* (a 20-second TTL, a malformed claim) harmless too.
export const DEFAULT_ROTATE_FLOOR_MS = 30_000;

// JwtLifetime is the pair of claims the rotation schedule is derived from.
// `iat` is null when the token does not carry a usable one.
export interface JwtLifetime {
  iat: number | null; // unix seconds
  exp: number; // unix seconds
}

// computeRotateDelayMs returns how long to wait before rotating a bearer.
//
// THE CLOCKS. `exp` and `iat` are both stamped by the identity service, so
// their DIFFERENCE is the token's lifetime on the server's clock and carries no
// skew. `receivedAtMs` and `nowMs` are both this machine's clock, so their
// difference is elapsed time on the browser's clock and carries no skew either.
// The formula only ever subtracts same-clock pairs, which is the whole point:
//
//     delay = max(floor, fraction * (exp - iat) - (now - receivedAt))
//
// A token with no usable `iat` has no server-measured lifetime to work from, so
// it falls back to the wall-clock reading `exp - now` -- today's arithmetic,
// skew and all, but still floored, which is what bounds the damage.
//
// The result is never below `floorMs`, including for an already-expired token:
// rotating instantly would not help (the refresh either works in 30s or it does
// not) and spinning is the failure this floor exists to prevent.
export function computeRotateDelayMs(
  lifetime: JwtLifetime,
  receivedAtMs: number,
  nowMs: number,
  fraction = DEFAULT_ROTATE_FRACTION,
  floorMs = DEFAULT_ROTATE_FLOOR_MS,
): number {
  if (lifetime.iat == null) {
    // No server-measured lifetime: fall back to the wall clock. `nowMs` is the
    // reference here, so there is no elapsed term to subtract.
    const ttlMs = lifetime.exp * 1000 - nowMs;
    return Math.max(floorMs, Math.floor(ttlMs * fraction));
  }
  const lifetimeMs = (lifetime.exp - lifetime.iat) * 1000;
  const targetMs = Math.max(floorMs, lifetimeMs * fraction);
  const elapsedMs = Math.max(0, nowMs - receivedAtMs);
  return Math.max(floorMs, Math.floor(targetMs - elapsedMs));
}

// remainingLifetimeMs answers "how much of this token is left", measured the
// same skew-free way computeRotateDelayMs schedules against. Used by the retry
// path to decide whether another attempt can still land before expiry.
export function remainingLifetimeMs(
  lifetime: JwtLifetime,
  receivedAtMs: number,
  nowMs: number,
): number {
  if (lifetime.iat == null) {
    return lifetime.exp * 1000 - nowMs;
  }
  const lifetimeMs = (lifetime.exp - lifetime.iat) * 1000;
  return lifetimeMs - Math.max(0, nowMs - receivedAtMs);
}

// decodeJwtLifetime reads the `iat` and `exp` claims from a JWT WITHOUT
// verifying the signature -- it only needs the lifetime to schedule rotation.
//
// Returns null for a non-JWT, an unparseable payload, or a missing/non-numeric
// `exp` (callers treat null as "don't auto-rotate this token"). A missing,
// non-numeric, or nonsensical `iat` (at or after `exp`) is reported as null
// ALONGSIDE a usable exp, which selects the wall-clock fallback rather than
// disabling rotation: a token still has to be rotated even when it declines to
// say when it was issued.
export function decodeJwtLifetime(jwt: string): JwtLifetime | null {
  const payload = jwt.split(".")[1];
  if (!payload) return null;
  try {
    const claims = JSON.parse(base64UrlDecode(payload)) as { exp?: unknown; iat?: unknown };
    if (typeof claims.exp !== "number" || !Number.isFinite(claims.exp)) return null;
    const iat =
      typeof claims.iat === "number" && Number.isFinite(claims.iat) && claims.iat < claims.exp
        ? claims.iat
        : null;
    return { iat, exp: claims.exp };
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

// resolveReconnect normalises the opt-in config, or returns null when
// auto-reconnect is off (the default).
function resolveReconnect(opts: ReconnectOptions | undefined): Required<ReconnectOptions> | null {
  if (!opts?.enabled) return null;
  const positive = (v: number | undefined, fallback: number): number =>
    typeof v === "number" && Number.isFinite(v) && v > 0 ? v : fallback;
  const initialDelayMs = positive(opts.initialDelayMs, DEFAULT_RECONNECT_INITIAL_MS);
  return {
    enabled: true,
    initialDelayMs,
    // Never below the initial delay: a ceiling under the floor would make the
    // backoff run BACKWARDS, retrying faster the longer an outage lasts.
    maxDelayMs: Math.max(initialDelayMs, positive(opts.maxDelayMs, DEFAULT_RECONNECT_MAX_MS)),
    maxAttempts: positive(opts.maxAttempts, Number.POSITIVE_INFINITY),
    stableAfterMs: positive(opts.stableAfterMs, DEFAULT_RECONNECT_STABLE_MS),
  };
}

// backoffDelayMs is exponential with FULL JITTER: a uniform draw from
// [0, capped], not `capped * (0.5 + random/2)`.
//
// Full jitter is the shape that actually decorrelates a fleet. A node
// restarting drops every browser at once, and a tight jitter band leaves them
// still moving as a herd -- the thundering retry lands on a node that has just
// come up with nothing warm. Exported for the test that pins the bounds.
export function backoffDelayMs(
  attempt: number,
  initialMs: number,
  maxMs: number,
  random: () => number = Math.random,
): number {
  const exponential = initialMs * 2 ** Math.max(0, attempt);
  const capped = Math.min(maxMs, Number.isFinite(exponential) ? exponential : maxMs);
  return Math.floor(random() * capped);
}

// openSocket dials and waits for the transport to open. Shared by the first
// dial and every reconnect, so a redial cannot drift from the original in how
// it carries the credential.
async function openSocket(
  opts: ConnectOptions,
  auth: ConnectionAuth | undefined,
): Promise<WebSocket> {
  const url = resolveEndpoint(opts.endpoint, auth);
  const factory = opts.webSocketFactory ?? defaultWebSocketFactory;
  const socket = factory(url, wsAuthSubprotocols(auth));
  await waitForOpen(socket);
  return socket;
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
// opt-in (#2524) restores the deprecated `?bearer_token=` carry for older
// front doors that don't negotiate the subprotocol scheme.
// Worker tokens remain on the query string until the worker surface migrates.
function resolveEndpoint(endpoint: string, auth: ConnectionAuth | undefined): string {
  const url = resolveWsUrl(endpoint);
  if (auth?.legacyUrlToken) {
    if (auth.bearer) url.searchParams.set("bearer_token", auth.bearer);
  }
  if (auth?.workerToken) url.searchParams.set("worker_token", auth.workerToken);
  return url.toString();
}
