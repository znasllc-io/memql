// The `integrationStatus` reply, for the mail-sender group (memql#4742).
//
// The builtin is @sdk, so the CALL is generated and typed. What the
// generator does not type is a builtin's REPLY SHAPE, which is why this
// module exists -- and it reads only the mail-relevant slice: the resolved
// sender mode, the configured/health verdict, and the operator prose. It
// deliberately does NOT read `credentials`: that array carries no values,
// but WHICH slots are filled is reconnaissance, and the Cluster panel has no
// reason to put it on a desktop when Settings' own Integrations section is
// the credential surface.
//
// The ENVELOPE WALK moved to integrationsReport.ts when that section landed
// (issue #4826) and this is a projection of it. Two walks over one reply
// would be two things free to disagree about where the report is, and the
// disagreement presents as "the Cluster panel shows mail and the Integrations
// section does not".

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readIntegrationsReport } from "./integrationsReport";

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

export function readMailStatus(rows: readonly Row[]): MailStatus | null {
  const report = readIntegrationsReport(rows);
  if (report === null) return null;
  const email = report.integrations.find((entry) => entry.name === "email");
  if (!email) return null;
  return {
    checkedAt: report.checkedAt,
    probed: report.probed,
    mode: email.mode,
    configured: email.configured,
    health: email.health,
    detail: email.detail,
  };
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
