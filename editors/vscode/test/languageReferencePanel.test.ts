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

interface OpenDeps {
  reader: () => LanguageClusterReader | undefined;
  canSelectCluster?: () => boolean;
  onDidChangeConnection?: (listener: () => void) => () => void;
  readDeadlineMs?: number;
}

function depsFrom(deps: OpenDeps): Parameters<typeof LanguageReferencePanel.open>[1] {
  return {
    pin: PIN,
    reader: deps.reader,
    canSelectCluster: deps.canSelectCluster ?? (() => true),
    ...(deps.onDidChangeConnection === undefined
      ? {}
      : { onDidChangeConnection: deps.onDidChangeConnection }),
    ...(deps.readDeadlineMs === undefined ? {} : { readDeadlineMs: deps.readDeadlineMs }),
  };
}

/** Open a fresh panel -- the class is a singleton, so any previous one closes first. */
function open(deps: OpenDeps): StubWebviewPanel {
  live?.close();
  live = undefined;
  resetRecorded();
  LanguageReferencePanel.open(CONTEXT, depsFrom(deps));
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined, "the panel was created");
  live = panel;
  return panel;
}

/** Re-open the SINGLETON with new deps, the way a second command invocation does. */
function reopen(deps: OpenDeps): void {
  LanguageReferencePanel.open(CONTEXT, depsFrom(deps));
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
// A panel that predates the connection surface
// -----------------------------------------------------------------------------

/**
 * A host whose ConnectionManager arrives LATER -- an untrusted window that is
 * later trusted.
 *
 * `onDidChangeConnection` is EXACTLY src/extension.ts's closure, including the
 * part that matters: with no manager there is nothing to subscribe to, so it
 * hands back a no-op unsubscribe over nothing. A panel that bound that once
 * and never again is deaf for the rest of its life, which is the defect these
 * cases exist for.
 */
function lateHost(): {
  deps: OpenDeps;
  surfaceAppears(): void;
  connect(cluster: string): void;
  fire(): void;
  listenerCount(): number;
} {
  let surface: { listeners: (() => void)[] } | undefined;
  let connectedTo = "";
  return {
    deps: {
      reader: () =>
        connectedTo === ""
          ? undefined
          : {
              name: connectedTo,
              edition: "2026",
              grammarVersion: "2026.09-example-0123abcd",
              executeNamed: () =>
                Promise.resolve({
                  rows: () => [
                    {
                      edition: "2026",
                      grammarVersion: "2026.09-example-0123abcd",
                      kinds: ["construct"],
                      entries: [
                        { kind: "construct", name: "query", signature: "s", description: "d" },
                      ],
                    },
                  ],
                }),
            },
      canSelectCluster: () => surface !== undefined,
      onDidChangeConnection: (listener) => {
        const bound = surface;
        if (bound === undefined) return () => undefined;
        bound.listeners.push(listener);
        return () => {
          const at = bound.listeners.indexOf(listener);
          if (at >= 0) bound.listeners.splice(at, 1);
        };
      },
    },
    surfaceAppears: () => {
      surface = { listeners: [] };
    },
    connect: (cluster) => {
      connectedTo = cluster;
    },
    fire: () => {
      for (const listener of [...(surface?.listeners ?? [])]) listener();
    },
    listenerCount: () => surface?.listeners.length ?? 0,
  };
}

test("a panel opened before the connection surface exists follows it after a re-open", async () => {
  // THE DEFECT THIS FAILS AGAINST: open() used to re-point the deps and reload
  // without re-subscribing, so the only binding a panel ever had was the one
  // its constructor made -- over a host with no ConnectionManager, in an
  // untrusted window. It reloaded once on re-open, correctly, and then never
  // updated again for any connect, disconnect or switch.
  const host = lateHost();
  const panel = open(host.deps);
  assert.match(panel.html, /No cluster is connected/);

  // Trust granted: the manager now exists. Running the command again re-opens
  // the singleton.
  host.surfaceAppears();
  reopen(host.deps);
  await settle();
  assert.equal(host.listenerCount(), 1, "the re-open did not bind a connection listener");

  // ...and a cluster connects afterwards, which the panel learns about only
  // through that listener.
  host.connect("local");
  host.fire();
  await settle();
  assert.match(panel.html, /Read from the cluster local/, "the panel did not follow the connect");
  panel.close();
  live = undefined;
});

test("granting workspace trust re-binds an open panel, without re-opening it", async () => {
  // The same seam from the other side. A panel nobody is about to re-open is
  // exactly the case the command cannot cover, so extension.ts's trust
  // listener calls this static -- it is the only thing in a position to.
  const host = lateHost();
  const panel = open(host.deps);
  assert.match(panel.html, /No cluster is connected/);

  host.surfaceAppears();
  LanguageReferencePanel.connectionSurfaceChanged();
  await settle();
  assert.equal(host.listenerCount(), 1, "the trust hook did not bind a connection listener");

  host.connect("prod");
  host.fire();
  await settle();
  assert.match(panel.html, /Read from the cluster prod/);
  panel.close();
  live = undefined;
});

test("re-binding replaces the listener rather than stacking a second one", async () => {
  // Two listeners means two loads per connection change, each racing the other
  // onto one panel. The guard is that subscribe() drops the old binding first.
  const host = lateHost();
  host.surfaceAppears();
  host.connect("local");
  const panel = open(host.deps);
  await settle();
  assert.equal(host.listenerCount(), 1);

  reopen(host.deps);
  LanguageReferencePanel.connectionSurfaceChanged();
  await settle();
  assert.equal(host.listenerCount(), 1, "a re-open stacked a second connection listener");

  // And a closed panel leaves none behind.
  panel.close();
  live = undefined;
  assert.equal(host.listenerCount(), 0, "the closed panel is still subscribed");
});

// -----------------------------------------------------------------------------
// The read deadline
// -----------------------------------------------------------------------------

test("a cluster that never answers is given up on, and the page says what it knows", async () => {
  // THE CLAIM THIS KEEPS: the page says in so many words that it is never a
  // spinner that never ends, and Dispatcher.sendAndWait has no deadline of its
  // own. Without one, a cluster that accepts the call and goes quiet leaves
  // "Reading the grammar and the vocabulary from local..." with no action --
  // retryHtml withholds Try again while a read is in flight.
  let sawSignal = false;
  const panel = open({
    readDeadlineMs: 5,
    reader: () => ({
      name: "local",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      // Never resolves. Rejects only when the deadline's signal fires, which
      // is what the SDK's own dispatcher does with QueryCallOptions.signal.
      executeNamed: (_name, _call, options) =>
        new Promise((_resolve, reject) => {
          if (options?.signal !== undefined) sawSignal = true;
          options?.signal?.addEventListener("abort", () => reject(new Error("aborted")), {
            once: true,
          });
        }),
    }),
  });
  assert.match(panel.html, /Reading the grammar and the vocabulary from local/);

  await new Promise((resolve) => setTimeout(resolve, 40));
  assert.ok(sawSignal, "the read was made with no AbortSignal, so no deadline can reach it");

  // It names the call and the time it was given, rather than the SDK's bare
  // "aborted" -- which would read as something the reader did.
  assert.match(panel.html, /memqlGrammar\(\) did not answer within 5ms, so the read was given up/);
  assert.match(panel.html, /memqlVocabulary\(\) did not answer within 5ms/);
  assert.match(panel.html, /The cluster may still be working on it/);
  assert.equal(
    panel.html.includes("Reading the grammar and the vocabulary"),
    false,
    "the page is still claiming the read is in flight",
  );

  // What the extension knows, and a way out.
  assert.match(panel.html, /built against edition 2026 \(frozen\), grammar 2026\.09-example-0123abcd/);
  assert.equal((panel.html.match(/data-act="reload"/g) ?? []).length, 1, "no Try again");
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
  // AND the trust listener re-binds an open panel. The behaviour is covered
  // above against the static itself; this is the wiring, which no stub can
  // drive -- `commands.executeCommand` records rather than dispatching, so the
  // command handler is unreachable from here.
  const trustBranch = extension.slice(
    extension.indexOf("onDidGrantWorkspaceTrust"),
    extension.indexOf("return { handleOpenUri }"),
  );
  assert.ok(
    trustBranch.includes("LanguageReferencePanel.connectionSurfaceChanged()"),
    "granting trust brings up the runtime surface without telling an open language reference about it",
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
