// The uninstall preview, as the rows an operator reads before consenting.
//
// `previewUninstall` (#3469) already decided what will be removed, what will be
// kept, and what has nothing behind it at all. What is asserted here is that
// the mapping to a view model preserves those decisions exactly: it does not
// re-derive the preserved verdict, does not re-order the two lists, does not
// re-render a target, and -- the promise the whole screen exists to make --
// does not quietly drop an artifact.
//
// The rules, one test each:
//   1. removals first, then preserved, each in the order it already carries
//   2. `preserved` alone decides the kind, in both directions
//   3. a step with nothing to remove produces no row
//   4. nothing absent from the preview is ever in the output
//   5. every preserved row carries a reason
//   6. labels name the step and its already-rendered target
//   7. every row says what removing it will ask of the operator
//
// The fixtures are built from the SHIPPED uninstall graph's descriptions, step
// ids AND elevations, so a label asserted here is a label an operator would
// really read and a `sudo` asserted here is the one the graph declares.

import test from "node:test";
import assert from "node:assert/strict";

import { removalPreviewItems, type PreviewInput, type PreviewStep } from "../src/install/removalPreview.js";

function step(over: Partial<PreviewStep> = {}): PreviewStep {
  return {
    id: "removeCluster",
    description: "Delete the local k3d cluster.",
    action: "run",
    reason: "",
    preserved: false,
    target: "cluster memql-local",
    elevation: "none",
    ...over,
  };
}

/** Partitions a step list exactly as `previewUninstall` does. */
function preview(steps: PreviewStep[]): PreviewInput {
  return {
    removals: steps.filter((s) => s.action === "run" && !s.preserved),
    preserved: steps.filter((s) => s.preserved),
  };
}

const CLUSTER = step({ id: "removeCluster", description: "Delete the local k3d cluster.", target: "cluster memql-local" });
const CHECKOUT = step({
  id: "removeCheckout",
  description: "Remove the pinned source checkout.",
  target: "path /home/dev/.memql/stack",
});
// The two privileged steps, with the elevations scripts/install/graph/
// uninstall.json actually declares. They are the reason rule 7 exists.
const HOSTS = step({
  id: "removeHostsBlock",
  description: "Delete the installer's marked block from the system hosts file, leaving every other line alone.",
  target: "hosts-file /etc/hosts",
  elevation: "sudo",
});
const CA = step({
  id: "removeLocalCA",
  description: "Uninstall the local mkcert CA from the system trust stores and delete its key material.",
  target: "caroot /home/dev/.local/share/mkcert",
  elevation: "user-trust",
});
const K3D = step({
  id: "removeToolK3d",
  description: "Remove the installed k3d binary -- after the cluster it manages is gone.",
  target: "path /home/dev/.memql/bin/k3d",
});
const KUBECTL = step({
  id: "removeToolKubectl",
  description: "Remove the installed kubectl binary -- after the cluster it talks to is gone.",
  target: "path /home/dev/.memql/bin/kubectl",
});
const MKCERT = step({
  id: "removeToolMkcert",
  description: "Remove the installed mkcert binary -- after the CA it is needed to uninstall is gone.",
  target: "path /home/dev/.memql/bin/mkcert",
});

/** The shipped uninstall graph, in its own topological order. */
function fullPreview(): PreviewInput {
  return preview([CLUSTER, CHECKOUT, HOSTS, CA, K3D, KUBECTL, MKCERT]);
}

// -----------------------------------------------------------------------------
// rule 1 -- the order rows are read in
// -----------------------------------------------------------------------------

test("rule 1 -- removals come first, in the order the preview already carries", () => {
  // The graph's topological order, which is NOT the install order reversed:
  // mkcert outlives the CA it is needed to uninstall.
  const items = removalPreviewItems(fullPreview());
  assert.deepEqual(
    items.map((i) => i.id),
    ["removeCluster", "removeCheckout", "removeHostsBlock", "removeLocalCA", "removeToolK3d", "removeToolKubectl", "removeToolMkcert"],
  );
});

test("rule 1 -- preserved rows follow every removal, in their own order", () => {
  const items = removalPreviewItems(
    preview([
      CLUSTER,
      { ...CHECKOUT, preserved: true },
      HOSTS,
      { ...CA, preserved: true },
      K3D,
    ]),
  );
  assert.deepEqual(
    items.map((i) => [i.id, i.kind]),
    [
      ["removeCluster", "removed"],
      ["removeHostsBlock", "removed"],
      ["removeToolK3d", "removed"],
      ["removeCheckout", "preserved"],
      ["removeLocalCA", "preserved"],
    ],
  );
});

test("rule 1 -- neither list is re-sorted, whatever order it arrives in", () => {
  const items = removalPreviewItems({ removals: [MKCERT, CLUSTER, CHECKOUT], preserved: [] });
  assert.deepEqual(
    items.map((i) => i.id),
    ["removeToolMkcert", "removeCluster", "removeCheckout"],
  );
});

// -----------------------------------------------------------------------------
// rule 2 -- `preserved` decides the kind, and nothing else does
// -----------------------------------------------------------------------------

test("rule 2 -- preserved false is REMOVED", () => {
  const items = removalPreviewItems(preview([step({ preserved: false })]));
  assert.equal(items.length, 1);
  assert.equal(items[0].kind, "removed");
});

test("rule 2 -- preserved true is PRESERVED", () => {
  // The load-bearing direction. remove-artifact.sh refuses unconditionally on
  // --pre-existing=true, so a row promising to delete this would be lying about
  // a developer's own k3d cluster. #3469 resolved the verdict; this mapping
  // copies it rather than working it out a second time.
  const items = removalPreviewItems(preview([step({ preserved: true })]));
  assert.equal(items.length, 1);
  assert.equal(items[0].kind, "preserved");
});

test("rule 2 -- a preserved step is preserved even when it is listed as a removal", () => {
  // The field is the verdict. Which array a step arrived in is not a second
  // opinion this mapping is entitled to prefer.
  const items = removalPreviewItems({ removals: [step({ preserved: true })], preserved: [] });
  assert.equal(items[0].kind, "preserved");
});

// -----------------------------------------------------------------------------
// rule 3 -- a step with nothing to remove is not a row
// -----------------------------------------------------------------------------

test("rule 3 -- a skipped step with no receipt entry produces no item", () => {
  // A skip means the receipt recorded nothing for the install step this one
  // reverses. There is no artifact on the machine, so there is nothing to tell
  // the operator about -- not as a removal and not as something preserved.
  const skipped = step({
    id: "removeHostsBlock",
    action: "skip",
    reason: "the receipt has no entry for hostsBlock",
    preserved: false,
    target: "",
  });
  const items = removalPreviewItems(preview([CLUSTER, skipped, K3D]));
  assert.deepEqual(
    items.map((i) => i.id),
    ["removeCluster", "removeToolK3d"],
  );
});

test("rule 3 -- a skip is omitted even if it turns up inside the removals list", () => {
  const skipped = step({ id: "removeLocalCA", action: "skip", reason: "no entry", preserved: false, target: "" });
  const items = removalPreviewItems({ removals: [CLUSTER, skipped], preserved: [] });
  assert.deepEqual(
    items.map((i) => i.id),
    ["removeCluster"],
  );
});

test("rule 3 -- a preview whose every step is a skip renders as an empty list", () => {
  const items = removalPreviewItems(
    preview([
      step({ id: "removeCluster", action: "skip", reason: "no entry", target: "" }),
      step({ id: "removeLocalCA", action: "skip", reason: "no entry", target: "" }),
    ]),
  );
  assert.deepEqual(items, []);
});

// -----------------------------------------------------------------------------
// rule 4 -- the preview alone, never the graph behind it
// -----------------------------------------------------------------------------

test("rule 4 -- a step the preview does not carry produces no item", () => {
  // The uninstall graph has seven steps. A machine where only the cluster and
  // one binary were ever installed gets two rows -- an itemization built from
  // the graph would offer to delete a trust store this install never touched.
  const items = removalPreviewItems(preview([CLUSTER, K3D]));
  assert.equal(items.length, 2);
  for (const item of items) {
    assert.doesNotMatch(item.label, /mkcert|hosts|checkout/i);
  }
});

test("rule 4 -- an empty preview renders nothing at all", () => {
  assert.deepEqual(removalPreviewItems({ removals: [], preserved: [] }), []);
});

// -----------------------------------------------------------------------------
// rule 5 -- a preserved row always says why
// -----------------------------------------------------------------------------

test("rule 5 -- the preview's own reason is used when it has one", () => {
  const items = removalPreviewItems(
    preview([step({ preserved: true, reason: "the cluster was already on this machine before the install" })]),
  );
  assert.equal(items[0].reason, "the cluster was already on this machine before the install");
});

test("rule 5 -- the fallback fires for a preserved step carrying no reason", () => {
  // A GUARD, not the expected path. previewUninstall now fills a preserved
  // step's reason at the source, so a real preview never reaches this branch.
  // It is tested anyway because the invariant is this function's own: `reason`
  // is typed `string`, an empty one stays representable whatever today's
  // producer does, and a preserved row rendered bare reads as a removal that
  // quietly failed.
  const items = removalPreviewItems(preview([step({ preserved: true, reason: "" }), step({ id: "removeLocalCA", preserved: true, reason: "   " })]));
  assert.equal(items.length, 2);
  for (const item of items) {
    assert.equal(item.reason, "it existed before the install");
  }
});

test("rule 5 -- no preserved row is ever emitted without a reason, and no removal invents one", () => {
  const everythingPreserved = preview([CLUSTER, CHECKOUT, HOSTS, CA, K3D, KUBECTL, MKCERT].map((s) => ({ ...s, preserved: true })));
  const preserved = removalPreviewItems(everythingPreserved);
  assert.equal(preserved.length, 7);
  for (const item of preserved) {
    assert.equal(item.kind, "preserved");
    assert.ok(item.reason && item.reason.length > 0, `${item.id} is preserved with no reason -- that reads as a removal that failed`);
  }
  for (const item of removalPreviewItems(fullPreview())) {
    assert.equal(item.reason, undefined);
  }
});

// -----------------------------------------------------------------------------
// rule 6 -- labels name the step and which artifact it is about
// -----------------------------------------------------------------------------

test("rule 6 -- the label is the description plus the already-rendered target", () => {
  const items = removalPreviewItems(preview([CLUSTER, CHECKOUT, HOSTS, CA]));
  assert.deepEqual(
    items.map((i) => i.label),
    [
      "Delete the local k3d cluster (cluster memql-local)",
      "Remove the pinned source checkout (path /home/dev/.memql/stack)",
      "Delete the installer's marked block from the system hosts file, leaving every other line alone (hosts-file /etc/hosts)",
      "Uninstall the local mkcert CA from the system trust stores and delete its key material (caroot /home/dev/.local/share/mkcert)",
    ],
  );
});

test("rule 6 -- the target is passed through verbatim, never re-rendered", () => {
  // It was phrased off the very params the removal will be given. A second
  // phrasing here is a preview that can name something other than what goes.
  const items = removalPreviewItems(preview([step({ target: "cluster a-name (with parens) and spaces" })]));
  assert.match(items[0].label, /\(cluster a-name \(with parens\) and spaces\)$/);
});

test("rule 6 -- three binaries read as three different rows", () => {
  // Three of the graph's seven steps remove a binary. Without the target the
  // operator cannot tell which file a verdict belongs to.
  const items = removalPreviewItems(preview([K3D, KUBECTL, MKCERT]));
  const labels = items.map((i) => i.label);
  assert.equal(new Set(labels).size, 3, `three binaries collapsed into ${new Set(labels).size} distinct labels: ${labels.join(", ")}`);
  assert.match(labels[0], /\(path \/home\/dev\/\.memql\/bin\/k3d\)$/);
  assert.match(labels[1], /\(path \/home\/dev\/\.memql\/bin\/kubectl\)$/);
  assert.match(labels[2], /\(path \/home\/dev\/\.memql\/bin\/mkcert\)$/);
});

test("rule 6 -- three binaries stay distinguishable even with one shared description", () => {
  const shared = "Remove the installed binary.";
  const items = removalPreviewItems(
    preview([
      step({ id: "removeToolK3d", description: shared, target: "path /home/dev/.memql/bin/k3d" }),
      step({ id: "removeToolKubectl", description: shared, target: "path /home/dev/.memql/bin/kubectl" }),
      step({ id: "removeToolMkcert", description: shared, target: "path /home/dev/.memql/bin/mkcert" }),
    ]),
  );
  assert.equal(new Set(items.map((i) => i.label)).size, 3);
});

test("rule 6 -- a missing target leaves the description alone, with no empty parentheses", () => {
  const items = removalPreviewItems(preview([step({ preserved: true, target: "" }), step({ id: "removeLocalCA", preserved: true, target: "   " })]));
  assert.deepEqual(
    items.map((i) => i.label),
    ["Delete the local k3d cluster", "Delete the local k3d cluster"],
  );
  for (const item of items) {
    assert.doesNotMatch(item.label, /\(\s*\)/);
  }
});

test("rule 6 -- a step with neither words nor target still gets a name", () => {
  // A blank row is an artifact the operator cannot consent to.
  const items = removalPreviewItems(preview([step({ id: "removeMystery", description: "", target: "" })]));
  assert.equal(items[0].label, "removeMystery");
});

// -----------------------------------------------------------------------------
// rule 7 -- every row says what removing it will ask of the operator
// -----------------------------------------------------------------------------

test("rule 7 -- the graph's elevation reaches every row, for all seven steps", () => {
  // The preview IS the confirmation: this list is the only moment consent is
  // given, so "you will be asked for your root password" and "your browsers
  // will be asked to stop trusting a CA" have to be visible BEFORE the run, not
  // discovered as prompts during it.
  const items = removalPreviewItems(fullPreview());
  assert.deepEqual(
    items.map((i) => [i.id, i.elevation]),
    [
      ["removeCluster", "none"],
      ["removeCheckout", "none"],
      ["removeHostsBlock", "sudo"],
      ["removeLocalCA", "user-trust"],
      ["removeToolK3d", "none"],
      ["removeToolKubectl", "none"],
      ["removeToolMkcert", "none"],
    ],
  );
});

test("rule 7 -- `none` is stated, never left absent", () => {
  // The renderer draws NOTHING for an absent elevation and deliberately does
  // not default it to "none", because "I was not told" and "this needs no
  // privileges" are different claims. A row that dropped the value would look
  // exactly like an unprivileged one -- which is the failure this assertion
  // exists to catch, since both render the same and only one is honest.
  for (const item of removalPreviewItems(fullPreview())) {
    assert.ok(
      Object.prototype.hasOwnProperty.call(item, "elevation"),
      `${item.id} carries no elevation key at all -- the renderer would draw no marker and the operator would read that as "this asks for nothing"`,
    );
    assert.notEqual(item.elevation, undefined);
  }
});

test("rule 7 -- a preserved row carries its elevation too", () => {
  // A preserved step still RUNS -- it is the script's refusal on
  // --pre-existing=true that keeps the artifact -- so what it will ask for is
  // the same question it would have asked to remove it.
  const items = removalPreviewItems(preview([{ ...CA, preserved: true }, { ...HOSTS, preserved: true }]));
  assert.deepEqual(
    items.map((i) => [i.id, i.kind, i.elevation]),
    [
      ["removeLocalCA", "preserved", "user-trust"],
      ["removeHostsBlock", "preserved", "sudo"],
    ],
  );
});

test("rule 7 -- the value is copied, never inferred from the step", () => {
  // Nothing here reads the id, the script or the target to work out what a step
  // will need. The graph declares it; a second derivation would be free to
  // disagree with the run, which is the one disagreement this row cannot afford.
  const surprising = step({ id: "removeToolK3d", description: "Remove the installed k3d binary.", elevation: "sudo" });
  assert.equal(removalPreviewItems(preview([surprising]))[0].elevation, "sudo");
});

// -----------------------------------------------------------------------------
// the shape the view kit renders
// -----------------------------------------------------------------------------

test("an item carries exactly the view-model fields, and no others", () => {
  const [removed] = removalPreviewItems(preview([CLUSTER]));
  assert.deepEqual(removed, {
    id: "removeCluster",
    label: "Delete the local k3d cluster (cluster memql-local)",
    kind: "removed",
    elevation: "none",
  });

  const [preserved] = removalPreviewItems(
    preview([{ ...CA, preserved: true, reason: "the CA pre-dates the install" }]),
  );
  assert.deepEqual(preserved, {
    id: "removeLocalCA",
    label:
      "Uninstall the local mkcert CA from the system trust stores and delete its key material (caroot /home/dev/.local/share/mkcert)",
    kind: "preserved",
    reason: "the CA pre-dates the install",
    elevation: "user-trust",
  });
});
