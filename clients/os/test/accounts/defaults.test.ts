import { describe, expect, it } from "vitest";
import { accountFromRow } from "../../src/apps/accounts/rows";
import { accountDefaultDomain, accountDefaultSender, accountDeployableDomain, accountDomains } from "../../src/apps/accounts/defaults";
const account = (values = {}) => accountFromRow({ id: "acme", status: "active", domain: "EXAMPLE.com.", ...values });
const sender = (id: string, values = {}) => ({ id, accountId: "acme", address: "news@example.com", status: "active", ...values });
describe("account creation defaults", () => {
  it("uses the legacy domain until explicitly configured, including an explicit opt-out", () => {
    expect(accountDefaultDomain(account())).toBe("example.com");
    expect(accountDomains(account({ domains: [] }))).toEqual([]);
    expect(accountDefaultDomain(account({ domains: ["other.test", "EXAMPLE.COM"], defaultDomain: "example.com" }))).toBe("example.com");
    expect(accountDefaultDomain(account({ status: "archived" }))).toBe("");
  });
  it("gives each deployable its own hostname and rejects invalid profile text", () => {
    expect(accountDeployableDomain(account(), "shop")).toBe("shop.example.com");
    expect(accountDeployableDomain(account(), "docs")).toBe("docs.example.com");
    for (const domain of ["https://example.com", "127.0.0.1", "localhost", "*.example.com", "-bad.test", "a..test"]) expect(accountDefaultDomain(account({ domain }))).toBe("");
    expect(accountDeployableDomain(account(), "invalid slug")).toBe("");
  });
  it("chooses only an unambiguous sender in the account's default domain", () => {
    expect(accountDefaultSender(account(), [sender("foreign", { accountId: "beta" }), sender("ours")])).toBe("ours");
    expect(accountDefaultSender(account(), [sender("a"), sender("b")])).toBe("");
    expect(accountDefaultSender(account(), [sender("a", { address: "news@different.test" })])).toBe("");
    expect(accountDefaultSender(account({ domain: "" }), [sender("a")])).toBe("");
  });
  it("honors a chosen sender and never silently replaces an unavailable one", () => {
    expect(accountDefaultSender(account({ defaultSenderIdentityId: "b" }), [sender("a"), sender("b")])).toBe("b");
    for (const values of [{ status: "disabled" }, { accountId: "beta" }]) expect(accountDefaultSender(account({ defaultSenderIdentityId: "b" }), [sender("a"), sender("b", values)])).toBe("");
    expect(accountDefaultSender(account({ defaultSenderIdentityId: "missing" }), [sender("a")])).toBe("");
  });
});
