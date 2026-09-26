import { StrictMode } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "../../src/chrome/SignIn";
import { canCoordinateIdentityRefresh } from "../../src/auth/identityClient";

vi.mock("../../src/auth/identityClient", () => ({ canCoordinateIdentityRefresh: vi.fn() }));
beforeEach(() => { vi.mocked(canCoordinateIdentityRefresh).mockReturnValue(true); });
afterEach(cleanup);

describe("signed-out entry", () => {
  it("opens the existing authorization flow once without an intermediate sign-in button", () => {
    const start = vi.fn(async () => {});
    render(<StrictMode><SignIn status="signed-out" onSignIn={start} /></StrictMode>);
    expect(start).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("status").textContent).toBe("Opening sign-in…");
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
  });
  it("keeps an unavailable identity service retryable without an authorization loop", () => {
    const start = vi.fn(async () => {});
    render(<SignIn status="unavailable" onSignIn={start} />);
    expect(start).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("Reconnect");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
  it("keeps unsupported browsers out of the refresh flow", () => {
    vi.mocked(canCoordinateIdentityRefresh).mockReturnValue(false);
    const start = vi.fn(async () => {});
    render(<SignIn status="signed-out" onSignIn={start} />);
    expect(start).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("Web Locks");
  });
  it("offers recovery when preparing authorization fails", async () => {
    const start = vi.fn().mockRejectedValue(new Error("storage unavailable"));
    render(<SignIn status="signed-out" onSignIn={start} />);
    expect((await screen.findByRole("alert")).textContent).toContain("couldn’t open sign-in");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(start).toHaveBeenCalledTimes(1);
  });
});
