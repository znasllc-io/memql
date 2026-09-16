import type { SiteRow } from "./rows";

export interface SiteHealth {
  siteId: string;
  hostname: string;
  bundleRef: string;
  checkedAt: string;
  state: string;
  reason: string;
  httpStatus: number;
}

export const HEALTH_MAX_AGE_MS = 5 * 60_000;

export function measuredAvailability(site: Pick<SiteRow, "bundleRef" | "health"> & { hostname?: string }, now = Date.now()): "Live" | "Unavailable" | "Unknown" {
  const h = site.health;
  if (!h || h.bundleRef !== site.bundleRef || (site.hostname !== undefined && h.hostname !== site.hostname)) return "Unknown";
  const checked = Date.parse(h.checkedAt);
  if (!Number.isFinite(checked) || checked > now + 30_000 || now - checked > HEALTH_MAX_AGE_MS) return "Unknown";
  return h.state === "reachable" ? "Live" : h.state === "unavailable" ? "Unavailable" : "Unknown";
}

export function healthExplanation(site: SiteRow, now = Date.now()): string {
  const h = site.health;
  if (!h || h.bundleRef !== site.bundleRef || h.hostname !== site.hostname) return "Published, but this deployment has not been checked yet.";
  const checked = Date.parse(h.checkedAt);
  if (!Number.isFinite(checked) || checked > now + 30_000) return "The health check has no valid observation time.";
  const age = Math.max(0, Math.floor((now - checked) / 60_000));
  const when = age === 0 ? "less than a minute ago" : `${age} ${age === 1 ? "minute" : "minutes"} ago`;
  if (now - checked > HEALTH_MAX_AGE_MS) return `Health check is overdue. Last checked ${when}.`;
  return `${h.reason || "Website check completed"}. Checked ${when}.`;
}
