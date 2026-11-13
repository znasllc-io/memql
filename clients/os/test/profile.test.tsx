import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { accessFromSummary } from "../src/modules/profile/useResolvedAccess";
import { resetIdsForTest } from "../src/system/desks";
import { LocalDesktopStore } from "../src/system/store";
import type { OsRuntimeConfig } from "../src/cluster/config";
import { appTileName } from "./appTile";

// Profile is a personal app reached directly from the shell picture. Its
// identity facts still come from the same authenticated cluster stream.

const ACCESS = {
  userId: "v1:identity:user:u-42",
  primaryEmail: "ada@example.test",
  role: "owner",
  roleName: "Owner",
  rank: 400,
};

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};

describe("accessFromSummary", () => {
  const summary = { requestId: "r", sessionId: "s", ...ACCESS } as never;

  it("reads MyAccess data only -- user id, primaryEmail, role, groups", () => {
    // `groups` rides through unchanged (epic memql#5165, section H): an empty
    // list is "not reported" as well as "none", and this layer must not turn
    // either into a claim of its own.
    expect(accessFromSummary(summary)).toEqual({ ...ACCESS, groups: [], accountIds: [], everyAccount: false });
  });

  it("KEEPS THE ROLE when there is no email", () => {
    // The parser this replaced required all three fields and returned null if
    // any was blank -- so a credential with no address (a PAT, an operator
    // key, a service account: the SDK's own type says they exist) erased a
    // perfectly good role and the shell rendered "You are unknown" to an
    // owner. An email is what Diagnostics prints, not what any decision reads.
    const noEmail = {
      requestId: "r", sessionId: "", userId: "u-1", primaryEmail: "",
      role: "owner", roleName: "Owner", rank: 400,
    } as never;
    expect(accessFromSummary(noEmail)).toEqual({
      userId: "u-1",
      primaryEmail: "",
      role: "owner",
      roleName: "Owner",
      rank: 400,
      groups: [],
      accountIds: [],
      everyAccount: false,
    });
  });

  it("KEEPS THE SLUG when the cluster could not name or rank it", () => {
    // A role deactivated under its holder, or a node whose catalog has not
    // loaded: the engine sends the slug it has and leaves role_name empty and
    // rank 0 (epic memql#5166). Narrowing on the name here would throw away a
    // perfectly good role for the second time in this function's history --
    // the parser it replaced did exactly that with the email.
    const unnamed = {
      requestId: "r", sessionId: "", userId: "u-1", primaryEmail: "a@b.c",
      role: "retired-lead",
    } as never;
    expect(accessFromSummary(unnamed)).toEqual({
      userId: "u-1",
      primaryEmail: "a@b.c",
      role: "retired-lead",
      roleName: "",
      rank: 0,
      groups: [],
      accountIds: [],
      everyAccount: false,
    });
  });

  it("keeps a user id even when the role is blank, and admits nothing gated", () => {
    // Fail-closed rather than null: "" ranks nowhere, so no gated surface
    // opens -- but the user id survives, and owner-scoped client filters
    // depend on it.
    const noRole = { requestId: "r", sessionId: "", userId: "u-1", primaryEmail: "a@b.c", role: "" } as never;
    expect(accessFromSummary(noRole)?.userId).toBe("u-1");
    expect(accessFromSummary(noRole)?.role).toBe("");
  });

  it("is null only when there is nothing usable", () => {
    expect(accessFromSummary(null)).toBeNull();
    const empty = { requestId: "r", sessionId: "", userId: "", primaryEmail: "a@b.c", role: "" } as never;
    expect(accessFromSummary(empty)).toBeNull();
  });
});

describe("the access facts surface in chrome", () => {
  function memStorage(): Pick<Storage, "getItem" | "setItem"> {
    const data = new Map<string, string>();
    return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
  }

  it("Profile -> About names the signed-in identity and role", async () => {
    resetIdsForTest();
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={ACCESS}
        config={CONFIG}
        ports={{ store: new LocalDesktopStore(memStorage()), disableConnection: true }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    const profile = await screen.findByRole("dialog", { name: "Profile" });
    fireEvent.click(within(profile).getByRole("button", { name: "About" }));
    expect(await screen.findByText("ada@example.test")).toBeTruthy();
    expect(screen.getByText("owner")).toBeTruthy();
    expect(screen.getByText("example.test")).toBeTruthy();
  });

  it("Profile retains the existing sign-out action", async () => {
    resetIdsForTest();
    const onSignOut = vi.fn();
    render(
      <Shell
        layout="desktop"
        onSignOut={onSignOut}
        access={ACCESS}
        config={CONFIG}
        ports={{ store: new LocalDesktopStore(memStorage()), disableConnection: true }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    const profile = await screen.findByRole("dialog", { name: "Profile" });
    fireEvent.click(await within(profile).findByRole("button", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledTimes(1);
  });

  it("the picture opens one Profile directly, outside the Launcher", async () => {
    resetIdsForTest();
    render(
      <Shell
        layout="desktop"
        onSignOut={vi.fn()}
        access={ACCESS}
        config={CONFIG}
        ports={{ store: new LocalDesktopStore(memStorage()), disableConnection: true }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = screen.getByRole("dialog", { name: "Launcher" });
    expect(within(launcher).queryByRole("button", { name: appTileName("Profile") })).toBeNull();
    fireEvent.keyDown(launcher, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    expect(screen.queryByRole("menu", { name: "Account" })).toBeNull();
    await screen.findByRole("dialog", { name: "Profile" });
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    expect(screen.getAllByRole("dialog", { name: "Profile" })).toHaveLength(1);
    // No connection in this harness, so the page holds its reading state --
    // which is the Profile section, not the identity service's page.
    expect(await screen.findByText("Reading your profile")).toBeTruthy();
  });
});
