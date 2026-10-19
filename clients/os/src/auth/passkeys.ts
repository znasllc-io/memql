import { isRoleTestWindow } from "@znasllc-io/memql-sdk-core/identityadmin";
import type { OsRuntimeConfig } from "../cluster/config";

// The RP ID is supplied by identity and kept stable across the UI move.
// Related Origin Requests let OS use credentials issued at identity's host.
//
// THE PASSKEY WINDOW (memql#6233). A browser may refuse that related-origin
// request. Safari leaves the check to Apple's AuthenticationServices, which
// never fetches identity's /.well-known/webauthn on a *.localhost cluster, and
// a password manager that intercepts WebAuthn may not implement related
// origins at all. The same request then runs in a small window on identity's
// own origin, where the RP ID is the page's own host: begin, the prompt and
// finish all happen there, and MemQL OS hands over the request and takes the
// result back by postMessage, each side checking the other's origin. No
// credential travels in a URL.
//
// A window opens only from the person's press, and the press that met the
// refusal is spent by then. So the refusal surfaces as PasskeyWindowNeeded for
// the screen to offer the window, and the rest of the session goes straight
// to it.
export class PasskeyWindowNeeded extends Error {
  constructor() {
    super("This browser asks for the passkey in a separate window.");
    this.name = "PasskeyWindowNeeded";
  }
}

const PASSKEY_WINDOW = "memql.passkeyWindow";

/** The browser refused the related-origin request: not a cancel by the person. */
export function relatedOriginRefused(error: unknown): boolean {
  if (!(error instanceof DOMException)) return false;
  return error.name === "SecurityError" || /\bRP ID\b|relying party/i.test(error.message);
}

/** Whether this session's passkey requests go to identity's window. */
export function passkeyWindowRemembered(): boolean {
  try { return sessionStorage.getItem(PASSKEY_WINDOW) === "1"; } catch { return false; }
}

function refused(error: unknown): unknown {
  if (!relatedOriginRefused(error)) return error;
  try { sessionStorage.setItem(PASSKEY_WINDOW, "1"); } catch { /* the next press meets the refusal again */ }
  return new PasskeyWindowNeeded();
}

export type PasskeyWindowRequest =
  | { kind: "register"; authorization: string; label: string; place?: PasskeyPlace }
  | { kind: "login"; begin: Record<string, unknown> };

/**
 * Runs one passkey request in identity's window and resolves with where to go
 * next, as the in-page request would. Call it synchronously from the press
 * that asked for it, or the browser blocks the window.
 */
export function passkeyInWindow(config: OsRuntimeConfig, request: PasskeyWindowRequest): Promise<string | undefined> {
  const origin = new URL(config.identityApiBaseUrl || window.location.origin).origin;
  const popup = window.open(new URL("/passkey", origin).toString(), "memql-passkey", "popup,width=520,height=640");
  if (!popup) return Promise.reject(new Error("Allow pop-up windows for MemQL OS, then try again."));
  return new Promise((resolve, reject) => {
    const settle = (done: () => void) => { clearInterval(watch); window.removeEventListener("message", receive); done(); };
    const receive = (event: MessageEvent) => {
      if (event.origin !== origin || event.source !== popup) return;
      const message = event.data as { type?: string; redirectTo?: string; error?: string } | null;
      if (message?.type === "memql-passkey:ready") popup.postMessage({ type: "memql-passkey:request", ...request }, origin);
      else if (message?.type === "memql-passkey:done") settle(() => resolve(message.redirectTo || undefined));
      else if (message?.type === "memql-passkey:failed") settle(() => reject(new Error(message.error || "The passkey operation failed")));
    };
    // Closing the window is the person's answer, as cancelling a prompt is.
    const watch = setInterval(() => {
      if (popup.closed) settle(() => reject(new DOMException("The passkey window was closed.", "NotAllowedError")));
    }, 400);
    window.addEventListener("message", receive);
  });
}

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
  if (passkeyWindowRemembered()) return passkeyInWindow(config, { kind: "register", authorization, label, place });
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
  const credential = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options as PublicKeyCredentialCreationOptionsJSON) })
    .catch(error => { throw refused(error); }) as PublicKeyCredential | null;
  if (!credential) throw new Error("No passkey was created");
  const finish = await post(config, "/auth/webauthn/register/finish", { challengeId: begin.challengeId, label, credential: credential.toJSON() }, authorization);
  return finish.redirectTo;
}

export async function loginWithPasskey(config: OsRuntimeConfig, context: Record<string, string>): Promise<string> {
  if (typeof PublicKeyCredential === "undefined" || !PublicKeyCredential.parseRequestOptionsFromJSON) throw new Error("Use a current browser with passkey support to sign in.");
  const request = {
    firstParty: !context.client_id && !context.redirect_uri,
    clientId: context.client_id, redirectUri: context.redirect_uri, state: context.state,
    codeChallenge: context.code_challenge, codeChallengeMethod: context.code_challenge_method,
  };
  if (passkeyWindowRemembered()) {
    const destination = await passkeyInWindow(config, { kind: "login", begin: request });
    if (!destination) throw new Error("Sign-in did not return a destination");
    return destination;
  }
  const begin = await post(config, "/auth/webauthn/login/begin", request);
  const credential = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(begin.requestOptions.publicKey) })
    .catch(error => { throw refused(error); }) as PublicKeyCredential | null;
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
