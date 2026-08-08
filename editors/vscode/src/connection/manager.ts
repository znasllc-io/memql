// The extension's single live cluster connection.
//
// Exactly one connection exists at a time: the working cluster's. Selecting a
// different cluster tears the old one down first. This mirrors the cockpit's
// single-connection invariant -- concurrent connections to different clusters
// make "which cluster did that row come from" unanswerable.
//
// Deliberately free of `vscode` imports so it is unit-testable -- src/views/
// and src/webview/ are the only adapter layers allowed to import it, and the
// rule is enforced mechanically (cmd/memql-lsp/vscodeimportrule_test.go).
// State changes are published through a plain listener set; the views adapt
// it to VS Code's event model.
//
// @znasllc-io/memql-sdk-core is pure ESM; this file compiles under
// moduleResolution "bundler" (tsconfig.json) and is bundled by esbuild both
// for the packaged extension (esbuild.js) and for `node --test`
// (esbuild.test.js), which is what makes the plain static import below work
// on the CommonJS extension host -- esbuild inlines the SDK and emits CJS,
// it is not left as a `require()` of an ESM-only package.
import { AuthoringClient } from "@znasllc-io/memql-sdk-core/authoring";
import { Connection } from "@znasllc-io/memql-sdk-core/client";
import type { ConnectOptions, Dispatcher, QueryClient, SubscriptionManager } from "@znasllc-io/memql-sdk-core/client";
import { WebSocket as NodeWebSocket } from "ws";

import { Latest, type LatestToken } from "../async/latest.js";
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
// a real WebSocket handshake to test the supersession race.
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
  // Rebuilt with `conn`, never carried across one. See `get authoring()`.
  private authoringClient: AuthoringClient | undefined;
  private current: ConnectionState = { status: "disconnected" };
  private readonly listeners = new Set<StateListener>();
  // Guards against an out-of-order settle: if the user selects cluster A then
  // cluster B before A's handshake finishes, A's completion must not overwrite
  // B's state. The shared guard (src/async/latest.ts) -- the same one
  // ConceptsCache and ConceptPanelState use, so "which async paths are
  // guarded" is a grep rather than a per-module question.
  private readonly latest = new Latest<"connection">();

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

  // The raw dispatcher, for the surfaces the SDK exposes as free functions
  // over one rather than as methods on QueryClient -- currently callTool
  // (memql#3309's tool runs). Undefined whenever `query` is, and for the same
  // reason: watchForTermination drops `conn` the moment the socket dies, so a
  // caller cannot be handed a dispatcher over a dead stream.
  get dispatcher(): Dispatcher | undefined {
    return this.conn?.dispatcher;
  }

  // The authoring surface: Gate-1 bundle validation and session-define, which
  // is what lets a run execute an editor buffer that has never been saved.
  //
  // Rebuilt whenever the connection changes rather than cached across one,
  // because a session-define is scoped to the STREAM: an AuthoringClient
  // holding a dead dispatcher would validate happily against nothing. The
  // object is stateless, so building it per connection costs nothing.
  get authoring(): AuthoringClient | undefined {
    if (this.conn === undefined) return undefined;
    if (this.authoringClient === undefined) {
      this.authoringClient = new AuthoringClient(this.conn.dispatcher);
    }
    return this.authoringClient;
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
    // begin(), not current(): starting a connect IS what makes every earlier
    // one stale, so this call supersedes them as it takes its token.
    const token = this.latest.begin();
    await this.closeCurrent();
    // closeCurrent is async (a microtask boundary): a second connect() call
    // issued without awaiting the first can already have superseded this one
    // by the time it resumes. Without this check a superseded call would
    // still publish its own "connecting" / "error" state over the current
    // cluster's.
    if (!this.latest.isCurrent(token)) return;

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
      if (!this.latest.isCurrent(token)) {
        // Superseded while dialing; drop this connection on the floor.
        conn.close();
        return;
      }
      this.conn = conn;
      this.authoringClient = undefined;
      this.publish({ status: "connected", clusterName: cluster.name, nodeId: conn.nodeId });
      // Not awaited: this resolves only when the socket eventually dies.
      void this.watchForTermination(conn, token, cluster.name);
    } catch (err) {
      if (!this.latest.isCurrent(token)) return;
      this.publish({
        status: "error",
        clusterName: cluster.name,
        message: err instanceof Error ? err.message : String(err),
      });
    }
  }

  async disconnect(): Promise<void> {
    // invalidate(), not begin(): a disconnect supersedes every outstanding
    // token but starts no operation of its own, so there is no token to mint.
    this.latest.invalidate();
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
  // deliberate teardown fires this too, and the staleness check is what
  // separates the two cases rather than a nicety. Both teardown paths
  // supersede BEFORE closing, which is what makes them stale by the time
  // their own done() fires:
  //
  //   connect(B)   begin()s, THEN closes A      -> A's done() holds a
  //                superseded token and cannot publish over B's state.
  //   disconnect() invalidate()s, THEN closes   -> its done() is likewise
  //                superseded and cannot overwrite the "disconnected" it
  //                publishes.
  //
  // Only a drop of the CURRENT connection reaches the publish below.
  private async watchForTermination(
    conn: Connection,
    token: LatestToken<"connection">,
    clusterName: string,
  ): Promise<void> {
    await conn.done();
    if (!this.latest.isCurrent(token)) return;
    // Belt and braces: the staleness check above already covers every path
    // that replaces the connection, but a stale done() must never clear a
    // connection it does not own.
    if (this.conn !== conn) return;
    // Drop the reference FIRST so `query` / `subscriptions` stop handing out
    // clients over a dead socket even before listeners run.
    this.conn = undefined;
    this.authoringClient = undefined;
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
    this.authoringClient = undefined;
  }
}
