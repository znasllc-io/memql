import { StrictMode } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { IdentityScreen } from "../../src/auth/IdentityScreen";
import { readTheBarNow } from "../readTheBar";

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

// The setup wizard's Continue and Back move it to another step, and a bar that
// has just changed takes no press for a moment (kit ActionBar): each step is
// read before the next press, on a fake Date so reading costs nothing.
beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); });
afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); sessionStorage.clear(); localStorage.removeItem("memql-os-theme"); document.documentElement.removeAttribute("data-theme"); });

/** Presses an act of the wizard's bar once it has been read. */
const pressBar = (name: string) => { readTheBarNow(); fireEvent.click(screen.getByRole("button", { name })); };

async function reachSubmission() {
  await screen.findByRole("button", { name: "Continue" });
  for (let i = 0; i < 3; i++) pressBar("Continue");
  readTheBarNow();
  return screen.getByRole("button", { name: "Create passkey" });
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
    expect(await screen.findByRole("heading", { name: "Register owner passkey" })).toBeTruthy();
    expect(screen.getAllByRole("group", { name: "Color theme" })).toHaveLength(1);
    // The wizard's act is gone: the passkey page carries the one act left.
    expect(screen.getAllByRole("button", { name: "Create passkey" })).toHaveLength(1);
    const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0]?.[0].pathname).toBe("/setup");
    expect(posts[0]?.[1]).toMatchObject({ credentials: "include", redirect: "error", headers: { "X-CSRF-Token": "setup-csrf" } });
    expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toMatchObject({ owner_first_name: "Ada", owner_email: "ada@example.test", brand_name: "Example" });
  });

  it("opens this device's passkey prompt from the wizard's one press, and offers another device after a cancel", async () => {
    // memql#6214: the owner pressed through the same information three times
    // before the browser ever asked for a passkey.
    const create = vi.fn()
      .mockRejectedValueOnce(new DOMException("The operation was cancelled.", "NotAllowedError"))
      .mockResolvedValueOnce({ toJSON: () => ({ id: "credential" }) });
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    Object.defineProperty(navigator, "credentials", { configurable: true, value: { create } });
    const fetcher = vi.fn(async (input: URL, init?: RequestInit) => {
      if (input.pathname === "/auth/webauthn/register/begin") return json({ challengeId: "one", creationOptions: { publicKey: { challenge: "abc" } } });
      if (input.pathname === "/auth/webauthn/register/finish") return json({ redirectTo: "https://os.example.test/" });
      if (init?.method === "POST") return json({ redirect: "/auth/setup/passkey" });
      if (input.pathname === "/auth/setup/passkey") return json({ page: "setup_passkey", data: { Local: true, EnrollmentToken: "enrollment" } });
      return json(setup);
    });
    vi.stubGlobal("fetch", fetcher);
    try {
      render(<IdentityScreen initialPath="/setup" />);
      fireEvent.click(await reachSubmission());
      // No second press: the prompt opened on its own, for this device.
      await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
      expect(create.mock.calls[0]?.[0].publicKey.authenticatorSelection).toEqual({ authenticatorAttachment: "platform" });
      // A cancel is the person's answer, said once, with the way forward.
      expect((await screen.findByRole("alert")).textContent).toBe("No passkey was created. Try again when you are ready.");
      pressBar("Use another device");
      await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
      expect(create.mock.calls[1]?.[0].publicKey.hints).toEqual(["hybrid", "security-key"]);
    } finally {
      delete (navigator as unknown as { credentials?: unknown }).credentials;
    }
  });

  it("offers identity's passkey window when the browser refuses the related origin, and opens it from the press", async () => {
    // memql#6233: Safari on a *.localhost cluster refuses OS's request for
    // identity's RP ID; the owner continues in a window on identity's origin.
    const create = vi.fn().mockRejectedValue(new DOMException("The origin of the document is not authorized for the provided RP ID.", "NotAllowedError"));
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    Object.defineProperty(navigator, "credentials", { configurable: true, value: { create } });
    vi.stubGlobal("fetch", vi.fn(async (input: URL, init?: RequestInit) => {
      if (input.pathname === "/auth/webauthn/register/begin") return json({ challengeId: "one", creationOptions: { publicKey: { challenge: "abc" } } });
      if (init?.method === "POST") return json({ redirect: "/auth/setup/passkey" });
      if (input.pathname === "/auth/setup/passkey") return json({ page: "setup_passkey", data: { Local: true, EnrollmentToken: "enrollment" } });
      return json(setup);
    }));
    const frame = document.createElement("iframe");
    document.body.appendChild(frame);
    const popup = frame.contentWindow!;
    const handOver = vi.spyOn(popup, "postMessage").mockImplementation(() => undefined);
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    try {
      render(<IdentityScreen initialPath="/setup" />);
      fireEvent.click(await reachSubmission());
      expect((await screen.findByRole("alert")).textContent).toBe("This browser asks for the passkey in a separate window.");
      expect(open).not.toHaveBeenCalled();
      pressBar("Open window");
      expect(open).toHaveBeenCalledWith(expect.stringMatching(/\/passkey$/), "memql-passkey", expect.any(String));
      window.dispatchEvent(new MessageEvent("message", { origin: window.location.origin, source: popup, data: { type: "memql-passkey:ready" } }));
      expect(handOver).toHaveBeenCalledWith(expect.objectContaining({ type: "memql-passkey:request", kind: "register", authorization: "Bootstrap enrollment" }), window.location.origin);
      expect(create).toHaveBeenCalledTimes(1);
    } finally {
      delete (navigator as unknown as { credentials?: unknown }).credentials;
      frame.remove();
    }
  });

  it("keeps theme selection through setup steps without losing the owner's draft", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json(setup)));
    render(<IdentityScreen initialPath="/setup" />);
    await screen.findByLabelText("First name");
    fireEvent.change(screen.getByLabelText("First name"), { target: { value: "Grace" } });
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    pressBar("Continue");
    fireEvent.click(screen.getByRole("radio", { name: "Dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    pressBar("Back");
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
    expect(screen.getByRole("button", { name: "Create passkey" })).toBeTruthy();
    expect(screen.getByText("Grace Owner")).toBeTruthy();
    pressBar("Create passkey");
    await screen.findByRole("heading", { name: "Register owner passkey" });
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
  fireEvent.click(screen.getByRole("button", { name: local ? "Create passkey" : "Send verification link" }));
  if (local) await screen.findByText("Your browser will ask you to confirm the passkey.");
  else await screen.findByRole("heading", { name: "Check your email" });
  const posts = fetcher.mock.calls.filter(([, init]) => init?.method === "POST");
  expect(posts).toHaveLength(1);
  expect(posts[0]?.[0].pathname).toBe("/auth/setup/resume");
  expect(Object.fromEntries(posts[0]?.[1]?.body as URLSearchParams)).toMatchObject({ email: "ada@example.test", client_id: "editor", code_challenge: "challenge", code_challenge_method: "S256" });
});

it("opens a local installation's first owner step under the editor's act, with the recorded email filled in", async () => {
  // The editor sends the email its install recorded as login_hint, and identity
  // echoes a plain address back as PrefillEmail; the person confirms, not types.
  vi.stubGlobal("fetch", vi.fn(async () => json({ page: "setup_resume", csrf: "resume-csrf", data: { Local: true, HasProof: false, PrefillEmail: "ada@example.test" } })));
  render(<IdentityScreen initialPath="/setup" />);
  await screen.findByRole("heading", { name: "Register owner passkey" });
  expect((screen.getByLabelText("Owner email") as HTMLInputElement).value).toBe("ada@example.test");
  expect(screen.queryByText("Ownership setup has started but is not complete.")).toBeNull();
  expect(screen.getByRole("button", { name: "Create passkey" })).toBeTruthy();
});

it("offers the forward act only once there is an owner email to confirm", async () => {
  // An act that is not legal is absent, never disabled (DESIGN.md rule 12).
  vi.stubGlobal("fetch", vi.fn(async () => json({ page: "setup_resume", csrf: "resume-csrf", data: { Local: true, HasProof: false } })));
  render(<IdentityScreen initialPath="/setup" />);
  const input = await screen.findByLabelText("Owner email");
  expect(screen.queryByRole("button", { name: "Create passkey" })).toBeNull();
  expect(screen.getByText("Enter the owner email")).toBeTruthy();
  fireEvent.change(input, { target: { value: "ada@example.test" } });
  expect(screen.getByRole("button", { name: "Create passkey" })).toBeTruthy();
});

it("keeps a local claim with attestation on its existing passkey", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => json({ page: "setup_resume", data: { Local: true, HasProof: true } })));
  render(<IdentityScreen initialPath="/setup" />);
  await screen.findByRole("button", { name: "Resume with your passkey" });
  expect(screen.queryByLabelText("Owner email")).toBeNull();
  expect(screen.queryByRole("button", { name: "Create passkey" })).toBeNull();
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

const SECURITY_KEY_ONLY = "This browser can save a passkey only to a security key. Without one, use Chrome, which can save it to your Google account or your phone.";
// jsdom names no platform, so the page cannot say "Touch ID" here.
const ANY_AUTHENTICATOR = "Your browser will ask you to confirm the passkey.";

it.each([
  // What Firefox on Linux reports: a local owner with no security key had no way forward.
  ["only a security key", { passkeyPlatformAuthenticator: false, hybridTransport: false }, SECURITY_KEY_ONLY],
  ["a passkey store and a phone", { passkeyPlatformAuthenticator: true, hybridTransport: true }, ANY_AUTHENTICATOR],
  ["a phone but no passkey store", { passkeyPlatformAuthenticator: false, hybridTransport: true }, ANY_AUTHENTICATOR],
  ["nothing it can describe", undefined, ANY_AUTHENTICATOR],
])("says where the owner passkey can go when the browser offers %s", async (_offer, capabilities, sentence) => {
  const probe = vi.fn(async () => capabilities);
  if (capabilities) vi.stubGlobal("PublicKeyCredential", { getClientCapabilities: probe });
  vi.stubGlobal("fetch", vi.fn(async () => json({ page: "setup_passkey", data: { Local: true, EnrollmentToken: "enrollment" } })));
  render(<IdentityScreen initialPath="/auth/setup/passkey" />);
  await screen.findByRole("button", { name: "Create passkey" });
  if (capabilities) await waitFor(() => expect(probe).toHaveBeenCalled());
  expect(await screen.findByText(sentence)).toBeTruthy();
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.queryByText(sentence === SECURITY_KEY_ONLY ? ANY_AUTHENTICATOR : SECURITY_KEY_ONLY)).toBeNull();
});
