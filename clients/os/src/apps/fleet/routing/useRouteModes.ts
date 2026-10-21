import { useCallback, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { useReading } from "../../../cluster/reading";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useGraphRevision } from "../../../live/useGraphRevision";
import { capacityFrom, type CapacityFacts } from "./capacity";
import {
  autoBalanceFrom,
  budgetFrom,
  routeModeFrom,
  routeModesFrom,
  shippedRouteModes,
  type AutoBalance,
  type Balance,
  type Budget,
  type JobSources,
  type RouteMode,
} from "./routeModes";

// The reads and writes behind Fleet > Routing > Routes.
//
// EVERY BUILTIN IS ASKED FOR BY NAME AND MAY BE ABSENT. The routes builtins
// land with the engine's routes work; a cluster without them answers "no such
// method" by having none on the connection, and each reading says so with
// `available: false` instead of failing -- the page then draws what it can
// (the shipped built-ins, read-only) rather than an error over everything.
//
// NONE OF THESE IS LIVE. Routes, Auto's balance, a budget and the capacity
// measurement are answered per request from registries and ledgers no graph
// event describes, so each is READ, re-read on the graph changes that move
// its answer (a machine registering, an embedder bound), and re-read after
// every write this page makes. Capacity also re-reads on the page's own
// AutoRefresh cadence, because slots fill and free with no event at all.

type Builtin = (args: Record<string, unknown>, opts?: { signal?: AbortSignal; priority?: string }) => Promise<{ rows(): Iterable<Row> }>;

function builtin(query: unknown, name: string): Builtin | null {
  const fn = (query as Record<string, unknown> | null | undefined)?.[name];
  return typeof fn === "function" ? (fn as Builtin).bind(query) : null;
}

async function rowsOf(read: Builtin, args: Record<string, unknown>, signal?: AbortSignal): Promise<Row[]> {
  const result = await read(args, { signal, priority: "background" });
  return [...result.rows()];
}

/** Concepts whose changes move a route's readiness or the page's facts. */
const ROUTE_CONCEPTS = ["v1:worker:registration", "v1:worker:delegationPolicy", "v1:platform:embedderBinding", "v1:router:route", "v1:router:budget"];

export interface RouteModesState {
  routes: RouteMode[];
  revision: number;
  /** True before the first read lands. */
  loading: boolean;
  error: string;
  /** False on a cluster without `routesForActor`: the routes drawn are the shipped ones. */
  available: boolean;
  reread: () => void;
}

export function useRouteModes(enabled = true): RouteModesState {
  const connection = useOsConnection();
  const { access } = useSession();
  const live = useGraphRevision(ROUTE_CONCEPTS, enabled);
  const [available, setAvailable] = useState(true);
  const read = useMemo(() => {
    if (!enabled || connection === null) return null;
    return async (signal: AbortSignal) => {
      const fn = builtin(connection.query, "routesForActor");
      if (fn === null) {
        setAvailable(false);
        return { routes: shippedRouteModes(), revision: 0 };
      }
      setAvailable(true);
      return routeModesFrom(await rowsOf(fn, {}, signal));
    };
  }, [connection, enabled]);
  const reading = useReading(`fleet:routing:routes:${access?.userId ?? ""}`, read, live);
  return {
    routes: reading.value?.routes ?? [],
    revision: reading.value?.revision ?? 0,
    loading: reading.value === null && reading.state !== "failed",
    error: reading.state === "failed" ? reading.error : "",
    available,
    reread: reading.reread,
  };
}

export interface ValueState<T> {
  value: T | null;
  loading: boolean;
  error: string;
  available: boolean;
  reread: () => void;
}

function useOptionalRead<T>(name: string, key: string, project: (rows: Row[]) => T | null, enabled: boolean, revision: string | number): ValueState<T> {
  const connection = useOsConnection();
  const [available, setAvailable] = useState(true);
  const read = useMemo(() => {
    if (!enabled || connection === null) return null;
    return async (signal: AbortSignal): Promise<T | null> => {
      const fn = builtin(connection.query, name);
      if (fn === null) {
        setAvailable(false);
        return null;
      }
      setAvailable(true);
      return project(await rowsOf(fn, {}, signal));
    };
  }, [connection, enabled, name, project]);
  const reading = useReading<T | null>(key, read, revision);
  return {
    value: reading.value ?? null,
    // Only the FIRST read is loading: a re-read keeps the value on screen.
    loading: (reading.state === "unread" || reading.state === "reading") && reading.value === null && reading.at === null,
    error: reading.state === "failed" ? reading.error : "",
    available,
    reread: reading.reread,
  };
}

/** Auto's balance, cluster-wide. */
export function useAutoBalance(enabled = true): ValueState<AutoBalance> {
  const live = useGraphRevision(["v1:router:route"], enabled);
  return useOptionalRead("autoBalance", "fleet:routing:autoBalance", autoBalanceFrom, enabled, live);
}

/** The signed-in person's monthly AI budget and this month's spend. */
export function useMyBudget(enabled = true): ValueState<Budget> {
  const { access } = useSession();
  const live = useGraphRevision(["v1:router:budget"], enabled);
  return useOptionalRead("myBudget", `fleet:routing:budget:${access?.userId ?? ""}`, budgetFrom, enabled, live);
}

const capacityProject = (rows: Row[]) => capacityFrom(rows);

/** What each machine can take right now; null where the cluster does not measure it. */
export function useFleetCapacity(enabled = true): ValueState<CapacityFacts> {
  const { access } = useSession();
  const live = useGraphRevision(["v1:worker:registration"], enabled);
  return useOptionalRead("fleetCapacity", `fleet:routing:capacity:${access?.userId ?? ""}`, capacityProject, enabled, live);
}

const embedderProject = (rows: Row[]): string => {
  const row = rows[0] === undefined ? {} : flatten(rows[0]);
  const ref = typeof row["providerRef"] === "string" ? (row["providerRef"] as string) : "";
  const activated = typeof row["activatedAt"] === "string" && row["activatedAt"] !== "";
  return activated ? ref.replace(/^fleet:/, "") : "";
};

/** The cluster's bound embedding model ("" when none is bound). */
export function useBoundEmbedder(enabled = true): ValueState<string> {
  const live = useGraphRevision(["v1:platform:embedderBinding"], enabled);
  return useOptionalRead("activeEmbedderBinding", "fleet:routing:embedder", embedderProject, enabled, live);
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

export interface RouteWrites {
  /** Whether this cluster can save routes at all. */
  supported: boolean;
  /** Whether it can change Auto's balance. */
  balanceSupported: boolean;
  saveRoute: (input: { id?: string; name?: string; jobs: JobSources; revision?: number }) => Promise<RouteMode | null>;
  deleteRoute: (id: string) => Promise<void>;
  resetRoute: (id: "local" | "apps", revision: number) => Promise<void>;
  saveBalance: (balance: Balance, revision: number) => Promise<void>;
  resetBalance: (revision: number) => Promise<void>;
}

const NOT_ON_THIS_CLUSTER = "This cluster cannot save routes yet.";

export function useRouteWrites(): RouteWrites {
  const connection = useOsConnection();
  const call = useCallback(
    async (name: string, args: Record<string, unknown>): Promise<Row[]> => {
      if (connection === null) throw new Error("Not connected to the cluster, so nothing was written.");
      const fn = builtin(connection.query, name);
      if (fn === null) throw new Error(NOT_ON_THIS_CLUSTER);
      return rowsOf(fn, args);
    },
    [connection],
  );
  return useMemo(
    () => ({
      supported: connection !== null && builtin(connection.query, "routeSave") !== null,
      balanceSupported: connection !== null && builtin(connection.query, "autoBalanceSave") !== null,
      saveRoute: async ({ id, name, jobs, revision }) => {
        const rows = await call("routeSave", {
          ...(id ? { id } : {}),
          ...(name !== undefined ? { name } : {}),
          fast: jobs.fast,
          strong: jobs.strong,
          reasoning: jobs.reasoning,
          ...(revision !== undefined ? { revision } : {}),
        });
        return rows[0] === undefined ? null : routeModeFrom(rows[0]);
      },
      deleteRoute: async (id) => {
        await call("routeDelete", { id });
      },
      resetRoute: async (id, revision) => {
        await call("routeReset", { id, revision });
      },
      saveBalance: async (balance, revision) => {
        await call("autoBalanceSave", { speed: balance.speed, cost: balance.cost, quality: balance.quality, revision });
      },
      resetBalance: async (revision) => {
        await call("autoBalanceReset", { revision });
      },
    }),
    [call, connection],
  );
}
