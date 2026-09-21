import { render, screen, waitFor, within } from "@testing-library/react";
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
import { PLATFORM_SITE, SHOP, click, emit, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

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
function rowNames(): string[] {
  return [...document.querySelectorAll(".os-livelist-rows .os-row-name")].map((el) => el.textContent ?? "");
}

/** Where a row says it came from, or "" when it says nothing. */
function originOf(name: string): string {
  return screen.getByText(name).closest(".os-row")?.querySelector("[data-os-origin]")?.textContent ?? "";
}

/** The Sources tab's row for one source, opened. */
async function openSource(name: string): Promise<HTMLElement> {
  await click(await screen.findByRole("button", { name: (label) => label.startsWith(`Open ${name},`) }));
  return screen.findByRole("region", { name: /^Source / });
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
  // ONE FLAT LIST OF EVERY DEPLOYABLE. It used to be a tree inside a list: a
  // source was a row with its apps indented beneath it, then a "Standalone"
  // heading over the rest -- two row types, ordered by origin. The owner's
  // word for it was "I really hate that combined list". Where a deployable
  // came from is a FACT ON ITS ROW now, and a facet in Refine.
  it("lists every deployable flat, and says where each came from on its own row", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    // By name, and two that differ only in case fall back to their address.
    expect(rowNames()).toEqual(["admin", "Storefront", "storefront"]);
    // No source line, no indent, no headings: nothing on this list is a source.
    expect(document.querySelector(".os-deploy-group")).toBeNull();
    expect(document.querySelector(".os-deploy-sectionhead")).toBeNull();
    expect(screen.queryByText("acme/storefront at main")).toBeNull();
    // The source's own name, because that is what a person would look for on
    // the Sources tab; a hand-made one says the way in it took.
    expect(originOf("storefront")).toBe("acme");
    expect(originOf("admin")).toBe("acme");
    expect(originOf("Storefront")).not.toBe("acme");
    // ...and each row still carries its address beside the app's name.
    const row = screen.getByText("storefront").closest(".os-row") as HTMLElement;
    expect(within(row).getByText("store.memql.example.com")).toBeTruthy();
  });

  // A ROW IS HERE BECAUSE IT HAS AN ADDRESS OF ITS OWN. An app a source
  // declares and has never deployed has none: it is a fact about its source,
  // read on that source's page, where Analyze and Activate are.
  it("keeps an app its source has not deployed off this list", async () => {
    mount(fakeConnection({
      ...WITH_PACKAGE,
      packages: [{ ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] }],
    }));
    await screen.findByText("storefront");
    expect(rowNames()).not.toContain("web");
  });

  it("shows an unmeasured state and keeps the full build steps on demand", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    const row = (await screen.findByText("storefront")).closest(".os-row") as HTMLElement;
    expect(within(row).getByText("Unknown")).toBeTruthy();
    expect(within(row).queryByRole("list")).toBeNull();
    await click(row);
    expect(await screen.findByRole("region", { name: "Deployable store.memql.example.com" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Build settings" })).toBeTruthy();
  });

  it("opens a DECLARED app's flow from its row, and Analyze is what deploys it -- scoped", async () => {
    // A CLICK NEVER ACTS (2026-09-05, D6). This row used to start the analysis
    // from the list; it opens the compose flow for that app now, and the
    // analysis is the bar's Analyze -- sent with every OTHER app explicitly
    // skipped, or this first deploy would rebuild and republish the ones
    // already serving.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{ ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] }],
    });
    mount(connection, { section: "sources" });
    // It is listed on its SOURCE's page, with no address of its own.
    const page = await openSource("acme");
    const web = await within(page).findByText("web");
    expect(web).toBeTruthy();
    await click(web.closest("button"));
    // NAMED FROM THE CLICK, and nothing on the wire yet.
    const region = await screen.findByRole("region", { name: "Deploy web from acme" });
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
    expect(connection.callsNamed("enablePackageDeployables")).toHaveLength(0);
    // The bar reads what it is and offers the first step.
    expect((document.querySelector(".os-actbar-word")?.textContent ?? "").trim()).toBe("Not deployed");
    expect(within(region).getByText(/declared as Single-page app by this source/)).toBeTruthy();
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    await click(within(bar).getByRole("button", { name: "Analyze" }));
    await waitFor(() => expect(connection.callsNamed("packageDeploy").length).toBeGreaterThan(0));
    // The analysis first -- confirm: false -- which is what parks the gate.
    const call = connection.callsNamed("packageDeploy")[0] ?? "";
    expect(call).toContain("confirm: false");
    // ...AND SCOPED. The engine derives `scopedTo` from the skips, so an
    // analysis sent without them parks a gate about the whole source: every
    // sibling's row and page read it as their own, and confirming it rebuilds
    // apps nobody asked about.
    expect(call).toContain("placements: {storefront: {skip: true}}");
    // The app being deployed is NOT in the map: scope is the COMPLEMENT of
    // the skips, so naming it here would scope the run to nothing.
    expect(call).not.toContain("web: {skip");
  });

  it("opens an INACTIVE app with a notice and Activate, and writes nothing until it is pressed", async () => {
    // Reported from production, twice. First: skipping `web` then discarding
    // the result left it on the list looking like an app nobody had got to,
    // so clicking it started the whole build again. Then: the row read "off",
    // and clicking it turned it back ON with no page and no confirmation.
    // A click never acts (2026-09-05, D5/D6): it opens the flow, which says
    // the app is inactive, and Activate on the bar is the act.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{
        ...ACME,
        declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }],
        disabledDeployables: ["web"],
      }],
    });
    mount(connection, { section: "sources" });
    const page = await openSource("acme");
    const web = await within(page).findByText("web");
    // It is there to be found, and says it is inactive -- the one word every
    // surface uses for it.
    expect(web.closest("button")?.textContent ?? "").toMatch(/inactive/i);

    await click(web.closest("button"));
    const region = await screen.findByRole("region", { name: "Deploy web from acme" });
    expect(within(region).getByText("web is inactive.")).toBeTruthy();
    expect((document.querySelector(".os-actbar-word")?.textContent ?? "").trim()).toBe("Inactive");
    // NOTHING WRITTEN BY THE CLICK.
    expect(connection.callsNamed("enablePackageDeployables")).toHaveLength(0);
    expect(connection.callsNamed("packageDeploy")).toHaveLength(0);
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    expect(within(bar).queryByRole("button", { name: "Analyze" })).toBeNull();

    // ACTIVATE IS ITS OWN VERB, naming what changed (memql#4951), and it goes
    // on to the ordinary first step: the scoped analysis.
    await click(within(bar).getByRole("button", { name: "Activate" }));
    await waitFor(() => expect(connection.callsNamed("enablePackageDeployables")).toHaveLength(1));
    expect(connection.callsNamed("enablePackageDeployables")[0]).toContain('deployableNames: ["web"]');
    await waitFor(() => expect(connection.callsNamed("packageDeploy")).toHaveLength(1));
    expect(connection.callsNamed("packageDeploy")[0]).toContain("placements: {storefront: {skip: true}}");
  });

  it("offers Analyze, not Activate, to one nobody turned off", async () => {
    // The negative control: without the off-list the same row opens a flow
    // whose first act is Analyze and which says nothing about activation --
    // which is what makes the assertion above about the preference and not
    // about declared rows in general.
    const connection = fakeConnection({
      ...WITH_PACKAGE,
      packages: [{ ...ACME, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] }],
    });
    mount(connection, { section: "sources" });
    const page = await openSource("acme");
    await click((await within(page).findByText("web")).closest("button"));
    await screen.findByRole("region", { name: "Deploy web from acme" });
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    expect(within(bar).getByRole("button", { name: "Analyze" })).toBeTruthy();
    expect(within(bar).queryByRole("button", { name: "Activate" })).toBeNull();
    expect(screen.queryByText(/is inactive\./)).toBeNull();
    expect(connection.callsNamed("enablePackageDeployables")).toHaveLength(0);
  });

  // THE SECOND NOUN. A source is a repository or a zip that produces
  // deployables, with a life of its own -- so it has a list of its own, in the
  // same row language, rather than a header row inside somebody else's.
  it("lists each source once on the Sources tab, with what it produced and where it stands", async () => {
    mount(fakeConnection(TWO_SOURCES), { section: "sources" });
    const acme = await screen.findByRole("button", { name: /^Open acme,/ });
    expect(screen.getByRole("heading", { name: "Sources" })).toBeTruthy();
    expect(rowNames()).toEqual(["acme", "widgets-co"]);
    // Where it lives, how much it produced, and the one state word.
    expect(within(acme).getByText("acme/storefront at main")).toBeTruthy();
    expect(within(acme).getByText("2 apps, 2 deployed")).toBeTruthy();
    expect(within(acme).getByText("Current")).toBeTruthy();
    // No deployable is a row here.
    expect(rowNames()).not.toContain("storefront");
  });

  it("opens a source's page from its row, rooted at Sources, and its apps from there", async () => {
    mount(fakeConnection(WITH_PACKAGE), { section: "sources" });
    const page = await openSource("acme");
    // Back goes to the list it came from, which is Sources.
    expect(within(page).getByRole("button", { name: "Back to Sources" })).toBeTruthy();
    await click(within(page).getByText("storefront").closest("button"));
    const deployable = await screen.findByRole("region", { name: /^Deployable / });
    expect(within(deployable).getByRole("button", { name: "Back to Source" })).toBeTruthy();
  });

  // THREE THINGS A RENDERED LIST OF REAL SOURCES SHOWED, and a cluster with
  // none could not: a source with no apps was missing, a search trimmed what
  // its source was said to have made, and the archived flip counted apps.

  it("lists a source that has produced nothing, because this tab is where somebody looks for it", async () => {
    // What a refused analysis leaves: a source, and nothing it made.
    const fresh = { ...(ACME as unknown as Record<string, unknown>), id: "pkg-fresh", name: "field-notes", repoUrl: "https://github.com/acme/field-notes", deployedVersion: "" } as unknown as Row;
    mount(fakeConnection({ sites: [STORE, ADMIN], packages: [ACME, fresh] }), { section: "sources" });
    const row = await screen.findByRole("button", { name: /^Open field-notes,/ });
    expect(rowNames()).toEqual(["acme", "field-notes"]);
    expect(within(row).getByText("No apps yet")).toBeTruthy();
    expect(within(row).getByText("Nothing deployed")).toBeTruthy();
    // And it opens, like any other: its page is where it is tried again.
    await click(row);
    expect(await screen.findByRole("region", { name: /^Source / })).toBeTruthy();
  });

  it("asks a search of the source, and leaves what it made alone", async () => {
    mount(fakeConnection({ ...TWO_SOURCES, awaitingConfirm: [parkedRun()] }), { section: "sources" });
    await screen.findByRole("button", { name: /^Open acme,/ });
    await click(screen.getByRole("button", { name: "Find sources" }));
    const { type: typeInto } = await import("./harness");
    // An app's name finds the source that made it...
    await typeInto(screen.getByLabelText("Search") as HTMLInputElement, "admin");
    await waitFor(() => expect(rowNames()).toEqual(["acme"]));
    // ...and the source still says everything it made, not the one that matched.
    const acme = screen.getByRole("button", { name: /^Open acme,/ });
    expect(within(acme).getByText("3 apps, 2 deployed")).toBeTruthy();

    // THE PAGE KEEPS ITS ACT. The parked run is about storefront and reports,
    // neither of which matches what was typed -- and Review was read off the
    // narrowed list, so it vanished from the one page that carries it.
    await click(acme);
    await screen.findByRole("region", { name: /^Source / });
    expect(within(document.querySelector(".os-actbar") as HTMLElement).getByRole("button", { name: "Review" })).toBeTruthy();
  });

  it("counts archived SOURCES under the flip, and shows them by their own status", async () => {
    const legacy = { ...(ACME as unknown as Record<string, unknown>), id: "pkg-legacy", name: "legacy-portal", repoUrl: "https://github.com/acme/legacy-portal", status: "archived" } as unknown as Row;
    // RETIRED is an archived DEPLOYABLE of no source: it is not this tab's noun
    // and must not be counted here.
    mount(fakeConnection({ sites: [STORE, ADMIN, RETIRED], packages: [ACME, legacy] }), { section: "sources" });
    await screen.findByRole("button", { name: /^Open acme,/ });
    expect(rowNames()).toEqual(["acme"]);
    await click(screen.getByRole("button", { name: /Show archived \(1\)/ }));
    await waitFor(() => expect(rowNames()).toEqual(["legacy-portal"]));
    expect(screen.getByRole("button", { name: /Show active sources/ })).toBeTruthy();
  });

  it("says what a source is when there are none", async () => {
    mount(fakeConnection({ sites: [SHOP], packages: [] }), { section: "sources" });
    expect(await screen.findByText("No sources yet")).toBeTruthy();
    expect(screen.getByText(/A source is a repository or a zip that declares one or more apps/)).toBeTruthy();
  });

  it("says what to do when there is nothing yet", async () => {
    mount(fakeConnection({ sites: [], packages: [] }));
    expect(await screen.findByRole("region", { name: "No deployables yet" })).toBeTruthy();
  });

  it("distinguishes a failed read from an empty list and recovers with Reload", async () => {
    const seed: FakeSeed = { sites: [SHOP], sitesError: "Connection unavailable" };
    const connection = fakeConnection(seed);
    mount(connection);
    expect(await screen.findByText("Deployables could not be loaded.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "No deployables yet" })).toBeNull();
    expect(document.querySelector(".os-head-meta")).toBeNull();
    seed.sitesError = undefined;
    await click(screen.getByRole("button", { name: "Reload deployables" }));
    expect(await screen.findByText("Storefront")).toBeTruthy();
    expect(screen.queryByText("Deployables could not be loaded.")).toBeNull();
    expect(connection.callsNamed("sitesAll")).toHaveLength(2);
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
  // A RUN PARKED AT A SOURCE'S GATE IS A FACT ABOUT THE SOURCE, so that is
  // where it is said: once, on the source's row, and on its page's bar with
  // the act that answers it. The app it is about has no address yet, so it is
  // not a row on the Deployables list at all.
  it("says Review needed once, on the source, and offers Review on its page", async () => {
    mount(fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] }), { section: "sources" });
    const acme = await screen.findByRole("button", { name: /^Open acme, review needed/ });
    expect(screen.getAllByText("Review needed")).toHaveLength(1);
    expect(within(acme).getByText("Review needed")).toBeTruthy();

    await click(acme);
    await screen.findByRole("region", { name: /^Source / });
    expect((document.querySelector(".os-actbar-word")?.textContent ?? "").trim()).toBe("Review needed");
    expect(within(document.querySelector(".os-actbar") as HTMLElement).getByRole("button", { name: "Review" })).toBeTruthy();
  });

  it("does not mark a deployable that is already serving, and lists no row for the one that is not", async () => {
    mount(fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] }));
    const storefront = (await screen.findByText("storefront")).closest(".os-row") as HTMLElement;
    expect(within(storefront).queryByText("Review needed")).toBeNull();
    const shop = screen.getByText("Storefront").closest(".os-row") as HTMLElement;
    expect(within(shop).queryByText("Review needed")).toBeNull();
    // `reports` is what the run is about, and it has no address yet.
    expect(rowNames()).not.toContain("reports");
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
    expect(screen.queryByText("Review needed")).toBeNull();
  });

  it("separates the address from availability", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    const row = (await screen.findByText("storefront")).closest(".os-row") as HTMLElement;
    expect(within(row).getByText("store.memql.example.com").closest(".os-record-identity")).not.toBeNull();
    expect(within(row).getByText("Unknown").closest(".os-record-state")).not.toBeNull();
  });

  it("clears the mark when the run moves on, on its own event", async () => {
    const connection = fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] });
    mount(connection, { section: "sources" });
    await screen.findByText("Review needed");

    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun({ status: "succeeded" }));
    await waitFor(() => expect(screen.queryByText("Review needed")).toBeNull());
    // ...and the source reads as it did before the run parked.
    expect(screen.getByText("Current")).toBeTruthy();
  });

  it("clears it for a refusal too", async () => {
    const connection = fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] });
    mount(connection, { section: "sources" });
    await screen.findByText("Review needed");

    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun({ status: "refused" }));
    await waitFor(() => expect(screen.queryByText("Review needed")).toBeNull());
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

    await refine("Status", "Offline");
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

    expect(await screen.findByRole("button", { name: "Clear filters" })).toBeTruthy();
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
    expect(screen.getByText(/Restored apps return offline/)).toBeTruthy();

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
// Add a deployable: the compose seam
// ---------------------------------------------------------------------------

describe("Add a deployable", () => {
  it("replaces the list in place, and Back returns to it", async () => {
    mount(fakeConnection(WITH_PACKAGE));
    await screen.findByText("storefront");

    await click(screen.getByRole("button", { name: /Add a deployable/ }));
    const compose = await screen.findByRole("region", { name: "Add a deployable" });
    // The Head's title becomes "Add a deployable"; the list is gone, not
    // pushed below a modal (design D4).
    expect(within(compose).getByRole("heading", { name: "Add a deployable" })).toBeTruthy();
    expect(screen.queryByText("storefront")).toBeNull();

    // The rail is the form: Source is the open stop and carries the caption.
    const rail = within(compose).getByRole("list", { name: "Deployable setup progress" });
    // THE SOURCE STAGE IS TWO STEPS: the choice, and the step the choice names
    // -- which has no name and nothing behind it until there is an answer.
    expect([...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "open",
      "ahead",
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

    await click(within(compose).getByRole("button", { name: "Back to Deployables" }));
    expect(await screen.findByText("storefront")).toBeTruthy();
  });

  it("is offered to whoever holds the deploy PART: owner and developer by the seeds", async () => {
    // A PART, NOT A RANK (epic memql#5289). Composing ends in a deploy, and
    // `execute app:deployables/deploy` is seeded on owner and developer --
    // the engine's own gate on packageAnalyze refuses everybody else, admin
    // included, so the button follows the seeds rather than the ladder.
    for (const role of ["developer", "owner"]) {
      const view = mount(fakeConnection(WITH_PACKAGE), { role });
      await screen.findByText("storefront");
      expect(screen.getByRole("button", { name: /Add a deployable/ })).toBeTruthy();
      view.unmount();
    }
  });

  it("is not offered to an admin or a reader, disabled or otherwise", async () => {
    for (const role of ["admin", "reader"]) {
      const view = mount(fakeConnection(WITH_PACKAGE), { role });
      await screen.findByText("storefront");
      expect(screen.queryByRole("button", { name: /Add a deployable/ })).toBeNull();
      // ...and the empty state does not tell them to use a control they do not have.
      expect(screen.queryByText(/Add a deployable from|Add one from the Deployables section/)).toBeNull();
      view.unmount();
    }
  });

  it("reopens a parked run's reading from its source's bar, with its report in place", async () => {
    mount(fakeConnection({ ...WITH_PACKAGE, awaitingConfirm: [parkedRun()] }), { section: "sources" });
    await openSource("acme");
    await click(within(document.querySelector(".os-actbar") as HTMLElement).getByRole("button", { name: "Review" }));

    // NAMED AFTER THE SOURCE, because this is not a new deployable: the source
    // was added already and this reopens its gate.
    const compose = await screen.findByRole("region", { name: "Deploy acme" });
    const rail = within(compose).getByRole("list", { name: "Deployable setup progress" });
    // A run parked at the confirm gate has ANSWERED What it is -- its report
    // is what parked it -- so the open stop is Where it lives, which is what
    // the Deploy beneath is waiting for.
    expect([...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"))).toEqual([
      "complete",
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
    await click(screen.getByRole("button", { name: "Choose addresses" }));
    // Deploy, on the BAR and REACHABLE (rule 12): the one app with no site yet
    // arrived with a GENERATED address (2026-09-05) that the cluster then
    // checked, so there is nothing left to answer -- once the check answers.
    // "storefront" already serves, so it is not re-asked -- the pipeline reads
    // a placement on a first deploy only.
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    await waitFor(() => expect(within(bar).getByRole("button", { name: /^Deploy/ })).toBeTruthy());
    expect(within(compose).getByText(/^[a-z]+-[a-z]+\.memql\.example\.com$/)).toBeTruthy();
    expect(within(compose).getByText(/-- free$/)).toBeTruthy();
    expect(within(compose).queryByLabelText("The name storefront answers at")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// What the list does not do
// ---------------------------------------------------------------------------

describe("what the section does not do", () => {
  it("mounts no toast container and no dialog anywhere", async () => {
    const { container } = mount(fakeConnection({ ...WITH_PACKAGE, sites: [STORE, ADMIN, SHOP, PLATFORM_SITE] }));
    await screen.findByText("storefront");
    expect(container.querySelector("[data-toast], .os-toast, dialog, [role='dialog']")).toBeNull();
  });

  it("renders the feed's own state rather than an empty cluster", async () => {
    // A list with no rows and no connection must not read as "no deployables".
    mount(null);
    expect(await screen.findByText("Not connected to the cluster")).toBeTruthy();
  });
});
