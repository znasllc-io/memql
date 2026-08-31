// The `integrationStatus` reply, for the mail-sender group (memql#4742).
//
// The builtin is @sdk, so the CALL is generated and typed. What the
// generator does not type is a builtin's REPLY SHAPE, which is why this
// module exists -- and it reads only the mail-relevant slice: the resolved
// sender mode, the configured/health verdict, and the operator prose. It
// deliberately does NOT read `credentials`: that array carries no values,
// but WHICH slots are filled is reconnaissance, and the OS has no reason to
// put it on a desktop when the portal's integrations console already owns
// the credential-rotation surface.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

export interface MailStatus {
  /** RFC3339, stamped SERVER-side by the handler. Not a client clock. */
  checkedAt: string;
  probed: boolean;
  /** "graph" | "smtp" | "log" -- the resolved sender lane. */
  mode: string;
  /** "yes" | "no" | "unknown" -- a tri-state; unknown is not no. */
  configured: string;
  /** "healthy" | "unhealthy" | "degraded" | "unknown". */
  health: string;
  detail: string;
}

const MAX_ENVELOPE_DEPTH = 4;

/**
 * Dig the report out of the builtin envelope.
 *
 * A top-level `builtin X(...)` does not come back as a row set: the engine
 * marshals the handler's node map into one value keyed by node id, and that
 * id is bare-ified on the way out. So this SEARCHES for the object carrying
 * an `integrations` array rather than walking a fixed path a rename would
 * silently turn into `undefined`.
 */
export function readMailStatus(rows: readonly Row[]): MailStatus | null {
  for (const row of rows) {
    const found = find(row, 0);
    if (found) return found;
  }
  return null;
}

function find(value: unknown, depth: number): MailStatus | null {
  if (depth > MAX_ENVELOPE_DEPTH || value === null || typeof value !== "object") return null;
  if (Array.isArray(value)) {
    for (const entry of value) {
      const found = find(entry, depth + 1);
      if (found) return found;
    }
    return null;
  }

  const bag = value as Record<string, unknown>;
  const integrations = bag["integrations"];
  if (Array.isArray(integrations)) {
    const email = integrations
      .filter((e): e is Record<string, unknown> => !!e && typeof e === "object")
      .find((e) => e["name"] === "email");
    if (email) {
      return {
        checkedAt: str(bag, "checkedAt"),
        probed: bool(bag, "probed"),
        mode: str(email, "mode"),
        configured: str(email, "configured"),
        health: str(email, "health"),
        detail: str(email, "detail"),
      };
    }
  }

  for (const nested of Object.values(bag)) {
    const found = find(nested, depth + 1);
    if (found) return found;
  }
  return null;
}

/**
 * The sentence the panel shows for a sender mode.
 *
 * Log-only is the case this exists for (memql#4477): the engine reports it
 * as configured=no, health=degraded, and it must READ that way. A cluster
 * whose mail goes nowhere while every send reports success is the failure
 * the whole check is for, and "healthy" is the one word that must never
 * appear next to it.
 */
export function mailHeadline(status: MailStatus): string {
  if (status.mode === "log") return "Log only -- mail is written to the node log and delivered to nobody";
  if (status.mode === "graph") return "Microsoft Graph";
  if (status.mode === "smtp") return "SMTP";
  return "Not reported";
}

/** The dot tone, in the shell's own vocabulary. */
export function mailTone(status: MailStatus): "reachable" | "unreachable" | "off" {
  if (status.health === "healthy") return "reachable";
  if (status.health === "unknown") return "off";
  return "unreachable";
}

function str(bag: Record<string, unknown>, key: string): string {
  const value = bag[key];
  return typeof value === "string" ? value : "";
}

function bool(bag: Record<string, unknown>, key: string): boolean {
  const value = bag[key];
  if (typeof value === "boolean") return value;
  return value === "true";
}
