import { act } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { IdentityScreen } from "../../src/auth/IdentityScreen";
import { signInProblem } from "../../src/auth/SignInPage";
import { loginWithPasskey } from "../../src/auth/passkeys";

vi.mock("../../src/auth/AuthProvider", () => {
  const auth = { config: { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os", authEnabled: true, domain: "example.test" }, authSource: { bearer: async () => null }, status: "signed-out" };
  return { useAuth: () => auth };
});
vi.mock("../../src/auth/passkeys", () => ({ loginWithPasskey: vi.fn(), registerPasskey: vi.fn() }));
const data = () => ({ Local: true, Stage: "email", AuthorizeMode: true, ClientID: "os", ClientName: "os", RedirectURI: `${window.location.origin}/auth/callback`, OAuthState: "opaque-state", CodeChallenge: "pkce", CodeChallengeMethod: "S256" });
function load(overrides = {}) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ page: "login", data: { ...data(), ...overrides }, csrf: "csrf" }))));
  render(<IdentityScreen initialPath="/authorize" />);
  return screen.findByRole("button", { name: "Sign in with a passkey" });
}
beforeEach(() => { vi.mocked(loginWithPasskey).mockReset(); });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("native OS sign-in", () => {
  it("keeps local sign-in passkey-only and removes redundant internal OAuth details", async () => {
    await load();
    expect(screen.getByRole("heading", { level: 1, name: "Sign in to MemQL OS" })).toBeTruthy();
    expect(screen.queryByLabelText("Email")).toBeNull();
    expect(screen.queryByText(/Return address:|Client:|Continue to os/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Return to MemQL OS" })).toBeNull();
    expect(screen.getByRole("button", { name: "Terms of Service" })).toBeTruthy();
  });
  it.each([
    [new Error("webauthn: no passkey matches the asserted credential"), "This passkey couldn’t be recognized."],
    [new DOMException("browser diagnostic", "NotAllowedError"), "Sign-in wasn’t completed."],
    [new DOMException("browser diagnostic", "AbortError"), "Sign-in wasn’t completed."],
    [new Error("private implementation response"), "Sign-in couldn’t be completed."],
  ])("presents a safe, retryable failure for %s", async (error, expected) => {
    vi.mocked(loginWithPasskey).mockRejectedValue(error);
    fireEvent.click(await load());
    expect((await screen.findByRole("alert")).textContent).toContain(expected);
    expect(screen.queryByText(error.message)).toBeNull();
    expect(screen.getByRole("button", { name: "Try passkey again" }).hasAttribute("disabled")).toBe(false);
    expect(loginWithPasskey).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ client_id: "os", redirect_uri: data().RedirectURI, state: "opaque-state", code_challenge: "pkce", code_challenge_method: "S256" }));
  });
  it("submits a passkey ceremony once, exposes pending state, and permits a fresh retry", async () => {
    let reject!: (error: Error) => void;
    vi.mocked(loginWithPasskey).mockImplementation(() => new Promise((_, fail) => { reject = fail; }));
    const button = await load();
    act(() => { button.click(); button.click(); });
    expect(loginWithPasskey).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Waiting for your passkey…" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("status").textContent).toContain("browser’s prompt");
    await act(async () => reject(new Error("No passkey was selected")));
    fireEvent.click(screen.getByRole("button", { name: "Try passkey again" }));
    expect(loginWithPasskey).toHaveBeenCalledTimes(2);
    await act(async () => reject(new Error("No passkey was selected")));
  });
  it("retains the unverified app and destination disclosure even when it calls itself os", async () => {
    await load({ ClientSelfRegistered: true, ClientName: "os" });
    expect(screen.getByText(data().RedirectURI)).toBeTruthy();
    expect(screen.getByText(/name has not been verified/)).toBeTruthy();
  });
  it("retains hosted email submission, CSRF and the OAuth handoff", async () => {
    await load({ Local: false });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "owner@example.test" } });
    fireEvent.click(screen.getByRole("button", { name: "Send sign-in link" }));
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([, init]) => init?.method === "POST")).toBe(true));
    const request = vi.mocked(fetch).mock.calls.find(([, init]) => init?.method === "POST")!;
    expect(request[1]?.headers).toMatchObject({ "X-CSRF-Token": "csrf" });
    expect(Object.fromEntries(request[1]?.body as URLSearchParams)).toMatchObject({ email: "owner@example.test", form: "email", client_id: "os", state: "opaque-state", code_challenge: "pkce" });
  });
  it("keeps legal navigation on the existing identity transport", async () => {
    await load();
    fireEvent.click(screen.getByRole("button", { name: "Privacy Notice" }));
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).endsWith("/legal/privacy"))).toBe(true));
  });
  it.each([
    ["waitlist_signup", "Request access", "waitlist", "Your name", "name"],
    ["needs_invite", "Use invitation", "invite", "Invitation token", "invitation"],
  ])("preserves the hosted %s form", async (stage, action, form, label, field) => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ page: "login", data: { ...data(), Local: false, Stage: stage }, csrf: "csrf" }))));
    render(<IdentityScreen initialPath="/login" />);
    const button = await screen.findByRole("button", { name: action });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "owner@example.test" } });
    fireEvent.change(screen.getByLabelText(label), { target: { value: "provided-value" } });
    expect(screen.queryByRole("button", { name: "Sign in with a passkey" })).toBeNull();
    fireEvent.click(button);
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([, init]) => init?.method === "POST")).toBe(true));
    const request = vi.mocked(fetch).mock.calls.find(([, init]) => init?.method === "POST")!;
    expect(Object.fromEntries(request[1]?.body as URLSearchParams)).toMatchObject({ form, [field]: "provided-value", email: "owner@example.test", state: "opaque-state" });
  });
  it("explains unsupported browsers without exposing protocol diagnostics", () => {
    expect(signInProblem(new Error("Use a current browser with passkey support to sign in.")).message).toBe("This browser can’t use passkeys here.");
  });
});
