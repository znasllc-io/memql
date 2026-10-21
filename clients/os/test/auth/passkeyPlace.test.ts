import { afterEach, describe, expect, it, vi } from "vitest";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import { deviceAuthenticatorName, placeCreationOptions, registerPasskey } from "../../src/auth/passkeys";

// One press to the prompt, and this device first (memql#6214): a Mac opens
// Touch ID, an iPhone Face ID, Windows its Hello prompt, unless the person
// asks for another device.

const config = { identityApiBaseUrl: "https://identity.example.test" } as OsRuntimeConfig;

afterEach(() => { vi.unstubAllGlobals(); });

const nav = (over: Partial<{ userAgent: string; platform: string; maxTouchPoints: number; userAgentData: { platform?: string } }>) => ({
  userAgent: "", platform: "", maxTouchPoints: 0, ...over,
});

describe("deviceAuthenticatorName", () => {
  it.each([
    ["a Mac", nav({ platform: "MacIntel" }), "Touch ID"],
    ["a Mac through client hints", nav({ userAgentData: { platform: "macOS" } }), "Touch ID"],
    ["an iPhone", nav({ userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)", platform: "iPhone" }), "Face ID or Touch ID"],
    ["an iPad asking for desktop sites", nav({ platform: "MacIntel", maxTouchPoints: 5 }), "Face ID or Touch ID"],
    ["Windows", nav({ platform: "Win32" }), "Windows Hello"],
    ["Android", nav({ userAgent: "Mozilla/5.0 (Linux; Android 15)", platform: "Linux armv8l" }), "your screen lock"],
    ["an unknown platform", nav({ platform: "Linux x86_64" }), ""],
  ])("names %s's own authenticator", (_device, navigator, name) => {
    expect(deviceAuthenticatorName(navigator)).toBe(name);
  });
});

describe("placeCreationOptions", () => {
  const options = { challenge: "abc", authenticatorSelection: { residentKey: "required", userVerification: "required", authenticatorAttachment: "cross-platform" } };

  it("asks for this device's own authenticator first", () => {
    const placed = placeCreationOptions(options, "device");
    expect(placed.authenticatorSelection).toEqual({ residentKey: "required", userVerification: "required", authenticatorAttachment: "platform" });
    expect(placed.hints).toEqual(["client-device"]);
    expect(placed.challenge).toBe("abc");
  });

  it("offers a phone or a security key when the person asks for another device", () => {
    const placed = placeCreationOptions(options, "other");
    expect(placed.authenticatorSelection).toEqual({ residentKey: "required", userVerification: "required" });
    expect(placed.hints).toEqual(["hybrid", "security-key"]);
  });

  it("leaves the identity service's options untouched", () => {
    placeCreationOptions(options, "device");
    expect(options.authenticatorSelection.authenticatorAttachment).toBe("cross-platform");
  });
});

describe("registerPasskey", () => {
  const begin = { challengeId: "one", creationOptions: { publicKey: { challenge: "abc", authenticatorSelection: { residentKey: "required" } } } };
  const respond = () => vi.fn()
    .mockResolvedValueOnce(new Response(JSON.stringify(begin)))
    .mockResolvedValueOnce(new Response(JSON.stringify({ redirectTo: "https://os.example.test/" })));

  it("goes straight to this device's authenticator when the browser has one", async () => {
    const create = vi.fn().mockResolvedValue({ toJSON: () => ({ id: "credential" }) });
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    vi.stubGlobal("navigator", { credentials: { create } });
    vi.stubGlobal("fetch", respond());
    expect(await registerPasskey(config, "Bootstrap grant", "Owner")).toBe("https://os.example.test/");
    const publicKey = create.mock.calls[0]?.[0].publicKey;
    expect(publicKey.authenticatorSelection.authenticatorAttachment).toBe("platform");
    expect(publicKey.hints).toEqual(["client-device"]);
  });

  it("leaves the browser's full choice when this device has no authenticator of its own", async () => {
    const create = vi.fn().mockResolvedValue({ toJSON: () => ({ id: "credential" }) });
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => false });
    vi.stubGlobal("navigator", { credentials: { create } });
    vi.stubGlobal("fetch", respond());
    await registerPasskey(config, "Bootstrap grant", "Owner");
    expect(create.mock.calls[0]?.[0].publicKey).toEqual(begin.creationOptions.publicKey);
  });

  it("uses another device when the person asks for one", async () => {
    const create = vi.fn().mockResolvedValue({ toJSON: () => ({ id: "credential" }) });
    vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: (x: unknown) => x, isUserVerifyingPlatformAuthenticatorAvailable: async () => true });
    vi.stubGlobal("navigator", { credentials: { create } });
    vi.stubGlobal("fetch", respond());
    await registerPasskey(config, "Bootstrap grant", "Owner", "other");
    const publicKey = create.mock.calls[0]?.[0].publicKey;
    expect(publicKey.authenticatorSelection.authenticatorAttachment).toBeUndefined();
    expect(publicKey.hints).toEqual(["hybrid", "security-key"]);
  });
});
