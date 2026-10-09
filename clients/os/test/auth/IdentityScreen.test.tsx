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

afterEach(() => { cleanup(); vi.unstubAllGlobals(); localStorage.removeItem("memql-os-theme"); document.documentElement.removeAttribute("data-theme"); });

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
    expect(screen.getAllByRole("group", { name: "Color theme" })).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Continue to passkey" })).toBeNull();
    const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0]?.[0].pathname).toBe("/setup");
    expect(posts[0]?.[1]).toMatchObject({ credentials: "include", redirect: "error", headers: { "X-CSRF-Token": "setup-csrf" } });
    expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toMatchObject({ owner_first_name: "Ada", owner_email: "ada@example.test", brand_name: "Example" });
  });

  it("keeps theme selection through setup steps without losing the owner's draft", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json(setup)));
    render(<IdentityScreen initialPath="/setup" />);
    await screen.findByLabelText("First name");
    fireEvent.change(screen.getByLabelText("First name"), { target: { value: "Grace" } });
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(screen.getByRole("radio", { name: "Dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect((screen.getByLabelText("First name") as HTMLInputElement).value).toBe("Grace");
    fireEvent.click(screen.getByRole("radio", { name: "System" }));
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(localStorage.getItem("memql-os-theme")).toBe("system");
    expect(screen.getAllByRole("group", { name: "Color theme" })).toHaveLength(1);
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

it.each([true, false])("recovers a skipped passkey with the original owner email (local=%s)", async local => {
  const fetcher = vi.fn(async (input: URL, init?: RequestInit) => {
    if (init?.method === "POST") return json({ redirect: local ? "/auth/setup/passkey" : "/check-email?request=verify" });
    if (input.pathname === "/auth/setup/passkey") return json({ page: "setup_passkey", data: { Local: true, EnrollmentToken: "renewed" } });
    if (input.pathname === "/check-email") return json({ page: "check_email", data: { Email: "ada@example.test", ExpiresIn: "10 minutes" } });
    return json({ page: "setup_resume", csrf: "resume-csrf", data: { Local: local, HasProof: false, ClientID: "editor", CodeChallenge: "challenge", CodeChallengeMethod: "S256" } });
  });
  vi.stubGlobal("fetch", fetcher);
  render(<IdentityScreen initialPath="/setup" />);
  fireEvent.change(await screen.findByLabelText("Owner email"), { target: { value: "ada@example.test" } });
  fireEvent.click(screen.getByRole("button", { name: local ? "Continue to passkey" : "Send verification link" }));
  if (local) await screen.findByRole("button", { name: "Create passkey and finish" });
  else await screen.findByRole("heading", { name: "Check your email" });
  const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
  expect(posts).toHaveLength(1);
  expect(posts[0]?.[0].pathname).toBe("/auth/setup/resume");
  expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toMatchObject({ email: "ada@example.test", client_id: "editor", code_challenge: "challenge", code_challenge_method: "S256" });
});

it("keeps a local claim with attestation on its existing passkey", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => json({ page: "setup_resume", data: { Local: true, HasProof: true } })));
  render(<IdentityScreen initialPath="/setup" />);
  await screen.findByRole("button", { name: "Resume with your passkey" });
  expect(screen.queryByLabelText("Owner email")).toBeNull();
  expect(screen.queryByRole("button", { name: "Continue to passkey" })).toBeNull();
});

it("reaches unfinished owner setup from sign-in without losing the editor request", async () => {
  const fetcher = vi.fn(async (input: URL) => input.pathname === "/login"
    ? json({ page: "login", data: { Stage: "email", Local: true, CanResumeSetup: true, ClientID: "editor", OAuthState: "pending-state", CodeChallenge: "challenge", CodeChallengeMethod: "S256" } })
    : json({ page: "setup_resume", data: { Local: true } }));
  vi.stubGlobal("fetch", fetcher);
  render(<IdentityScreen initialPath="/login" />);
  fireEvent.click(await screen.findByRole("button", { name: "Finish ownership setup" }));
  await screen.findByLabelText("Owner email");
  const request = fetcher.mock.calls.find(([url]) => url.pathname === "/setup")?.[0];
  expect(request?.searchParams.get("client_id")).toBe("editor");
  expect(request?.searchParams.get("state")).toBe("pending-state");
  expect(request?.searchParams.get("code_challenge")).toBe("challenge");
});
