// The cluster-document adapter's two decisions that are NOT in the pure module:
// what the header lens hands the command it posts, and what the provider does
// when the fetch fails.
//
// Both are reachable from a plain Node process because the adapter touches a
// narrow slice of `vscode` (an EventEmitter, a CodeLens, a Range) that
// test/support/vscodeStub.ts supplies, and because its connection is a
// STRUCTURAL dependency rather than a live ConnectionManager. The document and
// the uri are plain objects: the provider reads three fields off a Uri and the
// lens reads the same three, so faking them is more honest than driving the
// stub's Uri, which models neither an authority nor a query.
//
// Refs: #4248

import test from "node:test";
import assert from "node:assert/strict";

import type { TextDocument, Uri } from "vscode";

import {
  ClusterDocumentLens,
  ClusterDocumentProvider,
  type ClusterDocumentDeps,
} from "../src/constructs/clusterDocuments.js";

/** The three fields both halves read off a Uri, plus the cache key. */
function fakeUri(): Uri {
  return {
    toString: () => "memql-cluster://staging/cognition/queries.memql?kind=query&name=spaceParticipants",
    authority: "staging",
    path: "/cognition/queries.memql",
    query: "kind=query&name=spaceParticipants",
  } as unknown as Uri;
}

test("the header lens carries the cluster the document came from, not just the construct key", () => {
  // WITHOUT THE CLUSTER the command resolves the key against whatever is
  // connected now, so a click on a staging document while connected to prod
  // renders prod's construct of the same name with nothing saying so.
  const document = { uri: fakeUri() } as unknown as TextDocument;
  const lenses = new ClusterDocumentLens().provideCodeLenses(document);

  assert.equal(lenses.length, 1);
  assert.equal(lenses[0]?.command?.command, "memql.constructs.showDetails");
  assert.deepEqual(lenses[0]?.command?.arguments, [
    { cluster: "staging", kind: "query", name: "spaceParticipants" },
  ]);
  assert.match(String(lenses[0]?.command?.title), /staging/);
});

test("a lens is offered for nothing but a cluster document", () => {
  const document = { uri: { authority: "", path: "/", query: "" } } as unknown as TextDocument;
  assert.deepEqual(new ClusterDocumentLens().provideCodeLenses(document), []);
});

test("a failed fetch renders a notice, reports the failure once, and is not cached", async () => {
  // THE RE-FETCH HAS NO CALLER. invalidate() makes VS Code ask again, and a
  // rejection there would surface as the editor's own raw error text -- no
  // classification, no redaction, no channel line. So the provider answers with
  // a notice and hands the raw detail to the host through onError.
  const reported: [string, string][] = [];
  let calls = 0;
  const deps = {
    connections: {
      state: { status: "connected", clusterName: "staging", nodeId: "n1" },
      dispatcher: {
        sendAndWait: () => {
          calls += 1;
          return Promise.reject(new Error("stream closed"));
        },
      },
      onDidChangeState: () => () => undefined,
    },
    onError: (headline: string, detail: string) => {
      reported.push([headline, detail]);
    },
  } as unknown as ClusterDocumentDeps;

  const provider = new ClusterDocumentProvider(deps);
  try {
    const text = await provider.provideTextDocumentContent(fakeUri());
    assert.match(text, /could not be read/);
    assert.match(text, /staging/);
    assert.equal(/stream closed/.test(text), false, "the raw error text reached the document");

    assert.equal(reported.length, 1, `onError fired ${reported.length} times`);
    assert.match(reported[0]![0], /staging/);
    assert.match(reported[0]![1], /stream closed/);

    // Not cached: the next open must try the cluster again rather than serve a
    // statement about a failure that may be long over.
    await provider.provideTextDocumentContent(fakeUri());
    assert.equal(calls, 2, "the failure notice was cached");
  } finally {
    provider.dispose();
  }
});

test("a provider with no onError still answers the notice rather than throwing", async () => {
  // onError is OPTIONAL so `new ClusterDocumentProvider({ connections })` keeps
  // typechecking for callers that have no channel to report into.
  // The cast is on the CONNECTIONS only, so the argument object itself is
  // typechecked: this case fails to compile the day `onError` stops being
  // optional, which is what Tasks 5 and 6 build on.
  const provider = new ClusterDocumentProvider({
    connections: {
      state: { status: "connected", clusterName: "staging", nodeId: "n1" },
      dispatcher: { sendAndWait: () => Promise.reject(new Error("boom")) },
      onDidChangeState: () => () => undefined,
    } as unknown as ClusterDocumentDeps["connections"],
  });
  try {
    assert.match(await provider.provideTextDocumentContent(fakeUri()), /could not be read/);
  } finally {
    provider.dispose();
  }
});
