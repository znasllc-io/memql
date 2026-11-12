import { renderMemQLValue, type Row } from "@znasllc-io/memql-sdk-core/client";

import { formatBytes } from "../../kit/format";
import { flatten } from "../../kit/rows";
import { downloadFromPath } from "../files/actions/download";
import { rowsPhrase } from "./inventory";
import { kindName } from "./moves";

// An account's exports: whether it may be exported, the exports it has, and
// the words for each (design record 2026-11-12-account-export, sections F, G
// and K).
//
// PURE READING AND WRITING OF SEVEN BUILTINS' CALLS AND ROWS, all carrying
// `@requiresCapability("execute", "app:accounts/export")`. The page draws none
// of this for anybody who does not hold it.
//
// NO RAW CODE REACHES THE SCREEN. A failure code, a refusal code and a phase
// each have their own words here, and one this client does not know gets one
// plain sentence rather than itself. The engine's own detail is shown beneath
// the sentence, as it wrote it.

export type ExportState = "draft" | "queued" | "running" | "complete" | "failed" | "cancelled";

export interface ExportRow {
  id: string;
  accountId: string;
  accountName: string;
  requestedBy: string;
  /** The person's name, when the answer carries it. Their id is never shown. */
  requestedByName: string;
  requestedAt: string;
  state: string;
  phase: string;
  protected: boolean;
  attempt: number;
  /**
   * A cancel was asked of a running export. The engine answers that cancel
   * with the run still `running`; the runner writes `cancelled` at its next
   * heartbeat. Until then the export reads "Cancelling" and Cancel is not
   * offered again.
   */
  cancelRequested: boolean;
  startedAt: string;
  finishedAt: string;
  kindsTotal: number;
  kindsDone: number;
  rowsWritten: number;
  versionsWritten: number;
  bytesTotal: number;
  bytesDone: number;
  fileName: string;
  sizeBytes: number;
  sha256: string;
  /** The file's report, parsed; null when the answer carried none. */
  report: ExportReport | null;
  failureCode: string;
  failureDetail: string;
  removedAt: string;
  removedBy: string;
  removedByName: string;
}

/** What the report says that a person should know before they rely on the file. */
export interface ExportReport {
  /** Objects a carried row named that storage no longer had. */
  missingObjects: number;
  /** References outside the place a kind's declaration gives its bytes: recorded as refused, never read. */
  refused: number;
  /** Bytes counted but not carried: older versions, package snapshots, attachments. */
  notCarriedObjects: number;
  notCarriedBytes: number;
  /** Of the objects counted, not carried: those whose size storage did not give. */
  notCarriedUnknownSize: number;
  /** Secret values removed, as `kind.field`. */
  scrubbed: string[];
}

export interface ExportCondition {
  name: string;
  met: boolean;
  held: boolean;
  kindsTotal: number;
  kindsFinished: number;
  waiting: number;
  concepts: string[];
  rows: number;
  tasks: number;
  /** Kinds with a full pass that was asked for and has not finished (after a move, a placement or a re-tag). */
  pendingPasses: number;
}

export interface ExportReadiness {
  conditions: ExportCondition[];
  /** Rows of this cluster in no account. */
  unplaced: number;
  ready: boolean;
}

export interface ExportsPage {
  exports: ExportRow[];
  nextCursor: string;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function count(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    const n = Number(value);
    if (Number.isFinite(n)) return n;
  }
  return 0;
}

function bool(value: unknown): boolean {
  return value === true || value === "true";
}

function strings(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
}

/** The report, from an object or the JSON text it may travel as. */
export function reportFrom(value: unknown): ExportReport | null {
  let raw: unknown = value;
  if (typeof raw === "string") {
    if (raw.trim() === "") return null;
    try {
      raw = JSON.parse(raw);
    } catch {
      return null;
    }
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const r = raw as Record<string, unknown>;
  const bytes = (r["bytes"] && typeof r["bytes"] === "object" ? r["bytes"] : {}) as Record<string, unknown>;
  const scrubbed = Array.isArray(r["scrubbedFields"])
    ? (r["scrubbedFields"] as unknown[]).flatMap((f) => {
        if (f === null || typeof f !== "object") return [];
        const field = f as Record<string, unknown>;
        return [`${kindName(text(field["concept"]))}.${text(field["field"])}`];
      })
    : [];
  return {
    missingObjects: count(bytes["missing"]),
    refused: count(bytes["refused"]),
    notCarriedObjects: count(bytes["notCarriedObjects"]),
    notCarriedBytes: count(bytes["notCarriedBytes"]),
    notCarriedUnknownSize: count(bytes["notCarriedUnknownSize"]),
    scrubbed,
  };
}

export function exportFromRow(raw: Row): ExportRow {
  const row = flatten(raw);
  return {
    id: text(row["id"]),
    accountId: text(row["accountId"]),
    accountName: text(row["accountName"]),
    requestedBy: text(row["requestedBy"]),
    requestedByName: text(row["requestedByName"]),
    requestedAt: text(row["requestedAt"]),
    state: text(row["state"]),
    phase: text(row["phase"]),
    protected: bool(row["protected"]),
    attempt: count(row["attempt"]),
    cancelRequested: bool(row["cancelRequested"]),
    startedAt: text(row["startedAt"]),
    finishedAt: text(row["finishedAt"]),
    kindsTotal: count(row["kindsTotal"]),
    kindsDone: count(row["kindsDone"]),
    rowsWritten: count(row["rowsWritten"]),
    versionsWritten: count(row["versionsWritten"]),
    bytesTotal: count(row["bytesTotal"]),
    bytesDone: count(row["bytesDone"]),
    fileName: text(row["fileName"]),
    sizeBytes: count(row["sizeBytes"]),
    sha256: text(row["sha256"]),
    report: reportFrom(row["report"]),
    failureCode: text(row["failureCode"]),
    failureDetail: text(row["failureDetail"]),
    removedAt: text(row["removedAt"]),
    removedBy: text(row["removedBy"]),
    removedByName: text(row["removedByName"]),
  };
}

/** The one export row an act answers. Throws when the answer carried none. */
export function oneExport(rows: readonly Row[]): ExportRow {
  for (const raw of rows) {
    if (text(flatten(raw)["kind"]) === "export") return exportFromRow(raw);
  }
  throw new Error("The cluster answered without the export.");
}

/**
 * The prepared export, and its passphrase when it is protected.
 *
 * THE PASSPHRASE IS RETURNED, NEVER KEPT HERE: the caller holds it in its own
 * state for one step and drops it. It is read off this one answer only.
 */
export function preparedFromRows(rows: readonly Row[]): { export: ExportRow; passphrase: string } {
  for (const raw of rows) {
    const row = flatten(raw);
    if (text(row["kind"]) === "export") return { export: exportFromRow(raw), passphrase: text(row["passphrase"]) };
  }
  throw new Error("The cluster answered without the export.");
}

export function exportsPageFromRows(rows: readonly Row[]): ExportsPage {
  const page: ExportsPage = { exports: [], nextCursor: "" };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "export":
        page.exports.push(exportFromRow(raw));
        break;
      case "page":
        page.nextCursor = text(row["nextCursor"]);
        break;
    }
  }
  return page;
}

export function readinessFromRows(rows: readonly Row[]): ExportReadiness {
  const out: ExportReadiness = { conditions: [], unplaced: 0, ready: false };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "condition":
        out.conditions.push({
          name: text(row["name"]),
          met: bool(row["met"]),
          held: bool(row["held"]),
          kindsTotal: count(row["kindsTotal"]),
          kindsFinished: count(row["kindsFinished"]),
          waiting: count(row["waiting"]),
          concepts: strings(row["concepts"]),
          rows: count(row["rows"]),
          tasks: count(row["tasks"]),
          pendingPasses: count(row["pendingPasses"]),
        });
        break;
      case "unplaced":
        out.unplaced = count(row["rows"]);
        break;
      case "ready":
        out.ready = bool(row["ready"]);
        break;
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

/** The conditions in the order the page names them, with their labels. */
export const CONDITION_ORDER = ["recording", "assigned", "declared", "decided", "followed"] as const;

const CONDITION_LABEL: Record<string, string> = {
  recording: "Accounts recorded on rows",
  assigned: "Existing rows assigned",
  declared: "Every kind says whose it is",
  decided: "Rows awaiting a decision",
  followed: "Rows following a moved parent",
};

export function conditionLabel(name: string): string {
  return CONDITION_LABEL[name] ?? "Another condition";
}

/** A condition's state in words, with its count where it has one. */
export function conditionState(c: ExportCondition): string {
  switch (c.name) {
    case "recording":
      return c.met ? "Recorded" : c.held ? "Held by an operator" : "Not yet";
    case "assigned": {
      if (c.met) return "Assigned";
      const kinds = `${c.kindsFinished.toLocaleString("en-US")} of ${c.kindsTotal.toLocaleString("en-US")} kinds finished`;
      const counted = c.waiting > 0 ? `${kinds}, ${rowsPhrase(c.waiting)} waiting` : kinds;
      // A pass that was asked for holds the condition back even once every
      // kind finished its first pass; without this the count reads as met.
      if (c.pendingPasses <= 0) return counted;
      return `${counted}. ${c.pendingPasses === 1 ? "1 kind is" : `${c.pendingPasses.toLocaleString("en-US")} kinds are`} finishing a pass that was asked for.`;
    }
    case "declared":
      if (c.met) return "Yes";
      // Unmet with no kind named: the engine could not read the declarations,
      // which says nothing about the kinds themselves.
      return c.concepts.length === 0
        ? "Could not be read"
        : `${c.concepts.length === 1 ? "1 kind does not" : `${c.concepts.length} kinds do not`}: ${c.concepts.map(kindName).join(", ")}`;
    case "decided":
      return c.met ? "None" : `${rowsPhrase(c.rows)} awaiting`;
    case "followed":
      return c.met ? "None" : `${c.tasks.toLocaleString("en-US")} still following`;
    default:
      return c.met ? "Met" : "Not met";
  }
}

export function exportIsRemoved(x: ExportRow): boolean {
  return x.removedAt.trim() !== "";
}

/** Prepared, waiting or running: the cluster has not finished with it. */
export function exportIsActive(x: ExportRow): boolean {
  return x.state === "draft" || x.state === "queued" || x.state === "running";
}

export function exportStateWord(x: ExportRow): string {
  if (exportIsRemoved(x)) return "Removed";
  switch (x.state) {
    case "draft":
      return "Prepared";
    case "queued":
      return "Waiting to start";
    case "running":
      return x.cancelRequested ? "Cancelling" : "Running";
    case "complete":
      return "Complete";
    case "failed":
      return "Failed";
    case "cancelled":
      return "Cancelled";
    default:
      return "Not known";
  }
}

const PHASE_WORD: Record<string, string> = {
  checking: "Checking",
  rows: "Writing rows",
  bytes: "Copying files",
  sealing: "Sealing",
  verifying: "Verifying",
};

/**
 * "Writing rows: 1,204 rows, 3 of 9 kinds", or the phase alone; "" for a
 * prepared export, which has no progress, and for a queued export, whose state word ("Waiting to start") already says all that
 * is known. The engine does not say whether another export holds the
 * installation, so nothing here claims it.
 */
export function exportProgress(x: ExportRow): string {
  if (x.state === "queued" || x.state === "draft") return "";
  const phase = PHASE_WORD[x.phase] ?? "Starting";
  switch (x.phase) {
    case "rows":
      return `${phase}: ${rowsPhrase(x.rowsWritten)}, ${x.kindsDone.toLocaleString("en-US")} of ${x.kindsTotal.toLocaleString("en-US")} kinds`;
    case "bytes":
      return `${phase}: ${formatBytes(x.bytesDone)} of ${formatBytes(x.bytesTotal)}`;
    default:
      return phase;
  }
}

/** Why a run stopped, in a sentence. The engine's detail is shown beneath it. */
export function failureSentence(code: string): string {
  switch (code) {
    case "export_not_ready":
      return "The account was not ready to export.";
    case "export_rows_waiting":
      return "Rows were still being assigned to accounts, and the export stopped waiting for them.";
    case "export_census_mismatch":
      return "The rows written did not match the rows counted, so the file was not kept.";
    case "export_verify_failed":
      return "The file did not read back as it was written, so it was not kept.";
    case "export_rows_too_slow":
      return "Writing the rows took longer than an hour, so the export stopped.";
    case "export_failed":
      return "The export failed.";
    default:
      return "The export did not finish.";
  }
}

/**
 * A refusal from an act, as a sentence and the engine's own words.
 *
 * Refusals arrive as an error whose text begins with a code
 * (`export_in_progress: ...`). The code becomes a sentence; what follows it is
 * the engine's detail. A message with no code is the engine's words as they
 * are.
 */
export function refusalOf(err: unknown): { code: string; sentence: string; detail: string } {
  const message = err instanceof Error ? err.message : String(err);
  const match = /^\s*([a-z][a-z0-9_]*)\s*[:\-]\s*([\s\S]*)$/.exec(message);
  const code = match?.[1] ?? "";
  const rest = (match?.[2] ?? "").trim();
  // The code is for the caller to decide by; it is never drawn.
  switch (code) {
    case "export_not_ready":
      return { code, sentence: "This account is not ready to export.", detail: rest };
    case "export_in_progress":
      return { code, sentence: "Another export of this account is already prepared or running.", detail: rest };
    case "person_session_required":
      return { code, sentence: "Only a person signed in to this cluster can export an account.", detail: rest };
    case "export_draft_expired":
      return { code, sentence: "The prepared export was removed because it was not started in time.", detail: rest };
    default:
      // No code of ours: the whole message is the engine's words, unless it
      // is a code we do not know, which is never shown as itself.
      return match && /_/.test(code)
        ? { code, sentence: "The cluster refused.", detail: rest }
        : { code: "", sentence: "The cluster refused.", detail: message };
  }
}

/**
 * What a file leaves out, in the design record's words (section B, "What is
 * left out"). Said on the Contents step, before anything is written.
 */
export const LEFT_OUT: readonly { what: string; why: string }[] = [
  { what: "Sign-in material and visits", why: "They stay with this cluster." },
  { what: "Work in transit", why: "Uploads, outgoing messages and sends under way are not carried." },
  { what: "What is bound to a machine", why: "Workspaces and watched folders stay with the machine." },
  { what: "The installation's own rows", why: "They belong to the cluster; the file lists the ones it points at." },
  { what: "Secret values", why: "Their names are listed; the values never travel." },
  { what: "Attachments' and older bundles' bytes", why: "Counted, not carried." },
];

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------
// The SDK's generated methods build this same text and send it through
// `executeNamed`. The hooks call `executeNamed` with it directly so every
// account call goes through one path, the one the tests fake and whose sent
// text they check.

/** The default and the most `accountExports` answers in one page. */
export const EXPORTS_PAGE = 20;

export function readinessCall(accountId: string): string {
  return `builtin accountExportReadiness(accountId: ${renderMemQLValue(accountId)})`;
}

export function prepareCall(accountId: string, protect: boolean): string {
  return `builtin accountExportPrepare(accountId: ${renderMemQLValue(accountId)}, protect: ${protect ? "true" : "false"})`;
}

/** Start, cancel, remove and status: each takes the export's id alone. */
export function exportActCall(name: "accountExportStart" | "accountExportCancel" | "accountExportRemove" | "accountExportStatus", exportId: string): string {
  return `builtin ${name}(exportId: ${renderMemQLValue(exportId)})`;
}

export function exportsCall(accountId: string, after: string): string {
  const parts = [`accountId: ${renderMemQLValue(accountId)}`, `limit: ${EXPORTS_PAGE}`];
  if (after !== "") parts.push(`after: ${renderMemQLValue(after)}`);
  return `builtin accountExports(${parts.join(", ")})`;
}

// ---------------------------------------------------------------------------
// The download (section G)
// ---------------------------------------------------------------------------

/** The edge's same-origin marker for an export's content route. */
export function accountExportContentPath(baseUrl: string, exportId: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return `${mount}_memql/accounts/exports/${encodeURIComponent(exportId)}/content`;
}

/**
 * Save a complete export's file, through the Files app's download path: the
 * worker when this browser has one, the buffered path below its limit
 * otherwise. The page fetches with its bearer; nothing else carries it.
 */
export function downloadAccountExport(x: ExportRow, bearer: () => Promise<string | null>): Promise<void> {
  return downloadFromPath({
    contentPath: accountExportContentPath(import.meta.env.BASE_URL, x.id),
    fileName: x.fileName || `${x.accountName || "account"}.memql-account`,
    sizeBytes: x.sizeBytes,
    bearer,
  });
}

/** "4.2 MiB", or nothing before the file exists. */
export function exportSize(x: ExportRow): string {
  return x.sizeBytes > 0 ? formatBytes(x.sizeBytes) : "";
}
