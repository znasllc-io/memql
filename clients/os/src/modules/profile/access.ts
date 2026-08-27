// MyAccess data shape, data only. No portal chrome (memql#4706).

export interface ProfileAccess {
  userId: string;
  primaryEmail: string;
  clusterRole: string;
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export function parseProfileAccess(raw: unknown): ProfileAccess | null {
  if (!raw || typeof raw !== "object") return null;
  const rec = raw as Record<string, unknown>;
  const userId = text(rec.userId);
  const primaryEmail = text(rec.primaryEmail);
  const clusterRole = text(rec.clusterRole);
  if (!userId || !primaryEmail || !clusterRole) return null;
  return { userId, primaryEmail, clusterRole };
}
