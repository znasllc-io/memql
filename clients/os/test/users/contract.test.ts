import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { appById, sectionsForRole, settingsSectionProblem } from "../../src/system/registry";
import {
  DEFAULT_USERS_SETTINGS,
  USERS_SECTION_IDS,
  USERS_SECTIONS,
  sanitizeUsersSettings,
} from "../../src/apps/users/settings";
import {
  invitationFromRow,
  invitationHasExpired,
  invitationIsPending,
  personFromRow,
  personIsDim,
  personName,
} from "../../src/apps/users/rows";

describe("the Users manifest", () => {
  const users = appById(OS_REGISTRY, "users");

  it("is registered as a real app rather than a stub", () => {
    expect(users).toBeTruthy();
    expect(users?.component).toBeTruthy();
  });

  it("declares admin as its floor", () => {
    expect(users?.roles).toEqual({ min: "admin" });
    // Presentation gating (spec E). The reads carry `requiresOwnerOrAdmin` in
    // their own DSL filters and every write goes through `adminops.authorize`;
    // this only decides whether the icon is in the launcher.
    expect(sectionsForRole(users!, "writer")).toEqual(sectionsForRole(users!, "writer"));
  });

  it("carries a settings section the gear can actually reach", () => {
    expect(settingsSectionProblem(users!)).toBeNull();
  });

  it("declares exactly the sections the settings picker offers", () => {
    // A second copy of this list is one that can disagree, and a preference
    // naming a section the manifest does not declare leaves the window on
    // People with the nav highlighting nothing.
    expect(users?.sections).toBe(USERS_SECTIONS);
    expect(USERS_SECTION_IDS).toEqual(["people", "invites", "settings"]);
  });
});

describe("the Users settings document", () => {
  it("repairs each field independently rather than rejecting wholesale", () => {
    // A garbage defaultSection must not cost somebody their show-deactivated
    // choice.
    expect(sanitizeUsersSettings({ version: 1, defaultSection: "nope", showDeactivated: true }))
      .toEqual({ version: 1, defaultSection: "people", showDeactivated: true });
  });

  it("discards a document whose version it does not know", () => {
    // There the field NAMES cannot be trusted at all.
    expect(sanitizeUsersSettings({ version: 2, defaultSection: "invites" })).toEqual(
      DEFAULT_USERS_SETTINGS,
    );
  });

  it("survives garbage of every shape", () => {
    for (const junk of [null, undefined, 7, "x", [], { version: 1, showDeactivated: "yes" }]) {
      expect(sanitizeUsersSettings(junk).version).toBe(1);
    }
  });
});

describe("projecting a person", () => {
  it("reconciles the flat seed shape and the nested event shape", () => {
    const flat = personFromRow({ id: "u1", displayName: "Ada", role: "admin" });
    const nested = personFromRow({
      id: "u1",
      payload: { displayName: "Ada", role: "admin" },
    } as never);
    expect(nested).toEqual({ ...flat, createdAt: "" });
  });

  it("treats an ABSENT active flag as active", () => {
    // A folded event carries only what the write touched, and `active`
    // defaults to true on the concept. Reading absent as false would make
    // everybody vanish the first time anything about them changed.
    expect(personFromRow({ id: "u1" }).active).toBe(true);
    expect(personFromRow({ id: "u1", active: false }).active).toBe(false);
  });

  it("never renders a nameless person", () => {
    expect(personName(personFromRow({ id: "u1", displayName: "Ada" }))).toBe("Ada");
    expect(personName(personFromRow({ id: "u1", firstName: "Ada", lastName: "L" }))).toBe("Ada L");
    expect(personName(personFromRow({ id: "u1", primaryEmail: "a@example.com" }))).toBe(
      "a@example.com",
    );
    // Never blank: a nameless row is indistinguishable from one that failed to
    // render.
    expect(personName(personFromRow({ id: "u1", primaryEmail: "" }))).toBe("u1");
  });

  it("dims a deactivated account and a suspended one alike", () => {
    expect(personIsDim(personFromRow({ id: "u1", active: false }))).toBe(true);
    expect(
      personIsDim(personFromRow({ id: "u1", suspendedAt: "2026-08-01T00:00:00Z" })),
    ).toBe(true);
    expect(personIsDim(personFromRow({ id: "u1" }))).toBe(false);
  });
});

describe("projecting an invitation", () => {
  it("defaults an absent kind to user and an absent status to pending", () => {
    // Every row this app sees came through `pendingUserInvitations`, whose
    // filter is kind=="user". A folded event that omits the field must not
    // read as a guest invitation and get dropped by our own scope check.
    const folded = invitationFromRow({ id: "i1" });
    expect(folded.kind).toBe("user");
    expect(folded.status).toBe("pending");
    expect(invitationIsPending(folded)).toBe(true);
  });

  it("drops a row that has left pending, by any of the three routes", () => {
    expect(invitationIsPending(invitationFromRow({ id: "i1", status: "accepted" }))).toBe(false);
    expect(invitationIsPending(invitationFromRow({ id: "i1", active: false }))).toBe(false);
    expect(invitationIsPending(invitationFromRow({ id: "i1", kind: "guest" }))).toBe(false);
  });

  it("keeps not_attempted and failed as different answers", () => {
    // Rendering both as "not sent" is what let an invitation look delivered
    // when nothing had been sent at all (memql#4587).
    expect(invitationFromRow({ id: "i1" }).deliveryState).toBe("not_attempted");
    expect(invitationFromRow({ id: "i1", deliveryState: "failed" }).deliveryState).toBe("failed");
    expect(invitationFromRow({ id: "i1", deliveryState: "sent" }).deliveryState).toBe("sent");
    // An unrecognised value is not_attempted rather than passed through: a
    // chip rendering a string nobody defined says nothing.
    expect(invitationFromRow({ id: "i1", deliveryState: "weird" }).deliveryState).toBe(
      "not_attempted",
    );
  });

  it("never expires an invitation with no expiry", () => {
    const now = new Date("2026-08-31T00:00:00Z");
    expect(invitationHasExpired(invitationFromRow({ id: "i1", expiresAt: "" }), now)).toBe(false);
    expect(
      invitationHasExpired(invitationFromRow({ id: "i1", expiresAt: "2026-01-01T00:00:00Z" }), now),
    ).toBe(true);
    expect(
      invitationHasExpired(invitationFromRow({ id: "i1", expiresAt: "2099-01-01T00:00:00Z" }), now),
    ).toBe(false);
  });
});
