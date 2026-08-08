// Every URL the portal builds towards the identity service.
//
// The load-bearing assertion in this file is negative: NO CREDENTIAL EVER
// APPEARS IN A URL. That is a hard requirement of memql#3315, and it is a
// requirement precisely because a URL is the one part of a request that gets
// written down everywhere -- the ingress access log, the proxy log, browser
// history, `document.referrer`, a screenshot, a pasted bug report. A body is
// not. So the tests below inspect the constructed URLs rather than trusting a
// comment that says the tokens go in the body.

import { describe, expect, it, vi } from "vitest";

import {
  apiUrl,
  authorizeUrl,
  endIdentitySession,
  exchangeAuthorizationCode,
  IdentityHttpError,
  refreshAccessToken,
} from "../src/auth/identityClient";
import type { PortalRuntimeConfig } from "../src/cluster/config";

const CROSS_ORIGIN: PortalRuntimeConfig = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
};

const PROXIED: PortalRuntimeConfig = {
  ...CROSS_ORIGIN,
  // Empty = same-origin: the front door proxies the identity JSON endpoints.
  identityApiBaseUrl: "",
};

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

describe("authorizeUrl", () => {
  it("builds an OAuth 2.1 code request with mandatory S256 PKCE", () => {
    const url = new URL(
      authorizeUrl(CROSS_ORIGIN, {
        redirectUri: "https://cockpit.example.com/portal/auth/callback",
        state: "STATE-VALUE",
        codeChallenge: "CHALLENGE-VALUE",
      }),
    );
    expect(url.origin + url.pathname).toBe("https://identity.example.com/authorize");
    expect(url.searchParams.get("response_type")).toBe("code");
    expect(url.searchParams.get("client_id")).toBe("portal");
    expect(url.searchParams.get("redirect_uri")).toBe(
      "https://cockpit.example.com/portal/auth/callback",
    );
    expect(url.searchParams.get("state")).toBe("STATE-VALUE");
    expect(url.searchParams.get("code_challenge")).toBe("CHALLENGE-VALUE");
    // component/identity/web/authorize.go rejects anything else as
    // invalid_request -- OAuth 2.1 requires S256.
    expect(url.searchParams.get("code_challenge_method")).toBe("S256");
  });

  it("carries no verifier, no token, no secret", () => {
    const url = authorizeUrl(CROSS_ORIGIN, {
      redirectUri: "https://cockpit.example.com/portal/auth/callback",
      state: "STATE-VALUE",
      codeChallenge: "CHALLENGE-VALUE",
    });
    // The CHALLENGE is public by design (it is a hash); the VERIFIER is the
    // secret and must never leave this browser except in the token POST body.
    for (const forbidden of ["code_verifier", "client_secret", "access_token", "bearer"]) {
      expect(url).not.toContain(forbidden);
    }
  });

  it("refuses to build a URL when the cluster published no identity service", () => {
    expect(() =>
      authorizeUrl(
        { ...CROSS_ORIGIN, identityUrl: "" },
        { redirectUri: "https://x/y", state: "s", codeChallenge: "c" },
      ),
    ).toThrow(/no identity URL/);
  });
});

describe("apiUrl", () => {
  it("targets the identity origin when the deployment does not proxy", () => {
    expect(apiUrl(CROSS_ORIGIN, "/auth/refresh")).toBe(
      "https://identity.example.com/auth/refresh",
    );
  });

  it("stays relative when the front door proxies same-origin", () => {
    // Relative on purpose: the browser resolves it against the document, so
    // there is no origin for the portal to get wrong.
    expect(apiUrl(PROXIED, "/auth/refresh")).toBe("/auth/refresh");
  });
});

describe("the token endpoints", () => {
  it("sends the code and verifier in the BODY, never the query string", async () => {
    let seenUrl = "";
    let seenInit: RequestInit | undefined;
    const fetchImpl = vi.fn(async (url: string, init?: RequestInit) => {
      seenUrl = url;
      seenInit = init;
      return jsonResponse({ access_token: "AT", expires_in: 900 });
    });

    await exchangeAuthorizationCode(fetchImpl, CROSS_ORIGIN, {
      code: "AUTH-CODE",
      codeVerifier: "THE-VERIFIER",
      redirectUri: "https://cockpit.example.com/portal/auth/callback",
    });

    expect(new URL(seenUrl).search).toBe("");
    expect(seenUrl).not.toContain("AUTH-CODE");
    expect(seenUrl).not.toContain("THE-VERIFIER");

    const body = JSON.parse(String(seenInit?.body)) as Record<string, string>;
    expect(body.grant_type).toBe("authorization_code");
    expect(body.code).toBe("AUTH-CODE");
    expect(body.code_verifier).toBe("THE-VERIFIER");
    expect(body.client_id).toBe("portal");
    // Required: the HttpOnly refresh cookie is set by this response and would
    // be discarded cross-origin on a non-credentialed request.
    expect(seenInit?.credentials).toBe("include");
  });

  it("discards the refresh token identity returns in the body", async () => {
    // identity returns refresh_token in the JSON as well as in the HttpOnly
    // cookie. Reading it would hand a ~30-day credential to page JavaScript,
    // which is exactly what the cookie exists to prevent. The TokenSet type
    // has no field for it, and this asserts the returned object stays that
    // way even if the type is later widened.
    const fetchImpl = vi.fn(async () =>
      jsonResponse({
        access_token: "AT",
        expires_in: 900,
        refresh_token: "LONG-LIVED-REFRESH-TOKEN",
      }),
    );
    const tokens = await refreshAccessToken(fetchImpl, CROSS_ORIGIN);
    expect(tokens.accessToken).toBe("AT");
    expect(JSON.stringify(tokens)).not.toContain("LONG-LIVED-REFRESH-TOKEN");
  });

  it("surfaces identity's OAuth error envelope, flagging 401 as unauthenticated", async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ error: "invalid_grant", message: "refresh token is no longer valid" }, 401),
    );
    await expect(refreshAccessToken(fetchImpl, CROSS_ORIGIN)).rejects.toSatisfy(
      (err: unknown) =>
        err instanceof IdentityHttpError &&
        err.unauthenticated &&
        err.code === "invalid_grant" &&
        err.message.includes("no longer valid"),
    );
  });

  it("treats a non-JSON failure body as an honest status, not a SyntaxError", async () => {
    // An ingress 502 returns HTML. Throwing a JSON parse error from inside the
    // auth layer would report the wrong problem entirely.
    const fetchImpl = vi.fn(async () => ({
      ok: false,
      status: 502,
      json: async () => {
        throw new SyntaxError("Unexpected token <");
      },
    }) as unknown as Response);
    await expect(refreshAccessToken(fetchImpl, CROSS_ORIGIN)).rejects.toSatisfy(
      (err: unknown) => err instanceof IdentityHttpError && err.status === 502,
    );
  });

  it("rejects a 200 that carries no access token", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ token_type: "Bearer" }));
    await expect(refreshAccessToken(fetchImpl, CROSS_ORIGIN)).rejects.toThrow(/no access token/);
  });

  it("does not let a failed sign-out throw at the caller", async () => {
    // The operator's intent is already satisfied locally by the time this
    // runs; turning a best-effort cookie clear into an error dialog would be
    // reporting a failure for something they experience as done.
    const fetchImpl = vi.fn(async () => {
      throw new TypeError("Load failed");
    });
    await expect(endIdentitySession(fetchImpl, CROSS_ORIGIN)).resolves.toBeUndefined();
  });
});
