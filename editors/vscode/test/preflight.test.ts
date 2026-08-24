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

test("a run over a checkout-mode cluster says it returns to released images", () => {
  // THE OTHER HALF OF THE LANE STATEMENT (memql#4246). The rebuild preflight
  // says a rebuild switches to checkout-built images; this says what the three
  // released-lane verbs switch back, and it is said BEFORE the run rather than
  // discovered afterwards in the Deployments row. Crossing a lane is never
  // silent in either direction.
  const crossing = preflightItems({
    action: "repair",
    graph: GRAPH_OK,
    sudoFree: true,
    recordedKeyPath: "~/keys/anthropic.txt",
    imageSource: "checkout",
    releasedTag: "v0.17.0",
  });
  const lane = crossing.find((i) => i.label === "Image source");
  assert.equal(lane?.state, "attention");
  assert.match(lane?.detail ?? "", /returns local to released v0\.17\.0 images/);
  assert.match(lane?.detail ?? "", /Rebuild from checkout brings them back/);

  // A cluster already on released images is not crossing anything, so nothing
  // is said: a line that appeared on every install would be noise, and noise is
  // what makes the one that matters unreadable.
  assert.equal(
    preflightItems({
      action: "repair",
      graph: GRAPH_OK,
      sudoFree: true,
      recordedKeyPath: "",
      imageSource: "released",
      releasedTag: "v0.17.0",
    }).some((i) => i.label === "Image source"),
    false,
  );
  // And an install on a machine whose image source is unknown says nothing
  // either -- "" is no evidence, never "released".
  assert.equal(
    preflightItems({
      action: "install",
      graph: GRAPH_OK,
      sudoFree: true,
      recordedKeyPath: "",
    }).some((i) => i.label === "Image source"),
    false,
  );
});

test("the collect screen renders the checklist above the form it warns about", () => {
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
  // RE-EXPRESSED BY memql#4453, and the intent it protects is unchanged.
  //
  // This used to assert `checklist < Start`, because the checklist was placed
  // directly above a Start button that sat at the BOTTOM of the form -- so
  // "above Start" and "above the fields" were the same position. Actions-first
  // separated them: Start is now the first thing on the page, and the choice
  // was whether the checklist follows it up or stays behind among the fields.
  //
  // It follows it up. The thing #4195 wanted was that an operator has read what
  // the run will need BEFORE they commit to it, and the checklist is now in the
  // first screenful instead of at the end of a seven-field form -- which is a
  // better guarantee of that than the old ordering gave, on a form long enough
  // to scroll. What is asserted is therefore that it precedes the fields.
  assert.ok(
    html.indexOf("Before it runs") < html.indexOf('data-field="domain"'),
    "the checklist must render above the form whose answers it qualifies",
  );
  // And it is in the STATUS area, not stranded below the details.
  assert.ok(
    html.indexOf('data-act="begin"') < html.indexOf("Before it runs"),
    "the actions row comes first on every screen (memql#4453)",
  );
});

test("the collect screen still renders while the checklist is being gathered", () => {
  const html = renderCollectScreen({ action: "install", values: EMPTY_INPUTS, errors: [] });
  assert.doesNotMatch(html, /Before it runs/);
  assert.match(html, /data-act="begin"/);
});
