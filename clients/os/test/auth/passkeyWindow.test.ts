import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import { PasskeyWindowNeeded, loginWithPasskey, passkeyInWindow, passkeyWindowRemembered, registerPasskey, relatedOriginRefused } from "../../src/auth/passkeys";

// The passkey window (memql#6233): when the browser refuses OS's
// related-origin passkey request, the same request runs on identity's own
// origin in a window, and the result comes back by message.

const identity = "https://identity.example.test";
const config = { identityApiBaseUrl: identity } as OsRuntimeConfig;

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
  Object.defineProperty(popup, "closed", { configurable: true, get: () => closed });
  return { popup, post, close: () => { closed = true; } };
}

const say = (source: Window, data: unknown, origin = identity) =>
  window.dispatchEvent(new MessageEvent("message", { origin, source, data }));

describe("passkeyInWindow", () => {
  beforeEach(() => { vi.useFakeTimers(); });

  it("hands the request over once the window is ready and resolves with where to go next", async () => {
    const { popup, post } = fakePopup();
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    const result = passkeyInWindow(config, { kind: "register", authorization: "Bootstrap grant", label: "Owner passkey" });
    expect(open).toHaveBeenCalledWith(`${identity}/passkey`, "memql-passkey", expect.stringContaining("popup"));
    say(popup, { type: "memql-passkey:ready" });
    expect(post).toHaveBeenCalledWith({ type: "memql-passkey:request", kind: "register", authorization: "Bootstrap grant", label: "Owner passkey" }, identity);
    say(popup, { type: "memql-passkey:done", redirectTo: "https://os.example.test/" });
    await expect(result).resolves.toBe("https://os.example.test/");
  });

  it("listens to identity's window alone", async () => {
    const { popup, post } = fakePopup();
    const stranger = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const result = passkeyInWindow(config, { kind: "login", begin: { firstParty: true } });
    say(popup, { type: "memql-passkey:ready" }, "https://evil.example.test");
    say(stranger.popup, { type: "memql-passkey:ready" });
    expect(post).not.toHaveBeenCalled();
    say(stranger.popup, { type: "memql-passkey:done", redirectTo: "https://evil.example.test/" });
    say(popup, { type: "memql-passkey:done", redirectTo: "https://os.example.test/signed-in" });
    await expect(result).resolves.toBe("https://os.example.test/signed-in");
  });

  it("treats a closed window as the person's answer", async () => {
    const { popup, close } = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const result = passkeyInWindow(config, { kind: "login", begin: {} });
    close();
    vi.advanceTimersByTime(500);
    await expect(result).rejects.toMatchObject({ name: "NotAllowedError" });
  });

  it("passes on what identity refused", async () => {
    const { popup } = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup);
    const result = passkeyInWindow(config, { kind: "register", authorization: "Bearer x", label: "My passkey" });
    say(popup, { type: "memql-passkey:failed", error: "This enrolment link has expired." });
    await expect(result).rejects.toThrow("This enrolment link has expired.");
  });

  it("says so when the browser blocks the window", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    await expect(passkeyInWindow(config, { kind: "login", begin: {} })).rejects.toThrow("Allow pop-up windows");
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

    // The window opens inside the press, before any request leaves.
    const { popup } = fakePopup();
    const open = vi.spyOn(window, "open").mockReturnValue(popup);
    const fetched = vi.fn();
    vi.stubGlobal("fetch", fetched);
    void registerPasskey(config, "Bootstrap grant", "Owner passkey", "other");
    expect(open).toHaveBeenCalledTimes(1);
    expect(fetched).not.toHaveBeenCalled();
  });

  it("leaves a cancelled prompt as the person's answer", async () => {
    vi.stubGlobal("PublicKeyCredential", { parseRequestOptionsFromJSON: (x: unknown) => x });
    vi.stubGlobal("navigator", { credentials: { get: vi.fn().mockRejectedValue(new DOMException("The operation either timed out or was not allowed.", "NotAllowedError")) } });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ challengeId: "one", requestOptions: { publicKey: {} } }))));
    await expect(loginWithPasskey(config, {})).rejects.toMatchObject({ name: "NotAllowedError" });
    expect(passkeyWindowRemembered()).toBe(false);
  });
});
