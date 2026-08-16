// The moment an install finishes.
//
// Without this the install is a success nobody can use: the graph builds a k3d
// cluster, a hosts entry and a trust-store CA, the run ends, and the Clusters
// tree -- which reads clusters.yaml and nothing else -- shows exactly what it
// showed before. The machine changed and the editor did not.
//
// Every effect is injected, so the ORDER is what these cases assert. An order is
// only worth writing down if it can be tested.

import test from "node:test";
import assert from "node:assert/strict";

import {
  completeInstallHandoff,
  defaultClusterName,
  installedClusterEntry,
  type HandoffEffects,
} from "../src/install/handoff.js";
import type { ClusterUpdate } from "../src/clusters/file.js";

function recorder(over: Partial<HandoffEffects> = {}): {
  effects: HandoffEffects;
  order: string[];
  written: ClusterUpdate[];
} {
  const order: string[] = [];
  const written: ClusterUpdate[] = [];
  const effects: HandoffEffects = {
    write: async (u) => {
      order.push("write");
      written.push(u);
    },
    invalidatePresence: () => void order.push("invalidate"),
    refreshTree: () => void order.push("refresh"),
    // The WHOLE CONFIG now, not the name (znasllc-io#3905): the panel used to
    // build `{name, endpoint: ""}` here and the selection command dials what it
    // is handed, so a successful install ended by reporting "not configured.
    // Set an endpoint" about the cluster it had just written -- and withheld the
    // "Sign in" button, since `notConfigured` is not credential-recoverable.
    // Recording the endpoint too is what makes that regression visible here.
    select: async (c) => void order.push(`select:${c.name}@${c.endpoint}`),
    ...over,
  };
  return { effects, order, written };
}

// -----------------------------------------------------------------------------
// the entry
// -----------------------------------------------------------------------------

test("the entry is marked local, which is what earns it the uninstall action", () => {
  // The one field that cannot be inferred later. It makes the tree render this
  // as a local cluster and scopes the uninstall menu entry to it; a remote
  // registration never sets it, so neither kind claims the other's status.
  const entry = installedClusterEntry({ domain: "memql.localhost" });
  assert.equal(entry.local, true);
});

test("the endpoint follows the same convention the rest of the extension composes", () => {
  const entry = installedClusterEntry({ domain: "memql.localhost" });
  assert.equal(entry.endpoint, "api.memql.localhost:443");
  assert.equal(entry.domain, "memql.localhost");
});

test("no issuer is written -- it is derivable, and storing it would override discovery", () => {
  const entry = installedClusterEntry({ domain: "memql.localhost" });
  assert.equal(entry.issuer, undefined);
});

test("the name is the domain's first label, the way an operator says it", () => {
  assert.equal(defaultClusterName("memql.localhost"), "memql");
  assert.equal(defaultClusterName("staging.example.com"), "staging");
});

test("a name is never empty, because an empty one orphans the entry", () => {
  // upsertCluster rejects an empty name outright: every other reference
  // resolves a node BY name, so deleting it does not clear a value, it makes
  // the whole node unreachable.
  assert.equal(defaultClusterName(""), "local");
  assert.equal(defaultClusterName("..."), "local");
  assert.notEqual(installedClusterEntry({ domain: "" }).name, "");
});

test("a supplied name wins over the derived one", () => {
  assert.equal(installedClusterEntry({ domain: "memql.localhost", name: "parity" }).name, "parity");
});

test("surrounding dots and whitespace in a domain do not reach the endpoint", () => {
  const entry = installedClusterEntry({ domain: "  .memql.localhost.  " });
  assert.equal(entry.endpoint, "api.memql.localhost:443");
});

// -----------------------------------------------------------------------------
// the hand-off
// -----------------------------------------------------------------------------

test("the registry is written, then the tree is repainted, then the row is selected", () => {
  // Selecting a row the tree has not drawn yet is a no-op the operator reads as
  // "it installed and then nothing happened".
  const { effects, order } = recorder();
  return completeInstallHandoff({ domain: "memql.localhost" }, effects).then(() => {
    assert.deepEqual(order, [
      "write",
      "invalidate",
      "refresh",
      // The endpoint reaches the selection, rather than an empty placeholder.
      "select:memql@api.memql.localhost:443",
    ]);
  });
});

test("a successful hand-off reports the cluster and that sign-in is reachable", async () => {
  const { effects, written } = recorder();
  const result = await completeInstallHandoff({ domain: "memql.localhost" }, effects);

  assert.equal(result.ok, true);
  assert.equal(written.length, 1);
  assert.equal(written[0]?.name, "memql");
  if (result.ok) {
    assert.equal(result.cluster.local, true);
    assert.equal(result.canSignIn, true, "a domain is all identityBaseUrlFor needs");
  }
});

test("re-running an install updates the entry instead of refusing it", async () => {
  // The acceptance criterion, and the reason the effect is `upsertCluster` and
  // not `addCluster`: a repair, or a second run after a partial one, is an
  // ordinary thing to do, and addCluster throws on a duplicate name by design.
  // A caller wiring addCluster in here would pass every other test in this file
  // and fail on the second install.
  const seen: ClusterUpdate[] = [];
  const { effects } = recorder({
    write: async (u) => {
      seen.push(u);
    },
  });

  await completeInstallHandoff({ domain: "memql.localhost" }, effects);
  await completeInstallHandoff({ domain: "memql.localhost" }, effects);

  assert.equal(seen.length, 2, "the second run must write, not throw");
  assert.deepEqual(seen[0], seen[1], "and must write the same entry");
});

test("a failed registry write still says where the cluster answers", async () => {
  // A failed write is NOT a failed install. The cluster exists; only the record
  // of it is missing. A bare error would imply ten minutes of work were wasted.
  const { effects } = recorder({
    write: async () => {
      throw new Error("clusters.yaml is read-only");
    },
  });

  const result = await completeInstallHandoff({ domain: "memql.localhost" }, effects);

  assert.equal(result.ok, false);
  if (!result.ok) {
    assert.equal(result.reachableAt, "api.memql.localhost:443");
    // Compared against the result's OWN field, not a literal address.
    //
    // Two rewrites got here. A regex over the hostname was
    // js/regex/missing-regexp-anchor (matches anywhere); `includes` against a
    // string literal was js/incomplete-url-substring-sanitization (a substring
    // test on a URL is not a host check). Both flags are false positives here
    // -- this is a message assertion, not sanitization -- but both were right
    // about the SHAPE, and a third spelling of "compare a URL to a constant"
    // would just find a third rule.
    //
    // The requirement was never "the message contains this address". It is "the
    // message says where the cluster answers", and `reachableAt` is where that
    // answer lives. Asserting one field of the result against another states it
    // exactly, and no analyser reads it as validating a host.
    assert.ok(
      result.message.includes(result.reachableAt),
      `the message must say where the cluster answers: ${result.message}`,
    );
    assert.match(result.message, /read-only/, "the underlying cause survives");
  }
});

test("presence is invalidated even when the write fails", async () => {
  // The subtle one. The memo answers "is there a local cluster on this
  // machine", and a machine that just had one built now has one whether or not
  // the registry records it. A stale `absent` verdict would keep the "+"
  // offering to install a SECOND cluster over the top of the one that just
  // succeeded -- the exact destruction the evidence pass exists to prevent.
  const { effects, order } = recorder({
    write: async () => {
      throw new Error("nope");
    },
  });

  await completeInstallHandoff({ domain: "memql.localhost" }, effects);
  assert.ok(order.includes("invalidate"), `presence was never invalidated: ${order.join(", ")}`);
});

test("a failed write selects nothing and repaints nothing", async () => {
  const { effects, order } = recorder({
    write: async () => {
      throw new Error("nope");
    },
  });

  await completeInstallHandoff({ domain: "memql.localhost" }, effects);
  assert.ok(!order.some((o) => o.startsWith("select")), "nothing to select");
  assert.ok(!order.includes("refresh"), "nothing changed for the tree to show");
});
