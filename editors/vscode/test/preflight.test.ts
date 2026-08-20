// The "Before it runs" checklist (memql#4195).
//
// The wizard knew these facts before starting a run and told nobody until the
// moment each bit. The checklist words them up front; these cases pin the
// wording decisions and that the collect screen actually renders them.

import test from "node:test";
import assert from "node:assert/strict";

import { preflightItems } from "../src/state/preflight.js";
import { renderCollectScreen } from "../src/webview/installScreens.js";
import type { Inputs } from "../src/state/addCluster.js";

const EMPTY_INPUTS: Inputs = {
  domain: "",
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  provider: "",
  providerKeyFile: "",
  version: "",
};

const GRAPH_OK = { ok: true as const, steps: 12, needsElevation: true };

test("a loadable graph reads quiet; an unreadable one is the first attention item", () => {
  const ok = preflightItems({
    action: "install",
    graph: GRAPH_OK,
    sudoFree: true,
    recordedKeyPath: "",
  });
  assert.equal(ok[0]?.state, "ok");
  assert.match(ok[0]?.detail ?? "", /12 steps/);

  const bad = preflightItems({
    action: "install",
    graph: { ok: false, error: "ENOENT: ~/scripts/install/graph/install.json" },
    sudoFree: true,
    recordedKeyPath: "",
  });
  assert.equal(bad[0]?.state, "attention");
  assert.match(bad[0]?.detail ?? "", /refuse before its first step/);
});

test("elevation is attention exactly when sudo would ask", () => {
  const asks = preflightItems({
    action: "install",
    graph: GRAPH_OK,
    sudoFree: false,
    recordedKeyPath: "",
  });
  const privileges = asks.find((i) => i.label === "Privileges");
  assert.equal(privileges?.state, "attention");
  assert.match(privileges?.detail ?? "", /asked once/);
  assert.match(privileges?.detail ?? "", /held in memory/);

  const free = preflightItems({
    action: "install",
    graph: GRAPH_OK,
    sudoFree: true,
    recordedKeyPath: "",
  });
  assert.equal(free.find((i) => i.label === "Privileges")?.state, "ok");

  const none = preflightItems({
    action: "install",
    graph: { ok: true, steps: 3, needsElevation: false },
    sudoFree: false,
    recordedKeyPath: "",
  });
  assert.equal(none.find((i) => i.label === "Privileges")?.state, "ok");
});

test("a repair states whether the recorded key path is usable", () => {
  const recorded = preflightItems({
    action: "repair",
    graph: GRAPH_OK,
    sudoFree: true,
    recordedKeyPath: "~/keys/anthropic.txt",
  });
  const key = recorded.find((i) => i.label === "Provider key file");
  assert.equal(key?.state, "ok");
  assert.match(key?.detail ?? "", /~\/keys\/anthropic\.txt/);

  const missing = preflightItems({
    action: "repair",
    graph: GRAPH_OK,
    sudoFree: true,
    recordedKeyPath: "",
  });
  assert.equal(missing.find((i) => i.label === "Provider key file")?.state, "attention");
});

test("the collect screen renders the checklist above Start", () => {
  const html = renderCollectScreen({
    action: "install",
    values: EMPTY_INPUTS,
    errors: [],
    preflight: preflightItems({
      action: "install",
      graph: GRAPH_OK,
      sudoFree: false,
      recordedKeyPath: "",
    }),
  });
  assert.match(html, /Before it runs/);
  assert.match(html, /preflight-item attention/);
  assert.ok(
    html.indexOf("Before it runs") < html.indexOf('data-act="begin"'),
    "the checklist renders before the Start button",
  );
});

test("the collect screen still renders while the checklist is being gathered", () => {
  const html = renderCollectScreen({ action: "install", values: EMPTY_INPUTS, errors: [] });
  assert.doesNotMatch(html, /Before it runs/);
  assert.match(html, /data-act="begin"/);
});
