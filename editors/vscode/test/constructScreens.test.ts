// The construct page.
//
// THE PAGE'S JOB IS TO HAVE NO DEAD END, AND TO OFFER EACH ACT ONCE. Open
// source is one act: the host knows whether the file is in this workspace or
// has to be read from the cluster, so the page no longer offers two buttons for
// one intent -- and a promoted construct, which has no file, shows its source
// on the page and offers no act at all. Run lives in the action bar, which a
// view-only kind does not have. A concept's rows open in the editor and in
// MemQL OS.
//
// Refs: #4248 #3752

import test from "node:test";
import assert from "node:assert/strict";

import {
  CONSTRUCT_ACTS,
  constructFailedParts,
  constructLoadingParts,
  constructMeta,
  constructPageParts,
  type ConstructPageInput,
} from "../src/webview/constructScreens.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";

function construct(over: Partial<CatalogConstruct> = {}): CatalogConstruct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "cognition/queries.memql",
    description: "",
    runnable: false,
    args: [],
    boundConcept: "",
    sourceHash: "",
    source: "",
    ...over,
  };
}

function page(over: Partial<ConstructPageInput> = {}): string {
  const parts = constructPageParts({ construct: construct(), cluster: "local", source: "workspace", ...over });
  return parts.head + parts.body + parts.actions;
}

const act = (name: string): string => `data-act="${name}"`;

test("Open source is ONE act, offered when the file is here or on the cluster", () => {
  for (const source of ["workspace", "cluster"] as const) {
    const html = page({ source });
    assert.equal(html.split(act(CONSTRUCT_ACTS.openSource)).length - 1, 1, `${source}: not exactly one Open source`);
  }
  // The retired pair is gone.
  assert.doesNotMatch(page(), /View source from cluster|Open the \.memql file/);
});

test("Open source waits for the workspace lookup rather than offering a guess", () => {
  assert.equal(page({ source: "checking" }).includes(act(CONSTRUCT_ACTS.openSource)), false);
});

test("a promoted construct shows its source on the page and offers no fetch of a file it does not have", () => {
  const html = page({
    construct: construct({ origin: "promoted", originPath: "", namespace: "", source: "logic x { }" }),
    source: "none",
  });
  assert.equal(html.includes(act(CONSTRUCT_ACTS.openSource)), false);
  assert.match(html, /logic x \{ \}/);
  // No standing explanation beside it.
  assert.doesNotMatch(html, /What follows is what the cluster holds/);
});

test("Run lives in the action bar, one button, and a view-only kind has no bar at all", () => {
  const runnable = constructPageParts({
    construct: construct({ runnable: true, runnableKind: "query", args: [{ name: "spaceId", type: "string", required: true }] }),
    cluster: "local",
    source: "workspace",
  });
  assert.ok(runnable.actions.includes(act(CONSTRUCT_ACTS.run)));
  assert.ok(runnable.actions.includes(act(CONSTRUCT_ACTS.runWith)));
  assert.equal(runnable.actions.split('data-tone="primary"').length - 1, 1, "more than one primary");
  assert.match(runnable.actions, /Loaded on local/);
  assert.equal(runnable.body.includes(act(CONSTRUCT_ACTS.run)), false, "Run is drawn outside the bar");

  const viewOnly = constructPageParts({ construct: construct({ kind: "spec" }), cluster: "local", source: "workspace" });
  assert.equal(viewOnly.actions, "", "a view-only kind drew an action bar");
});

test("an automation's Run says it opens a form, and it draws no argument section", () => {
  const parts = constructPageParts({
    construct: construct({ kind: "automation", runnable: true, runnableKind: "automation" }),
    cluster: "local",
    source: "workspace",
  });
  assert.match(parts.actions, />Run\.\.\.</);
  assert.equal(parts.actions.includes(act(CONSTRUCT_ACTS.runWith)), false);
  assert.doesNotMatch(parts.body, /Arguments/);
});

test("a staged construct's bar says who can call it", () => {
  const parts = constructPageParts({
    construct: construct({ origin: "staged", originPath: "", namespace: "", runnable: true, runnableKind: "query" }),
    cluster: "local",
    source: "none",
  });
  assert.match(parts.actions, /Staged on local/);
  assert.match(parts.actions, /Only you can call it/);
});

test("a concept offers its rows in the editor and in MemQL OS, and no other kind does", () => {
  const conceptHtml = page({ construct: construct({ kind: "concept", name: "v1:cognition:space" }) });
  assert.ok(conceptHtml.includes(act(CONSTRUCT_ACTS.browseRows)));
  assert.ok(conceptHtml.includes(act(CONSTRUCT_ACTS.openInOs)));
  for (const kind of ["query", "mutation", "automation", "tool", "spec", "shape", "prompt", "provider"]) {
    const html = page({ construct: construct({ kind }) });
    assert.equal(html.includes(act(CONSTRUCT_ACTS.browseRows)), false, `${kind} offers rows`);
    assert.equal(html.includes(act(CONSTRUCT_ACTS.openInOs)), false, `${kind} offers MemQL OS`);
  }
});

test("say it once: kind and namespace in the meta, no placeholder sentences, details behind a disclosure", () => {
  assert.equal(constructMeta(construct()), "Query · cognition · Built in");
  // Promoted says where it lives in the bar, not the meta.
  assert.equal(constructMeta(construct({ origin: "promoted", namespace: "" })), "Query");
  const html = page({ construct: construct({ sourceHash: "abc123" }) });
  assert.doesNotMatch(html, /No description\.|takes no arguments|namespace.*none/);
  assert.match(html, /Arguments<span class="mq-subhead-meta">None/);
  // The file path and the hash are details: rendered, and closed.
  assert.match(html, /aria-expanded="false"[^>]*>.*Details/);
  assert.match(html, /abc123/);
});

test("an argument shows its type and flags in words", () => {
  const html = page({
    construct: construct({
      args: [
        { name: "mode", type: "string", required: true, enum: ["a", "b"], description: "Which mode" },
        { name: "tenant", type: "string", required: false, autoInjected: true },
      ],
    }),
  });
  assert.match(html, /string · Required · a \| b/);
  assert.match(html, /Set by the cluster/);
  assert.doesNotMatch(html, /auto-injected|one of:/);
  assert.match(html, /Which mode/);
});

test("the loading page is the shape of the content, and a failed read offers Try again", () => {
  const loading = constructLoadingParts();
  assert.match(loading.body, /mq-skeleton/);
  // The words are for screen readers only: nothing visible says "Loading".
  const visible = loading.body.replace(/<span class="mq-sr">[^<]*<\/span>/g, "");
  assert.doesNotMatch(visible, /Loading/, "loading is painted as words");
  const failed = constructFailedParts("spaceParticipants", "Couldn't read this cluster's constructs.");
  assert.match(failed.body, /role="alert"/);
  assert.ok(failed.body.includes(act(CONSTRUCT_ACTS.retry)));
});
