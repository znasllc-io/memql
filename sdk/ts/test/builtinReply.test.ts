// A top-level `builtin X(...)` reply is ONE data value keyed by node id
// (memql#4895 follow-up). The engine returns the handler's node map from
// executor.go's BuiltinFunctionExpression arm, ToAPIResult marshals that map
// as a single structpb.Value, and the wire carries
//
//   data: [ { "<id>": { id, concept, createdAt, payload: {...} }, ... } ]
//
// -- never one row per node. Result.rows() used to hand that wrapper back as
// a single "row", so every consumer reading a builtin reply through rows()
// (the OS Logs app, GitHub Connect, the probes) read an object with no
// payload field on it and rendered nothing. These pin the unwrap, and the
// order: the engine stamps PreserveOrder handlers' nodes with monotonically
// DECREASING createdAt in slice order (executor_builtin.go), so createdAt
// descending IS the handler's order, and it has to be compared at nanosecond
// precision because Date.parse truncates to milliseconds.

import test from "node:test";
import assert from "node:assert/strict";

import { Result } from "../src/client/types.js";

// Verbatim from a production bff answering `builtin logsTail(limit: 2)` on
// 2026-09-03 (ids and the dedupKey shortened). The handler's slice was
// [older, newer] -- oldest first, the tail's contract -- and the stamps say
// so: the older line carries the LATER createdAt.
const TAIL_REPLY = {
  data: [
    {
      "1dc7219d-ba45-4e1b-b8f1-0c967f157eb9": {
        concept: "v1:observability:logLine",
        createdAt: "2026-09-03T20:09:48.145304797Z",
        createdBy: "",
        id: "1dc7219d-ba45-4e1b-b8f1-0c967f157eb9",
        payload: {
          app: "",
          attributes: { automation: "datasync.drain", node: "agent-844fb847cb-jhctn" },
          component: "MemQL",
          id: "1dc7219d-ba45-4e1b-b8f1-0c967f157eb9",
          level: "warn",
          message: "duplicate automation execution prevented by cluster guard",
          node: "agent-844fb847cb-jhctn",
          nodeType: "agent",
          occurredAt: "2026-09-03T20:09:43.652801Z",
        },
        provenance: null,
        schema: null,
        type: "object",
      },
      "73bab17d-3777-44f6-914d-b9acd0052c23": {
        concept: "v1:observability:logLine",
        createdAt: "2026-09-03T20:09:48.145304796Z",
        createdBy: "",
        id: "73bab17d-3777-44f6-914d-b9acd0052c23",
        payload: {
          app: "",
          component: "MemQL",
          id: "73bab17d-3777-44f6-914d-b9acd0052c23",
          level: "warn",
          message: "duplicate automation execution prevented by cluster guard",
          node: "cognition-5f4c9bf9c-kxv4n",
          nodeType: "cognition",
          occurredAt: "2026-09-03T20:09:43.937196Z",
        },
        provenance: null,
        schema: null,
        type: "object",
      },
    },
  ],
  meta: { tookMs: "4" },
};

test("rows() -- a builtin's id-keyed node map unwraps to one flattened row per node", () => {
  const rows = new Result(TAIL_REPLY as never).rows();
  assert.equal(rows.length, 2);
  // Payload fields sit at the top level, the way a bundle node's do.
  assert.equal(rows[0]?.occurredAt, "2026-09-03T20:09:43.652801Z");
  assert.equal(rows[0]?.level, "warn");
  assert.equal(rows[0]?.concept, "v1:observability:logLine");
  assert.equal(rows[1]?.id, "73bab17d-3777-44f6-914d-b9acd0052c23");
});

test("rows() -- the handler's order survives: createdAt descending at nanosecond precision", () => {
  const rows = new Result(TAIL_REPLY as never).rows();
  // JSON key order is id-lexicographic ("1dc7…" < "73ba…") and both stamps
  // share one millisecond, so only a sub-millisecond comparison keeps the
  // older line first.
  assert.deepEqual(
    rows.map((r) => r.occurredAt),
    ["2026-09-03T20:09:43.652801Z", "2026-09-03T20:09:43.937196Z"],
  );
});

test("rows() -- a single reply node unwraps the same way", () => {
  const reply = {
    data: [
      {
        githubConnectBegin: {
          concept: "integration:identity:githubConnectBegin",
          createdAt: "0001-01-01T00:00:00Z",
          id: "githubConnectBegin",
          payload: { authorizeUrl: "", installUrl: "", reason: "github_app_not_configured" },
          type: "object",
        },
      },
    ],
  };
  const rows = new Result(reply as never).rows();
  assert.equal(rows.length, 1);
  assert.equal(rows[0]?.reason, "github_app_not_configured");
  assert.equal(rows[0]?.id, "githubConnectBegin");
});

test("rows() -- a flat data row (a logic's object-literal return) is left exactly as it was", () => {
  const rows = new Result({ data: [{ id: "u1", name: "x" }, { count: 3 }] } as never).rows();
  assert.deepEqual(rows, [{ id: "u1", name: "x" }, { count: 3 }]);
});

test("rows() -- an object whose values are not node envelopes is not mistaken for one", () => {
  // A nested object with the wrong shape (no concept) stays a single row.
  const rows = new Result({ data: [{ a: { id: "a", value: 1 }, b: { id: "b", value: 2 } }] } as never).rows();
  assert.equal(rows.length, 1);
  assert.deepEqual(Object.keys(rows[0] ?? {}), ["a", "b"]);
});

test("rows() -- a nanosecond stamp trimmed by Go's RFC3339Nano still sorts by instant", () => {
  // Go trims trailing zeros, so "…1453048Z" is LATER than "…145304797Z"
  // despite being the shorter (and lexicographically smaller) string.
  const reply = {
    data: [
      {
        a: { id: "a", concept: "x", createdAt: "2026-09-03T20:09:48.145304797Z", payload: { n: 1 } },
        b: { id: "b", concept: "x", createdAt: "2026-09-03T20:09:48.1453048Z", payload: { n: 2 } },
      },
    ],
  };
  const rows = new Result(reply as never).rows();
  assert.deepEqual(rows.map((r) => r.id), ["b", "a"]);
});
