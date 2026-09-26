import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { IdentityAccount } from "../../src/apps/identity/IdentityAccount";

const tokenData = { Tokens: [{ ID: "a", Label: "Automation", Active: true }, { ID: "b", Label: "Retired", Active: false }], NextCursor: "next page" };
const acts = () => ({ submit: vi.fn(), navigate: vi.fn(), register: vi.fn() });

describe("identity record lists", () => {
  it("counts the loaded page, keeps revoke outside row buttons, and retains pagination", () => {
    const actions = acts();
    render(<IdentityAccount page="me/tokens" data={tokenData} busy={false} {...actions} />);
    const list = screen.getByRole("list", { name: "Tokens" });
    expect(list.classList.contains("os-record-list")).toBe(true);
    expect(list.querySelectorAll(".os-record-row")).toHaveLength(2);
    expect(screen.getByRole("heading", { name: /^Tokens/ }).querySelector(".os-subhead-meta")?.textContent).toBe("2");
    const revoke = screen.getByRole("button", { name: "Revoke token" });
    expect(revoke.closest(".os-record-actions")).not.toBeNull();
    fireEvent.click(revoke);
    expect(actions.submit).toHaveBeenCalledWith("/me/tokens/revoke", { id: "a" });
    fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(actions.navigate).toHaveBeenCalledWith("/me/tokens?cursor=next%20page");
  });

  it("does not claim zero before data, during a refresh, or after a refusal", () => {
    const actions = acts();
    const view = render(<IdentityAccount page="me/tokens" data={{}} busy={false} {...actions} />);
    expect(document.querySelector(".os-subhead-meta")).toBeNull();
    view.rerender(<IdentityAccount page="me/tokens" data={tokenData} busy {...actions} />);
    expect(document.querySelector(".os-subhead-meta")).toBeNull();
    view.rerender(<IdentityAccount page="me/tokens" data={tokenData} busy={false} error="forbidden" {...actions} />);
    expect(document.querySelector(".os-subhead-meta")).toBeNull();
    view.rerender(<IdentityAccount page="me/tokens" data={{ Tokens: [] }} busy={false} {...actions} />);
    expect(document.querySelector(".os-subhead-meta")?.textContent).toBe("0");
  });

  it("preserves passkey rename and session revocation with separate truthful counts", () => {
    const actions = acts();
    render(<IdentityAccount page="me/devices" busy={false} {...actions} data={{ PasskeysEnabled: true, Passkeys: [{ ID: "key", Label: "Laptop", Active: true }], SessionsAvailable: true, Sessions: [{ ID: "session", Label: "Browser", ThisDevice: true }] }} />);
    expect(screen.getByRole("list", { name: "Passkeys" }).querySelectorAll(".os-record-row")).toHaveLength(1);
    expect(screen.getByRole("list", { name: "Sessions" }).querySelectorAll(".os-record-row")).toHaveLength(1);
    fireEvent.change(screen.getByLabelText("Name for Laptop"), { target: { value: "Work laptop" } });
    fireEvent.click(screen.getByRole("button", { name: "Save name" }));
    expect(actions.submit).toHaveBeenCalledWith("/me/devices/passkeys/rename", { id: "key", label: "Work laptop" });
    fireEvent.click(screen.getByRole("button", { name: "Sign out here" }));
    expect(actions.submit).toHaveBeenCalledWith("/me/devices/sessions/revoke", { id: "session" });
  });
});
