// The three states a remote instance's deploy pipeline can be in (memql#3740).
//
// NONE OF THEM IS AN ERROR, which is the whole design and the reason each one
// is reachable here. A row of buttons that turn out to be refused would be the
// error; naming the state is not.
//
// The distinction the tests exist to protect is between the two FAILURES.
// "Your role cannot see this" and "this cluster has no deploy pipeline" both
// arrive as a failed status read, and they ask completely different things of
// the operator: one is a permissions conversation, the other is the ordinary
// condition of every engine-only cluster. Telling them apart by matching on
// prose would break the first time the wording is improved, which is why the
// read carries a typed reason.

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility } from "../src/deploy/actions.js";
import type { StatusRead } from "../src/deploy/controller.js";
import { pipelineState } from "../src/deploy/pipelineState.js";
import type { Instance } from "../src/state/deployments.js";
import { renderRemoteInstance } from "../src/webview/deploymentScreens.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");
const REMOTE: Instance = {
  name: "staging",
  kind: "remote",
  presence: "installed-healthy",
  version: "v0.9.2",
  connected: true,
};

const OWNER = roleVisibility("owner");

function read(over: Partial<StatusRead> = {}): StatusRead {
  return { status: null, message: "", reason: "unavailable", ...over } as StatusRead;
}

test("a status that answered is the pipeline being present", () => {
  const state = pipelineState(
    read({ status: { environment: "staging" } as never, reason: "ok" }),
    OWNER,
  );
  assert.equal(state.kind, "present");
  assert.equal(state.actions.length, 5);
});

test("an engine-only cluster reads as no pipeline, in the engine's own words", () => {
  // The commonest state for anyone running this repository: the orchestration
  // moved to the product repo, and deploycontrol refuses docker-local outright.
  const state = pipelineState(
    read({
      message:
        "deployment status unavailable (FAILED_PRECONDITION) -- local clusters are operated via `make up` (k3d + ArgoCD), not the deploy console",
      reason: "unavailable",
    }),
    OWNER,
  );
  assert.equal(state.kind, "notConfigured");
  assert.match(state.detail, /local clusters are operated via/);
  // Not reworded: the operator may need to match it against a log line.
  assert.match(state.detail, /FAILED_PRECONDITION/);
  // And no buttons. An action whose own status read failed is an action with
  // nothing to act on.
  assert.deepEqual(state.actions, []);
});

test("the role gate is its own state, and an owner does not make it go away", () => {
  // The read is what was refused. This page reports what the ENGINE said; it
  // does not conclude from a role table that the refusal must have been wrong.
  const state = pipelineState(read({ message: "requires the owner or admin cluster role", reason: "permissionDenied" }), OWNER);
  assert.equal(state.kind, "notVisible");
  assert.match(state.detail, /owner or admin/);
  assert.deepEqual(state.actions, []);
});

test("a developer whose read was refused still lands in notVisible, not notConfigured", () => {
  const state = pipelineState(
    read({ message: "requires the owner or admin cluster role", reason: "permissionDenied" }),
    roleVisibility("developer"),
  );
  assert.equal(state.kind, "notVisible");
});

test("a present pipeline draws only what the role holds", () => {
  const developer = pipelineState(read({ status: {} as never, reason: "ok" }), roleVisibility("developer"));
  assert.deepEqual(developer.actions.map((a) => a.id).sort(), ["cutVersion", "deploy"]);

  const admin = pipelineState(read({ status: {} as never, reason: "ok" }), roleVisibility("admin"));
  assert.equal(admin.actions.some((a) => a.id === "promote"), true);
  // Rollback is owner-only in the service, and this mirrors it exactly.
  assert.equal(admin.actions.some((a) => a.id === "rollback"), false);

  const writer = pipelineState(read({ status: {} as never, reason: "ok" }), roleVisibility("writer"));
  assert.deepEqual(writer.actions, []);
});

test("a role that could not be read is offered everything, with the engine deciding", () => {
  const state = pipelineState(read({ status: {} as never, reason: "ok" }), roleVisibility(undefined));
  assert.equal(state.actions.length, 5);
  // And the page says so, rather than implying the buttons are permissions.
  assert.match(state.detail, /courtesy/);
});

test("an ok read with no status is still not a pipeline", () => {
  // Belt and braces: `reason` and `status` disagreeing is a contract violation
  // somewhere upstream, and the safe reading is the one that offers no buttons.
  const state = pipelineState(read({ status: null, reason: "ok" }), OWNER);
  assert.equal(state.kind, "notConfigured");
  assert.deepEqual(state.actions, []);
});

test("a failure with no message still says something", () => {
  const state = pipelineState(read({ message: "", reason: "unavailable" }), OWNER);
  assert.equal(state.kind, "notConfigured");
  assert.notEqual(state.detail, "");
});

// -----------------------------------------------------------------------------
// what the page actually draws
// -----------------------------------------------------------------------------

test("each of the three states renders, and the engine's sentence survives verbatim", () => {
  const refusal =
    "deployment status requires the owner or admin cluster role. Topology and deployment history above are ordinary concept rows and are unaffected.";
  const notVisible = renderRemoteInstance({
    instance: REMOTE,
    runs: [],
    pipeline: pipelineState(read({ message: refusal, reason: "permissionDenied" }), roleVisibility("developer")),
    nowMs: NOW,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  // VERBATIM. The sentence names the role that would have worked, and a
  // paraphrase is one more thing that can be wrong.
  assert.ok(notVisible.includes(refusal));
  assert.doesNotMatch(notVisible, /data-deploy=/);

  const notConfigured = renderRemoteInstance({
    instance: REMOTE,
    runs: [],
    pipeline: pipelineState(read({ message: "local clusters are operated via `make up`", reason: "unavailable" }), OWNER),
    nowMs: NOW,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  assert.match(notConfigured, /No deploy pipeline is configured/);
  assert.doesNotMatch(notConfigured, /data-deploy=/);

  const present = renderRemoteInstance({
    instance: REMOTE,
    runs: [],
    pipeline: pipelineState(read({ status: {} as never, reason: "ok" }), OWNER),
    nowMs: NOW,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  assert.match(present, /data-deploy="rollback"/);
});

test("an unreachable remote still lists, with its version drawn as unknown", () => {
  const html = renderRemoteInstance({
    instance: { name: "staging", kind: "remote", presence: "installed-unreachable", connected: false },
    runs: [],
    pipeline: pipelineState(read({ message: "not connected", reason: "unavailable" }), OWNER),
    nowMs: NOW,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  // Listed, not hidden -- and the version says the word rather than nothing,
  // so "we could not work it out" is never read as "it has none".
  assert.match(html, /staging/);
  assert.match(html, /unknown/);
});

test("a remote run's items are labelled Node types, never Steps", () => {
  // A local run's items are capability-script executions; a remote run's are
  // per-tier spec rows. The label is the only place that asymmetry is visible.
  const html = renderRemoteInstance({
    instance: REMOTE,
    runs: [
      {
        id: "d2",
        instance: "staging",
        kind: "rollout",
        toVersion: "v0.9.2",
        startedAt: "2026-08-13T00:00:00Z",
        status: "succeeded",
        items: [
          { label: "bff", status: "ok", detail: "v0.9.2 (inherited) - 2 replicas - digest abcdef012345" },
          { label: "cognition", status: "ok", detail: "v0.9.5 (pinned) - 1 replica" },
        ],
      },
    ],
    pipeline: pipelineState(read({ status: {} as never, reason: "ok" }), OWNER),
    nowMs: NOW,
    outcome: "",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  assert.match(html, /Node types/);
  assert.doesNotMatch(html, /&gt;Steps&lt;|>Steps</);
  assert.match(html, /bff/);
  assert.match(html, /2 replicas/);
  assert.match(html, /digest abcdef012345/);
  assert.match(html, /\(pinned\)/);
});

test("an outcome line is rendered as the engine wrote it, error or not", () => {
  const refused = renderRemoteInstance({
    instance: REMOTE,
    runs: [],
    pipeline: pipelineState(read({ status: {} as never, reason: "ok" }), OWNER),
    nowMs: NOW,
    outcome: "ERROR: rollback_deployment requires the owner cluster role (audit ae-1)",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  assert.match(refused, /requires the owner cluster role/);
  assert.match(refused, /audit ae-1/);
  assert.match(refused, /class="error"/);
});

test("everything a remote page draws is escaped", () => {
  const html = renderRemoteInstance({
    instance: { name: "<img src=x>", kind: "remote", presence: "installed-healthy", connected: true },
    runs: [],
    pipeline: pipelineState(read({ message: "<script>alert(1)</script>", reason: "unavailable" }), OWNER),
    nowMs: NOW,
    outcome: "<b>x</b>",
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
  });
  assert.doesNotMatch(html, /<\/?script/i);
  assert.doesNotMatch(html, /<img /);
  assert.match(html, /&lt;img src=x&gt;/);
});
