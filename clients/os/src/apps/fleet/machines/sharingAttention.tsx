import { useCallback, useMemo, useSyncExternalStore } from "react";

import { usePublishAttention } from "../../../attention/Attention";
import type { AttentionChange } from "../../../attention/model";
import { useSessionIfPresent } from "../../../chrome/access";
import { useMachines } from "../../../live/machines";
import { machineFromRow, type MachineRow } from "../rows";
import { canLend } from "./sharing";

// The unseen-change marker for sharing a machine with people and groups
// (epic memql#5344, design G15; clients/os/README.md "Unseen changes").
//
// ===========================================================================
// A RUNTIME CHANGE, BECAUSE IT IS NEWS TO SOME PEOPLE AND NOT TO OTHERS
// ===========================================================================
// Lending a machine to specific people is a new capability, and it is only
// one for somebody who HAS a machine to lend. A static manifest declaration
// would mark Fleet for everybody on the cluster -- including the people the
// feature is least about, who own nothing and would open the app to find a
// marker leading nowhere they can act. So it is published from here, at shell
// lifetime (the Deployables precedent), and only while the viewer owns a
// machine that is still in the fleet.
//
// The destination is that machine's Sharing view, and nothing above it: the
// app, the Machines section and a machine's Equipment view carry the mark and
// never acknowledge it.

/** Where the change leads: the Machines section, the Sharing view. */
export const MACHINE_SHARING_SECTION = "machines";
export const MACHINE_SHARING_TARGET = "machine-sharing";

/**
 * The one change. Advance `revision` only when sharing itself changes in a
 * way worth a person's attention again -- never because a machine was added,
 * which is data moving rather than the surface.
 */
export const MACHINE_SHARING_CHANGE: AttentionChange = {
  id: "fleet:machine-sharing",
  revision: "people-and-groups-1",
  appId: "fleet",
  sectionId: MACHINE_SHARING_SECTION,
  target: MACHINE_SHARING_TARGET,
  label: "Share a machine with people and groups",
  kind: "runtime",
};

/**
 * The change, when it applies to this viewer.
 *
 * OWNED, NOT MERELY LISTED. The machines feed can hold other people's
 * machines -- a cluster owner's subscription is admitted to every
 * registration row, and the collection folds whatever it is sent -- and those
 * are not the viewer's to lend.
 */
export function machineSharingAttention(machines: readonly MachineRow[], viewerId: string): AttentionChange[] {
  return machines.some((machine) => canLend(machine, viewerId)) ? [MACHINE_SHARING_CHANGE] : [];
}

/** Lives with the shell, so a closed Fleet is still marked. Renders nothing. */
export function FleetSharingAttentionFeed() {
  const { collection } = useMachines();
  const viewerId = useSessionIfPresent()?.access?.userId ?? "";
  const subscribe = useCallback((listener: () => void) => collection?.subscribe(listener) ?? (() => {}), [collection]);
  const snapshot = useSyncExternalStore(subscribe, () => collection?.snapshot ?? null, () => null);
  // The collection holds RAW rows (live/machines.tsx), so they are projected
  // here, on the read side, like every other consumer's.
  const changes = useMemo(
    () => machineSharingAttention((snapshot?.rows ?? []).map(machineFromRow), viewerId),
    [snapshot, viewerId],
  );
  usePublishAttention("fleet:machine-sharing", changes);
  return null;
}
