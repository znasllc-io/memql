import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { ChecksPart, pipelineFact } from "../../../src/apps/deployables/pipelines/ChecksPart";
import { pipelineFromRow } from "../../../src/apps/deployables/pipelines/rows";
import { seededAccessWithout } from "../../seededAccess";
import { click, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { PACKAGE_ID, SHA_A, VIEWER, minutesAgo, packageRow, pipelineRow, run, runRow } from "./fixtures";

// A source's checks (epic memql#5479, D14; issue memql#5501), through the real
// app on the Sources tab: the Pipeline fact, what Latest upstream's checks
// said, the Checks part between the facts and the apps, ", checks on" on the
// bar and the bar's pipeline act -- said to the source's owner, and to nobody
// else, because the reads behind them are the owner's.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function mount(connection: FakeConnection, opts: { navigate?: (id: string, options?: { fromContent?: boolean }) => void; capabilities?: ReturnType<typeof seededAccessWithout> } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="sources" navigate={opts.navigate ?? vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: "owner", userId: VIEWER, ...(opts.capabilities ? { capabilities: opts.capabilities } : {}) },
    ),
  );
}

const APP = siteRow({ id: "site-store", hostname: "store.memql.example.com", status: "live", bundleRef: "blob://sites/site-store/v2/", packageId: PACKAGE_ID, packageDeployableName: "storefront" });

// The newest run on main passed at the latest upstream commit, which is not
// the deployed one. The newest run anywhere is a pull request on `cart`.
const MAIN_PASSED = runRow({ id: "r-main", ownerUserId: VIEWER, event: "push", headBranch: "main", sha: SHA_A, title: "Fix shard balance", conclusion: "success", queuedAt: minutesAgo(30),
  stages: [{ name: "checks", status: "passed" }, { name: "tests", status: "passed" }, { name: "deploy", status: "passed" }, { name: "notify", status: "passed" }] });
const CART_FAILED = runRow({ id: "r-cart", ownerUserId: VIEWER, headBranch: "cart", sha: "c0ffee0000000000000000000000000000000000", title: "Show the cart", queuedAt: minutesAgo(5) });

function seed(over: Partial<FakeSeed> = {}): FakeSeed {
  return { sites: [APP], packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [MAIN_PASSED, CART_FAILED], ...over };
}

async function openSource(): Promise<HTMLElement> {
  await click(await screen.findByRole("button", { name: (label) => label.startsWith("Open shop,") }));
  return screen.findByRole("region", { name: "Source shop" });
}

/** A fact's value as it reads, whatever spans it is drawn in. */
function fact(page: HTMLElement, label: string): string {
  const term = within(page).getByText(label, { selector: "dt" });
  return term.nextElementSibling?.textContent ?? "";
}

function checksPart(page: HTMLElement): HTMLElement | null {
  return within(page).queryByRole("heading", { name: /^Checks/ })?.closest("section") ?? null;
}

function bar(): { word: string; detail: string; acts: string[]; buttons: string[] } {
  const el = document.querySelector(".os-actbar") as HTMLElement;
  return {
    word: el.querySelector(".os-actbar-word")?.textContent ?? "",
    detail: el.querySelector(".os-actbar-detail")?.textContent ?? "",
    acts: [...el.querySelectorAll(".os-actbar-acts button")].map((b) => b.textContent ?? ""),
    buttons: [...el.querySelectorAll(".os-actbar-acts .os-button")].map((b) => b.textContent ?? ""),
  };
}

beforeEach(() => {
  h.connection = null;
});

describe("a source with a pipeline that runs", () => {
  it("reads the Pipeline fact from the newest planned run, beside Tracking", async () => {
    mount(fakeConnection(seed()));
    const page = await openSource();
    // The newest run anywhere planned three stages (the cart run), so three.
    await waitFor(() => expect(fact(page, "Pipeline")).toBe("memql-package.yaml, 3 stages, webhook, cluster"));
    const terms = [...page.querySelectorAll("dt")].map((dt) => dt.textContent);
    expect(terms.indexOf("Pipeline")).toBe(terms.indexOf("Tracking") + 1);
  });

  it("says Latest upstream's checks passed and it is not yet deployed", async () => {
    mount(fakeConnection(seed()));
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Latest upstream")).toBe("3f9c2ab, checks passed, not yet deployed"));
    // The commit stays in the data voice; the words do not.
    expect(within(page).getByText("3f9c2ab").className).toBe("os-mono");
  });

  it("says only that they passed once that commit is the deployed one", async () => {
    mount(fakeConnection(seed({ packages: [packageRow({ deployedVersion: SHA_A })] })));
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Latest upstream")).toBe("3f9c2ab, checks passed"));
  });

  it("lists the newest run per branch, the default branch first, each opening its run", async () => {
    mount(fakeConnection(seed()));
    const page = await openSource();
    const part = await waitFor(() => {
      const found = checksPart(page);
      expect(found?.querySelectorAll(".os-row-name").length).toBe(2);
      return found!;
    });
    // main first although cart ran later: what serves is built from main.
    expect([...part.querySelectorAll(".os-row-name")].map((n) => n.textContent)).toEqual(["main", "cart"]);
    expect(within(part).getByText("Fix shard balance")).toBeTruthy();
    expect(within(part).getByText("Passed")).toBeTruthy();
    expect(within(part).getByText("Failed")).toBeTruthy();
    expect(part.querySelector(".os-head-meta")?.textContent).toBe("2");
    // The part sits between the source's facts and the apps it produces.
    const parts = [...page.querySelectorAll(".os-report-part")].map((p) => (p.querySelector(".os-report-heading")?.textContent ?? "").trim());
    expect(parts.findIndex((t) => t.startsWith("Checks"))).toBe(parts.findIndex((t) => t.startsWith("Apps it produces")) - 1);

    // Each row is read out as what opens, the commit, and how it went.
    expect(within(part).getByRole("button", { name: "Open the newest run on cart: Show the cart, failed at tests, 7m 40s" })).toBeTruthy();
    await click(within(part).getByRole("button", { name: "Open the newest run on main: Fix shard balance, passed, 4 stages, 7m 40s" }));
    expect(await screen.findByRole("heading", { name: "Fix shard balance" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Source shop" })).toBeNull();
  });

  it("opens Runs refined to this source from All runs", async () => {
    const navigate = vi.fn();
    mount(fakeConnection(seed()), { navigate });
    const page = await openSource();
    await click(await within(page).findByRole("button", { name: "All runs" }));
    expect(navigate).toHaveBeenCalledWith("runs", { fromContent: true });
  });

  it("reads checks on in the bar, and offers Pipeline settings as its one button", async () => {
    mount(fakeConnection(seed()));
    await openSource();
    await waitFor(() => expect(bar().detail).toBe("1 app, checks on"));
    expect(bar().word).toBe("Tracked");
    expect(bar().acts).toEqual(["Pipeline settings"]);
    expect(bar().buttons).toEqual(["Pipeline settings"]);
    // No deploy: a source is not a deployable.
    expect(bar().acts.some((a) => a.startsWith("Deploy"))).toBe(false);
  });

  it("opens the pipeline's settings from the bar", async () => {
    mount(fakeConnection(seed()));
    await openSource();
    await waitFor(() => expect(bar().acts).toEqual(["Pipeline settings"]));
    await click(screen.getByRole("button", { name: "Pipeline settings" }));
    expect(await screen.findByRole("region", { name: "Pipeline of shop" })).toBeTruthy();
  });

  it("puts Pipeline settings beside Review as text when a run waits at the source's gate", async () => {
    const parked = { id: "dep-parked", packageId: PACKAGE_ID, sourceVersion: SHA_A, status: "awaiting_confirm", report: { name: "shop", formatVersion: 1, deployables: [], dslDomains: [], problems: [], ok: true },
      dslVersion: "", deployables: [], snapshotArtifactId: "", buildLogTail: "", error: null, requestedBy: VIEWER, startedAt: minutesAgo(3), finishedAt: "", createdAt: minutesAgo(3) } as unknown as Row;
    mount(fakeConnection(seed({ awaitingConfirm: [parked] })));
    await openSource();
    await waitFor(() => expect(bar().acts).toEqual(["Pipeline settings", "Review"]));
    // ONE button: Review, the act the waiting gate asks for.
    expect(bar().buttons).toEqual(["Review"]);
  });

  it("keeps checks on and leaves out the act for an owner withheld the connect part", async () => {
    mount(fakeConnection(seed()), { capabilities: seededAccessWithout("owner", "app:deployables/connect") });
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Pipeline")).toBe("memql-package.yaml, 3 stages, webhook, cluster"));
    // The reading is the owner's to see; the act is the part's to grant.
    expect(bar().detail).toContain("checks on");
    expect(bar().acts).toEqual([]);
  });
});

describe("a source whose pipeline is disconnected", () => {
  it("says so, keeps its runs as history, and opens Pipeline settings", async () => {
    mount(fakeConnection(seed({ pipelines: [pipelineRow({ status: "disconnected" })] })));
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Pipeline")).toBe("Disconnected"));
    const part = checksPart(page)!;
    expect(within(part).getByText("Disconnected. Its runs stay as history.")).toBeTruthy();
    expect([...part.querySelectorAll(".os-row-name")].map((n) => n.textContent)).toEqual(["main", "cart"]);
    expect(bar().detail).not.toContain("checks on");
    expect(bar().acts).toEqual(["Pipeline settings"]);
  });
});

describe("a source with no pipeline", () => {
  it("says it is not connected, and offers Connect pipeline on the bar", async () => {
    mount(fakeConnection(seed({ pipelines: [], pipelineRuns: [] })));
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Pipeline")).toBe("Not connected"));
    const part = checksPart(page)!;
    expect(within(part).getByText(/^Not connected\. Connecting runs this repository's checks/)).toBeTruthy();
    // No count and no All runs: there is nothing to count or open.
    expect(part.querySelector(".os-head-meta")).toBeNull();
    expect(within(part).queryByRole("button", { name: "All runs" })).toBeNull();
    // Latest upstream is the commit and nothing more.
    expect(fact(page, "Latest upstream")).toBe("3f9c2ab");
    expect(bar().acts).toEqual(["Connect pipeline"]);
    expect(bar().buttons).toEqual(["Connect pipeline"]);
  });

  it("offers no Connect pipeline on an archived source, which opens no runs", async () => {
    mount(fakeConnection(seed({ pipelines: [], pipelineRuns: [], packages: [packageRow({ status: "archived" })] })));
    // An archived source is listed under the archived flip.
    await click(await screen.findByRole("button", { name: "Show archived (1)" }));
    const page = await openSource();
    await waitFor(() => expect(fact(page, "Pipeline")).toBe("Not connected"));
    expect(bar().acts).not.toContain("Connect pipeline");
  });
});

describe("before the pipelines feeds answer", () => {
  it("draws the shape of what is coming and claims nothing -- no Not connected, no act", async () => {
    const connection = fakeConnection(seed());
    // The pipelines read never answers.
    const query = connection.query as unknown as { executeNamed: (name: string, call: string, opts?: unknown) => Promise<unknown> };
    const answer = query.executeNamed;
    query.executeNamed = vi.fn((name: string, call: string, opts?: unknown) => (call === "query pipelinesForOwner()" ? new Promise(() => {}) : answer(name, call, opts)));
    mount(connection);
    const page = await openSource();
    // Loading is said to assistive tech, as a status, and never painted.
    expect(within(page).getByText("Loading the pipeline").closest("[role='status']")?.getAttribute("aria-busy")).toBe("true");
    expect(within(checksPart(page)!).getByText("Loading checks")).toBeTruthy();
    expect(within(page).queryByText(/Not connected/)).toBeNull();
    expect(bar().detail).not.toContain("checks on");
    expect(bar().acts).toEqual([]);
  });
});

describe("a source somebody else owns", () => {
  it("says nothing about checks at all: the reads behind them are the owner's", async () => {
    // A cluster owner opening a colleague's source. Their own pipelines feed
    // answers nothing for it, and "Not connected" would be an invented fact.
    mount(fakeConnection(seed({ packages: [packageRow({ ownerUserId: "u-other" })], pipelines: [], pipelineRuns: [] })));
    const page = await openSource();
    await within(page).findByText("Apps it produces");
    expect(within(page).queryByText("Pipeline", { selector: "dt" })).toBeNull();
    expect(checksPart(page)).toBeNull();
    expect(within(page).queryByText(/Not connected/)).toBeNull();
    expect(bar().detail).not.toContain("checks on");
    expect(bar().acts).not.toContain("Connect pipeline");
  });
});

describe("the Sources list", () => {
  it("reads pipeline where the app count goes, for a source that produces no apps and runs one", async () => {
    mount(fakeConnection(seed({ sites: [] })));
    const row = await screen.findByRole("button", { name: (label) => label.startsWith("Open shop,") });
    await waitFor(() => expect(within(row).getByText("pipeline")).toBeTruthy());
    expect(within(row).queryByText("No apps yet")).toBeNull();
  });

  it("reads No apps yet for one with no pipeline, which is the control", async () => {
    mount(fakeConnection(seed({ sites: [], pipelines: [], pipelineRuns: [] })));
    const row = await screen.findByRole("button", { name: (label) => label.startsWith("Open shop,") });
    await waitFor(() => expect(within(row).getByText("No apps yet")).toBeTruthy());
    expect(within(row).queryByText("pipeline")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The part on its own, for the one state the app does not reach yet
// ---------------------------------------------------------------------------

describe("the Checks part", () => {
  it("says a failed read failed, rather than staying a loading shape", () => {
    render(<ChecksPart pipeline={null} runs={[]} settled={false} error="the stream is down" />);
    expect(screen.getByText("Checks could not be read.")).toBeTruthy();
    expect(screen.getByText("the stream is down")).toBeTruthy();
    expect(screen.queryByText("Loading checks")).toBeNull();
  });

  it("lists six branches at most, and counts every one", () => {
    const pipeline = pipelineFromRow(pipelineRow());
    const runs = ["main", "a", "b", "c", "d", "e", "f"].map((branch, i) => run({ id: `r-${branch}`, headBranch: branch, queuedAt: minutesAgo(i + 1) }));
    render(<ChecksPart pipeline={pipeline} runs={runs} settled />);
    expect(document.querySelectorAll(".os-row-name").length).toBe(6);
    expect(document.querySelector(".os-head-meta")?.textContent).toBe("7");
  });

  it("leaves the stage count out of the Pipeline fact until a run has planned one", () => {
    const pipeline = pipelineFromRow(pipelineRow({ delivery: "poll", compute: "cluster_and_fleet" }));
    expect(pipelineFact(pipeline, [])).toBe("memql-package.yaml, polling, cluster and fleet");
    expect(pipelineFact(pipeline, [run({ stages: [], conclusion: "refused" })])).toBe("memql-package.yaml, polling, cluster and fleet");
    expect(pipelineFact(pipeline, [run({ stages: [{ name: "tests", status: "passed" }] })])).toBe("memql-package.yaml, 1 stage, polling, cluster and fleet");
  });
});
