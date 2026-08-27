import type { OsRuntimeConfig } from "../../cluster/config";
import type { IdentityFetch } from "../../auth/identityClient";

// Same facts the portal reads via query.getMyAccess (userId / primaryEmail /
// clusterRole). The OS bundle cannot dial the engine stream yet, so this is a
// slim identity /me read -- data only, not a second RBAC and not portal chrome.

export interface AccessFacts {
  userId: string;
  primaryEmail: string;
  clusterRole: string;
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export function parseAccessFacts(raw: unknown): AccessFacts | null {
  if (raw === null || typeof raw !== "object") return null;
  const row = raw as Record<string, unknown>;
  const userId = text(row.userId) || text(row.id);
  const primaryEmail = text(row.primaryEmail);
  const clusterRole = text(row.clusterRole) || text(row.role);
  if (!userId && !primaryEmail && !clusterRole) return null;
  return { userId, primaryEmail, clusterRole };
}

export function myAccessUrl(config: OsRuntimeConfig): string {
  const base = (config.identityUrl || config.identityApiBaseUrl).replace(/\/$/, "");
  return base + "/me/api/profile";
}

export async function fetchMyAccess(
  config: OsRuntimeConfig,
  fetchImpl: IdentityFetch = fetch,
): Promise<AccessFacts | null> {
  try {
    const response = await fetchImpl(myAccessUrl(config), {
      method: "GET",
      credentials: "include",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) return null;
    return parseAccessFacts(await response.json());
  } catch {
    return null;
  }
}
