import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DisconnectGitHub } from "../../src/apps/deployables/sources/ConnectedAccountCard";

const props = () => ({ busy: false, refusal: null, onDisconnect: vi.fn() });

describe("compact GitHub disconnect", () => {
  it("places a named, focusable icon beside the connected identity and requires confirmation", () => {
    const actions = props();
    render(<DisconnectGitHub compact summary={<span>Connected as @octocat</span>} {...actions} />);
    const icon = screen.getByRole("button", { name: "Disconnect GitHub" });
    expect(icon.classList.contains("os-icon-button")).toBe(true);
    expect(icon.title).toBe("Disconnect GitHub");
    expect(icon.textContent).toBe("");
    expect(icon.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    expect(icon.parentElement).toBe(screen.getByText("Connected as @octocat").parentElement);
    icon.focus();
    expect(document.activeElement).toBe(icon);
    fireEvent.click(icon);
    expect(actions.onDisconnect).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    const confirm = screen.getByRole("button", { name: "Disconnect" });
    expect(confirm.getAttribute("data-tone")).toBe("danger");
    fireEvent.click(confirm);
    expect(actions.onDisconnect).toHaveBeenCalledOnce();
  });

  it("returns keyboard focus to the icon when confirmation is canceled", () => {
    const actions = props();
    render(<DisconnectGitHub compact {...actions} />);
    fireEvent.click(screen.getByRole("button", { name: "Disconnect GitHub" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Disconnect GitHub" }));
    expect(actions.onDisconnect).not.toHaveBeenCalled();
  });

  it("disables the icon and both confirming controls while a disconnect is pending", () => {
    const actions = props();
    const view = render(<DisconnectGitHub compact {...actions} busy />);
    const icon = screen.getByRole("button", { name: "Disconnect GitHub" }) as HTMLButtonElement;
    expect(icon.disabled).toBe(true);
    expect(icon.getAttribute("aria-busy")).toBe("true");
    fireEvent.click(icon);
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    view.rerender(<DisconnectGitHub compact {...actions} />);
    fireEvent.click(screen.getByRole("button", { name: "Disconnect GitHub" }));
    view.rerender(<DisconnectGitHub compact {...actions} busy />);
    expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
    const confirm = screen.getByRole("button", { name: "Disconnect" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    expect(confirm.getAttribute("aria-busy")).toBe("true");
  });

  it("keeps the Settings word button and the server's refusal", () => {
    render(<DisconnectGitHub {...props()} refusal={{ code: "forbidden", message: "This credential belongs to another person." }} />);
    expect(screen.getByRole("button", { name: "Disconnect" }).classList.contains("os-button")).toBe(true);
    expect(screen.queryByRole("button", { name: "Disconnect GitHub" })).toBeNull();
    expect(screen.getByText("This credential belongs to another person.")).toBeTruthy();
  });
});
