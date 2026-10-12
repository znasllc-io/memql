import { describe, expect, it } from "vitest";

import type { AccountRow } from "../../src/apps/accounts/rows";
import { SELF_ACCOUNT_ID } from "../../src/apps/accounts/rows";
import { isOperatorOrganization, organizationName, selectableOrganizations } from "../../src/apps/email/organizations";
import { WORDS } from "../../src/apps/email/words";

// WHOSE EMAIL IT IS. Email names an organization in a handful of places and the
// cluster's own organization in one: this file pins the three small answers
// every one of them asks, and the one sentence for "this organization cannot
// send yet".

function account(over: Partial<AccountRow> & { id: string }): AccountRow {
  return {
    name: "",
    domain: "",
    primaryContactName: "",
    primaryContactEmail: "",
    notes: "",
    status: "active",
    configuredAt: "",
    ownerUserId: "",
    createdAt: "",
    domainToken: "",
    domainStatus: "",
    domainFailureReason: "",
    domainFailureDetail: "",
    domainLastCheckedAt: "",
    domainVerifiedAt: "",
    joinOnDomain: false,
    memqlDomain: "",
    memqlReservedAt: "",
    memqlReservationReason: "",
    ...over,
  };
}

describe("organizations", () => {
  const accounts = [
    account({ id: "self", name: "Operator organization" }),
    account({ id: "acme", name: "Acme" }),
    account({ id: "old", name: "Old client", status: "archived" }),
    account({ id: "", name: "Mid-write" }),
  ];

  it("offers the active organizations the person can read, never an archived one or a row with no id", () => {
    expect(selectableOrganizations(accounts).map((a) => a.id)).toEqual(["self", "acme"]);
    expect(selectableOrganizations([])).toEqual([]);
  });

  it("names an organization from the list the page already holds, and keeps an id it cannot resolve", () => {
    expect(organizationName(accounts, "acme")).toBe("Acme");
    // Archived is still a name: a sender tied to it is still tied to it.
    expect(organizationName(accounts, "old")).toBe("Old client");
    expect(organizationName(accounts, "elsewhere")).toBe("elsewhere");
    expect(organizationName(accounts, "")).toBe("");
  });

  it("spells the cluster's own organization in one place", () => {
    expect(isOperatorOrganization(SELF_ACCOUNT_ID)).toBe(true);
    expect(isOperatorOrganization("self")).toBe(true);
    expect(isOperatorOrganization("acme")).toBe(false);
    expect(isOperatorOrganization("")).toBe(false);
  });
});

describe("words", () => {
  it("says one sentence for sending that is not set up, naming the organization", () => {
    expect(WORDS.sendingNotSetUp("Acme")).toBe("Sending isn't set up for Acme.");
  });

  it("uses one word for a sender that has been put away", () => {
    expect(WORDS.archived).toBe("Archived");
    expect(WORDS.active).toBe("Active");
  });
});
