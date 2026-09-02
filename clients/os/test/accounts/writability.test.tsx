import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AccountDetail } from "../../src/apps/accounts/AccountDetail";
import { accountFromRow } from "../../src/apps/accounts/rows";
import { accountRow, withSession } from "./harness";

// WHO MAY EDIT WHICH ACCOUNT, in the panel (epic memql#4832, D2 / memql#4837).
//
// A row you may not write is now a row you can SEE -- rank-visible reads made
// that possible, and before them "cannot edit" and "cannot see" were the same
// state. So the panel has a new job: never offer a form the engine will refuse
// on save. These pin the three cases.
//
// PRESENTATION ONLY, as everything in this app is. The engine's write guard is
// the authority and refuses the same write whatever renders here; what is
// under test is whether somebody is handed a doomed Edit button.

function detail(row: ReturnType<typeof accountRow>, role: string) {
  const noop = { busy: false, error: "", update: async () => true, archive: async () => true };
  return render(
    withSession(
      <AccountDetail
        account={accountFromRow(row)}
        update={noop as never}
        archive={noop as never}
        onArchived={() => {}}
      />,
      { role },
    ),
  );
}

describe("who is offered the Edit button", () => {
  it("offers it on the viewer's OWN account", () => {
    detail(accountRow({ id: "acme", ownerUserId: "v1:identity:user:me" }), "admin");
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.queryByText(/Read-only/)).toBeNull();
  });

  it("withholds it on somebody else's account, and says why", () => {
    detail(accountRow({ id: "acme", ownerUserId: "v1:identity:user:ada" }), "developer");
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.getByText(/Read-only/)).toBeTruthy();
  });

  // THE CASE THAT IS EASY TO GET WRONG. An empty ownerUserId reads as
  // "nobody's, so everybody's" and the engine says the opposite:
  // sameRowAuthzOwner refuses an empty owner outright, so the clusterOwner
  // branch is the only one that admits the write -- and that branch is
  // `Role == RoleOwner` exactly, not a rank floor.
  it("withholds it on the CLUSTER-OWNED self account from an admin", () => {
    detail(accountRow({ id: "self", ownerUserId: "" }), "admin");
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.getByText(/Read-only/)).toBeTruthy();
  });

  it("offers it on the self account to a cluster owner", () => {
    detail(accountRow({ id: "self", ownerUserId: "" }), "owner");
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
  });

  // A developer OUTRANKS an admin under the one ladder, and it changes nothing
  // here: rank decides what you can SEE, ownership decides what you can write.
  // Conflating them is how a read widening becomes a write widening.
  it("does not let rank alone unlock a write", () => {
    detail(accountRow({ id: "acme", ownerUserId: "v1:identity:user:ada" }), "owner");
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });
});

describe("the self account's archive panel", () => {
  it("is replaced by the reason rather than disabled", () => {
    detail(accountRow({ id: "self", ownerUserId: "" }), "owner");
    expect(screen.queryByRole("button", { name: "Archive this client" })).toBeNull();
    expect(screen.getByText(/cannot be archived/)).toBeTruthy();
  });

  it("still offers archive on an ordinary client", () => {
    detail(accountRow({ id: "acme", ownerUserId: "v1:identity:user:me" }), "owner");
    expect(screen.getByRole("button", { name: "Archive this client" })).toBeTruthy();
  });
});
