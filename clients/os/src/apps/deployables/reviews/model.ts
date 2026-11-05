import { rowArray, rowBool, rowNumber, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { RecordTone } from "../../../kit/RecordRow";

// A store's reviews, as the reviews pack's builtins answer them
// (packs/reviewspack: reviewsList, reviewsGet, reviewsDecisionLog,
// reviewsSettings). Pure: the engine's answers in, what the Reviews section
// draws out, so every reading is a unit test.
//
// A REVIEW'S STATE IS THE ENGINE'S, never worked out here. It is derived from
// the decision log against the words the review holds now, and the panel only
// says what the engine said. So are the acts: reviewsGet answers the ones it
// would accept from this person on this review, and the panel offers those
// and no others.

export type ReviewState = "waiting" | "published" | "declined" | "removed" | "erased";
export type ReviewFilter = ReviewState | "all";
export type ReviewAct = "publish" | "decline" | "askToFix" | "escalate" | "remove" | "erase";

const STATES: readonly ReviewState[] = ["waiting", "published", "declined", "removed", "erased"];
const ACTS: readonly ReviewAct[] = ["publish", "decline", "askToFix", "escalate", "remove", "erase"];

/** The filters reviewsList counts reviews under. */
const COUNTED: readonly ReviewFilter[] = ["waiting", "published", "declined", "removed", "all"];

export interface ReviewDecision {
  id: string;
  reviewId: string;
  action: string;
  reason: string;
  note: string;
  decidedBy: string;
  at: string;
  revision: number;
}

export interface ReviewRow {
  id: string;
  state: ReviewState;
  productHandle: string;
  title: string;
  body: string;
  /** 1 to 5, or 0 when the writer gave none. */
  rating: number;
  authorName: string;
  /** Present for the store's people only; never shown to a shopper. */
  authorEmail: string;
  verifiedBuyer: boolean;
  revision: number;
  /** The fingerprint of these words: a decision names it back. */
  contentHash: string;
  createdAt: string;
  lastDecision: ReviewDecision | null;
}

export interface ReviewPage {
  reviews: ReviewRow[];
  total: number;
  next: string;
  /** How many reviews each filter holds, when the engine said. */
  counts: Partial<Record<ReviewFilter, number>>;
}

export interface ReviewDetail {
  review: ReviewRow;
  /** Newest first. */
  decisions: ReviewDecision[];
  /** The acts the engine would accept from this person on this review now. */
  acts: ReviewAct[];
  /** False for the cluster's operators, who host the reviews and decide none. */
  moderates: boolean;
}

export interface LogEntry extends ReviewDecision {
  reviewState: ReviewState | "";
  productHandle: string;
  title: string;
}

export interface DecisionLogPage {
  entries: LogEntry[];
  total: number;
  next: string;
}

export interface ReviewSettingsValues {
  publicDisplay: boolean;
  linkLifetimeDays: number;
  requestDelayDays: number;
  requestsEnabled: boolean;
  requestTemplateId: string;
  /** The sending identity requests leave as; named only by the cluster's operators. */
  requestSenderIdentityId: string;
  notifyEmail: string;
  removalApproverUserId: string;
}

export interface ReviewSettings {
  /** False until the cluster's operators set the store up. */
  exists: boolean;
  values: ReviewSettingsValues;
  may: { edit: boolean; nameApprover: boolean; nameSender: boolean };
  viewer: { moderates: boolean; approvesRemovals: boolean };
}

const asRow = (value: unknown): Row | null => (value && typeof value === "object" && !Array.isArray(value) ? (value as Row) : null);

/**
 * A nested value the wire may carry parsed or as JSON text: a builtin's node
 * payload is flattened into the row, and an array or object inside it has
 * reached this client either way.
 */
function nested(row: Row | null, key: string): unknown {
  const v = row?.[key];
  if (typeof v !== "string") return v;
  const text = v.trim();
  if (!text.startsWith("[") && !text.startsWith("{")) return v;
  try {
    return JSON.parse(text);
  } catch {
    return v;
  }
}

const list = (row: Row | null, key: string): Row[] => {
  const v = nested(row, key);
  return (Array.isArray(v) ? v : rowArray(row, key) ?? []).map(asRow).filter((r): r is Row => r !== null);
};

const object = (row: Row | null, key: string): Row | null => asRow(nested(row, key)) ?? rowObject(row, key);

/** A whole number the wire may carry as a number or as its text. */
const whole = (row: Row | null, key: string): number => {
  const v = row?.[key];
  if (typeof v === "number" && Number.isFinite(v)) return Math.trunc(v);
  if (typeof v === "string" && /^-?\d+$/.test(v.trim())) return Number(v.trim());
  return rowNumber(row, key);
};

/** A flag the wire may carry as a boolean or as its text. */
const flag = (row: Row | null, key: string): boolean => {
  const v = row?.[key];
  return v === true || v === "true" || rowBool(row, key);
};

function stateOf(value: string): ReviewState | "" {
  return (STATES as readonly string[]).includes(value) ? (value as ReviewState) : "";
}

export function decisionFrom(row: Row | null): ReviewDecision {
  return {
    id: rowString(row, "id"),
    reviewId: rowString(row, "reviewId"),
    action: rowString(row, "action"),
    reason: rowString(row, "reason"),
    note: rowString(row, "note"),
    decidedBy: rowString(row, "decidedBy"),
    at: rowString(row, "at") || rowString(row, "createdAt"),
    revision: Math.max(1, whole(row, "revision")),
  };
}

export function reviewFrom(row: Row | null): ReviewRow {
  const rating = whole(row, "rating");
  const last = object(row, "lastDecision");
  return {
    id: rowString(row, "id"),
    state: stateOf(rowString(row, "state")) || "waiting",
    productHandle: rowString(row, "productHandle"),
    title: rowString(row, "title"),
    body: rowString(row, "body"),
    rating: rating >= 1 && rating <= 5 ? rating : 0,
    authorName: rowString(row, "authorName"),
    authorEmail: rowString(row, "authorEmail"),
    verifiedBuyer: flag(row, "verifiedBuyer"),
    revision: Math.max(1, whole(row, "revision")),
    contentHash: rowString(row, "contentHash"),
    createdAt: rowString(row, "createdAt"),
    lastDecision: last === null ? null : decisionFrom(last),
  };
}

/** reviewsList's answer. */
export function reviewPageFrom(row: Row | null): ReviewPage {
  const counts: Partial<Record<ReviewFilter, number>> = {};
  const raw = object(row, "counts");
  for (const f of COUNTED) {
    if (raw !== null && raw[f] !== undefined) counts[f] = whole(raw, f);
  }
  return {
    reviews: list(row, "reviews").map(reviewFrom).filter((r) => r.id !== ""),
    total: whole(row, "total"),
    next: rowString(row, "next"),
    counts,
  };
}

/** reviewsGet's answer. */
export function reviewDetailFrom(row: Row | null): ReviewDetail {
  const acts = nested(row, "acts");
  return {
    review: reviewFrom(object(row, "review")),
    decisions: list(row, "decisions").map(decisionFrom),
    acts: (Array.isArray(acts) ? acts : []).filter((a): a is ReviewAct => typeof a === "string" && (ACTS as readonly string[]).includes(a)),
    moderates: flag(row, "moderates"),
  };
}

/** reviewsDecisionLog's answer. */
export function decisionLogFrom(row: Row | null): DecisionLogPage {
  return {
    entries: list(row, "decisions").map((r) => ({
      ...decisionFrom(r),
      reviewState: stateOf(rowString(r, "reviewState")),
      productHandle: rowString(r, "productHandle"),
      title: rowString(r, "title"),
    })),
    total: whole(row, "total"),
    next: rowString(row, "next"),
  };
}

/** reviewsSettings' answer. */
export function reviewSettingsFrom(row: Row | null): ReviewSettings {
  const s = object(row, "settings");
  const may = object(row, "may");
  const viewer = object(row, "viewer");
  return {
    exists: flag(row, "exists"),
    values: {
      publicDisplay: flag(s, "publicDisplay"),
      linkLifetimeDays: whole(s, "linkLifetimeDays") || 14,
      requestDelayDays: s !== null && s["requestDelayDays"] !== undefined ? whole(s, "requestDelayDays") : 14,
      requestsEnabled: flag(s, "requestsEnabled"),
      requestTemplateId: rowString(s, "requestTemplateId"),
      requestSenderIdentityId: rowString(s, "requestSenderIdentityId"),
      notifyEmail: rowString(s, "notifyEmail"),
      removalApproverUserId: rowString(s, "removalApproverUserId"),
    },
    may: { edit: flag(may, "edit"), nameApprover: flag(may, "nameApprover"), nameSender: flag(may, "nameSender") },
    viewer: { moderates: flag(viewer, "moderates"), approvesRemovals: flag(viewer, "approvesRemovals") },
  };
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

export interface Reason {
  value: string;
  label: string;
  /** What it means, in the moderator's terms: what counts, and what does not. */
  means: string;
}

/**
 * THE ONLY REASONS A REVIEW MAY BE WITHHELD: the pack's closed set
 * (packs/reviewspack/state.go, Reasons). Each applies to a five-star review
 * exactly as to a one-star one; a low rating, a harsh tone or a bad
 * experience is never one of them.
 */
export const REASONS: readonly Reason[] = [
  { value: "spam_or_advertising", label: "Spam or advertising", means: "Promotes another business or product, contains links, or repeats promotional text." },
  { value: "profanity", label: "Profanity", means: "A term on the store's profanity list, used offensively: whole words and phrases only, never letters inside a harmless word. A deliberate misspelling is judged in context. Mild frustration is not profanity." },
  { value: "harassing_or_obscene", label: "Harassing, threatening or obscene", means: "Threats, personal attacks on identifiable people, or sexual content." },
  { value: "discriminatory", label: "Discriminatory", means: "Attacks people for their race, gender, sexuality, ethnicity, religion, disability or another intrinsic characteristic." },
  { value: "personal_information", label: "Personal information", means: "Contact details, other people's full names, or anything that identifies a child. An order number on its own is not." },
  { value: "defamatory", label: "Defamatory", means: "A false statement of fact accusing an identifiable person or business of wrongdoing. Opinions and bad experiences are not." },
  { value: "false_or_dangerous", label: "Clearly false or dangerous", means: "An objectively false statement of fact, above all one that could put someone at risk. Opinions about results are not." },
  { value: "believed_fake", label: "Believed fake", means: "On objective evidence only: the same text sent several times, a product the store does not sell, or clearly another business." },
  { value: "unrelated", label: "Wholly unrelated", means: "Has nothing to do with the store's products or services." },
  { value: "confidential", label: "Confidential information", means: "Trade secrets, or confidential business or financial information." },
  { value: "undisclosed_connection", label: "Undisclosed connection", means: "Written by someone connected to the store who does not say so." },
];

/** A removal's own reason: the writer asked for their review to come down. */
export const WRITER_ASKED: Reason = { value: "reviewer_request", label: "The writer asked", means: "The writer asked for their review to be taken down." };

/** The reasons an act may name, or none for an act that names no reason. */
export function reasonsFor(act: ReviewAct): readonly Reason[] {
  switch (act) {
    case "decline":
    case "askToFix":
      return REASONS;
    case "remove":
      return [...REASONS, WRITER_ASKED];
    default:
      return [];
  }
}

export function reasonLabel(value: string): string {
  if (value === WRITER_ASKED.value) return WRITER_ASKED.label;
  return REASONS.find((r) => r.value === value)?.label ?? value;
}

/** The button for an act. */
export const ACT_LABEL: Readonly<Record<ReviewAct, string>> = {
  publish: "Publish",
  decline: "Decline",
  askToFix: "Ask to fix",
  escalate: "Record a safety concern",
  remove: "Remove",
  erase: "Erase the writer's data",
};

/** What was done, as a log line reads it. */
export function actionDone(action: string): string {
  switch (action) {
    case "publish":
      return "Published";
    case "decline":
      return "Declined";
    case "askToFix":
      return "Asked to fix";
    case "escalate":
      return "Safety concern recorded";
    case "remove":
      return "Removed";
    case "erase":
      return "Writer's data erased";
    default:
      return action;
  }
}

/** A state's dot: what waits asks for attention, what is shown is live. */
export function toneFor(state: ReviewState | ""): RecordTone {
  if (state === "waiting") return "warn";
  if (state === "published") return "accent";
  return "muted";
}

export function stateLabel(state: ReviewState | ""): string {
  switch (state) {
    case "waiting":
      return "Waiting";
    case "published":
      return "Published";
    case "declined":
      return "Declined";
    case "removed":
      return "Removed";
    case "erased":
      return "Erased";
    default:
      return "";
  }
}

/** What a list holding nothing says beneath its title: what would be there, not that nothing is. */
export function emptyFilterSentence(filter: ReviewFilter): string {
  switch (filter) {
    case "waiting":
      return "New reviews appear here until somebody publishes or declines them.";
    case "published":
      return "A review appears here once somebody publishes it, and stays until it is removed.";
    case "declined":
      return "No review was declined.";
    case "removed":
      return "No published review was removed.";
    case "erased":
      return "No writer's data was erased.";
    default:
      return "This store has no reviews yet.";
  }
}

/** A review's name in a list: its headline, else the start of its text. */
export function reviewName(review: Pick<ReviewRow, "title" | "body" | "state">): string {
  const title = review.title.trim();
  if (title !== "") return title;
  const body = review.body.trim().replace(/\s+/g, " ");
  if (body === "") return review.state === "erased" ? "Erased on request" : "Untitled review";
  return body.length > 60 ? `${body.slice(0, 59).trimEnd()}…` : body;
}

/** "4 of 5", or "No rating" when the writer gave none. */
export function ratingWords(rating: number): string {
  return rating > 0 ? `${rating} of 5` : "No rating";
}

/** Who wrote it, in plain words: "Dana, verified buyer", "No name given". */
export function writerWords(review: Pick<ReviewRow, "authorName" | "verifiedBuyer" | "state">): string {
  const name = review.authorName.trim() || (review.state === "erased" ? "Erased" : "No name given");
  return review.verifiedBuyer ? `${name}, verified buyer` : name;
}

/** "3 days", "5 hours", "a few minutes": how long something has waited. */
export function waitedFor(since: string, now: Date): string {
  const at = Date.parse(since);
  if (Number.isNaN(at)) return "";
  const minutes = Math.max(0, Math.floor((now.getTime() - at) / 60000));
  if (minutes < 60) return minutes < 5 ? "a few minutes" : `${minutes} minutes`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return hours === 1 ? "1 hour" : `${hours} hours`;
  const days = Math.floor(hours / 24);
  return `${days} days`;
}
