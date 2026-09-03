import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../kit/rows";

// One log line, as the surfaces read it (epic memql#4895).
//
// The projection is DEFENSIVE on every field. A `v1:observability:logLine`
// row never passes through the graph -- the store answers from its own
// hypertable and hands back synthetic nodes -- so nothing upstream has
// validated the shape before it reaches this browser, and a row the engine
// agent's next change reshapes must render as an em dash rather than crash
// ten thousand rows at once.

export type LogLevel = "debug" | "info" | "warn" | "error";

export const LOG_LEVELS: readonly LogLevel[] = ["debug", "info", "warn", "error"];

export interface LogRow {
  id: string;
  /** The instant, parsed. Invalid input reads as the epoch so a sort never
   *  throws; `occurredAt` keeps the wire text for cursors and titles. */
  at: Date;
  occurredAt: string;
  nodeType: string;
  node: string;
  level: LogLevel;
  component: string;
  app: string;
  message: string;
  attributes: Record<string, unknown>;
  subject: string;
  subjectConcept: string;
  session: string;
  userId: string;
}

function text(row: Row, key: string): string {
  const v = row[key];
  if (typeof v === "string") return v;
  if (typeof v === "number" && Number.isFinite(v)) return String(v);
  return "";
}

function record(row: Row, key: string): Record<string, unknown> {
  const v = row[key];
  if (v !== null && typeof v === "object" && !Array.isArray(v)) return v as Record<string, unknown>;
  return {};
}

/** The level, narrowed to the enum. Anything else reads as `info`: an
 *  unknown level must not be drawn as an error (the loudest reading) nor
 *  dropped (the silent one). */
export function levelOf(value: unknown): LogLevel {
  return typeof value === "string" && (LOG_LEVELS as readonly string[]).includes(value)
    ? (value as LogLevel)
    : "info";
}

export function logRowFromRow(raw: Row): LogRow {
  const row = flatten(raw);
  const occurredAt = text(row, "occurredAt") || text(row, "createdAt");
  const parsed = Date.parse(occurredAt);
  return {
    id: text(row, "id"),
    at: new Date(Number.isNaN(parsed) ? 0 : parsed),
    occurredAt,
    nodeType: text(row, "nodeType"),
    node: text(row, "node"),
    level: levelOf(row.level),
    component: text(row, "component"),
    app: text(row, "app"),
    message: text(row, "message"),
    attributes: record(row, "attributes"),
    subject: text(row, "subject"),
    subjectConcept: text(row, "subjectConcept"),
    session: text(row, "session"),
    userId: text(row, "userId"),
  };
}

/** The level as a word. Colour is never the only carrier (spec H), so warn
 *  and error are also named in full on every row. */
export function levelWord(level: LogLevel): string {
  switch (level) {
    case "debug":
      return "Debug";
    case "warn":
      return "Warning";
    case "error":
      return "Error";
    default:
      return "Info";
  }
}

function inlineValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (value === null || value === undefined) return "";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/** Attributes as `key=value` pairs on one line, the way a log reads. A
 *  string with a space is quoted so the pairs stay separable by eye. */
export function attrsInline(attributes: Record<string, unknown>): string {
  return Object.entries(attributes)
    .map(([key, value]) => {
      const rendered = inlineValue(value);
      return `${key}=${/\s/.test(rendered) ? JSON.stringify(rendered) : rendered}`;
    })
    .join(" ");
}

/** The last segment of a concept id -- "site" for `v1:platform:site` -- for
 *  the subject mark. Never composes or parses a canonical NODE id; this is
 *  the concept's own name, which the client already holds as a constant. */
export function conceptWord(concept: string): string {
  const trimmed = concept.trim();
  if (trimmed === "") return "subject";
  const parts = trimmed.split(":");
  return parts[parts.length - 1] || trimmed;
}
