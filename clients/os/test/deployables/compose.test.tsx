import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { DEPLOYMENT_CONCEPT } from "../../src/apps/deployables/packages/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import {
  artifactRow,
  click,
  emit,
  fakeConnection,
  probeReply,
  withSession,
  zipReply,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// The compose flow (epic memql#4885, task memql#4891): the five stops as
// INPUTS -- the probes the Source stop runs, the credential picker, the
// placements the Where-it-lives stop asks for, and the two clicks a first
// deploy takes (design D5).
//
// ===========================================================================
// WHAT IS ASSERTED, AND AGAINST WHAT
// ===========================================================================
// Everything goes through `connection.query` and `connection.subscriptions`
// exactly as production does, so the real LiveCollection, the real
// projections and the real GENERATED BUILDERS all run -- a suite that stubbed
// `query.sourceProbe` would record its arguments and never render the call,
// which is how a feature ships green and fails at parse on every call. The
// assertions are what a person SEES (a sentence, a control, a rail state) and
// what reached the WIRE (the call string), never a `.value`.
//
// ===========================================================================
// THE THREE PARKED CASES
// ===========================================================================
// The retired confirm gate's three assertions are turned on here under their
// exact former titles. Their reasoning is inherited unchanged: a hostname is
// chosen at a deployable's FIRST deploy and remembered on its site row, so
// the address is asked for only where it has never been answered -- which is
// the difference between a gate that protects somebody and a gate they learn
// to click past.

function memStore() {
  const data = new Map<string, string>();
  // SOURCE GROUPS START COLLAPSED IN PRODUCTION (epic memql#4937 follow-up),
  // and every assertion in this file is about what the list SHOWS rather than
  // about the disclosure -- so the group is seeded open, which is the
  // precondition those tests were written under. The default itself is
  // asserted in list.test.tsx ("collapsed until you open it"), where it
  // belongs.
  const seeded = { version: 1, density: "comfortable", expandedSources: ["pkg:pkg-acme"] };
  data.set("memql-os-deployables-v1", JSON.stringify(seeded));
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection | null, opts: { role?: string; section?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId={opts.section ?? "deployables"}
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

/**
 * Open New deployable, and answer with the compose region plus its render.
 *
 * The RENDER comes back because a couple of cases mount TWICE in one test (a
 * cluster owner and then an admin, say), and RTL's cleanup runs between
 * tests rather than within one -- a second mount without unmounting the first
 * leaves two regions with the same name and every query becomes ambiguous.
 */
async function compose(
  connection: FakeConnection,
  opts: { role?: string } = {},
): Promise<{ region: HTMLElement; view: ReturnType<typeof render> }> {
  const view = mount(connection, opts);
  await click(await screen.findByRole("button", { name: /New deployable/ }));
  return { region: await screen.findByRole("region", { name: "New deployable" }), view };
}

async function chooseSource(region: HTMLElement, name: RegExp): Promise<void> {
  await click(within(region).getByRole("radio", { name }));
}

/**
 * Choose the repository source, and ask for the URL-plus-token form.
 *
 * Since GitHub Connect (memql#4915) the repository branch leads with the
 * connection -- a picker for somebody who has one, Connect GitHub for
 * somebody who does not -- and the pasted URL and token live behind "Use a
 * token instead", closed. Every case below answers with a URL, so every case
 * asks for that form the way a person would; nothing they assert about it
 * changed.
 */
async function chooseRepository(region: HTMLElement): Promise<void> {
  await chooseSource(region, /A repository/);
  await click(within(region).getByRole("button", { name: "Use a token instead" }));
}

/** Type into a field by its accessible name, the way a person would. */
async function fill(label: string, value: string): Promise<void> {
  const input = screen.getByLabelText(label) as HTMLInputElement;
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** Leave the field -- what makes the Source stop ask the cluster about a repository. */
async function blur(label: string): Promise<void> {
  await act(async () => {
    fireEvent.blur(screen.getByLabelText(label));
  });
}

/** Choose an option of a kit Select by its accessible name. */
async function choose(label: string, option: string | RegExp): Promise<void> {
  await click(screen.getByLabelText(label));
  await click(await screen.findByRole("option", { name: option }));
}

function railStates(region: HTMLElement): (string | null)[] {
  const rail = within(region).getByRole("list", { name: "Deployable stops" });
  return [...rail.querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"));
}

beforeEach(() => {
  h.connection = null;
});

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const REPO = "https://github.com/acme/storefront";
const URL_FIELD = "The repository this deployable is built from";
const NAME_FIELD = "What this deployable is called";
const CREDENTIAL_FIELD = "The credential this source is fetched under, on github.com";

const CREDENTIAL: Row = {
  id: "cred-acme",
  ownerUserId: "u-me",
  host: "github.com",
  label: "acme deploy token",
  fingerprint: "...ab12",
  status: "active",
  lastUsedAt: "",
  revokedAt: "",
  createdAt: "2026-08-20T00:00:00Z",
} as unknown as Row;

const REPORT = {
  name: "acme",
  formatVersion: 1,
  deployables: [
    {
      name: "storefront",
      kind: "spa",
      path: "clients/web",
      buildPlan: "already built: dist",
      output: "dist",
      prebuilt: true,
    },
  ],
  dslDomains: [],
  problems: [],
  ok: true,
};

/** The run `packageDeploy(confirm: false)` parks. */
function parkedRun(packageId: string, over: Record<string, unknown> = {}): Row {
  return {
    id: "dep-1",
    packageId,
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

/** A source declaring TWO apps, so "skip one" is expressible. */
const REPORT_TWO = {
  ...REPORT,
  deployables: [
    ...REPORT.deployables,
    { name: "web", kind: "spa", path: "clients/marketing", buildPlan: "already built: dist", output: "dist", prebuilt: true },
  ],
};

const ZIP = artifactRow({ id: "artifact-zip", title: "storefront-build.zip" });

/** The id `createPackage` minted, read back off the wire. */
function mintedPackageId(connection: FakeConnection): string {
  return /packageId: "([^"]*)"/.exec(connection.callsNamed("createPackage")[0] ?? "")?.[1] ?? "";
}

// ---------------------------------------------------------------------------
// A repository, probed
// ---------------------------------------------------------------------------


/**
 * The compose flow's forward act, on the BAR (epic memql#4937, rule 12).
 *
 * It was in the Head, at the top of the flow, so answering a long Source stop
 * meant scrolling back UP to continue -- which is the complaint this epic
 * started from. Cancel sits beside it, so leaving is as reachable as
 * continuing.
 *
 * AND IT IS ABSENT RATHER THAN DISABLED when there is more to answer: a
 * disabled control is one somebody has to read past to learn it is not for
 * them yet, and the bar says what is still needed instead.
 */
function forwardAct(name: string): HTMLButtonElement | null {
  const acts = document.querySelector(".os-actbar-acts");
  if (acts === null) return null;
  const hit = [...acts.querySelectorAll("button")].find((b) => (b.textContent ?? "").trim() === name);
  return (hit as HTMLButtonElement | undefined) ?? null;
}

describe("the compose flow: the Source stop's probe", () => {
  it("says a public repository is public, and names the branch it will follow", async () => {
    const connection = fakeConnection({ sourceProbe: { "": probeReply() } });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);

    expect(await within(region).findByText("public, default branch main")).toBeTruthy();
    // The call reached the wire through the generated builder, and carried no
    // credential: a public repository needs none.
    expect(connection.callsNamed("sourceProbe")).toEqual([`builtin sourceProbe(repoUrl: "${REPO}")`]);
    // Nothing is parked: Analyze is reachable once it has a name.
    await fill(NAME_FIELD, "storefront");
    expect(forwardAct("Analyze")).toBeTruthy();
  });

  it("says 'private, or not there' and reveals the credential field", async () => {
    const connection = fakeConnection({
      sourceProbe: { "": probeReply({ reachable: false, reason: "not_found_or_private" }) },
      credentials: [CREDENTIAL],
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);

    expect(await within(region).findByText("private, or not there")).toBeTruthy();
    expect(within(region).getByLabelText(CREDENTIAL_FIELD)).toBeTruthy();
    // A definite answer ABOUT THE REPOSITORY parks the flow: the rail stops
    // at Source and Analyze is out of reach.
    expect(railStates(region)[0]).toBe("stopped");
    await fill(NAME_FIELD, "storefront");
    expect(forwardAct("Analyze")).toBeNull();
  });

  it("says 'this token cannot see it' when a chosen credential still cannot", async () => {
    const connection = fakeConnection({
      sourceProbe: {
        "": probeReply({ reachable: false, reason: "not_found_or_private" }),
        "cred-acme": probeReply({ reachable: false, reason: "credential_cannot_see_it" }),
      },
      credentials: [CREDENTIAL],
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await within(region).findByText("private, or not there");

    await choose(CREDENTIAL_FIELD, /acme deploy token/);
    expect(await within(region).findByText("this token cannot see it")).toBeTruthy();
    expect(connection.callsNamed("sourceProbe")).toContain(
      `builtin sourceProbe(repoUrl: "${REPO}", credentialId: "cred-acme")`,
    );
  });

  it("renders the OS copy for a credential this cluster refuses to resolve", async () => {
    for (const [reason, headline] of [
      ["credential_not_found", "This source's credential is not one you can use"],
      ["credential_revoked", "This source's credential was revoked"],
    ] as const) {
      const connection = fakeConnection({ sourceProbe: { "": probeReply({ reachable: false, reason }) } });
      const { region, view } = await compose(connection);
      await chooseRepository(region);
      await fill(URL_FIELD, REPO);
      await blur(URL_FIELD);
      expect(await within(region).findByText(headline)).toBeTruthy();
      view.unmount();
    }
  });

  it("refuses a non-GitHub URL its own way: the sentence, and Analyze out of reach", async () => {
    const connection = fakeConnection({
      sourceProbe: {
        "": probeReply({ host: "gitlab.com", reachable: false, defaultBranch: "", reason: "source_host_unsupported" }),
      },
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, "https://gitlab.com/acme/storefront");
    await blur(URL_FIELD);

    expect(await within(region).findByText("only github.com today, or upload a zip")).toBeTruthy();
    await fill(NAME_FIELD, "storefront");
    expect(forwardAct("Analyze")).toBeNull();
    // ...and no credential field: no token makes github.com out of a gitlab URL.
    expect(within(region).queryByLabelText(CREDENTIAL_FIELD)).toBeNull();
  });

  it("says it is rate-limited and lets the deploy go ahead anyway", async () => {
    const connection = fakeConnection({
      sourceProbe: { "": probeReply({ reachable: false, defaultBranch: "", reason: "rate_limited" }) },
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);

    expect(await within(region).findByText(/rate-limiting this cluster/)).toBeTruthy();
    await fill(NAME_FIELD, "storefront");
    // An answer about the PROBE, not about the repository: nothing parks.
    expect(railStates(region)[0]).toBe("complete");
    expect(forwardAct("Analyze")).toBeTruthy();
  });

  it("NEVER blocks Analyze on a probe that could not run, and shows the server's sentence", async () => {
    // Design H: the fetch is the authority and the probe is a courtesy. A
    // probe that threw must not stop somebody deploying a public repository.
    const connection = fakeConnection({ sourceProbeError: "source_unreadable: api.github.com is unreachable" });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);

    expect(await within(region).findByText("source_unreadable: api.github.com is unreachable")).toBeTruthy();
    await fill(NAME_FIELD, "storefront");
    expect(forwardAct("Analyze")).toBeTruthy();
    // The field is still editable -- nothing is wrong with what was typed.
    expect((screen.getByLabelText(URL_FIELD) as HTMLInputElement).disabled).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The token
// ---------------------------------------------------------------------------

describe("the compose flow: a token, pasted once", () => {
  it("sends the token in sourceCredentialCreate and in NO other call", async () => {
    const secret = "github_pat_" + "11ABCDEF" + "0123456789";
    const connection = fakeConnection({
      sourceProbe: {
        "": probeReply({ reachable: false, reason: "not_found_or_private" }),
        "cred-new": probeReply({ private: true }),
      },
      credentials: [],
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await within(region).findByText("private, or not there");

    await choose(CREDENTIAL_FIELD, /Add a credential/);
    // The stop states the shape of token to make, where the token is typed.
    expect(within(region).getByText(/fine-grained personal access token/)).toBeTruthy();
    expect(within(region).getByText(/contents: read/)).toBeTruthy();
    await fill("A name for this github.com credential", "work laptop");
    await fill("The github.com access token", secret);
    await click(within(region).getByRole("button", { name: "Add credential" }));

    expect(connection.calls.filter((call) => call.includes(secret))).toEqual([
      `builtin sourceCredentialCreate(host: "github.com", label: "work laptop", token: ${JSON.stringify(secret)})`,
    ]);
    // ...and the new credential is the chosen one, re-probed under it.
    expect(connection.callsNamed("sourceProbe")).toContain(
      `builtin sourceProbe(repoUrl: "${REPO}", credentialId: "cred-new")`,
    );
    expect(await within(region).findByText(/private, and reachable under this credential/)).toBeTruthy();
  });

  it("renders the server's refusal beside the field", async () => {
    const connection = fakeConnection({
      sourceProbe: { "": probeReply({ reachable: false, reason: "not_found_or_private" }) },
      credentialCreateError: "source_host_unsupported: only github.com is admitted today",
    });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await within(region).findByText("private, or not there");

    await choose(CREDENTIAL_FIELD, /Add a credential/);
    await fill("A name for this github.com credential", "work laptop");
    await fill("The github.com access token", "github_pat_" + "nope");
    await click(within(region).getByRole("button", { name: "Add credential" }));

    expect(await within(region).findByText("That credential was not stored.")).toBeTruthy();
    // The server's sentence, verbatim beneath the OS's own line -- the code
    // prefix the engine puts in front of it becomes the headline's job.
    expect(within(region).getByText("only github.com is admitted today")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// A zip in Files
// ---------------------------------------------------------------------------

describe("the compose flow: a zip in Files", () => {
  it("takes the PACKAGE path for a zip with a manifest at its root", async () => {
    const connection = fakeConnection({
      artifacts: [ZIP],
      artifactProbe: { "artifact-zip": zipReply({ isPackage: true }) },
    });
    const { region } = await compose(connection);
    await chooseSource(region, /A zip in Files/);
    await click(await within(region).findByText("storefront-build.zip"));

    expect(await within(region).findByText(/a package -- 12 files/)).toBeTruthy();
    expect(connection.callsNamed("artifactProbe")).toEqual(['builtin artifactProbe(artifactId: "artifact-zip")']);
    // No kind is asked for: a package declares each app's kind in its manifest.
    expect(within(region).queryByLabelText("What kind of deployable this is")).toBeNull();
  });

  it("takes the HAND-MADE path for a built site, and asks for the kind", async () => {
    const connection = fakeConnection({
      artifacts: [ZIP],
      artifactProbe: { "artifact-zip": zipReply({ isBuiltSite: true }) },
    });
    const { region } = await compose(connection);
    await chooseSource(region, /A zip in Files/);
    await click(await within(region).findByText("storefront-build.zip"));

    expect(await within(region).findByText(/a built site -- index.html at the root/)).toBeTruthy();
    expect(within(region).getByLabelText("What kind of deployable this is")).toBeTruthy();
    // The one sentence about the three kinds that are NOT offered, said once
    // in place of three disabled controls.
    expect(within(region).getByText(/Android, iOS and macOS builds are not deployables/)).toBeTruthy();
  });

  it("creates the draft at Analyze and publishes the zip at Deploy", async () => {
    const connection = fakeConnection({
      artifacts: [ZIP],
      artifactProbe: { "artifact-zip": zipReply({ isBuiltSite: true }) },
    });
    const { region } = await compose(connection);
    await chooseSource(region, /A zip in Files/);
    await click(await within(region).findByText("storefront-build.zip"));
    await choose("What kind of deployable this is", "Single-page app");
    await fill(NAME_FIELD, "Landing page");

    // Build reads SKIPPED before anything runs: a built site IS its output.
    expect(railStates(region)[3]).toBe("skipped");
    expect(within(region).getByText("its built output is in the source")).toBeTruthy();

    // A GENERATED ADDRESS, for when it should say nothing about what it
    // serves. It fills the same field rather than replacing it, so it is a
    // starting point like the suggestion and not a decision.
    const generate = within(region).getByRole("button", { name: /Generate an address for Landing page/ });
    await click(generate);
    const field = within(region).getByLabelText("The name Landing page answers at") as HTMLInputElement;
    expect(field.value).toMatch(/^[a-z]+-[a-z]+$/);

    await fill("The name Landing page answers at", "landing");
    await click(forwardAct("Analyze"));

    const create = connection.callsNamed("createSite")[0] ?? "";
    expect(create).toContain('hostname: "landing.memql.example.com"');
    expect(create).toContain('kind: "spa"');
    expect(create).toContain('status: "draft"');
    // Nothing has been published yet: the zip is deployed by the next click.
    expect(connection.callsNamed("sitePublishFromArtifact")).toHaveLength(0);

    await waitFor(() => expect(forwardAct("Deploy")).toBeTruthy());
    await click(forwardAct("Deploy"));
    expect(connection.callsNamed("sitePublishFromArtifact")[0]).toContain('artifactId: "artifact-zip"');
    expect(await within(region).findByText("Published. It is not serving yet.")).toBeTruthy();
  });

  it("says what a zip that is NEITHER is, and does not continue", async () => {
    const connection = fakeConnection({
      artifacts: [ZIP],
      artifactProbe: { "artifact-zip": zipReply({ fileCount: 3 }) },
    });
    const { region } = await compose(connection);
    await chooseSource(region, /A zip in Files/);
    await click(await within(region).findByText("storefront-build.zip"));

    expect(await within(region).findByText(/This zip holds 3 files/)).toBeTruthy();
    expect(forwardAct("Analyze")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Pushed by your CI
// ---------------------------------------------------------------------------

describe("the compose flow: pushed by your CI", () => {
  it("is a cluster owner's answer, and nobody else's", async () => {
    const { region: owner, view } = await compose(fakeConnection({}), { role: "owner" });
    expect(within(owner).getByRole("radio", { name: /Pushed by your CI/ })).toBeTruthy();
    view.unmount();

    const { region: admin } = await compose(fakeConnection({}), { role: "admin" });
    expect(within(admin).queryByRole("radio", { name: /Pushed by your CI/ })).toBeNull();
    // ...and the other two ARE offered, so the absence is about the rung
    // rather than about the stop having failed to render.
    expect(within(admin).getByRole("radio", { name: /A repository/ })).toBeTruthy();
  });

  it("shows the site id, the bundle route and the mint command after Analyze", async () => {
    const connection = fakeConnection({});
    const { region } = await compose(connection);
    await chooseSource(region, /Pushed by your CI/);
    await fill(NAME_FIELD, "Marketing site");
    await choose("What kind of deployable this is", "Website");
    await fill("The name Marketing site answers at", "marketing");
    await click(forwardAct("Analyze"));

    // The draft was created at the address chosen on the Where-it-lives stop,
    // with the placeholder bundle the convention records.
    const create = connection.callsNamed("createSite")[0] ?? "";
    expect(create).toContain('hostname: "marketing.memql.example.com"');
    expect(create).toContain('status: "draft"');
    expect(create).toContain("/pending/");

    expect(await within(region).findByText(/^POST https:\/\/api\.memql\.example\.com\/sites\/[^/]+\/bundles$/)).toBeTruthy();
    expect(
      within(region).getByText(/memql service-account-token mint --label marketing-site-ci --subject system:ci-publish/),
    ).toBeTruthy();
    // CHOSEN ONCE: the address is now a fact, not a field. Editing a slug
    // `createSite` has already claimed would change nothing.
    expect(within(region).queryByLabelText("The name Marketing site answers at")).toBeNull();
    expect(within(region).getByText("marketing.memql.example.com")).toBeTruthy();
    // Nothing is deployed from here: the Live stop is what waits.
    expect(forwardAct("Deploy")).toBeNull();
    expect(within(region).getByText(/Waiting for the first push from your CI/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Where each app will live -- the three cases parked by memql#4889
// ---------------------------------------------------------------------------

/** Compose a repository source through Analyze, and land on the parked run. */
async function analyzed(
  seed: FakeSeed = {},
  opts: { role?: string } = {},
): Promise<{ connection: FakeConnection; region: HTMLElement; view: ReturnType<typeof render> }> {
  const connection = fakeConnection({ sourceProbe: { "": probeReply() }, ...seed });
  const { region, view } = await compose(connection, opts);
  await chooseRepository(region);
  await fill(URL_FIELD, REPO);
  await blur(URL_FIELD);
  await fill(NAME_FIELD, "acme");
  await click(forwardAct("Analyze"));
  await emit(connection, DEPLOYMENT_CONCEPT, parkedRun(mintedPackageId(connection)), "NODE_CREATED");
  await within(region).findByText("clients/web");
  return { connection, region, view };
}

describe("the compose flow: where each app will live", () => {
  it("asks for a hostname only on a deployable's first deploy, and previews it", async () => {
    const { region } = await analyzed();

    // The report the run parked with is at What it is...
    expect(within(region).getByText("clients/web")).toBeTruthy();
    // ...and the address is asked for, previewed under the cluster's domain
    // at keystroke rate.
    await fill("The name storefront answers at", "shop");
    expect(within(region).getByText("shop.memql.example.com")).toBeTruthy();
    expect(
      within(region).getByText("Chosen once. A later deploy of this source keeps the same addresses."),
    ).toBeTruthy();
  });

  it("refuses a reserved name client-side, exactly as the server would", async () => {
    const { region } = await analyzed();

    await fill("The name storefront answers at", "api");
    expect(within(region).getByText(/"api" is reserved/)).toBeTruthy();
    // Deploy is out of reach while any app has no address the server could take.
    expect(forwardAct("Deploy")).toBeNull();

    await fill("The name storefront answers at", "shop");
    await waitFor(() =>
      expect(forwardAct("Deploy")).toBeTruthy(),
    );
  });

  it("blocks the deploy until it is confirmed, and shows the report first", async () => {
    const { connection } = await analyzed();

    // ANALYZE PARKED THE RUN AND BUILT NOTHING: the gate is always present.
    expect(connection.callsNamed("packageDeploy")).toHaveLength(1);
    expect(connection.callsNamed("packageDeploy")[0]).toContain("confirm: false");

    await fill("The name storefront answers at", "shop");
    await click(forwardAct("Deploy"));

    const confirmed = connection.callsNamed("packageDeploy")[1] ?? "";
    expect(confirmed).toContain("confirm: true");
    expect(confirmed).toContain('placements: {storefront: {hostname: "shop.memql.example.com"}}');
  });

  it("carries a SKIP all the way to packageDeploy", async () => {
    // THE GAP THAT LET SKIP SHIP BROKEN TWICE. `placementsFrom` and
    // `placementsPayload` were each tested alone and each was right; nothing
    // asserted the wizard's own click produced a call carrying the flag. The
    // engine skips at BUILD and at publish, so a lost flag is minutes of
    // building an app somebody said they did not want, and a site at draft.
    const connection = fakeConnection({ sourceProbe: { "": probeReply() } });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await fill(NAME_FIELD, "acme");
    await click(forwardAct("Analyze"));
    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun(mintedPackageId(connection), { report: REPORT_TWO }), "NODE_CREATED");
    await within(region).findByText("clients/marketing");

    await fill("The name storefront answers at", "shop");
    await click(within(region).getByRole("radiogroup", { name: "Deploy or skip web" }).querySelectorAll("[role=radio]")[1]);
    await click(forwardAct("Deploy"));

    const confirmed = connection.callsNamed("packageDeploy").at(-1) ?? "";
    expect(confirmed).toContain("confirm: true");
    expect(confirmed).toContain("skip: true");
    // ...and the skipped app is not given an address it was never asked for.
    expect(confirmed).toContain('storefront: {hostname: "shop.memql.example.com"}');
    expect(confirmed).not.toContain('web: {hostname');
  });

  it("records a skip as a STANDING choice, not just a fact about this run", async () => {
    // Reported from production: skipping `web` deployed nothing (correct), and
    // then `web` came back on the list as an ordinary "not deployed" row --
    // indistinguishable from one nobody had got to. Clicking it deployed the
    // very app that had just been declined.
    const connection = fakeConnection({ sourceProbe: { "": probeReply() } });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await fill(NAME_FIELD, "acme");
    await click(forwardAct("Analyze"));
    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun(mintedPackageId(connection), { report: REPORT_TWO }), "NODE_CREATED");
    await within(region).findByText("clients/marketing");

    await fill("The name storefront answers at", "shop");
    await click(within(region).getByRole("radiogroup", { name: "Deploy or skip web" }).querySelectorAll("[role=radio]")[1]);
    await click(forwardAct("Deploy"));

    const off = connection.callsNamed("disablePackageDeployables").at(-1) ?? "";
    expect(off).toContain('deployableNames: ["web"]');
    // ...and the app that was NOT skipped is not turned off with it.
    expect(off).not.toContain("storefront");
  });

  it("turns nothing off when nothing was skipped", async () => {
    // The negative control: an ordinary deploy must not write an off-list.
    const { connection } = await analyzed();
    await fill("The name storefront answers at", "shop");
    await click(forwardAct("Deploy"));
    expect(connection.callsNamed("disablePackageDeployables")).toHaveLength(0);
  });

  it("offers a cluster owner the client AND their own domain; an admin only the client", async () => {
    const { region, view } = await analyzed();
    expect(within(region).getByLabelText("The client storefront is for")).toBeTruthy();
    expect(within(region).getByLabelText("A domain of the client's own for storefront")).toBeTruthy();
    view.unmount();

    const { region: admin } = await analyzed({}, { role: "admin" });
    expect(within(admin).getByLabelText("The client storefront is for")).toBeTruthy();
    expect(within(admin).queryByLabelText("A domain of the client's own for storefront")).toBeNull();
  });

  it("carries every placement half that was answered, and omits every one that was not", async () => {
    const { connection } = await analyzed();

    await fill("The name storefront answers at", "shop");
    await fill("A domain of the client's own for storefront", "Shop.Acme.COM.");
    await click(forwardAct("Deploy"));

    const confirmed = connection.callsNamed("packageDeploy")[1] ?? "";
    // The own domain is normalized on the way out; the client half is ABSENT
    // rather than "", because an explicit empty is a request to tie to nobody.
    expect(confirmed).toContain('ownDomain: "shop.acme.com"');
    expect(confirmed).not.toContain("accountId");
  });
});

// ---------------------------------------------------------------------------
// A refused placement half is not a failed deploy
// ---------------------------------------------------------------------------

describe("the compose flow: what the run answers", () => {
  it("renders a refused domain at Where it lives, as a deployable that IS serving", async () => {
    const { connection, region } = await analyzed();

    await emit(
      connection,
      DEPLOYMENT_CONCEPT,
      parkedRun(mintedPackageId(connection), {
        status: "succeeded",
        deployables: [
          {
            name: "storefront",
            siteId: "site-shop",
            hostname: "shop.memql.example.com",
            bundleRef: "blob://sites/site-shop/v1/",
            version: "v1",
            created: true,
            domainRefusal: {
              code: "deployable_domain_refused",
              message: "custom_domain_reserved: acme.com is this cluster's own domain",
              scope: "storefront",
              fatal: false,
            },
          },
        ],
        finishedAt: "2026-09-01T13:05:00Z",
      }),
    );

    // The headline says it is LIVE. The guard's own sentence is beneath it.
    expect(
      await within(region).findByText("It is live at its cluster address, but the domain was not bound"),
    ).toBeTruthy();
    expect(within(region).getByText("custom_domain_reserved: acme.com is this cluster's own domain")).toBeTruthy();
    expect(within(region).getByText("Published to shop.memql.example.com", { exact: false })).toBeTruthy();
  });

  it("renders a refused CLIENT tie the same way, and says the deployable is live", async () => {
    const { connection, region } = await analyzed();

    await emit(
      connection,
      DEPLOYMENT_CONCEPT,
      parkedRun(mintedPackageId(connection), {
        status: "succeeded",
        deployables: [
          {
            name: "storefront",
            siteId: "site-shop",
            hostname: "shop.memql.example.com",
            bundleRef: "blob://sites/site-shop/v1/",
            version: "v1",
            created: true,
            accountRefusal: {
              code: "deployable_account_refused",
              message: "account_not_found: no client with that id is one you can read",
              scope: "storefront",
              fatal: false,
            },
          },
        ],
        finishedAt: "2026-09-01T13:05:00Z",
      }),
    );

    expect(await within(region).findByText("It is live, but not tied to that client")).toBeTruthy();
    expect(within(region).getByText("account_not_found: no client with that id is one you can read")).toBeTruthy();
  });

  it("stops the rail where a refused run stopped, in the server's own words", async () => {
    const connection = fakeConnection({ sourceProbe: { "": probeReply() } });
    const { region } = await compose(connection);
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await fill(NAME_FIELD, "acme");
    await click(forwardAct("Analyze"));

    await emit(
      connection,
      DEPLOYMENT_CONCEPT,
      parkedRun(mintedPackageId(connection), {
        status: "refused",
        report: null,
        error: { code: "package_manifest_missing", message: "no memql-package.yaml at the root of acme/storefront" },
      }),
      "NODE_CREATED",
    );

    expect(await within(region).findByText("no memql-package.yaml at the root of acme/storefront")).toBeTruthy();
    // What it is is where a manifest refusal belongs, and every stop after it
    // is unreached.
    expect(railStates(region)).toEqual(["complete", "stopped", "pending", "pending", "pending"]);
    // ...and the one forward act is Retry, on the bar beside Cancel -- so
    // leaving a stopped flow is as reachable as trying it again.
    expect(forwardAct("Retry")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Closing and reopening
// ---------------------------------------------------------------------------

describe("the compose flow: leaving and coming back", () => {
  it("lands on the same rail, with the report in place", async () => {
    const ACME: Row = {
      id: "pkg-acme",
      ownerUserId: "u-me",
      name: "acme",
      sourceKind: "repo",
      repoUrl: REPO,
      repoRef: "main",
      credentialId: "",
      artifactId: "",
      deployedVersion: "",
      latestKnownVersion: "",
      updateAvailable: false,
      status: "active",
      createdAt: "2026-09-01T10:00:00Z",
    } as unknown as Row;

    // A window that was closed mid-compose: the run is parked, and the list's
    // "will serve" row is how somebody finds it again.
    mount(fakeConnection({ packages: [ACME], awaitingConfirm: [parkedRun("pkg-acme")], sites: [] }));
    await click(await screen.findByText("storefront"));

    const region = await screen.findByRole("region", { name: "New deployable" });
    expect(within(region).getByText("acme/storefront at main")).toBeTruthy();
    expect(within(region).getByText("clients/web")).toBeTruthy();
    expect(railStates(region)).toEqual(["complete", "complete", "open", "skipped", "pending"]);
    expect(forwardAct("Deploy")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// What the compose surface does not do
// ---------------------------------------------------------------------------

describe("what the compose flow does not do", () => {
  it("mounts no toast and no dialog, and keeps the pasted URL one click away", async () => {
    const connection = fakeConnection({ sourceProbe: { "": probeReply() } });
    const { region } = await compose(connection);
    await chooseSource(region, /A repository/);

    expect(document.querySelector("[data-toast], .os-toast, dialog, [role='dialog']")).toBeNull();
    // GitHub Connect (memql#4915) landed, so a person with no connection is
    // offered one -- and the pasted URL is still a legitimate first answer
    // rather than something behind "Advanced": one plain control, in the
    // surface, saying what it does.
    expect(within(region).getByRole("button", { name: "Connect GitHub" })).toBeTruthy();
    expect(within(region).queryByLabelText(URL_FIELD)).toBeNull();
    await click(within(region).getByRole("button", { name: "Use a token instead" }));
    expect(within(region).getByLabelText(URL_FIELD)).toBeTruthy();
  });

  it("reads no zip Library until the zip branch is showing", async () => {
    const connection = fakeConnection({ artifacts: [ZIP] });
    const { region } = await compose(connection);
    expect(connection.callsNamed("libraryArtifacts")).toHaveLength(0);

    await chooseSource(region, /A zip in Files/);
    await waitFor(() => expect(connection.callsNamed("libraryArtifacts")).toHaveLength(1));
  });
});

// ---------------------------------------------------------------------------
// A private repository, end to end
// ---------------------------------------------------------------------------

describe("a private repository whose build output is committed", () => {
  it("goes New deployable -> published, with its token pasted once", async () => {
    const secret = "github_pat_" + "11PRIVATE" + "0123456789";
    const connection = fakeConnection({
      sourceProbe: {
        "": probeReply({ reachable: false, reason: "not_found_or_private" }),
        "cred-new": probeReply({ private: true, defaultBranch: "main" }),
      },
      credentials: [],
    });
    const { region } = await compose(connection);

    // Source: a repository this cluster cannot see anonymously.
    await chooseRepository(region);
    await fill(URL_FIELD, REPO);
    await blur(URL_FIELD);
    await within(region).findByText("private, or not there");

    // ...and a token, pasted once.
    await choose(CREDENTIAL_FIELD, /Add a credential/);
    await fill("A name for this github.com credential", "work laptop");
    await fill("The github.com access token", secret);
    await click(within(region).getByRole("button", { name: "Add credential" }));
    await within(region).findByText(/private, and reachable under this credential/);

    // Analyze.
    await fill(NAME_FIELD, "acme");
    await click(forwardAct("Analyze"));
    expect(connection.callsNamed("createPackage")[0]).toContain('credentialId: "cred-new"');
    await emit(connection, DEPLOYMENT_CONCEPT, parkedRun(mintedPackageId(connection)), "NODE_CREATED");
    await within(region).findByText("clients/web");

    // Where it lives, then Deploy.
    await fill("The name storefront answers at", "shop");
    await click(forwardAct("Deploy"));

    await emit(
      connection,
      DEPLOYMENT_CONCEPT,
      parkedRun(mintedPackageId(connection), {
        status: "succeeded",
        deployables: [
          {
            name: "storefront",
            siteId: "site-shop",
            hostname: "shop.memql.example.com",
            bundleRef: "blob://sites/site-shop/v1/",
            version: "v1",
            created: true,
          },
        ],
        finishedAt: "2026-09-01T13:05:00Z",
      }),
    );

    expect(await within(region).findByText("Published. It is not serving yet.")).toBeTruthy();
    expect(railStates(region)).toEqual(["complete", "complete", "complete", "skipped", "complete"]);
    // The addresses are facts now, and the one that landed is the run's own.
    expect(within(region).queryByLabelText("The name storefront answers at")).toBeNull();
    expect(within(region).getAllByText("shop.memql.example.com").length).toBeGreaterThan(0);

    // THE WHOLE POINT: the token reached exactly one call.
    const carrying = connection.calls.filter((call) => call.includes(secret));
    expect(carrying).toHaveLength(1);
    expect(carrying[0]).toContain("sourceCredentialCreate");
  });
});
