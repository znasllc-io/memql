// The runtime-config document the bundle reads from its serving node.
//
// The tolerance rules here are the contract between a cached index.html and a
// node that may have rolled since: an older bundle must keep working against a
// newer node, and a field it does not recognise must not break it.

import { describe, expect, it, vi } from "vitest";

import {
  loadRuntimeConfig,
  normalizeRuntimeConfig,
  runtimeConfigPathFor,
  UNKNOWN_RUNTIME_CONFIG,
} from "../src/cluster/config";

describe("runtimeConfigPathFor", () => {
  it("derives the path from the portal's mount", () => {
    expect(runtimeConfigPathFor("/portal/")).toBe("/portal/runtime-config.json");
  });

  it("tolerates the mount without a trailing slash", () => {
    expect(runtimeConfigPathFor("/portal")).toBe("/portal/runtime-config.json");
  });

  it("preserves a deployment base path", () => {
    // SERVER_PUBLIC_PATH=/memql registers /memql/portal/, so the document
    // lives beneath it too.
    expect(runtimeConfigPathFor("/memql/portal/")).toBe("/memql/portal/runtime-config.json");
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
    const cfg = await loadRuntimeConfig(fetchImpl, "/portal/runtime-config.json");
    expect(cfg.identityUrl).toBe("https://identity.example.com");
  });

  it("throws with the path when the node does not answer", async () => {
    const fetchImpl = vi.fn(
      async () => ({ ok: false, status: 404, json: async () => ({}) }) as unknown as Response,
    );
    await expect(loadRuntimeConfig(fetchImpl, "/portal/runtime-config.json")).rejects.toThrow(
      /runtime-config\.json responded 404/,
    );
  });
});

describe("UNKNOWN_RUNTIME_CONFIG", () => {
  it("assumes auth is enforced", () => {
    expect(UNKNOWN_RUNTIME_CONFIG.authEnabled).toBe(true);
  });
});
