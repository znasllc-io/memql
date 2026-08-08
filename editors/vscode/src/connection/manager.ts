// The extension's single live cluster connection.
//
// Exactly one connection exists at a time: the working cluster's. Selecting a
// different cluster tears the old one down first. This mirrors the cockpit's
// single-connection invariant -- concurrent connections to different clusters
// make "which cluster did that row come from" unanswerable.
//
// Deliberately free of `vscode` imports so it is unit-testable. State changes
// are published through a plain listener set; the views adapt it to VS Code's
// event model.
//
// @znasllc-io/memql-sdk-core is pure ESM; this file compiles under
// moduleResolution "bundler" (tsconfig.json) and is bundled by esbuild both
// for the packaged extension (esbuild.js) and for `node --test`
// (esbuild.test.js), which is what makes the plain static import below work
// on the CommonJS extension host -- esbuild inlines the SDK and emits CJS,
// it is not left as a `require()` of an ESM-only package.
import { Connection } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectOptions, QueryClient, SubscriptionManager } from "@znasllc-io/memql-sdk-core/client";
import { WebSocket as NodeWebSocket } from "ws";

import type { ClusterConfig } from "../clusters/model.js";
import { isOidcOnly, needsAuth } from "../clusters/model.js";
import { webSocketUrlFor } from "./endpoint.js";

export type ConnectionState =
  | { status: "disconnected" }
  | { status: "connecting"; clusterName: string }
  | { status: "connected"; clusterName: string; nodeId: string }
  | { status: "error"; clusterName: string; message: string };

export type StateListener = (state: ConnectionState) => void;

// The shape of `Connection.dial`. Constructor-injectable (defaults to
// `defaultDial` below) purely for test determinism: ConnectionManager's own
// tests supply a fake that resolves/rejects on command, rather than driving
// a real WebSocket handshake to test the generation-counter race.
export type DialFn = (opts: ConnectOptions) => Promise<Connection>;

// The VS Code extension host (Electron's bundled Node -- 20.9 on the
// declared engines.vscode ^1.91.0 floor) has no global WebSocket below Node
// 22; the SDK's default factory throws in that case ("memql sdk: no global
// WebSocket available -- pass webSocketFactory or run in a browser / Node
// 22+", sdk/ts/src/client/connection.ts). The `ws` package supplies a real
// implementation so `connect()` actually succeeds on the declared floor.
const defaultDial: DialFn = (opts) =>
  Connection.dial({
    ...opts,
    webSocketFactory: (url, protocols) =>
      new NodeWebSocket(url, protocols) as unknown as WebSocket,
  });

export class ConnectionManager {
  private conn: Connection | undefined;
  private current: ConnectionState = { status: "disconnected" };
  private readonly listeners = new Set<StateListener>();
  // Guards against an out-of-order settle: if the user selects cluster A then
  // cluster B before A's handshake finishes, A's completion must not overwrite
  // B's state.
  private generation = 0;

  constructor(private readonly dial: DialFn = defaultDial) {}

  get state(): ConnectionState {
    return this.current;
  }

  get query(): QueryClient | undefined {
    return this.conn?.query;
  }

  get subscriptions(): SubscriptionManager | undefined {
    return this.conn?.subscriptions;
  }

  onDidChangeState(listener: StateListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  private publish(state: ConnectionState): void {
    this.current = state;
    for (const l of this.listeners) l(state);
  }

  async connect(cluster: ClusterConfig): Promise<void> {
    const gen = ++this.generation;
    await this.closeCurrent();
    // closeCurrent is async (a microtask boundary): a second connect() call
    // issued without awaiting the first can already have bumped `generation`
    // by the time this resumes. Without this check a superseded call would
    // still publish its own "connecting" / "error" state over the current
    // cluster's.
    if (gen !== this.generation) return;

    // isOidcOnly is checked FIRST, before needsAuth: a PAT-less OIDC cluster
    // with a real endpoint makes needsAuth() false (issuer+clientId count as
    // "configured" there), so gating on needsAuth alone would skip straight
    // to dialing with an empty bearer and surface a raw handshake/auth
    // failure instead of this actionable message.
    if (isOidcOnly(cluster) || needsAuth(cluster)) {
      const message = isOidcOnly(cluster)
        ? "This cluster is configured for OIDC. Authenticate it in the memQL Cockpit first, or add a PAT to clusters.yaml."
        : "This cluster is not configured. Set an endpoint and a PAT.";
      this.publish({ status: "error", clusterName: cluster.name, message });
      return;
    }

    this.publish({ status: "connecting", clusterName: cluster.name });
    try {
      const conn = await this.dial({
        endpoint: webSocketUrlFor(cluster),
        auth: { bearer: cluster.pat ?? "" },
        clientId: "memql-vscode",
        sdkName: "memql-vscode",
      });
      if (gen !== this.generation) {
        // Superseded while dialing; drop this connection on the floor.
        conn.close();
        return;
      }
      this.conn = conn;
      this.publish({ status: "connected", clusterName: cluster.name, nodeId: conn.nodeId });
      // Not awaited: this resolves only when the socket eventually dies.
      void this.watchForTermination(conn, gen, cluster.name);
    } catch (err) {
      if (gen !== this.generation) return;
      this.publish({
        status: "error",
        clusterName: cluster.name,
        message: err instanceof Error ? err.message : String(err),
      });
    }
  }

  async disconnect(): Promise<void> {
    this.generation++;
    await this.closeCurrent();
    this.publish({ status: "disconnected" });
  }

  // Nothing else notices a connection dying. Without this, a server-side drop
  // leaves every view reporting "Connected", `get query()` handing out a
  // client over a dead socket, and the CDC subscription the concept browser
  // advertises silently gone -- with no notice anywhere.
  //
  // `Connection.done()` (-> Dispatcher.done()) resolves on ANY termination and
  // never rejects. That includes OUR OWN close(): closeCurrent() calls
  // conn.close(), which stops the dispatcher, which resolves done(). So a
  // deliberate teardown fires this too, and the generation check is what
  // separates the two cases rather than a nicety:
  //
  //   connect(B)   bumps generation, THEN closes A -> A's done() sees a stale
  //                gen and cannot publish over B's state.
  //   disconnect() bumps generation, THEN closes    -> its done() is likewise
  //                stale and cannot overwrite the "disconnected" it publishes.
  //
  // Only a drop of the CURRENT connection reaches the publish below.
  private async watchForTermination(
    conn: Connection,
    gen: number,
    clusterName: string,
  ): Promise<void> {
    await conn.done();
    if (gen !== this.generation) return;
    // Belt and braces: the generation check above already covers every path
    // that replaces the connection, but a stale done() must never clear a
    // connection it does not own.
    if (this.conn !== conn) return;
    // Drop the reference FIRST so `query` / `subscriptions` stop handing out
    // clients over a dead socket even before listeners run.
    this.conn = undefined;
    this.publish({
      status: "error",
      clusterName,
      message: `Connection to ${clusterName} was lost. Select the cluster again to reconnect.`,
    });
  }

  private async closeCurrent(): Promise<void> {
    if (this.conn === undefined) return;
    try {
      this.conn.close();
    } catch {
      // A close on an already-dead socket is not worth surfacing.
    }
    this.conn = undefined;
  }
}
