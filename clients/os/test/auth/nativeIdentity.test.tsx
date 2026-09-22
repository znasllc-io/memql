import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ownershipState, nativeIdentity, identityEntry, identityLocation } from "../../src/auth/nativeIdentity";
import { OwnershipWizard } from "../../src/auth/OwnershipWizard";
import { IdentityAccount } from "../../src/apps/identity/IdentityAccount";
import { registerPasskey, loginWithPasskey } from "../../src/auth/passkeys";
import { probeSession } from "../../src/auth/identityClient";

const config = { identityUrl: "https://identity.example.test", identityApiBaseUrl: "", oauthClientId: "os", authEnabled: true, domain: "example.test" };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.history.replaceState({}, "", "/"); });

describe("identity authority", () => {
  it("does not present an identity outage as signed out", async () => {
    vi.stubGlobal("navigator", { locks: { request: (_name: string, _options: unknown, run: () => Promise<Response>) => run() } });
    await expect(probeSession(config, async () => new Response("", { status: 503 }))).rejects.toThrow("unavailable");
    expect(await probeSession(config, async () => new Response("", { status: 401 }))).toEqual({ signedIn: false });
  });
  it.each([503, 500, 404])("never turns an HTTP %s into unclaimed", async status => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "Unavailable" }), { status })));
    await expect(ownershipState(config)).rejects.toThrow();
  });
  it("requires an explicit state and preserves the cookie origin", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ state: "unclaimed" })));
    vi.stubGlobal("fetch", fetcher);
    expect(await ownershipState(config)).toBe("unclaimed");
    expect(fetcher.mock.calls[0]?.[0].origin).toBe(config.identityUrl);
    expect(fetcher.mock.calls[0]?.[1].credentials).toBe("include");
    fetcher.mockResolvedValue(new Response("{}"));
    await expect(ownershipState(config)).rejects.toThrow();
    await expect(nativeIdentity(config, "https://evil.test/")).rejects.toThrow("origin refused");
  });
  it("round trips OAuth and link values through a fragment without query exposure", () => {
    const entry = "/authorize?state=a%2Bb&code_challenge=a%25b&redirect_uri=https%3A%2F%2Fclient.test%2Fcallback";
    window.history.replaceState({}, "", identityLocation(entry));
    expect(window.location.search).toBe("");
    expect(identityEntry()).toBe(entry);
  });
});

it("walks through all existing setup choices and submits only on verification", async () => {
  const submit = vi.fn();
  render(<OwnershipWizard data={{ PrefillDomain: "example.test" }} busy={false} error="" submit={submit} />);
  expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
  for (const [label, value] of [["First name", "Ada"], ["Last name", "Lovelace"], ["Owner email", "ada@example.test"], ["Phone number (optional)", "+1555010100"], ["Role at organization (optional)", "Engineer"], ["Date of birth (optional)", "1990-01-01"]]) {
    fireEvent.change(screen.getByLabelText(label!), { target: { value } });
  }
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.change(screen.getByLabelText("Organization name (optional)"), { target: { value: "Example" } });
  fireEvent.change(screen.getByLabelText("Internal email domains (comma-separated)"), { target: { value: "example.test" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.change(screen.getByLabelText("Approved email domains (comma-separated)"), { target: { value: "partner.test" } });
  fireEvent.change(screen.getByLabelText("Notify these emails about access requests (comma-separated)"), { target: { value: "ops@example.test" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(submit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Send verification link" }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("/setup", expect.objectContaining({
    owner_first_name: "Ada", owner_last_name: "Lovelace", owner_email: "ada@example.test", domain: "example.test",
    owner_phone: "+1555010100", owner_primary_role: "Engineer", owner_birthdate: "1990-01-01", brand_name: "Example",
    internal_domains: "example.test", registration_domains: "partner.test", access_request_notify_emails: "ops@example.test",
  })));
});

it("requires the server's revocation warning and keeps unknown sessions distinct from none", () => {
  const submit = vi.fn();
  render(<IdentityAccount page="me/devices" data={{ PasskeysEnabled: true, SessionsAvailable: false,
    RevokeWarning: { ID: "key", Headline: "Last credential", Detail: "You will lose access", LastCredential: true } }}
    busy={false} submit={submit} navigate={vi.fn()} register={vi.fn()} />);
  expect(screen.getByText("Session information is unavailable.")).toBeTruthy();
  expect(screen.queryByText("No active sessions.")).toBeNull();
  expect(screen.getByRole("alert").textContent).toContain("You will lose access");
  fireEvent.click(screen.getByRole("button", { name: "Revoke passkey" }));
  expect(submit).toHaveBeenCalledWith("/me/devices/passkeys/revoke", { id: "key", confirm: "yes" });
});

it("requires allowed domains before choosing domain-restricted registration", () => {
  render(<OwnershipWizard data={{ PrefillOwnerFirstName: "Ada", PrefillOwnerLastName: "Lovelace", PrefillOwnerEmail: "ada@example.test", PrefillDomain: "example.test", PrefillMode: "domain_restricted" }} busy={false} error="" submit={vi.fn()} />);
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
  fireEvent.change(screen.getByLabelText("Approved email domains (comma-separated)"), { target: { value: "example.test" } });
  expect(screen.getByRole("button", { name: "Continue" })).toBeTruthy();
});


it("local setup requires a passkey and never offers an email verification action", () => {
  const submit = vi.fn();
  render(<OwnershipWizard data={{ Local: true, PrefillOwnerFirstName: "Ada", PrefillOwnerLastName: "Owner", PrefillOwnerEmail: "ada@example.test", PrefillDomain: "test" }} busy={false} error="" submit={submit} />);
  expect(screen.getByText(/Contact information only/)).toBeTruthy();
  for (let i = 0; i < 3; i++) fireEvent.click(screen.getByRole("button", { name: "Continue" }));
  expect(screen.queryByRole("button", { name: "Send verification link" })).toBeNull();
  expect(screen.getByText(/No email is sent/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Continue to passkey" }));
  expect(submit).toHaveBeenCalledWith("/setup", expect.objectContaining({ owner_email: "ada@example.test" }));
});

it("does not finish setup when passkeys are unavailable or the user cancels", async () => {
  const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ creationOptions: { publicKey: {} }, challengeId: "one" })));
  vi.stubGlobal("fetch", fetcher);
  vi.stubGlobal("PublicKeyCredential", undefined);
  await expect(registerPasskey(config, "Bootstrap grant", "Owner")).rejects.toThrow("current browser");
  expect(fetcher).not.toHaveBeenCalled();
  vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x });
  vi.stubGlobal("navigator", { credentials: { create: vi.fn().mockResolvedValue(null) } });
  await expect(registerPasskey(config, "Bootstrap grant", "Owner")).rejects.toThrow("No passkey");
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it("resumes a verified passkey save without creating a second credential", async () => {
  vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x });
  const create = vi.fn(); vi.stubGlobal("navigator", { credentials: { create } });
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ resume: true })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ success: true, redirectTo: "https://identity.example.test/login" })));
  vi.stubGlobal("fetch", fetcher);
  expect(await registerPasskey(config, "Bootstrap grant", "Owner")).toBe("https://identity.example.test/login");
  expect(create).not.toHaveBeenCalled();
  expect(fetcher.mock.calls[1]?.[1].body).toBe("{}");
});

it("explicitly requests first-party passkey sign-in when no OAuth client is in scope", async () => {
  vi.stubGlobal("PublicKeyCredential", { parseRequestOptionsFromJSON: (x: unknown) => x });
  vi.stubGlobal("navigator", { credentials: { get: vi.fn().mockResolvedValue({ toJSON: () => ({ id: "credential" }) }) } });
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ requestOptions: { publicKey: {} }, challengeId: "one" })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ redirectTo: "/" })));
  vi.stubGlobal("fetch", fetcher);
  expect(await loginWithPasskey(config, {})).toBe("/");
  expect(JSON.parse(fetcher.mock.calls[0]?.[1].body)).toEqual({ firstParty: true });
});
