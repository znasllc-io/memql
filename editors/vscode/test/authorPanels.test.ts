// The authoring pages, driven through the webview stub: the bugs the kit
// rebuild fixed, each asserted where a person would have met it.
//
//   - a run Result's row click opened the concept on page one with nothing
//     selected, and the row may not even have been on that page;
//   - the concept page said "No rows for space." before its first read had
//     answered;
//   - the automation row picker said "Loading rows..." forever over a concept
//     with no rows;
//   - the construct page was blank until a workspace file lookup finished;
//   - rows in all three lists were mouse-only;
//   - the language reference's search is now drawn by the host, patched in
//     place, rather than by a page script the host could not see;
//   - a saved run with no file could never be run from the Runs view;
//   - the Result tab appeared only once a run had finished.
//
// Every page is built on the kit and its LiveView assigns a new document for
// each changed render until the page says `ready` -- which the stub never
// does unless a case sends it -- so `html` is always the latest render.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import type { ConnectionManager, ConnectionState } from "../src/connection/manager.js";
import { savedRunCatalogTarget } from "../src/constructs/catalogTarget.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import { AutomationRunPanel } from "../src/webview/automationPanel.js";
import { ConceptPanel } from "../src/webview/conceptPanel.js";
import { ConstructPanel } from "../src/webview/constructPanel.js";
import { LanguageReferencePanel } from "../src/webview/languageReferencePanel.js";
import { rowListHtml } from "../src/webview/rowListView.js";
import { ResultPanel, RunPanel, type RunPanelHost } from "../src/webview/runPanel.js";
import { recorded, resetRecorded, setNextInputBoxResult, type StubWebviewPanel } from "./support/vscodeStub.js";

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

function settle(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

function lastPanel(): StubWebviewPanel {
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined, "no panel was created");
  return panel;
}

/** The page's visible text, roughly: markup and screen-reader-only words stripped. */
function visible(html: string): string {
  const body = html.slice(html.indexOf("<body"));
  return body
    .replace(/<script[\s\S]*?<\/script>/g, "")
    .replace(/<span class="mq-sr">[^<]*<\/span>/g, "")
    .replace(/<[^>]+>/g, " ")
    .replace(/&#39;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&");
}

// ---------------------------------------------------------------------------
// The concept page
// ---------------------------------------------------------------------------

const SPACE: Concept = {
  id: "v1:cognition:space",
  domain: "cognition",
  entity: "space",
  description: "",
  displayCard: { primary: "name" },
} as unknown as Concept;

const CONNECTED: ConnectionState = { status: "connected", clusterName: "local", nodeId: "bff-0" };

interface FakeQuery {
  pages: { rows: Record<string, unknown>[] }[];
  rows: Record<string, Record<string, unknown>>;
  pendingPage?: Promise<void>;
}

function connectionsWith(fake: FakeQuery): ConnectionManager {
  const query = {
    executeNamed: async (name: string, call: string) => {
      if (name === "conceptBrowse") {
        await fake.pendingPage;
        const page = fake.pages.shift() ?? { rows: [] };
        return { rawNodes: () => page.rows, meta: () => ({ cursor: "" }) };
      }
      const id = /id==(\S+)$/.exec(call)?.[1] ?? "";
      const row = fake.rows[id];
      return { rawNodes: () => (row === undefined ? [] : [row]), meta: () => ({}) };
    },
  };
  return {
    state: CONNECTED,
    query,
    subscriptions: { subscribeGraph: () => () => undefined },
    onDidChangeState: () => () => undefined,
  } as unknown as ConnectionManager;
}

test("a result row opens its concept page ON that row, even one that is not on the first page", async () => {
  resetRecorded();
  const fake: FakeQuery = {
    pages: [{ rows: [{ id: "A", concept: SPACE.id, payload: { name: "Design review" } }] }],
    rows: { C: { id: "C", concept: SPACE.id, payload: { name: "Hiring pipeline" } } },
  };
  ConceptPanel.open(CONTEXT, connectionsWith(fake), SPACE, "C");
  await settle();
  await settle();
  const panel = lastPanel();
  // The detail pane holds the clicked row, read by its id.
  assert.match(panel.html, /Hiring pipeline/, "the page did not open on the clicked row");
  panel.close();
});

test("the concept page is the shape of its rows until the first page answers -- never 'No rows'", async () => {
  resetRecorded();
  let release!: () => void;
  const fake: FakeQuery = {
    pages: [{ rows: [] }],
    rows: {},
    pendingPage: new Promise<void>((resolve) => {
      release = resolve;
    }),
  };
  ConceptPanel.open(CONTEXT, connectionsWith(fake), { ...SPACE, id: "v1:cognition:loading" } as Concept);
  await settle();
  const panel = lastPanel();
  assert.match(panel.html, /mq-skeleton/);
  assert.doesNotMatch(visible(panel.html), /No rows/, "the page called a concept empty before asking");

  release();
  await settle();
  await settle();
  assert.match(visible(panel.html), /No rows yet\./, "an answered empty read is said as such");
  panel.close();
});

test("the concept page's rows are buttons a keyboard can reach", async () => {
  resetRecorded();
  const fake: FakeQuery = {
    pages: [{ rows: [{ id: "A", concept: SPACE.id, payload: { name: "Design review" } }] }],
    rows: {},
  };
  ConceptPanel.open(CONTEXT, connectionsWith(fake), { ...SPACE, id: "v1:cognition:keys" } as Concept);
  await settle();
  await settle();
  const panel = lastPanel();
  assert.match(panel.html, /<button type="button" class="vk-row" data-act="selectRow" data-value="A"/);
  panel.close();
});

test("the concept page's Sign in reaches the sign-in command with an argument it can take", async () => {
  // THE BUG: it passed the cluster's NAME, and memql.clusters.signIn takes a
  // cluster row -- `target.cluster.name` on a string threw, so the button did
  // nothing. It now sends no argument, as the view welcomes do.
  resetRecorded();
  const signedOut = {
    ...connectionsWith({ pages: [], rows: {} }),
    state: { status: "error", clusterName: "local", message: "expired", reason: "credentialExpired" },
    query: undefined,
    subscriptions: undefined,
  } as unknown as ConnectionManager;
  ConceptPanel.open(CONTEXT, signedOut, { ...SPACE, id: "v1:cognition:signin" } as Concept);
  await settle();
  const panel = lastPanel();
  assert.match(visible(panel.html), /Sign in to see these rows\./);
  panel.send({ type: "signIn" });
  const at = recorded.executed.lastIndexOf("memql.clusters.signIn");
  assert.ok(at >= 0, "Sign in reached no command");
  for (const arg of recorded.executedArgs[at] ?? []) {
    assert.equal(typeof arg, "object", "a bare value is not a cluster row");
  }
  panel.close();
});

test("the sign-in command takes a cluster row, a cluster name, or nothing", () => {
  // Every surface that says sign-in is needed offers memql.clusters.signIn,
  // and they do not all hold a row: the link-from-MemQL-OS toast and the view
  // welcomes hold a name or nothing. The link toast once passed `cluster.name`
  // to a handler that read `target.cluster.name` and threw on the click, so
  // the handler's own signature is the guard.
  const text = fs.readFileSync(path.join(__dirname, "..", "..", "src", "extension.ts"), "utf8");
  const handler = /registerCommand\(\s*['"]memql\.clusters\.signIn['"]\s*,\s*async\s*\(\s*arg\?:\s*([^)]*)\)/.exec(text);
  assert.ok(handler, "memql.clusters.signIn is not registered with an optional argument");
  assert.match(handler?.[1] ?? "", /ClusterNode/, "the handler no longer takes a cluster row");
  assert.match(handler?.[1] ?? "", /string/, "the handler no longer takes a cluster name");
});

test("rowListHtml wraps each view-kit row in a button carrying its id, and marks the selected one", () => {
  const html = rowListHtml({
    rows: [
      { id: "A", name: "One" },
      { id: "B", name: "Two" },
    ],
    concept: { id: "c", entity: "thing", displayCard: { primary: "name" } },
    selectedRowId: "B",
    act: "openRow",
    data: { "concept-id": "c" },
    label: "Rows of thing",
  });
  assert.ok(html !== undefined);
  assert.equal((html.match(/<button type="button"/g) ?? []).length, 2);
  assert.match(html, /data-act="openRow" data-value="B" data-row-id="B" data-concept-id="c" data-selected="true" aria-current="true"/);
  assert.match(html, /aria-label="Rows of thing"/);
  // An empty list is the caller's to say.
  assert.equal(rowListHtml({ rows: [], concept: { id: "c", entity: "thing" }, act: "x", label: "x" }), undefined);
});

// ---------------------------------------------------------------------------
// The automation form
// ---------------------------------------------------------------------------

test("the automation row picker says an empty concept is empty, instead of loading forever", async () => {
  resetRecorded();
  AutomationRunPanel.open(
    CONTEXT,
    {
      run: async () => ({ status: "superseded" }) as never,
      saveConfig: async () => undefined,
      browseRows: async () => ({ rows: [], nextCursor: "" }),
      concept: () => undefined,
    },
    {
      uri: "file:///w/a.memql",
      name: "autoJoinSI",
      trigger: { event: "node.created", concept: "v1:cognition:participant" },
    },
  );
  await settle();
  await settle();
  const panel = lastPanel();
  assert.doesNotMatch(visible(panel.html), /Loading rows/);
  assert.match(visible(panel.html), /No participant rows yet\./);
  // The include-step-output choice is a switch.
  assert.match(panel.html, /role="switch"[^>]*data-field="includeStepOutput"/);
  panel.close();
});

// ---------------------------------------------------------------------------
// The construct page
// ---------------------------------------------------------------------------

const QUERY: CatalogConstruct = {
  name: "spaceParticipants",
  kind: "query",
  namespace: "cognition",
  origin: "bundle",
  originPath: "cognition/queries.memql",
  description: "The people and agents in a space.",
  runnable: true,
  runnableKind: "query",
  args: [],
  boundConcept: "",
  sourceHash: "",
  source: "",
};

test("the construct page is drawn at once, before the workspace lookup answers", () => {
  resetRecorded();
  const calls: string[] = [];
  ConstructPanel.open(
    CONTEXT,
    QUERY,
    {
      viewSourceFromCluster: async () => undefined,
      browseRows: async () => undefined,
      openInOs: async () => undefined,
      run: async (_c, withArgs) => {
        calls.push(withArgs ? "runWith" : "run");
      },
    },
    "local",
  );
  const panel = lastPanel();
  // Synchronously: no awaited file stat stands between the click and the page.
  assert.match(panel.html, /spaceParticipants/);
  assert.equal(panel.title, "spaceParticipants", "the tab carries the name, not a prefix");
  panel.send({ type: "run" });
  assert.deepEqual(calls, ["run"], "Run goes through the host's one run path");
  panel.close();
});

test("the construct page opened from a cluster document shows its loading shape, then the failure with Try again", async () => {
  resetRecorded();
  let attempts = 0;
  ConstructPanel.openLoading(
    CONTEXT,
    "spaceParticipants",
    {
      viewSourceFromCluster: async () => undefined,
      browseRows: async () => undefined,
      openInOs: async () => undefined,
      run: async () => undefined,
    },
    "local",
    async () => {
      attempts += 1;
      if (attempts === 1) throw new Error("Couldn't read this cluster's constructs.");
      return QUERY;
    },
  );
  const panel = lastPanel();
  assert.match(panel.html, /mq-skeleton/);
  await settle();
  assert.match(visible(panel.html), /Couldn't read this cluster's constructs\./);
  panel.send({ type: "retry" });
  await settle();
  assert.match(visible(panel.html), /The people and agents in a space\./);
  panel.close();
});

test("a reused construct page names the construct it is loading, not the last one", async () => {
  resetRecorded();
  const deps = {
    viewSourceFromCluster: async () => undefined,
    browseRows: async () => undefined,
    openInOs: async () => undefined,
    run: async () => undefined,
  };
  ConstructPanel.open(CONTEXT, QUERY, deps, "local");
  const panel = lastPanel();
  let release!: (c: CatalogConstruct) => void;
  ConstructPanel.openLoading(CONTEXT, "autoJoinSI", deps, "local", () => new Promise((resolve) => (release = resolve)));
  assert.equal(recorded.webviews.length, 1, "the page is a singleton");
  assert.equal(panel.title, "autoJoinSI", "the tab still names the previous construct while loading");
  release({ ...QUERY, name: "autoJoinSI" });
  await settle();
  panel.close();
});

// ---------------------------------------------------------------------------
// The result
// ---------------------------------------------------------------------------

test("the Result tab opens in its running shape at the click, and fills in when the run lands", () => {
  resetRecorded();
  const host: RunPanelHost = {
    run: async () => ({ status: "declined", target: TARGET }),
    saveConfig: async () => undefined,
    concepts: () => new Map(),
    openRow: () => undefined,
  };
  ResultPanel.running(CONTEXT, host, TARGET);
  const panel = lastPanel();
  assert.match(panel.html, /Running/);
  assert.match(panel.html, /mq-skeleton/);
  ResultPanel.show(CONTEXT, host, {
    status: "ok",
    target: TARGET,
    rows: [],
    raw: [],
    ranDeployedDefinition: false,
    injected: true,
  });
  assert.equal(recorded.webviews.length, 1, "the result opened a second tab");
  assert.match(visible(panel.html), /No rows\./);
  assert.match(visible(panel.html), /From this editor · not saved/);
  panel.close();
});

const TARGET = { uri: "file:///w/q.memql", kind: "query" as const, name: "spaceParticipants", args: [] };

test("a declined run opens no Result tab, and the form says it was cancelled", async () => {
  // THE BUG: the Result tab opened in "Running" at the click, BEFORE the write
  // confirmation, and a No then left it open to say nothing ran. The host now
  // opens it once the run really starts, and a No is answered on the form.
  resetRecorded();
  RunPanel.open(
    CONTEXT,
    {
      run: async () => ({ status: "declined", target: FORM_TARGET }),
      saveConfig: async () => undefined,
      concepts: () => new Map(),
      openRow: () => undefined,
    },
    FORM_TARGET,
  );
  const panel = recorded.webviews.find((p) => p.viewType === "memqlRun");
  assert.ok(panel !== undefined);
  panel.send({ type: "input", field: "spaceId", value: "01J8Z2QK6N" });
  panel.send({ type: "run" });
  await settle();
  assert.equal(
    recorded.webviews.filter((p) => p.viewType === "memqlRunResult").length,
    0,
    "a run nobody confirmed opened a Result tab",
  );
  assert.match(visible(panel.html), /Cancelled\. Nothing ran\./);
  assert.match(visible(panel.html), /Ready/);
  panel.close();
});

test("Save as on a Result saves the run it shows, with the values it ran with", async () => {
  resetRecorded();
  const saved: { name: string; values: Record<string, unknown> }[] = [];
  const host: RunPanelHost = {
    run: async () => ({ status: "declined", target: TARGET }),
    saveConfig: async (_target, name, values) => {
      saved.push({ name, values });
    },
    concepts: () => new Map(),
    openRow: () => undefined,
  };
  ResultPanel.show(
    CONTEXT,
    host,
    { status: "ok", target: TARGET, rows: [], raw: [], ranDeployedDefinition: false, injected: true },
    { limit: 5 },
  );
  const panel = lastPanel();
  assert.match(panel.html, /data-act="saveAs"/);
  setNextInputBoxResult("recent five");
  panel.send({ type: "saveAs" });
  await settle();
  await settle();
  assert.deepEqual(saved, [{ name: "recent five", values: { limit: 5 } }]);
  assert.ok(recorded.infos.some((m) => m.includes('Saved "recent five"')));
  panel.close();
});

test("the run host shows the running Result from the orchestrator's start, not from the click", () => {
  const source = fs.readFileSync(path.join(__dirname, "..", "..", "src", "extension.ts"), "utf8");
  const at = source.indexOf("const host: RunPanelHost = {");
  const body = source.slice(at, source.indexOf("saveConfig:", at));
  assert.match(body, /orchestrator\.run\(target, values, started\)/);
  assert.match(body, /ResultPanel\.running\(context, host, target\)/);
  const show = source.slice(source.indexOf("const runAndShow"), source.indexOf("};", source.indexOf("const runAndShow")));
  assert.doesNotMatch(show, /ResultPanel\.running/, "the Runs view opens Running before any confirmation");
  const form = fs.readFileSync(path.join(__dirname, "..", "..", "src", "webview", "runPanel.ts"), "utf8");
  const doRun = form.slice(form.indexOf("private async doRun()"), form.indexOf("private async doSave()"));
  assert.doesNotMatch(doRun, /ResultPanel\.running/, "the form opens Running before any confirmation");
});

// ---------------------------------------------------------------------------
// The language reference
// ---------------------------------------------------------------------------

test("the language reference's search is drawn by the host and keeps its term", async () => {
  resetRecorded();
  LanguageReferencePanel.open(CONTEXT, {
    pin: { edition: "2026", status: "frozen", grammarVersion: "2026.09-example-0123abcd", version: "0.6.0" },
    canSelectCluster: () => true,
    reader: () => ({
      name: "local",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      executeNamed: async (_name, call) => ({
        rows: () =>
          call.includes("Grammar")
            ? [{ format: "ebnf", edition: "2026", grammarVersion: "g", content: "<file> ::= <use>*\n<query> ::= \"query\"\n" }]
            : [{ edition: "2026", grammarVersion: "g", kinds: ["construct"], entries: [{ kind: "construct", name: "query", signature: "s", description: "d" }] }],
      }),
    }),
  });
  await settle();
  const panel = lastPanel();
  assert.match(panel.html, /2 productions/);
  panel.send({ type: "input", field: "search", value: "query" });
  assert.match(panel.html, /1 of 2 productions/, "the host did not narrow the lists");
  assert.match(panel.html, /data-field="search" value="query"/);
  panel.close();
});

// ---------------------------------------------------------------------------
// A saved run with no file
// ---------------------------------------------------------------------------

test("a saved run with no file is resolved from the cluster's catalog", () => {
  const automation: CatalogConstruct = {
    ...QUERY,
    name: "autoJoinSI",
    kind: "automation",
    runnableKind: "automation",
    trigger: { event: "node.created", concept: "v1:cognition:participant" },
  };
  const catalog = [QUERY, automation];

  const run = savedRunCatalogTarget({ kind: "query", construct: "spaceParticipants" }, catalog);
  assert.ok(run !== undefined && "run" in run, "the saved query found no target");
  assert.equal(run.run.name, "spaceParticipants");
  assert.match(run.run.uri, /^memql-catalog:/, "the run is the cluster's definition, not a file");

  const auto = savedRunCatalogTarget({ kind: "automation", construct: "autoJoinSI" }, catalog);
  assert.ok(auto !== undefined && "automation" in auto);
  assert.deepEqual(auto.automation.trigger, { event: "node.created", concept: "v1:cognition:participant" });

  assert.equal(savedRunCatalogTarget({ kind: "query", construct: "gone" }, catalog), undefined);
});

// ---------------------------------------------------------------------------
// The run form
// ---------------------------------------------------------------------------

test("a required boolean is a switch, and one left off runs as false rather than 'Required'", async () => {
  resetRecorded();
  const sent: Record<string, unknown>[] = [];
  RunPanel.open(
    CONTEXT,
    {
      run: async (_target, values) => {
        sent.push(values);
        return { status: "declined", target: FORM_TARGET };
      },
      saveConfig: async () => undefined,
      concepts: () => new Map(),
      openRow: () => undefined,
    },
    FORM_TARGET,
  );
  const panel = recorded.webviews.find((p) => p.viewType === "memqlRun");
  assert.ok(panel !== undefined);
  assert.match(panel.html, /role="switch"[^>]*data-field="includeArchived"/);
  // Typed into the text field, one input message per keystroke.
  panel.send({ type: "input", field: "spaceId", value: "01J8Z2QK6N" });
  panel.send({ type: "run" });
  await settle();
  assert.deepEqual(sent, [{ spaceId: "01J8Z2QK6N", includeArchived: false }]);
  panel.close();
  recorded.webviews.find((p) => p.viewType === "memqlRunResult")?.close();
});

const FORM_TARGET = {
  uri: "file:///w/q.memql",
  kind: "query" as const,
  name: "spaceParticipants",
  args: [
    { name: "spaceId", type: "string" as const, required: true },
    { name: "includeArchived", type: "boolean" as const, required: true },
  ],
};
