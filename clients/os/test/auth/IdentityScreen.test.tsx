import { StrictMode } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { IdentityScreen } from "../../src/auth/IdentityScreen";

// Match AuthProvider's memoized dependencies. Keep nativeIdentity real so these
// tests exercise the POST body, CSRF header, JSON errors, and redirect fetch.
vi.mock("../../src/auth/AuthProvider", () => {
  const value = {
    config: { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os", authEnabled: true, domain: "example.test" },
    authSource: { bearer: async () => null },
    status: "unclaimed",
  };
  return { useAuth: () => value };
});

const setup = {
  page: "setup_wizard", csrf: "setup-csrf",
  data: { Local: true, PrefillDomain: "example.test", PrefillOwnerFirstName: "Ada", PrefillOwnerLastName: "Owner", PrefillOwnerEmail: "ada@example.test", PrefillOrgName: "Example" },
};
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

async function reachSubmission() {
  await screen.findByRole("button", { name: "Continue" });
  for (let i = 0; i < 3; i++) fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  return screen.getByRole("button", { name: "Continue to passkey" });
}

describe("ownership setup through IdentityScreen and native transport", () => {
  it("submits once and follows the server redirect to passkey setup, including StrictMode", async () => {
    const fetcher = vi.fn(async (input: URL, init?: RequestInit) => {
      if (init?.method === "POST") return json({ redirect: "/auth/setup/passkey" });
      if (input.pathname === "/auth/setup/passkey") return json({ page: "setup_passkey", csrf: "passkey-csrf", data: { Local: true, EnrollmentToken: "enrollment" } });
      return json(setup);
    });
    vi.stubGlobal("fetch", fetcher);
    render(<StrictMode><IdentityScreen initialPath="/setup" /></StrictMode>);
    fireEvent.click(await reachSubmission());
    expect(await screen.findByRole("button", { name: "Create passkey and finish" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Continue to passkey" })).toBeNull();
    const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0]?.[0].pathname).toBe("/setup");
    expect(posts[0]?.[1]).toMatchObject({ credentials: "include", redirect: "error", headers: { "X-CSRF-Token": "setup-csrf" } });
    expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toMatchObject({ owner_first_name: "Ada", owner_email: "ada@example.test", brand_name: "Example" });
  });

  it.each(["validation", "network"])("preserves entered fields and final step after a %s refusal, and permits retry", async kind => {
    const message = kind === "validation" ? "Organization name is required and must be at most 200 characters." : "Failed to fetch";
    let attempts = 0;
    const fetcher = vi.fn(async (_input: URL, init?: RequestInit) => {
      if (init?.method !== "POST") return json(setup);
      attempts++;
      if (attempts > 1) return json({ page: "setup_passkey", data: { Local: true, EnrollmentToken: "enrollment" } });
      if (kind === "network") throw new TypeError(message);
      return json({ page: "error", data: { Message: message } }, 400);
    });
    vi.stubGlobal("fetch", fetcher);
    render(<IdentityScreen initialPath="/setup" />);
    await screen.findByLabelText("First name");
    fireEvent.change(screen.getByLabelText("First name"), { target: { value: "Grace" } });
    fireEvent.click(await reachSubmission());
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(screen.getByRole("button", { name: "Continue to passkey" })).toBeTruthy();
    expect(screen.getByText("Grace Owner")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Continue to passkey" }));
    await screen.findByRole("button", { name: "Create passkey and finish" });
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(attempts).toBe(2);
  });
});
