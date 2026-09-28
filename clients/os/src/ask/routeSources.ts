import { useCallback, useMemo } from "react";

import { useReading, type ReadingState } from "../cluster/reading";
import { isWorkerOnline } from "../apps/fleet/online";
import { doorsFromRow, type DoorsReading } from "../apps/fleet/models/useInference";
import { isRevoked, machineFromRow, machineName, type MachineRow } from "../apps/fleet/rows";
import { useMachines } from "../live/machines";
import { useOsConnection } from "../live/connection";
import { WHERE_LABEL, ROUTE_WHERE, type AskLevel, type RouteWhere } from "./askRoute";

// Which of the picker's Where rows can serve RIGHT NOW, and in one line why
// or why not -- computed from the readings Fleet already takes, never from
// anything this surface invents (design brief section 6: unready rows stay
// visible, quiet, not choosable, with a one-line reason).
//
// - Apps (Claude Code, Codex): the machines feed (`myWorkersWithStatus`,
//   already retained by the shell's MachinesProvider), read the way Fleet's
//   Machines section reads it: a machine that is online AND reports the app
//   allowed and signed in (`rows.ts` appsFrom) can open a session.
// - Local and Vendor: `inferenceStatus`, the Model library's reading of which
//   doors this caller can reach. It is a virtual projection with no event
//   behind it, so it is read when the picker opens.
//
// A reading that has not answered is PENDING, not "no": its rows keep their
// shape with a skeleton where the line goes, and cannot be chosen until the
// answer lands. A reading that failed says so in the line.

export type SourceState = "ready" | "unready" | "pending";

export interface RouteOption {
  where: RouteWhere;
  label: string;
  state: SourceState;
  /** When ready, what the row means; when unready, why not. Empty while pending. */
  note: string;
}

export interface Settled<T> {
  state: "pending" | "failed" | "read";
  value: T;
}

export interface RouteFacts {
  machines: Settled<MachineRow[]>;
  doors: Settled<DoorsReading | null>;
}

const COULD_NOT_CHECK = "Could not check";

function appOption(where: "claude-code" | "codex", machines: Settled<MachineRow[]>, now: Date): RouteOption {
  const label = WHERE_LABEL[where];
  if (machines.state === "pending") return { where, label, state: "pending", note: "" };
  if (machines.state === "failed") return { where, label, state: "unready", note: COULD_NOT_CHECK };
  const holding = machines.value
    .filter((machine) => !isRevoked(machine))
    .map((machine) => ({ machine, app: machine.apps.find((app) => app.id === where), online: isWorkerOnline(machine, now) }))
    .filter((entry) => entry.app !== undefined);
  const serving = holding.find((entry) => entry.online && entry.app!.runnable);
  if (serving) return { where, label, state: "ready", note: `On ${machineName(serving.machine)}` };
  if (holding.length === 0) return { where, label, state: "unready", note: "Not installed on any machine" };
  // An online machine's reason is the one somebody can act on now; an
  // offline one only says where to look.
  const online = holding.find((entry) => entry.online);
  if (online) {
    const name = machineName(online.machine);
    return { where, label, state: "unready", note: online.app!.allowed ? `Not signed in on ${name}` : `Not allowed on ${name}` };
  }
  return { where, label, state: "unready", note: `${machineName(holding[0]!.machine)} is offline` };
}

/** Every Where row, in the picker's order, with its state and line. */
export function routeOptions(facts: RouteFacts, level: AskLevel, now: Date = new Date()): RouteOption[] {
  const { doors } = facts;
  return ROUTE_WHERE.map((where): RouteOption => {
    const label = WHERE_LABEL[where];
    switch (where) {
      case "auto":
        return { where, label, state: "ready", note: "Follows your rules" };
      case "claude-code":
      case "codex":
        return appOption(where, facts.machines, now);
      case "local":
        if (doors.state === "pending") return { where, label, state: "pending", note: "" };
        if (doors.state === "failed" || doors.value === null) return { where, label, state: "unready", note: COULD_NOT_CHECK };
        return doors.value.localEligible
          ? { where, label, state: "ready", note: "Your machines" }
          : { where, label, state: "unready", note: "No local model online" };
      case "vendor":
        if (doors.state === "pending") return { where, label, state: "pending", note: "" };
        if (doors.state === "failed" || doors.value === null) return { where, label, state: "unready", note: COULD_NOT_CHECK };
        if (!doors.value.federationConfigured) return { where, label, state: "unready", note: "No vendor key" };
        return { where, label, state: "ready", note: level === "strong" || level === "reasoning" ? "Strongest vendor" : "Cheapest vendor" };
    }
  });
}

function settledReading<T>(state: ReadingState, value: T | null, empty: T): Settled<T> {
  if (state === "read") return { state: "read", value: value ?? empty };
  if (state === "failed") return { state: "failed", value: empty };
  return { state: "pending", value: empty };
}

/** The picker's two readings, live while it is open. */
export function useRouteFacts(): RouteFacts {
  const connection = useOsConnection();
  const feed = useMachines();

  const readDoors = useCallback(
    async (signal: AbortSignal): Promise<DoorsReading | null> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.inferenceStatus({}, { signal });
      return doorsFromRow(result.single());
    },
    [connection],
  );
  const doors = useReading<DoorsReading | null>("ask:route:doors", connection === null ? null : readDoors);

  // `presence` changes identity on every fold of the feed, so it is what
  // re-derives the rows when a machine comes online while the picker is open.
  const machines = useMemo((): Settled<MachineRow[]> => {
    const rows = feed.collection?.snapshot?.rows ?? [];
    if (feed.feedState === "disconnected") return { state: "failed", value: [] };
    if (!feed.settled) return { state: "pending", value: [] };
    return { state: "read", value: rows.map(machineFromRow).filter((machine) => machine.id !== "") };
  }, [feed.collection, feed.feedState, feed.settled, feed.presence]);

  return { machines, doors: settledReading(doors.state, doors.value, null) };
}
