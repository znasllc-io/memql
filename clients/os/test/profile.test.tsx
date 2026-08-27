import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { parseProfileAccess } from "../src/modules/profile/access";
import { resetIdsForTest } from "../src/system/desks";
import { LocalDesktopStore } from "../src/system/store";
import type { OsRuntimeConfig } from "../src/cluster/config";

// The Profile MODULE is gone (spec A0): its MyAccess facts now surface in
// Settings -> About and the avatar menu. The parse layer stays -- App.tsx
// still reads it at boot.

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

describe("parseProfileAccess", () => {
  it("reads MyAccess data only -- user id, primaryEmail, clusterRole", () => {
    expect(parseProfileAccess(ACCESS)).toEqual(ACCESS);
  });

  it("rejects a payload that is not the identity shape", () => {
    expect(parseProfileAccess(null)).toBeNull();
    expect(parseProfileAccess({ email: "nope" })).toBeNull();
    expect(parseProfileAccess({ primaryEmail: "a@b.c" })).toBeNull();
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
