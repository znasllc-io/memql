// What the wizard reads from detect's minimum-requirements report
// (scripts/lib/requirements.sh): the verdict passes through as the script wrote
// it, and anything malformed is ABSENT rather than guessed, so a report the
// wizard cannot read never refuses an install on its own.

import test from "node:test";
import assert from "node:assert/strict";

import { parseRequirements, requirementsShortfallDetail } from "../src/install/platform.js";

const report = {
  meetsMinimum: false,
  meetsRecommended: false,
  checks: [
    { id: "memory", label: "Memory", observed: "24 GB", minimum: "32 GB", recommended: "48 GB", status: "below" },
    { id: "cpu", label: "Processor", observed: "10 cores", minimum: "8 cores", recommended: "", status: "recommended" },
  ],
  shortfalls: ["Memory: 24 GB. The minimum is 32 GB."],
  docs: "https://example.test/minimum-requirements",
};

test("a detect result's requirements pass through as written", () => {
  const parsed = parseRequirements({ supported: true, requirements: report });
  assert.ok(parsed);
  assert.equal(parsed.meetsMinimum, false);
  assert.equal(parsed.meetsRecommended, false);
  assert.deepEqual(parsed.shortfalls, report.shortfalls);
  assert.deepEqual(
    parsed.checks.map((c) => [c.id, c.status]),
    [
      ["memory", "below"],
      ["cpu", "recommended"],
    ],
  );
  assert.equal(parsed.docs, report.docs);
});

test("a detect that predates the report, or a malformed one, is absent", () => {
  assert.equal(parseRequirements(undefined), undefined);
  assert.equal(parseRequirements({ supported: true }), undefined);
  assert.equal(parseRequirements({ requirements: "no" }), undefined);
  assert.equal(parseRequirements({ requirements: { meetsMinimum: "false", meetsRecommended: false } }), undefined);
});

test("a check with a status this side does not know is dropped, not coerced", () => {
  const parsed = parseRequirements({
    requirements: { ...report, checks: [...report.checks, { id: "x", label: "X", status: "maybe" }] },
  });
  assert.deepEqual(
    parsed?.checks.map((c) => c.id),
    ["memory", "cpu"],
  );
});

test("the modal lists the script's sentences, then why and what is still possible", () => {
  const detail = requirementsShortfallDetail(parseRequirements({ requirements: report })!);
  const lines = detail.split("\n");
  assert.equal(lines[0], "Memory: 24 GB. The minimum is 32 GB.");
  assert.match(detail, /runs the whole cluster and its AI models on this computer/);
  assert.match(detail, /connect to a cluster that runs somewhere else/);
});
