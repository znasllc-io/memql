import { afterEach, describe, expect, it } from "vitest";
import { cleanup } from "@testing-library/react";

import {
  runCoversApp,
  type DeploymentRow,
} from "../../src/apps/deployables/packages/rows";
import { actsFor, runForApp, siblingRunInFlight } from "../../src/apps/deployables/page/acts";
import { DEFAULT_LIST_FILTER, foldDeployables } from "../../src/apps/deployables/list";
import type { SiteRow } from "../../src/apps/deployables/rows";
import type { PackageRow } from "../../src/apps/deployables/packages/rows";

// A RUN'S SCOPE, and what every reader of it does differently (memql#4953).
//
// A `v1:platform:packageDeployment` row recorded nothing about which
// deployables it was for, and runs are routinely scoped to one app: a redeploy
// from a deployable's page sends every sibling `skip: true`, and the compose
// gate does the same when entered for one declared app. So every reader
// assumed the source's newest run was about whatever it was looking at, and:
//
//   - `storefront`'s page read "Building" while `web` analyzed, hid Unpublish
//     and Redeploy, and offered a Cancel that killed `web`'s deploy;
//   - the standing rail drew a SERVING app's Where-it-lives, Build and Live as
//     `ahead`, in the list's compact rail too;
//   - the What-it-is stop and the refusal notice rendered the sibling's report
//     and error, so one app's build failure appeared as this app's.
//
// Every test below fails against that reading.

afterEach(cleanup);

function deployment(over: Partial<DeploymentRow> & { id: string }): DeploymentRow {
  return {
    packageId: "pkg-acme",
    sourceVersion: "abc1234",
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
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "",
    heartbeatAt: "",
    scopedTo: [],
    fromDeploymentId: "",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  };
}

describe("whose run is this", () => {
  it("a run naming no deployables is about all of them", () => {
    // EMPTY MEANS EVERY APP, which is what makes the field safe to add to a
    // live timeline: every row written before it has none, and those runs
    // WERE about the whole source.
    const whole = deployment({ id: "dep-1" });
    expect(runCoversApp(whole, "storefront")).toBe(true);
    expect(runCoversApp(whole, "web")).toBe(true);
  });

  it("a scoped run is about the apps it names, and no others", () => {
    const scoped = deployment({ id: "dep-1", scopedTo: ["web"] });
    expect(runCoversApp(scoped, "web")).toBe(true);
    expect(runCoversApp(scoped, "storefront")).toBe(false);
  });

  it("picks this app's newest run out of the source's timeline", () => {
    // The shape the page reads: one source, two apps, the sibling's run
    // newest. `rows[0]` is the wrong answer and was the one being used.
    const timeline = [
      deployment({ id: "dep-web", status: "building", scopedTo: ["web"] }),
      deployment({ id: "dep-store", scopedTo: ["storefront"] }),
    ];
    expect(runForApp(timeline, "storefront")?.id).toBe("dep-store");
    expect(runForApp(timeline, "web")?.id).toBe("dep-web");
  });

  it("finds a sibling's run only while it is in flight", () => {
    const moving = [deployment({ id: "dep-web", status: "building", scopedTo: ["web"] })];
    expect(siblingRunInFlight(moving, "storefront")?.id).toBe("dep-web");
    expect(siblingRunInFlight(moving, "web")).toBeNull();

    const finished = [deployment({ id: "dep-web", status: "succeeded", scopedTo: ["web"] })];
    expect(siblingRunInFlight(finished, "storefront")).toBeNull();
  });

  it("a sibling PARKED at the gate is not in flight", () => {
    // It is waiting for a person: nothing is executing and nothing holds the
    // source. Counting it would mean one unanswered gate froze every other app
    // of the source until somebody went looking for it.
    const parked = [deployment({ id: "dep-web", status: "awaiting_confirm", scopedTo: ["web"] })];
    expect(siblingRunInFlight(parked, "storefront")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The bar
// ---------------------------------------------------------------------------

const site: SiteRow = {
  id: "site-store",
  hostname: "store.memql.example.com",
  status: "live",
  kind: "spa",
  title: "Store",
  bundleRef: "blob://sites/site-store/v2/",
  systemOwned: false,
  ownerUserId: "u-me",
  accountId: "",
  packageId: "pkg-acme",
  packageDeployableName: "storefront",
  storeDomain: "",
  storefrontTokenRef: "",
  createdAt: "",
} as unknown as SiteRow;

const pkg: PackageRow = {
  id: "pkg-acme",
  name: "acme",
  status: "active",
  declares: [
    { name: "storefront", kind: "spa" },
    { name: "web", kind: "spa" },
  ],
  disabledDeployables: [],
  updateAvailable: false,
} as unknown as PackageRow;

describe("the bar while a sibling deploys", () => {
  const siblingBuilding = deployment({ id: "dep-web", status: "building", scopedTo: ["web"] });

  it("keeps this app's own state and words", () => {
    const reading = actsFor({ site, pkg, run: null, siblingRun: siblingBuilding, canWrite: true });
    // NOT "Building". A published deployable is still published while another
    // app of its source deploys, and saying otherwise was the defect.
    expect(reading.state).toBe("Published");
    expect(reading.tone).toBe("live");
  });

  it("withholds the acts that would start a second run, and says why", () => {
    const reading = actsFor({ site, pkg, run: null, siblingRun: siblingBuilding, canWrite: true });
    const names = reading.acts.map((a) => a.name);
    // THE PROTECTION THE WRONG READING WAS ACCIDENTALLY PROVIDING. There is no
    // per-source concurrency gate in the engine, and a roll rewrites one
    // pointer and restarts the cluster onto it -- so two runs of one source
    // racing there is not something to let anybody discover.
    expect(names).not.toContain("Redeploy");
    expect(names).not.toContain("Deploy the update");
    // ABSENT, NOT DISABLED (DESIGN.md rule 12).
    expect(reading.acts.every((a) => a.name !== "Redeploy")).toBe(true);
    // ...and the line that already explains the state explains this too.
    // SHORT, because .os-actbar-detail ellipsizes and a truncated explanation
    // of a missing button is worse than none. The state word and its live dot
    // already say the app is published.
    expect(reading.detail).toBe("waiting for web's deploy to finish");
  });

  it("keeps the acts that only change THIS site", () => {
    const reading = actsFor({ site, pkg, run: null, siblingRun: siblingBuilding, canWrite: true });
    // Unpublish touches this site's own status and no source pointer, so it
    // has no reason to wait for a deploy of a different app.
    expect(reading.acts.map((a) => a.name)).toContain("Unpublish");
  });

  it("offers everything again once the sibling's run ends", () => {
    // THE CONTROL. Without it "withholds the acts" would pass on a bar that
    // never offers them.
    const reading = actsFor({ site, pkg, run: null, siblingRun: null, canWrite: true });
    expect(reading.acts.map((a) => a.name)).toContain("Redeploy");
    expect(reading.detail).not.toContain("waiting for");
  });

  it("a run of this app's OWN still owns the bar", () => {
    // Scope narrows whose run a page reads; it does not stop a page reading
    // its own.
    const mine = deployment({ id: "dep-store", status: "building", scopedTo: ["storefront"] });
    const reading = actsFor({ site, pkg, run: mine, siblingRun: null, canWrite: true });
    expect(reading.state).toBe("Building");
    expect(reading.acts.map((a) => a.name)).toEqual(["Cancel"]);
  });
});

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

describe("the list's compact rails", () => {
  it("marks only the rows a parked run is about", () => {
    // Before this, `newestParkedRun` keyed on packageId alone and every row of
    // a source carried the same run -- so one app's gate marked every sibling
    // "a deploy is waiting for you" and repainted their compact rails as
    // unreached.
    const store = { ...site };
    const web = { ...site, id: "site-web", hostname: "web.memql.example.com", packageDeployableName: "web" } as SiteRow;
    const parked = deployment({
      id: "dep-parked",
      status: "awaiting_confirm",
      scopedTo: ["web"],
      report: { deployables: [{ name: "web", kind: "spa" }], dslDomains: [], problems: [], ok: true } as never,
    });

    const groups = foldDeployables([store, web], [pkg], [parked], DEFAULT_LIST_FILTER, false);
    const rows = groups.flatMap((g) => g.rows);

    const storeRow = rows.find((r) => r.app === "storefront");
    const webRow = rows.find((r) => r.app === "web");
    expect(storeRow?.parked).toBeNull();
    expect(webRow?.parked?.id).toBe("dep-parked");
  });

  it("a whole-source parked run still marks every row", () => {
    // THE CONTROL, and the compatibility case: a run with no scope -- which is
    // every run written before the field existed -- is about the whole source
    // and marks all of it.
    const store = { ...site };
    const web = { ...site, id: "site-web", hostname: "web.memql.example.com", packageDeployableName: "web" } as SiteRow;
    const parked = deployment({ id: "dep-parked", status: "awaiting_confirm" });

    const groups = foldDeployables([store, web], [pkg], [parked], DEFAULT_LIST_FILTER, false);
    const rows = groups.flatMap((g) => g.rows);
    // Both serving apps. (A third row is the source's own, raised because this
    // run's report names no deployables -- a parked run is never invisible.)
    expect(rows.find((r) => r.app === "storefront")?.parked?.id).toBe("dep-parked");
    expect(rows.find((r) => r.app === "web")?.parked?.id).toBe("dep-parked");
  });
});
