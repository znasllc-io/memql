import { beforeEach, describe, expect, it } from "vitest";
import { entriesFrom, organizationEntriesFrom } from "../../src/modules/profile/useEffectiveAccess";
import { accessAdmits } from "../../src/system/registry";
import { clearEffectiveCapabilities, effectiveCapabilities, holds, holdsForOrganization, setEffectiveCapabilities, type OrganizationCapability } from "../../src/system/roles";
import { NO_PARTS, partsForOrganization } from "../../src/apps/deployables/parts";

const scoped: OrganizationCapability[] = ["acme", "beta"].flatMap(accountId => [
  ["read", "app:campaigns"], ["read", "app:deployables"], ["create", "data"], ["update", "data"], ["execute", "app:deployables/deploy"],
].map(([verb, resource]) => ({ accountId, verb: verb!, resource: resource!, effect: accountId === "acme" ? "allow" : "deny" })));

beforeEach(() => clearEffectiveCapabilities());

describe("organization capability discovery", () => {
  it("opens an app allowed in Acme despite Beta's flattened deny without changing global actions or the self-view", () => {
    const row = { ok: true, entries: [{ verb: "read", resource: "app:campaigns", effect: "deny", source: "group" }], organizationEntries: scoped };
    const entries = entriesFrom(row)!;
    setEffectiveCapabilities(entries, organizationEntriesFrom(row));
    expect(accessAdmits("app:campaigns")).toBe(true);
    expect(accessAdmits("app:deployables")).toBe(true);
    expect(holds("read", "app:campaigns")).toBe(false);
    expect(holds("execute", "app:deployables/deploy")).toBe(false);
    expect(effectiveCapabilities()).toEqual(entries);
    expect(accessAdmits("app:settings")).toBe(false);
  });

  it("uses the selected organization's action decision and refuses foreign or missing targets", () => {
    setEffectiveCapabilities([], scoped);
    expect(partsForOrganization("acme", NO_PARTS).deploy).toBe(true);
    expect(partsForOrganization("beta", NO_PARTS).deploy).toBe(false);
    expect(partsForOrganization("foreign", NO_PARTS).deploy).toBe(false);
    expect(partsForOrganization("", NO_PARTS).deploy).toBe(false);
    expect(holdsForOrganization("acme", "create", "data")).toBe(true);
    expect(holdsForOrganization("beta", "update", "data")).toBe(false);
  });

  it("replaces scoped permissions on refresh and clears them on sign out", () => {
    setEffectiveCapabilities([], scoped);
    setEffectiveCapabilities([], scoped.filter(entry => entry.accountId === "beta"));
    expect(accessAdmits("app:campaigns")).toBe(false);
    expect(holdsForOrganization("acme", "update", "data")).toBe(false);
    clearEffectiveCapabilities();
    expect(accessAdmits("app:deployables")).toBe(false);
  });

  it("retains global operator decisions when no scoped set is returned", () => {
    setEffectiveCapabilities([{ verb: "update", resource: "data", effect: "allow", source: "role" }]);
    expect(holdsForOrganization("self", "update", "data")).toBe(true);
  });

  it("drops malformed scopes and treats unknown effects as refusals", () => {
    expect(organizationEntriesFrom({ ok: false, organizationEntries: scoped })).toEqual([]);
    expect(organizationEntriesFrom({ ok: true, organizationEntries: [{ accountId: "", verb: "read", resource: "app:campaigns", effect: "allow" }, { accountId: "acme", verb: "read", resource: "app:campaigns", effect: "unknown" }] })).toEqual([{ accountId: "acme", verb: "read", resource: "app:campaigns", effect: "deny" }]);
  });
});
