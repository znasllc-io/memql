import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { absent, figureFrom, type Figure } from "../../../cluster/figure";
import { boolOr, flatten, stringsOf } from "../../../kit";

// Joining the data-origins INVENTORY to the connector HEALTH, and every
// decision that join makes -- kept pure so the honesty properties can be
// asserted without a DOM.
//
// ===========================================================================
// TWO READS, AND NEITHER IS THE OTHER'S DEFAULT
// ===========================================================================
// `dataOrigins` is the DECLARATION: what every concept says about who owns
// its data. `syncStatesAll` is the OBSERVATION: how the connectors carrying
// it are actually doing. They come back separately and a concept can have one
// without the other -- a connector declared and never run has an inventory
// row and no health row at all.
//
// That case is the whole reason `Figure` exists. A concept whose connector
// has never run has no lag, no drift, no outbox depth and no dead letters --
// and rendering any of those as `0` says "we looked, and the answer is none",
// which is the opposite of the truth and reads as a healthy row. So an
// unmatched inventory row gets ABSENT figures, and the table draws an em
// dash. `figureFrom` produces exactly that for an absent key, so a measured
// zero -- a sweep that ran and found nothing -- still renders as `0` and
// still means what it says.

/** The declared half: one concept's data-origins record. */
export interface OriginDeclaration {
  conceptId: string;
  dataState: string;
  origin: string;
  mirroredTo: string[];
  connectors: string[];
}

/** One row of the table: a concept, a connector, and how that pairing is
 *  going -- or the fact that nothing has reported on it. */
export interface OriginRow {
  /** Stable across renders and unique in the table. */
  key: string;
  conceptId: string;
  dataState: string;
  origin: string;
  mirroredTo: string[];
  connector: string;
  /** "inbound" | "outbound" | "" when nothing has reported and the
   *  declaration implies none. */
  direction: string;
  /** False when NOTHING has ever reported on this pairing. Every figure
   *  below is absent in that case, and the difference matters: it is the
   *  reason the row exists at all. */
  hasHealth: boolean;
  backfillStatus: string;
  backfillCursor: string;
  lagSeconds: Figure;
  driftCount: Figure;
  outboxDepth: Figure;
  deadLetterCount: Figure;
  paused: boolean;
  lastError: string;
  lastReconcileAt: string;
}

export interface OriginJoin {
  rows: OriginRow[];
  /** How many concepts the inventory declared, in total. The Head says how
   *  many of these the table is about, because a table of 6 rows over a
   *  cluster of 400 concepts is otherwise read as the whole picture. */
  declared: number;
  /** Concepts with at least one connector -- what the table is about. */
  withConnector: number;
  /** Every connector the inventory names, sorted. The dead-letter band's
   *  fan-out is over exactly this set. */
  connectors: string[];
  /**
   * Health rows that matched no inventory entry.
   *
   * Surfaced rather than dropped. A health row for a (concept, connector)
   * the registry no longer declares is a connector still running against a
   * declaration that has moved -- exactly the state an operator wants told,
   * and one a silent inner join would erase.
   */
  unmatchedHealth: number;
}

export function declarationFromRow(raw: Row): OriginDeclaration {
  const row = flatten(raw);
  return {
    conceptId: stringOf(row, "conceptId"),
    dataState: stringOf(row, "dataState"),
    origin: stringOf(row, "origin"),
    mirroredTo: stringsOf(row, "mirroredTo"),
    connectors: stringsOf(row, "connectors"),
  };
}

/** The direction a declaration IMPLIES for one of its connectors, used only
 *  when nothing has reported. A mirror's connector fills it (inbound); an
 *  origin's connectors drain it (outbound). Anything else is left blank
 *  rather than guessed. */
export function impliedDirection(dataState: string, origin: string, connector: string): string {
  if (dataState === "mirror" && origin === connector) return "inbound";
  if (dataState === "origin") return "outbound";
  return "";
}

/**
 * The table.
 *
 * One row per concept x connector in the ordinary case. A concept CAN carry a
 * health row in each direction (`v1:platform:syncState.direction` says so), so
 * where two exist both are rendered -- collapsing them would drop a real
 * reading, and picking one would pick silently.
 */
export function joinOrigins(inventoryRows: readonly Row[], healthRows: readonly Row[]): OriginJoin {
  const declarations = inventoryRows.map(declarationFromRow).filter((d) => d.conceptId !== "");

  // Health, bucketed by the pairing it reports on.
  const health = new Map<string, Row[]>();
  for (const raw of healthRows) {
    const row = flatten(raw);
    const conceptId = stringOf(row, "conceptId");
    const connector = stringOf(row, "connector");
    if (conceptId === "" || connector === "") continue;
    const key = `${conceptId}|${connector}`;
    const list = health.get(key) ?? [];
    list.push(row);
    health.set(key, list);
  }

  const rows: OriginRow[] = [];
  const connectors = new Set<string>();
  const matched = new Set<string>();

  for (const declaration of declarations) {
    for (const connector of declaration.connectors) {
      connectors.add(connector);
      const key = `${declaration.conceptId}|${connector}`;
      const found = health.get(key) ?? [];
      if (found.length === 0) {
        rows.push(unmeasuredRow(declaration, connector));
        continue;
      }
      matched.add(key);
      for (const row of found) rows.push(measuredRow(declaration, connector, row));
    }
  }

  let unmatchedHealth = 0;
  for (const [key, list] of health.entries()) {
    if (!matched.has(key)) unmatchedHealth += list.length;
  }

  rows.sort((a, b) => a.key.localeCompare(b.key));

  return {
    rows,
    declared: declarations.length,
    withConnector: declarations.filter((d) => d.connectors.length > 0).length,
    connectors: [...connectors].sort((a, b) => a.localeCompare(b)),
    unmatchedHealth,
  };
}

function unmeasuredRow(declaration: OriginDeclaration, connector: string): OriginRow {
  return {
    key: `${declaration.conceptId}|${connector}|`,
    conceptId: declaration.conceptId,
    dataState: declaration.dataState,
    origin: declaration.origin,
    mirroredTo: declaration.mirroredTo,
    connector,
    direction: impliedDirection(declaration.dataState, declaration.origin, connector),
    hasHealth: false,
    backfillStatus: "",
    backfillCursor: "",
    // EVERY figure absent, and this is the line the whole module exists for.
    // Nothing has reported on this pairing, so there is no lag, no drift, no
    // depth and no dead letters -- there is no measurement.
    lagSeconds: absent("unmeasured"),
    driftCount: absent("unmeasured"),
    outboxDepth: absent("unmeasured"),
    deadLetterCount: absent("unmeasured"),
    paused: false,
    lastError: "",
    lastReconcileAt: "",
  };
}

function measuredRow(declaration: OriginDeclaration, connector: string, row: Row): OriginRow {
  const direction = stringOf(row, "direction");
  return {
    key: `${declaration.conceptId}|${connector}|${direction}`,
    conceptId: declaration.conceptId,
    dataState: declaration.dataState,
    origin: declaration.origin,
    mirroredTo: declaration.mirroredTo,
    connector,
    direction,
    hasHealth: true,
    backfillStatus: stringOf(row, "backfillStatus"),
    backfillCursor: stringOf(row, "backfillCursor"),
    // Per FIELD, not per row: a health row that reported a depth and never a
    // drift count has one measurement and one absence, and folding them into
    // "this row is measured" would print a zero for the half nobody wrote.
    lagSeconds: figureFrom(row, "lagSeconds"),
    driftCount: figureFrom(row, "driftCount"),
    outboxDepth: figureFrom(row, "outboxDepth"),
    deadLetterCount: figureFrom(row, "deadLetterCount"),
    paused: boolOr(row, "paused", false),
    lastError: stringOf(row, "lastError"),
    lastReconcileAt: stringOf(row, "lastReconcileAt"),
  };
}

/**
 * What a data state MEANS for what a caller may do, in one clause.
 *
 * A MIRROR IS THE ONE THAT CHANGES THE ANSWER. `executeWrite` refuses every
 * write to a mirror concept that does not come from the connector its
 * `@origin` names -- mutation, tool handler, raw insert, staged write -- and
 * neither row-authz escape applies: internal origin says the ENGINE is
 * writing when the question is whether the connector is, and a cluster
 * owner's edit is reverted by the next reconcile like anyone else's. That is
 * a harder rule than any tier on this page, so the row says it.
 */
export function dataStateSentence(state: string, origin: string, mirroredTo: readonly string[]): string {
  if (state === "mirror") {
    return `Read-only here. ${origin || "An external system"} owns this data, and the engine refuses every write to it that does not come from that connector -- a cluster owner's included.`;
  }
  if (state === "origin") {
    return mirroredTo.length === 0
      ? "MemQL owns this data and syncs it outward."
      : `MemQL owns this data and syncs it out to ${mirroredTo.join(", ")}.`;
  }
  if (state === "native") return "MemQL owns this data and nobody else holds a copy.";
  return "";
}

/** The acts this row's state offers. Legal-only, per DESIGN.md rule 12:
 *  Resume is not rendered on a running connector and Pause is not rendered
 *  on a paused one. */
export function originActs(row: Pick<OriginRow, "paused">): ("backfill" | "pause" | "resume")[] {
  return row.paused ? ["backfill", "resume"] : ["backfill", "pause"];
}

function stringOf(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}
