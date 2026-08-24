// The two context keys, as a table (memql#4424).
//
// WHY A TABLE TEST AND NOT A HOST TEST. What these keys decide is which
// `viewsWelcome` renders and which title-menu entries appear, and no API in an
// Extension Development Host reads back either -- the same wall
// test/clusterMenus.test.ts documents for context menus. What CAN be checked,
// and is the whole of the decision, is the mapping from a ConnectionState to a
// pair of booleans. It is pure, it has four inputs, and every one of them is
// driven here.
//
// THE CASE WORTH THE FILE is `error`. A cluster that was selected and did not
// answer is `clusterSelected: true`, and folding it in with `disconnected`
// would replace a cluster that is DOWN with three views saying nothing is
// chosen -- which sends an operator to the cluster picker to re-select the
// cluster they already have. That is design D2's "selected-but-unreachable is
// NOT the empty state", and it lives or dies on one boolean.
//
// Refs: #4424 #4423

import test from "node:test";
import assert from "node:assert/strict";

import type { ConnectionState } from "../src/connection/manager.js";
import {
  CLUSTER_SELECTED_KEY,
  CONNECTED_KEY,
  NOT_CONNECTED_REFUSAL,
  connectionContextKeys,
} from "../src/state/connectionContext.js";

const CASES: ReadonlyArray<{
  what: string;
  state: ConnectionState;
  clusterSelected: boolean;
  connected: boolean;
}> = [
  {
    what: "nothing has been selected",
    state: { status: "disconnected" },
    clusterSelected: false,
    connected: false,
  },
  {
    what: "a cluster is being dialled",
    state: { status: "connecting", clusterName: "local" },
    clusterSelected: true,
    connected: false,
  },
  {
    what: "a cluster is held",
    state: { status: "connected", clusterName: "local", nodeId: "node-1" },
    clusterSelected: true,
    connected: true,
  },
  {
    what: "the dial was refused",
    state: {
      status: "error",
      clusterName: "staging",
      message: "no route to host",
      reason: "unreachable",
    },
    clusterSelected: true,
    connected: false,
  },
  {
    what: "the connection was lost",
    state: {
      status: "error",
      clusterName: "staging",
      message: "Connection to staging was lost.",
      reason: "lost",
    },
    clusterSelected: true,
    connected: false,
  },
  {
    what: "the credential expired",
    state: {
      status: "error",
      clusterName: "staging",
      message: "the access token is expired",
      reason: "credentialExpired",
    },
    clusterSelected: true,
    connected: false,
  },
];

for (const row of CASES) {
  test(`${row.what} -> clusterSelected=${row.clusterSelected} connected=${row.connected}`, () => {
    assert.deepEqual(connectionContextKeys(row.state), {
      clusterSelected: row.clusterSelected,
      connected: row.connected,
    });
  });
}

test("only the disconnected state empties the cluster-backed views", () => {
  // Stated as its own claim rather than left to be read out of the table above,
  // because it is the property the welcomes rest on: exactly one of the four
  // states may render "Not connected. Select a cluster.", and any future state
  // that joined it would empty a view over a cluster the editor is holding.
  const empties = CASES.filter((row) => !row.clusterSelected).map((row) => row.state.status);
  assert.deepEqual(empties, ["disconnected"]);
});

test("connected is never true without clusterSelected", () => {
  // The pair is ordered: a transport cannot be up for a cluster that was never
  // chosen. A manifest clause reading `memql.connected && !memql.clusterSelected`
  // must be unsatisfiable, and this is what makes it so.
  for (const row of CASES) {
    if (row.connected) assert.equal(row.clusterSelected, true, row.what);
  }
});

test("the key names are the ones the manifest is keyed on", () => {
  // A typo here is invisible in a running editor: VS Code treats an unknown
  // context key as unset, so `!memql.clusterSelcted` is permanently true and
  // its welcome renders over a connected cluster with nothing complaining.
  // test/viewsWelcome.test.ts asserts the manifest side against these same two
  // constants, which is what makes the pair a single fact.
  assert.equal(CLUSTER_SELECTED_KEY, "memql.clusterSelected");
  assert.equal(CONNECTED_KEY, "memql.connected");
});

test("the refusal is one sentence, shared by the welcomes and by runs.execute", () => {
  // Design D2's Runs exception refuses with it at execution time; the three
  // welcomes open with the same words. Two spellings would be two refusals an
  // operator has to learn are the same one.
  assert.equal(NOT_CONNECTED_REFUSAL, "Not connected. Select a cluster first.");
  assert.ok(NOT_CONNECTED_REFUSAL.startsWith("Not connected."));
});
