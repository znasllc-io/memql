import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

// Settings -> Keys (epic memql#4984), and the property the section exists for:
// whether every identity replica publishes the SAME keyset.
//
// Divergent keysets fail roughly half of all auth (memql#3400) -- a token
// minted by one replica is rejected by a verifier that fetched JWKS from
// another -- and the symptom is "sign-in is broken" with every manifest
// looking correct. `make status` checks it from a terminal; nothing in a
// browser did.

const h = vi.hoisted(() => {
  const state = {
    /** One entry per read, cycled. A list of two DIFFERENT keysets is what a
     *  diverged cluster looks like from one hostname. */
    answers: [] as unknown[],
    failEvery: 0,
    reads: 0,
    auditEvents: [] as unknown[],
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef",
    subscriptions: null,
    dispatcher: null,
    query: {
      recentAuditEvents: vi.fn(async () => ({ rows: () => state.auditEvents })),
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

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { agreementOf, PROBE_READS, probeKeyset } = await import(
  "../../src/apps/settings/keyFacts"
);
const { keysetFingerprint } = await import("../../src/apps/settings/adminWire");

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

/** A fetch that answers from `h.state.answers`, cycling, and fails every Nth
 *  read when `failEvery` is set. */
function stubFetch(): typeof globalThis.fetch {
  return (async () => {
    const i = h.state.reads;
    h.state.reads += 1;
    if (h.state.failEvery > 0 && (i + 1) % h.state.failEvery === 0) {
      return { ok: false, status: 503, json: async () => ({}) } as Response;
    }
    const keys = h.state.answers[i % h.state.answers.length];
    return { ok: true, status: 200, json: async () => ({ keys }) } as Response;
  }) as typeof globalThis.fetch;
}

function wrap(children: ReactNode, role: string, identityUrl: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: role },
        config: { ...UNKNOWN_RUNTIME_CONFIG, domain: "example.com", identityUrl },
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

async function renderKeys(role = "owner", identityUrl = "https://identity.example.com") {
  const view = render(
    wrap(<SettingsApp sectionId="keys" navigate={vi.fn()} askContext={vi.fn()} />, role, identityUrl),
  );
  await act(async () => {
    for (let i = 0; i < 12; i += 1) await Promise.resolve();
  });
  return view;
}

const KEY_A = { kid: "k-alpha", kty: "OKP", crv: "Ed25519", alg: "EdDSA", use: "sig" };
const KEY_B = { kid: "k-beta", kty: "OKP", crv: "Ed25519", alg: "EdDSA", use: "sig" };

beforeEach(() => {
  vi.clearAllMocks();
  h.state.answers = [[KEY_A]];
  h.state.failEvery = 0;
  h.state.reads = 0;
  h.state.auditEvents = [];
  vi.stubGlobal("fetch", stubFetch());
});

describe("the keyset fingerprint", () => {
  it("is order-independent", () => {
    // A JWKS feed states no order, so two replicas holding an identical keyset
    // can serve it in different orders. Comparing the raw document would
    // report a disagreement that does not exist -- and an operator who chases
    // one false alarm will not chase the real one.
    expect(keysetFingerprint([KEY_A, KEY_B])).toBe(keysetFingerprint([KEY_B, KEY_A]));
  });

  it("distinguishes genuinely different keysets", () => {
    // The negative control for the line above: without it, a fingerprint that
    // returned "" for everything would pass that test.
    expect(keysetFingerprint([KEY_A])).not.toBe(keysetFingerprint([KEY_A, KEY_B]));
  });
});

describe("the agreement reading", () => {
  it("calls matching reads evidence and not proof", async () => {
    const probe = await probeKeyset("https://identity.example.com", stubFetch(), new AbortController().signal);
    const said = agreementOf(probe);
    expect(said.tone).toBe("agree");
    // THE CLAIM IS BOUNDED, and this is the assertion that keeps it bounded.
    // The front door chooses which replica answers each read, so four matching
    // answers may all have come from one. A section that said "coherent" here
    // would be giving the reassuring answer a broken cluster gives half the
    // time.
    expect(said.sentence).toMatch(/evidence .*not proof/);
  });

  it("calls differing reads proof of divergence", () => {
    const said = agreementOf({ distinct: ["k-alpha", "k-beta"], reads: 4, keys: [KEY_A], failures: [] });
    expect(said.tone).toBe("diverged");
    expect(said.sentence).toMatch(/2 different keysets/);
  });

  it("will not read agreement out of no reads at all", () => {
    // An outage is not coherence. Saying "they agree" off zero answers is the
    // worst sentence this surface could produce.
    expect(agreementOf({ distinct: [], reads: 0, keys: [], failures: ["503"] }).tone).toBe(
      "unknown",
    );
  });
});

describe("Settings -> Keys", () => {
  it("leads with agreement, then lists what is published", async () => {
    await renderKeys();
    expect(screen.getByText(new RegExp(`${PROBE_READS} reads all returned the same keyset`))).toBeTruthy();
    const published = screen.getByRole("region", { name: "Published keys" });
    expect(within(published).getByText("k-alpha")).toBeTruthy();
  });

  it("reports divergence as an error and says what to do", async () => {
    h.state.answers = [[KEY_A], [KEY_B]];
    await renderKeys();
    expect(screen.getByText(/different keysets came back from the same hostname/)).toBeTruthy();
    expect(screen.getByText(/Roll the identity Deployment/)).toBeTruthy();
    // Both keysets are shown, so an operator can see WHICH replicas disagree
    // rather than only that they do.
    expect(screen.getByRole("region", { name: "The keysets that came back" })).toBeTruthy();
  });

  it("counts a failed read as a failure, never as a disagreement", async () => {
    // The distinction that keeps an outage from being reported as divergence.
    h.state.failEvery = 2;
    await renderKeys();
    expect(screen.getByText(new RegExp(`of ${PROBE_READS} reads did not answer`))).toBeTruthy();
    expect(screen.queryByText(/different keysets came back/)).toBeNull();
  });

  it("explains an underivable identity origin rather than fetching a relative path", async () => {
    await renderKeys("owner", "");
    expect(screen.getByText(/publishes no identity origin/)).toBeTruthy();
  });

  it("says the rotation history was not asked for, rather than that there is none", async () => {
    // ABSENT HISTORY AND UN-ASKED HISTORY ARE DIFFERENT ANSWERS.
    // `recentAuditEvents` is owner-only, so an admin's empty result would read
    // as "no key has ever been rotated" -- told to exactly the person who
    // cannot check it.
    await renderKeys("admin");
    const rotation = screen.getByRole("region", { name: "Rotation" });
    expect(within(rotation).getByText(/not asked for here/)).toBeTruthy();
    expect(h.connection.query.recentAuditEvents).not.toHaveBeenCalled();
  });

  it("reads the rotation history for an owner", async () => {
    h.state.auditEvents = [
      {
        action: "jwks_rotated",
        outcome: "success",
        occurredAt: "2026-09-01T00:00:00Z",
        actorEmail: "owner@example.com",
      },
    ];
    await renderKeys("owner");
    const rotation = screen.getByRole("region", { name: "Rotation" });
    expect(within(rotation).getByText("2026-09-01T00:00:00Z")).toBeTruthy();
    expect(within(rotation).getByText("owner@example.com")).toBeTruthy();
  });

  it("offers no rotate control, and says why there is none", async () => {
    await renderKeys();
    expect(screen.queryByRole("button", { name: /Rotate/ })).toBeNull();
    expect(screen.getByText(/re-seal and a roll rather than a button/)).toBeTruthy();
  });
});
