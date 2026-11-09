import { beforeEach, describe, expect, it } from "vitest";
import { entriesFrom, organizationEntriesFrom } from "../../src/modules/profile/useEffectiveAccess";
import { accessAdmits } from "../../src/system/registry";
import { clearEffectiveCapabilities, effectiveCapabilities, holds, holdsForOrganization, setEffectiveCapabilities, type OrganizationCapability } from "../../src/system/roles";
import { NO_PARTS, partsForOrganization } from "../../src/apps/deployables/parts";

const scoped: OrganizationCapability[] = ["acme", "beta"].flatMap(accountId => [
  ["read", "app:email"], ["read", "app:deployables"], ["read", "data"], ["create", "data"], ["update", "data"], ["execute", "app:deployables/deploy"],
].map(([verb, resource]) => ({ accountId, verb: verb!, resource: resource!, effect: accountId === "acme" ? "allow" : "deny" })));

beforeEach(() => clearEffectiveCapabilities());

describe("organization capability discovery", () => {
  it("opens an app allowed in Acme despite Beta's flattened deny without changing global actions or the self-view", () => {
    const row = { ok: true, entries: [{ verb: "read", resource: "app:email", effect: "deny", source: "group" }], organizationEntries: scoped };
    const entries = entriesFrom(row)!;
    setEffectiveCapabilities(entries, organizationEntriesFrom(row));
    expect(accessAdmits("app:email")).toBe(true);
    expect(accessAdmits("app:deployables")).toBe(true);
    expect(holds("read", "app:email")).toBe(false);
    expect(holds("execute", "app:deployables/deploy")).toBe(false);
    expect(effectiveCapabilities()).toEqual(entries);
    expect(accessAdmits("app:settings")).toBe(false);
  });

  it("opens Pipelines where an organization opens it, as the engine answers it per organization", () => {
    // Allowed in Acme, denied in Beta: the merged answer is Beta's deny, and
    // the engine answers the door per organization, so Acme's allow opens it.
    setEffectiveCapabilities([
      { verb: "read", resource: "app:pipelines", effect: "deny", source: "group" },
      { verb: "read", resource: "app:files", effect: "allow", source: "role" },
      { verb: "read", resource: "app:logs", effect: "deny", source: "role" },
    ], [
      { accountId: "acme", verb: "read", resource: "app:pipelines", effect: "allow" },
      { accountId: "acme", verb: "read", resource: "data", effect: "allow" },
      { accountId: "beta", verb: "read", resource: "app:pipelines", effect: "deny" },
    ]);
    expect(holds("read", "app:pipelines")).toBe(false);
    expect(accessAdmits("app:pipelines")).toBe(true);
    // A door the organization entries do not carry is answered once: the merged answer.
    expect(accessAdmits("app:files")).toBe(true);
    expect(accessAdmits("app:logs")).toBe(false);
  });

  it("requires app and data reads in the same organization even when the global app decision allows", () => {
    const mismatched = scoped.map(entry => entry.resource === "data" && entry.verb === "read"
      ? { ...entry, effect: entry.accountId === "acme" ? "deny" as const : "allow" as const }
      : entry);
    setEffectiveCapabilities([{ verb: "read", resource: "app:deployables", effect: "allow", source: "role" }], mismatched);
    expect(holds("read", "app:deployables")).toBe(true);
    expect(accessAdmits("app:deployables")).toBe(false);
    expect(accessAdmits("app:email")).toBe(false);
    expect(partsForOrganization("acme", NO_PARTS).deploy).toBe(false);
    expect(partsForOrganization("beta", NO_PARTS).deploy).toBe(false);
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
    expect(accessAdmits("app:email")).toBe(false);
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
    expect(organizationEntriesFrom({ ok: true, organizationEntries: [{ accountId: "", verb: "read", resource: "app:email", effect: "allow" }, { accountId: "acme", verb: "read", resource: "app:email", effect: "unknown" }] })).toEqual([{ accountId: "acme", verb: "read", resource: "app:email", effect: "deny" }]);
  });
});
