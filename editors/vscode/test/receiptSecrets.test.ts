// The install receipt must not carry a live credential (memql#3908).
//
// `~/.memql/install-receipt.json` stored every step's `result` verbatim, and two
// steps return credentials: `enrolment-link.sh` puts an `mql_enr_` enrolment URL
// on `result.enrolUrl`, `magic-link.sh` puts the owner's single-use sign-in link
// on `result.link`. So every install and every repair wrote a plaintext
// single-use credential into a file that is kept for the life of the install and
// never rewritten -- and nothing ever read either field back.
//
// memql#3886 built the withholding machinery for the RUN LOG and its own comment
// names `enrolUrl` as a field that must not survive. This is the same argument
// applied to the longer-lived file.
//
// THE HARD PART IS NOT THE WITHHOLDING, IT IS NOT BREAKING THE UNINSTALL.
// Receipt results are read programmatically -- `recordedTarget` pulls
// `entry.result[target.resultField]` to tell remove-artifact.sh where an
// artifact is -- so a false positive does not cost a log line, it strands a
// cluster or a CA on the machine with nothing that knows it is there. Both
// directions are asserted below, and the second one structurally.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  WITHHELD,
  withholdResult,
  withholdsFromReceipt,
  withholdsFromRunLog,
} from "../src/install/secrets.js";
import { appendReceiptEntry, readReceipt, removalParams, type ReceiptEntry } from "../src/install/receipt.js";

function entry(over: Partial<ReceiptEntry> = {}): ReceiptEntry {
  return {
    stepId: "step",
    script: "install.step",
    receipt: "",
    preExisting: false,
    params: {},
    result: {},
    changed: true,
    recordedAt: "2026-08-16T00:00:00.000Z",
    ...over,
  };
}

// ---------------------------------------------------------------------------
// the credential must not survive the write
// ---------------------------------------------------------------------------

test("a receipt written by a run carrying both link steps holds neither URL", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-receipt-"));
  const file = path.join(dir, "install-receipt.json");
  try {
    // The shape the two scripts emit on their success paths. The token BODIES
    // are deliberately low-entropy placeholders rather than realistic random
    // strings: what every assertion below keys on is the URL prefix and the
    // `mql_enr_` marker, and a realistic body is a secret-shaped literal in a
    // tracked file -- which the repository's own gitleaks gate flags, correctly.
    await appendReceiptEntry(file, "install", entry({
      stepId: "magicLink",
      script: "install.magicLink",
      result: {
        context: "k3d-memql",
        namespace: "memql",
        email: "owner@example.test",
        linkState: "recovered",
        link: "https://identity.memql.localhost/auth/complete?token=PLACEHOLDER-NOT-A-TOKEN",
        candidates: 1,
      },
    }));
    await appendReceiptEntry(file, "install", entry({
      stepId: "enrolmentLink",
      script: "install.enrolmentLink",
      result: {
        context: "k3d-memql",
        namespace: "memql",
        target: "identity",
        email: "owner@example.test",
        enrolUrl: "https://identity.memql.localhost/enroll?code=mql_enr_PLACEHOLDER-NOT-A-TOKEN",
        ownerClaimed: true,
        enrolmentState: "minted",
      },
    }));

    // Asserted against the BYTES, not the parsed object: what matters is what is
    // sitting on disk for the life of the install.
    const raw = await fs.readFile(file, "utf8");
    assert.equal(raw.includes("mql_enr_"), false, "the enrolment token is in the receipt file");
    assert.equal(raw.includes("/auth/complete?token="), false, "the magic link is in the receipt file");
    assert.equal(raw.includes("/enroll?code="), false, "the enrolment URL is in the receipt file");

    const receipt = await readReceipt(file);
    assert.ok(receipt);
    const magic = receipt.entries.find((e) => e.stepId === "magicLink");
    const enrol = receipt.entries.find((e) => e.stepId === "enrolmentLink");
    assert.equal(magic?.result.link, WITHHELD);
    assert.equal(enrol?.result.enrolUrl, WITHHELD);

    // The STATUS fields survive, which is memql#3886's whole point: they are
    // the facts a stuck operator needs, and they are states not credentials.
    assert.equal(magic?.result.linkState, "recovered");
    assert.equal(enrol?.result.enrolmentState, "minted");
    assert.equal(enrol?.result.ownerClaimed, true);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("withholding leaves every other value in its original JSON type", () => {
  const out = withholdResult({
    cluster: "k3d-memql",
    clusterCreated: true,
    replicas: 2,
    hostnames: ["api.memql.localhost", "identity.memql.localhost"],
    pins: { k3d: "v5.6.0" },
    caroot: "/home/operator/.memql/mkcert",
    enrolUrl: "https://identity.memql.localhost/enroll?code=mql_enr_PLACEHOLDER",
  });

  assert.equal(typeof out.cluster, "string");
  assert.equal(out.clusterCreated, true, "a boolean must not become the string \"true\"");
  assert.equal(typeof out.clusterCreated, "boolean");
  assert.equal(out.replicas, 2);
  assert.equal(typeof out.replicas, "number");
  assert.deepEqual(out.hostnames, ["api.memql.localhost", "identity.memql.localhost"]);
  assert.deepEqual(out.pins, { k3d: "v5.6.0" });
  assert.equal(out.caroot, "/home/operator/.memql/mkcert");
  assert.equal(out.enrolUrl, WITHHELD);
});

test("an empty credential field is still withheld rather than dropped", () => {
  // enrolment-link.sh reports enrolUrl:"" on the awaiting-first-sign-in path.
  // The KEY must survive so a reader can still see the step reported the field.
  const out = withholdResult({ enrolUrl: "", enrolmentState: "awaitingFirstSignIn" });
  assert.ok("enrolUrl" in out);
  assert.equal(out.enrolUrl, WITHHELD);
  assert.equal(out.enrolmentState, "awaitingFirstSignIn");
});

// ---------------------------------------------------------------------------
// and the uninstall must still find its artifacts
// ---------------------------------------------------------------------------

// STRUCTURAL, not a list. Every artifact class the uninstall knows about is
// exercised with a value of the shape that class actually carries, so a NEW
// removal target whose field name happens to end in a credential word --
// `downloadUrl`, `installToken` -- fails here at authoring time instead of
// silently skipping its removal step on somebody's machine.
const REMOVAL_SHAPES: Array<{ kind: string; field: string; value: string }> = [
  { kind: "binary", field: "path", value: "/home/operator/.memql/bin/memql" },
  { kind: "checkout", field: "dest", value: "/home/operator/.memql/src" },
  { kind: "hostsEntries", field: "hostsFile", value: "/etc/hosts" },
  { kind: "mkcertCA", field: "caroot", value: "/home/operator/.memql/mkcert" },
  { kind: "stack", field: "cluster", value: "k3d-memql" },
];

test("no uninstall removal target is withheld", () => {
  for (const shape of REMOVAL_SHAPES) {
    assert.equal(
      withholdsFromReceipt(shape.field, shape.value),
      false,
      `${shape.kind} records its removal target on result.${shape.field}; withholding it makes the uninstall skip the step and strand the artifact`,
    );
  }
});

test("a removal target survives a path longer than the run log's length cap", () => {
  // THE REASON withholdsFromReceipt IS NOT withholdsFromRunLog. The run log
  // drops any string over 96 characters as "nobody reads this as a status" --
  // true of a history somebody skims, and false of a path the uninstall needs.
  const deep = `/home/operator/${"nested-project-directory/".repeat(5)}memql/src`;
  assert.ok(deep.length > 96, "fixture must actually exceed the cap to prove anything");

  assert.equal(withholdsFromRunLog("dest", deep), true, "the run log still drops it, deliberately");
  assert.equal(withholdsFromReceipt("dest", deep), false, "the receipt must keep it");

  const kept = withholdResult({ dest: deep });
  const params = removalParams(entry({ receipt: "checkout", result: kept }));
  assert.ok(params, "removalParams returned null: the uninstall would skip and strand the checkout");
  assert.equal(params.path, deep);
});

test("the two predicates still agree about an actual credential", () => {
  const cases: Array<[string, unknown]> = [
    ["enrolUrl", "https://identity.memql.localhost/enroll?code=mql_enr_x"],
    ["link", "https://identity.memql.localhost/auth/complete?token=PLACEHOLDER"],
    ["apiKey", "sk-ant-PLACEHOLDER"],
    ["token", "short"],
  ];
  for (const [name, value] of cases) {
    assert.equal(withholdsFromReceipt(name, value), true, `receipt must withhold ${name}`);
    assert.equal(withholdsFromRunLog(name, value), true, `run log must withhold ${name}`);
  }
});
