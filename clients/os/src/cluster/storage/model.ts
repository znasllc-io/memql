import type { Row } from "@znasllc-io/memql-sdk-core/client";

export interface StorageReport {
  observedAt: number;
  dataBytes: number;
  dataCapacityBytes: number;
  walBytes: number;
  walCapacityBytes: number;
  issues: string[];
}
export const STORAGE_REPORT_MAX_AGE_MS = 15 * 60_000;
export function readStorageReport(row: Row | null): StorageReport {
  if (!row || row.available !== true) throw new Error(typeof row?.reason === "string" ? row.reason : "Storage measurements are unavailable.");
  const keys = ["observedAt", "dataBytes", "dataCapacityBytes", "walBytes", "walCapacityBytes"] as const;
  for (const key of keys) {
    if (typeof row[key] !== "number" || !Number.isFinite(row[key]) || row[key] < 0) throw new Error("Storage measurements are incomplete.");
  }
  if (!(Number(row.observedAt) > 0 && Number(row.dataCapacityBytes) > 0 && Number(row.walCapacityBytes) > 0)) throw new Error("Storage measurements are incomplete.");
  return { observedAt: Number(row.observedAt) * 1000, dataBytes: Number(row.dataBytes), dataCapacityBytes: Number(row.dataCapacityBytes), walBytes: Number(row.walBytes), walCapacityBytes: Number(row.walCapacityBytes), issues: Array.isArray(row.issues) ? row.issues.filter((v): v is string => typeof v === "string") : [] };
}
export function reportFresh(report: StorageReport, now: number): boolean {
  const age = now - report.observedAt;
  return age >= -60_000 && age <= STORAGE_REPORT_MAX_AGE_MS;
}
// Alert policy belongs to the existing monitor. Other database-health failures
// (backups, replication) do not pretend to be a capacity warning here.
export function storageWarning(report: StorageReport): boolean {
  return report.issues.some((issue) => issue === "data-volume-over-80-percent" || issue === "wal-volume-over-80-percent");
}
export const percentUsed = (used: number, capacity: number) => `${Math.round(used / capacity * 100)}%`;
export function warningSentence(report: StorageReport): string {
  const wal = report.issues.includes("wal-volume-over-80-percent");
  const data = report.issues.includes("data-volume-over-80-percent");
  if (data && wal) return "Database and recovery storage are nearly full.";
  return wal ? `Recovery storage is ${percentUsed(report.walBytes, report.walCapacityBytes)} full.` : `Database storage is ${percentUsed(report.dataBytes, report.dataCapacityBytes)} full.`;
}
