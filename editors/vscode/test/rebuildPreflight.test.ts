// The "Before it runs" list for a rebuild, and the screens around it
// (memql#4246).
//
// The item that carries the epic is "Image source". A rebuild moves a cluster
// off released images and onto ones built from a working tree, and NOTHING ELSE
// on the machine announces that -- the row afterwards reads `checkout <commit>`
// and the operator is left to work out why. Crossing a lane is stated BEFORE it
// happens, in both directions: this screen says what a rebuild switches to, and
// state/preflight.ts says what an install, upgrade or repair switches back.
//
// `state` is preflight.ts's own vocabulary -- "ok" | "attention" -- so the two
// checklists render through one `renderPreflight` and cannot diverge in how a
// warning looks.

import test from "node:test";
import assert from "node:assert/strict";

import { rebuildPreflightItems } from "../src/state/rebuildPreflight.js";
import type { StepProgress } from "../src/state/addCluster.js";
import { renderFailedScreen, renderRebuildScreen } from "../src/webview/installScreens.js";

const base = {
  dockerReachable: true,
  checkoutDir: "/home/me/.memql/src",
  checkoutIsMemql: true,
  state: {
    commit: "abc1234def",
    ref: { kind: "tag" as const, name: "v0.17.0" },
    dirtyCount: 4,
    deployDirty: false,
  },
  nodes: "",
  imageSource: "released" as const,
  releasedTag: "v0.17.0",
};

test("the checklist states every fact the design names, in order", () => {
  const labels = rebuildPreflightItems(base).map((i) => i.label);
  assert.deepEqual(labels, ["Docker", "Checkout", "Git state", "Nodes", "Image source", "Duration"]);
});

test("crossing from released images is stated, and staying in checkout mode is not", () => {
  const crossing = rebuildPreflightItems(base).find((i) => i.label === "Image source")!;
  assert.equal(crossing.state, "attention");
  assert.match(crossing.detail, /switches local to images built from your checkout/);
  assert.match(crossing.detail, /install, upgrade or repair returns it to released v0\.17\.0 images/);
  const staying = rebuildPreflightItems({ ...base, imageSource: "checkout" }).find(
    (i) => i.label === "Image source",
  )!;
  assert.equal(staying.state, "ok");
});

test("a dirty deploy/ tree is called out because manifests do not ride a rebuild", () => {
  const git = rebuildPreflightItems({
    ...base,
    state: { ...base.state, deployDirty: true },
  }).find((i) => i.label === "Git state")!;
  assert.equal(git.state, "attention");
  assert.match(git.detail, /deploy\/ has edits/);
  assert.match(git.detail, /manifests do not ride a rebuild/);
});

test("a missing checkout or an unreachable docker is a note naming the fix", () => {
  assert.match(rebuildPreflightItems({ ...base, dockerReachable: false })[0]!.detail, /Docker/);
  assert.equal(rebuildPreflightItems({ ...base, dockerReachable: false })[0]!.state, "attention");
  assert.match(
    rebuildPreflightItems({ ...base, checkoutIsMemql: false })[1]!.detail,
    /not a MemQL checkout/,
  );
});

test("the node line names the default honestly", () => {
  assert.match(
    rebuildPreflightItems(base).find((i) => i.label === "Nodes")!.detail,
    /all app nodes/,
  );
  assert.match(
    rebuildPreflightItems({ ...base, nodes: "bff,agent" }).find((i) => i.label === "Nodes")!.detail,
    /bff, agent/,
  );
});

test("the checklist words the same string the run will be given", () => {
  // ONE NORMALISER, BOTH PATHS. The display side used to tidy the operator's
  // typing for the sentence while the send side forwarded it raw, so the
  // checklist blessed "bff, agent" and the script exited 2 on " agent".
  const nodes = (raw: string): string =>
    rebuildPreflightItems({ ...base, nodes: raw }).find((i) => i.label === "Nodes")!.detail;
  assert.match(nodes("bff, agent"), /bff, agent\./);
  assert.match(nodes(" bff ,agent,, "), /bff, agent\./);
  // A list that is nothing but separators is not a list.
  assert.match(nodes("  ,  , "), /all app nodes/);
});

// -----------------------------------------------------------------------------
// the rebuild's own screens
// -----------------------------------------------------------------------------

test("a failed rebuild offers Retry and Back -- guided has nothing to offer it", () => {
  // "Switch to guided" is a WIZARD concept: it re-runs one step with the
  // operator driving the privileged part by hand. A rebuild is one unprivileged
  // step, so the control is a second Retry wearing a name that promises
  // something else.
  const failure: StepProgress = {
    id: "rebuildFromCheckout",
    description: "Build the node images",
    state: "failed",
    exitCode: 5,
    reason: "an image build failed",
    log: "",
    guided: false,
    remedy: "",
  };
  const rebuild = renderFailedScreen({
    steps: [failure],
    mode: "rebuild",
    running: false,
    failures: [failure],
  });
  assert.match(rebuild, /data-act="retry"/);
  assert.match(rebuild, /data-act="cancel"/);
  assert.doesNotMatch(rebuild, /data-act="guided"/);

  // Every other run keeps it: the wizard's graph has privileged steps, which is
  // the whole case for the control.
  assert.match(
    renderFailedScreen({ steps: [failure], mode: "deploy", running: false, failures: [failure] }),
    /data-act="guided"/,
  );
});

test("the rebuild screen names the checkout and asks one thing", () => {
  const html = renderRebuildScreen({
    checkoutDir: "/home/me/.memql/src",
    nodes: "bff",
    preflight: rebuildPreflightItems(base),
  });
  assert.match(html, /\/home\/me\/\.memql\/src/);
  assert.match(html, /data-field="nodes"/);
  assert.match(html, /data-act="beginRebuild"/);
  // The checklist is above the Start button, so it is an informed click.
  assert.ok(
    html.indexOf("Before it runs") < html.indexOf('data-act="beginRebuild"'),
    "the checklist must render before the Start button",
  );
});

test("git state that could not be read says so rather than reporting a clean tree", () => {
  // The honest answer, and the one that matters: "clean at 0000000" in front of
  // a build whose provenance nothing recorded is worse than saying nothing.
  const items = rebuildPreflightItems({ ...base, state: undefined });
  const git = items.find((i) => i.label === "Git state")!;
  assert.equal(git.state, "attention");
  assert.match(git.detail, /git could not read the checkout/);
  // And the list is still the same six lines: a fact that could not be read is
  // still a line, because a missing row reads as a fact nobody thought about.
  assert.equal(items.length, 6);
});

test("the git line names the ref, the short commit and the count", () => {
  const git = rebuildPreflightItems(base).find((i) => i.label === "Git state")!;
  assert.match(git.detail, /tag v0\.17\.0/);
  assert.match(git.detail, /abc1234\b/);
  assert.match(git.detail, /4 uncommitted files/);
  assert.equal(git.state, "ok");
  // One file is not "1 files".
  const one = rebuildPreflightItems({
    ...base,
    state: { ...base.state, dirtyCount: 1 },
  }).find((i) => i.label === "Git state")!;
  assert.match(one.detail, /1 uncommitted file\./);
  const clean = rebuildPreflightItems({
    ...base,
    state: { ...base.state, dirtyCount: 0 },
  }).find((i) => i.label === "Git state")!;
  assert.match(clean.detail, /clean/);
});

test("a detached checkout is named as detached rather than as an empty ref", () => {
  const git = rebuildPreflightItems({
    ...base,
    state: { ...base.state, ref: { kind: "detached" as const, name: "" } },
  }).find((i) => i.label === "Git state")!;
  assert.match(git.detail, /detached HEAD/);
});
