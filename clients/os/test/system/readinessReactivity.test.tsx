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

// THE FRAME A CONFIGURED CLUSTER MUST NOT FLASH A SETUP SCREEN IN.
//
// ===========================================================================
// WHY A UNIT TEST OF gateFor IS NOT ENOUGH
// ===========================================================================
// gateFor's "unknown" arm is trivially correct in isolation. What this covers
// is the wiring around it: that the window frame actually consults readiness,
// that it draws NOTHING while the feed is in flight, and that it flips without
// a remount when the rows land. The failure it exists to catch is the one the
// role ladder already had once -- a memo that computes an answer against
// empty state and never recomputes, because nothing it named in its deps
// changed when the real data arrived.
//
// So the readiness read is DEFERRED behind a gate this test opens by hand,
// reproducing the production ordering: the window opens first and the rows
// land after.

function summary(role: string): AccessSummary {
  return {
    requestId: "req-1",
    userId: "v1:identity:user:u-42",
    primaryEmail: "ada@example.test",
    sessionId: "sess-1",
    role,
  } as AccessSummary;
}

function ladderRows() {
  return SEEDED_LADDER.map((r) => ({
    slug: r.slug,
    name: r.name,
    rank: r.rank,
    aliases: r.aliases,
    active: true,
  }));
}

function readinessRow(module: string, state: string) {
  return {
    id: `v1:platform:moduleReadiness:${module}--bff-a`,
    module,
    nodeId: "bff-a",
    nodeType: "bff",
    state,
    core: true,
    lanes: [],
    reportedAt: "2026-09-06T11:58:00Z",
  };
}

function nodeRow() {
  return {
    id: "v1:cluster:node:bff-a",
    nodeType: "bff",
    health: "healthy",
    lastSeen: new Date().toISOString(),
  };
}

function fakeConnection(readinessRows: unknown[]) {
  let openReadiness!: () => void;
  const gate = new Promise<void>((res) => {
    openReadiness = res;
  });
  const stub = {
    getMyAccess: vi.fn(async () => summary("owner")),
    activeRoles: vi.fn(async () => ({ rows: () => ladderRows(), meta: () => ({ cursor: "" }) })),
    executeNamed: vi.fn(async (name: string) => {
      if (name === "moduleReadinessAll") {
        await gate;
        return { rows: () => readinessRows, meta: () => null };
      }
      if (name === "staleClusterNodes") {
        return { rows: () => [nodeRow()], meta: () => null };
      }
      return { rows: () => [], meta: () => null };
    }),
  };
  return {
    connection: {
      query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
      dispatcher: { sendAndWait: vi.fn() },
      subscriptions: { subscribeGraph: () => () => {} },
    },
    openReadiness,
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

async function openApp(name: string) {
  const open = await screen.findByRole("button", { name: "Launcher" });
  fireEvent.click(open);
  const dialog = await screen.findByRole("dialog", { name: "Launcher" });
  fireEvent.click(within(dialog).getByRole("button", { name: new RegExp(`^(?:Unseen change )?${name}$`) }));
}

beforeEach(() => {
  setRoleLadder(SEEDED_LADDER);
  h.connection = null;
});

afterEach(() => {
  setRoleLadder(SEEDED_LADDER);
});

// THE CORE GATE, THROUGH THE REAL SHELL (epic memql#5118).
//
// This is the one case that pins the COMPOSITION rather than the component.
// `CoreGate`'s own suite mounts it inside a shell provider by hand, so every
// case there passed while `Shell.tsx` mounted the real one OUTSIDE
// `ShellRoster` -- where `useAppReach` reads a null shell, every section list
// is empty, and the inference stop's act degrades to prose pointing at an app
// the gate has not mounted. On a local cluster, which reaches no federation by
// design, that left an owner with Sign out as the only working control on a
// screen demanding they set up inference.
//
// Nothing but the real `<Shell>` can catch that, which is why it lives here
// beside the other case that exists for a wiring failure a unit test cannot
// see.
describe("the core gate, mounted by the real shell", () => {
  it("holds an owner and offers an act that can actually run", async () => {
    const { connection, openReadiness } = fakeConnection([
      readinessRow("ai", "unconfigured"),
      readinessRow("storage", "configured"),
    ]);
    h.connection = connection;
    mountShell();

    // Before the rows land the shell OPENS -- only positive evidence holds
    // anybody, and this is the frame every configured cluster passes through.
    expect(await screen.findByRole("button", { name: "Launcher" })).toBeTruthy();

    openReadiness();

    // The gate takes the screen.
    await waitFor(() => {
      expect(screen.getByRole("list", { name: "Set up this cluster" })).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Launcher" })).toBeNull();
    expect(document.querySelector("[data-os-dock]")).toBeNull();

    // AND THE ACT WORKS. A button, not the words-only fallback -- which is the
    // whole finding: `useAppReach` has to reach a real shell from in here.
    expect(await screen.findByRole("button", { name: "Open Fleet" })).toBeTruthy();
    expect(screen.queryByText(/Pair a machine in Fleet, under Machines/)).toBeNull();

    // And taking it opens the app, over a desk the gate steps aside for.
    fireEvent.click(screen.getByRole("button", { name: "Open Fleet" }));
    await waitFor(() => {
      expect(screen.getByRole("dialog", { name: "Fleet" })).toBeTruthy();
    });
  });

  it("gives a reader the sentence, and no rail", async () => {
    const { connection, openReadiness } = fakeConnection([readinessRow("ai", "unconfigured")]);
    connection.query.getMyAccess = vi.fn(async () => summary("reader"));
    h.connection = connection;
    mountShell();
    openReadiness();

    await waitFor(() => {
      expect(
        screen.getByText("An owner or developer has to set up inference before anyone can use it."),
      ).toBeTruthy();
    });
    expect(screen.queryByRole("list", { name: "Set up this cluster" })).toBeNull();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
  });
});

describe("an unconfigured app gates only once readiness has loaded", () => {
  it("keeps preparation open and gates Rules once sending readiness has loaded", async () => {
    const { connection, openReadiness } = fakeConnection([
      readinessRow("email", "unconfigured"),
      readinessRow("campaigns", "configured"),
    ]);
    h.connection = connection;
    mountShell();
    await openApp("Campaigns");
    const win = await screen.findByRole("dialog", { name: "Campaigns" });
    fireEvent.click(within(win).getByRole("button", { name: "Rules" }));

    // Unknown: nothing gated, nothing drawn. This is the frame the test
    // exists for -- a configured cluster passes through it on every load.
    expect(within(win).queryByText(/is not set up yet/)).toBeNull();
    expect(within(win).queryByRole("button", { name: /is not set up/ })).toBeNull();

    openReadiness();

    await waitFor(() => {
      expect(within(win).getByRole("heading", { name: "Rules is not set up yet" })).toBeTruthy();
    });
    // TWO marks, and both are wanted: the title-bar gear and the rail's own
    // Settings entry. A person looking at the window chrome and a person
    // reading the section list are looking in different places.
    //
    // Asserted through the BUTTONS' accessible names rather than the dots'.
    // The dot is decorative inside a button -- a labelled role="img" nested in
    // one appends to that button's name -- so the button is what has to say it,
    // and the button's name is what a screen reader actually announces.
    const settingsEntry = within(win).getByRole("button", {
      name: "Campaigns settings, not set up",
    });
    expect(within(win).getByRole("button", { name: "Campaigns settings, not set up" })).toBeTruthy();

    // The rail is intact and Settings still answers: an app must never gate
    // the one place its own repair lives.
    fireEvent.click(settingsEntry);
    await waitFor(() => {
      expect(within(win).queryByText(/is not set up yet/)).toBeNull();
    });
    // And the mark STAYS while they stand in Settings fixing it -- it is
    // computed from the app's requirements, not the current section's.
    expect(
      within(win).getByRole("button", { name: "Campaigns settings, not set up" }),
    ).toBeTruthy();
  });

  it("draws nothing on a configured app", async () => {
    const { connection, openReadiness } = fakeConnection([
      readinessRow("email", "configured"),
      readinessRow("campaigns", "configured"),
    ]);
    h.connection = connection;
    mountShell();
    await openApp("Campaigns");
    openReadiness();
    const win = await screen.findByRole("dialog", { name: "Campaigns" });
    await waitFor(() =>
      expect(connection.query.executeNamed).toHaveBeenCalledWith(
        "moduleReadinessAll",
        expect.anything(),
        expect.anything(),
      ),
    );
    expect(within(win).queryByText(/is not set up yet/)).toBeNull();
    expect(within(win).queryByRole("button", { name: /is not set up/ })).toBeNull();
    // The gear keeps its plain name when there is nothing to report.
    expect(within(win).getByRole("button", { name: "Campaigns settings" })).toBeTruthy();
  });
});
