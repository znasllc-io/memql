// The copy-diagnostics report (memql#4744). PURE: every input is passed in,
// including the clock, so the whole thing is a snapshot test.
//
// WHAT IS DELIBERATELY NOT IN IT. The report exists to be pasted into a
// support thread, an issue, a chat -- somewhere with an audience the person
// pasting it has not fully enumerated. So it carries no bearer or token
// material, no credential presence map (which vendor slots are filled is
// reconnaissance even without the values -- the same argument that makes
// `providerAuthStatus` owner-only), and no email address but the session's
// own. Cluster facts appear only when this session was ADMITTED to them and
// the reads actually returned; "not admitted" is a line in the report, not
// a silent omission, because a reader has to be able to tell a fact that is
// absent from one that was never asked for.

import type { ConnectionHistory } from "./connectionHistory";
import type { HiddenSurface } from "./hiddenSurfaces";

/**
 * The cluster half, supplied by the Cluster section's own hooks so this
 * module never reaches for a connection. Three states, told apart on
 * purpose: a person who cannot read the facts and a person whose read
 * failed need different next steps.
 */
export type ClusterReport =
  | { state: "not-admitted" }
  | { state: "unavailable"; reason: string }
  | { state: "facts"; lines: readonly (readonly [string, string])[] };

export interface DiagnosticsInput {
  /** Report timestamp, milliseconds since the epoch. */
  at: number;
  domain: string;
  /** The bundle's own build identifier (`__OS_BUILD__`). */
  build: string;
  /** The resolved WebSocket endpoint, displayed and never re-dialed. */
  endpoint: string;
  userId: string;
  primaryEmail: string;
  clusterRole: string;
  connection: ConnectionHistory;
  /** Live status from the shell's own context; authoritative over the buffer. */
  connectionStatus: string;
  themePack: string;
  mode: string;
  reducedMotion: boolean;
  /** Names of the apps this session may open, in registry order. */
  admittedApps: readonly string[];
  hidden: readonly HiddenSurface[];
  cluster: ClusterReport;
}

const UNKNOWN = "unknown";

export function buildDiagnosticsReport(input: DiagnosticsInput): string {
  const out: string[] = [];

  out.push("MemQL OS -- diagnostics");
  out.push(`Generated: ${iso(input.at)}`);
  out.push("");

  out.push("Session");
  out.push(`  Cluster domain:   ${orUnknown(input.domain)}`);
  out.push(`  Shell build:      ${orUnknown(input.build)}`);
  out.push(`  Signed in as:     ${orUnknown(input.primaryEmail)}`);
  out.push(`  User id:          ${orUnknown(input.userId)}`);
  out.push(`  Cluster role:     ${orUnknown(input.clusterRole)}`);
  out.push("");

  out.push("Connection");
  out.push(`  Status:           ${orUnknown(input.connectionStatus)}`);
  out.push(`  Endpoint:         ${orUnknown(input.endpoint)}`);
  out.push(
    `  Last reconnect:   ${
      input.connection.lastReconnectAt === null
        ? "none in this session"
        : iso(input.connection.lastReconnectAt)
    }`,
  );
  if (input.connection.transitions.length === 0) {
    out.push("  Transitions:      none recorded");
  } else {
    out.push("  Transitions:");
    for (const t of input.connection.transitions) {
      const attempt = t.attempt > 0 ? ` attempt ${t.attempt}` : "";
      const error = t.error ? ` -- ${t.error}` : "";
      const kind = t.baseline ? " (reading at open)" : "";
      out.push(`    ${iso(t.at)}  ${t.status}${attempt}${kind}${error}`);
    }
  }
  out.push("");

  out.push("Appearance");
  out.push(`  Theme:            ${orUnknown(input.themePack)}`);
  out.push(`  Mode:             ${orUnknown(input.mode)}`);
  out.push(`  Reduced motion:   ${input.reducedMotion ? "on" : "off"}`);
  out.push("");

  out.push("Apps you can open");
  if (input.admittedApps.length === 0) {
    out.push("  none");
  } else {
    for (const name of input.admittedApps) out.push(`  ${name}`);
  }
  out.push("");

  out.push("Hidden from this session (presentation gating; row authz is the engine's)");
  if (input.hidden.length === 0) {
    out.push("  nothing hidden");
  } else {
    for (const h of input.hidden) {
      out.push(`  ${h.label} (${h.kind}) -- requires ${h.requires}`);
    }
  }
  out.push("");

  out.push("Cluster facts");
  if (input.cluster.state === "not-admitted") {
    out.push("  not admitted");
  } else if (input.cluster.state === "unavailable") {
    out.push(`  unavailable -- ${input.cluster.reason}`);
  } else if (input.cluster.lines.length === 0) {
    out.push("  none reported");
  } else {
    for (const [label, value] of input.cluster.lines) {
      out.push(`  ${label}: ${orUnknown(value)}`);
    }
  }

  return out.join("\n") + "\n";
}

/**
 * UTC, always. A report is read by someone who is not the person who
 * generated it, in a timezone they did not choose; a local-time stamp with
 * no offset is the one that gets misread, and adding the offset makes the
 * same session produce different text on two machines.
 */
function iso(ms: number): string {
  return new Date(ms).toISOString();
}

function orUnknown(value: string): string {
  return value.trim() === "" ? UNKNOWN : value;
}
