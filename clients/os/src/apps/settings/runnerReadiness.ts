import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { findIntegrationEnvelope } from "./integrationsReport";

export interface RunnerReading {
  nodeId: string;
  available: boolean;
  isolation: string;
  validUntil: string;
  detail: string;
}

export interface RunnerReport {
  checkedAt: string;
  runners: RunnerReading[];
}

export function readRunnerReport(rows: readonly Row[]): RunnerReport | null {
  const envelope = findIntegrationEnvelope(rows);
  const reports = envelope?.["integrations"];
  if (!Array.isArray(reports)) return null;
  const report = reports.find((r: unknown) => object(r)?.["name"] === "pipelines");
  const runners = object(report)?.["runners"];
  if (!Array.isArray(runners)) return null;
  return {
    checkedAt: text(envelope?.["checkedAt"]),
    runners: runners.map((value: unknown) => {
      const row = object(value);
      return {
        nodeId: text(row?.["nodeId"]),
        available: row?.["available"] === true,
        isolation: text(row?.["isolation"]),
        validUntil: text(row?.["validUntil"]),
        detail: text(row?.["detail"]),
      };
    }),
  };
}

/** Server time plus elapsed reading age avoids trusting the browser's clock. */
export function runnerWords(runner: RunnerReading, report: RunnerReport, ageMs: number): string {
  if (ageMs >= 60_000) return "Readiness needs refresh";
  if (!runner.available) return "Unavailable";
  if (runner.isolation === "passed") {
    const observed = Date.parse(report.checkedAt);
    const expires = Date.parse(runner.validUntil);
    if (!Number.isFinite(observed) || !Number.isFinite(expires)) return "Readiness unknown";
    return expires > observed + Math.max(0, ageMs) ? "Ready" : "Isolation check expired";
  }
  switch (runner.isolation) {
    case "expired": return "Isolation check expired";
    case "failed": return "Isolation check failed";
    case "inconclusive": return "Isolation not confirmed";
    case "not_proven": return "Isolation not checked";
    default: return "Readiness unknown";
  }
}

function object(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}
