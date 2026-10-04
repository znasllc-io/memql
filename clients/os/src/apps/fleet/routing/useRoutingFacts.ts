import { useCallback, useMemo, useSyncExternalStore } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useNow } from "../../../kit";
import { useSession } from "../../../chrome/access";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { useLiveView } from "../../../live/liveView";
import { useMachines } from "../../../live/machines";
import { useInferenceStatus } from "../../settings/routingFacts";
import { isWorkerOnline } from "../online";
import { delegationPolicyFromRow, isRevoked, machineFromRow, type MachineRow } from "../rows";
import { factsFrom, type RouteLike, type RoutingFacts } from "./sources";
import { useRoutingPolicy } from "./useRoutingPolicy";

// The facts a route's readiness is read from, assembled from reads this app
// already makes -- no new query:
//
//   machines     the LIVE registration feed (MachinesProvider), so a machine
//                that goes to sleep dims its slots without a refresh
//   vendors      `inferenceStatus`, read once, the same reading Levels uses --
//                which also carries the engine's local floor
//                (`eligibleModelIds`) the strongest/fastest selectors honour
//   preference   the owner's model order, off the live machine-choice row,
//                because `fleet:strongest` honours it first
//   app order    the owner's delegation app order, read once, because
//                `app:*` takes the first runnable app in it
//
// NOT `appSessionsInstalled`. That field says whether the NODE ANSWERING can
// open app sessions, and the node answering an OS read is the BFF -- app
// sessions open on agent replicas -- so it is false on every healthy cluster
// and would mark every app idle.

export function useRoutingFacts(routes: readonly RouteLike[], enabled = true): RoutingFacts {
  const { collection, settled } = useMachines();
  const view = useLiveView<Row, MachineRow>(collection, "fleet:routing:machines", (rows) =>
    rows.map(machineFromRow).filter((m) => m.id),
  );
  const subscribe = useCallback((listener: () => void) => view?.subscribe(listener) ?? (() => {}), [view]);
  const snapshot = useSyncExternalStore(subscribe, () => view?.snapshot ?? null, () => null);
  const now = useNow(15_000);
  const inference = useInferenceStatus(true);
  const machineChoice = useRoutingPolicy();
  const connection = useOsConnection();
  const { access } = useSession();
  const readOrder = useMemo(() => {
    const query = connection?.query ?? null;
    if (!enabled || query === null) return null;
    return async (signal: AbortSignal) => {
      const row = (await query.delegationPolicyForUser({}, { signal })).rows()[0];
      return row === undefined ? [] : delegationPolicyFromRow(row).appOrder;
    };
  }, [connection, enabled]);
  const appOrder = useReading<string[]>(`fleet:routing:appOrder:${access?.userId ?? ""}`, readOrder).value;

  const rows = snapshot?.rows;
  const preference = machineChoice.policy?.modelPreference;
  return useMemo(
    () =>
      factsFrom({
        machinesRead: settled && !snapshot?.error,
        machines: (rows ?? []).map((m) => ({
          id: m.id,
          name: m.name,
          displayName: m.displayName,
          online: isWorkerOnline(m, now),
          revoked: isRevoked(m),
          labels: m.reportedLabels,
          apps: m.apps,
        })),
        inference: {
          read: inference.status.read,
          federationConfigured: inference.status.federationConfigured,
          error: inference.status.error,
          eligibleModelIds: inference.status.eligibleModelIds,
          fleetCatalogInstalled: inference.status.fleetCatalogInstalled,
        },
        routes,
        preference: preference ?? [],
        appOrder: appOrder ?? [],
      }),
    [settled, snapshot?.error, rows, now, inference.status, routes, preference, appOrder],
  );
}
