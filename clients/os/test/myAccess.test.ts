import { describe, expect, it } from "vitest";

import { fetchMyAccess, parseAccessFacts } from "../src/modules/profile/myAccess";

const config = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "https://identity.example.test",
  oauthClientId: "portal",
  authEnabled: true,
  domain: "example.test",
};

describe("MyAccess slim client", () => {
  it("reads the same facts as portal MyAccess: userId, primaryEmail, clusterRole", () => {
    const facts = parseAccessFacts({
      userId: "usr_1",
      primaryEmail: "ada@example.test",
      clusterRole: "member",
    });
    expect(facts).toEqual({
      userId: "usr_1",
      primaryEmail: "ada@example.test",
      clusterRole: "member",
    });
  });

  it("accepts identity /me/api/profile role alias without inventing a second RBAC", () => {
    const facts = parseAccessFacts({
      id: "usr_2",
      primaryEmail: "al@example.test",
      role: "owner",
    });
    expect(facts).toEqual({
      userId: "usr_2",
      primaryEmail: "al@example.test",
      clusterRole: "owner",
    });
  });

  it("hits identity /me/api/profile with credentials, data only", async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    const fetchImpl = async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      return new Response(
        JSON.stringify({ userId: "usr_1", primaryEmail: "ada@example.test", clusterRole: "member" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    };
    const facts = await fetchMyAccess(config, fetchImpl);
    expect(facts?.primaryEmail).toBe("ada@example.test");
    expect(calls[0]?.url).toBe("https://identity.example.test/me/api/profile");
    expect(calls[0]?.init?.credentials).toBe("include");
  });

  it("returns null when the identity read is unavailable, never throws", async () => {
    const facts = await fetchMyAccess(config, async () => new Response("no", { status: 404 }));
    expect(facts).toBeNull();
  });
});
