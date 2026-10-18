import { isRoleTestWindow } from "@znasllc-io/memql-sdk-core/identityadmin";
import type { OsRuntimeConfig } from "../cluster/config";

// The RP ID is supplied by identity and kept stable across the UI move.
// Related Origin Requests let OS use credentials issued at identity's host.
async function post(config: OsRuntimeConfig, path: string, body: unknown, authorization?: string) {
  if (isRoleTestWindow()) throw new Error("Account security is unavailable during role tests. Use your original tab.");
  const response = await fetch(new URL(path, config.identityApiBaseUrl || window.location.origin), {
    method: "POST", credentials: "include", headers: { "Content-Type": "application/json", ...(authorization ? { Authorization: authorization } : {}) },
    body: JSON.stringify(body), signal: AbortSignal.timeout(30000),
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "The passkey operation failed");
  return data;
}

/**
 * What this device's own authenticator is called, in the words its owner
 * knows it by: "Touch ID" on a Mac, "Face ID or Touch ID" on an iPhone or
 * iPad, "Windows Hello", or "" when the platform is not recognised.
 */
export function deviceAuthenticatorName(nav: Pick<Navigator, "userAgent" | "platform" | "maxTouchPoints"> & { userAgentData?: { platform?: string } } = navigator): string {
  const platform = (nav.userAgentData?.platform || nav.platform || "").toLowerCase();
  const agent = (nav.userAgent || "").toLowerCase();
  // iPadOS asks for desktop sites and reports a Mac; touch gives it away.
  if (/iphone|ipad|ipod/.test(agent) || (platform.startsWith("mac") && nav.maxTouchPoints > 1)) return "Face ID or Touch ID";
  if (platform.startsWith("mac") || platform === "macos") return "Touch ID";
  if (platform.startsWith("win")) return "Windows Hello";
  if (/android/.test(agent) || platform === "android") return "your screen lock";
  return "";
}

/**
 * Whether to offer this device's own authenticator first. True when the
 * browser says one is available (Touch ID, Face ID, Windows Hello, an Android
 * screen lock); false when it cannot answer or says no.
 */
export async function deviceAuthenticatorAvailable(): Promise<boolean> {
  try {
    return typeof PublicKeyCredential !== "undefined"
      && typeof PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable === "function"
      && await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable();
  } catch {
    return false;
  }
}

/**
 * Where the browser should look first when it asks for the passkey: this
 * device ("device") goes straight to Touch ID, Face ID or Windows Hello;
 * "other" offers a phone or a security key. The identity service's options
 * are otherwise untouched, and verification does not depend on either.
 */
export type PasskeyPlace = "device" | "other";

export function placeCreationOptions(publicKey: Record<string, unknown>, place: PasskeyPlace): Record<string, unknown> {
  const selection = { ...((publicKey.authenticatorSelection as Record<string, unknown> | undefined) ?? {}) };
  if (place === "device") {
    selection.authenticatorAttachment = "platform";
    return { ...publicKey, authenticatorSelection: selection, hints: ["client-device"] };
  }
  delete selection.authenticatorAttachment;
  return { ...publicKey, authenticatorSelection: selection, hints: ["hybrid", "security-key"] };
}

export async function registerPasskey(config: OsRuntimeConfig, authorization: string, label: string, place?: PasskeyPlace): Promise<string | undefined> {
  if (typeof PublicKeyCredential === "undefined" || !PublicKeyCredential.parseCreationOptionsFromJSON) throw new Error("Use a current browser with passkey support to add a passkey.");
  const begin = await post(config, "/auth/webauthn/register/begin", { label }, authorization);
  if (begin.redirectTo) return begin.redirectTo;
  if (begin.resume) {
    const finish = await post(config, "/auth/webauthn/register/finish", {}, authorization);
    return finish.redirectTo;
  }
  // This device first when it has its own authenticator, unless the person
  // asked for another one; the browser's full choice otherwise.
  const chosen = place ?? (await deviceAuthenticatorAvailable() ? "device" : undefined);
  const options = chosen ? placeCreationOptions(begin.creationOptions.publicKey, chosen) : begin.creationOptions.publicKey;
  const credential = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options as PublicKeyCredentialCreationOptionsJSON) }) as PublicKeyCredential | null;
  if (!credential) throw new Error("No passkey was created");
  const finish = await post(config, "/auth/webauthn/register/finish", { challengeId: begin.challengeId, label, credential: credential.toJSON() }, authorization);
  return finish.redirectTo;
}

export async function loginWithPasskey(config: OsRuntimeConfig, context: Record<string, string>): Promise<string> {
  if (typeof PublicKeyCredential === "undefined" || !PublicKeyCredential.parseRequestOptionsFromJSON) throw new Error("Use a current browser with passkey support to sign in.");
  const begin = await post(config, "/auth/webauthn/login/begin", {
    firstParty: !context.client_id && !context.redirect_uri,
    clientId: context.client_id, redirectUri: context.redirect_uri, state: context.state,
    codeChallenge: context.code_challenge, codeChallengeMethod: context.code_challenge_method,
  });
  const credential = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(begin.requestOptions.publicKey) }) as PublicKeyCredential | null;
  if (!credential) throw new Error("No passkey was selected");
  const finish = await post(config, "/auth/webauthn/login/finish", { challengeId: begin.challengeId, credential: credential.toJSON() });
  if (!finish.redirectTo) throw new Error("Sign-in did not return a destination");
  return finish.redirectTo;
}

/**
 * Whether this browser says it can keep a new passkey only on a security key:
 * no passkey store of its own and no phone sign-in, as Firefox on Linux
 * reports. A browser that cannot answer, or answers anything else, is false.
 */
export async function onlySecurityKeys(): Promise<boolean> {
  const credential = globalThis.PublicKeyCredential as unknown as { getClientCapabilities?: () => Promise<Record<string, boolean | undefined>> } | undefined;
  if (typeof credential?.getClientCapabilities !== "function") return false;
  try {
    const capabilities = await credential.getClientCapabilities();
    return capabilities.passkeyPlatformAuthenticator === false && capabilities.hybridTransport === false;
  } catch {
    return false;
  }
}
