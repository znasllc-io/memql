import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../../chrome/access";
import { useLiveCollection, usePagedCollection } from "../../../live/useLiveCollection";
import { WORKER_REGISTRATION_CONCEPT } from "../../../live/machines";
import { machineFromRow, isRevoked, type MachineRow } from "../rows";

/** Mirror server membership for graph updates; search is executed by the
 * server before the first page, not against the ten records already loaded. */
function matches(machine: MachineRow, search: string, includeRevoked: boolean): boolean {
  if (!includeRevoked && isRevoked(machine)) return false;
  const text = search.trim().toLowerCase();
  return !text || [machine.displayName, machine.name, machine.hostname, machine.os, machine.arch].some(value => value.toLowerCase().includes(text));
}

export function useMachineDirectory(search: string, includeRevoked: boolean) {
  const { access } = useSession();
  const viewer = access?.userId ?? "";
  const query = search.trim();
  const feed = usePagedCollection<Row>(viewer ? `fleet:directory:${JSON.stringify([viewer, query, includeRevoked])}` : null, connection => ({
    concept: WORKER_REGISTRATION_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.myMachinesBrowse({ search: query || undefined, includeRevoked }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (registrationId, signal) => (await connection.query.myMachineById({ registrationId }, { signal })).rows()[0] ?? null,
    comparePageRows: (a, b) => String(b["registeredAt"] ?? "").localeCompare(String(a["registeredAt"] ?? "")) || String(b["id"]).localeCompare(String(a["id"])),
    inScope: raw => {
      const machine = machineFromRow(raw);
      return machine.ownerUserId === viewer && matches(machine, query, includeRevoked);
    },
  }));
  const machines = useMemo(() => feed.snapshot.rows.map(machineFromRow).filter(machine => machine.id)
    .sort((a, b) => b.registeredAt.localeCompare(a.registeredAt) || b.id.localeCompare(a.id)), [feed.snapshot]);
  return { ...feed, machines };
}

/** Detail identity is independent of search and loaded pages. A deep link can
 * open a machine beyond the current page, and changing search cannot discard
 * the selected machine or a retained settings draft. */
export function useMachineRecord(registrationId: string) {
  const { access } = useSession();
  const viewer = access?.userId ?? "";
  const feed = useLiveCollection<Row>(viewer && registrationId ? `fleet:machine:${JSON.stringify([viewer, registrationId])}` : null, connection => ({
    concept: WORKER_REGISTRATION_CONCEPT,
    eventInScope: id => id === registrationId,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.myMachineById({ registrationId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await connection.query.myMachineById({ registrationId: id }, { signal })).rows()[0] ?? null,
    inScope: raw => {
      const machine = machineFromRow(raw);
      return machine.id === registrationId && machine.ownerUserId === viewer;
    },
    paged: false,
  }));
  const machine = useMemo(() => feed.snapshot.rows[0] ? machineFromRow(feed.snapshot.rows[0]) : undefined, [feed.snapshot]);
  return { ...feed, machine };
}
