// The shell's ONE sdk-core Connection (spec D7). Dialed after sign-in at
// the edge's same-origin bridge (`/_memql/ws` -- the OS is served by
// component/edge exactly like the portal, so the cluster is always the
// origin that served the bundle: no endpoint config, no CORS). The SDK
// owns reconnect, resubscription and bearer rotation; this provider only
// renders status and hands out the live handles.

import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Connection,
  type ConnectionAuth,
  type ConnectionStatus,
} from "@znasllc-io/memql-sdk-core/client";

import type { OsAuthSource } from "../auth/source";
import { ConnectionStatusContext, type ShellConnectionStatus } from "../chrome/connection";

/** The edge's same-origin API marker, resolved against document.location. */
export function bridgePathFor(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + "_memql/ws";
}

export const osBridgePath = bridgePathFor(import.meta.env.BASE_URL);

interface OsConnectionValue {
  connection: Connection | null;
}

const Ctx = createContext<OsConnectionValue>({ connection: null });

export function useOsConnection(): Connection | null {
  return useContext(Ctx).connection;
}

async function toAuth(source: OsAuthSource): Promise<ConnectionAuth | undefined> {
  const bearer = await source.bearer();
  if (bearer === null || bearer === "") return undefined;
  return { bearer, onTokenExpired: () => source.refresh() };
}

const REDIAL_DELAY_MS = 5_000;

export function OsConnectionProvider({
  authSource,
  enabled,
  children,
}: {
  authSource: OsAuthSource;
  enabled: boolean;
  children: ReactNode;
}) {
  const [connection, setConnection] = useState<Connection | null>(null);
  const [status, setStatus] = useState<ShellConnectionStatus>("disconnected");
  const generation = useRef(0);

  useEffect(() => {
    if (!enabled) {
      setConnection(null);
      setStatus("disconnected");
      return;
    }
    generation.current += 1;
    const mine = generation.current;
    let conn: Connection | null = null;
    let unsubscribe: (() => void) | null = null;
    let redial: ReturnType<typeof setTimeout> | null = null;
    let disposed = false;

    const open = async () => {
      setStatus("reconnecting");
      try {
        const auth = await toAuth(authSource);
        const dialed = await Connection.dial({
          endpoint: osBridgePath,
          ...(auth ? { auth } : {}),
          clientId: "memql-os",
          sdkName: "memql-os",
          // SDK-owned reconnect (memql#4537): redial, replay every
          // subscription, and report status -- the dock dot renders it.
          reconnect: { enabled: true },
        });
        if (disposed || generation.current !== mine) {
          dialed.close();
          return;
        }
        conn = dialed;
        unsubscribe = dialed.onStatusChange((ev: { status: ConnectionStatus }) => {
          setStatus(ev.status);
        });
        setConnection(dialed);
      } catch {
        if (disposed || generation.current !== mine) return;
        // The INITIAL dial failed (SDK reconnect only exists once a stream
        // did): report honestly and retry on a quiet cadence.
        setStatus("disconnected");
        redial = setTimeout(() => void open(), REDIAL_DELAY_MS);
      }
    };

    void open();
    return () => {
      disposed = true;
      if (redial) clearTimeout(redial);
      unsubscribe?.();
      conn?.close();
      setConnection(null);
    };
  }, [enabled, authSource]);

  const value = useMemo(() => ({ connection }), [connection]);
  return (
    <Ctx.Provider value={value}>
      <ConnectionStatusContext.Provider value={status}>{children}</ConnectionStatusContext.Provider>
    </Ctx.Provider>
  );
}
