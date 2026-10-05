import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type AccessSummary, type Row } from "@znasllc-io/memql-sdk-core/client";

// THE MARK UNDER THE GEAR FOR AN OPTIONAL ITEM (epic memql#5479, design
// record D15).
//
// ===========================================================================
// WHY THE REAL SHELL
// ===========================================================================
// The publisher is pure enough to unit-test, and it is, below. What it cannot
// show alone is the composition that decides whether the mark is RIGHT: that
// the shell mounts it inside the attention provider, that the dock's Settings
// icon draws it with the attention dot the dock already carries, that the
// effective capability set decides who sees it, and -- the trap -- that the
// window frame, which acknowledges every unseen change aimed at the section it
// shows, does not acknowledge this one when the owner opens Settings >
// Pipelines. Only "Not now" may.
//
// The dot on the Settings icon is shared with the registry's own FEATURE
// changes (Settings declares three), so every assertion reads the dot's title
// -- the labels it carries -- rather than whether a dot is there at all.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { Shell } from "../../src/chrome/Shell";
import {
  OPTIONAL_READINESS_REVISION,
  OPTIONAL_READINESS_TARGET,
  optionalReadinessChanges,
  optionalReadinessId,
} from "../../src/chrome/OptionalReadiness";
import { OS_REGISTRY } from "../../src/apps/registry";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import type { Readiness } from "../../src/live/readiness";
import { resetIdsForTest } from "../../src/system/desks";
import type { Verdict } from "../../src/system/readinessFold";
import { setRoleLadder } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { StubAskTransport } from "../ask/stubTransport";
import { rowsResult } from "../deployables/harness";
import { installSeededAccess } from "../seededAccess";
import { SEEDED_LADDER } from "../seededLadder";

const LABEL = "Pipelines can be set up";
const NOT_NOW_CALL = 'mutation acknowledgeAttention(changeId: "readiness:pipelines", revision: "optional-1")';

// ---------------------------------------------------------------------------
// The publisher, pure
// ---------------------------------------------------------------------------

function verdict(module: string, state: Verdict["state"], flags: { optional?: boolean; dismissable?: boolean } = {}): Verdict {
  return {
    module,
    state,
    core: false,
    optional: flags.optional ?? true,
    dismissable: flags.dismissable ?? true,
    disagreement: [],
    nodes: [],
    lanes: [],
    unknown: [],
    stale: [],
    aside: [],
  };
}

function readiness(verdicts: Verdict[], loaded = true): Readiness {
  const by = new Map(verdicts.map((v) => [v.module, v]));
  return { loaded, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

const registry = { apps: OS_REGISTRY.apps, widgets: OS_REGISTRY.widgets };

describe("which optional items are published", () => {
  afterEach(() => installSeededAccess("owner"));

  it("publishes an open item, aimed past the section, for a viewer who may open it", () => {
    installSeededAccess("owner");
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured")]), registry)).toEqual([
      {
        id: "readiness:pipelines",
        revision: "optional-1",
        appId: "settings",
        sectionId: "pipelines",
        target: "readiness",
        label: LABEL,
        kind: "runtime",
      },
    ]);
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "partial")]), registry)).toHaveLength(1);
    expect([optionalReadinessId("pipelines"), OPTIONAL_READINESS_REVISION, OPTIONAL_READINESS_TARGET]).toEqual([
      "readiness:pipelines",
      "optional-1",
      "readiness",
    ]);
  });

  it("publishes nothing for a viewer without read app:settings/pipelines", () => {
    installSeededAccess("developer");
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured")]), registry)).toEqual([]);
    installSeededAccess("admin");
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured")]), registry)).toEqual([]);
  });

  it("publishes nothing for an item that is done, not known, not optional, not dismissable or not loaded", () => {
    installSeededAccess("owner");
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "configured")]), registry)).toEqual([]);
    // Nobody reported it: not known to be unset, so no claim the owner has work.
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unreported")]), registry)).toEqual([]);
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured", { optional: false })]), registry)).toEqual([]);
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured", { dismissable: false })]), registry)).toEqual([]);
    expect(optionalReadinessChanges(readiness([verdict("pipelines", "unconfigured")], false), registry)).toEqual([]);
    expect(optionalReadinessChanges(undefined, registry)).toEqual([]);
    // An optional item with nowhere to be set up from has nowhere to point.
    expect(optionalReadinessChanges(readiness([verdict("storage", "unconfigured")]), registry)).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Through the real shell
// ---------------------------------------------------------------------------

function summary(role: string): AccessSummary {
  return { requestId: "req-1", userId: "v1:identity:user:u-42", primaryEmail: "ada@example.test", sessionId: "sess-1", role } as AccessSummary;
}

function ladderRows() {
  return SEEDED_LADDER.map((r) => ({ slug: r.slug, name: r.name, rank: r.rank, aliases: r.aliases, active: true }));
}

/** One agent's pipelines row: optional, dismissable, and its report lane. */
function pipelinesRow(state: string) {
  return {
    id: "v1:platform:moduleReadiness:pipelines--agent-a",
    module: "pipelines",
    nodeId: "agent-a",
    nodeType: "agent",
    state,
    core: false,
    optional: true,
    dismissable: true,
    lanes: [
      {
        name: "report",
        configurableFrom: "os",
        complete: state === "configured",
        slots: [
          { name: "githubApp", present: state === "configured", source: state === "configured" ? "set" : "" },
          { name: "repository", present: state === "configured", source: state === "configured" ? "set" : "" },
          { name: "runner", present: true, source: "set" },
        ],
      },
    ],
    reportedAt: "2026-10-04T08:00:00Z",
  };
}

function fakeShellConnection({ role, state, receipts = [] }: { role: string; state: string; receipts?: Row[] }) {
  let openReadiness!: () => void;
  const gate = new Promise<void>((resolve) => {
    openReadiness = resolve;
  });
  const acknowledged: string[] = [];
  const stub = {
    getMyAccess: vi.fn(async () => summary(role)),
    activeRoles: vi.fn(async () => ({ rows: () => ladderRows(), meta: () => ({ cursor: "" }) })),
    // Typed `unknown`: it answers two wire shapes (a projected Result and a
    // bare `rows()` reply) and is handed over as a QueryClient by a cast.
    executeNamed: vi.fn(async (name: string, call: string): Promise<unknown> => {
      if (name === "moduleReadinessAll") {
        await gate;
        return { rows: () => [pipelinesRow(state)], meta: () => null };
      }
      if (name === "staleClusterNodes") {
        return {
          rows: () => [{ id: "v1:cluster:node:agent-a", nodeType: "agent", health: "healthy", lastSeen: new Date().toISOString() }],
          meta: () => null,
        };
      }
      if (name === "myAttentionReceipts") return rowsResult(receipts);
      if (name === "acknowledgeAttention") {
        acknowledged.push(call);
        return rowsResult([]);
      }
      return { rows: () => [], meta: () => null };
    }),
  };
  return {
    connection: {
      query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
      dispatcher: { sendAndWait: vi.fn() },
      subscriptions: { subscribeGraph: () => () => {} },
      // What the Settings app's connection history reads off the connection
      // the moment a Settings window opens.
      nodeId: "bff-test",
      engineVersion: "v9.9.9",
      engineCommit: "abcdef",
      onStatusChange: () => () => {},
    },
    stub,
    acknowledged,
    openReadiness,
  };
}

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function mountShell(role: string, state: string, receipts: Row[] = []) {
  const fake = fakeShellConnection({ role, state, receipts });
  h.connection = fake.connection;
  // The effective set the cluster would answer for this role. The fake answers
  // the read with nothing, which installs nothing, so this set stands.
  installSeededAccess(role);
  resetIdsForTest();
  render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      config={{ ...UNKNOWN_RUNTIME_CONFIG, domain: "example.test" }}
      ports={{ store: new LocalDesktopStore(memStorage()), askTransport: new StubAskTransport(), askVoice: null }}
    />,
  );
  return fake;
}

/** The labels on the dock's Settings icon's dot; "" when it carries none. */
function settingsDotLabels(): string {
  const dock = screen.getByRole("toolbar", { name: "Apps" });
  const settings = within(dock).getByRole("button", { name: /^Settings/ });
  return settings.querySelector(".os-attention-dot")?.getAttribute("title") ?? "";
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 5; i++) await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

/** Wait for the two reads every mark depends on, then for what follows them. */
async function readinessAndReceiptsRead(fake: ReturnType<typeof fakeShellConnection>) {
  await waitFor(() => {
    const names = fake.stub.executeNamed.mock.calls.map(([name]) => name);
    expect(names).toContain("myAttentionReceipts");
    expect(names).toContain("staleClusterNodes");
  });
  await settle();
}

describe("the mark on the Settings icon, through the real shell", () => {
  beforeEach(() => {
    setRoleLadder(SEEDED_LADDER);
    h.connection = null;
  });
  afterEach(() => {
    setRoleLadder(SEEDED_LADDER);
    installSeededAccess("owner");
  });

  it("marks Settings for an owner while pipelines is optional and not set up", async () => {
    const fake = mountShell("owner", "unconfigured");
    await readinessAndReceiptsRead(fake);
    // Let the bounded entry wait expire while the feed remains unavailable.
    await screen.findByRole("toolbar", { name: "Apps" }, { timeout: 4000 });
    expect(settingsDotLabels()).not.toContain(LABEL);
    fake.openReadiness();
    await waitFor(() => expect(settingsDotLabels()).toContain(LABEL));
  });

  it("does not mark it for a developer, who holds no read app:settings/pipelines", async () => {
    const fake = mountShell("developer", "unconfigured");
    fake.openReadiness();
    await readinessAndReceiptsRead(fake);
    await waitFor(() =>
      expect(fake.stub.executeNamed.mock.calls.some(([name]) => name === "moduleReadinessAll")).toBe(true),
    );
    await settle();
    // The Settings icon is there, carrying the registry's own changes -- just
    // not this one.
    expect(within(screen.getByRole("toolbar", { name: "Apps" })).getByRole("button", { name: /^Settings/ })).toBeTruthy();
    expect(settingsDotLabels()).not.toContain(LABEL);
  });

  it("does not mark it once pipelines is set up", async () => {
    const fake = mountShell("owner", "configured");
    fake.openReadiness();
    await readinessAndReceiptsRead(fake);
    await settle();
    expect(settingsDotLabels()).not.toContain(LABEL);
  });

  it("does not mark it for an owner who already answered Not now", async () => {
    const fake = mountShell("owner", "unconfigured", [
      { id: "receipt-1", changeId: "readiness:pipelines", revision: "optional-1" } as unknown as Row,
    ]);
    // The receipts are in BEFORE the item is known open, so a mark could only
    // appear by ignoring them -- never as a frame before they land.
    await readinessAndReceiptsRead(fake);
    fake.openReadiness();
    await settle();
    await settle();
    expect(settingsDotLabels()).not.toContain(LABEL);
  });

  it("is not acknowledged by opening Settings > Pipelines; Not now clears it and the section stays", async () => {
    const fake = mountShell("owner", "unconfigured");
    fake.openReadiness();
    await waitFor(() => expect(settingsDotLabels()).toContain(LABEL));

    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = await screen.findByRole("dialog", { name: "Launcher" });
    fireEvent.click(within(launcher).getByRole("button", { name: /^(?:Unseen change )?Settings$/ }));
    const win = await screen.findByRole("dialog", { name: "Settings" });
    fireEvent.change(within(win).getByRole("combobox", { name: "More Settings sections" }), { target: { value: "pipelines" } });
    expect(await within(win).findByRole("heading", { name: "Pipelines" })).toBeTruthy();

    // STANDING ON THE SECTION IS NOT AN ANSWER.
    await settle();
    expect(fake.acknowledged.filter((call) => call.includes('"readiness:pipelines"'))).toEqual([]);
    expect(settingsDotLabels()).toContain(LABEL);

    fireEvent.click(within(win).getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(settingsDotLabels()).not.toContain(LABEL));
    expect(fake.acknowledged).toEqual([NOT_NOW_CALL]);
    expect(within(win).queryByRole("button", { name: "Not now" })).toBeNull();
    expect(within(win).getByRole("heading", { name: "Pipelines" })).toBeTruthy();
  });
});
