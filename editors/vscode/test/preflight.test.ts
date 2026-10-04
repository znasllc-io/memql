// The checks on the install and repair form (memql#4195).
//
// The wizard knew these facts before starting a run and told nobody until the
// moment each bit. The form states them as a compact list -- a label and one
// word each -- and says nothing about a check that has nothing to say. These
// cases pin both halves: what is said, and what is no longer said.

import test from "node:test";
import assert from "node:assert/strict";

import { preflightChecks } from "../src/state/preflight.js";
import { collectScreen } from "../src/webview/addClusterScreens.js";
import type { Inputs } from "../src/state/addCluster.js";

const EMPTY_INPUTS: Inputs = {
  domain: "memql.localhost",
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  version: "v0.21.3",
};

const GRAPH_OK = { ok: true as const, steps: 12, needsElevation: true };

test("the installer's own files read Ready; missing ones say the fix", () => {
  const ok = preflightChecks({ action: "install", graph: GRAPH_OK, sudoFree: true });
  assert.deepEqual(ok[0], { label: "Installer", word: "Ready", tone: "ok" });

  const bad = preflightChecks({
    action: "install",
    graph: { ok: false, error: "ENOENT: ~/scripts/install/graph/install.json" },
    sudoFree: true,
  });
  assert.equal(bad[0]?.word, "Missing");
  assert.equal(bad[0]?.tone, "error");
  assert.equal(bad[0]?.note, "Reinstall the MemQL extension.");
  // The read error is for the output channel, not the page.
  assert.doesNotMatch(JSON.stringify(bad), /ENOENT|install\.json/);
});

test("the password row appears exactly when a password will be asked for", () => {
  const asks = preflightChecks({ action: "install", graph: GRAPH_OK, sudoFree: false });
  const password = asks.find((c) => c.label === "Your password");
  assert.equal(password?.word, "Needed");
  assert.equal(password?.tone, "attention");
  assert.match(password?.note ?? "", /^Asked once/);

  // Nothing to act on, nothing said: sudo runs without asking, or no step
  // needs it. "No step needs elevation" was a row of OK that taught a reader
  // to skip the list.
  assert.equal(
    preflightChecks({ action: "install", graph: GRAPH_OK, sudoFree: true }).some((c) => c.label === "Your password"),
    false,
  );
  assert.equal(
    preflightChecks({
      action: "install",
      graph: { ok: true, steps: 3, needsElevation: false },
      sudoFree: false,
    }).some((c) => c.label === "Your password"),
    false,
  );
});

test("every state is one word, and the list names no internal", () => {
  for (const sudoFree of [true, false]) {
    for (const imageSource of ["checkout", "released", ""] as const) {
      const checks = preflightChecks({
        action: "repair",
        graph: GRAPH_OK,
        sudoFree,
        imageSource,
        releasedTag: "v0.17.0",
      });
      for (const check of checks) {
        assert.equal(check.word.split(" ").length, 1, `"${check.word}" is not one word`);
        assert.doesNotMatch(
          `${check.label} ${check.word} ${check.note ?? ""}`,
          /graph|sudo|elevation|receipt|steps, loaded|verif/i,
        );
      }
    }
  }
});

test("no action's checks mention an AI credential", () => {
  // There is no key path anywhere in the product: both cloud vendors are
  // reached by workload identity federation (epic memql#5088).
  for (const action of ["install", "repair"] as const) {
    const checks = preflightChecks({ action, graph: GRAPH_OK, sudoFree: false });
    assert.equal(
      checks.some((c) => /key|credential|provider|vendor/i.test(`${c.label} ${c.note ?? ""}`)),
      false,
      `the ${action} checks name an AI credential: ${JSON.stringify(checks)}`,
    );
  }
});

test("a run over a checkout-built cluster says its build is replaced, and how to get it back", () => {
  // THE LANE CROSSING (memql#4246), said before the run rather than discovered
  // afterwards in the Deployments row.
  const crossing = preflightChecks({
    action: "repair",
    graph: GRAPH_OK,
    sudoFree: true,
    imageSource: "checkout",
    releasedTag: "v0.17.0",
  });
  const lane = crossing.find((c) => c.label === "Images");
  assert.equal(lane?.word, "Replaced");
  assert.equal(lane?.tone, "attention");
  assert.match(lane?.note ?? "", /replaced by release v0\.17\.0/);
  assert.match(lane?.note ?? "", /Rebuild from checkout brings it back/);

  // Not a crossing, not a line.
  for (const imageSource of ["released", ""] as const) {
    assert.equal(
      preflightChecks({ action: "repair", graph: GRAPH_OK, sudoFree: true, imageSource, releasedTag: "v0.17.0" }).some(
        (c) => c.label === "Images",
      ),
      false,
    );
  }
});

test("the form draws the checks as a compact list above its action bar", () => {
  const parts = collectScreen({
    action: "install",
    values: EMPTY_INPUTS,
    errors: [],
    versionChoices: [],
    checks: preflightChecks({ action: "install", graph: GRAPH_OK, sudoFree: false }),
    moreOpen: false,
  });
  assert.match(parts.body, /<h2 class="mq-subhead">Checks/);
  assert.match(parts.body, /<dt>Your password<\/dt><dd><span class="ac-word" data-tone="attention">Needed<\/span>/);
  // Install is the one forward act, on the bar and nowhere else.
  assert.match(parts.actions, /data-act="begin"[^>]*>Install</);
  assert.doesNotMatch(parts.body, /data-act="begin"/);
});

test("while the checks are gathered the list is the shape of a list, never words", () => {
  const parts = collectScreen({ action: "install", values: EMPTY_INPUTS, errors: [], versionChoices: [], moreOpen: false });
  assert.match(parts.body, /class="mq-skeleton" data-shape="facts"/);
  assert.doesNotMatch(parts.body, /Checking\.\.\.|Loading/);
  // Install is not held hostage to the checks: the run itself enforces them.
  assert.match(parts.actions, /data-act="begin"/);
});

test("a missing installer takes Install away rather than offering a run that cannot start", () => {
  const parts = collectScreen({
    action: "install",
    values: EMPTY_INPUTS,
    errors: [],
    versionChoices: [],
    checks: preflightChecks({ action: "install", graph: { ok: false, error: "gone" }, sudoFree: true }),
    moreOpen: false,
  });
  assert.doesNotMatch(parts.actions, /data-act="begin"/);
  assert.match(parts.actions, /Can(?:'|&#39;)t start/);
});
