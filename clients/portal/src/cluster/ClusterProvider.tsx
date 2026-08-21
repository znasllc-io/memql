// The portal's single connection to the cluster.
//
// ONE Connection per document, owned here and shared through context. The SDK
// Connection multiplexes every request over one WebSocket (queries,
// subscriptions, AI, everything), so a second one buys nothing and costs a
// second handshake, a second auth rotation timer, and two answers to "are we
// connected?".
//
// Connection state is a first-class thing the UI renders (the rail
// profile), not an implementation detail -- an operations console whose
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
import {
  Connection,
  ModulesClient,
  type QueryClient,
  type SubscriptionManager,
} from "@znasllc-io/memql-sdk-core/client";
import { aiSuggest, type AiSuggestOptions, type AiSuggestResult } from "@znasllc-io/memql-sdk-core/ai";
import { DeployControlClient } from "@znasllc-io/memql-sdk-core/deploy";
import {
  mintAccountToken,
  revokeAccountToken,
  type AccountTokenMintResult,
  type AccountTokenRevokeResult,
  type MintAccountTokenArgs,
} from "@znasllc-io/memql-sdk-core/identity";
import { IdentityAdminClient } from "@znasllc-io/memql-sdk-core/identityadmin";

import { anonymousAuthSource, toConnectionAuth, type PortalAuthSource } from "./auth";
import { portalBridgePath } from "./endpoint";

// The connection-scoped typed clients (memql#4234). Everything here is an
// SDK surface constructed over the ONE stream's transport; the transport
// handle itself (the Dispatcher) never leaves this module, so nothing in
// the app can compose a raw envelope -- the seam the old `dispatcher`
// field left open cannot regrow. Built together when the stream comes up
// and nulled together when it ends, for the same reason query and
// subscriptions are: a request must not outlive the stream it was issued
// on.
export interface ClusterClients {
  // The deploy console's nine RPCs (memql#3311).
  deployControl: DeployControlClient;
  // The admin console's writes, behind the cluster's own owner/admin gate.
  identityAdmin: IdentityAdminClient;
  // The modules inventory + pack flip (memql#4188/#4189).
  modules: ModulesClient;
  // Account-token custody (memql#3322): plaintext exists only in the mint
  // reply; these are the typed identity-subpackage functions, bound to the
  // stream.
  mintAccountToken: (args: MintAccountTokenArgs) => Promise<AccountTokenMintResult>;
  revokeAccountToken: (identityId: string, signal?: AbortSignal) => Promise<AccountTokenRevokeResult>;
  // Structured-output suggestion (the composer's arrangement suggester).
  suggest: (
    domain: string,
    payload: Record<string, unknown>,
    opts?: AiSuggestOptions,
  ) => Promise<AiSuggestResult>;
}

export type ConnectionStatus =
  // No dial attempted, because the provider is disabled -- there is no
  // session to dial with yet (memql#3315). Distinct from "closed" (a stream
  // existed and ended) and from "error" (one was attempted and refused);
  // collapsing the three would make the header lie about which happened.
  | "idle"
  | "connecting"
  | "connected"
  | "error"
  | "closed";

export interface ClusterState {
  status: ConnectionStatus;
  // Present only while status === "connected". Everything that reads the
  // cluster goes through this, so a component cannot accidentally query a
  // connection that is still handshaking.
  query: QueryClient | null;
  // The live-update surface, from the SAME Connection as `query` (memql#3316).
  // Exposed alongside it, and nulled at the same moment, so a subscription
  // cannot outlive the stream it was opened on -- a CDC handler still firing
  // against a dead socket is how a list ends up "live" and wrong.
  subscriptions: SubscriptionManager | null;
  // The connection-scoped typed clients (see ClusterClients above). null
  // exactly when `query` is null.
  clients: ClusterClients | null;
  // Server identity from the ServerHello, for the rail profile. Empty until
  // connected.
  nodeId: string;
  serverVersion: string;
  // The release the node's binary was cut from (ServerHello.engine_version).
  // Empty when the node predates the field or the binary was not cut -- the
  // profile renders that as "dev". Not ServerHello.version (the wire
  // protocol, "v1").
  engineVersion: string;
  // Human-readable failure, empty unless status === "error".
  error: string;
  // Redial. Safe to call in any state; tears down whatever is live first.
  reconnect: () => void;
}

const ClusterContext = createContext<ClusterState | null>(null);

export interface ClusterProviderProps {
  children: ReactNode;
  // The credential source. Defaults to the no-credential source; the app
  // passes the identity-backed one (#3315). See src/cluster/auth.ts.
  auth?: PortalAuthSource;
  // Whether to dial at all. False parks the provider in "idle" and opens no
  // socket -- see the effect below for why that is not just an optimisation.
  enabled?: boolean;
  // Overridable for tests, which have no server to dial.
  dial?: typeof Connection.dial;
}

export function ClusterProvider({
  children,
  auth = anonymousAuthSource,
  enabled = true,
  dial = Connection.dial,
}: ClusterProviderProps): ReactNode {
  const [status, setStatus] = useState<ConnectionStatus>(enabled ? "connecting" : "idle");
  const [query, setQuery] = useState<QueryClient | null>(null);
  const [subscriptions, setSubscriptions] = useState<SubscriptionManager | null>(null);
  const [clients, setClients] = useState<ClusterClients | null>(null);
  const [nodeId, setNodeId] = useState("");
  const [serverVersion, setServerVersion] = useState("");
  const [engineVersion, setEngineVersion] = useState("");
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

    // Not yet dialable -- no session (#3315). Dialing anyway would open an
    // unauthenticated upgrade the front door refuses, retried behind a
    // sign-in page the operator has not filled in, which fills the ingress
    // log with 401s that describe nothing wrong. Bumping `generation` above
    // is what makes this ALSO cancel an in-flight dial when a session ends.
    if (!enabled) {
      setQuery(null);
      setSubscriptions(null);
      setClients(null);
      setStatus("idle");
      setError("");
      return;
    }

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
        // `?? null` rather than a bare read: the context's contract is
        // "a manager, or null", and a Connection-shaped object without one
        // (an older SDK, a test double narrowed to the methods it needs)
        // must land on the null branch that disables live updates rather
        // than on a call to undefined.subscribeGraph().
        setSubscriptions(conn.subscriptions ?? null);
        // `?? null` for the same reason as subscriptions above: a narrowed
        // test double without a dispatcher must land on the null branch that
        // hides the client-backed surfaces, not on a constructor over
        // undefined.
        const transport = conn.dispatcher ?? null;
        setClients(
          transport === null
            ? null
            : {
                deployControl: new DeployControlClient(transport),
                identityAdmin: new IdentityAdminClient(transport),
                modules: new ModulesClient(transport),
                mintAccountToken: (args) => mintAccountToken(transport, args),
                revokeAccountToken: (identityId, signal) =>
                  revokeAccountToken(transport, identityId, signal),
                suggest: (domain, payload, opts) => aiSuggest(transport, domain, payload, opts),
              },
        );
        setNodeId(conn.nodeId);
        setServerVersion(conn.serverVersion);
        setEngineVersion(conn.engineVersion ?? "");
        setStatus("connected");

        // The stream can end without anyone calling close() -- the node rolls,
        // the bearer is rejected, the front door drops the upgrade. Surface
        // that instead of leaving the indicator green over a dead socket.
        void conn.done().then(() => {
          if (generation.current !== mine) return;
          setQuery(null);
          setSubscriptions(null);
          setClients(null);
          setStatus("closed");
        });
      } catch (err) {
        if (cancelled || generation.current !== mine) return;
        setQuery(null);
        setSubscriptions(null);
        setClients(null);
        setStatus("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();

    return () => {
      cancelled = true;
      connection?.close();
    };
  }, [auth, dial, attempt, enabled]);

  const reconnect = useCallback(() => setAttempt((n) => n + 1), []);

  const value = useMemo<ClusterState>(
    () => ({
      status,
      query,
      subscriptions,
      clients,
      nodeId,
      serverVersion,
      engineVersion,
      error,
      reconnect,
    }),
    [status, query, subscriptions, clients, nodeId, serverVersion, engineVersion, error, reconnect],
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
