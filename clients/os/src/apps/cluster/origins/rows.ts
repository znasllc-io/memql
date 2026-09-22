import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { absent, figureFrom, type Figure } from "../../../kit/measure";
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
 * One connector, and how much of what it carries has reported.
 *
 * ===========================================================================
 * WHY A PER-CONNECTOR READING EXISTS AT ALL
 * ===========================================================================
 * The table reports per PAIRING and the Head reports one grand total. Neither
 * answers the question an operator actually arrives with, which is about the
 * CONNECTOR: is this thing working?
 *
 * The Shopify connector declares 65 concepts. When it could not read its own
 * store list (memql#5574) it enumerated nothing, reported on nothing, and the
 * table drew 65 rows of em dashes -- each one individually honest, and
 * together silent about the only thing worth saying. The issue's own words for
 * the production symptom were "the mirror stopped changing, with nothing
 * anywhere saying why".
 *
 * So this is the connector as the subject: how many of its pairings have
 * reported, how many are paused, how many are carrying an error.
 */
export interface ConnectorCoverage {
  connector: string;
  /** Pairings the declarations name for this connector. */
  concepts: number;
  /** Pairings something has actually reported on. A MEASURED count, not a
   *  Figure: the health rows are in hand and were counted, so zero here means
   *  "we looked and none matched" -- which is what a zero is for. */
  reported: number;
  /** Reported AND paused -- an operator's own decision, and the one cause of
   *  a quiet connector that is not a fault. */
  paused: number;
  /** Reported AND carrying a lastError. */
  withError: number;
}

/**
 * Every connector the declarations name, with its coverage, sorted by name.
 *
 * Derived from the join rather than from a third read: the two reads the page
 * already makes hold all of it, and a separate query would be a second answer
 * that could disagree with the table underneath it.
 */
export function connectorCoverage(rows: readonly OriginRow[]): ConnectorCoverage[] {
  const byConnector = new Map<string, ConnectorCoverage>();
  for (const row of rows) {
    if (row.connector === "") continue;
    const held = byConnector.get(row.connector) ?? {
      connector: row.connector,
      concepts: 0,
      reported: 0,
      paused: 0,
      withError: 0,
    };
    held.concepts += 1;
    if (row.hasHealth) {
      held.reported += 1;
      if (row.paused) held.paused += 1;
      if (row.lastError !== "") held.withError += 1;
    }
    byConnector.set(row.connector, held);
  }
  return [...byConnector.values()].sort((a, b) => a.connector.localeCompare(b.connector));
}

/**
 * What a connector's coverage says, in one clause -- or "" when the honest
 * answer is nothing and the line stays quiet.
 *
 * ===========================================================================
 * THE SENTENCE FOR A SILENT CONNECTOR NAMES TWO CAUSES AND PICKS NEITHER
 * ===========================================================================
 * "Nothing has reported" has two explanations and this page can tell them
 * apart in exactly no cases: a connector nobody has started yet, and a
 * connector that is running and enumerating nothing, produce the identical
 * absence here. memql#5574 was the second; a fresh cluster is the first.
 *
 * So the sentence states both and sends the reader to where the difference
 * actually lives, which is the connector's own configuration. Choosing one --
 * "this connector is broken", or the cheerier "not started yet" -- would be
 * inventing a fact out of an absence, and on a page whose entire design rests
 * on an em dash not being a zero that would be the one unforgivable line.
 */
export function coverageSentence(c: ConnectorCoverage): string {
  if (c.concepts === 0) return "";
  const noun = c.concepts === 1 ? "concept" : "concepts";
  if (c.reported === 0) {
    return `Nothing has reported on any of its ${c.concepts} ${noun}. A connector that has not run yet and one that is running and reading nothing look the same from here -- the difference is in the connector's own configuration.`;
  }
  if (c.reported < c.concepts) {
    return `${c.reported} of ${c.concepts} ${noun} have reported. The rest have no measurement, which is not the same as no lag.`;
  }
  if (c.paused === c.concepts) {
    return `Every concept is paused. Deliveries are staged and nothing is being applied.`;
  }
  if (c.paused > 0) {
    return `All ${c.concepts} ${noun} have reported; ${c.paused} ${c.paused === 1 ? "is" : "are"} paused.`;
  }
  return "";
}

/**
 * What a connector's coverage means for how its line reads: quiet when
 * everything has reported, attention when nothing has.
 *
 * A TONE, not a verdict. `silent` says only that a reading is missing, which
 * is what the page knows; nothing here claims a fault.
 */
export function coverageTone(c: ConnectorCoverage): "reporting" | "partial" | "silent" | "paused" {
  if (c.concepts === 0 || c.reported === 0) return "silent";
  if (c.paused === c.concepts) return "paused";
  if (c.reported < c.concepts) return "partial";
  return "reporting";
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
