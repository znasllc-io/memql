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

// The SDK is a pure ESM package (`"type": "module"`, no CJS export
// condition); this extension is CommonJS (the VS Code extension host loads
// `main` via `require`). A type-only import can be resolved statically with
// the `resolution-mode` attribute, but the runtime value cannot -- `require()`
// of an ESM-only module throws ERR_REQUIRE_ESM -- so `Connection.dial` is
// reached through a dynamic `import()` inside `connect()` instead.
import type {
  Connection,
  QueryClient,
  SubscriptionManager,
} from "@znasllc-io/memql-sdk-core/client" with { "resolution-mode": "import" };

import type { ClusterConfig } from "../clusters/model.js";
import { isOidcOnly, needsAuth } from "../clusters/model.js";
import { webSocketUrlFor } from "./endpoint.js";

export type ConnectionState =
  | { status: "disconnected" }
  | { status: "connecting"; clusterName: string }
  | { status: "connected"; clusterName: string; nodeId: string }
  | { status: "error"; clusterName: string; message: string };

export type StateListener = (state: ConnectionState) => void;

export class ConnectionManager {
  private conn: Connection | undefined;
  private current: ConnectionState = { status: "disconnected" };
  private readonly listeners = new Set<StateListener>();
  // Guards against an out-of-order settle: if the user selects cluster A then
  // cluster B before A's handshake finishes, A's completion must not overwrite
  // B's state.
  private generation = 0;

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

    if (needsAuth(cluster)) {
      const message = isOidcOnly(cluster)
        ? "This cluster is configured for OIDC. Authenticate it in the memQL Cockpit first, or add a PAT to clusters.yaml."
        : "This cluster is not configured. Set an endpoint and a PAT.";
      this.publish({ status: "error", clusterName: cluster.name, message });
      return;
    }

    this.publish({ status: "connecting", clusterName: cluster.name });
    try {
      const { Connection: ConnectionClass } = await import("@znasllc-io/memql-sdk-core/client");
      const conn = await ConnectionClass.dial({
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
