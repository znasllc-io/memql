// The portal's single connection to the cluster.
//
// ONE Connection per document, owned here and shared through context. The SDK
// Connection multiplexes every request over one WebSocket (queries,
// subscriptions, AI, everything), so a second one buys nothing and costs a
// second handshake, a second auth rotation timer, and two answers to "are we
// connected?".
//
// Connection state is a first-class thing the UI renders (the header's
// indicator), not an implementation detail -- an operations console whose
// data silently stops updating is worse than one that says it is
// disconnected.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Connection, type QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { anonymousAuthSource, toConnectionAuth, type PortalAuthSource } from "./auth";
import { portalBridgePath } from "./endpoint";

export type ConnectionStatus = "connecting" | "connected" | "error" | "closed";

export interface ClusterState {
  status: ConnectionStatus;
  // Present only while status === "connected". Everything that reads the
  // cluster goes through this, so a component cannot accidentally query a
  // connection that is still handshaking.
  query: QueryClient | null;
  // Server identity from the ServerHello, for the header. Empty until
  // connected.
  nodeId: string;
  serverVersion: string;
  // Human-readable failure, empty unless status === "error".
  error: string;
  // Redial. Safe to call in any state; tears down whatever is live first.
  reconnect: () => void;
}

const ClusterContext = createContext<ClusterState | null>(null);

export interface ClusterProviderProps {
  children: ReactNode;
  // The credential source. Defaults to the no-credential source; #3315
  // replaces it with the identity-backed one. See src/cluster/auth.ts.
  auth?: PortalAuthSource;
  // Overridable for tests, which have no server to dial.
  dial?: typeof Connection.dial;
}

export function ClusterProvider({
  children,
  auth = anonymousAuthSource,
  dial = Connection.dial,
}: ClusterProviderProps): ReactNode {
  const [status, setStatus] = useState<ConnectionStatus>("connecting");
  const [query, setQuery] = useState<QueryClient | null>(null);
  const [nodeId, setNodeId] = useState("");
  const [serverVersion, setServerVersion] = useState("");
  const [error, setError] = useState("");
  // Bumping this re-runs the dial effect. A counter rather than a boolean so
  // repeated reconnects while already in "error" each start a fresh attempt.
  const [attempt, setAttempt] = useState(0);

  // Guards against a settle from a SUPERSEDED dial writing state. React 19's
  // StrictMode mounts effects twice in development, and a reconnect can fire
  // while a previous dial is still in flight -- in both cases the older
  // attempt must be dropped silently rather than overwriting the newer one's
  // result.
  const generation = useRef(0);

  useEffect(() => {
    const mine = ++generation.current;
    let connection: Connection | null = null;
    let cancelled = false;

    setStatus("connecting");
    setError("");

    void (async () => {
      try {
        const connectionAuth = await toConnectionAuth(auth);
        const conn = await dial({
          endpoint: portalBridgePath,
          ...(connectionAuth ? { auth: connectionAuth } : {}),
          clientId: "memql-portal",
          sdkName: "memql-portal",
        });
        if (cancelled || generation.current !== mine) {
          conn.close();
          return;
        }
        connection = conn;
        setQuery(conn.query);
        setNodeId(conn.nodeId);
        setServerVersion(conn.serverVersion);
        setStatus("connected");

        // The stream can end without anyone calling close() -- the node rolls,
        // the bearer is rejected, the front door drops the upgrade. Surface
        // that instead of leaving the indicator green over a dead socket.
        void conn.done().then(() => {
          if (generation.current !== mine) return;
          setQuery(null);
          setStatus("closed");
        });
      } catch (err) {
        if (cancelled || generation.current !== mine) return;
        setQuery(null);
        setStatus("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();

    return () => {
      cancelled = true;
      connection?.close();
    };
  }, [auth, dial, attempt]);

  const reconnect = useCallback(() => setAttempt((n) => n + 1), []);

  const value = useMemo<ClusterState>(
    () => ({ status, query, nodeId, serverVersion, error, reconnect }),
    [status, query, nodeId, serverVersion, error, reconnect],
  );

  return <ClusterContext.Provider value={value}>{children}</ClusterContext.Provider>;
}

// useCluster is the only sanctioned way to reach the connection. Throwing on
// a missing provider is deliberate: returning a null-shaped default would let
// a mis-mounted component render an empty page forever instead of failing at
// the point of the mistake.
export function useCluster(): ClusterState {
  const state = useContext(ClusterContext);
  if (state === null) {
    throw new Error("useCluster must be used inside <ClusterProvider>");
  }
  return state;
}
