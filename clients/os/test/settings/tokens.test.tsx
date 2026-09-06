import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Settings -> Tokens (epic memql#4984). Every credential in the cluster that
// is not a browser session, and the one act that can be taken on one.
//
// The planted hash is the sweep's target: `patSummary` and `nodeTokenSummary`
// are credential-free SHAPES, so no key material should be able to reach this
// surface even if the server sent some. It is long on purpose.
const PLANTED_HASH = "PLANTED-KEY-HASH-DO-NOT-EMIT-0000000000";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    people: [] as unknown[],
    tokensByUser: {} as Record<string, unknown[]>,
    nodeTokens: [] as unknown[],
    readError: null as Error | null,
    adminCalls: [] as { op: string; id: string }[],
    adminError: null as Error | null,
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef",
    subscriptions: null,
    dispatcher: { send: vi.fn() },
    query: {
      searchUsers: vi.fn(async () => {
        if (state.readError) throw state.readError;
        return reply(state.people);
      }),
      patIdentitiesForUser: vi.fn(async ({ userId }: { userId: string }) =>
        reply(state.tokensByUser[userId] ?? []),
      ),
      nodeTokenIdentitiesAdmin: vi.fn(async () => reply(state.nodeTokens)),
    },
    onStatusChange: () => () => {},
  };
  return { connection, state };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The admin client is the write half, and it is stubbed at the SDK boundary
// rather than through the dispatcher: what this surface has to get right is
// which op it calls with which id, and a hand-rolled envelope encoder in a
// test would be a second implementation of the thing under test.
vi.mock("@znasllc-io/memql-sdk-core/identityadmin", async (importOriginal) => {
  // THE ERROR CLASS IS THE REAL ONE. It composes its own message
  // ("identity admin: <verb>: PERMISSION_DENIED: <reason>") and derives
  // isPermissionDenied from a gRPC code, and a stub of it would let this file
  // assert against a shape the engine never produces -- which is how a test
  // comes to pass while the surface renders nothing a person can read.
  const actual =
    await importOriginal<typeof import("@znasllc-io/memql-sdk-core/identityadmin")>();
  class IdentityAdminClient {
    constructor(_dispatcher: unknown) {}
    async revokePersonalAccessToken(identityId: string) {
      h.state.adminCalls.push({ op: "revokePersonalAccessToken", id: identityId });
      if (h.state.adminError) throw h.state.adminError;
      return { auditEventId: "ae-1" };
    }
    async revokeNodeToken(identityId: string) {
      h.state.adminCalls.push({ op: "revokeNodeToken", id: identityId });
      if (h.state.adminError) throw h.state.adminError;
      return { auditEventId: "ae-2" };
    }
    async updateClusterSettings(settings: unknown) {
      h.state.adminCalls.push({ op: "updateClusterSettings", id: JSON.stringify(settings) });
      if (h.state.adminError) throw h.state.adminError;
      return { auditEventId: "ae-3" };
    }
  }
  return { ...actual, IdentityAdminClient };
});

const { IdentityAdminError } = await import("@znasllc-io/memql-sdk-core/identityadmin");
const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { MAX_PEOPLE_SCANNED } = await import("../../src/apps/settings/tokenFacts");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: role },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com" },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

async function renderTokens(role = "admin") {
  const view = render(
    wrap(<SettingsApp sectionId="tokens" navigate={vi.fn()} askContext={vi.fn()} />, role),
  );
  await act(async () => {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  });
  return view;
}

beforeEach(() => {
  vi.clearAllMocks();
  h.state.people = [
    { id: "u-1", primaryEmail: "ada@example.com" },
    { id: "u-2", primaryEmail: "grace@example.com" },
  ];
  h.state.tokensByUser = {
    "u-1": [
      {
        id: "pat-live",
        label: "laptop",
        active: true,
        lastUsedAt: "2026-09-01T10:00:00Z",
        createdAt: "2026-08-01T10:00:00Z",
        usableByAgents: true,
        keyHash: PLANTED_HASH,
      },
    ],
    "u-2": [
      {
        id: "pat-dead",
        label: "old ci runner",
        active: false,
        createdAt: "2026-07-01T10:00:00Z",
      },
    ],
  };
  h.state.nodeTokens = [
    {
      id: "node-live",
      nodeId: "bff-2",
      nodeType: "bff",
      active: true,
      mintedBy: "bootstrap",
      lastConnectAt: "2026-09-05T09:00:00Z",
      createdAt: "2026-08-01T09:00:00Z",
    },
  ];
  h.state.readError = null;
  h.state.adminCalls = [];
  h.state.adminError = null;
});

describe("Settings -> Tokens", () => {
  it("keeps the two credential kinds apart", async () => {
    await renderTokens();
    const personal = screen.getByRole("region", { name: "Personal access tokens" });
    const nodes = screen.getByRole("region", { name: "Node credentials" });
    expect(within(personal).getByText("laptop")).toBeTruthy();
    expect(within(personal).queryByText("bff-2")).toBeNull();
    expect(within(nodes).getByText("bff-2")).toBeTruthy();
    expect(within(nodes).queryByText("laptop")).toBeNull();
  });

  it("offers no Revoke on an already-revoked row", async () => {
    await renderTokens();
    const personal = screen.getByRole("region", { name: "Personal access tokens" });
    // ABSENT, not disabled (DESIGN.md rule 12). A disabled control advertises
    // an act whose only explanation is a refusal.
    expect(within(personal).queryByRole("button", { name: "Revoke old ci runner" })).toBeNull();
    expect(within(personal).getByRole("button", { name: "Revoke laptop" })).toBeTruthy();
  });

  it("asks before revoking, and names what breaks", async () => {
    await renderTokens();
    fireEvent.click(screen.getByRole("button", { name: "Revoke the credential for bff-2" }));
    // The question is the one being asked, not "Revoke?".
    expect(screen.getByText("Revoke the credential for bff-2?")).toBeTruthy();
    expect(screen.getByText(/cannot rejoin until it is re-bootstrapped/)).toBeTruthy();
    // Nothing has happened yet.
    expect(h.state.adminCalls).toEqual([]);
  });

  it("revokes through the identity admin op, never a mutation", async () => {
    await renderTokens();
    fireEvent.click(screen.getByRole("button", { name: "Revoke laptop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke it" }));
    await act(async () => {
      for (let i = 0; i < 4; i += 1) await Promise.resolve();
    });
    expect(h.state.adminCalls).toEqual([
      { op: "revokePersonalAccessToken", id: "pat-live" },
    ]);
  });

  it("keeps it when the answer is keep it", async () => {
    await renderTokens();
    fireEvent.click(screen.getByRole("button", { name: "Revoke laptop" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep it" }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(h.state.adminCalls).toEqual([]);
    expect(screen.queryByText(/Revoke laptop, held by/)).toBeNull();
  });

  it("renders a refusal beside the control, with the audit id it wrote", async () => {
    // 7 is PERMISSION_DENIED, which is what makes isPermissionDenied true and
    // selects the "the cluster refused that" sentence over "that did not go
    // through".
    h.state.adminError = new IdentityAdminError(
      "revoking a personal access token",
      7,
      "role_below_admin",
      "ae-99",
    );
    await renderTokens();
    fireEvent.click(screen.getByRole("button", { name: "Revoke laptop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke it" }));
    await act(async () => {
      for (let i = 0; i < 4; i += 1) await Promise.resolve();
    });
    expect(screen.getByText("The cluster refused that, and nothing was revoked.")).toBeTruthy();
    expect(screen.getByText(/PERMISSION_DENIED: role_below_admin/)).toBeTruthy();
    // A denial is audited too, and the id is what an operator quotes.
    expect(screen.getByText(/ae-99/)).toBeTruthy();
  });

  it("says how far the fan-out reached", async () => {
    await renderTokens();
    expect(screen.getByText(/Read across all 2 people/)).toBeTruthy();
  });

  it("says when it stopped short rather than drawing a complete-looking list", async () => {
    // The failure this prevents: an operator searching for a leaked token,
    // not finding it, and concluding it does not exist -- when the surface
    // simply never looked that far.
    h.state.people = Array.from({ length: MAX_PEOPLE_SCANNED + 5 }, (_, i) => ({
      id: `u-${i}`,
      primaryEmail: `p${i}@example.com`,
    }));
    await renderTokens();
    expect(
      screen.getByText(
        new RegExp(`first ${MAX_PEOPLE_SCANNED} of ${MAX_PEOPLE_SCANNED + 5} people`),
      ),
    ).toBeTruthy();
  });

  it("renders no key material anywhere in the DOM", async () => {
    const { container } = await renderTokens();
    expect(container.innerHTML).not.toContain(PLANTED_HASH);
    // The negative control. Without it this asserts only that the sweep ran.
    expect(container.innerHTML).toContain("laptop");
  });
});
