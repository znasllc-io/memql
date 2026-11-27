import type { AccountAccess } from "../src/apps/accounts/useAccountAccess";

export function accountAccessFixture(over: Partial<AccountAccess> = {}): AccountAccess {
  return {
    accountId: "a1", ownerUserId: "owner", primary: false, actorRole: "owner", clusterOwner: true,
    revision: "revision-one", apps: ["app:deployables"],
    people: [{ id: "owner", name: "Account owner", email: "owner@example.test", role: "owner", apps: ["app:deployables"] }],
    candidates: [], groups: [], invitations: [], ...over,
  };
}
