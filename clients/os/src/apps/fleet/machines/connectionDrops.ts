import { useCallback } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { formatFreshness } from "../../../kit/format";

/**
 * A machine's recent connection drops (network-aware fleet selection): the
 * v1:worker:connectionDrop rows the cluster records when the machine's
 * connection ended in a way Cockpit did not choose -- replaced by a new
 * connection before the cluster saw it end, or ended with work running.
 * Pipeline steps prefer machines with none recent; Fleet shows the facts, not
 * that verdict, because the window belongs to each run's placement policy.
 *
 * `reading` and `unavailable` never show a figure: a count nobody read is not
 * zero (DESIGN.md, "Readable, truthful data").
 */
export type ConnectionDrops =
  | { state: "reading" }
  | { state: "unavailable" }
  | { state: "read"; count: number; capped: boolean; lastAt: string };

/** The query's page: a machine at it has "100 or more" drops. */
const READ_CAP = 100;

/** How far back the Details page looks. */
const LOOKBACK_MS = 24 * 60 * 60 * 1000;

/**
 * A time in the width the drop rows are written in (`DropTimeLayout` in
 * component/worker): RFC 3339 in UTC with nine fraction digits. The window is
 * a comparison of text, and `toISOString` writes three, so a window starting
 * at "...:56.123Z" left out a row written "...:56.123456789Z" in that
 * millisecond.
 */
export function dropTime(at: Date): string {
  return at.toISOString().replace(/Z$/, "000000Z");
}

/** The summary of the rows `myWorkerConnectionDrops` answered with. */
export function connectionDropsFromRows(rows: readonly Row[]): ConnectionDrops {
  let lastAt = "";
  for (const row of rows) {
    const at = rowString(row, "droppedAt");
    if (at !== "" && (lastAt === "" || Date.parse(at) > Date.parse(lastAt))) lastAt = at;
  }
  return { state: "read", count: rows.length, capped: rows.length >= READ_CAP, lastAt };
}

/** The fact's words, in the Round trip fact's style: "3 in the last day, last 2h ago". */
export function connectionDropsWords(drops: ConnectionDrops, now: Date): { value: string; title?: string } {
  if (drops.state === "reading") return { value: "Checking" };
  if (drops.state === "unavailable") return { value: "Could not be read" };
  if (drops.count === 0) return { value: "None in the last day" };
  const count = drops.capped ? `${drops.count} or more` : String(drops.count);
  return { value: `${count} in the last day, last ${formatFreshness(drops.lastAt, now)}`, title: drops.lastAt };
}

/**
 * Reads one machine's drops of the last day, in the background, like the
 * machine's recommendation. There is no live subscription: the rows are
 * written by the agent replica that held the machine's stream and publish to
 * no browser, so the page's own refresh (`reread`) is what keeps it current.
 */
export function useConnectionDrops(workerId: string, enabled: boolean): { drops: ConnectionDrops; reread: () => void } {
  const connection = useOsConnection();
  const read = useCallback(async (signal: AbortSignal) => {
    const since = dropTime(new Date(Date.now() - LOOKBACK_MS));
    const result = await connection!.query.myWorkerConnectionDrops({ registrationId: workerId, since }, { signal, priority: "background" });
    return connectionDropsFromRows(result.rows() as Row[]);
  }, [connection, workerId]);
  const reading = useReading(`machine-connection-drops:${workerId}`, enabled && connection && workerId ? read : null);
  const drops: ConnectionDrops = reading.state === "failed" ? { state: "unavailable" } : reading.value ?? { state: "reading" };
  return { drops, reread: reading.reread };
}
