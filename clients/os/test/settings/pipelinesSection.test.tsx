import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// Settings -> Pipelines (epic memql#5479, design record D15).
//
// ===========================================================================
// WHAT THIS PINS
// ===========================================================================
// The three sub-steps are the readiness row's own facts, and "not known" is
// never drawn as "not done". The GitHub App is set up through the cluster's
// app manifest, offered only to somebody the cluster says may. An installation
// whose permissions lag the app is named with GitHub's own page, and a read
// that failed says so. "Not now" writes exactly one receipt and nothing else
// does -- not even standing on the section, which every case here does: the
// section is mounted inside a VISIBLE `AttentionDestination` for its own
// section, exactly as the window frame mounts every app body.
//
// The connection is faked under `executeNamed` (the Deployables harness), so
// the generated builders render the call strings asserted below.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import { AttentionDestination, AttentionMarker, AttentionProvider } from "../../src/attention/Attention";
import { SessionProvider } from "../../src/chrome/access";
import { OptionalReadiness } from "../../src/chrome/OptionalReadiness";
import { OsProvider, useOs } from "../../src/chrome/state";
import { OS_REGISTRY } from "../../src/apps/registry";
import { PIPELINES_RETURN_PATH, PipelinesSection } from "../../src/apps/settings/PipelinesSection";
import {
  fleetSentence,
  permissionWords,
  readPipelines,
} from "../../src/apps/settings/pipelinesReadiness";
import { machinesAllowingPipelines } from "../../src/apps/deployables/pipelines/fleet";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import type { Readiness } from "../../src/live/readiness";
import { MachinesProvider } from "../../src/live/machines";
import { resetIdsForTest } from "../../src/system/desks";
import { MODULE_DESCRIPTIONS } from "../../src/system/modules";
import type { Verdict } from "../../src/system/readinessFold";
import type { OsAppProps } from "../../src/system/registry";
import { fakeConnection, rowsResult, type FakeConnection, type FakeSeed } from "../deployables/harness";
import { installSeededAccess } from "../seededAccess";

const NOT_NOW_CALL = 'mutation acknowledgeAttention(changeId: "readiness:pipelines", revision: "optional-1")';

/** The registry with no FEATURE declarations, so the only change a Settings
 *  mark can carry here is the runtime one under test. */
const RUNTIME_ONLY = OS_REGISTRY.apps.map((app) => ({ ...app, attentionChanges: [] }));

type Slots = Partial<Record<"githubApp" | "repository" | "runner", boolean>>;

/** The pipelines verdict, with the report lane when `slots` is given. */
function pipelines(
  state: Verdict["state"],
  slots: Slots | null,
  flags: { optional?: boolean; dismissable?: boolean } = {},
): Verdict {
  const lanes =
    slots === null
      ? []
      : [
          {
            name: "report",
            configurableFrom: "os",
            complete: state === "configured",
            slots: Object.entries(slots).map(([name, present]) => ({ name, present: present === true, source: present ? "set" : "" })),
          },
        ];
  return {
    module: "pipelines",
    state,
    core: false,
    optional: flags.optional ?? true,
    dismissable: flags.dismissable ?? true,
    disagreement: [],
    nodes: [],
    lanes,
    unknown: [],
    stale: [],
    aside: [],
  };
}

function readinessOf(verdicts: Verdict[]): Readiness {
  const by = new Map(verdicts.map((v) => [v.module, v]));
  return { loaded: true, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

function machine(id: string, over: Record<string, unknown> = {}): Row {
  return { id, ownerUserId: "u-me", name: id, capabilityDescriptor: { actionContracts: { "workerHost.pipeline_step": 2 } }, labels: { pipelines: "allowed" }, revokedAt: "", ...over } as unknown as Row;
}

interface Seed extends FakeSeed {
  machines?: Row[];
  receipts?: Row[];
  ackError?: string;
}

interface Wire {
  fake: FakeConnection;
  /** Every acknowledgeAttention call string that reached the wire. */
  acknowledged: string[];
}

/** The Deployables fake, with the attention and machines reads answered too. */
function connect(seed: Seed = {}): Wire {
  const fake = fakeConnection(seed);
  const base = fake.query.executeNamed.bind(fake.query);
  const acknowledged: string[] = [];
  (fake.query as unknown as { executeNamed: unknown }).executeNamed = vi.fn(
    async (name: string, call: string, opts?: { cursor?: string; signal?: AbortSignal }) => {
      if (call === "query myAttentionReceipts()") return rowsResult(seed.receipts ?? []);
      if (call.startsWith("mutation acknowledgeAttention(")) {
        acknowledged.push(call);
        if (seed.ackError !== undefined) throw new Error(seed.ackError);
        return rowsResult([]);
      }
      if (call === "query myWorkersWithStatus()") return rowsResult(seed.machines ?? []);
      return base(name, call, opts as never);
    },
  );
  h.connection = { query: fake.query, subscriptions: fake.subscriptions, dispatcher: fake.dispatcher };
  return { fake, acknowledged };
}

/** What the shell holds, read off the provider the section really mounts under. */
function WindowsProbe() {
  const { state } = useOs();
  return <pre data-testid="windows">{JSON.stringify(Object.values(state.shell.windows).map((w) => [w.appId, w.sectionId]))}</pre>;
}

function mount(verdict: Verdict | null, { role = "owner", intent, consumeIntent }: { role?: string } & Pick<OsAppProps, "intent" | "consumeIntent"> = {}) {
  installSeededAccess(role);
  return render(
    <SessionProvider
      value={{
        access: { userId: "u-me", primaryEmail: "owner@example.com", role, roleName: "", rank: 0 },
        config: UNKNOWN_RUNTIME_CONFIG,
        ladderLoaded: true,
        readiness: readinessOf(verdict === null ? [] : [verdict]),
      }}
    >
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }}>
        <AttentionProvider apps={RUNTIME_ONLY}>
          <MachinesProvider>
            <OptionalReadiness />
            <span data-testid="settings-mark">
              <AttentionMarker appId="settings" />
            </span>
            {/* As the window frame mounts every section body: a visible
                destination for the section itself. */}
            <AttentionDestination appId="settings" sectionId="pipelines">
              <PipelinesSection intent={intent} consumeIntent={consumeIntent} />
            </AttentionDestination>
            <WindowsProbe />
          </MachinesProvider>
        </AttentionProvider>
      </OsProvider>
    </SessionProvider>,
  );
}

function stops(): HTMLElement[] {
  return within(screen.getByRole("list", { name: "What pipelines need" })).getAllByRole("listitem");
}

function stopStates(): (string | null)[] {
  return stops().map((li) => li.getAttribute("data-state"));
}

function bar(): HTMLElement {
  return screen.getByRole("group", { name: "What you can do with this" });
}

function settingsMark(): HTMLElement | null {
  return within(screen.getByTestId("settings-mark")).queryByRole("img", { name: "Unseen change" });
}

/** Let every pending read settle -- a negative is only worth asserting after. */
async function settle() {
  await act(async () => {
    for (let i = 0; i < 5; i++) await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

beforeEach(() => {
  resetIdsForTest();
  h.connection = null;
});

afterEach(() => {
  installSeededAccess("owner");
});

// ---------------------------------------------------------------------------
// The reading
// ---------------------------------------------------------------------------

describe("the pipelines reading", () => {
  it("reads each sub-step from the report lane", () => {
    const r = readPipelines(pipelines("partial", { githubApp: true, repository: false, runner: true }));
    expect(r.steps.map((s) => [s.name, s.reading])).toEqual([
      ["GitHub App", "done"],
      ["A repository", "notDone"],
      ["Compute", "done"],
    ]);
    expect(r.state).toBe("partlySetUp");
    expect(r.words).toBe("Partly set up");
    expect(r.detail).toBe("2 of 3 done");
    expect(readPipelines(pipelines("unconfigured", { githubApp: false, repository: false, runner: false })).words).toBe("Not set up");
  });

  // NO LANE IS NOT "NOT DONE": an older engine, or nobody reporting.
  it("never invents a not done when the lane is absent", () => {
    const unreported = readPipelines(pipelines("unreported", null));
    expect(unreported.steps.map((s) => s.reading)).toEqual(["notKnown", "notKnown", "notKnown"]);
    expect([unreported.words, unreported.detail, unreported.done]).toEqual(["Not reported", "", null]);
    expect(readPipelines(null).words).toBe("Not reported");

    const configured = readPipelines(pipelines("configured", null));
    expect(configured.steps.map((s) => s.reading)).toEqual(["done", "done", "done"]);
    expect([configured.words, configured.detail]).toEqual(["Set up", "3 of 3 done"]);

    // Reported as not set up, but not which: the item says so, and no stop
    // claims to be the missing one.
    const unconfigured = readPipelines(pipelines("unconfigured", null));
    expect(unconfigured.steps.map((s) => s.reading)).toEqual(["notKnown", "notKnown", "notKnown"]);
    expect([unconfigured.words, unconfigured.detail]).toEqual(["Not set up", ""]);
  });

  it("leaves a slot the lane does not carry not known", () => {
    const r = readPipelines(pipelines("partial", { githubApp: true }));
    expect(r.steps.map((s) => s.reading)).toEqual(["done", "notKnown", "notKnown"]);
    expect([r.words, r.detail]).toEqual(["Partly set up", ""]);
  });

  it("says could not check, in the kit's words, when the nodes answered and none could", () => {
    const v = pipelines("unreported", null);
    v.unknown = ["agent-a"];
    expect(readPipelines(v).words).toBe("Could not check");
  });

  it("reads a missing permission aloud", () => {
    expect(permissionWords("checks:write")).toBe("write checks");
    expect(permissionWords("merge_queues:read")).toBe("read merge queues");
    expect(permissionWords("contents")).toBe("contents");
  });

  it("counts only the viewer's own unrevoked machines whose own report allows pipelines", () => {
    const rows = [
      machine("m1"),
      machine("m2"),
      machine("m3", { revokedAt: "2026-09-30T10:00:00Z" }),
      machine("m4", { ownerUserId: "u-other" }),
      // An operator label cannot grant what the machine itself does not say.
      machine("m5", { labels: {}, operatorLabels: { pipelines: "allowed" } }),
      machine("m6", { labels: { pipelines: "denied" } }),
    ].map(machineFromRow);
    expect(machinesAllowingPipelines(rows, "u-me")).toBe(2);
    expect(machinesAllowingPipelines(rows, "")).toBe(0);
    expect(fleetSentence(2)).toBe("2 of your machines allow pipelines.");
    expect(fleetSentence(1)).toBe("1 of your machines allows pipelines.");
    expect(fleetSentence(0)).toMatch(/^None of your machines allows pipelines yet/);
  });
});

// ---------------------------------------------------------------------------
// The section
// ---------------------------------------------------------------------------

describe("Settings > Pipelines", () => {
  it("draws the three sub-steps from the report lane, with the description verbatim", async () => {
    connect({
      githubApp: { configured: true, slug: "memql-znas" },
      machines: [machine("m1"), machine("m2"), machine("m3", { revokedAt: "2026-09-30T10:00:00Z" })],
    });
    mount(pipelines("partial", { githubApp: true, repository: false, runner: true }));

    expect(screen.getByRole("heading", { name: "Pipelines" })).toBeTruthy();
    expect(screen.getByText("Optional")).toBeTruthy();
    expect(screen.getByText(MODULE_DESCRIPTIONS.pipelines)).toBeTruthy();
    expect(stopStates()).toEqual(["done", "open", "done"]);
    expect(await screen.findByText("Registered as memql-znas.")).toBeTruthy();
    expect(screen.getByText("No repository's pipeline is connected yet. Connect one from its source's page.")).toBeTruthy();
    expect(screen.getByText("The step dispatcher is installed.")).toBeTruthy();
    expect(screen.queryByText("This cluster can run steps.")).toBeNull();
    expect(await screen.findByText("2 of your machines allow pipelines.")).toBeTruthy();
    expect(within(bar()).getByText("Partly set up")).toBeTruthy();
    expect(within(bar()).getByText("2 of 3 done")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Open Sources" }));
    expect(JSON.parse(screen.getByTestId("windows").textContent ?? "[]")).toEqual([["deployables", "sources"]]);
  });

  it("says not known rather than not done when the cluster has not said which", async () => {
    connect({ githubApp: { configured: false, canSetup: true } });
    mount(pipelines("unreported", null));
    expect(stopStates()).toEqual(["unknown", "unknown", "unknown"]);
    expect(screen.getByText("This cluster has not said whether it has a GitHub App.")).toBeTruthy();
    expect(screen.getByText("This cluster has not said whether a repository's pipeline is connected.")).toBeTruthy();
    expect(screen.getByText("This cluster has not said whether it can run steps.")).toBeTruthy();
    expect(within(bar()).getByText("Not reported")).toBeTruthy();
    expect(within(bar()).queryByText(/of 3 done/)).toBeNull();
    await settle();
    // Not known offers nothing: no setup, no dismissal, no mark.
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Not now" })).toBeNull();
    expect(settingsMark()).toBeNull();
  });

  it("reads a configured verdict with no lane as every sub-step done", () => {
    connect({});
    mount(pipelines("configured", null));
    expect(stopStates()).toEqual(["done", "done", "done"]);
    expect(within(bar()).getByText("Set up")).toBeTruthy();
    expect(within(bar()).getByText("3 of 3 done")).toBeTruthy();
    expect(bar().getAttribute("data-tone")).toBe("live");
  });

  it("offers the app-manifest setup to an owner the cluster says may set it up", async () => {
    const wire = connect({ githubApp: { configured: false, canSetup: true } });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: true }));
    const setUp = await screen.findByRole("button", { name: "Set up GitHub" });
    // The repository cannot be connected before the app exists: dimmed, and
    // with no act that would lead somewhere it fails.
    expect(stopStates()).toEqual(["open", "ahead", "done"]);
    expect(screen.queryByRole("button", { name: "Open Sources" })).toBeNull();

    fireEvent.click(setUp);
    await waitFor(() => expect(wire.fake.callsNamed("githubAppSetupBegin")).toHaveLength(1));
    // The manifest is the cluster's; the browser sends only where to come back.
    expect(wire.fake.callsNamed("githubAppSetupBegin")[0]).toBe(
      `builtin githubAppSetupBegin(returnPath: "${PIPELINES_RETURN_PATH}", organization: "")`,
    );
    expect(PIPELINES_RETURN_PATH).toBe("/?connect=pipelines&connectApp=settings");
  });

  it("tells somebody the cluster says may not set it up who can, and offers nothing", async () => {
    connect({ githubApp: { configured: false, canSetup: false } });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: false }));
    expect(await screen.findByText("This cluster has no GitHub App yet. A cluster owner can set it up.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
    // Compute is not this page's to do: unset, never a held ring.
    expect(stopStates()).toEqual(["open", "ahead", "waiting"]);
    expect(screen.getByText(/This cluster cannot run steps yet\. A command step fails until the pipelines runner is installed\./)).toBeTruthy();
  });

  it("never offers a second app while the agents have not yet seen the first", async () => {
    connect({ githubApp: { configured: true, slug: "memql-znas", canSetup: true } });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: true }));
    expect(await screen.findByText("Registered as memql-znas. The cluster has not reported it yet.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Set up GitHub" })).toBeNull();
  });

  it("names every installation whose permissions lag the app, with GitHub's own page", async () => {
    const wire = connect({
      githubApp: { configured: true, slug: "memql-znas" },
      laggingInstallations: [
        {
          installationId: 7,
          account: "acme",
          accountType: "Organization",
          htmlUrl: "https://github.com/organizations/acme/settings/installations/7",
          missingPermissions: ["checks:write", "merge_queues:read"],
          suspended: false,
        },
        {
          installationId: 9,
          account: "octo",
          accountType: "User",
          htmlUrl: "https://github.com/settings/installations/9",
          missingPermissions: ["checks:write"],
          suspended: true,
        },
      ] as unknown as Row[],
    });
    mount(pipelines("configured", { githubApp: true, repository: true, runner: true }));

    expect(await screen.findByText("GitHub is asking acme to accept this app's new permissions.")).toBeTruthy();
    expect(screen.getByText(/Missing: write checks, read merge queues\./)).toBeTruthy();
    expect(screen.getByText("GitHub is asking octo to accept this app's new permissions.")).toBeTruthy();
    expect(screen.getByText(/Its installation is also suspended/)).toBeTruthy();
    const links = screen.getAllByRole("link", { name: "Review on GitHub" });
    expect(links.map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/organizations/acme/settings/installations/7",
      "https://github.com/settings/installations/9",
    ]);
    for (const a of links) {
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toContain("noopener");
    }
    // Asked once, when the section opened.
    expect(wire.fake.callsNamed("pipelinesInstallations")).toEqual(["builtin pipelinesInstallations()"]);
  });

  it("says a failed installations read is a failure, never an empty list", async () => {
    connect({ githubApp: { configured: true, slug: "memql-znas" }, laggingInstallationsError: "github: dial tcp: i/o timeout" });
    mount(pipelines("configured", { githubApp: true, repository: true, runner: true }));
    expect(await screen.findByText("GitHub could not be asked about the app's installations.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Review on GitHub" })).toBeNull();
  });

  it("answers Not now with exactly the item's receipt, and the act goes while the section stays", async () => {
    const wire = connect({ githubApp: { configured: false, canSetup: true } });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: true }));

    const notNow = await within(bar()).findByRole("button", { name: "Not now" });
    expect(settingsMark()?.getAttribute("title")).toBe("Pipelines can be set up");
    // STANDING ON THE SECTION IS NOT AN ANSWER: it is mounted inside a visible
    // destination for itself, and nothing has been acknowledged.
    await settle();
    expect(wire.acknowledged).toEqual([]);
    expect(settingsMark()).not.toBeNull();

    fireEvent.click(notNow);
    await waitFor(() => expect(within(bar()).queryByRole("button", { name: "Not now" })).toBeNull());
    expect(wire.acknowledged).toEqual([NOT_NOW_CALL]);
    expect(settingsMark()).toBeNull();
    // The item stays, with its state.
    expect(screen.getByRole("heading", { name: "Pipelines" })).toBeTruthy();
    expect(within(bar()).getByText("Partly set up")).toBeTruthy();
  });

  it("keeps the mark and the act when Not now is refused, and says so", async () => {
    const wire = connect({ githubApp: { configured: false, canSetup: true }, ackError: "the receipt could not be written" });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: false }));
    fireEvent.click(await within(bar()).findByRole("button", { name: "Not now" }));
    expect(await screen.findByText("Your answer was not saved, so Settings stays marked.")).toBeTruthy();
    expect(wire.acknowledged).toEqual([NOT_NOW_CALL]);
    expect(within(bar()).getByRole("button", { name: "Not now" })).toBeTruthy();
    expect(settingsMark()).not.toBeNull();
  });

  it("offers no Not now once the item is set up, or when it may not be dismissed", async () => {
    connect({ githubApp: { configured: true, slug: "memql-znas" } });
    const view = mount(pipelines("configured", { githubApp: true, repository: true, runner: true }));
    await settle();
    expect(within(bar()).getByText("Set up")).toBeTruthy();
    expect(within(bar()).queryByRole("button", { name: "Not now" })).toBeNull();
    expect(settingsMark()).toBeNull();
    view.unmount();

    connect({ githubApp: { configured: false, canSetup: true } });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: false }, { dismissable: false }));
    await settle();
    expect(within(bar()).getByText("Not set up")).toBeTruthy();
    expect(within(bar()).queryByRole("button", { name: "Not now" })).toBeNull();
    expect(settingsMark()).toBeNull();
  });

  it("offers no Not now to somebody who already answered it", async () => {
    const wire = connect({
      githubApp: { configured: false, canSetup: true },
      receipts: [{ id: "r-1", changeId: "readiness:pipelines", revision: "optional-1" } as unknown as Row],
    });
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: true }));
    await waitFor(() =>
      expect((wire.fake.query.executeNamed as ReturnType<typeof vi.fn>).mock.calls.some(([name]) => name === "myAttentionReceipts")).toBe(true),
    );
    await settle();
    // The item is still open -- the receipt, not the state, is why.
    expect(within(bar()).getByText("Partly set up")).toBeTruthy();
    expect(within(bar()).queryByRole("button", { name: "Not now" })).toBeNull();
    expect(settingsMark()).toBeNull();
    expect(wire.acknowledged).toEqual([]);
  });

  it("holds what came back from GitHub beside the app's stop, and asks again", async () => {
    const wire = connect({ githubApp: { configured: false, canSetup: true } });
    const consumeIntent = vi.fn();
    mount(pipelines("unconfigured", { githubApp: false, repository: false, runner: true }), {
      intent: { id: "intent-1", payload: { connect: { reason: "github_app_setup_failed", section: "pipelines", appId: "settings" } } },
      consumeIntent,
    });
    expect(await screen.findByText("GitHub could not be set up")).toBeTruthy();
    expect(within(stops()[0]!).getByText("GitHub could not be set up")).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("intent-1");
    await waitFor(() => expect(wire.fake.callsNamed("githubAppStatus").length).toBeGreaterThanOrEqual(2));
  });
});
