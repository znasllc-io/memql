import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";

// What an account holds, and how far the assignment of existing rows has got
// (account row ownership, section H).
//
// PURE READING OF TWO BUILTINS' ROWS. `accountInventory` and
// `accountAssignmentStatus` answer with tagged rows (`kind`), and everything
// here turns those into what the Accounts page draws. Nothing here decides who
// may see it: both builtins carry `@requiresCapability("execute",
// "app:accounts/inventory")`, and the page only draws them for a holder.

/** One concept that holds rows for the account. */
export interface InventoryConcept {
  /** The concept id, `v1:notes:note`. */
  concept: string;
  namespace: string;
  /** The concept's own name, `note`. */
  name: string;
  rows: number;
  versions: number;
}

export interface Inventory {
  concepts: InventoryConcept[];
  /** Bytes of the account's current files. Zero when it has none. */
  fileBytes: number;
}

/** One line of the page: the concepts of one area, added up. */
export interface InventoryArea {
  key: string;
  label: string;
  rows: number;
  /** Set on the Files area only: what the account's current files weigh. */
  bytes: number | null;
  concepts: InventoryConcept[];
}

/**
 * Which area a namespace reads as, in the words of the app a person knows it
 * from. A namespace with no entry shows under its own name, capitalised, so a
 * pack's concepts are never dropped from the count.
 */
const AREA_OF_NAMESPACE: Readonly<Record<string, string>> = {
  campaigns: "Email",
  platform: "Deployables",
  library: "Files",
  notes: "Notes",
  todos: "To-dos",
  calendar: "Calendar",
  work: "Work",
  shopify: "Store",
  commerce: "Store",
  knowledge: "Knowledge",
  compose: "Materializer",
  pipelines: "Pipelines",
  identity: "People",
  skills: "Agents",
  agents: "Agents",
};

const FILES_AREA = "Files";

export function areaLabel(namespace: string): string {
  const known = AREA_OF_NAMESPACE[namespace];
  if (known !== undefined) return known;
  const name = namespace.trim();
  return name === "" ? "Other" : name.charAt(0).toUpperCase() + name.slice(1);
}

/** A count off the wire: a number, or the string an int64 travels as. */
function count(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    const n = Number(value);
    if (Number.isFinite(n)) return n;
  }
  return 0;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function bool(value: unknown): boolean {
  return value === true || value === "true";
}

export function inventoryFromRows(rows: readonly Row[]): Inventory {
  const concepts: InventoryConcept[] = [];
  let fileBytes = 0;
  for (const raw of rows) {
    const row = flatten(raw);
    const kind = text(row["kind"]);
    if (kind === "files") {
      fileBytes += count(row["bytes"]);
    } else if (kind === "concept") {
      const concept = text(row["concept"]);
      if (concept === "") continue;
      concepts.push({
        concept,
        namespace: text(row["namespace"]),
        name: text(row["name"]) || concept,
        rows: count(row["rows"]),
        versions: count(row["versions"]),
      });
    }
  }
  return { concepts, fileBytes };
}

/** Concepts grouped into areas, ordered by the area's label. */
export function inventoryAreas(inventory: Inventory): InventoryArea[] {
  const byLabel = new Map<string, InventoryArea>();
  const areaFor = (label: string): InventoryArea => {
    let area = byLabel.get(label);
    if (area === undefined) {
      area = { key: label, label, rows: 0, bytes: null, concepts: [] };
      byLabel.set(label, area);
    }
    return area;
  };
  for (const concept of inventory.concepts) {
    const area = areaFor(areaLabel(concept.namespace));
    area.rows += concept.rows;
    area.concepts.push(concept);
  }
  // Bytes belong to Files. A Files area exists whenever the library holds a
  // row; one is drawn for bytes alone only so that bytes are never dropped.
  if (inventory.fileBytes > 0 || byLabel.has(FILES_AREA)) {
    areaFor(FILES_AREA).bytes = inventory.fileBytes;
  }
  return [...byLabel.values()].sort((a, b) => a.label.localeCompare(b.label));
}

export function inventoryIsEmpty(inventory: Inventory): boolean {
  return inventory.concepts.every((c) => c.rows === 0) && inventory.fileBytes === 0;
}

/** "1 row", "1,204 rows". */
export function rowsPhrase(rows: number): string {
  return rows === 1 ? "1 row" : `${rows.toLocaleString("en-US")} rows`;
}

// ---------------------------------------------------------------------------
// The assignment of existing rows
// ---------------------------------------------------------------------------

export interface AssignmentProgress {
  concept: string;
  assigned: number;
  kept: number;
  shared: number;
  conflicts: number;
  passStartedAt: string;
  passFinishedAt: string;
  verifiedThrough: string;
}

export interface AssignmentConflict {
  concept: string;
  rowId: string;
  conflict: string;
  detail: string;
  foundAt: string;
}

export interface AssignmentStatus {
  /** Whether accounts are recorded on rows on this cluster. */
  enabled: boolean;
  /** An operator holds the switch off (`MEMQL_ACCOUNT_STAMPING_ACTIVATE=false`). */
  held: boolean;
  progress: AssignmentProgress[];
  /** Rows per account, bare account id. */
  accounts: { accountId: string; rows: number }[];
  conflicts: AssignmentConflict[];
}

export function assignmentStatusFromRows(rows: readonly Row[]): AssignmentStatus {
  const status: AssignmentStatus = { enabled: false, held: false, progress: [], accounts: [], conflicts: [] };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "switch":
        status.enabled = bool(row["enabled"]);
        status.held = bool(row["held"]);
        break;
      case "progress":
        status.progress.push({
          concept: text(row["concept"]),
          assigned: count(row["assigned"]),
          kept: count(row["kept"]),
          shared: count(row["shared"]),
          conflicts: count(row["conflicts"]),
          passStartedAt: text(row["passStartedAt"]),
          passFinishedAt: text(row["passFinishedAt"]),
          verifiedThrough: text(row["verifiedThrough"]),
        });
        break;
      case "account":
        status.accounts.push({ accountId: text(row["accountId"]), rows: count(row["rows"]) });
        break;
      case "conflict":
        status.conflicts.push({
          concept: text(row["concept"]),
          rowId: text(row["rowId"]),
          conflict: text(row["conflict"]),
          detail: text(row["detail"]),
          foundAt: text(row["foundAt"]),
        });
        break;
    }
  }
  return status;
}

/**
 * Which one thing the panel says.
 *
 * HELD WINS over everything: a held node neither stamps nor assigns even when
 * another replica has turned the switch on, so a progress report would describe
 * work this cluster is not doing. WAITING is the switch off and not held: the
 * handshake is waiting for a node of the previous release to leave. A concept
 * is finished once its pass has a finish time; the job keeps running after
 * that to catch rows written in the meantime, which is not "running" to a
 * person reading the page.
 */
export type AssignmentPhase = "held" | "waiting" | "running" | "finished";

export function assignmentPhase(status: AssignmentStatus): AssignmentPhase {
  if (status.held) return "held";
  if (!status.enabled) return "waiting";
  if (status.progress.length === 0) return "running";
  return status.progress.every(conceptFinished) ? "finished" : "running";
}

export function conceptFinished(progress: AssignmentProgress): boolean {
  return progress.passFinishedAt.trim() !== "";
}

/** The engine's code for the documented limit: an account too large to count in one read. */
export const INVENTORY_TOO_LARGE = "account_inventory_too_large";

/**
 * The engine's sentence when a failed inventory read is the documented
 * "too large to count" limit, and null for any other failure.
 *
 * KEYED ON THE CODE, NEVER ON THE SENTENCE: the engine answers this limit as
 * `account_inventory_too_large: <sentence>`, and the sentence is its to word.
 * The limit is a fact about the account, not a fault, so it is shown as a
 * plain note rather than an error.
 */
export function inventoryLimitNote(error: string): string | null {
  const match = new RegExp(`(?:^|\\s)${INVENTORY_TOO_LARGE}\\s*:\\s*([\\s\\S]*)$`).exec(error);
  if (match === null) return null;
  return match[1]!.trim() || "This account holds too many rows to count here.";
}
