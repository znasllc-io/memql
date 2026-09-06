import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit";

// One audit event, projected off `auditEventFull`.

export interface AuditRow {
  id: string;
  occurredAt: string;
  category: string;
  action: string;
  actorUserId: string;
  actorEmail: string;
  actorRole: string;
  targetType: string;
  targetId: string;
  targetEmail: string;
  detail: string;
  sourceIP: string;
  userAgent: string;
  correlationId: string;
  outcome: string;
  failureReason: string;
}

export function auditFromRow(raw: Row): AuditRow {
  const row = flatten(raw);
  return {
    id: stringOf(row, "id"),
    occurredAt: stringOf(row, "occurredAt"),
    category: stringOf(row, "category"),
    action: stringOf(row, "action"),
    actorUserId: stringOf(row, "actorUserId"),
    actorEmail: stringOf(row, "actorEmail"),
    actorRole: stringOf(row, "actorRole"),
    targetType: stringOf(row, "targetType"),
    targetId: stringOf(row, "targetId"),
    targetEmail: stringOf(row, "targetEmail"),
    detail: stringOf(row, "detail"),
    sourceIP: stringOf(row, "sourceIP"),
    userAgent: stringOf(row, "userAgent"),
    correlationId: stringOf(row, "correlationId"),
    outcome: stringOf(row, "outcome"),
    failureReason: stringOf(row, "failureReason"),
  };
}

/**
 * The categories the filter offers.
 *
 * A FIXED LIST, and it is deliberately not derived from the page on screen.
 * Deriving it would offer exactly the categories the first fifty rows happen
 * to carry, so the one an operator is hunting -- a category with no recent
 * events, which is often the interesting case -- would be missing from the
 * control that exists to find it.
 *
 * An event whose category is not in this list still renders; the filter just
 * has no chip for it. That is the right way round: the list is a convenience
 * over a free-text argument, not a claim about what the engine can write.
 */
export const AUDIT_CATEGORIES = [
  "authentication",
  "authorization",
  "credential",
  "configuration",
  "data",
  "security",
] as const;

/** The outcome word's tone. `failure` is the only one that earns emphasis --
 *  a page of successes with every row marked is a page with nothing marked. */
export function outcomeTone(outcome: string): "neutral" | "accent" | "muted" {
  if (outcome === "failure" || outcome === "denied") return "accent";
  return "muted";
}

function stringOf(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}
