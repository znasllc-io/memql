// The three states a remote cluster's deploy pipeline can be in (memql#3740),
// and what the remote page says about each.
//
// NONE OF THEM IS AN ERROR, which is the whole design. "Your role cannot see
// this" and "this cluster has no deploy pipeline" both arrive as a failed
// status read and ask different things of the operator, so the read carries a
// typed reason. And NOT CONNECTED IS NEITHER: the old page headed an editor
// that was merely signed out "No deploy pipeline is configured for this
// cluster". That is now the bar's job, with Sign in on it.

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility } from "../src/deploy/actions.js";
import type { StatusRead } from "../src/deploy/controller.js";
import { remoteOverviewBar } from "../src/deploy/instanceActions.js";
import { pipelineState, type PipelineState } from "../src/deploy/pipelineState.js";
import type { Instance } from "../src/state/deployments.js";
import type { ConnectionWord } from "../src/state/deploymentsCatalog.js";
import { remoteOverviewScreen, type ActOutcome } from "../src/webview/deploymentScreens.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");
const REMOTE: Instance = {
  name: "staging",
  kind: "remote",
  presence: "installed-healthy",
  version: "v0.9.2",
  versionLabel: "v0.9.2",
  connected: true,
};

const OWNER = roleVisibility("owner");

function read(over: Partial<StatusRead> = {}): StatusRead {
  return { status: null, message: "", reason: "unavailable", ...over } as StatusRead;
}

function status(rollouts: { name: string; phase: string }[] = []): StatusRead["status"] {
  return { rollouts } as never;
}

test("a status that answered is the pipeline being present, with nothing to say", () => {
  const state = pipelineState(read({ status: status(), reason: "ok" }), OWNER);
  assert.equal(state.kind, "present");
  assert.equal(state.actions.length, 4);
  assert.equal(state.line, "");
});

test("an engine-only cluster reads as no pipeline: one short line, the engine's words kept for Details", () => {
  const engine =
    "deployment status unavailable (FAILED_PRECONDITION) -- local clusters are operated via `make up` (k3d + ArgoCD), not the deploy console";
  const state = pipelineState(read({ message: engine, reason: "unavailable" }), OWNER);
  assert.equal(state.kind, "notConfigured");
  assert.equal(state.line, "Deployments aren't set up for this cluster.");
  assert.equal(state.engineMessage, engine);
  assert.deepEqual(state.actions, []);
});

test("the role gate is its own state, and an owner does not make it go away", () => {
  const state = pipelineState(read({ message: "requires the owner or admin cluster role", reason: "permissionDenied" }), OWNER);
  assert.equal(state.kind, "notVisible");
  assert.equal(state.line, "Your role can't view deployment status.");
  assert.match(state.engineMessage, /owner or admin/);
  assert.deepEqual(state.actions, []);
});

test("a present pipeline draws only what the role holds", () => {
  const developer = pipelineState(read({ status: status(), reason: "ok" }), roleVisibility("developer"));
  assert.deepEqual(developer.actions.map((a) => a.id).sort(), ["cutVersion", "deploy"]);
  const admin = pipelineState(read({ status: status(), reason: "ok" }), roleVisibility("admin"));
  assert.equal(admin.actions.some((a) => a.id === "rolloutAction"), true);
  assert.equal(admin.actions.some((a) => a.id === "rollback"), false);
  assert.deepEqual(pipelineState(read({ status: status(), reason: "ok" }), roleVisibility("writer")).actions, []);
});

test("an ok read with no status is still not a pipeline", () => {
  const state = pipelineState(read({ status: null, reason: "ok" }), OWNER);
  assert.equal(state.kind, "notConfigured");
  assert.deepEqual(state.actions, []);
});

test("only a rollout part-way through can be promoted or aborted, and it is named", () => {
  const state = pipelineState(
    read({
      status: status([
        { name: "bff", phase: "Paused" },
        { name: "agent", phase: "Healthy" },
        { name: "cognition", phase: "Progressing" },
        { name: "", phase: "Paused" },
      ]),
      reason: "ok",
    }),
    OWNER,
  );
  assert.deepEqual(state.rollouts, ["bff", "cognition"]);
  assert.deepEqual(pipelineState(read({ message: "x", reason: "unavailable" }), OWNER).rollouts, []);
});

// -----------------------------------------------------------------------------
// what the remote page draws
// -----------------------------------------------------------------------------

function page(connection: ConnectionWord, pipeline: PipelineState | undefined, over: { outcome?: ActOutcome; instance?: Instance } = {}): string {
  const instance = over.instance ?? REMOTE;
  const parts = remoteOverviewScreen({
    instance,
    bar: remoteOverviewBar({ instance, connection, upgrade: { kind: "none", reason: "" }, pipeline, visibility: OWNER, runs: [] }),
    connection,
    runs: [],
    nowMs: NOW,
    pipeline,
    upgrade: { kind: "none", reason: "" },
    ...(over.outcome === undefined ? {} : { outcome: over.outcome }),
    detailsOpen: false,
  });
  // The kit escapes an apostrophe; the assertions read the words.
  return (parts.head + parts.body + parts.actions).replace(/&#39;/g, "'");
}

test("signed out is Sign in, never 'No deploy pipeline is configured'", () => {
  const html = page("signIn", undefined);
  assert.ok(!/deploy pipeline/i.test(html));
  assert.ok(!html.includes("aren't set up"));
  assert.ok(html.includes("Sign in to see history."));
  assert.ok(html.includes(`data-act="signIn"`));
  assert.ok(html.includes("Not signed in"));
});

test("not connected offers Connect, and says the history needs it", () => {
  const html = page("none", undefined);
  assert.ok(html.includes("Connect to see history."));
  assert.ok(html.includes(`data-act="connect"`));
});

test("each pipeline state says one short line, and the engine's sentence stays in Details", () => {
  const engine = "deployment status unavailable (FAILED_PRECONDITION)";
  const notConfigured = page("connected", pipelineState(read({ message: engine, reason: "unavailable" }), OWNER));
  assert.ok(notConfigured.includes("Deployments aren't set up for this cluster."));
  // Present, but under Details rather than as the page's words.
  const details = notConfigured.indexOf('id="dp-details"');
  assert.ok(details > 0 && notConfigured.indexOf("FAILED_PRECONDITION") > details);
  assert.ok(!notConfigured.includes("The engine decides every one of these"), "developer doctrine reached the page");

  const notVisible = page("connected", pipelineState(read({ message: "requires the owner or admin cluster role", reason: "permissionDenied" }), roleVisibility("developer")));
  assert.ok(notVisible.includes("Your role can't view deployment status."));
});

test("an outcome is one sentence on the page, its audit reference under Details", () => {
  const html = page("connected", pipelineState(read({ status: status(), reason: "ok" }), OWNER), {
    outcome: { tone: "error", line: "You need the owner role to roll back.", auditId: "ae-1", signIn: false },
  });
  assert.ok(html.includes("You need the owner role to roll back."));
  assert.ok(html.indexOf("ae-1") > html.indexOf('id="dp-details"'));
  assert.ok(!/SUCCESS:|ERROR:/.test(html), "a log-shaped line reached the page");
});

test("an expired session's outcome offers Sign in beside the sentence", () => {
  const html = page("connected", pipelineState(read({ status: status(), reason: "ok" }), OWNER), {
    outcome: { tone: "error", line: "Your session has expired.", auditId: "", signIn: true },
  });
  const notice = html.slice(html.indexOf("Your session has expired."));
  assert.ok(notice.includes(`data-act="signIn"`));
});

test("everything a remote page draws is escaped", () => {
  const html = page("connected", pipelineState(read({ message: "<script>alert(1)</script>", reason: "unavailable" }), OWNER), {
    instance: { name: "<img src=x>", kind: "remote", presence: "installed-healthy", connected: true },
    outcome: { tone: "info", line: "<b>x</b>", auditId: "<i>", signIn: false },
  });
  assert.doesNotMatch(html, /<\/?script/i);
  assert.doesNotMatch(html, /<img /);
  assert.doesNotMatch(html, /<b>x/);
  assert.match(html, /&lt;img src=x&gt;/);
});
