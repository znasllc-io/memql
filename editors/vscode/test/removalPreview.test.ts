// The uninstall preview, as the rows an operator reads before consenting.
//
// `previewUninstall` (#3469) already decided what will be removed, what will be
// kept, and what has nothing behind it at all. What is asserted here is that
// the mapping to the page's rows preserves those decisions exactly: it does
// not re-order the two lists and -- the promise the whole screen exists to
// make -- does not quietly drop an artifact or invent one.
//
// The rules, one section each:
//   1. removals first, then kept, each in the order it already carries
//   2. a step with nothing to remove produces no row
//   3. nothing absent from the preview is ever in the output
//   4. every kept row says why, and no removal does
//
// Naming, the masked place and shared tools are addClusterLanding.test.ts.
// The fixtures carry the SHIPPED uninstall graph's step ids and elevations.

import test from "node:test";
import assert from "node:assert/strict";

import { removalRows, type RowStep } from "../src/install/removalPreview.js";

function step(over: Partial<RowStep> = {}): RowStep {
  return {
    id: "removeCluster",
    description: "Delete the local k3d cluster.",
    action: "run",
    preserved: false,
    elevation: "none",
    shared: false,
    sharedReason: "",
    ...over,
  };
}

/** Partitions a step list exactly as `previewUninstall` does. */
function preview(steps: RowStep[]): { removals: RowStep[]; preserved: RowStep[] } {
  return {
    removals: steps.filter((s) => s.action === "run" && !s.preserved),
    preserved: steps.filter((s) => s.preserved),
  };
}

const HOME = "/home/dev";
const CLUSTER = step({ id: "removeCluster", params: { cluster: "memql-local" } });
const CHECKOUT = step({ id: "removeCheckout", description: "Remove the pinned source checkout.", params: { path: `${HOME}/.memql/stack` } });
const HOSTS = step({ id: "removeHostsBlock", description: "Delete the hosts block.", elevation: "sudo", params: { "hosts-file": "/etc/hosts" } });
const IMAGES = step({ id: "removeImages", description: "Remove local images." });

// -----------------------------------------------------------------------------
// rule 1 -- the order rows are read in
// -----------------------------------------------------------------------------

test("rule 1 -- removals come first, then what is kept, each in the order the preview carries", () => {
  const rows = removalRows(preview([CLUSTER, { ...CHECKOUT, preserved: true }, HOSTS, { ...IMAGES, preserved: true }]), HOME);
  assert.deepEqual(
    rows.map((r) => [r.id, r.kept]),
    [
      ["removeCluster", false],
      ["removeHostsBlock", false],
      ["removeCheckout", true],
      ["removeImages", true],
    ],
  );
});

test("rule 1 -- neither list is re-sorted, whatever order it arrives in", () => {
  // The graph's topological order is NOT the install order reversed, so a
  // plausible-looking sort here would replace a real ordering.
  const rows = removalRows({ removals: [HOSTS, CLUSTER, CHECKOUT], preserved: [] }, HOME);
  assert.deepEqual(
    rows.map((r) => r.id),
    ["removeHostsBlock", "removeCluster", "removeCheckout"],
  );
});

// -----------------------------------------------------------------------------
// rule 2 -- a step with nothing to remove is not a row
// -----------------------------------------------------------------------------

test("rule 2 -- a skipped step with no receipt entry produces no row, wherever it turns up", () => {
  // A skip means the receipt recorded nothing for the install step this one
  // reverses. There is no artifact on the machine, so there is nothing to tell
  // the operator about -- not as a removal and not as something kept.
  const skipped = step({ id: "removeHostsBlock", action: "skip" });
  assert.deepEqual(
    removalRows(preview([CLUSTER, skipped, CHECKOUT]), HOME).map((r) => r.id),
    ["removeCluster", "removeCheckout"],
  );
  assert.deepEqual(
    removalRows({ removals: [CLUSTER, skipped], preserved: [] }, HOME).map((r) => r.id),
    ["removeCluster"],
  );
});

// -----------------------------------------------------------------------------
// rule 3 -- the preview alone, never the graph behind it
// -----------------------------------------------------------------------------

test("rule 3 -- a step the preview does not carry produces no row", () => {
  // A machine where only the cluster and the checkout were ever installed gets
  // two rows; an itemization built from the graph would offer to edit a hosts
  // file this install never touched.
  const rows = removalRows(preview([CLUSTER, CHECKOUT]), HOME);
  assert.equal(rows.length, 2);
  for (const row of rows) assert.doesNotMatch(`${row.name} ${row.detail}`, /hosts|address/i);
  assert.deepEqual(removalRows({ removals: [], preserved: [] }, HOME), []);
});

// -----------------------------------------------------------------------------
// rule 4 -- a kept row always says why
// -----------------------------------------------------------------------------

test("rule 4 -- every kept row carries a reason, and no removal invents one", () => {
  // A kept row with no reason reads as a removal that quietly failed.
  const kept = removalRows(preview([CLUSTER, CHECKOUT, IMAGES].map((s) => ({ ...s, preserved: true }))), HOME);
  assert.equal(kept.length, 3);
  for (const row of kept) {
    assert.equal(row.kept, true);
    assert.ok(row.reason.length > 0, `${row.id} is kept with no reason`);
  }
  for (const row of removalRows(preview([CLUSTER, CHECKOUT, IMAGES]), HOME)) assert.equal(row.reason, "");
});

test("what a removal asks for is said, and a kept row asks for nothing", () => {
  const [hosts] = removalRows(preview([HOSTS]), HOME);
  assert.equal(hosts?.asks, "password");
  const [kept] = removalRows(preview([{ ...HOSTS, preserved: true }]), HOME);
  assert.equal(kept?.asks, undefined);
});
