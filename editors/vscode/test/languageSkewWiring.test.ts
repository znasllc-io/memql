// The connect-time language notice, as src/extension.ts presents it
// (memql#5362, D25).
//
// editionSkew.test.ts pins the decision, the words and the once-per-session
// memory away from `vscode`. This file pins the half only an editor has: the
// toast's severity and buttons, what "Open in Extensions" runs, and where the
// details are written. A decision nobody presents -- or one presented with its
// buttons dropped -- passes every case in that file and does nothing on
// screen, which is the failure themeOfferWiring.test.ts exists for too.
//
// ITS OWN FILE, because activation happens once per process. Activation runs
// first so the MemQL Connection channel exists, exactly as it does in the
// editor; the notice is then driven through a REAL ConnectionManager over a
// fake dial, wired by the same function registerRuntimeSurface wires the
// activation-built manager with. That manager dials a real cluster, which no
// case in this lane can reach -- so the last case reads registerRuntimeSurface
// itself and fails if the call wiring it is removed, the textual approach the
// vscode-import rule takes (cmd/memql-lsp/vscodeimportrule_test.go).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { activate, wireLanguageSkewNotice } from "../src/extension.js";
import { ConnectionManager } from "../src/connection/manager.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import { THEME_OFFER_ANSWERED_KEY } from "../src/theme/themeOffer.js";
import type { LanguageFacts } from "../src/version/editionSkew.js";
import {
  recorded,
  setNextInformationMessageChoice,
  setNextWarningMessageChoice,
  workspace,
} from "./support/vscodeStub.js";

// The isolation activation.test.ts takes: the runtime surface mkdirs ~/.memql,
// and an empty PATH keeps the language client from finding a server.
const home = fs.mkdtempSync(path.join(os.tmpdir(), "memql-languageskew-"));
process.env.HOME = home;
process.env.PATH = "";

const globalState = {
  store: new Map<string, unknown>([[THEME_OFFER_ANSWERED_KEY, true]]),
  get<T>(key: string): T | undefined {
    return globalState.store.get(key) as T | undefined;
  },
  update(key: string, value: unknown): Promise<void> {
    globalState.store.set(key, value);
    return Promise.resolve();
  },
};

const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, "extension", relative),
  globalState,
} as unknown as ExtensionContext;

workspace.isTrusted = true;
activate(context);

const GRAMMAR_A = "2026.08-asof-fallback-and-annotation-arg-narrowings-c0eedce6";
const GRAMMAR_B = "2026.09-dsl-v1-foundations-0123abcd";
const EXTENSION: LanguageFacts = { edition: "2026", grammarVersion: GRAMMAR_A, version: "0.4.0" };

function liveJwt(): string {
  const b64 = (v: unknown): string => Buffer.from(JSON.stringify(v)).toString("base64url");
  return `${b64({ alg: "RS256" })}.${b64({ sub: "u", exp: Math.floor(Date.now() / 1000) + 3600 })}.sig`;
}

function cluster(name: string): ClusterConfig {
  return { name, endpoint: "api.memql.localhost:443", token: liveJwt() };
}

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

// The id these cases hand the adapter, standing in for `context.extension.id`.
// Deliberately NOT this extension's real id: the assertion is that
// "Open in Extensions" opens whatever id the host reported, and a copy of the
// real id in the adapter would pass a test written with the real id too.
const HOST_REPORTED_ID = "tester.memql-under-test";

// A manager whose every dial answers with `language`, wired the way
// registerRuntimeSurface wires the real one. The host's id rides in an object
// rather than a defaulted parameter: JavaScript applies a parameter default to
// an explicit `undefined` too, which would quietly turn "no id" back into one.
function wiredManager(
  language: () => LanguageFacts,
  host: { extensionId: string | undefined } = { extensionId: HOST_REPORTED_ID },
): ConnectionManager {
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn(language())));
  wireLanguageSkewNotice(manager, () => EXTENSION, host.extensionId);
  return manager;
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i++) await Promise.resolve();
}

function connectionChannel(): { lines: string[]; shown: boolean } {
  const channel = recorded.outputChannels.find((c) => c.name === "MemQL Connection");
  assert.ok(channel !== undefined, "activation did not create the MemQL Connection channel");
  return channel;
}

test("a newer cluster raises a warning naming the release, with both actions", async () => {
  const manager = wiredManager(() => ({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }));
  setNextWarningMessageChoice("Open in Extensions");
  await manager.connect(cluster("newer"));
  await flush();

  const at = recorded.warnings.indexOf(
    "This cluster's MemQL grammar is newer than this extension's. Update MemQL for Visual Studio Code and Cursor to 0.5.0 or newer so completion and diagnostics match the cluster.",
  );
  assert.ok(at >= 0, `the notice was not shown; saw: ${JSON.stringify(recorded.warnings)}`);
  assert.deepEqual(recorded.warningActions[at], ["Open in Extensions", "Show details"]);
});

test("Open in Extensions opens the page of the id the host reported", () => {
  // Armed on the case above. The id is the assertion: `extension.open` with
  // any other id opens somebody else's page and looks just as successful.
  const at = recorded.executed.lastIndexOf("extension.open");
  assert.ok(at >= 0, `extension.open was not run; ran: ${JSON.stringify(recorded.executed)}`);
  assert.deepEqual(recorded.executedArgs[at], [HOST_REPORTED_ID]);
});

test("with no id from the host, the notice draws no Open in Extensions button", async () => {
  // Only a fake context lacks `extension`, and a control that cannot work is
  // not drawn: the warning keeps its words and its details.
  const manager = wiredManager(
    () => ({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.6.0" }),
    { extensionId: undefined },
  );
  await manager.connect(cluster("no-id"));
  await flush();

  const at = recorded.warnings.indexOf(
    "This cluster's MemQL grammar is newer than this extension's. Update MemQL for Visual Studio Code and Cursor to 0.6.0 or newer so completion and diagnostics match the cluster.",
  );
  assert.ok(at >= 0, `the notice was not shown; saw: ${JSON.stringify(recorded.warnings)}`);
  assert.deepEqual(recorded.warningActions[at], ["Show details"]);
});

test("the details land in the MemQL Connection channel, naming both grammars and the release", () => {
  const text = connectionChannel().lines.join("\n");
  assert.match(text, /"newer"/, "the record names the cluster it is about");
  assert.ok(text.includes(GRAMMAR_A), "the extension's grammar is recorded");
  assert.ok(text.includes(GRAMMAR_B), "the cluster's grammar is recorded");
  assert.ok(text.includes("MemQL for Visual Studio Code and Cursor 0.5.0 is the first release that carries the cluster's grammar."));
});

test("a cluster on a newer edition raises a warning naming both editions and the release", async () => {
  // The case #5362's acceptance names: connecting to a cluster with a newer
  // edition shows the notice naming both versions.
  const manager = wiredManager(() => ({ edition: "2027", grammarVersion: GRAMMAR_B, editorRelease: "0.9.0" }));
  await manager.connect(cluster("next-edition"));
  await flush();

  const at = recorded.warnings.indexOf(
    "This cluster speaks MemQL edition 2027; this extension speaks edition 2026. Update MemQL for Visual Studio Code and Cursor to 0.9.0 or newer.",
  );
  assert.ok(at >= 0, `the notice was not shown; saw: ${JSON.stringify(recorded.warnings)}`);
  assert.deepEqual(recorded.warningActions[at], ["Open in Extensions", "Show details"]);
});

test("reconnecting to the same cluster and grammar raises nothing new", async () => {
  const before = recorded.warnings.length + recorded.infos.length;
  const manager = wiredManager(() => ({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.5.0" }));
  await manager.connect(cluster("repeat"));
  await manager.connect(cluster("repeat"));
  await flush();
  assert.equal(recorded.warnings.length + recorded.infos.length, before + 1, "once per cluster and grammar, per session");
});

test("an older cluster is an information notice whose one action reveals the details", async () => {
  const manager = wiredManager(() => ({ edition: "2026", grammarVersion: GRAMMAR_B, editorRelease: "0.3.1" }));
  setNextInformationMessageChoice("Show details");
  await manager.connect(cluster("older"));
  await flush();

  const at = recorded.infos.indexOf(
    "This cluster runs an older MemQL grammar than this extension. Completion may offer forms it refuses until the cluster is updated.",
  );
  assert.ok(at >= 0, `the notice was not shown; saw: ${JSON.stringify(recorded.infos)}`);
  assert.deepEqual(recorded.infoActions[at], ["Show details"]);
  assert.equal(connectionChannel().shown, true, "Show details reveals the channel the record is in");
});

test("a matching cluster, and one that predates the fields, raise nothing", async () => {
  const before = recorded.warnings.length + recorded.infos.length;
  const matching = wiredManager(() => ({ edition: "2026", grammarVersion: GRAMMAR_A, editorRelease: "0.4.0" }));
  await matching.connect(cluster("matching"));
  const old = wiredManager(() => ({}));
  await old.connect(cluster("old"));
  await flush();
  assert.equal(recorded.warnings.length + recorded.infos.length, before);
});

// The body of a top-level function in src/extension.ts: from its signature to
// the first `}` in column 0, which is where every top-level function in that
// hand-formatted, two-space-indented file closes.
function topLevelFunctionBody(source: string, name: string): string {
  const start = source.indexOf(`\nfunction ${name}(`);
  assert.ok(start >= 0, `src/extension.ts has no top-level function ${name}; if it was renamed, follow it here`);
  const end = source.indexOf("\n}\n", start);
  assert.ok(end > start, `could not find where ${name} closes`);
  return source.slice(start, end);
}

// Comment LINES removed -- `//`, and the lines of a `/* */` or JSDoc block --
// so a commented-out call does not count. Line-based on purpose: a regex over
// `/* ... */` would treat the `/*` inside a glob string such as '**/*.memql'
// as a comment opener and delete real code.
function withoutCommentLines(body: string): string {
  return body
    .split("\n")
    .filter((line) => !/^\s*(\/\/|\/\*|\*)/.test(line))
    .join("\n");
}

test("activation wires the notice onto the manager it builds, with the host's manifest and id", () => {
  // Every case above calls wireLanguageSkewNotice on a manager of its own.
  // This one holds registerRuntimeSurface to calling it on THE manager
  // activation builds (`connections`), with this extension's manifest and the
  // id the host reports -- the call no unit test can drive, since that
  // manager dials a real cluster. Delete the call and this fails.
  const source = fs.readFileSync(path.join(__dirname, "..", "..", "src", "extension.ts"), "utf8");
  const body = withoutCommentLines(topLevelFunctionBody(source, "registerRuntimeSurface"));

  assert.match(body, /wireLanguageSkewNotice\(\s*connections\s*,/, "registerRuntimeSurface does not wire the notice onto its manager");
  assert.match(
    body,
    /extensionLanguageFacts\(\s*context\.extension\?\.packageJSON\s*\)/,
    "the extension's side must come from its own manifest",
  );
  assert.match(body, /context\.extension\?\.id\b/, "the id Open in Extensions opens must be the one the host reports");
});
