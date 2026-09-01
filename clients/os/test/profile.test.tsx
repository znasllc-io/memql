import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { accessFromSummary } from "../src/modules/profile/useResolvedAccess";
import { resetIdsForTest } from "../src/system/desks";
import { LocalDesktopStore } from "../src/system/store";
import type { OsRuntimeConfig } from "../src/cluster/config";

// The Profile MODULE is gone (spec A0): its MyAccess facts now surface in
// Settings -> About and the avatar menu. The parse layer stays -- the Shell
// reads it from the cluster stream (memql#4775).

const ACCESS = {
  userId: "v1:identity:user:u-42",
  primaryEmail: "ada@example.test",
  clusterRole: "owner",
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

  it("reads MyAccess data only -- user id, primaryEmail, clusterRole", () => {
    expect(accessFromSummary(summary)).toEqual(ACCESS);
  });

  it("KEEPS THE ROLE when there is no email", () => {
    // The parser this replaced required all three fields and returned null if
    // any was blank -- so a credential with no address (a PAT, an operator
    // key, a service account: the SDK's own type says they exist) erased a
    // perfectly good role and the shell rendered "You are unknown" to an
    // owner. An email is what Diagnostics prints, not what any decision reads.
    const noEmail = { requestId: "r", sessionId: "", userId: "u-1", primaryEmail: "", clusterRole: "owner" } as never;
    expect(accessFromSummary(noEmail)).toEqual({
      userId: "u-1",
      primaryEmail: "",
      clusterRole: "owner",
    });
  });

  it("keeps a user id even when the role is blank, and admits nothing gated", () => {
    // Fail-closed rather than null: "" ranks nowhere, so no gated surface
    // opens -- but the user id survives, and owner-scoped client filters
    // depend on it.
    const noRole = { requestId: "r", sessionId: "", userId: "u-1", primaryEmail: "a@b.c", clusterRole: "" } as never;
    expect(accessFromSummary(noRole)?.userId).toBe("u-1");
    expect(accessFromSummary(noRole)?.clusterRole).toBe("");
  });

  it("is null only when there is nothing usable", () => {
    expect(accessFromSummary(null)).toBeNull();
    const empty = { requestId: "r", sessionId: "", userId: "", primaryEmail: "a@b.c", clusterRole: "" } as never;
    expect(accessFromSummary(empty)).toBeNull();
  });
});

describe("the access facts surface in chrome", () => {
  function memStorage(): Pick<Storage, "getItem" | "setItem"> {
    const data = new Map<string, string>();
    return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
  }

  it("Settings -> About names the signed-in identity and role", () => {
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
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name: "Settings" }),
    );
    expect(screen.getByText("ada@example.test")).toBeTruthy();
    expect(screen.getByText("owner")).toBeTruthy();
    expect(screen.getByText("example.test")).toBeTruthy();
  });

  it("the avatar menu carries the email and the sign out", () => {
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
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    const menu = screen.getByRole("menu", { name: "Account" });
    expect(within(menu).getByText("ada@example.test")).toBeTruthy();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledTimes(1);
  });
});
