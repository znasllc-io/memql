import { useCallback, useEffect, useMemo, useState } from "react";

import type { Connection, Row } from "@znasllc-io/memql-sdk-core/client";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../chrome/access";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection } from "../../live/useLiveCollection";
import { roleAdmits } from "../../system/roles";
import {
  clusterFromRow,
  databaseFromRow,
  deploymentFromRow,
  identityProviderFromRow,
  latestDeployment,
  nodeSpecFromRow,
  providerFromRow,
  resolvedVersion,
  ridesTheSpine,
  type ClusterRow,
  type DatabaseRow,
  type DeploymentRow,
  type IdentityProviderRow,
  type NodeSpecRow,
  type ProviderStatusRow,
} from "./clusterRows";
import { readMailStatus, type MailStatus } from "./mailStatus";
import type { ClusterReport } from "./buildDiagnosticsReport";

// The Cluster facts hooks (memql#4742).
//
// TWO KINDS OF FACT, READ TWO WAYS, AND THE SPLIT IS THE DESIGN.
//
// Graph rows (the cluster singleton, the deployment history) are live: they
// are rows somebody writes, the mesh broadcasts their events, and a
// deployment landing while an operator watches is exactly the thing a
// console should show without being asked.
//
// Registry projections (`integrationStatus`, `providerAuthStatus`) are
// request/reply with an explicit Refresh and a fetched-at stamp. They are
// projections of ONE NODE's in-memory state, not of rows anyone writes, so
// there is no graph event to subscribe to -- and a panel that LOOKED live
// while showing a registry that stopped moving would invite an operator to
// trust a minutes-old reading. Which replica answered is not knowable from
// here, and the panel says so. This is the same deliberate downgrade
// `clients/portal/src/admin/useProviders.ts` documents.

/** The section's role floor. Presentation only; every gate is server-side. */
export const CLUSTER_SECTION_ROLE = { min: "admin" } as const;

export interface AsyncFacts<T> {
  value: T | null;
  loading: boolean;
  /** The engine's own refusal text when the read was declined. */
  error: string;
  /** Client clock, for reads whose reply carries no server stamp. */
  fetchedAt: number | null;
  reload: () => void;
}

// ---------------------------------------------------------------- graph rows

export interface ClusterIdentity {
  cluster: ClusterRow | null;
  /** True while the seed has not settled. */
  loading: boolean;
}

export function useClusterIdentity(): ClusterIdentity {
  const connection = useOsConnection();
  const handle = useLiveCollection<Row>(connection === null ? null : "settings:cluster", (conn) => ({
    concept: Concepts.CLUSTER_CLUSTER,
    seed: async (_cursor, signal) => {
      const result = await conn.query.existingCluster({}, { signal });
      return { rows: [...result.rows()], nextCursor: "" };
    },
  }));

  const rows = handle.snapshot.rows;
  const cluster = useMemo(() => (rows.length > 0 ? clusterFromRow(rows[0]!) : null), [rows]);
  return { cluster, loading: handle.snapshot.state === "seeding" };
}

/**
 * The cluster's database and identity-provider rows (memql#4766).
 *
 * ONE HOOK FOR BOTH because they are one question -- "what is this cluster
 * built on" -- read from two singletons, and a panel that loaded them
 * separately would show half an answer twice as often.
 *
 * NOT a live collection, unlike the cluster and deployment feeds above. These
 * rows change when a bff restarts, which is not something to hold a
 * subscription open for; a read on open is the honest cadence and it says when
 * it looked.
 *
 * CLUSTER-OWNER ONLY, while this section admits admin. That gap is deliberate
 * and is the same one `providerAuthStatus` has: the reads carry
 * `requiresOwner && actor.isClusterOwner==true`, so an admin gets an empty
 * result rather than an error. `enabled` keeps us from issuing a read we know
 * will come back empty; it is a courtesy and never the authorization.
 */
export interface InfrastructureFacts {
  database: DatabaseRow | null;
  identityProvider: IdentityProviderRow | null;
  loading: boolean;
  error: string;
}

export function useInfrastructureFacts(enabled: boolean): InfrastructureFacts {
  const connection = useOsConnection();
  const [state, setState] = useState<InfrastructureFacts>({
    database: null,
    identityProvider: null,
    loading: false,
    error: "",
  });

  useEffect(() => {
    if (connection === null || !enabled) {
      setState({ database: null, identityProvider: null, loading: false, error: "" });
      return;
    }
    const controller = new AbortController();
    let live = true;
    setState((held) => ({ ...held, loading: true, error: "" }));
    void (async () => {
      try {
        const [db, idp] = await Promise.all([
          connection.query.clusterDatabase({}, { signal: controller.signal }),
          connection.query.clusterIdentityProvider({}, { signal: controller.signal }),
        ]);
        if (!live) return;
        const dbRows = [...db.rows()];
        const idpRows = [...idp.rows()];
        setState({
          database: dbRows.length > 0 ? databaseFromRow(dbRows[0]!) : null,
          identityProvider: idpRows.length > 0 ? identityProviderFromRow(idpRows[0]!) : null,
          loading: false,
          error: "",
        });
      } catch (err: unknown) {
        if (!live) return;
        setState({
          database: null,
          identityProvider: null,
          loading: false,
          error: err instanceof Error ? err.message : String(err),
        });
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, enabled]);

  return state;
}

export interface DeploymentFacts {
  latest: DeploymentRow | null;
  specs: readonly NodeSpecRow[];
  loading: boolean;
  error: string;
}

export function useDeploymentFacts(clusterId: string): DeploymentFacts {
  const connection = useOsConnection();
  const handle = useLiveCollection<Row>(
    connection === null || clusterId === "" ? null : `settings:deployments:${clusterId}`,
    (conn) => ({
      concept: Concepts.CLUSTER_DEPLOYMENT,
      seed: async (_cursor, signal) => {
        const result = await conn.query.deploymentsForCluster({ clusterId }, { signal });
        return { rows: [...result.rows()], nextCursor: "" };
      },
    }),
  );

  const rows = handle.snapshot.rows;
  const latest = useMemo(() => latestDeployment(rows.map(deploymentFromRow)), [rows]);

  // The per-node specs are a FETCH keyed off the resolved deployment, never
  // a collection keyed on it: the deployment id resolves asynchronously, and
  // a collection key that carries async-resolved state tears its
  // subscription down and re-seeds the moment the value lands.
  const [specs, setSpecs] = useState<readonly NodeSpecRow[]>([]);
  const [error, setError] = useState("");
  const deploymentId = latest?.deploymentId ?? "";

  useEffect(() => {
    if (connection === null || deploymentId === "") {
      setSpecs([]);
      return;
    }
    let stale = false;
    setError("");
    void connection.query
      .nodeSpecsForDeployment({ deploymentId })
      .then((result) => {
        if (stale) return;
        setSpecs([...result.rows()].map(nodeSpecFromRow));
      })
      .catch((err: unknown) => {
        if (stale) return;
        setSpecs([]);
        setError(messageOf(err));
      });
    return () => {
      stale = true;
    };
  }, [connection, deploymentId]);

  return { latest, specs, loading: handle.snapshot.state === "seeding", error };
}

// ------------------------------------------------------- registry projections

export function useMailStatus(enabled: boolean): AsyncFacts<MailStatus> {
  return useRequestReply(enabled, async (conn, signal) => {
    // probe=false: the configuration question only. A live reachability
    // check reaches a third party on the caller's say-so, so it is a
    // control someone presses, never something a panel does on render.
    const result = await conn.query.integrationStatus({ probe: false }, { signal });
    return readMailStatus([...result.rows()]);
  });
}

export function useProviderStatus(enabled: boolean): AsyncFacts<readonly ProviderStatusRow[]> {
  return useRequestReply(enabled, async (conn, signal) => {
    const result = await conn.query.providerAuthStatus({}, { signal });
    return [...result.rows()].map(providerFromRow);
  });
}

function useRequestReply<T>(
  enabled: boolean,
  read: (conn: Connection, signal: AbortSignal) => Promise<T | null>,
): AsyncFacts<T> {
  const connection = useOsConnection();
  const [value, setValue] = useState<T | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  // Refresh is an epoch counter, not a cache invalidation protocol: the
  // question "what does a node say right now" has no cache to invalidate.
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void read(connection, controller.signal)
      .then((next) => {
        if (stale) return;
        setValue(next);
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        // A server-side refusal arrives as a rejected promise carrying the
        // engine's own words. It is rendered IN-SURFACE where the panel
        // would be, never as a toast, and never rewritten -- an admin
        // reading "providerAuthStatus is owner-only" has been told exactly
        // what happened, which no paraphrase of ours would improve on.
        setValue(null);
        setError(messageOf(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
    // `read` is re-created every render by design; the effect is keyed on
    // what actually changes the answer.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connection, enabled, epoch]);

  return { value, loading, error, fetchedAt, reload };
}

// ------------------------------------------------------------- the diagnostics
// adapter

/**
 * The cluster half of the diagnostics report. Three states rather than an
 * optional: a session that may not read these facts and a session whose
 * read failed need different next steps, and a report that flattened both
 * into an absent section would tell its reader neither.
 */
export function useClusterReport(): ClusterReport {
  const { access } = useSession();
  const admitted = roleAdmits(access?.clusterRole ?? "", CLUSTER_SECTION_ROLE);
  const identity = useClusterIdentity();
  const deployment = useDeploymentFacts(admitted ? (identity.cluster?.id ?? "") : "");
  const mail = useMailStatus(admitted);

  if (!admitted) return { state: "not-admitted" };
  if (mail.error !== "") return { state: "unavailable", reason: mail.error };

  const lines: [string, string][] = [];
  if (identity.cluster) {
    lines.push(["Cluster", identity.cluster.name]);
    lines.push(["Region", identity.cluster.region]);
    // NO "Status" LINE. `v1:cluster:cluster.status` was removed (memql#4772):
    // it had one writer that stamped a constant at bootstrap and nothing that
    // ever refreshed it, so this row told an operator "healthy" with every
    // node in the cluster down -- a health verdict indistinguishable from a
    // real one, which is what memql#4766 called unshippable when it was the
    // database's.
    //
    // What is left on this panel IS observed: the deployment's own status and
    // per-node-type versions and replica counts below come from
    // `v1:cluster:deployment` rows something actually writes. If a cluster
    // health line is wanted here, DERIVE it at read time from
    // `seedNodeTypes` against live `v1:cluster:node` rows -- do not restore a
    // stored field.
    lines.push(["Provider", identity.cluster.provider]);
    lines.push(["Cluster version", identity.cluster.version]);
  }
  if (deployment.latest) {
    lines.push(["Deployment", deployment.latest.deploymentId]);
    lines.push(["Deployment status", deployment.latest.status]);
    lines.push(["Engine version", deployment.latest.version]);
    for (const spec of deployment.specs) {
      const version = resolvedVersion(spec, deployment.latest.version);
      const spine = ridesTheSpine(spec) ? " (engine version)" : "";
      lines.push([`  ${spec.nodeType}`, `${version}${spine} x${spec.replicas}`]);
    }
  }
  if (mail.value) {
    lines.push(["Mail sender", mail.value.mode]);
    lines.push(["Mail health", mail.value.health]);
  }
  // AI provider status is owner-only and is deliberately NOT in the report:
  // which vendors a deployment talks to and which are misconfigured is the
  // reconnaissance the builtin's own gate exists to keep off a wider
  // audience, and a report is pasted somewhere its audience is not known.
  return { state: "facts", lines };
}

function messageOf(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
