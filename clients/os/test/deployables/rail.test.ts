import { afterEach, describe, expect, it, vi } from "vitest";

import {
  HEAD_STATES,
  PROBE_REASONS,
  RAIL_STOPS,
  headActionFor,
  railFor,
  refusalStopFor,
  TERMINAL_RUN_STATUSES,
  type ComposeInput,
  type RailInput,
  type StandingInput,
  type StandingSite,
} from "../../src/apps/deployables/page/rail";
import { runIsMoving } from "../../src/apps/deployables/page/acts";
import type { AnalysisReport, DeploymentRow, ReportDeployable } from "../../src/apps/deployables/packages/rows";
import { STOP_IDS } from "../../src/apps/deployables/targets";

// The rail is this surface's one idea, and what it SAYS is the thing worth
// pinning -- so the assertions run against `railFor`, a pure function from
// rows to the stops to draw, rather than against a DOM.
//
// The case that matters most is the SKIPPED one. A rail that quietly omitted
// stage and roll would be a rail that never explained why an app-only deploy
// lands in seconds and restarts nothing, and an omission is exactly the kind
// of absence a test written against rendered output does not notice.

function deployment(over: Partial<DeploymentRow> = {}): DeploymentRow {
  return {
    id: "dep-1",
    packageId: "pkg-1",
    sourceVersion: "abc1234",
    status: "succeeded",
    report: { deployables: [], dslDomains: [], problems: [], ok: true },
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
    finishedAt: "2026-09-01T12:00:30Z",
    heartbeatAt: "2026-09-01T12:00:25Z",
    createdAt: "2026-09-01T12:00:00Z",
    ...over,
  };
}

function stageOf(input: RailInput, id: string) {
  const found = railFor(input).stages.find((s) => s.id === id);
  if (found === undefined) throw new Error(`no stage ${id} on the rail`);
  return found;
}

// ---------------------------------------------------------------------------
// The deploy reading -- today's six stages, reproduced exactly
// ---------------------------------------------------------------------------

function stage(row: DeploymentRow, id: string) {
  return stageOf({ mode: "deploy", deployment: row }, id);
}

describe("the deploy reading", () => {
  it("marks stage and roll SKIPPED for a package with no MemQL, and says why", () => {
    const row = deployment({ report: { dslDomains: [], deployables: [], ok: true } });

    for (const id of ["staging_dsl", "rolling"]) {
      expect(stage(row, id).state).toBe("skipped");
    }
    // The reason is the whole value of drawing a skipped stage at all -- and
    // the two stages say DIFFERENT things, because stacking one sentence twice
    // reads as a stutter rather than as two facts.
    expect(stage(row, "staging_dsl").reason).toContain("ships no MemQL");
    expect(stage(row, "rolling").reason).toContain("nothing had to restart");
    expect(stage(row, "staging_dsl").reason).not.toBe(stage(row, "rolling").reason);
  });

  it("does NOT mark them skipped for a package that ships MemQL", () => {
    // The reachable positive for the case above: the same shape, one field
    // different, and the two stages are ordinary again. Without this, "skipped"
    // could be the only answer the function ever gives.
    const row = deployment({
      report: { dslDomains: [{ domain: "acme", constructs: { concept: 2 }, files: 1 }], deployables: [], ok: true },
      dslVersion: "packages/acme/deadbeef/",
    });

    for (const id of ["staging_dsl", "rolling"]) {
      expect(stage(row, id).state).toBe("done");
    }
  });

  it("says 'already the version running' when the MemQL was there but unchanged", () => {
    // Two different reasons for the same skip, and they are not
    // interchangeable: "this package has none" and "yours is already live"
    // send a reader to different conclusions about what just happened.
    const row = deployment({
      report: { dslDomains: [{ domain: "acme", constructs: {}, files: 1 }], deployables: [], ok: true },
      dslVersion: "",
      status: "succeeded",
    });
    expect(stage(row, "staging_dsl").reason).toContain("already the version this cluster is running");
  });

  it("marks the running stage and leaves the ones after it unreached", () => {
    const row = deployment({ status: "building", finishedAt: "" });
    expect(stage(row, "analyzing").state).toBe("done");
    expect(stage(row, "building").state).toBe("current");
    expect(stage(row, "publishing").state).toBe("ahead");
  });

  it("stops a failed run where it got to, and leaves every later stage unreached", () => {
    // This is the D6 guarantee made visible: a failure before publish means
    // nothing was published, and the rail is where somebody reads that.
    const row = deployment({
      status: "failed",
      buildLogTail: "npm ERR! missing script: build",
      deployables: [],
      report: { dslDomains: [], deployables: [], ok: true },
    });
    expect(stage(row, "analyzing").state).toBe("done");
    expect(stage(row, "building").state).toBe("stopped");
    expect(stage(row, "publishing").state).toBe("ahead");
  });

  it("a refusal during analysis stops at analysis", () => {
    const row = deployment({ status: "refused", report: null, buildLogTail: "", deployables: [] });
    expect(stage(row, "analyzing").state).toBe("stopped");
    expect(stage(row, "building").state).toBe("ahead");
  });

  it("keeps the D6 order, and keeps it in the DOM order even when read backwards", () => {
    // The rollback view renders the same stages reversed. The ORDER in the
    // returned array never changes -- only how it is read -- which is what
    // keeps a screen reader's order honest while the picture runs backwards.
    const ids = railFor({ mode: "deploy", deployment: deployment() }).stages.map((s) => s.id);
    expect(ids).toEqual(["analyzing", "awaiting_confirm", "building", "staging_dsl", "rolling", "publishing"]);
  });
});

// ---------------------------------------------------------------------------
// The five stops the target defines
// ---------------------------------------------------------------------------

const NOT_OFFERED = "iOS is not offered on this cluster yet";

function app(over: Partial<ReportDeployable> = {}): ReportDeployable {
  return {
    name: "shop",
    kind: "spa",
    path: "apps/shop",
    buildPlan: "prebuilt output found -- build skipped",
    output: "dist",
    prebuilt: true,
    ...over,
  };
}

function report(over: Partial<AnalysisReport> = {}): AnalysisReport {
  return { deployables: [app()], dslDomains: [], problems: [], ok: true, ...over };
}

/** The report with one offered app and one the cluster knows but does not offer. */
function reportWithUnofferedApp(): AnalysisReport {
  return report({
    deployables: [
      app(),
      app({
        name: "mobile",
        kind: "ios",
        path: "apps/mobile",
        prebuilt: false,
        buildPlan: "",
        problem: { code: "deployable_target_not_offered", message: NOT_OFFERED, scope: "mobile", fatal: true },
      }),
    ],
  });
}

function site(over: Partial<StandingSite> = {}): StandingSite {
  return { hostname: "shop.memql.example.com", kind: "spa", status: "live", bundleRef: "blob://sites/site-1/v1/", ...over };
}

const REPO = { sourceKind: "repo", repoUrl: "https://github.com/acme/shop", repoRef: "main" };

function standing(over: Partial<StandingInput> = {}): StandingInput {
  return { mode: "standing", pkg: REPO, app: "shop", run: deployment({ report: report() }), site: site(), ...over };
}

function statesOf(input: RailInput): Record<string, string> {
  const out: Record<string, string> = {};
  for (const s of railFor(input).stages) out[s.id] = s.state;
  return out;
}

describe("the stop set", () => {
  it("is the target's five stops, in the rail's order, and both new modes draw exactly them", () => {
    expect(RAIL_STOPS.map((s) => s.id)).toEqual([...STOP_IDS]);
    expect(railFor(standing()).stages.map((s) => s.id)).toEqual([...STOP_IDS]);
    expect(railFor({ mode: "compose", answered: [], open: "source", probeReason: "", report: null, problem: null }).stages.map((s) => s.id)).toEqual([
      ...STOP_IDS,
    ]);
  });
});

describe("the standing reading", () => {
  it("a prebuilt app skips Build, with the reason", () => {
    const build = stageOf(standing(), "build");
    expect(build.state).toBe("skipped");
    expect(build.reason).toBe("its built output is in the source");
  });

  it("does NOT skip Build for an app that had to be built", () => {
    // The reachable positive: the same shape, one flag different.
    const built = report({ deployables: [app({ prebuilt: false, buildPlan: "npm run build -> dist", command: "npm run build" })] });
    expect(stageOf(standing({ run: deployment({ report: built }) }), "build").state).toBe("done");
  });

  it("a first deploy ends on 'not serving yet', waiting on the person", () => {
    const first = standing({ site: site({ status: "draft" }) });
    const live = stageOf(first, "live");
    expect(live.reason).toBe("Published to shop.memql.example.com. Not serving yet.");
    // Nothing is moving: the stop holds a still ring, because the next thing
    // that happens is Make it live, and that is the person's.
    expect(live.state).toBe("open");
    expect(stageOf(first, "source").state).toBe("done");
    expect(stageOf(first, "source").reason).toBe("acme/shop at main");
    expect(stageOf(first, "whereItLives").state).toBe("done");
    expect(stageOf(first, "whereItLives").reason).toBe("shop.memql.example.com");
  });

  it("a redeploy is one click: a live site with nothing newer has every stop settled", () => {
    const settled = statesOf(standing());
    expect(settled).toEqual({ source: "done", whatItIs: "done", whereItLives: "done", build: "skipped", live: "done" });
    expect(stageOf(standing(), "live").reason).toBe("Live at shop.memql.example.com.");
    expect(headActionFor({ at: "live", updateAvailable: false })).toEqual({ label: "Redeploy", disabled: false, tone: "quiet" });
  });

  it("a not-offered target renders its sentence on What it is, and the rest deploys", () => {
    const input = standing({ run: deployment({ report: reportWithUnofferedApp() }) });
    const what = stageOf(input, "whatItIs");
    expect(what.state).toBe("done");
    expect(what.reason).toContain(NOT_OFFERED);
    // Non-fatal to the package: the offered app's stops read exactly as they
    // would without the sibling.
    expect(stageOf(input, "whereItLives").state).toBe("done");
    expect(stageOf(input, "build").state).toBe("skipped");
    expect(stageOf(input, "live").state).toBe("done");
  });

  it("names the app's kind and build plan as What it is", () => {
    expect(stageOf(standing(), "whatItIs").reason).toBe("Single-page app, already built");
    const built = report({ deployables: [app({ prebuilt: false, command: "npm run build", buildPlan: "npm run build -> dist" })] });
    expect(stageOf(standing({ run: deployment({ report: built }) }), "whatItIs").reason).toBe("Single-page app, builds with npm run build");
  });

  it("while a run is in flight, the stop it is at is current and the later ones are ahead", () => {
    const at = (status: string) => statesOf(standing({ run: deployment({ status, report: report(), finishedAt: "" }) }));

    expect(at("analyzing")).toEqual({ source: "done", whatItIs: "current", whereItLives: "ahead", build: "ahead", live: "ahead" });
    expect(at("awaiting_confirm").whatItIs).toBe("current");
    expect(at("building")).toEqual({ source: "done", whatItIs: "done", whereItLives: "done", build: "current", live: "ahead" });
    for (const status of ["staging_dsl", "rolling", "publishing"]) {
      expect(at(status).build).toBe("skipped");
      expect(at(status).live).toBe("current");
    }
  });

  it("a run refused at Build stops there, with the engine's refusal, and a live site stays live", () => {
    const refused = deployment({
      status: "refused",
      report: report({ deployables: [app({ prebuilt: false, command: "npm run build", buildPlan: "npm run build -> dist" })] }),
      buildLogTail: "npm ERR! missing script: build",
      error: { code: "deployable_build_failed", message: "npm run build exited 1 in apps/shop", scope: "shop" },
    });
    const input = standing({ run: refused });
    const build = stageOf(input, "build");
    expect(build.state).toBe("stopped");
    expect(build.reason).toBe("npm run build exited 1 in apps/shop");
    // The site is still serving what it was serving (design H), and the
    // standing rail says so rather than dimming a live site into "not
    // reached": the failed update is the Build stop's story, not Live's.
    expect(stageOf(input, "live").state).toBe("done");
    // ...but for a site that was never published, nothing was.
    expect(stageOf(standing({ run: refused, site: null }), "live").state).toBe("ahead");
  });

  it("a run refused before any analysis stops at Source", () => {
    const refused = deployment({
      status: "refused",
      report: null,
      error: { code: "credential_not_found", message: "credential acme-token is not one this caller can read" },
    });
    const input = standing({ run: refused, site: null });
    expect(stageOf(input, "source").state).toBe("stopped");
    expect(stageOf(input, "source").reason).toBe("credential acme-token is not one this caller can read");
    expect(stageOf(input, "whatItIs").state).toBe("ahead");
    expect(stageOf(input, "live").state).toBe("ahead");
  });

  it("a run refused by the analysis stops at What it is", () => {
    const refused = deployment({
      status: "refused",
      report: report({ deployables: [], problems: [{ code: "package_manifest_missing", message: "memql-package.yaml was not found at the root", fatal: true }] }),
      error: { code: "package_manifest_missing", message: "memql-package.yaml was not found at the root" },
    });
    const what = stageOf(standing({ run: refused, site: null }), "whatItIs");
    expect(what.state).toBe("stopped");
    expect(what.reason).toBe("memql-package.yaml was not found at the root");
  });

  it("Live reads the site's status", () => {
    expect(stageOf(standing({ site: site({ status: "live" }) }), "live")).toMatchObject({ state: "done", reason: "Live at shop.memql.example.com." });

    const paused = stageOf(standing({ site: site({ status: "disabled" }) }), "live");
    expect(paused.state).toBe("stopped");
    // The 503-versus-404 sentence the lifecycle carries today.
    expect(paused.reason).toBe("Paused. It answers 503 rather than 404, so a deliberately paused site stays distinguishable from a typo.");

    const archived = stageOf(standing({ site: site({ status: "archived" }) }), "live");
    expect(archived.state).toBe("skipped");
    expect(archived.reason).toBe("Archived. It answers nothing, like a site that never existed.");

    // The placeholder a new deployable starts with (deployables.md) is not a
    // publish, and reading it as one would tell a person their CI had pushed
    // when nothing has.
    const pending = stageOf(standing({ site: site({ status: "draft", bundleRef: "blob://sites/site-1/pending/" }) }), "live");
    expect(pending.state).toBe("ahead");
    expect(pending.reason).toBe("Nothing published yet.");

    expect(stageOf(standing({ site: null }), "live")).toMatchObject({ state: "ahead", reason: "Nothing published yet." });
  });

  it("a hand-made site reads its bundle form as its source", () => {
    const handMade = standing({ pkg: null, app: "", run: null, site: site() });
    expect(stageOf(handMade, "source")).toMatchObject({ state: "done", reason: "uploaded bundle" });
    expect(stageOf(handMade, "whatItIs")).toMatchObject({ state: "done", reason: "Single-page app" });
    expect(stageOf(handMade, "build")).toMatchObject({ state: "skipped", reason: "its built output is in the source" });
    expect(stageOf(handMade, "live").state).toBe("done");

    // A source that has been named but never analyzed has no verdict yet.
    const unanalyzed = standing({ run: null, site: null });
    expect(stageOf(unanalyzed, "source").state).toBe("done");
    expect(stageOf(unanalyzed, "whatItIs").state).toBe("ahead");
  });

  it("names the stop a refused run's error belongs to, and no stop for any other run", () => {
    // The page renders the OS headline beneath the sentence the rail already
    // shows at that stop, so it has to ask the same question the rail
    // answers -- and a page that guessed would send the person to repair the
    // wrong thing.
    const refusedAtBuild = deployment({
      status: "refused",
      buildLogTail: "npm ERR! missing script: build",
      error: { code: "deployable_build_failed", message: "npm run build exited 1" },
    });
    expect(refusalStopFor(refusedAtBuild)).toBe("build");
    expect(refusalStopFor(deployment({ status: "failed", report: null, error: { code: "credential_revoked", message: "revoked" } }))).toBe("source");
    expect(refusalStopFor(deployment({ status: "refused", report: report(), error: { code: "package_manifest_invalid", message: "bad yaml" } }))).toBe("whatItIs");
    // In flight, parked, finished, or nothing at all: no refusal to place.
    for (const status of ["analyzing", "awaiting_confirm", "building", "publishing", "succeeded"]) {
      expect(refusalStopFor(deployment({ status }))).toBeNull();
    }
    expect(refusalStopFor(null)).toBeNull();
  });

  it("never reads a DOM", () => {
    // Every mode runs with no `document` and no `window` in scope. A reading
    // that reached for either would throw here rather than in the one place
    // nobody renders it -- the list row's compact rail is drawn for every row
    // the moment the feed arrives.
    vi.stubGlobal("document", undefined);
    vi.stubGlobal("window", undefined);
    expect(() => railFor(standing())).not.toThrow();
    expect(() => railFor({ mode: "deploy", deployment: deployment() })).not.toThrow();
    expect(() => railFor({ mode: "compose", answered: [], open: "source", probeReason: "", report: null, problem: null })).not.toThrow();
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

// ---------------------------------------------------------------------------
// The compose reading -- the same five stops as inputs
// ---------------------------------------------------------------------------

function compose(over: Partial<ComposeInput> = {}): ComposeInput {
  return { mode: "compose", answered: [], open: "source", probeReason: "", report: null, problem: null, ...over };
}

describe("the compose reading", () => {
  it("a private repository with no credential parks the Source stop", () => {
    const input = compose({ probeReason: PROBE_REASONS.notReachable });
    const source = stageOf(input, "source");
    expect(source.state).toBe("stopped");
    expect(source.reason).toBe("private, or not there");
    // Nothing past a parked stop is reachable yet.
    expect(statesOf(input)).toEqual({ source: "stopped", whatItIs: "pending", whereItLives: "pending", build: "pending", live: "pending" });
  });

  it("the open stop is open, the answered ones complete, the rest pending", () => {
    const input = compose({ answered: ["source"], open: "whatItIs", answers: { source: "acme/shop at main" } });
    expect(statesOf(input)).toEqual({ source: "complete", whatItIs: "open", whereItLives: "pending", build: "pending", live: "pending" });
    // A complete stop's note is its answer. An open or pending stop has no
    // reason of its own -- an empty reason is the contract for "the blurb is
    // the note", and Rail.tsx draws the blurb (railView.test.tsx pins that).
    expect(stageOf(input, "source").reason).toBe("acme/shop at main");
    expect(stageOf(input, "whatItIs").reason).toBe("");
    expect(stageOf(input, "live").reason).toBe("");
  });

  it("a fatal problem parks the flow at the stop it belongs to, with the server's sentence", () => {
    const manifest = compose({
      answered: ["source"],
      open: "whatItIs",
      problem: { code: "package_manifest_missing", message: "memql-package.yaml was not found at the root of the tree" },
    });
    expect(stageOf(manifest, "whatItIs")).toMatchObject({ state: "stopped", reason: "memql-package.yaml was not found at the root of the tree" });
    expect(stageOf(manifest, "source").state).toBe("complete");
    expect(stageOf(manifest, "whereItLives").state).toBe("pending");

    // A credential refusal is the Source stop's, wherever the flow was open.
    const revoked = compose({ answered: ["source"], open: "whatItIs", problem: { code: "credential_revoked", message: "credential acme-token was revoked" } });
    expect(stageOf(revoked, "source")).toMatchObject({ state: "stopped", reason: "credential acme-token was revoked" });
    expect(stageOf(revoked, "whatItIs").state).toBe("pending");

    // A build refusal is Build's, and a publish refusal is Live's.
    const build = compose({ answered: ["source", "whatItIs", "whereItLives"], open: null, problem: { code: "deployable_build_failed", message: "npm run build exited 1" } });
    expect(stageOf(build, "build")).toMatchObject({ state: "stopped", reason: "npm run build exited 1" });
    expect(stageOf(build, "live").state).toBe("pending");
    const publish = compose({ answered: ["source", "whatItIs", "whereItLives"], open: null, problem: { code: "deployable_publish_failed", message: "the bundle has no index.html" } });
    expect(stageOf(publish, "live")).toMatchObject({ state: "stopped", reason: "the bundle has no index.html" });

    // A code this build has no table entry for parks the stop that was open
    // when it arrived -- that is where the flow was, and guessing a different
    // stop would send the person to repair the wrong thing.
    const unknown = compose({ answered: ["source"], open: "whatItIs", problem: { code: "something_new", message: "a sentence from a newer engine" } });
    expect(stageOf(unknown, "whatItIs")).toMatchObject({ state: "stopped", reason: "a sentence from a newer engine" });
  });

  it("a not-offered target renders its sentence on What it is, and the flow proceeds", () => {
    const input = compose({ answered: ["source", "whatItIs"], open: "whereItLives", report: reportWithUnofferedApp() });
    const what = stageOf(input, "whatItIs");
    expect(what.state).toBe("complete");
    expect(what.reason).toContain(NOT_OFFERED);
    expect(stageOf(input, "whereItLives").state).toBe("open");

    // Even handed in as THE problem, it parks nothing: it is fatal to that
    // app and not to the package, exactly as go_pack_not_deployable is.
    const asProblem = compose({
      answered: ["source", "whatItIs"],
      open: "whereItLives",
      report: reportWithUnofferedApp(),
      problem: { code: "deployable_target_not_offered", message: NOT_OFFERED, scope: "mobile" },
    });
    expect(stageOf(asProblem, "whatItIs").state).toBe("complete");
    expect(stageOf(asProblem, "whereItLives").state).toBe("open");
  });

  it("summarises the report as What it is, once it is answered", () => {
    const one = compose({ answered: ["source", "whatItIs"], open: "whereItLives", report: report() });
    expect(stageOf(one, "whatItIs").reason).toBe("1 app");
    const more = compose({
      answered: ["source", "whatItIs"],
      open: "whereItLives",
      report: report({
        deployables: [app(), app({ name: "docs", kind: "static" })],
        dslDomains: [{ domain: "acme", constructs: { concept: 2 }, files: 3 }],
      }),
    });
    expect(stageOf(more, "whatItIs").reason).toBe("2 apps, 1 MemQL domain");
  });

  it("forecasts a prebuilt Build as skipped, with the reason, before anything runs", () => {
    // The confirm gate's rail was always a FORECAST rather than a record, and
    // that is the compose rail's job too: a person learns before the click
    // that this source needs no build.
    const input = compose({ answered: ["source", "whatItIs"], open: "whereItLives", report: report() });
    expect(stageOf(input, "build")).toMatchObject({ state: "skipped", reason: "its built output is in the source" });
    const built = compose({ answered: ["source", "whatItIs"], open: "whereItLives", report: report({ deployables: [app({ prebuilt: false })] }) });
    expect(stageOf(built, "build").state).toBe("pending");
  });
});

// ---------------------------------------------------------------------------
// The Head's action, by state (design section C)
// ---------------------------------------------------------------------------

describe("the Head's action", () => {
  it("follows the design's table, one action per state", () => {
    expect(headActionFor({ at: "composing", sourceComplete: false })).toEqual({ label: "Analyze", disabled: true, tone: "primary" });
    expect(headActionFor({ at: "composing", sourceComplete: true })).toEqual({ label: "Analyze", disabled: false, tone: "primary" });
    expect(headActionFor({ at: "awaiting_confirm", placementsComplete: false })).toEqual({ label: "Deploy", disabled: true, tone: "primary" });
    expect(headActionFor({ at: "awaiting_confirm", placementsComplete: true })).toEqual({ label: "Deploy", disabled: false, tone: "primary" });
    // A run at a non-terminal stage has NO action: the rail is moving.
    expect(headActionFor({ at: "running" })).toBeNull();
    expect(headActionFor({ at: "draft_with_bundle" })).toEqual({ label: "Make it live", disabled: false, tone: "primary" });
    expect(headActionFor({ at: "live", updateAvailable: true })).toEqual({ label: "Deploy the update", disabled: false, tone: "primary" });
    expect(headActionFor({ at: "refused_or_failed" })).toEqual({ label: "Retry", disabled: false, tone: "primary" });
    expect(headActionFor({ at: "live", updateAvailable: false })).toEqual({ label: "Redeploy", disabled: false, tone: "quiet" });
  });

  it("answers for all nine states, and only the moving one answers nothing", () => {
    expect(HEAD_STATES).toHaveLength(9);
    const silent = HEAD_STATES.filter((state) => headActionFor(state) === null);
    expect(silent).toEqual([{ at: "running" }]);
  });
});

// ---------------------------------------------------------------------------
// Purity, structurally
// ---------------------------------------------------------------------------

describe("no DOM in the readings", () => {
  // Read through Vite's raw glob, as map.test.tsx does: it is what the bundler
  // itself sees. The compact rail is drawn for every list row the moment the
  // feed arrives, and a reading that touched the DOM would be a reading that
  // could not be asserted without one.
  const sources = import.meta.glob("../../src/apps/deployables/{page/rail,targets}.ts", {
    query: "?raw",
    eager: true,
    import: "default",
  }) as Record<string, string>;

  it("neither rail.ts nor targets.ts reads the DOM or imports a renderer", () => {
    expect(Object.keys(sources).sort()).toEqual(["../../src/apps/deployables/page/rail.ts", "../../src/apps/deployables/targets.ts"]);
    const forbidden = [/\bdocument\b/, /\bwindow\b/, /\bnavigator\b/, /\blocalStorage\b/, /from "react"/, /from "lucide-react"/, /\.tsx"/];
    for (const [path, source] of Object.entries(sources)) {
      for (const pattern of forbidden) {
        expect(pattern.test(source), `${path} matches ${pattern}`).toBe(false);
      }
    }
  });
});

// ---------------------------------------------------------------------------
// THE SETTLED ROW, WHICH IS WHAT THE LIST ACTUALLY DRAWS
// ---------------------------------------------------------------------------
//
// `standingInputFor` (list.ts) passes `run: row.parked` -- the PARKED run --
// and a run that finished is not parked, so every settled row reaches the rail
// with `run: null` and therefore `report: null`. Every other test in this file
// passes a run carrying a report, which is exactly why this went out: the
// helper's default hid the one input the list produces.
//
// Measured on the live cluster after the v0.20.22 deploy: zero parked runs
// existed, both storefront apps had deployed successfully, and the list drew
// check / EMPTY / check / EMPTY / check -- reporting "not reached" for a build
// whose bundle it was serving on the same row.
describe("a settled deployable, drawn from its standing facts alone", () => {
  const settled = { ...standing(), run: null };

  it("does not report What-it-is and Build as unreached when the site is live and published", () => {
    const states = statesOf(settled);
    expect(states["whatItIs"]).not.toBe("ahead");
    expect(states["build"]).not.toBe("ahead");
  });

  it("reads every stop from the row, so a live published app is done end to end", () => {
    expect(statesOf(settled)).toEqual({
      source: "done",
      whatItIs: "done",
      whereItLives: "done",
      build: "done",
      live: "done",
    });
  });

  it("still says Build has not happened when nothing has been published", () => {
    // The negative control. If Build went `done` for any site with a row, the
    // mark would be decoration rather than a reading -- so a draft that has
    // never published must still read as not built.
    const never = { ...standing(), run: null, site: site({ status: "draft", bundleRef: "blob://sites/site-1/pending/" }) };
    expect(statesOf(never)["build"]).toBe("ahead");
  });
});

// ---------------------------------------------------------------------------
// ONE ANSWER TO "IS THIS RUN FINISHED"
// ---------------------------------------------------------------------------
//
// There were two. `rail.ts` kept a TERMINAL map and `acts.ts` kept a TERMINAL
// set, and when `cancelled` was added to the engine (epic memql#4937) only the
// set learned about it -- so the action bar treated a cancelled run as over
// while the rail treated it as still running, on the same row at the same
// moment. A person's own click reads as a deploy that never ends.
describe("terminal statuses", () => {
  it("agree between the rail and the acts bar", () => {
    // Both read the same set now; this fails the moment a second copy appears.
    for (const status of TERMINAL_RUN_STATUSES) {
      expect(runIsMoving(deployment({ status }))).toBe(false);
    }
  });

  it("include cancelled -- a person's own stop is over, not still running", () => {
    expect([...TERMINAL_RUN_STATUSES]).toContain("cancelled");
    const states = statesOf({ ...standing(), run: deployment({ status: "cancelled" }) });
    // Not `current` anywhere: nothing is moving.
    expect(Object.values(states)).not.toContain("current");
  });

  it("still treats a running status as running", () => {
    // The negative control: if everything were terminal the assertions above
    // would pass on a rail that never moves.
    expect(runIsMoving(deployment({ status: "building" }))).toBe(true);
    expect(statesOf({ ...standing(), run: deployment({ status: "building" }) })["build"]).toBe("current");
  });
});

// ---------------------------------------------------------------------------
// A RUN THAT DECIDED NOTHING MARKS NOTHING
// ---------------------------------------------------------------------------
//
// Reported from production: a deployable that was live and serving showed an
// ERROR on its What-it-is stop, reading "this cluster lost the node that was
// running this deploy... nothing was published, and every site is still
// serving what it was serving."
//
// The sentence refutes the mark it was rendered as. `abandoned` and
// `cancelled` are the two terminal statuses where the engine GUARANTEES the
// run published nothing and failed at nothing -- one is a loss the cluster
// observed, the other a person's own stop. Neither decides anything about the
// deployable, so neither belongs on a stop that describes it.
//
// It landed on What-it-is because the run died PARKED, `stoppedAt` was
// `awaiting_confirm`, and RUN_STOP maps that to `whatItIs` -- so the death of
// a gate was painted onto the stop that says what the app is.
//
// Where it does belong: the run's own timeline (Every attempt), and the action
// bar, which already reads "serving the version before the last attempt, which
// did not finish".
describe("a run that published nothing", () => {
  const live = site({ status: "live", bundleRef: "blob://sites/site-1/v1/" });

  it("leaves a serving deployable's stops alone when the run was abandoned", () => {
    const states = statesOf({ ...standing(), site: live, run: deployment({ status: "abandoned" }) });
    expect(Object.values(states)).not.toContain("stopped");
    expect(states["whatItIs"]).toBe("done");
    expect(states["live"]).toBe("done");
  });

  it("does the same for a run somebody cancelled", () => {
    const states = statesOf({ ...standing(), site: live, run: deployment({ status: "cancelled" }) });
    expect(Object.values(states)).not.toContain("stopped");
  });

  it("places no page-level refusal for either", () => {
    expect(refusalStopFor(deployment({ status: "abandoned" }))).toBeNull();
    expect(refusalStopFor(deployment({ status: "cancelled" }))).toBeNull();
  });

  it("STILL marks the stop for a run that was refused", () => {
    // The negative control, and the distinction the fix rests on: a refusal is
    // a decision ABOUT this deployable -- a manifest it cannot read, a path
    // that is not there -- so it belongs on the stop that repairs it. If this
    // went quiet too, the rail would have stopped reporting real failures.
    const states = statesOf({ ...standing(), site: live, run: deployment({ status: "refused" }) });
    expect(Object.values(states)).toContain("stopped");
    expect(refusalStopFor(deployment({ status: "refused" }))).not.toBeNull();
  });

  it("still counts both as terminal, so nothing reads as running", () => {
    expect(runIsMoving(deployment({ status: "abandoned" }))).toBe(false);
    expect(runIsMoving(deployment({ status: "cancelled" }))).toBe(false);
  });
});
