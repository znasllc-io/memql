// The language reference panel: the document it puts on screen, the calls it
// makes, and the manifest entry that opens it.
//
// The page's CONTENT is decided in src/state/languageReference.ts and
// src/webview/languageReferenceScreens.ts and tested in
// test/languageReference.test.ts, which is where the interesting assertions
// live. This file is the other half of that split -- that the panel really
// wires those modules to a webview document, really asks the cluster for both
// artifacts, and is really reachable from the palette.
//
// Refs: memql#5388

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import {
  COMMAND_LANGUAGE_REFERENCE,
  LanguageReferencePanel,
  type LanguageClusterReader,
} from "../src/webview/languageReferencePanel.js";
import { GRAMMAR_CALL, VOCABULARY_CALL, languagePin } from "../src/state/languageReference.js";
import { DARK, LIGHT } from "../src/webview/palette.js";
import { recorded, resetRecorded, type StubWebviewPanel } from "./support/vscodeStub.js";

// dist-test/test/<name>.js, so the package root is two levels up.
const ROOT = path.resolve(__dirname, "..", "..");

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

const PIN = {
  edition: "2026",
  status: "frozen",
  grammarVersion: "2026.09-example-0123abcd",
  version: "0.6.0",
};

/** Settle the panel's two in-flight reads. */
function settle(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

let live: StubWebviewPanel | undefined;

/** Open a fresh panel -- the class is a singleton, so any previous one closes first. */
function open(deps: {
  reader: () => LanguageClusterReader | undefined;
  canSelectCluster?: () => boolean;
}): StubWebviewPanel {
  live?.close();
  live = undefined;
  resetRecorded();
  LanguageReferencePanel.open(CONTEXT, {
    pin: PIN,
    reader: deps.reader,
    canSelectCluster: deps.canSelectCluster ?? (() => true),
  });
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined, "the panel was created");
  live = panel;
  return panel;
}

test("with no cluster the panel renders the pin rather than waiting", () => {
  const panel = open({ reader: () => undefined });
  assert.equal(panel.viewType, "memqlLanguageReference");
  assert.match(panel.html, /No cluster is connected/);
  assert.ok(panel.html.includes("2026.09-example-0123abcd"), "the pinned grammar version");
  assert.ok(panel.html.includes("frozen"), "the pinned edition status");
  panel.close();
  live = undefined;
});

test("the document carries the brand palette and the CSP that forbids inline handlers", () => {
  const panel = open({ reader: () => undefined });
  // Both palettes ship in every document -- the CSS carries light and dark and
  // the stamped attribute picks one.
  assert.ok(panel.html.includes(DARK.accent), "the dark accent is missing");
  assert.ok(panel.html.includes(LIGHT.accent), "the light accent is missing");
  assert.match(panel.html, /default-src 'none'; style-src 'nonce-/);
  assert.match(panel.html, /<body[^>]*>/);
  panel.close();
  live = undefined;
});

test("a connected cluster is asked for both artifacts, and both land on the page", async () => {
  const calls: string[] = [];
  const panel = open({
    reader: () => ({
      name: "local",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      executeNamed: (_name, call) => {
        calls.push(call);
        return Promise.resolve({
          rows: () =>
            call === GRAMMAR_CALL
              ? [
                  {
                    format: "ebnf",
                    edition: "2026",
                    grammarVersion: "2026.09-example-0123abcd",
                    content: '(* ---- A file ---- *)\n<file> ::= <use>* <declaration>*\n',
                  },
                ]
              : [
                  {
                    edition: "2026",
                    grammarVersion: "2026.09-example-0123abcd",
                    kinds: ["construct"],
                    entries: [
                      {
                        kind: "construct",
                        name: "query",
                        signature: "query <Concept> <name> { ... }",
                        description: "Read function.",
                      },
                    ],
                  },
                ],
        });
      },
    }),
  });

  await settle();
  assert.deepEqual([...calls].sort(), [GRAMMAR_CALL, VOCABULARY_CALL].sort());
  assert.ok(panel.html.includes("1 productions"), "the grammar did not reach the page");
  assert.ok(panel.html.includes("1 entries"), "the vocabulary did not reach the page");
  assert.match(panel.html, /Read from the cluster local/);
  // The search box appears only once there is something to search.
  assert.ok(panel.html.includes('id="lr-search"'), "no search box over two artifacts");
  panel.close();
  live = undefined;
});

test("a refused call is named on the page, and the other artifact still renders", async () => {
  const panel = open({
    reader: () => ({
      name: "local",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      executeNamed: (name, call) =>
        call === GRAMMAR_CALL
          ? Promise.reject(new Error("unknown builtin"))
          : Promise.resolve({
              rows: () => [
                {
                  edition: "2026",
                  grammarVersion: "2026.09-example-0123abcd",
                  kinds: ["construct"],
                  entries: [{ kind: "construct", name: name, signature: "s", description: "d" }],
                },
              ],
            }),
    }),
  });

  await settle();
  // NAMED, because the two are separate calls with separate reasons to fail: a
  // cluster too old to carry one of them refuses that one by name.
  assert.match(panel.html, /memqlGrammar\(\) could not be read: unknown builtin/);
  assert.match(panel.html, /did not answer with a grammar/);
  assert.ok(panel.html.includes("1 entries"), "the vocabulary that DID answer is missing");
  panel.close();
  live = undefined;
});

// -----------------------------------------------------------------------------
// The manifest
// -----------------------------------------------------------------------------

interface Manifest {
  memql?: { edition?: string; status?: string; grammarVersion?: string };
  version?: string;
  contributes: {
    commands: { command: string; title: string }[];
    menus: Record<string, { command: string; when?: string }[]>;
  };
}

const manifest = JSON.parse(
  fs.readFileSync(path.join(ROOT, "package.json"), "utf8"),
) as Manifest;

test("the command is contributed, registered, and reachable from the palette", () => {
  // THE THREE HALVES TOGETHER. A command contributed but not registered fails
  // with "command not found" when pressed, and one registered but not
  // contributed cannot be found at all. The id is spelled out here rather than
  // imported on the manifest side, on purpose: it crosses into package.json,
  // which no TypeScript import reaches.
  assert.equal(COMMAND_LANGUAGE_REFERENCE, "memql.language.showReference");
  const declared = manifest.contributes.commands.find(
    (c) => c.command === "memql.language.showReference",
  );
  assert.ok(declared !== undefined, "memql.language.showReference is not contributed");
  assert.equal(declared.title, "MemQL: Show Language Reference");

  const extension = fs.readFileSync(path.join(ROOT, "src", "extension.ts"), "utf8");
  assert.ok(
    extension.includes("registerCommand(COMMAND_LANGUAGE_REFERENCE"),
    "the command is contributed but never registered in extension.ts",
  );

  // NOT GATED ON TRUST. It reads no credential and opens no connection, and a
  // `when` clause of isWorkspaceTrusted would hide the one MemQL surface a
  // restricted folder can actually keep its promise about.
  const palette = manifest.contributes.menus["commandPalette"] ?? [];
  const entry = palette.find((e) => e.command === "memql.language.showReference");
  assert.equal(
    entry?.when,
    undefined,
    "the language reference is gated in the palette; it works with no cluster and no trust",
  );
});

test("the manifest this extension ships pins the status the panel prints", () => {
  // The Go gate (cmd/memql-lsp/editionstatus_test.go) holds the pin to
  // test/conformance/<edition>/manifest.json; this holds the READER to the
  // manifest, so renaming the key on one side cannot leave every reference
  // panel silently saying "not stated".
  const pin = languagePin(manifest);
  assert.equal(pin.edition, manifest.memql?.edition);
  assert.ok(pin.status !== "", 'package.json carries no "memql": {"status": ...}');
  assert.equal(pin.version, manifest.version);
  assert.match(pin.grammarVersion, /^\d{4}\.\d{2}-[a-z0-9-]+-[0-9a-f]{8}$/);
});
