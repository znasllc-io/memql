import { afterEach, describe, expect, it, vi } from "vitest";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import { PasskeyWindowNeeded, loginWithPasskey, openPasskeyWindow, passkeyWindowRemembered, registerPasskey, relatedOriginRefused } from "../../src/auth/passkeys";

// The passkey window (memql#6233): when the browser refuses OS's
// related-origin passkey request, the browser's prompt runs in a window on
// identity's own origin. Begin and finish stay on OS's origin, through the
// edge, where identity's cookies live -- as a deployed OS is configured, with
// identity's URL and no separate API base.

const identity = "https://identity.example.test";
const config = { identityUrl: identity, identityApiBaseUrl: "" } as OsRuntimeConfig;

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); sessionStorage.clear(); document.body.innerHTML = ""; });

describe("relatedOriginRefused", () => {
  it.each([
    ["Chrome's SecurityError", new DOMException("The relying party ID is not a registrable domain suffix of, nor equal to the current domain.", "SecurityError"), true],
    ["Safari's refusal", new DOMException("The origin of the document is not authorized for the provided RP ID.", "NotAllowedError"), true],
    ["a cancelled prompt", new DOMException("The operation either timed out or was not allowed.", "NotAllowedError"), false],
    ["an ordinary error", new Error("SecurityError"), false],
  ])("reads %s", (_case, error, refused) => {
    expect(relatedOriginRefused(error)).toBe(refused);
  });
});

// A real window to stand in for the popup, so a message's source is one.
function fakePopup() {
  const frame = document.createElement("iframe");
  document.body.appendChild(frame);
  const popup = frame.contentWindow!;
  const post = vi.spyOn(popup, "postMessage").mockImplementation(() => undefined);
  let closed = false;
  vi.spyOn(popup, "close").mockImplementation(() => { closed = true; });
  Object.defineProperty(popup, "closed", { configurable: true, get: () => closed });
  return { popup, post, closed: () => closed, close: () => { closed = true; } };
}

const say = (source: Window, data: unknown, origin = identity) =>
  window.dispatchEvent(new MessageEvent("message", { origin, source, data }));

const options = { challenge: "abc", rp: { id: "identity.example.test" } };

describe("the window runs only the prompt", () => {
  it("opens inside the press, keeps begin and finish on this origin, and never hands over a token", async () => {
    sessionStorage.setItem("memql.passkeyWindow", "1");
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    const begin = { challengeId: "one", creationOptions: { publicKey: { challenge: "abc", authenticatorSelection: {} } } };
    const fetcher = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(begin)))
      .mockResolvedValueOnce(new Response(JSON.stringify({ redirectTo: "https://os.example.test/" })));
    vi.stubGlobal("fetch", fetcher);
    const { popup, post, closed } = fakePopup();
    const open = vi.spyOn(window, "open").mockReturnValue(popup);

    const result = registerPasskey(config, "Bootstrap grant", "Owner passkey");
    expect(open).toHaveBeenCalledWith(`${identity}/passkey`, "memql-passkey", expect.stringContaining("popup"));
    expect(open.mock.invocationCallOrder[0]).toBeLessThan(fetcher.mock.invocationCallOrder[0] ?? Infinity);

    say(popup, { type: "memql-passkey:ready" });
    await vi.waitFor(() => expect(post).toHaveBeenCalled());
    const [request, target] = post.mock.calls[0]!;
    expect(target).toBe(identity);
    expect(request).toMatchObject({ type: "memql-passkey:request", kind: "create", publicKey: { challenge: "abc", authenticatorSelection: { authenticatorAttachment: "platform" } } });
    expect(JSON.stringify(request)).not.toContain("Bootstrap");

    say(popup, { type: "memql-passkey:credential", credential: { id: "credential" } });
    await expect(result).resolves.toBe("https://os.example.test/");
    const [beginURL, beginInit] = fetcher.mock.calls[0]!;
    expect(String(beginURL)).toBe(`${window.location.origin}/auth/webauthn/register/begin`);
    expect(beginInit.headers).toMatchObject({ Authorization: "Bootstrap grant" });
    const [finishURL, finishInit] = fetcher.mock.calls[1]!;
    expect(String(finishURL)).toBe(`${window.location.origin}/auth/webauthn/register/finish`);
    expect(JSON.parse(finishInit.body)).toEqual({ challengeId: "one", label: "Owner passkey", credential: { id: "credential" } });
    expect(closed()).toBe(true);
  });

  it("listens to identity's window alone", async () => {
    const { popup, post } = fakePopup();
    const stranger = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const prompt = openPasskeyWindow(config);
    const asked = prompt.get(options);
    say(popup, { type: "memql-passkey:ready" }, "https://evil.example.test");
    say(stranger.popup, { type: "memql-passkey:ready" });
    await Promise.resolve();
    expect(post).not.toHaveBeenCalled();
    say(popup, { type: "memql-passkey:ready" });
    await vi.waitFor(() => expect(post).toHaveBeenCalledWith({ type: "memql-passkey:request", kind: "get", publicKey: options }, identity));
    say(stranger.popup, { type: "memql-passkey:credential", credential: { id: "forged" } });
    say(popup, { type: "memql-passkey:credential", credential: { id: "asserted" } });
    await expect(asked).resolves.toEqual({ id: "asserted" });
  });

  it("treats a closed window as the person's answer", async () => {
    vi.useFakeTimers();
    const { popup, close } = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const asked = openPasskeyWindow(config).create(options);
    close();
    vi.advanceTimersByTime(500);
    await expect(asked).rejects.toMatchObject({ name: "NotAllowedError" });
  });

  it("passes on what the window's browser refused", async () => {
    const { popup } = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const asked = openPasskeyWindow(config).create(options);
    say(popup, { type: "memql-passkey:failed", name: "InvalidStateError", error: "This passkey is already registered." });
    await expect(asked).rejects.toMatchObject({ name: "InvalidStateError", message: "This passkey is already registered." });
  });

  it("says so when the browser blocks the window", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    await expect(openPasskeyWindow(config).get(options)).rejects.toThrow("Allow pop-up windows");
  });
});

describe("the refusal", () => {
  const begin = { challengeId: "one", creationOptions: { publicKey: { challenge: "abc", authenticatorSelection: {} } } };

  it("asks for the window once, and the next press goes straight to it", async () => {
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    vi.stubGlobal("navigator", { credentials: { create: vi.fn().mockRejectedValue(new DOMException("The origin of the document is not authorized for the provided RP ID.", "NotAllowedError")) } });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(begin))));
    await expect(registerPasskey(config, "Bootstrap grant", "Owner passkey")).rejects.toBeInstanceOf(PasskeyWindowNeeded);
    expect(passkeyWindowRemembered()).toBe(true);

    const { popup } = fakePopup();
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    const fetched = vi.fn(() => new Promise<Response>(() => undefined));
    vi.stubGlobal("fetch", fetched);
    void registerPasskey(config, "Bootstrap grant", "Owner passkey", "other");
    expect(open).toHaveBeenCalledTimes(1);
    expect(open.mock.invocationCallOrder[0]).toBeLessThan(fetched.mock.invocationCallOrder[0] ?? Infinity);
  });

  it("leaves a cancelled prompt as the person's answer", async () => {
    vi.stubGlobal("PublicKeyCredential", { parseRequestOptionsFromJSON: (x: unknown) => x });
    vi.stubGlobal("navigator", { credentials: { get: vi.fn().mockRejectedValue(new DOMException("The operation either timed out or was not allowed.", "NotAllowedError")) } });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ challengeId: "one", requestOptions: { publicKey: {} } }))));
    await expect(loginWithPasskey(config, {})).rejects.toMatchObject({ name: "NotAllowedError" });
    expect(passkeyWindowRemembered()).toBe(false);
  });
});
