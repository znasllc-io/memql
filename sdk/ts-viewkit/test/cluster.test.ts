// The cluster renderers (memql#3312).
//
// These assert the MARKUP CONTRACT the hosts depend on -- the data attributes
// a host colours by, the `data-row-id` the existing click delegation reads,
// and the fact that an absent version or deployment id is drawn rather than
// dropped. They deliberately do NOT assert on CSS: styles.test.ts owns the
// class/stylesheet drift guard, and duplicating it here would mean two places
// to update for every rule.

import test from "node:test";
import assert from "node:assert/strict";

import {
  renderDeploymentHistory,
  renderTally,
  renderTopologyGrid,
  type DeploymentHistoryView,
  type TallyRowView,
  type TopologyNodeView,
} from "../src/cluster.js";
import { renderToHtml } from "../src/vnode.js";

function node(over: Partial<TopologyNodeView> = {}): TopologyNodeView {
  return {
    id: "bff-local",
    label: "bff",
    detail: "bff-local",
    version: "2026.6.21",
    deployment: "a1b2c3d4e5f6",
    health: "healthy",
    orphan: false,
    orphanReason: "",
    ...over,
  };
}

function deployment(over: Partial<DeploymentHistoryView> = {}): DeploymentHistoryView {
  return {
    id: "d-100",
    version: "2026.6.21",
    environment: "staging",
    provider: "azure",
    status: "succeeded",
    current: false,
    ...over,
  };
}

// -----------------------------------------------------------------------------
// renderTopologyGrid
// -----------------------------------------------------------------------------

test("an empty topology renders the empty-state, not an empty grid", () => {
  const html = renderToHtml(renderTopologyGrid([]));
  assert.match(html, /vk-empty/);
  assert.match(html, /No cluster nodes registered\./);
  assert.doesNotMatch(html, /vk-grid/);
});

test("a topology tile carries the node id, health and running version", () => {
  const html = renderToHtml(renderTopologyGrid([node()]));
  assert.match(html, /data-node-id="bff-local"/);
  // Health appears twice by design: on the tile (so a host can style the whole
  // tile) and on the pill (so it can style just the badge).
  assert.match(html, /<li class="vk-tile" data-node-id="bff-local" data-health="healthy">/);
  assert.match(html, /<span class="vk-tile-health" data-health="healthy">healthy<\/span>/);
  assert.match(html, /2026\.6\.21/);
  assert.match(html, /deploy a1b2c3d4e5f6/);
  assert.doesNotMatch(html, /data-orphan/);
  assert.doesNotMatch(html, /vk-flag/);
});

test("an orphan tile is flagged and carries its reason as the tooltip", () => {
  const html = renderToHtml(
    renderTopologyGrid([
      node({ orphan: true, orphanReason: "deployment d-099 is not the current deployment" }),
    ]),
  );
  assert.match(html, /data-orphan="true"/);
  assert.match(html, /class="vk-flag" title="deployment d-099 is not the current deployment"/);
  assert.match(html, /\[orphan\]/);
});

test("an unresolved version or deployment id is drawn explicitly, never dropped", () => {
  // The whole point of the grid is "which release is this node running". A
  // silently absent line reads as "nothing to report" rather than "this could
  // not be resolved", which is a different and much worse answer.
  const html = renderToHtml(renderTopologyGrid([node({ version: "", deployment: "" })]));
  assert.match(html, /version unknown/);
  assert.match(html, /no deployment id/);
  // Both use the vk-null marker so they read as "not data" the way a null
  // field does in the detail pane.
  assert.match(html, /class="vk-tile-meta vk-null"/);
});

test("a node with no health at all still renders a pill", () => {
  const html = renderToHtml(renderTopologyGrid([node({ health: "" })]));
  assert.match(html, /<span class="vk-tile-health" data-health="">unknown<\/span>/);
});

test("a tile with no secondary detail omits only that line", () => {
  const html = renderToHtml(renderTopologyGrid([node({ detail: "" })]));
  assert.doesNotMatch(html, /vk-tile-detail/);
  assert.match(html, /vk-tile-name/);
});

test("topology values are escaped", () => {
  const html = renderToHtml(
    renderTopologyGrid([node({ label: '<script>x</script>', orphan: true, orphanReason: '"quoted"' })]),
  );
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, /title="&quot;quoted&quot;"/);
});

// -----------------------------------------------------------------------------
// renderTally
// -----------------------------------------------------------------------------

test("an empty tally renders the caller's own empty message", () => {
  const html = renderToHtml(renderTally([], "This deployment declares no per-tier spec."));
  assert.match(html, /vk-empty/);
  assert.match(html, /This deployment declares no per-tier spec\./);
});

test("an unflagged tally row carries no flag marker", () => {
  const rows: TallyRowView[] = [
    { name: "cognition", detail: "2026.6.21", count: "2 of 2", flag: "" },
  ];
  const html = renderToHtml(renderTally(rows, "empty"));
  assert.match(html, /<li class="vk-tally-row">/);
  assert.match(html, /cognition/);
  assert.match(html, /2 of 2/);
  assert.doesNotMatch(html, /data-flagged/);
  assert.doesNotMatch(html, /vk-flag/);
});

test("a flagged tally row sets data-flagged and renders the bracketed flag", () => {
  const rows: TallyRowView[] = [
    { name: "agent", detail: "2026.6.21", count: "1 of 2", flag: "under-replica" },
  ];
  const html = renderToHtml(renderTally(rows, "empty"));
  assert.match(html, /data-flagged="true"/);
  assert.match(html, /<span class="vk-flag">\[under-replica\]<\/span>/);
});

test("a tally row with no detail omits the detail slot", () => {
  const html = renderToHtml(
    renderTally([{ name: "bff", detail: "", count: "1", flag: "" }], "empty"),
  );
  assert.doesNotMatch(html, /vk-tally-detail/);
});

// -----------------------------------------------------------------------------
// renderDeploymentHistory
// -----------------------------------------------------------------------------

test("an empty history renders the empty-state", () => {
  const html = renderToHtml(renderDeploymentHistory([]));
  assert.match(html, /No deployments recorded for this cluster\./);
});

test("history rows reuse the vk-row contract so existing click delegation works", () => {
  // The host's row-click listener reads data-row-id. Reusing it is what lets
  // the deployment list be selectable with no new wiring on either consumer.
  const html = renderToHtml(renderDeploymentHistory([deployment()]));
  assert.match(html, /<li class="vk-row" data-row-id="d-100">/);
  assert.match(html, /class="vk-row-primary">2026\.6\.21</);
  assert.match(html, /class="vk-row-secondary">staging</);
  assert.match(html, /class="vk-row-tertiary">azure</);
  assert.match(html, /class="vk-row-status" data-status="succeeded">succeeded</);
});

test("the current deployment is marked, and only it", () => {
  const html = renderToHtml(
    renderDeploymentHistory([
      deployment({ id: "d-101", current: true }),
      deployment({ id: "d-100", current: false }),
    ]),
  );
  assert.equal(html.match(/data-current="true"/g)?.length, 1);
  assert.equal(html.match(/\[current\]/g)?.length, 1);
  // The marker must land on the row that claimed it.
  assert.match(html, /data-row-id="d-101" data-current="true"/);
});

test("the selected deployment is marked independently of the current one", () => {
  const html = renderToHtml(
    renderDeploymentHistory(
      [deployment({ id: "d-101", current: true }), deployment({ id: "d-100" })],
      "d-100",
    ),
  );
  assert.match(html, /data-row-id="d-100" data-selected="true"/);
  assert.doesNotMatch(html, /data-row-id="d-101" data-selected/);
});

test("a version-less deployment falls back to its id so it stays identifiable", () => {
  // A freshly cut, still-pending record can have no version yet. Rendering an
  // empty primary slot would make it unclickable in practice -- there would be
  // nothing to aim at.
  const html = renderToHtml(
    renderDeploymentHistory([deployment({ id: "d-102", version: "", status: "pending" })]),
  );
  assert.match(html, /class="vk-row-primary">d-102</);
});

test("deployment history preserves the order it is handed", () => {
  // Newest-first is the CALLER's decision (state/deploymentHistory.ts) -- the
  // renderer must not quietly re-sort, or the two consumers could disagree
  // about what "newest" means.
  const html = renderToHtml(
    renderDeploymentHistory([
      deployment({ id: "z-first" }),
      deployment({ id: "a-second" }),
    ]),
  );
  assert.ok(html.indexOf("z-first") < html.indexOf("a-second"));
});

test("history values are escaped", () => {
  const html = renderToHtml(
    renderDeploymentHistory([deployment({ version: '<img src=x onerror=1>' })]),
  );
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});
