import { describe, expect, it } from "vitest";
import { defaultOrganization, organizationChosen } from "../../src/apps/accounts/organization";
import { accountFromRow } from "../../src/apps/accounts/rows";
import { accessFromSummary } from "../../src/modules/profile/useResolvedAccess";
import type { ProfileAccess } from "../../src/modules/profile/access";

const accounts = ["self", "acme", "other"].map((id) => accountFromRow({ id, name: id, status: "active" }));
const access = (scope: Partial<ProfileAccess>): ProfileAccess => ({ userId: "me", primaryEmail: "me@example.test", role: "owner", roleName: "Owner", rank: 999, ...scope });

describe("organization defaults from verified access", () => {
  it("defaults an operator to the existing self account regardless of list order", () => {
    expect(defaultOrganization([...accounts].reverse(), access({ everyAccount: true }))).toBe("self");
    expect(defaultOrganization(accounts.slice(1), access({ everyAccount: true }))).toBe("");
  });
  it("defaults a client employee only to their sole authorized organization", () => {
    expect(defaultOrganization(accounts, access({ everyAccount: false, accountIds: ["acme"] }))).toBe("acme");
  });
  it("requires an explicit choice for multiple memberships, without rank or self fallback", () => {
    expect(defaultOrganization(accounts, access({ accountIds: ["self", "acme"] }))).toBe("");
    expect(defaultOrganization(accounts, access({ accountIds: [] }))).toBe("");
    expect(defaultOrganization(accounts, null)).toBe("");
  });
  it("does not offer an absent, archived or unverified organization as a default", () => {
    expect(defaultOrganization(accounts, access({ accountIds: ["foreign"] }))).toBe("");
    const archived = [accountFromRow({ id: "acme", status: "archived" })];
    expect(defaultOrganization(archived, access({ accountIds: ["acme"] }))).toBe("");
    expect(organizationChosen(archived, "acme")).toBe(false);
    expect(organizationChosen(accounts, "foreign")).toBe(false);
    expect(organizationChosen(accounts, "")).toBe(false);
  });
  it("preserves the server scope when projecting MyAccess into the OS session", () => {
    const projected = accessFromSummary({ ...access({}), accountIds: ["acme"], everyAccount: false } as Parameters<typeof accessFromSummary>[0]);
    expect(projected).toMatchObject({ accountIds: ["acme"], everyAccount: false });
  });
});
