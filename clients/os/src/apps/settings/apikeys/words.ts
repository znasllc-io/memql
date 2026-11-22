import { refusalSentence, type Row } from "@znasllc-io/memql-sdk-core/client";

import { formatMoney } from "../../../kit/format";
import { flatten } from "../../../kit/rows";
import type { RecordTone } from "../../../kit/RecordRow";

// Settings > API keys, as values (design record
// 2026-11-21-vendor-accounts-and-api-keys, U1 to U3).
//
// PURE, beside the components that draw it, for the reason machines/sharing.ts
// is: what a key's row says, which state is drawn and what a refusal reads as
// are the parts of this surface a person is promised things by, and they are
// asserted here without three layers of render between the test and the rule.
//
// NOTHING HERE HOLDS A KEY'S VALUE. A card has no field for one (the cluster
// sends none), and the one function that is handed what a person typed,
// `refusalWords`, takes it only to make sure it is never repeated.

export type Vendor = "anthropic" | "openai";

/** The vendors a key can be for, in the order the wizard offers them. */
export const VENDORS: readonly { value: Vendor; label: string; description: string; placeholder: string }[] = [
  { value: "anthropic", label: "Anthropic", description: "Claude models", placeholder: "sk-ant-..." },
  { value: "openai", label: "OpenAI", description: "GPT models", placeholder: "sk-..." },
];

export function vendorName(vendor: string): string {
  return VENDORS.find((v) => v.value === vendor)?.label ?? vendor;
}

/** Who a key is lent to, as its owner's card carries it. */
export interface KeySharing {
  mode: "owner" | "people";
  paused: boolean;
  userIds: string[];
  groupIds: string[];
  pausedUserIds: string[];
  pausedGroupIds: string[];
}

export const NOT_LENT: KeySharing = { mode: "owner", paused: false, userIds: [], groupIds: [], pausedUserIds: [], pausedGroupIds: [] };

/**
 * One key, as the reader may see it.
 *
 * `full` is false for the SHORT card of a key lent to the reader: its name,
 * vendor, owner and state, and nothing about how its owner set it up. The
 * fields a short card does not carry are blank here, never guessed.
 */
export interface KeyCard {
  id: string;
  vendor: string;
  label: string;
  /** The last characters of the key, so its owner can tell two apart. */
  fingerprint: string;
  isDefault: boolean;
  autoUse: boolean;
  /** `use`: the vendor accepts the key but it may not list models (K12). */
  checkedBy: string;
  state: string;
  /** The vendor's words behind a refusal or a credit hold. */
  detail: string;
  addedAt: string;
  checkedAt: string;
  lastUsedAt: string;
  heldUntil: string;
  /** How many people and groups it is lent to. */
  lentTo: number;
  ownerUserId: string;
  ownerName: string;
  relation: "mine" | "lent" | "other";
  /** A lent key whose owner paused lending it to the reader. */
  paused: boolean;
  full: boolean;
  /** Who it is lent to. Only on the reader's own key. */
  sharing: KeySharing;
  /** A sentence from the cluster about the act that returned this card. */
  note: string;
}

function text(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v.trim() : "";
}

/** A flag on a builtin's row, which has reached this client as text too. */
function flag(row: Row, key: string): boolean {
  return row[key] === true || row[key] === "true";
}

function count(row: Row, key: string): number {
  const n = Number(row[key]);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((m): m is string => typeof m === "string" && m.trim() !== "") : [];
}

function sharingFrom(v: unknown): KeySharing {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return NOT_LENT;
  const s = v as Row;
  return {
    mode: s["mode"] === "people" ? "people" : "owner",
    paused: flag(s, "paused"),
    userIds: strings(s["userIds"]),
    groupIds: strings(s["groupIds"]),
    pausedUserIds: strings(s["pausedUserIds"]),
    pausedGroupIds: strings(s["pausedGroupIds"]),
  };
}

/** One card off the wire. Null for a row that names no key. */
export function keyCardFrom(raw: unknown): KeyCard | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const row = flatten(raw as Row);
  const id = text(row, "id");
  if (id === "") return null;
  const relation = text(row, "relation");
  const lent = relation === "lent" && !("fingerprint" in row);
  return {
    id,
    vendor: text(row, "vendor"),
    label: text(row, "label") || "Unnamed key",
    fingerprint: text(row, "fingerprint"),
    isDefault: flag(row, "isDefault"),
    autoUse: flag(row, "autoUse"),
    checkedBy: text(row, "checkedBy"),
    state: text(row, "state") || "unchecked",
    detail: text(row, "detail"),
    addedAt: text(row, "addedAt"),
    checkedAt: text(row, "checkedAt"),
    lastUsedAt: text(row, "lastUsedAt"),
    heldUntil: text(row, "heldUntil"),
    lentTo: count(row, "lentTo"),
    ownerUserId: text(row, "ownerUserId"),
    ownerName: text(row, "ownerName"),
    relation: relation === "mine" ? "mine" : relation === "lent" ? "lent" : "other",
    paused: flag(row, "paused"),
    full: !lent,
    sharing: sharingFrom(row["sharing"]),
    note: text(row, "note"),
  };
}

export interface KeyLists {
  mine: KeyCard[];
  lent: KeyCard[];
  /** Every active key on the cluster, for a cluster owner who asked. */
  all: KeyCard[];
}

function cards(v: unknown): KeyCard[] {
  return Array.isArray(v) ? v.map(keyCardFrom).filter((c): c is KeyCard => c !== null) : [];
}

/**
 * The lists `vendorKeysForActor` answers. Null for a reply that is not one:
 * the cluster always answers one row, so anything else is a failed read and
 * never "no keys".
 */
export function keyListsFrom(rows: readonly Row[]): KeyLists | null {
  const first = rows[0] === undefined ? null : flatten(rows[0]);
  if (first === null || !Array.isArray(first["mine"]) || !Array.isArray(first["lent"])) return null;
  return { mine: cards(first["mine"]), lent: cards(first["lent"]), all: cards(first["all"]) };
}

// ---------------------------------------------------------------------------
// The state, in words
// ---------------------------------------------------------------------------

export interface StateWords {
  word: string;
  tone: RecordTone;
  /** The key cannot pay until something changes, so it is said wherever it is drawn. */
  needsAttention: boolean;
}

/** A key's state as a person reads it. A lent key its owner paused is Paused whatever else is true. */
export function stateWords(card: Pick<KeyCard, "state" | "paused">): StateWords {
  if (card.paused) return { word: "Paused", tone: "muted", needsAttention: true };
  switch (card.state) {
    case "good":
      return { word: "Working", tone: "accent", needsAttention: false };
    case "refused":
      return { word: "Refused", tone: "warn", needsAttention: true };
    case "credit":
      return { word: "Out of credit", tone: "warn", needsAttention: true };
    default:
      return { word: "Not checked yet", tone: "muted", needsAttention: false };
  }
}

/**
 * Whether the list draws each row's state.
 *
 * A LIST THAT MIXES STATES DRAWS THEM; ONE WHERE EVERY KEY IS WORKING DOES
 * NOT, because every row would say the same word (DESIGN.md, "Record lists").
 * A KEY THAT CANNOT PAY IS ALWAYS SAID: with one key and that key refused,
 * the list does not mix states, and nothing else on the page would tell its
 * owner why their calls are not being paid for.
 */
export function drawsStates(shown: readonly Pick<KeyCard, "state" | "paused">[]): boolean {
  const words = new Set(shown.map((card) => stateWords(card).word));
  return words.size > 1 || shown.some((card) => stateWords(card).needsAttention);
}

// ---------------------------------------------------------------------------
// What it cost
// ---------------------------------------------------------------------------

/** One person's use of a key this month. */
export interface ActivityPerson {
  userId: string;
  name: string;
  mine: boolean;
  calls: number;
  estimatedCost: number;
}

/** What a key paid for this month, by this cluster's estimate. */
export interface KeyActivity {
  keyId: string;
  month: string;
  calls: number;
  estimatedCost: number;
  /** False when a model that was called has no price listed here: the total is then short. */
  priced: boolean;
  people: ActivityPerson[];
}

export function activityFrom(rows: readonly Row[]): KeyActivity | null {
  const first = rows[0] === undefined ? null : flatten(rows[0]);
  if (first === null || text(first, "keyId") === "") return null;
  const people = Array.isArray(first["people"]) ? first["people"] : [];
  return {
    keyId: text(first, "keyId"),
    month: text(first, "month"),
    calls: count(first, "calls"),
    estimatedCost: Number(first["estimatedCost"]) || 0,
    priced: first["priced"] !== false && first["priced"] !== "false",
    people: people
      .filter((p): p is Row => p !== null && typeof p === "object")
      .map((p) => ({ userId: text(p, "userId"), name: text(p, "name"), mine: flag(p, "mine"), calls: count(p, "calls"), estimatedCost: Number(p["estimatedCost"]) || 0 })),
  };
}

const dollars = formatMoney("USD");

/**
 * A month's cost, in a row's few words.
 *
 * NEVER A ZERO THAT IS NOT ONE. A key nothing used cost nothing and says "Not
 * used"; a key whose calls have no price listed here is "Not priced", not
 * $0.00; and a month only partly priced is a floor, said the way a count the
 * engine stopped at is ("or more").
 */
export function costWords(activity: Pick<KeyActivity, "calls" | "estimatedCost" | "priced">): string {
  if (activity.calls === 0) return "Not used this month";
  // ALWAYS SAID TO BE AN ESTIMATE, on the row as on the key's page. The figure
  // is this cluster's own, from the price it lists for each model, and a
  // number with a dollar sign beside a vendor's name reads as that vendor's
  // bill unless it says otherwise.
  if (activity.priced) return `Estimated ${dollars(activity.estimatedCost)} this month`;
  if (activity.estimatedCost > 0) return `Estimated ${dollars(activity.estimatedCost)} or more this month`;
  return "Not priced";
}

/** The same figure on a key's page, where the label already says "this month". */
export function costFigure(activity: Pick<KeyActivity, "calls" | "estimatedCost" | "priced">): string {
  if (activity.calls === 0) return "Nothing yet";
  if (activity.priced) return dollars(activity.estimatedCost);
  if (activity.estimatedCost > 0) return `${dollars(activity.estimatedCost)} or more`;
  return "Not priced";
}

export function callsWords(calls: number): string {
  return calls === 1 ? "1 call this month" : `${calls.toLocaleString("en-US")} calls this month`;
}

// ---------------------------------------------------------------------------
// A key's name
// ---------------------------------------------------------------------------

export const MAX_NAME = 60;

/**
 * Why a name cannot be used, or "" when it can. An empty name is not a
 * problem to report: nothing has been typed yet.
 *
 * Names are compared without regard to case across both vendors, as the
 * cluster compares them: "Work" and "work" are one name to a person.
 */
export function nameProblem(name: string, taken: readonly string[]): string {
  const trimmed = name.trim();
  if (trimmed === "") return "";
  if (trimmed.length > MAX_NAME) return `A name is at most ${MAX_NAME} characters.`;
  if (taken.some((held) => held.trim().toLowerCase() === trimmed.toLowerCase())) return "You already have a key with that name.";
  return "";
}

// ---------------------------------------------------------------------------
// A refusal, read for a person
// ---------------------------------------------------------------------------

const NOT_PERMITTED = /capability_not_held|rank_floor_not_met|not permitted/i;

/**
 * The sentence a refused call gives a person.
 *
 * The key builtins' own refusals are already sentences and come through as
 * written, with the frames a call adds taken off. A coded refusal comes from
 * the gate in front of them and reads in this surface's words.
 *
 * `typed` IS WHAT THE PERSON TYPED INTO THE KEY FIELD, AND IT IS NEVER
 * REPEATED. The cluster's refusals do not carry it. If one ever did, this is
 * the last place it could be caught before it is drawn, so it is cut out here
 * whatever the message was.
 */
export function refusalWords(err: unknown, construct: string, typed = ""): string {
  const raw = err instanceof Error ? err.message : String(err);
  let sentence = NOT_PERMITTED.test(raw)
    ? "You may not add API keys on this cluster. A cluster owner can allow it for you in Users, under Access."
    : refusalSentence(err, construct).trim();
  const secret = typed.trim();
  if (secret !== "" && sentence.includes(secret)) sentence = "The cluster refused, and its answer cannot be shown here.";
  return sentence === "" ? "That did not complete. Try again." : sentence;
}

/** What to say when a write's reply never came: it may have landed. */
export const UNANSWERED: Readonly<Record<string, string>> = {
  add: "The connection dropped before the cluster answered, so this key may have been added. Check the list before trying again.",
  update: "The connection dropped before the cluster answered, so this change may have been saved. Check the key before trying again.",
  check: "The connection dropped before the cluster answered. The check may still have been recorded.",
  remove: "The connection dropped before the cluster answered, so this key may have been removed. Check the list before trying again.",
  sharing: "The connection dropped before the cluster answered, so this change may have been saved.",
};
