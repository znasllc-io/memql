import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { parseProfileAccess } from "../src/modules/profile/access";

const ACCESS = {
  userId: "v1:identity:user:u-42",
  primaryEmail: "ada@example.test",
  clusterRole: "owner",
};

describe("parseProfileAccess", () => {
  it("reads MyAccess data only — user id, primaryEmail, clusterRole", () => {
    expect(parseProfileAccess(ACCESS)).toEqual(ACCESS);
  });

  it("rejects a payload that is not the identity shape", () => {
    expect(parseProfileAccess(null)).toBeNull();
    expect(parseProfileAccess({ email: "nope" })).toBeNull();
    expect(parseProfileAccess({ primaryEmail: "a@b.c" })).toBeNull();
  });
});

describe("Profile module", () => {
  it("occupies a slot on desktop when opened from the launcher", () => {
    render(<Shell layout="desktop" onSignOut={vi.fn()} access={ACCESS} />);
    expect(document.querySelector("[data-os-module='profile']")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Profile" }));
    const slot = document.querySelector("[data-os-slot='a']");
    expect(slot?.querySelector("[data-os-module='profile']")).toBeTruthy();
    expect(screen.getByText("ada@example.test")).toBeTruthy();
    expect(screen.getByText("owner")).toBeTruthy();
    expect(screen.getByText("v1:identity:user:u-42")).toBeTruthy();
  });

  it("occupies a slot on iPad", () => {
    render(<Shell layout="ipad" onSignOut={vi.fn()} access={ACCESS} />);
    fireEvent.click(screen.getByRole("button", { name: "Profile" }));
    expect(document.querySelector("[data-os-slot] [data-os-module='profile']")).toBeTruthy();
  });

  it("is the phone allowlist (not a slot) with the research sheet", () => {
    render(<Shell layout="phone" onSignOut={vi.fn()} access={ACCESS} />);
    expect(document.querySelector("[data-os-slot]")).toBeNull();
    expect(document.querySelector("[data-os-module='profile']")).toBeTruthy();
    expect(document.querySelector("[data-os-research]")?.getAttribute("data-os-host")).toBe(
      "sheet",
    );
    expect(screen.getByText("ada@example.test")).toBeTruthy();
  });

  it("keeps sign out in chrome after Profile occupies a slot", () => {
    const onSignOut = vi.fn();
    render(<Shell layout="desktop" onSignOut={onSignOut} access={ACCESS} />);
    fireEvent.click(screen.getByRole("button", { name: "Profile" }));
    const chromeSignOut = document.querySelector(".os-chrome-actions [data-sign-out]");
    expect(chromeSignOut).toBeTruthy();
    expect(document.querySelector("[data-os-module='profile'] [data-sign-out]")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledOnce();
  });
});
