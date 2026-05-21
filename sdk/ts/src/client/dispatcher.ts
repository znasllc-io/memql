// Dispatcher multiplexes envelopes over a single WebSocket. It
// correlates server replies to outbound requests by messageId /
// correlateTo, routes streaming-session replies to per-request_id
// listeners, and surfaces everything else on a fanout event channel.
//
// Mirrors sdk/go/client/dispatcher.go. The Go version uses two
// distinct termination signals (Done vs Unexpected); we expose the
// same distinction so consumer reconnect logic can fire only on
// stream errors, not on caller-initiated close.

import { newShortId } from "./id.js";
import {
  streamRequestId,
  type ClientMessage,
  type ServerMessage,
} from "./wire.js";

export type ServerListener = (msg: ServerMessage) => void;
export type StreamListener = (msg: ServerMessage) => void;
export type Unregister = () => void;

export interface DispatcherOptions {
  socket: WebSocket;
  logger?: Pick<Console, "warn" | "info" | "error"> | null;
}

export interface PendingError extends Error {
  reason: "closed" | "transport";
}

function pendingError(message: string, reason: PendingError["reason"]): PendingError {
  const err = new Error(message) as PendingError;
  err.reason = reason;
  return err;
}

interface PendingEntry {
  resolve: (msg: ServerMessage) => void;
  reject: (err: PendingError) => void;
}

export class Dispatcher {
  private readonly socket: WebSocket;
  private readonly logger: DispatcherOptions["logger"];
  private readonly pending = new Map<string, PendingEntry>();
  private readonly streams = new Map<string, StreamListener>();
  private readonly eventListeners = new Set<ServerListener>();

  private stopped = false;
  private unexpected = false;
  private readonly doneResolvers: Array<() => void> = [];
  private readonly unexpectedResolvers: Array<() => void> = [];

  constructor(opts: DispatcherOptions) {
    this.socket = opts.socket;
    this.logger = opts.logger ?? null;

    this.socket.addEventListener("message", this.handleMessage);
    this.socket.addEventListener("close", this.handleClose);
    this.socket.addEventListener("error", this.handleError);
  }

  // send writes an envelope. Assigns messageId when missing. Returns
  // the messageId so callers can pair with later replies.
  send(msg: ClientMessage): string {
    const out: ClientMessage = { ...msg };
    if (!out.messageId) out.messageId = newShortId();
    this.rawSend(out);
    return out.messageId;
  }

  // sendAndWait blocks until a server envelope with correlateTo ==
  // messageId arrives (or the stream tears down / the signal fires).
  // The Go SDK calls this for every request-response method.
  async sendAndWait(msg: ClientMessage, signal?: AbortSignal): Promise<ServerMessage> {
    const out: ClientMessage = { ...msg };
    if (!out.messageId) out.messageId = newShortId();
    const id = out.messageId;
    return new Promise<ServerMessage>((resolve, reject) => {
      if (signal?.aborted) {
        reject(pendingError("aborted", "closed"));
        return;
      }
      const entry: PendingEntry = { resolve, reject };
      this.pending.set(id, entry);
      const abortListener = () => {
        const cur = this.pending.get(id);
        if (cur === entry) this.pending.delete(id);
        reject(pendingError("aborted", "closed"));
      };
      if (signal) signal.addEventListener("abort", abortListener, { once: true });
      try {
        this.rawSend(out);
      } catch (err) {
        this.pending.delete(id);
        reject(pendingError(`send failed: ${(err as Error).message}`, "transport"));
        return;
      }
      const cleanup = () => {
        if (signal) signal.removeEventListener("abort", abortListener);
      };
      const wrap = (orig: (msg: ServerMessage) => void) => (msg: ServerMessage) => {
        cleanup();
        orig(msg);
      };
      const wrapErr = (orig: (err: PendingError) => void) => (err: PendingError) => {
        cleanup();
        orig(err);
      };
      entry.resolve = wrap(entry.resolve);
      entry.reject = wrapErr(entry.reject);
    });
  }

  // addEventListener attaches a fanout handler for uncorrelated
  // server messages (graph events, heartbeats). Multiple consumers
  // are supported -- unlike the Go SDK's single-consumer event chan,
  // a browser SDK typically has several panes wanting events.
  addEventListener(handler: ServerListener): Unregister {
    this.eventListeners.add(handler);
    return () => this.eventListeners.delete(handler);
  }

  // registerStream wires a per-session listener for streaming
  // protocols whose replies share a request_id rather than the
  // per-message messageId used by sendAndWait. Caller must invoke
  // the returned unregister fn when the session ends.
  registerStream(requestId: string, handler: StreamListener): Unregister {
    this.streams.set(requestId, handler);
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  // done resolves on any termination (intentional or otherwise).
  done(): Promise<void> {
    if (this.stopped) return Promise.resolve();
    return new Promise<void>((resolve) => this.doneResolvers.push(resolve));
  }

  // unexpectedClose resolves ONLY when the socket terminated due to
  // an error event (network drop, server crash). Stop() does not
  // close this -- mirrors the Go dispatcher's Unexpected() channel.
  unexpectedClose(): Promise<void> {
    if (this.unexpected) return Promise.resolve();
    return new Promise<void>((resolve) => this.unexpectedResolvers.push(resolve));
  }

  // stop tears the dispatcher down. The socket itself is closed by
  // the owning Connection.
  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.socket.removeEventListener("message", this.handleMessage);
    this.socket.removeEventListener("close", this.handleClose);
    this.socket.removeEventListener("error", this.handleError);
    this.failAllPending(pendingError("dispatcher stopped", "closed"));
    for (const resolve of this.doneResolvers) resolve();
    this.doneResolvers.length = 0;
  }

  private rawSend(msg: ClientMessage): void {
    if (this.socket.readyState !== WebSocket.OPEN) {
      throw new Error(`socket not open (readyState=${this.socket.readyState})`);
    }
    this.socket.send(JSON.stringify(msg));
  }

  private readonly handleMessage = (ev: MessageEvent): void => {
    let data: string;
    if (typeof ev.data === "string") {
      data = ev.data;
    } else if (ev.data instanceof ArrayBuffer) {
      data = new TextDecoder().decode(new Uint8Array(ev.data));
    } else if (ev.data instanceof Blob) {
      // Browsers under default binaryType=blob. Fan to a microtask --
      // we don't await per-message because dispatch order matters.
      ev.data
        .text()
        .then((text) => this.routeText(text))
        .catch((err) => this.logger?.warn?.("memql sdk: failed to read blob frame", err));
      return;
    } else {
      this.logger?.warn?.("memql sdk: unsupported frame type", ev.data);
      return;
    }
    this.routeText(data);
  };

  private routeText(data: string): void {
    let msg: ServerMessage;
    try {
      msg = JSON.parse(data) as ServerMessage;
    } catch (err) {
      this.logger?.warn?.("memql sdk: malformed JSON frame", err);
      return;
    }
    this.route(msg);
  }

  private route(msg: ServerMessage): void {
    const correlateTo = msg.correlateTo ?? "";
    if (correlateTo) {
      const entry = this.pending.get(correlateTo);
      if (entry) {
        this.pending.delete(correlateTo);
        entry.resolve(msg);
        return;
      }
    }
    const reqId = streamRequestId(msg);
    if (reqId) {
      const stream = this.streams.get(reqId);
      if (stream) {
        stream(msg);
        return;
      }
    }
    for (const handler of this.eventListeners) {
      try {
        handler(msg);
      } catch (err) {
        this.logger?.warn?.("memql sdk: event listener threw", err);
      }
    }
  }

  private readonly handleClose = (): void => {
    if (this.stopped) return; // already torn down via stop()
    this.stopped = true;
    this.failAllPending(pendingError("stream closed", "transport"));
    // Reaching here without our own stop() call -> treat as
    // unexpected. The Connection owner sets stopped before
    // initiating a graceful close, so this branch only fires on
    // server / network terminations.
    this.markUnexpected();
    for (const resolve of this.doneResolvers) resolve();
    this.doneResolvers.length = 0;
  };

  private readonly handleError = (): void => {
    // The browser's WebSocket error event is opaque (no error
    // payload). All we know is that the socket is doomed; close
    // will follow. Mark unexpected so consumers waiting on
    // unexpectedClose() resolve before the close event clears
    // pending requests.
    if (!this.stopped) this.markUnexpected();
  };

  private markUnexpected(): void {
    if (this.unexpected) return;
    this.unexpected = true;
    for (const resolve of this.unexpectedResolvers) resolve();
    this.unexpectedResolvers.length = 0;
  }

  private failAllPending(err: PendingError): void {
    for (const entry of this.pending.values()) entry.reject(err);
    this.pending.clear();
  }
}
