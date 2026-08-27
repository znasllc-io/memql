import { describe, expect, it } from "vitest";

import { authorizeUrl, redirectUriFor } from "../src/auth/identityClient";

const config = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "https://identity.example.test",
  oauthClientId: "portal",
  authEnabled: true,
  domain: "example.test",
};

describe("identityClient", () => {
  it("builds the callback on the OS origin, same client id as the portal", () => {
    expect(redirectUriFor("https://os.example.test")).toBe("https://os.example.test/auth/callback");
  });

  it("puts PKCE on the authorize URL and never a token", () => {
    const url = authorizeUrl(config, {
      redirectUri: "https://os.example.test/auth/callback",
      state: "st",
      codeChallenge: "ch",
    });
    const parsed = new URL(url);
    expect(parsed.pathname).toBe("/authorize");
    expect(parsed.searchParams.get("client_id")).toBe("portal");
    expect(parsed.searchParams.get("code_challenge_method")).toBe("S256");
    expect(parsed.search).not.toMatch(/token|refresh|access/i);
  });
});
