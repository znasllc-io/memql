import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { SignIn } from "../../src/chrome/SignIn";
import { installSharedWebLocks } from "./webLocks";

afterEach(cleanup);

describe("sign-in browser requirements", () => {
  it("explains an unsupported browser instead of offering unsafe concurrent refresh", () => {
    Object.defineProperty(navigator, "locks", { configurable: true, value: undefined });
    render(<SignIn status="unavailable" onSignIn={async () => {}} />);
    expect(screen.getByText(/Update your browser/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
  });

  it("opens sign-in directly when the browser can coordinate tabs", () => {
    installSharedWebLocks();
    const start = vi.fn(async () => {});
    render(<SignIn status="signed-out" onSignIn={start} />);
    expect(start).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
  });
});
