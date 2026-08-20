// The runtime-config document the bundle reads from its serving node.
//
// The tolerance rules here are the contract between a cached index.html and a
// node that may have rolled since: an older bundle must keep working against a
// newer node, and a field it does not recognise must not break it.

import { describe, expect, it, vi } from "vitest";

import {
  isRuntimeConfigReady,
  loadRuntimeConfig,
  normalizeRuntimeConfig,
  runtimeConfigPathFor,
  UNKNOWN_RUNTIME_CONFIG,
} from "../src/cluster/config";

describe("runtimeConfigPathFor", () => {
  // The production value since memql#3711: the portal is site #1, served at
  // its own origin's root by component/edge/runtimeconfig.go, which answers
  // exactly this path ("/runtime-config.json") for every hosted site.
  it("derives the path from a root mount", () => {
    expect(runtimeConfigPathFor("/")).toBe("/runtime-config.json");
  });

  // The function stays general -- worth pinning even though the edge itself
  // resolves by hostname rather than a shared path prefix (component/server
  // .EdgePaths' own doc comment), so no *edge-served* site is actually
  // mounted below the root in production today.
  it("tolerates a mount without a trailing slash", () => {
    expect(runtimeConfigPathFor("/preview")).toBe("/preview/runtime-config.json");
  });
});

describe("normalizeRuntimeConfig", () => {
  it("reads a well-formed document", () => {
    expect(
      normalizeRuntimeConfig({
        identityUrl: "https://identity.example.com/",
        identityApiBaseUrl: "https://identity.example.com",
        oauthClientId: "portal",
        authEnabled: true,
      }),
    ).toEqual({
      identityUrl: "https://identity.example.com",
      identityApiBaseUrl: "https://identity.example.com",
      oauthClientId: "portal",
      authEnabled: true,
    });
  });

  it("treats an absent authEnabled as enforced", () => {
    // Fail closed. Assuming an unfamiliar cluster is open is the wrong way to
    // be wrong: it would render the shell and let an operator believe they
    // were connected as themselves.
    expect(normalizeRuntimeConfig({}).authEnabled).toBe(true);
    expect(normalizeRuntimeConfig({ authEnabled: "no" }).authEnabled).toBe(true);
    expect(normalizeRuntimeConfig(null).authEnabled).toBe(true);
    // Only an explicit false disables.
    expect(normalizeRuntimeConfig({ authEnabled: false }).authEnabled).toBe(false);
  });

  it("ignores a field it does not know", () => {
    const cfg = normalizeRuntimeConfig({
      identityUrl: "https://identity.example.com",
      // Exactly the field a future server-side cluster registry would add
      // (see src/cluster/endpoint.ts): an older bundle must ignore it, not
      // choke on it.
      clusters: [{ name: "prod" }],
    }) as unknown as Record<string, unknown>;
    expect(cfg.clusters).toBeUndefined();
    expect(cfg.identityUrl).toBe("https://identity.example.com");
  });
});

describe("loadRuntimeConfig", () => {
  it("reads the document without sending a credential", async () => {
    const fetchImpl = vi.fn(async (_url: string, init?: RequestInit) => {
      // Same-origin and public; it must not carry the identity cookie.
      expect(init?.credentials).toBe("omit");
      expect(init?.cache).toBe("no-store");
      return {
        ok: true,
        status: 200,
        json: async () => ({ identityUrl: "https://identity.example.com", authEnabled: true }),
      } as unknown as Response;
    });
    const cfg = await loadRuntimeConfig(fetchImpl, "/runtime-config.json");
    expect(cfg.identityUrl).toBe("https://identity.example.com");
  });

  it("throws with the path when the node does not answer", async () => {
    const fetchImpl = vi.fn(
      async () => ({ ok: false, status: 404, json: async () => ({}) }) as unknown as Response,
    );
    await expect(loadRuntimeConfig(fetchImpl, "/runtime-config.json")).rejects.toThrow(
      /runtime-config\.json responded 404/,
    );
  });
});

describe("UNKNOWN_RUNTIME_CONFIG", () => {
  it("assumes auth is enforced", () => {
    expect(UNKNOWN_RUNTIME_CONFIG.authEnabled).toBe(true);
  });
});

describe("isRuntimeConfigReady", () => {
  // AuthCallbackPage sits outside RequireAuth and will exchange on first
  // paint if this returns true. UNKNOWN must stay false so a cold callback
  // does not POST an empty client_id (memql#4154).
  it("is false until both identityUrl and oauthClientId are published", () => {
    expect(isRuntimeConfigReady(UNKNOWN_RUNTIME_CONFIG)).toBe(false);
    expect(
      isRuntimeConfigReady({ ...UNKNOWN_RUNTIME_CONFIG, identityUrl: "https://identity.example.com" }),
    ).toBe(false);
    expect(isRuntimeConfigReady({ ...UNKNOWN_RUNTIME_CONFIG, oauthClientId: "portal" })).toBe(false);
  });

  it("is true for the same-origin production shape (empty identityApiBaseUrl)", () => {
    expect(
      isRuntimeConfigReady({
        identityUrl: "https://identity.memql.localhost",
        identityApiBaseUrl: "",
        oauthClientId: "portal",
        authEnabled: true,
      }),
    ).toBe(true);
  });
});
