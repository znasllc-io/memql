import type { OsRuntimeConfig } from "../cluster/config";

// The RP ID is supplied by identity and kept stable across the UI move.
// Related Origin Requests let OS use credentials issued at identity's host.
async function post(config: OsRuntimeConfig, path: string, body: unknown, authorization?: string) {
  const response = await fetch(new URL(path, config.identityApiBaseUrl || config.identityUrl), {
    method: "POST", credentials: "include", headers: { "Content-Type": "application/json", ...(authorization ? { Authorization: authorization } : {}) },
    body: JSON.stringify(body), signal: AbortSignal.timeout(30000),
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "The passkey operation failed");
  return data;
}

export async function registerPasskey(config: OsRuntimeConfig, authorization: string, label: string): Promise<string | undefined> {
  if (typeof PublicKeyCredential === "undefined" || !PublicKeyCredential.parseCreationOptionsFromJSON) throw new Error("Use a current browser with passkey support to add a passkey.");
  const begin = await post(config, "/auth/webauthn/register/begin", { label }, authorization);
  if (begin.redirectTo) return begin.redirectTo;
  if (begin.resume) {
    const finish = await post(config, "/auth/webauthn/register/finish", {}, authorization);
    return finish.redirectTo;
  }
  const credential = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(begin.creationOptions.publicKey) }) as PublicKeyCredential | null;
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
