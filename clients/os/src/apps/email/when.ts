import type { When } from "./campaigns/wizardModel";

// WHEN MAIL GOES, ON A PERSON'S OWN CLOCK (memql#6033).
//
// A person picks a date and time with the browser's own control, which answers
// a `datetime-local` string ("2026-10-14T09:00"): their zone, no offset. The
// engine takes an instant. Everything that crosses between the two is here, and
// so is every sentence the Email app says about a time, so a list row, a
// wizard's question and its rail all read a time the same way.
//
// THE CLOCK FACE IS 24-HOUR. "9:00" and "14:30" cannot be mistaken for each
// other; "2:30" with its AM or PM left off can, and a send is the one place a
// twelve-hour slip costs somebody's inbox at the wrong end of the day. The words
// around the numbers are the app's own English, so a sentence reads in one
// language whatever the browser's locale.

const LOCAL = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/;

/** How many days a month has, in a year: February knows about leap years. */
function daysIn(year: number, month: number): number {
  return new Date(Date.UTC(year, month, 0)).getUTCDate();
}

/**
 * A `datetime-local` value, read on this browser's clock, as the RFC 3339 UTC
 * instant the engine takes: "2026-10-14T09:00" in Phoenix is
 * "2026-10-14T16:00:00Z". Null for anything that is not a real date and time
 * (February 30, 24:00, a value that already carries a zone): the control never
 * produces one, and a time that does not parse is not a time to send at.
 */
export function localToInstant(local: string): string | null {
  const match = LOCAL.exec(local.trim());
  if (match === null) return null;
  const [year, month, day, hour, minute, second] = match.slice(1).map((part) => Number(part ?? "0")) as [number, number, number, number, number, number];
  if (month < 1 || month > 12 || day < 1 || day > daysIn(year, month)) return null;
  if (hour > 23 || minute > 59 || second > 59) return null;
  const date = new Date(year, month - 1, day, hour, minute, second);
  if (Number.isNaN(date.getTime())) return null;
  return date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The `datetime-local` value for an instant on this browser's clock: what a control's `min` is given. */
export function instantToLocal(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** This browser's time zone, by its IANA name: what a chosen time is read in. */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

interface WallClock {
  weekday: string;
  month: string;
  day: string;
  time: string;
}

/** An instant as a wall clock in a zone reads it. A zone the browser does not know reads as UTC. */
function wallClock(instant: string, zone: string): WallClock | null {
  const date = new Date(instant);
  if (Number.isNaN(date.getTime())) return null;
  const options: Intl.DateTimeFormatOptions = { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit", hourCycle: "h23" };
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat("en-US", { ...options, timeZone: zone }).formatToParts(date);
  } catch {
    parts = new Intl.DateTimeFormat("en-US", { ...options, timeZone: "UTC" }).formatToParts(date);
  }
  const part = (type: Intl.DateTimeFormatPartTypes) => parts.find((p) => p.type === type)?.value ?? "";
  return { weekday: part("weekday"), month: part("month"), day: part("day"), time: `${Number(part("hour"))}:${part("minute")}` };
}

/** "Tue, Oct 14 at 9:00": an instant on a zone's clock, for a sentence. */
export function momentWords(instant: string, zone: string): string {
  const clock = wallClock(instant, zone);
  return clock === null ? instant : `${clock.weekday}, ${clock.month} ${clock.day} at ${clock.time}`;
}

/**
 * When a campaign goes, as the words that finish a sentence: "now",
 * "on Tue, Oct 14 at 9:00", "every 2 weeks from Tue, Oct 14 at 9:00".
 *
 * The time is the person's, chosen on this browser's clock, and is read on the
 * clock of `zone` -- which in the wizard is this browser's own, so it reads back
 * exactly as it was picked.
 */
export function whenWords(when: When, zone: string): string {
  if (when.kind === "now") return "now";
  const instant = localToInstant(when.at);
  const moment = instant === null ? "a time not chosen yet" : momentWords(instant, zone);
  if (when.kind === "schedule") return instant === null ? "at a time not chosen yet" : `on ${moment}`;
  return `${when.weeks === 1 ? "every week" : `every ${when.weeks} weeks`} from ${moment}`;
}

/** "Oct 14, 9:00", on this browser's clock: a time on a list row. Not an instant is said as it is. */
export function shortMoment(instant: string, zone = browserZone()): string {
  const clock = wallClock(instant, zone);
  return clock === null ? instant : `${clock.month} ${clock.day}, ${clock.time}`;
}

/** "Oct 3", on this browser's clock: a day on a list row. */
export function shortDay(instant: string, zone = browserZone()): string {
  const clock = wallClock(instant, zone);
  return clock === null ? instant : `${clock.month} ${clock.day}`;
}

/** A wait, as minutes and seconds: "0:42", "12:05". Never negative. */
export function elapsedClock(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}
