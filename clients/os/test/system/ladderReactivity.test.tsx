import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type AccessSummary } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { Shell } from "../../src/chrome/Shell";
import { resetIdsForTest } from "../../src/system/desks";
import { setRoleLadder } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { SEEDED_LADDER } from "../seededLadder";

// THE WINDOW THE HARNESS SEEDS AWAY (memql#4857).
//
// ===========================================================================
// WHY EVERY OTHER ROLE TEST STAYS GREEN THROUGH THIS BUG
// ===========================================================================
// The role ladder is async cluster state (epic memql#4832): the shell reads
// `activeRoles` on boot and installs it into module-level state, and every
// app-gating surface calls roleAdmits, which reads that state. `test/setup.ts`
// calls setRoleLadder(SEEDED_LADDER) before every suite so the launcher tests
// measure something -- which means no suite ever renders the shell in the one
// state production always passes through: the signed-in ROLE resolved (a small
// fast read) while the LADDER has not (a slow one, measured at 30s+ cold).
//
// In that window roleAdmits refuses every gated app -- correctly fail-closed.
// The defect is that the launcher never recovers: its `apps` memo depends on
// [registry, actorRole, query], none of which change when the ladder finally
// lands, so the gated apps it computed as hidden stay hidden for the life of
// the session. The owner sees only the ungated apps; Diagnostics, which
// re-renders on the ladderLoaded context flip, says nothing is hidden.
//
// This test starts from the production cold state (ladder EMPTY) and drives
// the REAL load through the connection, then asserts the gated app appears
// with no query typed and no remount. It fails against the stale memo.

function summary(clusterRole: string): AccessSummary {
  return {
    requestId: "req-1",
    userId: "v1:identity:user:u-42",
    primaryEmail: "ada@example.test",
    sessionId: "sess-1",
    clusterRole,
  } as AccessSummary;
}

/** Rungs in the wire shape `activeRoles` returns (what rungsFrom reads). */
function ladderRows() {
  return SEEDED_LADDER.map((r) => ({
    slug: r.slug,
    name: r.name,
    rank: r.rank,
    aliases: r.aliases,
    active: true,
  }));
}

/**
 * A connection whose role read resolves immediately and whose ladder read
 * (`activeRoles`) is DEFERRED behind a gate we open by hand -- reproducing the
 * production ordering where the role lands before the ladder.
 */
function fakeConnection() {
  let openLadder!: () => void;
  const ladderGate = new Promise<void>((res) => {
    openLadder = res;
  });
  const stub = {
    getMyAccess: vi.fn(async () => summary("owner")),
    activeRoles: vi.fn(async () => {
      await ladderGate;
      return { rows: () => ladderRows(), meta: () => ({ cursor: "" }) };
    }),
    executeNamed: vi.fn(async () => ({ rows: () => [], meta: () => null })),
  };
  return {
    connection: {
      query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
      dispatcher: { sendAndWait: vi.fn() },
      subscriptions: { subscribeGraph: () => () => {} },
    },
    openLadder,
  };
}

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function mountShell() {
  resetIdsForTest();
  return render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      config={{ ...UNKNOWN_RUNTIME_CONFIG, domain: "example.test" }}
      ports={{ store: new LocalDesktopStore(memStorage()) }}
    />,
  );
}

async function launcherApps(): Promise<string[]> {
  const open = await screen.findByRole("button", { name: "Launcher" });
  fireEvent.click(open);
  const dialog = await screen.findByRole("dialog", { name: "Launcher" });
  const apps = within(dialog)
    .getAllByRole("button")
    .map((b) => b.textContent?.trim() ?? "")
    .filter(Boolean);
  // Close again so the next open re-queries fresh.
  fireEvent.keyDown(dialog, { key: "Escape" });
  return apps;
}

beforeEach(() => {
  // Start from the production COLD state, not the harness's pre-seeded one.
  setRoleLadder([]);
  h.connection = null;
});

afterEach(() => {
  // Restore the shared seed so sibling suites see production's default.
  setRoleLadder(SEEDED_LADDER);
});

describe("the launcher recovers when the role ladder lands after the role", () => {
  it("shows the admin-gated app once the ladder loads, with no query typed", async () => {
    const { connection, openLadder } = fakeConnection();
    h.connection = connection;
    mountShell();

    // The role has resolved (owner) but the ladder has not: fail-closed, the
    // gated app is hidden and the ungated one is present -- so an empty gated
    // set is evidence about the ladder, not about a shell that failed to draw.
    await waitFor(async () => {
      expect(await launcherApps()).toContain("Settings");
    });
    expect(await launcherApps()).not.toContain("Users");

    // The ladder read lands. Nothing the launcher's memo depended on changed;
    // only the ladder did. The gated app must now appear.
    openLadder();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
  });
});
