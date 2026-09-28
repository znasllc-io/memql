import { useCallback, useMemo, useSyncExternalStore } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useNow } from "../../../kit";
import { useLiveView } from "../../../live/liveView";
import { useMachines } from "../../../live/machines";
import { useInferenceStatus } from "../../settings/routingFacts";
import { isWorkerOnline } from "../online";
import { isRevoked, machineFromRow, type MachineRow } from "../rows";
import { factsFrom, type RouteLike, type RoutingFacts } from "./sources";
import { useRoutingPolicy } from "./useRoutingPolicy";

// The facts a route's readiness is read from, assembled from reads this app
// already makes -- no new query:
//
//   machines     the LIVE registration feed (MachinesProvider), so a machine
//                that goes to sleep dims its slots without a refresh
//   vendors      `inferenceStatus`, read once, the same reading Levels uses
//   preference   the owner's model order, off the live machine-choice row,
//                because `fleet:strongest` honours it first

export function useRoutingFacts(routes: readonly RouteLike[]): RoutingFacts {
  const { collection, settled } = useMachines();
  const view = useLiveView<Row, MachineRow>(collection, "fleet:routing:machines", (rows) =>
    rows.map(machineFromRow).filter((m) => m.id),
  );
  const subscribe = useCallback((listener: () => void) => view?.subscribe(listener) ?? (() => {}), [view]);
  const snapshot = useSyncExternalStore(subscribe, () => view?.snapshot ?? null, () => null);
  const now = useNow(15_000);
  const inference = useInferenceStatus(true);
  const machineChoice = useRoutingPolicy();

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
        },
        routes,
        preference: preference ?? [],
      }),
    [settled, snapshot?.error, rows, now, inference.status, routes, preference],
  );
}
