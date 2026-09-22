import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { clearEffectiveCapabilities, notifyGrantWritten, setRoleLadder } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { installSeededAccess, seededAccessFor } from "../seededAccess";
import { SEEDED_LADDER } from "../seededLadder";

// THE WINDOW THE HARNESS SEEDS AWAY (memql#4857), FOR THE EFFECTIVE SET (epic
// memql#5289, D10/D11).
//
// ===========================================================================
// WHY EVERY OTHER ROLE TEST STAYS GREEN THROUGH THIS BUG
// ===========================================================================
// The effective capability set is async cluster state: the shell reads
// `effectiveCapabilitiesForActor` after the identity and installs it into
// module-level state, and every app-gating surface reads that state out of
// band. `test/setup.ts` installs the owner's seeded set before every suite so
// the launcher tests measure something -- which means no suite renders the
// shell in the one state production always passes through: the identity
// resolved while the SET has not.
//
// In that window `holds` refuses everything -- correctly fail-closed. The
// defect this guards against is the launcher never recovering: a memo keyed
// only on [registry, query] would compute every app as hidden and never
// recompute when the set lands. `accessEpoch` is what it must depend on, and
// this test is what fails if it does not.
//
// The two RE-READ triggers (D11) are pinned here too: a window focus and a
// grant written from this browser both ask the cluster again, and the
// launcher follows the new answer without a query typed and without a
// remount. Grant rows are cluster-owner tier and never broadcast, so these
// two are the only ways a person's shell learns of a grant written for them.

function summary(role: string): AccessSummary {
  return {
    requestId: "req-1",
    userId: "v1:identity:user:u-42",
    primaryEmail: "ada@example.test",
    sessionId: "sess-1",
    role,
  } as AccessSummary;
}

function effectiveRow(role: string, extra: { verb: string; resource: string; effect: "allow" | "deny"; source: string }[] = []) {
  return { ok: true, code: "", role, userId: "u-42", entries: [...seededAccessFor(role), ...extra] };
}

/**
 * A connection whose identity read resolves immediately and whose EFFECTIVE
 * read is DEFERRED behind a gate we open by hand -- reproducing the
 * production ordering where the identity lands before the set. Each open
 * answers the next reply in `answers`, so a re-read can be told apart from
 * the first read by what it returns.
 */
function fakeConnection(answers: (() => ReturnType<typeof effectiveRow>)[]) {
  let openGate!: () => void;
  let gate = new Promise<void>((res) => {
    openGate = res;
  });
  let calls = 0;
  let refuseNext = false;
  const stub = {
    getMyAccess: vi.fn(async () => summary("owner")),
    activeRoles: vi.fn(async () => ({
      rows: () => SEEDED_LADDER.map((r) => ({ ...r, active: true })),
      meta: () => ({ cursor: "" }),
    })),
    executeNamed: vi.fn(async (name: string) => {
      if (name !== "effectiveCapabilitiesForActor") return { rows: () => [], meta: () => null };
      await gate;
      const answer = answers[Math.min(calls, answers.length - 1)]!;
      calls += 1;
      if (refuseNext) {
        refuseNext = false;
        throw new Error("stream refused");
      }
      return { rows: () => [answer()], meta: () => null };
    }),
  };
  return {
    connection: {
      query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
      dispatcher: { sendAndWait: vi.fn() },
      subscriptions: { subscribeGraph: () => () => {} },
    },
    open: () => openGate(),
    /** Close the gate again so the NEXT read waits until `open` is called. */
    rearm: () => {
      gate = new Promise<void>((res) => {
        openGate = res;
      });
    },
    effectiveCalls: () => calls,
    /** Make the NEXT effective read fail. */
    refuseOnce: () => {
      refuseNext = true;
    },
    stub,
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
  clearEffectiveCapabilities();
  setRoleLadder(SEEDED_LADDER);
  h.connection = null;
});

afterEach(() => {
  // Restore the shared seed so sibling suites see production's default.
  installSeededAccess("owner");
});

describe("the launcher recovers when the effective set lands after the identity", () => {
  it("discovers an organization-authorized app and removes it when that permission is revoked", async () => {
    const denied = [{ verb: "read", resource: "app:campaigns", effect: "deny" as const, source: "group" as const }];
    const fake = fakeConnection([
      () => ({ ...effectiveRow("viewer"), entries: denied, organizationEntries: [
        { accountId: "acme", verb: "read", resource: "app:campaigns", effect: "allow" },
        { accountId: "beta", verb: "read", resource: "app:campaigns", effect: "deny" },
      ] }),
      () => ({ ...effectiveRow("viewer"), entries: denied, organizationEntries: [
        { accountId: "beta", verb: "read", resource: "app:campaigns", effect: "deny" },
      ] }),
    ]);
    h.connection = fake.connection;
    mountShell();
    await screen.findByRole("button", { name: "Launcher" });
    expect(await launcherApps()).not.toContain("Campaigns");
    await act(async () => fake.open());
    await waitFor(() => expect(fake.effectiveCalls()).toBe(1));
    expect(await launcherApps()).toContain("Campaigns");
    fake.rearm();
    fireEvent.focus(window);
    await act(async () => fake.open());
    await waitFor(() => expect(fake.effectiveCalls()).toBe(2));
    expect(await launcherApps()).not.toContain("Campaigns");
  });

  it("shows the gated app once the set loads, with no query typed", async () => {
    const fake = fakeConnection([() => effectiveRow("owner")]);
    h.connection = fake.connection;
    mountShell();

    // The identity has resolved (owner) but the set has not: fail-closed,
    // every app is hidden and the Launcher itself is drawn -- so an empty
    // launcher is evidence about the set, not about a shell that failed to
    // draw.
    await screen.findByRole("button", { name: "Launcher" });
    expect(await launcherApps()).not.toContain("Users");

    // The read lands. Nothing the launcher's memo depended on changed but the
    // epoch. The gated app must now appear.
    fake.open();

    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });
  });
});

describe("the two re-reads (D11)", () => {
  it("asks again on window focus, and follows the new answer", async () => {
    // A writer whose second answer carries a grant to Users by name.
    const fake = fakeConnection([
      () => effectiveRow("writer"),
      () => effectiveRow("writer", [{ verb: "read", resource: "app:users", effect: "allow", source: "user" }]),
    ]);
    h.connection = fake.connection;
    mountShell();
    fake.open();
    await waitFor(async () => {
      expect(await launcherApps()).toContain("Training");
    });
    expect(await launcherApps()).not.toContain("Users");
    expect(fake.effectiveCalls()).toBe(1);

    // Somebody granted this person Users while they were in another tab.
    fireEvent(window, new Event("focus"));

    await waitFor(async () => {
      expect(fake.effectiveCalls()).toBe(2);
      expect(await launcherApps()).toContain("Users");
    });
  });

  it("asks again after a grant written from this browser", async () => {
    const fake = fakeConnection([
      () => effectiveRow("owner"),
      () => effectiveRow("owner", [{ verb: "read", resource: "app:users", effect: "deny", source: "user" }]),
    ]);
    h.connection = fake.connection;
    mountShell();
    fake.open();
    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });

    // The Access screen wrote a grant; it tells the session scope so.
    notifyGrantWritten();

    await waitFor(async () => {
      expect(fake.effectiveCalls()).toBe(2);
      expect(await launcherApps()).not.toContain("Users");
    });
  });

  it("keeps the set it holds when a re-read is refused", async () => {
    const fake = fakeConnection([() => effectiveRow("owner")]);
    h.connection = fake.connection;
    mountShell();
    fake.open();
    await waitFor(async () => {
      expect(await launcherApps()).toContain("Users");
    });

    // The next read fails outright. Emptying the set would blank the
    // launcher over one dropped call and read as access being taken away.
    fake.refuseOnce();
    fireEvent(window, new Event("focus"));
    await waitFor(() => {
      expect(fake.effectiveCalls()).toBe(2);
    });
    expect(await launcherApps()).toContain("Users");
  });
});
