import { describe, expect, it } from "vitest";

import { actsFor, runIsCancellable, runIsMoving, type ActsInput } from "../../src/apps/deployables/page/acts";
import { openStopFor } from "../../src/apps/deployables/page/rail";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { deploymentFromRow, packageFromRow } from "../../src/apps/deployables/packages/rows";

// acts.test.ts -- the two pure readings epic memql#4937 turns on.
//
// `actsFor` is the whole of DESIGN.md rule 12 for this app, and `openStopFor`
// is the whole of "a settled stop is one line, and exactly one is open". Both
// are asserted here with NO DOM: what they SAY is the design, and a render
// test of either would assert it through three layers that can each fail for
// unrelated reasons.
//
// THE NEGATIVE CASES CARRY MORE THAN THE POSITIVE ONES. A draft yielding no
// Archive is the bug this epic was reported for; a system-owned row yielding
// no acts AT ALL rather than disabled ones is the rule the whole bar rests on.

function site(over: Record<string, unknown> = {}) {
  return siteFromRow({
    id: "site-1",
    hostname: "shop.example.com",
    kind: "spa",
    status: "live",
    bundleRef: "blob://sites/site-1/v1/",
    ownerUserId: "u-me",
    ...over,
  });
}

function pkg(over: Record<string, unknown> = {}) {
  return packageFromRow({
    id: "pkg-1",
    name: "acme",
    sourceKind: "repo",
    repoUrl: "https://github.com/acme/storefront",
    repoRef: "main",
    status: "active",
    ...over,
  });
}

function run(status: string, over: Record<string, unknown> = {}) {
  return deploymentFromRow({ id: "dep-1", packageId: "pkg-1", status, ...over });
}

const BASE: ActsInput = { site: site(), pkg: null, run: null, canWrite: true };
const names = (input: Partial<ActsInput>) => actsFor({ ...BASE, ...input }).acts.map((a) => a.name);

describe("actsFor -- acts follow the state, in one place", () => {
  it("live offers Take offline and the forward act", () => {
    expect(names({})).toEqual(["Take offline", "Deploy"]);
    // A SOURCE-BACKED deployable with nothing newer REDEPLOYS. Naming an
    // update that does not exist would be a claim about the upstream.
    expect(names({ pkg: pkg() })).toEqual(["Take offline", "Redeploy"]);
    expect(names({ pkg: pkg({ updateAvailable: true }) })).toEqual(["Take offline", "Deploy the update"]);
  });

  it("offline offers Archive and Go live, and says what 503 means", () => {
    const reading = actsFor({ ...BASE, site: site({ status: "disabled" }) });
    expect(reading.acts.map((a) => a.name)).toEqual(["Archive", "Go live"]);
    expect(reading.state).toBe("Offline");
    // The engine's own distinction, kept where the state word stopped
    // carrying it: a deliberate pause answers 503, an archive answers 404.
    expect(reading.detail).toContain("temporarily unavailable");
  });

  it("archived offers Delete -- the rung that releases the name", () => {
    const reading = actsFor({ ...BASE, site: site({ status: "archived" }) });
    expect(reading.acts.map((a) => a.name)).toEqual(["Delete", "Restore"]);
    // ...and it says the thing that made this epic necessary: archiving does
    // NOT free the hostname. The uniqueness probe reads `deleted` and never
    // `status`, so an archived name is held until something deletes it.
    expect(reading.detail).toContain("the name is still held");
  });

  // ---- THE BUG THIS EPIC WAS REPORTED FOR ----------------------------------

  it("A DRAFT OFFERS NO ARCHIVE, and offers Discard instead", () => {
    // Before this, a draft rendered an ENABLED "Archive this deployable" that
    // validateSiteStatusTransition refuses -- archiving admits `disabled`
    // alone -- and the draft had no control anywhere that could reach
    // `disabled`. The only lifecycle control it offered was one the engine
    // rejected. This is the assertion that makes that unrepresentable.
    const acts = names({ site: site({ status: "draft" }) });
    expect(acts).not.toContain("Archive");
    expect(acts).toContain("Discard");
  });

  it("a draft holding a placeholder deploys; one holding real bytes goes live", () => {
    // The BUNDLE decides, not what produced it. A draft already holding bytes
    // is one status write from serving; one holding the placeholder has
    // nothing to serve yet.
    expect(names({ site: site({ status: "draft", bundleRef: "blob://sites/site-1/pending/" }) })).toEqual([
      "Discard",
      "Deploy",
    ]);
    expect(names({ site: site({ status: "draft" }) })).toEqual(["Discard", "Go live"]);
  });

  it("A SYSTEM-OWNED ROW OFFERS NOTHING AT ALL -- not disabled controls", () => {
    const reading = actsFor({ ...BASE, site: site({ systemOwned: true }) });
    expect(reading.acts).toEqual([]);
    // ...and it says why, rather than leaving an absence with no account of
    // itself. Six greyed-out buttons are six controls to read past.
    expect(reading.detail).toContain("re-seeded live at every boot");
  });

  it("a reader sees the state and no acts", () => {
    for (const status of ["live", "disabled", "archived", "draft"]) {
      expect(names({ site: site({ status }), canWrite: false })).toEqual([]);
    }
    expect(actsFor({ ...BASE, canWrite: false }).state).toBe("Live");
  });

  // ---- the run in flight ---------------------------------------------------

  it("a parked run offers Cancel and Deploy -- on a deployable that has not served", () => {
    // The gate is the reading only where there is no truer one. On a LIVE
    // deployable it is not: see "a source's gate, on a deployable that has its
    // own state" below, which is the bug this qualification fixes.
    const fresh = site({ status: "draft", bundleRef: "" });
    const reading = actsFor({ ...BASE, site: fresh, pkg: pkg(), run: run("awaiting_confirm") });
    expect(reading.acts.map((a) => a.name)).toEqual(["Cancel", "Deploy"]);
    expect(reading.state).toBe("Ready to deploy");
  });

  it("a run in flight offers Cancel until the roll, and NOTHING after it", () => {
    for (const status of ["analyzing", "building"]) {
      expect(names({ pkg: pkg(), run: run(status) })).toEqual(["Cancel"]);
    }
    // FROM THE ROLL ON THERE IS NO CANCEL. A roll restarts the cluster onto
    // the staged MemQL, and stopping half way through is the one outcome
    // worse than either finishing or not starting -- so the bar says so
    // rather than offering a control the engine refuses.
    for (const status of ["staging_dsl", "rolling", "publishing"]) {
      const reading = actsFor({ ...BASE, pkg: pkg(), run: run(status) });
      expect(reading.acts).toEqual([]);
      expect(reading.detail).toContain("past the point where stopping is safe");
    }
  });

  it("a live deployable whose last run broke says Retry, not 'the update'", () => {
    // There is no update -- there is an attempt that did not land, and the
    // deployable is still serving the version before it.
    for (const status of ["refused", "failed"]) {
      const reading = actsFor({ ...BASE, pkg: pkg(), run: run(status) });
      expect(reading.acts.map((a) => a.name)).toEqual(["Take offline", "Retry the deploy"]);
      expect(reading.detail).toContain("did not finish");
    }
  });

  it("deleting is a state with no acts, because the teardown is asynchronous", () => {
    const reading = actsFor({ ...BASE, deleting: true, releasing: "www.acme.com" });
    expect(reading.acts).toEqual([]);
    expect(reading.state).toBe("Deleting");
    expect(reading.detail).toContain("www.acme.com");
  });

  it("a cancelled run is terminal, so the deployable's own state resumes", () => {
    // `cancelled` is NOT a flavour of failed: nothing broke and nothing was
    // published, so the bar goes back to describing the deployable rather
    // than the run.
    expect(runIsMoving(run("cancelled"))).toBe(false);
    expect(runIsCancellable(run("cancelled"))).toBe(false);
    expect(names({ pkg: pkg(), run: run("cancelled") })).toEqual(["Take offline", "Redeploy"]);
  });

  // ---- two ladders (2026-09-05 design, D2 and D4) ------------------------

  it("A SOURCE'S APP IS NEVER ARCHIVED: it deactivates, from every state", () => {
    // A standalone's bundle is its only copy, so it climbs Offline ->
    // Archived -> Deleted. A source's app is reproducible from its source, so
    // its one destructive act is Deactivate -- and it is offered from every
    // state, live included, because the typed confirmation is the step.
    const app = site({ packageId: "pkg-1", packageDeployableName: "storefront" });
    expect(names({ site: app, pkg: pkg() })).toEqual(["Take offline", "Redeploy"]);
    expect(names({ site: { ...app, status: "disabled" }, pkg: pkg() })).toEqual(["Deactivate", "Go live"]);
    expect(names({ site: { ...app, status: "draft" }, pkg: pkg() })).toEqual(["Deactivate", "Go live"]);
    expect(names({ site: { ...app, status: "draft", bundleRef: "" }, pkg: pkg() })).toEqual(["Deactivate", "Deploy"]);
    // An app the OLD cascade left archived still holds its name; Deactivate
    // is what frees it.
    expect(names({ site: { ...app, status: "archived" }, pkg: pkg() })).toEqual(["Deactivate", "Restore"]);
    for (const status of ["live", "disabled", "draft", "archived"] as const) {
      const offered = names({ site: { ...app, status }, pkg: pkg() });
      for (const never of ["Archive", "Delete", "Discard"]) expect(offered).not.toContain(never);
    }
  });

  it("decides the ladder from the ROW, so an app of an archived source still deactivates", () => {
    // An archived source is not in the active feed, so `pkg` is null for its
    // apps -- and those are exactly the rows whose names still need freeing.
    const orphan = site({ status: "archived", packageId: "pkg-gone", packageDeployableName: "storefront" });
    expect(names({ site: orphan, pkg: null })).toEqual(["Deactivate", "Restore"]);
  });

  it("reads the state word and its clause from the one vocabulary", () => {
    expect(actsFor({ ...BASE }).detail).toBe("serving at shop.example.com");
    const built = actsFor({ ...BASE, site: site({ status: "draft" }) });
    expect(built.state).toBe("Built");
    expect(built.detail).toContain("not live yet");
    const empty = actsFor({ ...BASE, site: site({ status: "draft", bundleRef: "" }) });
    expect(empty.state).toBe("Not deployed");
    expect(actsFor({ ...BASE, site: site({ status: "disabled" }) }).detail).toContain("temporarily unavailable");
    expect(actsFor({ ...BASE, site: site({ status: "archived" }) }).detail).toContain("name is still held");
  });

  it("never offers more than three acts, on any state", () => {
    // Rule 12's bound. A bar with four things on it is a toolbar.
    for (const status of ["live", "disabled", "archived", "draft"]) {
      for (const p of [null, pkg(), pkg({ updateAvailable: true })]) {
        expect(actsFor({ ...BASE, site: site({ status }), pkg: p }).acts.length).toBeLessThanOrEqual(3);
      }
    }
  });
});

describe("openStopFor -- exactly one stop, chosen by what is a question", () => {
  const standing = (over: Record<string, unknown> = {}) =>
    ({ mode: "standing" as const, pkg: null, app: "", run: null, site: site(), ...over });

  it("opens Live for a settled deployable", () => {
    expect(openStopFor(standing())).toBe("live");
    expect(openStopFor(standing({ site: site({ status: "draft" }) }))).toBe("live");
    expect(openStopFor(standing({ site: site({ status: "archived" }) }))).toBe("live");
  });

  it("opens the stop a RUNNING run is at -- the moving thing is the question", () => {
    expect(openStopFor(standing({ run: run("analyzing") }))).toBe("whatItIs");
    expect(openStopFor(standing({ run: run("building") }))).toBe("build");
    // A PARKED run is not running, so it chooses the stop only for a
    // deployable with nothing of its own to show. The BASE site here is live.
    expect(openStopFor(standing({ run: run("awaiting_confirm") }))).toBe("live");
    expect(openStopFor(standing({ site: site({ status: "draft", bundleRef: "" }), run: run("awaiting_confirm") }))).toBe(
      "whatItIs",
    );
  });

  // THE CASE THAT CARRIES THE DESIGN. A test that only covered the happy path
  // would pass against a rail that always opened Live -- and would send
  // somebody to Live to read "the build failed", which is sending them to
  // repair the wrong thing.
  it("opens the stop a REFUSED run stopped at, not Live", () => {
    const stopped = deploymentFromRow({
      id: "dep-1",
      packageId: "pkg-1",
      status: "failed",
      stoppedAt: "building",
      error: { code: "deployable_build_failed", message: "the build did not finish" },
    });
    expect(openStopFor(standing({ pkg: pkg(), run: stopped }))).toBe("build");
  });
});

// ---------------------------------------------------------------------------
// A GATE ON THE SOURCE IS NOT THIS DEPLOYABLE'S STATE
// ---------------------------------------------------------------------------
//
// Reported with screenshots: `storefront` was live and serving at
// meadow-grotto since 01:10, its rail read five green marks, and its bar said
// "Ready to deploy -- this deploy is waiting for you" with Cancel and Deploy.
// The page also opened on What-it-is rather than Live.
//
// Both came from the SOURCE's parked run. `reading()` tested
// `run.status === "awaiting_confirm"` BEFORE `site.status`, and `openStopFor`
// read RUN_STOP, which maps that status to `whatItIs`. A run at the gate is
// waiting for a PERSON: nothing is executing, and it decides nothing about an
// app that is already serving.
//
// This is the principle `holdWhileTheSourceIsBusy` already states for a
// sibling that is genuinely deploying -- "a published deployable is still
// published while another app of its source deploys, and saying otherwise is
// the defect this is part of fixing". It just was not applied to the gate.
describe("a source's gate, on a deployable that has its own state", () => {
  const gate = run("awaiting_confirm");

  it("leaves a live deployable live, with its own acts", () => {
    const reading = actsFor({ ...BASE, pkg: pkg(), run: gate });
    expect(reading.state).toBe("Live");
    expect(reading.acts.map((a) => a.name)).toEqual(["Take offline", "Redeploy"]);
  });

  it("says the gate is there without claiming it is the state", () => {
    // Discoverable, not authoritative: the source's own row in the list says
    // "a deploy is waiting for you", and this says the same thing beside a
    // state word that stays true.
    expect(actsFor({ ...BASE, pkg: pkg(), run: gate }).detail).toContain("waiting for you");
  });

  it("leaves an offline deployable offline", () => {
    const reading = actsFor({ ...BASE, site: site({ status: "disabled" }), pkg: pkg(), run: gate });
    expect(reading.state).toBe("Offline");
  });

  it("opens on Live, not on the stop the gate parked at", () => {
    expect(openStopFor({ mode: "standing", pkg: pkg(), app: "storefront", run: gate, site: site() })).toBe("live");
  });

  it("STILL speaks for a deployable that has never served", () => {
    // The negative control. Nothing is serving, so the gate IS the most
    // relevant thing on the page and the state word is its own.
    const fresh = site({ status: "draft", bundleRef: "" });
    expect(actsFor({ ...BASE, site: fresh, pkg: pkg(), run: gate }).state).toBe("Ready to deploy");
    expect(openStopFor({ mode: "standing", pkg: pkg(), app: "storefront", run: gate, site: fresh })).toBe("whatItIs");
  });

  it("STILL yields to a run that is actually running", () => {
    // The other control: something IS happening to this app, so the bar says
    // so even though it is live.
    expect(actsFor({ ...BASE, pkg: pkg(), run: run("building") }).state).toBe("Building");
  });

  it("STILL speaks when the gate is scoped to THIS app", () => {
    // The branch that makes this scoping rather than a blanket rule: a gate
    // naming this deployable is its own redeploy awaiting a yes, and it is
    // the question on this page even though the app is serving. Without a
    // case where `scopedTo` matches, the whole distinction is untested and
    // "ignore a parked run on a live app" would pass just as well.
    const mine = site({ packageDeployableName: "storefront" });
    const scoped = run("awaiting_confirm", { scopedTo: ["storefront"] });
    const reading = actsFor({ ...BASE, site: mine, pkg: pkg(), run: scoped });
    expect(reading.state).toBe("Ready to deploy");
    // Its OWN sentence, not the appended clause -- the gate IS the state
    // here, so nothing is mentioned beside it.
    expect(reading.detail).toContain("the report above is what it would do");
    expect(reading.detail).not.toContain("a deploy for this source");
    expect(openStopFor({ mode: "standing", pkg: pkg(), app: "storefront", run: scoped, site: mine })).toBe("whatItIs");
  });
});
