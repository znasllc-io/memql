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
  /** The one store (partition) of a partitioned connector this row is about,
   *  or "" for a connector that keeps no partitions apart. */
  partition: string;
  /** The connection never asked for this domain: the engine's own sentence
   *  starts `connector: not requested`. Never an error on screen. */
  notRequested: boolean;
  /** "inbound" | "outbound" | "" when nothing has reported and the
   *  declaration implies none. */
  direction: string;
  /** False when NOTHING has ever reported on this pairing. Every figure
   *  below is absent in that case, and the difference matters: it is the
   *  reason the row exists at all. */
  hasHealth: boolean;
  backfillStatus: string;
  /** The row's backfill status is `failed`. Its cursor may lead nowhere (a
   *  download link that expired), so "Backfill now" starts it again from the
   *  start rather than from the cursor. */
  backfillFailed: boolean;
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
  // The stores of every connector that keeps them apart. A connector named
  // here has written at least one row with a partition, and from then on its
  // rows with none are history: what it wrote before it kept its stores apart.
  const partitions = new Map<string, Set<string>>();
  for (const raw of healthRows) {
    const row = flatten(raw);
    const conceptId = stringOf(row, "conceptId");
    const connector = stringOf(row, "connector");
    if (conceptId === "" || connector === "") continue;
    const key = `${conceptId}|${connector}`;
    const list = health.get(key) ?? [];
    list.push(row);
    health.set(key, list);
    const partition = stringOf(row, "partitionKey");
    if (partition !== "") {
      const held = partitions.get(connector) ?? new Set<string>();
      held.add(partition);
      partitions.set(connector, held);
    }
  }
  const isHistory = (row: Row): boolean =>
    stringOf(row, "partitionKey") === "" && partitions.has(stringOf(row, "connector"));

  const rows: OriginRow[] = [];
  const connectors = new Set<string>();
  const matched = new Set<string>();

  for (const declaration of declarations) {
    for (const connector of declaration.connectors) {
      connectors.add(connector);
      const key = `${declaration.conceptId}|${connector}`;
      const found = (health.get(key) ?? []).filter((row) => !isHistory(row));
      matched.add(key);
      const stores = partitions.get(connector);
      if (stores === undefined) {
        if (found.length === 0) rows.push(unmeasuredRow(declaration, connector, ""));
        for (const row of found) rows.push(measuredRow(declaration, connector, row));
        continue;
      }
      // A partitioned connector: every store it serves is its own pairing,
      // and a store nothing has reported on for this domain is unmeasured
      // there -- not hidden because another store reported.
      for (const store of stores) {
        const own = found.filter((row) => stringOf(row, "partitionKey") === store);
        if (own.length === 0) rows.push(unmeasuredRow(declaration, connector, store));
        for (const row of own) rows.push(measuredRow(declaration, connector, row));
      }
    }
  }

  let unmatchedHealth = 0;
  for (const [key, list] of health.entries()) {
    if (!matched.has(key)) unmatchedHealth += list.filter((row) => !isHistory(row)).length;
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

function unmeasuredRow(declaration: OriginDeclaration, connector: string, partition: string): OriginRow {
  return {
    key: `${declaration.conceptId}|${connector}|${partition}|`,
    conceptId: declaration.conceptId,
    dataState: declaration.dataState,
    origin: declaration.origin,
    mirroredTo: declaration.mirroredTo,
    connector,
    partition,
    notRequested: false,
    direction: impliedDirection(declaration.dataState, declaration.origin, connector),
    hasHealth: false,
    backfillStatus: "",
    backfillFailed: false,
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
  const partition = stringOf(row, "partitionKey");
  const lastError = stringOf(row, "lastError");
  return {
    key: `${declaration.conceptId}|${connector}|${partition}|${direction}`,
    conceptId: declaration.conceptId,
    dataState: declaration.dataState,
    origin: declaration.origin,
    mirroredTo: declaration.mirroredTo,
    connector,
    partition,
    notRequested: isNotRequested(lastError),
    direction,
    hasHealth: true,
    backfillStatus: stringOf(row, "backfillStatus"),
    backfillFailed: stringOf(row, "backfillStatus") === "failed",
    backfillCursor: stringOf(row, "backfillCursor"),
    // Per FIELD, not per row: a health row that reported a depth and never a
    // drift count has one measurement and one absence, and folding them into
    // "this row is measured" would print a zero for the half nobody wrote.
    lagSeconds: figureFrom(row, "lagSeconds"),
    driftCount: figureFrom(row, "driftCount"),
    outboxDepth: figureFrom(row, "outboxDepth"),
    deadLetterCount: figureFrom(row, "deadLetterCount"),
    paused: boolOr(row, "paused", false),
    lastError,
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
  /** The stores of a connector that keeps them apart; 0 for one that does
   *  not. Its sentences then name the stores, so a row per domain per store
   *  is not read as that many concepts. */
  stores: number;
  /** Rows the connection never asked for. Counted in nothing below: only
   *  kept so a connector that reaches nothing is not read as reporting. */
  outside: number;
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
  const stores = new Map<string, Set<string>>();
  for (const row of rows) {
    if (row.connector === "") continue;
    const held = byConnector.get(row.connector) ?? {
      connector: row.connector,
      stores: 0,
      outside: 0,
      concepts: 0,
      reported: 0,
      paused: 0,
      withError: 0,
    };
    byConnector.set(row.connector, held);
    if (row.partition !== "") {
      const own = stores.get(row.connector) ?? new Set<string>();
      own.add(row.partition);
      stores.set(row.connector, own);
      held.stores = own.size;
    }
    // What the connection never asked for is outside it: not a concept it
    // carries, not reported, not paused and not a failure of it.
    if (row.notRequested) {
      held.outside += 1;
      continue;
    }
    held.concepts += 1;
    if (row.hasHealth) {
      held.reported += 1;
      if (row.paused) held.paused += 1;
      if (row.lastError !== "") held.withError += 1;
    }
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
  // " of its 2 stores" for a connector that keeps its stores apart, and
  // nothing for one that does not, whose sentences stay as they always were.
  const ofStores = c.stores === 0 ? "" : c.stores === 1 ? " of its store" : ` of its ${c.stores} stores`;
  if (c.concepts === 0) {
    if (c.outside === 0) return "";
    return c.stores === 0
      ? "None of its concepts is part of its connection."
      : `None of its concepts is part of the connection${ofStores}.`;
  }
  const noun = c.concepts === 1 ? "concept" : "concepts";
  if (c.reported === 0) {
    const what = c.stores === 0 ? `any of its ${c.concepts} ${noun}` : `any of the ${c.concepts} ${noun}${ofStores}`;
    return `Nothing has reported on ${what}. A connector that has not run yet and one that is running and reading nothing look the same from here -- the difference is in the connector's own configuration.`;
  }
  if (c.reported < c.concepts) {
    return `${c.reported} of ${c.concepts} ${noun}${ofStores} have reported. The rest have no measurement, which is not the same as no lag.`;
  }
  if (c.paused === c.concepts) {
    return `Every concept${ofStores} is paused. Deliveries are staged and nothing is being applied.`;
  }
  if (c.paused > 0) {
    return `All ${c.concepts} ${noun}${ofStores} have reported; ${c.paused} ${c.paused === 1 ? "is" : "are"} paused.`;
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

/** The engine's own sentence for a domain the connection never asked for. */
const NOT_REQUESTED = "connector: not requested";

export function isNotRequested(lastError: string): boolean {
  return lastError.startsWith(NOT_REQUESTED);
}

/** One store's domains: those its connection reaches, and -- apart, because
 *  neither is a fault -- those it never asked for. */
export interface PartitionGroup {
  /** The connector a store's group belongs to; "" for the shared group of
   *  connectors that keep no stores apart. */
  connector: string;
  /** The store, or "" for the rows of connectors that keep no stores apart. */
  partition: string;
  rows: OriginRow[];
  outside: OriginRow[];
}

/**
 * The table, by store. The group of connectors that keep no stores apart
 * (partition "") comes first and is drawn as the table always was; each store
 * follows, by name. Rows keep the order they arrive in.
 *
 * History -- a partitioned connector's rows from before it kept its stores
 * apart -- never reaches here: `joinOrigins` leaves it out, so the coverage
 * above the table counts exactly what the table draws.
 */
export function groupByPartition(rows: readonly OriginRow[]): PartitionGroup[] {
  // Keyed by connector AND store: two connectors that happen to name a store
  // alike never share a group or its closed line.
  const groups = new Map<string, PartitionGroup>();
  for (const row of rows) {
    const connector = row.partition === "" ? "" : row.connector;
    const key = `${connector}|${row.partition}`;
    const group = groups.get(key) ?? { connector, partition: row.partition, rows: [], outside: [] };
    (row.notRequested ? group.outside : group.rows).push(row);
    groups.set(key, group);
  }
  return [...groups.values()].sort((a, b) => a.partition.localeCompare(b.partition) || a.connector.localeCompare(b.connector));
}

/** One inbound source's deliveries that used their four tries. */
export interface GivenUpSource {
  source: string;
  count: number;
  /** The reason on the source's most recently received delivery, as the engine wrote it. */
  lastError: string;
}

/**
 * `inboundRequestsGivenUp` by source, in the order the sources first appear.
 * The query answers the most recently received delivery first, so a source's
 * first row carries the reason on its most recently received delivery, and
 * the source received from most recently leads.
 */
export function givenUpBySource(rows: readonly Row[]): GivenUpSource[] {
  const bySource = new Map<string, GivenUpSource>();
  for (const raw of rows) {
    const row = flatten(raw);
    const source = stringOf(row, "source");
    if (source === "") continue;
    const held = bySource.get(source);
    if (held === undefined) bySource.set(source, { source, count: 1, lastError: stringOf(row, "lastError") });
    else held.count += 1;
  }
  return [...bySource.values()];
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
