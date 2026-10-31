import { localToInstant } from "../../email/when";

// When an access code ends, and when it was last opened, in words (design record
// 2026-10-29-deployable-access-codes, D8 and D13).
//
// THE ENGINE DECIDES AND THE PANEL READS. Validity is compared with the engine's
// clock on every request, so nothing here gates anything: these words only say
// what a row says, against this browser's clock, and the exact time stands behind
// each of them.

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** `siteAccessSession`'s @retention: as far back as "last opened" can see. */
const SESSION_RETENTION_MS = 30 * DAY;

/** D8's five, shortest first. There is no "never": every code ends. */
export const EXPIRY_PRESETS = [
  { id: "10m", label: "10 minutes", ms: 10 * MINUTE },
  { id: "1h", label: "1 hour", ms: HOUR },
  { id: "1d", label: "1 day", ms: DAY },
  { id: "7d", label: "7 days", ms: 7 * DAY },
  { id: "30d", label: "30 days", ms: 30 * DAY },
] as const;

export type ExpiryPreset = (typeof EXPIRY_PRESETS)[number];

/** A preset as the instant the engine takes, measured from `now`. */
export function expiryFromPreset(preset: ExpiryPreset, now: number): string {
  return new Date(now + preset.ms).toISOString();
}

export const CUSTOM_EXPIRY = "custom";

/** What a person has said so far: a preset's id or CUSTOM_EXPIRY, and the date behind Custom. */
export interface ExpiryDraft {
  choice: string;
  at: string;
}

/**
 * Where the question starts: a day.
 *
 * SHORT, BECAUSE THE TWO MISTAKES DO NOT COST THE SAME. A code that ends too soon
 * is extended in two presses and stays the same code; a code left to run too long
 * is a working credential nobody is thinking about.
 */
export const DEFAULT_EXPIRY: ExpiryDraft = { choice: "1d", at: "" };

/** The instant a draft names, or null while it names none: Custom with no date yet. */
export function expiryFromDraft(draft: ExpiryDraft, now: number): string | null {
  if (draft.choice === CUSTOM_EXPIRY) return localToInstant(draft.at);
  const preset = EXPIRY_PRESETS.find((p) => p.id === draft.choice);
  return preset === undefined ? null : expiryFromPreset(preset, now);
}

/**
 * Whether a code can no longer open anything.
 *
 * AN UNREADABLE EXPIRY COUNTS AS PAST, as it does in the engine's regenerate, so
 * the panel asks for a new one rather than offering what would be refused.
 */
export function hasExpired(expiresAt: string, now: number): boolean {
  const at = Date.parse(expiresAt);
  return !Number.isFinite(at) || now >= at;
}

/** "8 minutes", "1 hour", "3 days": the largest unit that reads, rounded to it. */
function span(ms: number): string {
  const count = (n: number, unit: string): string => `${n} ${unit}${n === 1 ? "" : "s"}`;
  const minutes = Math.round(ms / MINUTE);
  if (minutes < 60) return count(minutes, "minute");
  const hours = Math.round(ms / HOUR);
  if (hours < 24) return count(hours, "hour");
  return count(Math.round(ms / DAY), "day");
}

/**
 * "Expires in 8 minutes", and "Expired" from the instant it is past.
 *
 * THE LAST MINUTE HAS ITS OWN WORD. Rounded, thirty seconds left reads as zero
 * minutes, and zero minutes reads as expired: a code still opening the site, drawn
 * as one that does not.
 */
export function expiryWord(expiresAt: string, now: number): string {
  const at = Date.parse(expiresAt);
  if (!Number.isFinite(at)) return "Expiry unknown";
  const left = at - now;
  if (left <= 0) return "Expired";
  return left < MINUTE ? "Expires in under a minute" : `Expires in ${span(left)}`;
}

/**
 * Whether anybody has opened this secret, as far as anything can still say.
 *
 * AN OPENING OLDER THAN THE SECRET BELONGS TO THE ONE IT REPLACED (D7): the code
 * in anybody's hand now has not been used by whoever used that.
 *
 * "YET" IS A CLAIM ABOUT THE SECRET'S WHOLE LIFE, and sessions are retired at 30
 * days (D13), so past that the absence of a session says nothing about the time
 * before it. An `issuedAt` that cannot be read gets the weaker sentence too.
 */
export function lastOpenedWord(lastOpenedAt: string, issuedAt: string, now: number): string {
  const opened = Date.parse(lastOpenedAt);
  const issued = Date.parse(issuedAt);
  if (Number.isFinite(opened) && !(Number.isFinite(issued) && opened < issued)) {
    // A time ahead of this browser's clock is skew, and reads as just now.
    const elapsed = now - opened;
    return elapsed < MINUTE ? "Last opened just now" : `Last opened ${span(elapsed)} ago`;
  }
  return Number.isFinite(issued) && now - issued <= SESSION_RETENTION_MS ? "Not opened yet" : "Not opened in the last 30 days";
}
