// Value formatting shared by every element.
//
// One module so a number reads the same in a table cell, a chart axis and a
// stat tile. Two elements formatting the same value differently is the same
// class of drift the shared stylesheet exists to prevent.
//
// EVERYTHING HERE IS UTC AND LOCALE-FREE. `toLocaleString` would make the
// rendered output depend on the host's ICU data, which means the VS Code
// panel, the portal and the test suite can each produce a different string
// for the same row -- and view-kit's tests assert on strings. A view-kit
// timestamp is therefore the wire's own instant, rendered in UTC, in the same
// ISO-derived shape the wire used.

import type { RowLike } from "./types.js";

// scalarText renders a row field as display text. Objects and arrays return
// "" rather than "[object Object]": a slot pointed at a structured field has
// nothing to show, and an empty string lets the caller fall back.
export function scalarText(row: RowLike, field: string | undefined): string {
  if (!field) return "";
  const v = row[field];
  if (typeof v === "string") return v;
  if (typeof v === "number") return Number.isFinite(v) ? formatNumber(v) : "";
  if (typeof v === "boolean") return String(v);
  return "";
}

// numberValue reads a field as a number. Strings that are numeric are
// accepted -- the wire carries int64 as a string in some encodings, and a
// chart refusing to plot "42" would be a surprising failure.
export function numberValue(row: RowLike, field: string | undefined): number | undefined {
  if (!field) return undefined;
  const v = row[field];
  if (typeof v === "number") return Number.isFinite(v) ? v : undefined;
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    return Number.isFinite(n) ? n : undefined;
  }
  return undefined;
}

export function booleanValue(row: RowLike, field: string | undefined): boolean | undefined {
  if (!field) return undefined;
  const v = row[field];
  if (typeof v === "boolean") return v;
  if (v === "true") return true;
  if (v === "false") return false;
  return undefined;
}

// instantValue reads a field as a UTC instant.
export function instantValue(row: RowLike, field: string | undefined): Date | undefined {
  if (!field) return undefined;
  const v = row[field];
  if (typeof v !== "string" || v === "") return undefined;
  const ms = Date.parse(v);
  return Number.isNaN(ms) ? undefined : new Date(ms);
}

function pad(n: number, width = 2): string {
  return String(n).padStart(width, "0");
}

export function formatDate(d: Date): string {
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

export function formatTime(d: Date): string {
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

export function formatDateTime(d: Date): string {
  return `${formatDate(d)} ${formatTime(d)}`;
}

export const MONTH_NAMES: readonly string[] = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

// Sunday-first, matching the ISO week's rendering convention in every calendar
// UI this package is likely to sit in. Two letters so a narrow column still
// shows all seven.
export const WEEKDAY_NAMES: readonly string[] = ["Su", "Mo", "Tu", "We", "Th", "Fr", "Sa"];

// formatNumber groups thousands and keeps at most two decimals, trimming
// trailing zeros. Hand-rolled rather than Intl.NumberFormat for the
// determinism reason at the top of this file.
export function formatNumber(value: number): string {
  if (!Number.isFinite(value)) return "";
  const negative = value < 0;
  const abs = Math.abs(value);
  const rounded = Math.round(abs * 100) / 100;
  const whole = Math.floor(rounded);
  const fraction = rounded - whole;
  let out = String(whole).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  if (fraction > 0) {
    out += String(Math.round(fraction * 100) / 100).slice(1);
  }
  return negative ? `-${out}` : out;
}

// formatRelative renders an instant as elapsed time -- "2 days ago", "in 3
// hours" -- which is the question people actually ask of a timestamp.
//
// LOCALE-FREE AND UTC, like everything else here: no Intl.RelativeTimeFormat,
// because view-kit's tests assert on strings and the VS Code panel, the portal
// and a CI runner must not each produce a different one.
//
// `now` is a parameter with a default rather than a call to Date.now() inside,
// so a test can pin it. A renderer that reads the clock is a renderer whose
// output cannot be asserted on.
//
// The boundaries are deliberately coarse. "1 month ago" for anything from 30
// to 60 days is imprecise and correct: the exact instant is one hover away,
// and a reader scanning a column wants the magnitude.
export function formatRelative(value: Date, now: Date = new Date()): string {
  const seconds = Math.round((now.getTime() - value.getTime()) / 1000);
  const future = seconds < 0;
  const abs = Math.abs(seconds);

  const said = (n: number, unit: string): string => {
    const plural = n === 1 ? unit : `${unit}s`;
    return future ? `in ${n} ${plural}` : `${n} ${plural} ago`;
  };

  // Under a minute is "just now" in both directions: "in 4 seconds" is noise
  // for a value that will be wrong before it is read.
  if (abs < 45) return "just now";
  if (abs < 3600) return said(Math.round(abs / 60), "minute");
  if (abs < 86400) return said(Math.round(abs / 3600), "hour");
  if (abs < 2592000) return said(Math.round(abs / 86400), "day");
  if (abs < 31536000) return said(Math.round(abs / 2592000), "month");
  return said(Math.round(abs / 31536000), "year");
}

// formatCompact keeps a value short where it is read at a glance -- an axis
// tick, a column of figures. Full precision belongs where a reader has asked
// for one number: a tooltip, a direct label, a detail pane.
//
// It lived in chart.ts until table cells needed it too. Two implementations of
// "what does 1,247,932 look like when it has to be short" is exactly the drift
// this module exists to prevent.
export function formatCompact(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1e9) return `${trimTenth(value / 1e9)}B`;
  if (abs >= 1e6) return `${trimTenth(value / 1e6)}M`;
  if (abs >= 1e3) return `${trimTenth(value / 1e3)}k`;
  return formatNumber(value);
}

function trimTenth(value: number): string {
  return String(Math.round(value * 10) / 10);
}

export function isMissing(value: unknown): boolean {
  return value === null || value === undefined || value === "";
}

// compareScalars orders two PRESENT row values for a sortable column. Numbers
// compare numerically, everything else by code unit -- not by locale, per the
// header. Missing values are the caller's problem on purpose: they must sort
// last in BOTH directions (an empty cell is not "the smallest value", it is
// the absence of one), so they cannot go through a comparator the caller
// negates for descending.
export function compareScalars(a: unknown, b: unknown): number {
  if (typeof a === "number" && typeof b === "number") return a - b;
  if (typeof a === "boolean" && typeof b === "boolean") return Number(a) - Number(b);
  const sa = String(a);
  const sb = String(b);
  return sa < sb ? -1 : sa > sb ? 1 : 0;
}
