// The cluster's MemQL language against this extension's, at connect
// (memql#5362, D25 of the language-freeze program design record).
//
// The extension's completion and diagnostics are the grammar it was built
// from; a cluster's are the grammar IT was built from. These tests pin what
// the comparison concludes in every state, the exact words each notice uses,
// and -- as describe.ts insists one layer over -- that no state claims an
// order it cannot show. A grammar version is a label, not a number: the only
// orders this module may state are an edition (a year) against an edition,
// and the release that carries the cluster's grammar against this extension's
// own version.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { ConnectionManager } from "../src/connection/manager.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";
import {
  LanguageSkewMemory,
  OPEN_IN_EXTENSIONS,
  compareLanguage,
  extensionLanguageFacts,
  languageSkewNotice,
  watchLanguageSkew,
  type LanguageFacts,
  type LanguageSkewNotice,
} from "../src/version/editionSkew.js";

const GRAMMAR_A = "2026.08-asof-fallback-and-annotation-arg-narrowings-c0eedce6";
const GRAMMAR_B = "2026.09-dsl-v1-foundations-0123abcd";

// This extension, as its package.json pins it.
const EXTENSION: LanguageFacts = { edition: "2026", grammarVersion: GRAMMAR_A, version: "0.4.0" };

// --- The states --------------------------------------------------------------

test("a cluster that reports no edition is unknown, and unknown shows nothing", () => {
  // A cluster older than the handshake fields states "" for each. Nothing can
  // be compared, so nothing is said -- the same silence skewHint.ts keeps
  // when a version is not recorded.
  for (const edition of [undefined, "", "  "]) {
    const skew = compareLanguage({ edition, grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, EXTENSION);
    assert.equal(skew.state, "unknown", `edition ${JSON.stringify(edition)}`);
    assert.equal(skew.headline, "");
    assert.equal(skew.releaseToInstall, undefined);
    assert.equal(languageSkewNotice(skew), undefined);
  }
});

test("an extension build that carries no pin is unknown too", () => {
  // A test harness, or a manifest someone stripped. The pin is what this
  // side of the comparison IS, so without it there is nothing to compare.
  const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, { version: "0.4.0" });
  assert.equal(skew.state, "unknown");
  assert.equal(languageSkewNotice(skew), undefined);
});

test("a grammar missing on either side is unknown, never a match", () => {
  // "Cannot tell" is not "current": describe.ts's whole rule.
  assert.equal(compareLanguage({ edition: "2026", editorRelease: "0.4.0" }, EXTENSION).state, "unknown");
  assert.equal(
    compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_A }, { edition: "2026", version: "0.4.0" }).state,
    "unknown",
  );
});

test("the same edition and grammar is a match, and a match shows nothing", () => {
  const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_A, editorRelease: "0.4.0" }, EXTENSION);
  assert.equal(skew.state, "match");
  assert.equal(skew.headline, "");
  assert.equal(languageSkewNotice(skew), undefined);
});

test("a newer grammar whose release is newer than this extension is clusterNewer, naming the release", () => {
  const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, EXTENSION);
  assert.equal(skew.state, "clusterNewer");
  assert.equal(
    skew.headline,
    "This cluster's MemQL grammar is newer than this extension's. Update MemQL for VS Code to 0.5.0 or newer so completion and diagnostics match the cluster.",
  );
  assert.equal(skew.releaseToInstall, "0.5.0");
});

test("a later edition is clusterNewer, naming both editions and the release", () => {
  const skew = compareLanguage({ edition: "2027", grammarVersion: GRAMMAR_B, editorRelease: "0.9.0" }, EXTENSION);
  assert.equal(skew.state, "clusterNewer");
  assert.equal(
    skew.headline,
    "This cluster speaks MemQL edition 2027; this extension speaks edition 2026. Update MemQL for VS Code to 0.9.0 or newer.",
  );
  assert.equal(skew.releaseToInstall, "0.9.0");
});

test("a later edition with no release named still says which edition to get, and names no release", () => {
  // The release comes from the cluster; one that did not send it leaves the
  // notice naming the edition instead of inventing a version.
  const skew = compareLanguage({ edition: "2027", grammarVersion: GRAMMAR_B, editorRelease: "" }, EXTENSION);
  assert.equal(skew.state, "clusterNewer");
  assert.equal(
    skew.headline,
    "This cluster speaks MemQL edition 2027; this extension speaks edition 2026. Update MemQL for VS Code to a release that speaks edition 2027.",
  );
  assert.equal(skew.releaseToInstall, undefined);
});

test("a grammar first carried by an older release than this extension is clusterOlder", () => {
  const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.3.1" }, EXTENSION);
  assert.equal(skew.state, "clusterOlder");
  assert.equal(
    skew.headline,
    "This cluster's MemQL grammar is older than this extension's. The editor may suggest forms this cluster refuses.",
  );
  assert.equal(skew.releaseToInstall, undefined, "an older cluster is not fixed by installing anything here");
});

test("an earlier edition is clusterOlder, naming both editions", () => {
  const skew = compareLanguage({ edition: "2025", grammarVersion: GRAMMAR_B, editorRelease: "0.2.0" }, EXTENSION);
  assert.equal(skew.state, "clusterOlder");
  assert.equal(
    skew.headline,
    "This cluster speaks MemQL edition 2025; this extension speaks edition 2026. The editor may suggest forms this cluster refuses.",
  );
});

test("different grammars that cannot be ordered are differs, naming both grammar versions", () => {
  // The release that carries the cluster's grammar is this extension's own
  // version, yet the grammars differ: a locally built extension. Or the
  // cluster named a release that is not one. Either way no order can be
  // shown, and the notice says exactly that.
  for (const editorRelease of ["0.4.0", "main", "", undefined]) {
    const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease }, EXTENSION);
    assert.equal(skew.state, "differs", `editorRelease ${JSON.stringify(editorRelease)}`);
    assert.equal(
      skew.headline,
      `This cluster's MemQL grammar (${GRAMMAR_B}) differs from this extension's (${GRAMMAR_A}), and which is newer cannot be shown. Completion and diagnostics may not match the cluster.`,
    );
    assert.equal(skew.releaseToInstall, undefined);
  }
});

test("editions that are not years cannot be ordered either", () => {
  const skew = compareLanguage({ edition: "next", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, EXTENSION);
  assert.equal(skew.state, "differs");
  assert.equal(
    skew.headline,
    "This cluster speaks MemQL edition next and this extension speaks edition 2026, and which is newer cannot be shown. Completion and diagnostics may not match the cluster.",
  );
});

test("an extension version that is not a release cannot be ordered against the cluster's release", () => {
  const skew = compareLanguage(
    { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" },
    { ...EXTENSION, version: "0.4.0-1737072000" },
  );
  assert.equal(skew.state, "differs");
});

test("surrounding whitespace is not a difference", () => {
  const skew = compareLanguage(
    { edition: " 2026 ", grammarVersion: ` ${GRAMMAR_A}\n`, editorRelease: " 0.4.0 " },
    EXTENSION,
  );
  assert.equal(skew.state, "match");
});

// --- What every notice must carry --------------------------------------------

const NOTICE_CASES: Array<{ name: string; cluster: LanguageFacts }> = [
  { name: "clusterNewer by grammar", cluster: { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" } },
  { name: "clusterNewer by edition", cluster: { edition: "2027", grammarVersion: GRAMMAR_B, editorRelease: "0.9.0" } },
  { name: "clusterNewer by edition, no release", cluster: { edition: "2027", grammarVersion: GRAMMAR_B } },
  { name: "clusterOlder by grammar", cluster: { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.3.1" } },
  { name: "clusterOlder by edition", cluster: { edition: "2025", grammarVersion: GRAMMAR_B, editorRelease: "0.2.0" } },
  { name: "differs by grammar", cluster: { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.4.0" } },
  { name: "differs by edition", cluster: { edition: "next", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" } },
];

test("every notice's headline names both sides, or the release to install", () => {
  for (const { name, cluster } of NOTICE_CASES) {
    const skew = compareLanguage(cluster, EXTENSION);
    const h = skew.headline;
    const namesBothSides = /\bcluster\b/i.test(h) && /\bextension\b/i.test(h);
    const namesTheRelease = skew.releaseToInstall !== undefined && h.includes(skew.releaseToInstall);
    assert.ok(namesBothSides || namesTheRelease, `${name}: ${h}`);
    if (skew.releaseToInstall !== undefined) {
      assert.ok(h.includes(`to ${skew.releaseToInstall} or newer`), `${name} must name the release to install: ${h}`);
    }
  }
});

test("no notice claims an order it cannot show", () => {
  // Only clusterNewer and clusterOlder may say "newer than" / "older than";
  // differs says the order cannot be shown.
  for (const { name, cluster } of NOTICE_CASES) {
    const skew = compareLanguage(cluster, EXTENSION);
    if (skew.state === "differs") {
      assert.doesNotMatch(skew.headline, /\b(newer|older) than\b/, `${name}: ${skew.headline}`);
      assert.match(skew.headline, /cannot be shown/, `${name}: ${skew.headline}`);
    }
  }
});

test("the details name both editions, both grammars, and the release that carries the cluster's grammar", () => {
  const skew = compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, EXTENSION);
  const details = skew.details.join("\n");
  assert.deepEqual(skew.details, [
    `The cluster speaks MemQL edition 2026, grammar ${GRAMMAR_B}.`,
    `This extension (MemQL for VS Code 0.4.0) speaks edition 2026, grammar ${GRAMMAR_A}.`,
    "MemQL for VS Code 0.5.0 is the first release that carries the cluster's grammar.",
  ]);
  assert.ok(details.includes(GRAMMAR_A) && details.includes(GRAMMAR_B));
});

test("the details say so when the cluster names no release", () => {
  const skew = compareLanguage({ edition: "2027", grammarVersion: GRAMMAR_B }, EXTENSION);
  assert.equal(
    skew.details[2],
    "The cluster did not name the release of MemQL for VS Code that carries its grammar.",
  );
});

test("no copy apologises or decorates", () => {
  for (const { cluster } of NOTICE_CASES) {
    const skew = compareLanguage(cluster, EXTENSION);
    for (const line of [skew.headline, ...skew.details]) {
      assert.doesNotMatch(line, /\b(sorry|unfortunately|oops|please)\b/i, line);
      assert.doesNotMatch(line, /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}]/u, `no emoji: ${line}`);
    }
  }
});

// --- The notice: severity and actions ----------------------------------------

test("a newer cluster is a warning offering Open in Extensions", () => {
  const notice = languageSkewNotice(
    compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }, EXTENSION),
  );
  assert.equal(notice?.severity, "warning");
  assert.deepEqual(notice?.actions, [OPEN_IN_EXTENSIONS]);
  assert.equal(OPEN_IN_EXTENSIONS, "Open in Extensions");
});

test("an older or unorderable cluster is information, with no action of its own", () => {
  // "Show details" is added by the toast helper for every notice; neither of
  // these has anything to install.
  for (const editorRelease of ["0.3.1", "0.4.0"]) {
    const notice = languageSkewNotice(
      compareLanguage({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease }, EXTENSION),
    );
    assert.equal(notice?.severity, "information", editorRelease);
    assert.deepEqual(notice?.actions, [], editorRelease);
  }
});

// --- Once per cluster and grammar, per session --------------------------------

test("the memory answers yes once per cluster and grammar", () => {
  const memory = new LanguageSkewMemory();
  const cluster = { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" };
  assert.equal(memory.firstTime("prod", cluster), true);
  assert.equal(memory.firstTime("prod", cluster), false, "a reconnect to the same cluster is not news");
  assert.equal(memory.firstTime("prod", { ...cluster, grammarVersion: "2026.10-next-89abcdef" }), true, "an upgraded cluster is");
  assert.equal(memory.firstTime("staging", cluster), true, "another cluster is its own question");
});

// --- What the extension reads about itself ------------------------------------

test("the extension's facts are read from its manifest, and only strings count", () => {
  assert.deepEqual(extensionLanguageFacts({ version: "0.4.0", memql: { edition: "2026", grammarVersion: GRAMMAR_A } }), {
    edition: "2026",
    grammarVersion: GRAMMAR_A,
    version: "0.4.0",
  });
  // The fake contexts activation tests build have no `extension` at all.
  for (const manifest of [undefined, null, "0.4.0", {}, { memql: "2026" }, { memql: { edition: 2026 } }]) {
    const facts = extensionLanguageFacts(manifest);
    assert.equal(facts.edition, undefined, JSON.stringify(manifest));
    assert.equal(facts.grammarVersion, undefined, JSON.stringify(manifest));
  }
});

test("the manifest this extension ships carries the pin it compares with", () => {
  // The Go parity gate (cmd/memql-lsp/editorparity_test.go) holds the pin to
  // the parser; this holds the READER to the manifest, so renaming the key on
  // one side cannot leave every connect silently unknown.
  const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "..", "package.json"), "utf8")) as {
    version: string;
  };
  const facts = extensionLanguageFacts(manifest);
  assert.equal(facts.edition, "2026");
  assert.match(facts.grammarVersion ?? "", /^\d{4}\.\d{2}-[a-z0-9-]+-[0-9a-f]{8}$/);
  assert.equal(facts.version, manifest.version);
});

// --- The watcher, over a real ConnectionManager ------------------------------

function liveJwt(): string {
  const b64 = (v: unknown): string => Buffer.from(JSON.stringify(v)).toString("base64url");
  return `${b64({ alg: "RS256" })}.${b64({ sub: "u", exp: Math.floor(Date.now() / 1000) + 3600 })}.sig`;
}

function cluster(name: string): ClusterConfig {
  return { name, endpoint: "api.memql.localhost:443", token: liveJwt() };
}

// Just what ConnectionManager touches, plus the three language facts.
function fakeConn(language: LanguageFacts): Connection {
  let resolveDone!: () => void;
  const done = new Promise<void>((resolve) => {
    resolveDone = resolve;
  });
  return {
    nodeId: "bff",
    engineVersion: "",
    edition: language.edition ?? "",
    grammarVersion: language.grammarVersion ?? "",
    editorRelease: language.editorRelease ?? "",
    query: {},
    subscriptions: {},
    close: () => resolveDone(),
    done: () => done,
  } as unknown as Connection;
}

test("the watcher presents a notice on connect, once per cluster and grammar", async () => {
  let next: LanguageFacts = { edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" };
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn(next)));
  const presented: Array<{ cluster: string; notice: LanguageSkewNotice }> = [];
  const stop = watchLanguageSkew(manager, () => EXTENSION, (notice, clusterName) => {
    presented.push({ cluster: clusterName, notice });
  });

  await manager.connect(cluster("prod"));
  assert.equal(presented.length, 1);
  assert.equal(presented[0]?.cluster, "prod");
  assert.equal(presented[0]?.notice.severity, "warning");

  await manager.connect(cluster("prod"));
  assert.equal(presented.length, 1, "reconnecting to the same cluster and grammar says nothing new");

  next = { edition: "2026", grammarVersion: GRAMMAR_A, editorRelease: "0.4.0" };
  await manager.connect(cluster("staging"));
  assert.equal(presented.length, 1, "a matching cluster shows nothing");

  next = { edition: "", grammarVersion: "", editorRelease: "" };
  await manager.connect(cluster("old"));
  assert.equal(presented.length, 1, "a cluster predating the fields shows nothing");

  next = { edition: "2026", grammarVersion: "2026.10-next-89abcdef", editorRelease: "0.6.0" };
  await manager.connect(cluster("prod"));
  assert.equal(presented.length, 2, "the same cluster on a new grammar is news again");

  stop();
  next = { edition: "2026", grammarVersion: "2026.11-later-00000000", editorRelease: "0.7.0" };
  await manager.connect(cluster("prod"));
  assert.equal(presented.length, 2, "a stopped watcher presents nothing");
});

test("the watcher reads the extension's facts at connect, not at wiring", async () => {
  // The manifest is read inside the listener, so a test (or a future reload)
  // that changes what the extension reports is heard on the next connect.
  const manager = new ConnectionManager(() =>
    Promise.resolve(fakeConn({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" })),
  );
  let reads = 0;
  const presented: LanguageSkewNotice[] = [];
  watchLanguageSkew(
    manager,
    () => {
      reads += 1;
      return EXTENSION;
    },
    (notice) => presented.push(notice),
  );
  assert.equal(reads, 0, "nothing is read until a connection is made");
  await manager.connect(cluster("prod"));
  assert.equal(reads, 1);
  assert.equal(presented.length, 1);
});
