import { renderMemQLValue, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";

// The rows of a kind, moving them between accounts, and the rows that await a
// decision or sit in no account (design record 2026-11-12-account-export,
// sections I and K).
//
// PURE READING AND WRITING OF FOUR BUILTINS' CALLS AND ROWS: `accountRows`,
// `accountUnplaced`, `accountRowsMove` and `accountConflictLeaveOut`. Each
// answers tagged rows (`kind`), as the inventory does. Nothing here decides
// who may call them: all four carry `@requiresCapability("execute",
// "app:accounts/move")`, and the page draws none of this for anybody else.

/** One row of a kind, as `accountRows` answers it. */
export interface KindRow {
  id: string;
  concept: string;
  /** The values of the fields the kind's `@displayCard` names; the id when it names none. */
  primary: string;
  secondary: string;
  tertiary: string;
  status: string;
  ownerUserId: string;
  createdAt: string;
  updatedAt: string;
  /** The row moves when its parent moves, and cannot be chosen on its own. */
  followsParent: boolean;
  /** The kind it follows, as a concept id; empty when the answer does not say. */
  parentConcept: string;
}

/** One page of a kind's rows. `nextCursor` is empty on the last page. */
export interface KindRowsPage {
  rows: KindRow[];
  nextCursor: string;
}

/** The answer of one move, or of its preview. */
export interface MovePlan {
  /** Rows that move (or would), per kind. */
  moves: { concept: string; rows: number }[];
  /** Rows that follow them, per kind. */
  /**
   * The kinds whose rows follow them, NAMED, NOT COUNTED: a background pass
   * carries what hangs off a moved row, so the engine names the kinds and does
   * not know how many rows will follow. A `rows` field, should one ever
   * arrive, is not read.
   */
  follows: { concept: string }[];
  /** Each refused row, with its reason. */
  refused: { concept: string; id: string; reason: string }[];
  /** The totals the engine counted, or null when it sent none. */
  summary: { moved: number; followerKinds: number; refused: number } | null;
}

/** A kind with rows that belong to no account. */
export interface UnplacedKind {
  concept: string;
  namespace: string;
  name: string;
  rows: number;
}

/** A row no rule could place, awaiting somebody's decision. */
export interface OpenConflict {
  concept: string;
  rowId: string;
  conflict: string;
  detail: string;
  /** Bare account ids the engine suggests, first offered when placing it. */
  candidates: string[];
  foundAt: string;
}

export interface Unplaced {
  kinds: UnplacedKind[];
  conflicts: OpenConflict[];
}

/** What one move names: a row of a kind. */
export interface RowRef {
  concept: string;
  id: string;
}

/** One row of one kind, as a key: ids are unique within a kind only. */
export function rowKey(concept: string, id: string): string {
  return `${concept}\u001f${id}`;
}

/** The engine's limit on rows in one move. */
export const MOVE_LIMIT = 500;

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

/** A list of ids off the wire: an array, or one comma-separated string. */
function ids(value: unknown): string[] {
  if (Array.isArray(value)) return value.filter((v): v is string => typeof v === "string" && v.trim() !== "");
  if (typeof value === "string") return value.split(",").map((v) => v.trim()).filter((v) => v !== "");
  return [];
}

/** A kind's own name, from its concept id: `v1:library:artifact` reads `artifact`. */
export function kindName(concept: string): string {
  const parts = concept.split(":");
  return parts[parts.length - 1] || concept;
}

export function kindRowsFromRows(rows: readonly Row[]): KindRowsPage {
  const page: KindRowsPage = { rows: [], nextCursor: "" };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "row": {
        const id = text(row["id"]);
        if (id === "") continue;
        page.rows.push({
          id,
          concept: text(row["concept"]),
          primary: text(row["primary"]),
          secondary: text(row["secondary"]),
          tertiary: text(row["tertiary"]),
          status: text(row["status"]),
          ownerUserId: text(row["ownerUserId"]),
          createdAt: text(row["createdAt"]),
          updatedAt: text(row["updatedAt"]),
          followsParent: bool(row["followsParent"]),
          parentConcept: text(row["parentConcept"]),
        });
        break;
      }
      case "page":
        page.nextCursor = text(row["nextCursor"]);
        break;
    }
  }
  return page;
}

export function movePlanFromRows(rows: readonly Row[]): MovePlan {
  const plan: MovePlan = { moves: [], follows: [], refused: [], summary: null };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "moved":
      case "wouldMove":
        plan.moves.push({ concept: text(row["concept"]), rows: count(row["rows"]) });
        break;
      case "follows":
        plan.follows.push({ concept: text(row["concept"]) });
        break;
      case "refused":
        plan.refused.push({ concept: text(row["concept"]), id: text(row["id"]), reason: text(row["reason"]) });
        break;
      case "summary":
        plan.summary = { moved: count(row["moved"]), followerKinds: count(row["followerKinds"]), refused: count(row["refused"]) };
        break;
    }
  }
  return plan;
}

export function unplacedFromRows(rows: readonly Row[]): Unplaced {
  const out: Unplaced = { kinds: [], conflicts: [] };
  for (const raw of rows) {
    const row = flatten(raw);
    switch (text(row["kind"])) {
      case "unplaced": {
        const concept = text(row["concept"]);
        if (concept === "") continue;
        out.kinds.push({
          concept,
          namespace: text(row["namespace"]),
          name: text(row["name"]) || kindName(concept),
          rows: count(row["rows"]),
        });
        break;
      }
      case "conflict":
        out.conflicts.push({
          concept: text(row["concept"]),
          rowId: text(row["rowId"]),
          conflict: text(row["conflict"]),
          detail: text(row["detail"]),
          candidates: ids(row["candidates"]),
          foundAt: text(row["foundAt"]),
        });
        break;
    }
  }
  return out;
}

/** How many rows would move, whatever the engine summed. */
export function planMoves(plan: MovePlan): number {
  return plan.summary?.moved ?? plan.moves.reduce((sum, m) => sum + m.rows, 0);
}

/** Refused rows, grouped per kind and reason, in the order they arrived. */
export function refusedByKind(plan: MovePlan): { concept: string; reason: string; rows: number }[] {
  const groups = new Map<string, { concept: string; reason: string; rows: number }>();
  for (const r of plan.refused) {
    const key = `${r.concept}\u001f${r.reason}`;
    const held = groups.get(key);
    if (held) held.rows += 1;
    else groups.set(key, { concept: r.concept, reason: r.reason, rows: 1 });
  }
  return [...groups.values()];
}

/**
 * Why the engine refused to move a row, in a sentence.
 *
 * A CLOSED SET on the engine's side. A reason this client does not know yet
 * gets one plain sentence rather than its code: no code reaches the screen.
 */
export function refusalSentence(reason: string): string {
  switch (reason) {
    case "cluster_wide":
      return "This kind belongs to the whole cluster, not to an account.";
    case "undeclared":
      return "This kind does not say which account its rows belong to.";
    case "stays":
      return "Rows of this kind stay where they were made.";
    case "follows_parent":
      return "These rows follow their parent; move the parent and they move with it.";
    case "states_own_account":
      return "This kind states its own account and is changed in its own app.";
    case "same_account":
      return "These rows are already in that account.";
    case "unknown_account":
      return "That account is not known to this cluster.";
    case "not_found":
      return "These rows were not found; they may have been removed.";
    default:
      return "It cannot be moved.";
  }
}

/**
 * Whether a failed move may still have landed: the request went out and its
 * answer was lost (the connection dropped or closed while it was in flight).
 * A refusal from the engine is an answer, and a refused move moved nothing;
 * a lost answer says nothing either way, so the screen must not claim it.
 */
export function moveOutcomeUnknown(err: unknown): boolean {
  if (!(err instanceof Error) || !("reason" in err)) return false;
  const reason = (err as { reason?: unknown }).reason;
  return reason === "transport" || reason === "closed";
}

// ---------------------------------------------------------------------------
// The calls. The SDK's generated methods build this same text and send it
// through `executeNamed`; the hooks call `executeNamed` with it directly so
// every account call goes through one path, the one the tests fake and whose
// sent text they check. Optional arguments the caller left empty are left out.
// ---------------------------------------------------------------------------

/** The default and the most the engine answers in one page. */
export const ROWS_PAGE = 50;

export function accountRowsCall(args: { accountId: string; concept: string; after: string; search: string }): string {
  const parts = [
    `accountId: ${renderMemQLValue(args.accountId)}`,
    `concept: ${renderMemQLValue(args.concept)}`,
    `limit: ${ROWS_PAGE}`,
  ];
  if (args.after !== "") parts.push(`after: ${renderMemQLValue(args.after)}`);
  if (args.search !== "") parts.push(`search: ${renderMemQLValue(args.search)}`);
  return `builtin accountRows(${parts.join(", ")})`;
}

export function accountRowsMoveCall(rows: readonly RowRef[], toAccountId: string, preview: boolean): string {
  const refs = rows.map((r) => ({ concept: r.concept, id: r.id }));
  return `builtin accountRowsMove(rows: ${renderMemQLValue(refs)}, toAccountId: ${renderMemQLValue(toAccountId)}, preview: ${preview ? "true" : "false"})`;
}

export function accountConflictLeaveOutCall(conflict: Pick<OpenConflict, "concept" | "rowId" | "conflict">): string {
  return `builtin accountConflictLeaveOut(concept: ${renderMemQLValue(conflict.concept)}, rowId: ${renderMemQLValue(conflict.rowId)}, conflict: ${renderMemQLValue(conflict.conflict)})`;
}
