import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { ComposePage } from "../../src/apps/deployables/page/ComposePage";
import { HistoryView } from "../../src/apps/deployables/page/HistoryView";
import { DeployablePage } from "../../src/apps/deployables/page/DeployablePage";
import { deploymentFromRow as runFromRow, packageFromRow } from "../../src/apps/deployables/packages/rows";
import { placementsPayload } from "../../src/apps/deployables/packages/calls";
import {
  deploymentFromRow,
  publishedAsCandidate,
  publishedOnlyCandidates,
  recordedAsCandidate,
  type DeploymentRow,
  type PackageRow,
} from "../../src/apps/deployables/packages/rows";
import { actsFor, candidateBlockedReason, candidateTargetOffered } from "../../src/apps/deployables/page/acts";
import { ALL_PARTS, partsWithout, type PartsHeld } from "../../src/apps/deployables/parts";
import { publishedVersions } from "../../src/apps/deployables/preview/PreviewSection";
import { siteFromRow, type SiteRow } from "../../src/apps/deployables/rows";
import { click, fakeConnection, previewReadinessRow, siteRow, withSession, type FakeSeed } from "./harness";

// DEPLOYING A VERSION AS THE CANDIDATE (memql#5601).
//
// A package deploy's placement may say `target: "candidate"`: the build is
// published as the site's candidateRef, the live site keeps serving what it
// served, and the run's outcome records candidateRef INSTEAD of bundleRef. The
// OS offers it at the confirm step of a redeploy, for a live deployable that is
// not a storefront, to a person holding the preview part -- and afterwards says
// plainly that the live site did not change.

function deployment(over: Partial<DeploymentRow> & { id: string }): DeploymentRow {
  return {
    packageId: "pkg-acme",
    sourceVersion: "3f2a1b9c0d4e5f60718293a4b5c6d7e8f9012345",
    status: "succeeded",
    report: null,
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "",
    buildLogTail: "",
    builtOn: null,
    error: null,
    requestedBy: "u-me",
    automatic: false,
    nodeId: "bff-1",
    stoppedAt: "",
    startedAt: "2026-10-01T12:00:00Z",
    finishedAt: "2026-10-01T12:01:00Z",
    heartbeatAt: "",
    scopedTo: [],
    fromDeploymentId: "",
    createdAt: "2026-10-01T12:00:00Z",
    ...over,
  };
}

const WEB: SiteRow = siteFromRow(
  siteRow({
    id: "site-web",
    hostname: "web.memql.example.com",
    kind: "spa",
    status: "live",
    bundleRef: "blob://sites/site-web/v2/",
    packageId: "pkg-acme",
    packageDeployableName: "web",
  }),
);

const PKG = {
  id: "pkg-acme",
  name: "acme",
  status: "active",
  declares: [
    { name: "web", kind: "spa" },
    { name: "shop", kind: "shopify_storefront" },
  ],
  disabledDeployables: [],
  updateAvailable: true,
} as unknown as PackageRow;

/** A gate parked FOR this app -- the redeploy somebody is being asked to confirm. */
const PARKED = deployment({ id: "dep-parked", status: "awaiting_confirm", scopedTo: ["web"], finishedAt: "" });

describe("the run's outcome", () => {
  it("reads candidateRef off an outcome, beside an empty bundleRef", () => {
    const run = deploymentFromRow({
      id: "dep-candidate",
      status: "succeeded",
      deployables: [{ name: "web", siteId: "site-web", hostname: "web.memql.example.com", candidateRef: "blob://sites/site-web/v3/", version: "v3" }],
    });
    expect(run.deployables[0]?.candidateRef).toBe("blob://sites/site-web/v3/");
    expect(run.deployables[0]?.bundleRef).toBe("");
  });

  it("reads the deployables a run records as candidates, and none on an older run", () => {
    expect(deploymentFromRow({ id: "dep-c", status: "awaiting_confirm", candidates: ["web", 7, ""] }).candidates).toEqual(["web"]);
    expect(deploymentFromRow({ id: "dep-old", status: "awaiting_confirm" }).candidates).toEqual([]);
    expect(recordedAsCandidate(deploymentFromRow({ id: "dep-c", candidates: ["web"] }), "web")).toBe(true);
    expect(recordedAsCandidate(deploymentFromRow({ id: "dep-c", candidates: ["web"] }), "shop")).toBe(false);
  });

  it("normalizes candidateRef at the read boundary, as it does bundleRef", () => {
    // The engine omits an unset outcome field (`omitempty`), and every run
    // written before memql#5601 has none, so absent must read as "" -- the
    // value a reader compares -- rather than leaking through the spread as
    // undefined. A non-string is a malformed row and reads as "" too.
    const run = deploymentFromRow({
      id: "dep-old",
      status: "succeeded",
      deployables: [
        { name: "web", siteId: "site-web", bundleRef: "blob://sites/site-web/v2/" },
        { name: "admin", siteId: "site-admin", candidateRef: 7 },
      ],
    });
    expect(run.deployables.map((o) => o.candidateRef)).toEqual(["", ""]);
  });

  it("knows a run that published only candidates, and which app it published as one", () => {
    const candidate = deployment({
      id: "dep-candidate",
      deployables: [{ name: "web", siteId: "site-web", hostname: "web.memql.example.com", bundleRef: "", candidateRef: "blob://sites/site-web/v3/", version: "v3", created: false }],
    });
    expect(publishedOnlyCandidates(candidate)).toBe(true);
    expect(publishedAsCandidate(candidate, "web")).toBe(true);
    expect(publishedAsCandidate(candidate, "shop")).toBe(false);

    const serving = deployment({
      id: "dep-serving",
      deployables: [{ name: "web", siteId: "site-web", hostname: "web.memql.example.com", bundleRef: "blob://sites/site-web/v3/", version: "v3", created: false }],
    });
    expect(publishedOnlyCandidates(serving)).toBe(false);
    expect(publishedAsCandidate(serving, "web")).toBe(false);
    // A run that published nothing at all is not a candidate run either: the
    // engine's own reading (component/packages/candidate.go candidatesOnly).
    expect(publishedOnlyCandidates(deployment({ id: "dep-empty" }))).toBe(false);
  });
});

describe("the placement on the wire", () => {
  it("sends the target that was chosen, serving included, and nothing when none was", () => {
    // At the confirm an explicit target wins and an omitted one reuses what the
    // run recorded when it opened -- so "the live version" must be said, not
    // left to a default somebody else's open may have replaced.
    expect(
      placementsPayload({
        web: { hostname: "", accountId: "", ownDomain: "", target: "candidate" },
        shop: { hostname: "", accountId: "", ownDomain: "", skip: true },
      }),
    ).toEqual({ web: { target: "candidate" }, shop: { skip: true } });
    expect(placementsPayload({ web: { hostname: "", accountId: "", ownDomain: "", target: "serving" } })).toEqual({
      web: { target: "serving" },
    });
    expect(placementsPayload({ web: { hostname: "", accountId: "", ownDomain: "" } })).toEqual({});
  });
});

describe("the versions a candidate can be chosen from", () => {
  it("offers a version this source deployed as a candidate, beside the ones it served", () => {
    // A candidate deploy records candidateRef and never bundleRef, so a picker
    // reading bundleRef alone could never offer the version back once it was
    // withdrawn -- the one thing somebody exercising it would want to do.
    const runs = [
      deployment({
        id: "dep-3",
        deployables: [{ name: "web", siteId: "site-web", hostname: "", bundleRef: "", candidateRef: "blob://sites/site-web/v3/", version: "v3", created: false }],
      }),
      deployment({
        id: "dep-1",
        deployables: [{ name: "web", siteId: "site-web", hostname: "", bundleRef: "blob://sites/site-web/v1/", version: "v1", created: false }],
      }),
    ];
    expect(publishedVersions(runs, WEB)).toEqual(["blob://sites/site-web/v3/", "blob://sites/site-web/v1/"]);
  });
});

describe("when the choice is offered", () => {
  it("is offered for a live deployable's own gate, to a person holding deploy and preview", () => {
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: PARKED, can: ALL_PARTS })).toBe(true);
  });

  it("is not offered for a storefront, whose Testing and Production serve one build", () => {
    const shop = { ...WEB, kind: "shopify_storefront" };
    expect(candidateTargetOffered({ site: shop, pkg: PKG, run: PARKED, can: ALL_PARTS })).toBe(false);
  });

  it("is not offered without the preview part, which the engine rechecks for a candidate", () => {
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: PARKED, can: partsWithout("preview") })).toBe(false);
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: PARKED, can: partsWithout("deploy") })).toBe(false);
  });

  it("is not offered where nothing is live to keep serving", () => {
    for (const status of ["draft", "disabled", "archived"] as const) {
      expect(candidateTargetOffered({ site: { ...WEB, status }, pkg: PKG, run: PARKED, can: ALL_PARTS })).toBe(false);
    }
  });

  it("is not offered for a gate that is not this deployable's, nor with no gate at all", () => {
    const wholeSource = { ...PARKED, scopedTo: [] };
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: wholeSource, can: ALL_PARTS })).toBe(false);
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: deployment({ id: "dep-ok" }), can: ALL_PARTS })).toBe(false);
    expect(candidateTargetOffered({ site: WEB, pkg: PKG, run: null, can: ALL_PARTS })).toBe(false);
  });

  it("is not offered on a system-owned row", () => {
    expect(candidateTargetOffered({ site: { ...WEB, systemOwned: true }, pkg: PKG, run: PARKED, can: ALL_PARTS })).toBe(false);
  });
});

describe("the bar at the gate", () => {
  it("names the act after the choice: Deploy by default, Deploy as candidate when chosen", () => {
    const names = (target?: "serving" | "candidate") =>
      actsFor({ site: WEB, pkg: PKG, run: PARKED, can: ALL_PARTS, deployTarget: target }).acts.map((a) => a.name);
    expect(names()).toEqual(["Cancel", "Deploy"]);
    expect(names("serving")).toEqual(["Cancel", "Deploy"]);
    expect(names("candidate")).toEqual(["Cancel", "Deploy as candidate"]);
  });

  it("keeps Deploy when the version ships MemQL, which no candidate can keep from the live site", () => {
    // A package's MemQL is staged and rolled for the whole cluster whatever the
    // app's target, so the engine refuses a candidate for a plan that carries
    // it; the choice says why rather than offering a deploy that would be
    // refused.
    const withDsl = {
      ...PARKED,
      report: { dslDomains: [{ domain: "billing", constructs: { query: 3 }, files: 2 }], deployables: [], problems: [], ok: true },
    };
    // NOT KNOWN WHETHER IT CHANGES (no dslChanges on the report): the
    // conservative reading, because the engine refuses a change.
    expect(candidateBlockedReason(withDsl)).toMatch(/MemQL \(billing\)/);
    expect(candidateBlockedReason(withDsl)).toMatch(/any change to it takes effect for the whole cluster/);
    expect(actsFor({ site: WEB, pkg: PKG, run: withDsl, can: ALL_PARTS, deployTarget: "candidate" }).acts.map((a) => a.name)).toEqual([
      "Cancel",
      "Deploy",
    ]);
    // A report with no MemQL, or none at all, blocks nothing.
    expect(candidateBlockedReason(PARKED)).toBe("");
    expect(candidateBlockedReason({ ...PARKED, report: { dslDomains: [], deployables: [], problems: [], ok: true } })).toBe("");
  });

  it("asks the report whether the MemQL CHANGES, when it says", () => {
    // The engine refuses a candidate only when the plan would change the
    // active MemQL set, and a product bundle usually ships its MemQL
    // unchanged -- so "ships" alone would hide the choice in the common case.
    const ships = { dslDomains: [{ domain: "billing", constructs: { query: 3 }, files: 2 }], deployables: [], problems: [], ok: true };
    const unchanged = { ...PARKED, report: { ...ships, dslChanges: false } };
    const changed = { ...PARKED, report: { ...ships, dslChanges: true } };
    expect(candidateBlockedReason(unchanged)).toBe("");
    expect(candidateBlockedReason(changed)).toMatch(/it changes the MemQL this cluster runs/);
    expect(actsFor({ site: WEB, pkg: PKG, run: unchanged, can: ALL_PARTS, deployTarget: "candidate" }).acts.map((a) => a.name)).toEqual([
      "Cancel",
      "Deploy as candidate",
    ]);
    expect(actsFor({ site: WEB, pkg: PKG, run: changed, can: ALL_PARTS, deployTarget: "candidate" }).acts.map((a) => a.name)).toEqual([
      "Cancel",
      "Deploy",
    ]);
  });

  it("ignores a candidate choice where it is not offered", () => {
    const shop = { ...WEB, kind: "shopify_storefront" };
    expect(actsFor({ site: shop, pkg: PKG, run: PARKED, can: ALL_PARTS, deployTarget: "candidate" }).acts.map((a) => a.name)).toEqual([
      "Cancel",
      "Deploy",
    ]);
  });
});

// ---------------------------------------------------------------------------
// Rendered: the gate on a deployable's page, and what the result says
// ---------------------------------------------------------------------------

const ACME_ROW: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/web",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "1111111111111111111111111111111111111111",
  latestKnownVersion: "3333333333333333333333333333333333333333",
  updateAvailable: true,
  status: "active",
  declares: [{ name: "web", kind: "spa" }],
  createdAt: "2026-09-01T10:00:00Z",
};

const WEB_ROW: Row = siteRow({
  id: "site-web",
  hostname: "web.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-web/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "web",
});

const REPORT = {
  name: "acme",
  formatVersion: 1,
  deployables: [{ name: "web", kind: "spa", path: "web", buildPlan: "already built: dist", output: "dist", prebuilt: true }],
  dslDomains: [],
  problems: [],
  ok: true,
};

function runRow(over: Record<string, unknown> & { id: string }): Row {
  return {
    packageId: "pkg-acme",
    sourceVersion: "2222222222222222222222222222222222222222",
    status: "succeeded",
    report: REPORT,
    dslVersion: "",
    deployables: [],
    snapshotArtifactId: "",
    buildLogTail: "",
    error: null,
    requestedBy: "u-me",
    scopedTo: ["web"],
    startedAt: "2026-10-01T12:00:00Z",
    finishedAt: "2026-10-01T12:01:00Z",
    createdAt: "2026-10-01T12:00:00Z",
    ...over,
  };
}

const outcome = (over: Record<string, unknown>) => ({ name: "web", siteId: "site-web", hostname: "web.memql.example.com", version: "v", created: false, ...over });

const SERVED_V1 = runRow({ id: "dep-v1", sourceVersion: "1111111111111111111111111111111111111111", startedAt: "2026-09-30T12:00:00Z", createdAt: "2026-09-30T12:00:00Z", deployables: [outcome({ bundleRef: "blob://sites/site-web/v1/" })] });
const SERVED_V2 = runRow({ id: "dep-v2", sourceVersion: "2222222222222222222222222222222222222222", deployables: [outcome({ bundleRef: "blob://sites/site-web/v2/" })] });
const CANDIDATE_V3 = runRow({ id: "dep-v3", sourceVersion: "3333333333333333333333333333333333333333", startedAt: "2026-10-02T12:00:00Z", createdAt: "2026-10-02T12:00:00Z", deployables: [outcome({ candidateRef: "blob://sites/site-web/v3/" })] });
const GATE = runRow({ id: "dep-parked", status: "awaiting_confirm", sourceVersion: "3333333333333333333333333333333333333333", startedAt: "2026-10-02T12:00:00Z", createdAt: "2026-10-02T12:00:00Z", finishedAt: "" });

/** The readiness a spa answers: not a storefront, nothing being exercised. */
const SPA_READINESS = previewReadinessRow({
  siteId: "site-web",
  hostname: "web.memql.example.com",
  storefront: false,
  bundleRef: "blob://sites/site-web/v2/",
  canPreview: false,
  previewRefusal: { code: "", message: "", remedy: "" },
} as never);

function openPage(seed: FakeSeed, opts: { site?: Row; can?: PartsHeld } = {}) {
  const row = opts.site ?? WEB_ROW;
  const connection = fakeConnection({
    sites: [row],
    packages: [ACME_ROW],
    previewReadiness: { "site-web": SPA_READINESS },
    ...seed,
  });
  h.connection = connection;
  render(
    withSession(
      <DeployablePage
        site={siteFromRow(row)}
        pkg={packageFromRow(ACME_ROW)}
        credentials={[]}
        viewerUserId="u-me"
        nameOf={() => ""}
        can={opts.can ?? ALL_PARTS}
        clusterDomain="memql.example.com"
        onBack={vi.fn()}
        onOpenSource={vi.fn()}
      />,
    ),
  );
  return connection;
}

function barActs(): string[] {
  const bar = document.querySelector(".os-actbar") as HTMLElement | null;
  if (bar === null) return [];
  return within(bar)
    .queryAllByRole("button")
    .map((b) => (b.textContent ?? "").trim())
    .filter((t) => t !== "");
}

function barAct(name: string): HTMLButtonElement {
  return within(document.querySelector(".os-actbar") as HTMLElement).getByRole("button", { name: new RegExp(`^${name}`) }) as HTMLButtonElement;
}

async function atTheGate(): Promise<void> {
  await waitFor(() => expect(document.querySelector(".os-actbar-word")?.textContent).toBe("Ready to deploy"));
}

function option(name: "Live version" | "Candidate"): HTMLElement {
  const group = screen.getByRole("radiogroup", { name: "Deploy this version as" });
  return within(group).getByText(name).closest('[role="radio"]') as HTMLElement;
}

afterEach(() => {
  h.connection = null;
});

describe("the gate on a live deployable's page", () => {
  it("asks where the version goes, with the live version chosen, and says what each choice does", async () => {
    openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] } });
    await atTheGate();
    expect(option("Live version").getAttribute("aria-checked")).toBe("true");
    expect(option("Candidate").getAttribute("aria-checked")).toBe("false");
    expect(option("Live version").textContent).toContain("Visitors to web.memql.example.com get it as soon as it is in place.");
    expect(option("Candidate").textContent).toContain("web.memql.example.com keeps serving its current version.");
    expect(barActs()).toEqual(["Cancel", "Deploy"]);
  });

  it("confirms the live version by NAMING it", async () => {
    const connection = openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] } });
    await atTheGate();
    await click(barAct("Deploy"));
    expect(connection.callsNamed("packageDeploy")).toEqual([
      'builtin packageDeploy(packageId: "pkg-acme", confirm: true, placements: {web: {target: "serving"}}, deploymentId: "dep-parked")',
    ]);
  });

  it("confirms the candidate when it is chosen, and the act says so first", async () => {
    const connection = openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] } });
    await atTheGate();
    await click(option("Candidate"));
    expect(option("Candidate").getAttribute("aria-checked")).toBe("true");
    expect(barActs()).toEqual(["Cancel", "Deploy as candidate"]);
    await click(barAct("Deploy as candidate"));
    expect(connection.callsNamed("packageDeploy")).toEqual([
      'builtin packageDeploy(packageId: "pkg-acme", confirm: true, placements: {web: {target: "candidate"}}, deploymentId: "dep-parked")',
    ]);
  });

  it("is not asked for a storefront, whose two websites serve one build", async () => {
    const shop = siteRow({ ...WEB_ROW, id: "site-web", kind: "shopify_storefront" } as never);
    openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] } }, { site: shop });
    await atTheGate();
    expect(screen.queryByRole("radiogroup", { name: "Deploy this version as" })).toBeNull();
    expect(barActs()).toEqual(["Cancel", "Deploy"]);
  });

  it("is not asked of a person without the preview part", async () => {
    openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] } }, { can: partsWithout("preview") });
    await atTheGate();
    expect(screen.queryByRole("radiogroup", { name: "Deploy this version as" })).toBeNull();
    expect(barActs()).toEqual(["Cancel", "Deploy"]);
  });

  it("opens on the choice the run recorded, so a run opened as a candidate shows as one", async () => {
    const opened = runRow({ ...GATE, id: "dep-parked", candidates: ["web"] });
    const connection = openPage({ deployments: { "pkg-acme": [opened, SERVED_V2] } });
    await atTheGate();
    expect(option("Candidate").getAttribute("aria-checked")).toBe("true");
    expect(barActs()).toEqual(["Cancel", "Deploy as candidate"]);
    // ...and the person can still choose otherwise, which the confirm says.
    await click(option("Live version"));
    await click(barAct("Deploy"));
    expect(connection.callsNamed("packageDeploy")).toEqual([
      'builtin packageDeploy(packageId: "pkg-acme", confirm: true, placements: {web: {target: "serving"}}, deploymentId: "dep-parked")',
    ]);
  });

  it("offers the candidate for a version whose MemQL does not change what the cluster runs", async () => {
    const unchanged = runRow({ ...GATE, id: "dep-parked", report: { ...REPORT, dslDomains: [{ domain: "billing", constructs: { query: 2 }, files: 1 }], dslChanges: false } });
    openPage({ deployments: { "pkg-acme": [unchanged, SERVED_V2] } });
    await atTheGate();
    expect(option("Candidate").getAttribute("aria-disabled")).toBeNull();
    await click(option("Candidate"));
    expect(barActs()).toEqual(["Cancel", "Deploy as candidate"]);
  });

  it("shows the candidate as unavailable, with why, for a version that changes the cluster's MemQL", async () => {
    const changes = runRow({ ...GATE, id: "dep-parked", report: { ...REPORT, dslDomains: [{ domain: "billing", constructs: { query: 2 }, files: 1 }], dslChanges: true } });
    openPage({ deployments: { "pkg-acme": [changes, SERVED_V2] } });
    await atTheGate();
    expect(option("Candidate").getAttribute("aria-disabled")).toBe("true");
    expect(option("Candidate").textContent).toContain("it changes the MemQL this cluster runs");
  });

  it("shows the candidate as unavailable, with why, for a version that ships MemQL nobody has compared", async () => {
    const withDsl = runRow({ ...GATE, id: "dep-parked", report: { ...REPORT, dslDomains: [{ domain: "billing", constructs: { query: 2 }, files: 1 }] } });
    openPage({ deployments: { "pkg-acme": [withDsl, SERVED_V2] } });
    await atTheGate();
    expect(option("Candidate").getAttribute("aria-disabled")).toBe("true");
    expect(option("Candidate").textContent).toContain("it includes MemQL (billing), and any change to it takes effect for the whole cluster");
    await click(option("Candidate"));
    expect(option("Live version").getAttribute("aria-checked")).toBe("true");
    expect(barActs()).toEqual(["Cancel", "Deploy"]);
  });

  it("renders a refused candidate confirm beside the choice, while the gate is still open", async () => {
    const refusal = "candidate_carries_dsl: this package ships MemQL (billing), so deploying any app as a candidate would roll it for everyone -- deploy it as the live version, or ship the MemQL on its own first";
    openPage({ deployments: { "pkg-acme": [GATE, SERVED_V2] }, deployError: refusal });
    await atTheGate();
    await click(option("Candidate"));
    await click(barAct("Deploy as candidate"));
    const choice = document.querySelector(".deployable-deploy-target") as HTMLElement;
    await waitFor(() => expect(choice.textContent).toContain("deploy it as the live version, or ship the MemQL on its own first"));
    // ONCE, and where the question is -- not again at the top of the page.
    expect(screen.getAllByText(/deploy it as the live version, or ship the MemQL on its own first/)).toHaveLength(1);

    // Taking the remedy answers it: the refusal was about a question no longer
    // being asked.
    await click(option("Live version"));
    expect(screen.queryByText(/deploy it as the live version, or ship the MemQL on its own first/)).toBeNull();
    expect(barActs()).toEqual(["Cancel", "Deploy"]);
  });
});

describe("after a version went out as the candidate", () => {
  const WITH_CANDIDATE = siteRow({ ...WEB_ROW, id: "site-web", candidateRef: "blob://sites/site-web/v3/" } as never);

  it("says so on the latest attempt, and that the live site did not change", async () => {
    openPage({ deployments: { "pkg-acme": [CANDIDATE_V3, SERVED_V2] } }, { site: WITH_CANDIDATE });
    await waitFor(() => expect(screen.getByText("Deployed as candidate")).toBeTruthy());
    expect(screen.getByText(/3333333 · the live site is unchanged/)).toBeTruthy();
  });

  it("lists the candidate among the versions, and does not offer to roll back to what never served", async () => {
    openPage({ deployments: { "pkg-acme": [CANDIDATE_V3, SERVED_V2, SERVED_V1] } }, { site: WITH_CANDIDATE });
    await click(await screen.findByRole("button", { name: "Version v3, candidate" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Deployed as the candidate and not served yet. Promote the candidate to serve it.")).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: /Roll .* back/ })).toBeNull();
    await click(within(dialog).getByRole("button", { name: /^Close/ }));
    // A version that DID serve keeps its roll back.
    await click(await screen.findByRole("button", { name: "Version v1, available" }));
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Roll web.memql.example.com back to v1" })).toBeTruthy();
  });
});

describe("the source's history", () => {
  function openHistory(runs: Row[]) {
    const connection = fakeConnection({ sites: [WEB_ROW], packages: [ACME_ROW], deployments: { "pkg-acme": runs } });
    h.connection = connection;
    render(withSession(<HistoryView pkg={packageFromRow(ACME_ROW)} can={ALL_PARTS} onBack={vi.fn()} />));
  }

  function attempt(version: string): HTMLElement {
    return screen.getByText(version, { selector: ".os-attempt-version" }).closest("article") as HTMLElement;
  }

  it("reads a candidate run as a candidate, never as live", async () => {
    openHistory([CANDIDATE_V3, SERVED_V2, SERVED_V1]);
    await waitFor(() => expect(screen.getByText("3333333", { selector: ".os-attempt-version" })).toBeTruthy());
    expect(attempt("3333333").querySelector(".os-attempt-status")?.textContent).toBe("candidate");
    expect(attempt("2222222").querySelector(".os-attempt-status")?.textContent).toBe("live");
    expect(within(attempt("3333333")).getByText("candidate", { selector: ".os-chip" })).toBeTruthy();
  });

  it("offers no roll back to a candidate run, nor to the run that is still serving behind it", async () => {
    openHistory([CANDIDATE_V3, SERVED_V2, SERVED_V1]);
    await waitFor(() => expect(screen.getByText("3333333", { selector: ".os-attempt-version" })).toBeTruthy());
    expect(within(attempt("3333333")).queryByRole("button", { name: /Roll back/ })).toBeNull();
    // v2 still serves -- the candidate changed nothing -- so rolling back to it
    // would re-point nothing either.
    expect(within(attempt("2222222")).queryByRole("button", { name: /Roll back/ })).toBeNull();
    expect(within(attempt("1111111")).getByRole("button", { name: "Roll back to 1111111" })).toBeTruthy();
  });
});

describe("the compose flow's confirm", () => {
  it("names a target for every app it publishes, the ones it does not show included", async () => {
    // A WHOLE-SOURCE GATE, reviewed through compose: `storefront` is new and
    // asked where it lives; `web` already serves and is not shown. Omitting a
    // target keeps what the run recorded when it opened, so a confirm that
    // named none could publish `web` as a candidate with nothing on screen
    // saying so. Compose never offers a candidate, so it says "serving".
    const two = { ...REPORT, deployables: [{ ...REPORT.deployables[0]!, name: "storefront", path: "storefront" }, REPORT.deployables[0]!] };
    const source = { ...ACME_ROW, declares: [{ name: "storefront", kind: "spa" }, { name: "web", kind: "spa" }] };
    const parked = runRow({ id: "dep-whole", status: "awaiting_confirm", scopedTo: [], candidates: ["web"], report: two, finishedAt: "" });
    const connection = fakeConnection({ sites: [WEB_ROW], packages: [source], deployments: { "pkg-acme": [parked] } });
    h.connection = connection;
    const pkg = packageFromRow(source);
    render(
      withSession(
        <ComposePage
          clusterDomain="memql.example.com"
          can={ALL_PARTS}
          isClusterOwner
          viewerUserId="u-me"
          credentials={[]}
          source={pkg}
          parked={{ pkg, run: runFromRow(parked) }}
          placed={["web"]}
          siteFeed={{ state: "live", error: "" }}
          placedSources={[{ packageId: "pkg-acme", name: "web", siteId: "site-web" }]}
          onBack={vi.fn()}
        />,
      ),
    );
    const act = async (name: string) => {
      await waitFor(() => expect([...document.querySelectorAll(".os-actbar-acts button")].some((b) => b.textContent?.trim() === name)).toBe(true));
      await click([...document.querySelectorAll<HTMLButtonElement>(".os-actbar-acts button")].find((b) => b.textContent?.trim() === name));
    };
    await act("Choose addresses");
    const field = (await screen.findByLabelText("The name storefront answers at")) as HTMLInputElement;
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(field, "shop");
    field.dispatchEvent(new Event("input", { bubbles: true }));
    await act("Deploy");
    const confirm = connection.callsNamed("packageDeploy").find((c) => c.includes("confirm: true")) ?? "";
    expect(confirm).toContain('storefront: {accountId: "self", domains: [], hostname: "shop.memql.example.com", target: "serving"}');
    expect(confirm).toContain('web: {target: "serving"}');
    expect(confirm).toContain('deploymentId: "dep-whole"');
  });
});
