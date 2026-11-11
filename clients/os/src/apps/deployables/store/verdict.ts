import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The `shopifyStoreVerdict` REPLY, read into the shape the Store page draws.
//
// ===========================================================================
// THE ENGINE DECIDES AND WORDS IT; THIS FILE ONLY READS IT
// ===========================================================================
// Whether a store is being recorded, and what fixes it when it is not, is
// computed once, in `integrations/shopify/verdict.go`, and every sentence a
// person reads arrives in the reply: a reason's `sentence`, its `detail`, its
// `actLabel`, the plain words of `outside` and `withheld`. Nothing here maps a
// reason code to text -- a second wording would be a second copy of the rules
// that drifts from the first. The only words this file owns are the five
// state words, which DESIGN.md asks every surface to draw the same way.
//
// ===========================================================================
// AN ABSENT TIME STAYS ABSENT
// ===========================================================================
// The handler sends "" for a time it does not have, and Go formats a zero
// `time.Time` as the year 1 ("0001-01-01T00:00:00Z"). Both are read as null,
// so a store never checked shows a dash and never "Jan 1, 1".

/** A store's state, exactly as the engine compares it. */
export type VerdictState = "recording" | "starting" | "paused" | "needs attention" | "not connected";

const STATES: readonly VerdictState[] = ["recording", "starting", "paused", "needs attention", "not connected"];

/** One reason a store needs attention, with its one act (or none). */
export interface VerdictReason {
  code: string;
  /** The domain the reason is about, in plain words, or "". */
  domain: string;
  /** The engine's sentence. Drawn as it arrives. */
  sentence: string;
  /** The engine's detail, or "". */
  detail: string;
  count: number;
  /** True when `count` is a floor; the sentence already says "At least". */
  atLeast: boolean;
  /** "", "approve", "reconnect", "register", "retry" or "declare". */
  act: string;
  actLabel: string;
  actUrl: string;
}

/** One line of "What is recorded". */
export interface RecordedKind {
  /** catalog, orders, customers, discounts or returns. */
  kind: string;
  /** When the last change of the kind arrived, or null. */
  lastChangeAt: string | null;
  /** When the kind was last checked, or null. */
  lastCheckedAt: string | null;
}

/** One store's verdict, as the reply carries it. */
export interface StoreVerdict {
  storeId: string;
  domain: string;
  state: VerdictState;
  since: string | null;
  resumedAt: string | null;
  connectedAt: string | null;
  reasons: VerdictReason[];
  recorded: RecordedKind[];
  /** What the connection does not reach, in plain words. Never a reason. */
  outside: string[];
  /** Fields Shopify withholds, in plain words. Never a reason. */
  withheld: string[];
  /** The day the store's recorded orders begin (YYYY-MM-DD), or null. */
  ordersFrom: string | null;
  /** The owner's alert: none, staged, sent or failed, and why it failed. */
  alert: { state: string; reason: string };
  /** Whether the caller may take the acts. */
  may: { act: boolean };
}

/**
 * The verdict out of a reply's rows, or null when no row names one of the
 * five states -- an engine with no Shopify connector answers
 * `{status: "unconfigured"}`, and a state this client does not know is not
 * one it can draw.
 */
export function readStoreVerdict(rows: readonly Row[]): StoreVerdict | null {
  for (const row of rows) {
    const state = row["state"];
    if (typeof state !== "string" || !STATES.includes(state as VerdictState)) continue;
    const alert = record(row["alert"]);
    const may = record(row["may"]);
    return {
      storeId: text(row["storeId"]),
      domain: text(row["domain"]),
      state: state as VerdictState,
      since: time(row["since"]),
      resumedAt: time(row["resumedAt"]),
      connectedAt: time(row["connectedAt"]),
      reasons: list(row["reasons"]).map(toReason),
      recorded: list(row["recorded"]).map(toRecorded),
      outside: words(row["outside"]),
      withheld: words(row["withheld"]),
      ordersFrom: text(row["ordersFrom"]).trim() === "" ? null : text(row["ordersFrom"]).trim(),
      alert: { state: text(alert["state"]) || "none", reason: text(alert["reason"]) },
      may: { act: may["act"] === true },
    };
  }
  return null;
}

/** A state in one word, for a row, a slot or a bar. "" for a store that is not connected. */
export function verdictWord(state: VerdictState): string {
  switch (state) {
    case "recording":
      return "Recording";
    case "starting":
      return "Starting";
    case "paused":
      return "Paused";
    case "needs attention":
      return "Needs attention";
    default:
      return "";
  }
}

/**
 * A CONNECTED store's word once its verdict has been asked for (ruling 39):
 * the verdict's word; "Not connected" when the engine says so over a row that
 * still reads connected; "Status unknown" when the read failed or its reply
 * named no state this client knows; "" while it is first read. Never
 * "Connected": a store whose verdict was asked is said by its verdict.
 */
export function connectedStoreWord(verdict: StoreVerdict | null, read: "unread" | "reading" | "read" | "failed"): string {
  if (verdict !== null) return verdict.state === "not connected" ? "Not connected" : verdictWord(verdict.state);
  return read === "read" || read === "failed" ? "Status unknown" : "";
}

function toReason(value: unknown): VerdictReason {
  const r = record(value);
  const count = r["count"];
  return {
    code: text(r["code"]),
    domain: text(r["domain"]),
    sentence: text(r["sentence"]),
    detail: text(r["detail"]),
    count: typeof count === "number" && Number.isFinite(count) ? count : 0,
    atLeast: r["atLeast"] === true,
    act: text(r["act"]),
    actLabel: text(r["actLabel"]),
    actUrl: text(r["actUrl"]),
  };
}

function toRecorded(value: unknown): RecordedKind {
  const r = record(value);
  return { kind: text(r["kind"]), lastChangeAt: time(r["lastChangeAt"]), lastCheckedAt: time(r["lastCheckedAt"]) };
}

function time(value: unknown): string | null {
  const t = text(value).trim();
  return t === "" || t.startsWith("0001-01-01") ? null : t;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function words(value: unknown): string[] {
  return list(value).filter((w): w is string => typeof w === "string" && w.trim() !== "");
}
