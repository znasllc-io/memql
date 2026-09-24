import type { AppSetupState } from "../../../system/registry";
import { isGithubAppGrant, type CredentialFeedStatus, type CredentialRow } from "./rows";

export function githubAccountsFor(rows: readonly CredentialRow[], viewer: string, revoked: readonly string[]) {
  return rows.filter(row => row.id && viewer && row.ownerUserId === viewer && isGithubAppGrant(row))
    .map(row => revoked.includes(row.id) ? { ...row, status: "revoked" } : row);
}

export function accountSetupState(accounts: readonly CredentialRow[], feed: Pick<CredentialFeedStatus, "state" | "error">): AppSetupState {
  if (feed.state !== "live" || feed.error) return "unknown";
  return accounts.some(account => account.status === "active") ? "ready" : "partial";
}
