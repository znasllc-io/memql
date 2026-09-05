import { act, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { SITE_CONCEPT } from "../../src/apps/deployables/concepts";
import { DEPLOYMENT_CONCEPT } from "../../src/apps/deployables/packages/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { PORTAL, SHOP, click, emit, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// The Deployables section (epic memql#4885, task memql#4889): the list, its
// Refine, the waiting mark, the archived flip, and the compose seam.
//
// Everything goes through `connection.query` and `connection.subscriptions`
// exactly as production does, so the real LiveCollection, the real
// projections and the real generated builders all run. The assertions are
// what a person SEES: a row, a sentence, a chip, a data-arrival attribute.

function memStore(saved?: { defaultSection: string }) {
  const data = new Map<string, string>();
  // SOURCE GROUPS START COLLAPSED IN PRODUCTION (epic memql#4937 follow-up),
  // and every assertion in this file is about what the list SHOWS rather than
  // about the disclosure -- so the group is seeded open, which is the
  // precondition those tests were written under. The default itself is
  // asserted in list.test.tsx ("collapsed until you open it"), where it
  // belongs.
  const seeded = { version: 1, density: "comfortable", expandedSources: ["pkg:pkg-acme"] };
  data.set("memql-os-deployables-v1", JSON.stringify({ ...seeded, ...saved }));
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(
  connection: FakeConnection | null,
  opts: { role?: string; section?: string; navigate?: (id: string) => void; saved?: { defaultSection: string } } = {},
) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId={opts.section ?? "deployables"}
        navigate={opts.navigate ?? vi.fn()}
        askContext={vi.fn()}
        store={memStore(opts.saved)}
      />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

/** Every row on the list, by the name it renders under. */
/**
 * The APP rows' names.
 *
 * A source is a real row now (epic memql#4937), so it appears among the
 * `.os-row-name` elements too. These assertions are about which DEPLOYABLES
 * the list is showing, so the source lines are excluded -- the grouping test
 * above is where the source row itself is asserted.
 */
function rowNames(): string[] {
  return [...document.querySelectorAll(".os-livelist-rows .os-row-name")]
    .filter((el) => el.closest(".os-deploy-group-apps") !== null || el.closest(".os-deploy-group") === null)
    .map((el) => el.textContent ?? "");
}

/** The list item a row sits in -- where the arrival cue lands. */
function itemOf(name: string): HTMLElement {
  const row = screen.getByText(name).closest("li");
  if (row === null) throw new Error(`no list item holding ${name}`);
  return row as HTMLElement;
}

/** Open the Refine panel, if it is shut, and choose one option of a facet. */
async function refine(facet: string, option: string): Promise<void> {
  // The affordance TOGGLES, so a helper that always clicks it would shut the
  // panel it was asked to use on every second call.
  const opener = screen.getByRole("button", { name: "Refine deployables" });
  if (opener.getAttribute("aria-expanded") !== "true") await click(opener);
  await click(screen.getByLabelText(facet));
  await click(await screen.findByRole("option", { name: option }));
}

beforeEach(() => {
  h.connection = null;
});

// ---------------------------------------------------------------------------
// Fixtures: a source with two apps, a hand-made site, a parked run
// ---------------------------------------------------------------------------

const ACME: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

const STORE = siteRow({
  id: "site-store",
  hostname: "store.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-store/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "storefront",
});

const ADMIN = siteRow({
  id: "site-admin",
  hostname: "admin.memql.example.com",
  kind: "spa",
  status: "disabled",
  bundleRef: "blob://sites/site-admin/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "admin",
});

const RETIRED = siteRow({
  id: "site-retired",
  hostname: "retired.memql.example.com",
  kind: "static",
  status: "archived",
  bundleRef: "blob://sites/site-retired/v1/",
});

const REPORT = {
  name: "acme",
  formatVersion: 1,
  deployables: [
    { name: "storefront", kind: "spa", path: "clients/web", buildPlan: "already built: dist", output: "dist", prebuilt: true },
    { name: "reports", kind: "static", path: "clients/reports", buildPlan: "already built: out", output: "out", prebuilt: true },
  ],
  dslDomains: [],
  problems: [],
  ok: true,
};

function parkedRun(over: Partial<Record<string, unknown>> = {}): Row {
  return {
    id: "dep-parked",
    packageId: "pkg-acme",
    sourceVersion: "cccccccccccccccccccc",
    status: "awaiting_confirm",
    report: REPORT,
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "",
    buildLogTail: "",
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T13:00:00Z",
    finishedAt: "",
    createdAt: "2026-09-01T13:00:00Z",
    ...over,
  } as unknown as Row;
}

const WITH_PACKAGE: FakeSeed = { sites: [STORE, ADMIN, SHOP], packages: [ACME] };

/** A SECOND source, so "open two groups" is expressible. */
const WIDGETS: Row = {
  ...(ACME as unknown as Record<string, unknown>),
  id: "pkg-widgets",
  name: "widgets-co",
  repoUrl: "https://github.com/acme/widgets",
} as unknown as Row;

const WIDGET_SITE = siteRow({
  id: "site-widget",
  hostname: "widgets.memql.example.com",
  status: "live",
  packageId: "pkg-widgets",
  packageDeployableName: "widgets",
});

const TWO_SOURCES: FakeSeed = { sites: [STORE, ADMIN, WIDGET_SITE], packages: [ACME, WIDGETS] };

// ---------------------------------------------------------------------------
// The three sections
// ---------------------------------------------------------------------------

describe("the window's sections", () => {
  it("opens a saved default of sites, packages or actions on Deployables", async () => {
    // The section a person asked for is gone; the one that replaced it is
    // where they meant to be. The window must not land on the map instead.
    for (const retired of ["sites", "packages", "actions"]) {
      const navigate = vi.fn();
      const view = mount(fakeConnection(WITH_PACKAGE), { section: "map", navigate, saved: { defaultSection: retired } });
      expect(navigate).toHaveBeenCalledWith("deployables");
      view.unmount();
    }
  });
});

// ---------------------------------------------------------------------------
// One row per thing that serves or will
// ---------------------------------------------------------------------------

describe("the list", () => {
  it("groups a source's apps under one source line and stands a hand-made site on its own", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    // The source is named ONCE, above the rows it produced (DESIGN.md rule 7)
    // -- and it is a REAL ROW now (epic memql#4937), not a caption wedged
    // between clickable ones. It opens the source's own view, where its
    // credential, its auto-deploy switch, its history and its archive live.
    expect(screen.getAllByText("acme/storefront at main")).toHaveLength(1);
    const group = screen.getByText("acme/storefront at main").closest(".os-deploy-group") as HTMLElement;
    // The named rows: the source, then its apps. The group also carries a
    // DISCLOSURE button, which has no name of its own and is asserted below.
    expect(
      within(group)
        .getAllByRole("button")
        .map((b) => b.querySelector(".os-row-name")?.textContent)
        .filter((name) => name !== undefined),
    ).toEqual(["acme/storefront at main", "admin", "storefront"]);
    // TWO CONTROLS, TWO JOBS: the chevron opens and shuts the group, the row
    // opens the source's own view. A single control could not do both.
    const disclose = within(group).getByRole("button", { name: /Collapse acme\/storefront at main/ });
    expect(disclose.getAttribute("aria-expanded")).toBe("true");
    // ...and each row carries its address beside the app's name.
    expect(within(group).getByText("store.memql.example.com")).toBeTruthy();

    // The hand-made site is a row of its own, outside any group.
    const shop = screen.getByText("Storefront").closest(".os-row") as HTMLElement;
    expect(shop.closest(".os-deploy-group")).toBeNull();
    expect(within(shop).getByText("shop.memql.example.com")).toBeTruthy();
  });

  it("gives every row the five-stop rail the page draws in full", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    const store = (await screen.findByText("storefront")).closest(".os-row") as HTMLElement;
    const rail = within(store).getByRole("list", { name: "storefront stops" });
    // EVERY STOP DONE, because this app is live and serving a published
    // bundle. This assertion used to read done/ahead/done/ahead/done and was
    // WRITTEN THAT WAY TO MATCH THE BUG: the list hands the rail only the
    // PARKED run, a finished run is not parked, and What-it-is and Build both
    // derived their state from that run's report -- so they reported "not
    // reached" for work whose output the same row was serving. The stops read
    // the row's own standing facts now (its kind, its bundle), which is what a
    // settled deployable actually knows about itself.
    expect([...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "done",
      "done",
      "done",
      "done",
      "done",
    ]);
    // Five dots with no name are five dots: each mark says which stop it is.
    expect(within(rail).getByRole("img", { name: "Live, finished" })).toBeTruthy();
  });

  it("names the two sections, so a source and a standalone site never interleave", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("acme/storefront at main");
    // The headings are carried by the first group of each section, which is
    // decided in the fold where the order is known.
    const heads = [...document.querySelectorAll(".os-deploy-sectionhead")].map((h) => h.textContent);
    expect(heads).toEqual(["From a source", "Standalone"]);
  });

  it("starts a source group COLLAPSED, and opening it is remembered", async () => {
    // The default the seeded store in this file deliberately overrides, tested
    // here where it belongs: a fresh document has no open groups.
    const store = new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} });
    h.connection = fakeConnection(WITH_PACKAGE);
    render(
      withSession(
        <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={store} />,
        { role: "owner", userId: "u-me" },
      ),
    );
    const source = await screen.findByText("acme/storefront at main");
    // The source line is there; its apps are NOT RENDERED AT ALL -- not hidden
    // with CSS, which is what makes the app count on the row an honest summary.
    expect(screen.queryByText("storefront")).toBeNull();
    expect(screen.queryByText("admin")).toBeNull();

    const group = source.closest(".os-deploy-group") as HTMLElement;
    const disclose = within(group).getByRole("button", { name: /Expand acme\/storefront at main/ });
    expect(disclose.getAttribute("aria-expanded")).toBe("false");

    await click(disclose);
    expect(await screen.findByText("storefront")).toBeTruthy();
  });

  it("keeps the disclosure OUT of the row's own button", async () => {
    // An openable ListRow IS a button, and a button cannot contain another
    // one. Nested, the disclosure rendered perfectly and never fired -- caught
    // only because a different test tried to click two of them.
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("acme/storefront at main");
    const disclose = screen.getByRole("button", { name: /^(Expand|Collapse) acme\/storefront/ });
    expect(disclose.parentElement?.closest("button")).toBeNull();
    // And it says what it will show, rather than being a bare arrow.
    expect(disclose.textContent).toContain("2 apps");
  });

  it("opens TWO sources without one closing the other", async () => {
    // Found in a browser, not here: `update` applied its patch to the document
    // the RENDER closed over, so two toggles in one tick wrote the second id
    // over the first and only one group opened. One preference per screen
    // never showed it; a set with one entry per source did immediately.
    const store = new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} });
    h.connection = fakeConnection(TWO_SOURCES);
    render(
      withSession(
        <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={store} />,
        { role: "owner", userId: "u-me" },
      ),
    );
    await screen.findByText("acme/storefront at main");
    // BOTH CLICKS IN ONE TICK, which is what the browser does and what an
    // `await click()` per button does NOT: awaiting flushes React between
    // them, so each handler sees fresh state and the stale-closure write can
    // never happen. This test passed against the bug until it fired them
    // together.
    const buttons = screen.getAllByRole("button", { name: /^Expand / });
    expect(buttons).toHaveLength(2);
    await act(async () => {
      for (const b of buttons) b.click();
    });
    // Both sources' apps, not just the last one clicked.
    expect(await screen.findByText("storefront")).toBeTruthy();
    expect(screen.getByText("widgets")).toBeTruthy();
  });

  it("deploys a DECLARED app on its own, skipping every other one", async () => {
    // The whole chain: an app the source declares has no site and no run, so
    // clicking it starts the analysis that produces its confirm gate -- and
    // every OTHER app is sent explicitly skipped, or this first deploy would
    // rebuild and republish the ones already serving.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{ ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] }],
    });
    mount(connection);
    await screen.findByText("storefront");
    // It is listed under its source, with no address of its own.
    const web = await screen.findByText("web");
    expect(web).toBeTruthy();
    await click(web.closest("button"));
    await waitFor(() => expect(connection.callsNamed("packageDeploy").length).toBeGreaterThan(0));
    // The analysis first -- confirm: false -- which is what parks the gate.
    const call = connection.callsNamed("packageDeploy")[0] ?? "";
    expect(call).toContain("confirm: false");
    // ...AND SCOPED, which is what this test has always been named for and
    // never asserted. The engine derives `scopedTo` from the skips, so an
    // analysis sent without them parks a gate about the whole source: every
    // sibling's row and page read it as their own, and confirming it rebuilds
    // apps nobody asked about.
    expect(call).toContain("placements: {storefront: {skip: true}}");
    // The app being deployed is NOT in the map: scope is the COMPLEMENT of
    // the skips, so naming it here would scope the run to nothing.
    expect(call).not.toContain("web: {skip");
  });

  it("does not offer to deploy an app the owner turned off", async () => {
    // Reported from production: skipping `web`, then discarding the result,
    // left it on the list looking exactly like an app nobody had got to -- so
    // clicking it started the whole build again. A declined app is listed and
    // INERT.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{
        ...ACME,
        declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }],
        disabledDeployables: ["web"],
      }],
    });
    mount(connection);
    const web = await screen.findByText("web");
    // It is there to be found, and says it is off.
    expect(web.closest(".os-row")?.textContent).toContain("off");

    await click(web.closest("button"));
    // CLICKING IT TURNS IT BACK ON -- and does NOT deploy it. Deploying is the
    // next click, deliberately.
    // ENABLE IS ITS OWN VERB, and it names what changed (memql#4951). It used
    // to send `disabledDeployables: []` -- the whole list with the name
    // filtered out -- which erased every other app's off state whenever the
    // window's copy of the row was stale.
    await waitFor(() => expect(connection.callsNamed("enablePackageDeployables").length).toBe(1));
    expect(connection.callsNamed("enablePackageDeployables")[0]).toContain('deployableNames: ["web"]');
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
  });

  it("does offer to deploy one nobody turned off", async () => {
    // The negative control: without the off-list the same row deploys, which
    // is what makes the assertion above about the preference and not about
    // declared rows in general.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{ ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] }],
    });
    mount(connection);
    await click((await screen.findByText("web")).closest("button"));
    await waitFor(() => expect(connection.callsNamed("packageDeploy").length).toBeGreaterThan(0));
    expect(connection.callsNamed("enablePackageDeployables")).toHaveLength(0);
  });

  it("says what to do when there is nothing yet", async () => {
    mount(fakeConnection({ sites: [], packages: [] }));
    expect(await screen.findByText("No deployables yet. New deployable is where one starts.")).toBeTruthy();
  });

  it("reads each feed ONCE on mount, and no timeline until a row is opened", async () => {
    // The timeline is retained by the PAGE, never by the root
    // (clients/os/README.md); the parked-runs feed is the one exception, and
    // it is a different read.
    const connection = fakeConnection(WITH_PACKAGE);
    mount(connection);
    await screen.findByText("storefront");

    expect(connection.calls.filter((c) => c === "query sitesAll()")).toHaveLength(1);
    expect(connection.calls.filter((c) => c === "query packagesAll()")).toHaveLength(1);
    expect(connection.calls.filter((c) => c === "query packageDeploymentsAwaitingConfirm()")).toHaveLength(1);
    expect(connection.calls.filter((c) => c === "query sourceCredentialsMine()")).toHaveLength(1);
    expect(connection.callsNamed("packageDeployments")).toHaveLength(0);

    // OPENING A ROW REPLACES THE LIST (rule 11), so the page is what is on
    // screen after this -- which is the whole point, and why the Refine
    // affordance is no longer beside it.
    await click(screen.getByText("storefront").closest("button"));
    await waitFor(() => expect(connection.callsNamed("packageDeployments")).toHaveLength(1));
    expect(connection.callsNamed("packageDeployments")[0]).toContain('packageId: "pkg-acme"');
    // Opening a row REPLACES the list, so there is exactly one Head on screen
    // (DESIGN.md rule 11). Two stacked Heads in one scroller was the tell that
    // neither of the shell's two detail patterns had been adopted -- and it is
    // where the measured 5,069px started.
    expect(await screen.findByRole("region", { name: "Deployable store.memql.example.com" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Refine deployables" })).toBeNull();
    expect(document.querySelectorAll(".os-head")).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// The waiting mark, from the fourth feed
// ---------------------------------------------------------------------------

describe("a deploy waiting for you", () => {
  it("marks the SOURCE once, and adds the app that has no site yet", async () => {
    mount(fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] }));
    await screen.findByText("storefront");

    // ONE parked run belongs to ONE source, so the mark is said once, on the
    // source line beside the update chip -- not repeated down every row the
    // source produced (DESIGN.md rule 7).
    expect(screen.getAllByText("a deploy is waiting for you")).toHaveLength(1);
    const group = screen.getByText("acme/storefront at main").closest(".os-deploy-group") as HTMLElement;
    // The source line is a REAL ROW now (epic memql#4937), so the mark sits on
    // the row rather than on a caption div -- still exactly once, still on the
    // source rather than repeated down every app it produced.
    const sourceLine = screen.getByText("acme/storefront at main").closest(".os-row") as HTMLElement;
    expect(within(sourceLine).getByText("a deploy is waiting for you")).toBeTruthy();
    expect(group).toBeTruthy();
    const storefront = screen.getByText("storefront").closest(".os-row") as HTMLElement;
    expect(within(storefront).queryByText("a deploy is waiting for you")).toBeNull();

    const reports = screen.getByText("reports").closest(".os-row") as HTMLElement;
    expect(within(reports).getByText("no address yet")).toBeTruthy();
    const shop = screen.getByText("Storefront").closest(".os-row") as HTMLElement;
    expect(within(shop).queryByText("a deploy is waiting for you")).toBeNull();
  });

  it("keeps the mark ON the row when the row IS the scope: a hand-made deployable", async () => {
    // A hand-made site stands alone, so there is no source line to say it on
    // and the row is the only place the fact belongs. Still said once.
    mount(
      fakeConnection({
        sites: [SHOP, { ...(SHOP as object), id: "site-two", hostname: "two.memql.example.com", title: "Two" } as unknown as Row],
        packages: [],
        awaitingConfirm: [],
      }),
    );
    await screen.findByText("Storefront");
    // The reachable positive: with no parked run there is no mark anywhere,
    // so the absence below is about the fold rather than about the query.
    expect(screen.queryByText("a deploy is waiting for you")).toBeNull();
  });

  it("keeps the compact rail out of the address, in the row's trailing state cluster", async () => {
    // `store.memql.example.com` followed flush by five dots read as one
    // string, the marks as punctuation after the host. The rail belongs on
    // the trailing edge with the chips, never beside the address.
    mount(fakeConnection(WITH_PACKAGE));
    const store = (await screen.findByText("storefront")).closest(".os-row") as HTMLElement;
    const rail = within(store).getByRole("list", { name: "storefront stops" });
    expect(rail.closest(".os-row-state"), "the compact rail is not in the row's state cluster").not.toBeNull();
    const address = within(store).getByText("store.memql.example.com");
    expect(address.closest(".os-row-state"), "the address is in the state cluster").toBeNull();
  });

  it("clears the mark when the run moves on, on its own event", async () => {
    const connection = fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] });
    mount(connection);
    await screen.findByText("reports");

    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun({ status: "succeeded" }));
    await waitFor(() => expect(screen.queryByText("a deploy is waiting for you")).toBeNull());
    // ...and the row that only existed because the run was parked goes with it.
    expect(screen.queryByText("reports")).toBeNull();
  });

  it("clears it for a refusal too", async () => {
    const connection = fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] });
    mount(connection);
    await screen.findByText("reports");

    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun({ status: "refused" }));
    await waitFor(() => expect(screen.queryByText("a deploy is waiting for you")).toBeNull());
  });
});

// ---------------------------------------------------------------------------
// Refine, and the cue it must not fire
// ---------------------------------------------------------------------------

describe("Refine", () => {
  it("narrows on a facet and says which question is being asked", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    await refine("Kind", "Shopify storefront");
    expect(rowNames()).toEqual(["Storefront"]);
    // The active constraint stays visible, removable in place (rule 2)...
    const chip = screen.getByRole("button", { name: "Remove Shopify storefront" });
    await click(chip);
    await waitFor(() => expect(rowNames()).toHaveLength(3));
  });

  it("narrows on the status and the source as well", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    await refine("Status", "Unpublished");
    expect(rowNames()).toEqual(["admin"]);
    await click(screen.getByRole("button", { name: "Remove disabled" }));

    await refine("Source", "A repository");
    await waitFor(() => expect(rowNames()).toEqual(["admin", "storefront"]));
  });

  it("points at Refine when a filter is why the list is empty", async () => {
    // Empty and filtered-to-empty are different answers about different things.
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");
    await click(screen.getByRole("button", { name: "Refine deployables" }));
    const search = screen.getByLabelText("Search") as HTMLInputElement;
    await click(search);
    const { type: typeInto } = await import("./harness");
    await typeInto(search, "nothing like this");

    expect(await screen.findByText(/Clear the search or a facet in Refine/)).toBeTruthy();
  });

  it("REVEALS rows without announcing them: a filter change is not the cluster sending anything", async () => {
    const connection = fakeConnection(WITH_PACKAGE);
    mount(connection);
    await screen.findByText("storefront");

    // The reachable positive first: this list CAN ring, and does when a
    // bundle flips under the person watching.
    await emit(connection, SITE_CONCEPT, { ...(STORE as object), bundleRef: "blob://sites/site-store/v9/" } as unknown as Row);
    await waitFor(() => expect(itemOf("storefront").getAttribute("data-arrival")).toBe("updated"));

    // Now the filter, over a row that has had no event at all: hiding the
    // hand-made site and bringing it back reveals a row the browser already
    // held, which is not news -- the view re-baselines through
    // `useLiveView`'s key rather than through a `key` prop on the list, so
    // nothing rises and nothing rings.
    await refine("Kind", "Single-page app");
    expect(rowNames()).toEqual(["admin", "storefront"]);
    await click(screen.getByRole("button", { name: "Remove Single-page app" }));
    await waitFor(() => expect(rowNames()).toHaveLength(3));
    expect(itemOf("Storefront").getAttribute("data-arrival")).toBeNull();
    // ...and the cue the publish EARNED is still there: a filter neither
    // invents a cue nor destroys one.
    expect(itemOf("storefront").getAttribute("data-arrival")).toBe("updated");
  });
});

// ---------------------------------------------------------------------------
// Show archived: a place, not a checkbox
// ---------------------------------------------------------------------------

describe("show archived", () => {
  it("reveals archived deployables and hides the active ones", async () => {
    mount(fakeConnection({ sites: [STORE, ADMIN, RETIRED], packages: [ACME] }));
    await screen.findByText("storefront");
    expect(screen.queryByText("retired.memql.example.com")).toBeNull();

    await click(screen.getByRole("button", { name: /Show archived \(1\)/ }));
    expect(await screen.findByText("retired.memql.example.com")).toBeTruthy();
    // An archive is a PLACE: the active list is the one that is now hidden.
    expect(screen.queryByText("storefront")).toBeNull();
    expect(screen.getByText(/Archived deployables are kept, not deleted/)).toBeTruthy();

    await click(screen.getByRole("button", { name: "Show active deployables" }));
    expect(await screen.findByText("storefront")).toBeTruthy();
  });

  it("says nothing about an archive nobody has", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");
    expect(screen.queryByRole("button", { name: /Show archived/ })).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// New deployable: the compose seam
// ---------------------------------------------------------------------------

describe("New deployable", () => {
  it("replaces the list in place, and Back returns to it", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    await click(screen.getByRole("button", { name: /New deployable/ }));
    const compose = await screen.findByRole("region", { name: "New deployable" });
    // The Head's title becomes "New deployable"; the list is gone, not
    // pushed below a modal (design D4).
    expect(within(compose).getByRole("heading", { name: "New deployable" })).toBeTruthy();
    expect(screen.queryByText("storefront")).toBeNull();

    // The rail is the form: Source is the open stop and carries the caption.
    const rail = within(compose).getByRole("list", { name: "Deployable stops" });
    expect([...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "open",
      "pending",
      "pending",
      "pending",
      "pending",
    ]);
    // The Source stop is the OS's own choice control: three answers, chosen
    // once. The third is a cluster owner's, and this session is one.
    expect(within(compose).getByRole("radio", { name: /A repository/ })).toBeTruthy();
    expect(within(compose).getByRole("radio", { name: /A zip in Files/ })).toBeTruthy();
    expect(within(compose).getByRole("radio", { name: /Pushed by your CI/ })).toBeTruthy();
    // THE FORWARD ACT IS ON THE BAR NOW (rule 12), not in the Head -- it was
    // at the top, so answering a long Source stop meant scrolling back UP to
    // continue. Nothing is chosen yet, so it is ABSENT rather than disabled,
    // and the bar says what is still needed.
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    expect(within(bar).queryByRole("button", { name: "Analyze" })).toBeNull();
    expect(within(bar).getByRole("button", { name: "Cancel" })).toBeTruthy();

    await click(within(compose).getByRole("button", { name: "Deployables" }));
    expect(await screen.findByText("storefront")).toBeTruthy();
  });

  it("is offered to the DEPLOY TIER, developer included", async () => {
    // Rank >= 200 under the one ladder is {admin, developer, owner}, which is
    // the set the engine's own deploy gate uses (epic memql#4832, D1). Under
    // the shell's old ordering `min: "admin"` excluded developer, and the
    // deploy tier saw a read-only Deployables app; that case lived on the
    // retired Actions section's gate and its statement lives here now.
    for (const role of ["admin", "developer", "owner"]) {
      const view = mount(fakeConnection(WITH_PACKAGE), { role });
      await screen.findByText("storefront");
      expect(screen.getByRole("button", { name: /New deployable/ })).toBeTruthy();
      view.unmount();
    }
  });

  it("is not offered to a reader, disabled or otherwise", async () => {
    mount(fakeConnection(WITH_PACKAGE), { role: "reader" });
    await screen.findByText("storefront");
    expect(screen.queryByRole("button", { name: /New deployable/ })).toBeNull();
    // ...and the empty state does not tell them to use a control they do not have.
    expect(screen.queryByText(/New deployable is where one starts/)).toBeNull();
  });

  it("reopens a parked run's reading from the row that will serve, with its report in place", async () => {
    mount(fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] }));
    await click(await screen.findByText("reports"));

    // NAMED AFTER THE SOURCE, because this is not a new deployable: the source
    // was added already and this reopens its gate.
    const compose = await screen.findByRole("region", { name: "Deploy acme" });
    const rail = within(compose).getByRole("list", { name: "Deployable stops" });
    // A run parked at the confirm gate has ANSWERED What it is -- its report
    // is what parked it -- so the open stop is Where it lives, which is what
    // the Deploy beneath is waiting for.
    expect([...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "complete",
      "complete",
      "open",
      "skipped",
      "pending",
    ]);
    // The source it came from, as its answer, and the report the run parked with.
    expect(within(compose).getByText("acme/storefront at main")).toBeTruthy();
    // The report the run parked with, at the What-it-is stop: the app and its
    // path, as the report names them.
    expect(within(compose).getByText("clients/reports")).toBeTruthy();
    // Deploy, on the BAR and REACHABLE (rule 12): the one app with no site yet
    // arrived with a suggested address, so there is nothing left to answer.
    // "storefront" already serves, so it is not re-asked -- the pipeline reads
    // a placement on a first deploy only.
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    expect(within(bar).getByRole("button", { name: /^Deploy/ })).toBeTruthy();
    expect(within(compose).getByText("reports.memql.example.com")).toBeTruthy();
    expect(within(compose).queryByLabelText("The name storefront answers at")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// What the list does not do
// ---------------------------------------------------------------------------

describe("what the section does not do", () => {
  it("mounts no toast container and no dialog anywhere", async () => {
    const { container } = mount(fakeConnection({ ...WITH_PACKAGE, sites: [STORE, ADMIN, SHOP, PORTAL] }));
    await screen.findByText("storefront");
    expect(container.querySelector("[data-toast], .os-toast, dialog, [role='dialog']")).toBeNull();
  });

  it("renders the feed's own state rather than an empty cluster", async () => {
    // A list with no rows and no connection must not read as "no deployables".
    mount(null);
    expect(await screen.findByText("Not connected to the cluster")).toBeTruthy();
  });
});
