// What the read-only marking LOOKS LIKE (memql#4244).
//
// constructs/readonly.ts is where every decision lives and readonly.test.ts is
// where the decisions are asserted. This is the other half: the FileDecoration
// the marker builds, which is the only part of the feature an operator sees,
// and which has failed silently before -- the badge argument was passed in the
// tooltip's position, so a read-only file carried a colour and a hover and no
// badge at all, and nothing said so.
//
// The editor is the vscode stub (test/support/vscodeStub.ts, aliased by
// esbuild.test.js), which deliberately does NOT reproduce
// `FileDecoration.validate`: the real one throws on a badge longer than two
// code points and the extension host then drops the decoration and logs a
// warning nobody reads. That length rule is asserted against `reasonBadge`
// itself, in readonly.test.ts, where it can fail loudly.

import test from "node:test";
import assert from "node:assert/strict";

import { ReadonlyMarker } from "../src/constructs/readonlyDecorations.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import { Uri, workspace, writtenSettings } from "./support/vscodeStub.js";

const CHECKOUT = "/home/me/.memql/src";
const OTHER_CLONE = "/home/me/work/memql";

function construct(over: Partial<CatalogConstruct> = {}): CatalogConstruct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "cognition/queries.memql",
    description: "",
    runnable: true,
    args: [],
    boundConcept: "participant",
    sourceHash: "abc",
    source: "",
    ...over,
  };
}

const CATALOG = [construct(), construct({ name: "orders", origin: "bundle", originPath: "shop/queries.memql" })];

/** The workspaceState the marker records its own settings keys in. */
function fakeMemento(): never {
  const store = new Map<string, unknown>();
  return {
    get: (key: string, fallback?: unknown) => store.get(key) ?? fallback,
    update: (key: string, value: unknown) => {
      store.set(key, value);
      return Promise.resolve();
    },
    keys: () => [...store.keys()],
  } as never;
}

/**
 * A marker over one workspace folder, pointed at one cluster.
 *
 * `constructs` is passed at every call site rather than defaulted, because the
 * NOT-CONNECTED case is `undefined` -- and a parameter default fires on an
 * explicit `undefined`, so a default here would silently hand that case the
 * catalog it is meant to be without.
 */
async function markerIn(
  folder: string,
  cluster: { name: string; local?: boolean; checkout?: string } | undefined,
  constructs: readonly CatalogConstruct[] | undefined,
): Promise<ReadonlyMarker> {
  writtenSettings.clear();
  workspace.workspaceFolders = [{ uri: Uri.file(folder) }];
  const marker = new ReadonlyMarker(fakeMemento());
  await marker.update(constructs, cluster);
  return marker;
}

function decorationFor(marker: ReadonlyMarker, folder: string, rel: string) {
  return marker.provideFileDecoration(Uri.file(`${folder}/${rel}`) as never);
}

/**
 * The theme colour's id.
 *
 * Cast, because `vscode.ThemeColor` exposes no readable member in the public
 * API -- the editor resolves it internally. The stub keeps the id it was
 * constructed with, and WHICH colour is the point here: grey (disabled), not a
 * warning colour, because a read-only file is not a problem with the file.
 */
function colorId(decoration: { color?: unknown } | undefined): string | undefined {
  return (decoration?.color as { id?: string } | undefined)?.id;
}

test("a read-only file carries a badge, a hover and the greyed colour -- all three", () => {
  // THE REGRESSION. The badge was passed where the tooltip goes and then
  // overwritten, so the one mark a developer could see at a glance never
  // rendered.
  return markerIn(OTHER_CLONE, { name: "staging", local: false }, CATALOG).then((marker) => {
    const core = decorationFor(marker, OTHER_CLONE, "dsl/cognition/queries.memql");
    assert.ok(core !== undefined, "a core file on a remote cluster must be decorated");
    assert.equal(core?.badge, "C");
    assert.match(String(core?.tooltip), /Core engine DSL -- read-only/);
    assert.equal(colorId(core), "disabledForeground");

    const bundle = decorationFor(marker, OTHER_CLONE, "dsl/shop/queries.memql");
    assert.equal(bundle?.badge, "R");
    assert.match(String(bundle?.tooltip), /product bundle/);
    // Two reasons, two marks: a single badge for both would make the hover the
    // only way to tell a sealed file from one that another cluster would unlock.
    assert.notEqual(core?.badge, bundle?.badge);
  });
});

test("a local cluster locks nothing, and says so on the file rather than in the settings", async () => {
  const marker = await markerIn(OTHER_CLONE, { name: "local", local: true, checkout: CHECKOUT }, CATALOG);
  const core = decorationFor(marker, OTHER_CLONE, "dsl/cognition/queries.memql");
  assert.equal(core?.badge, "L");
  assert.match(String(core?.tooltip), /not the checkout local rebuilds from \(\/home\/me\/\.memql\/src\)/);
  assert.equal(core?.propagate, false);
  // Nothing written: the hint is a hover, and `files.readonlyInclude` is for
  // files this editor is actually marking read-only.
  assert.deepEqual(writtenSettings.get("files.readonlyInclude"), {});
});

test("in the cluster's own checkout there is no decoration at all", async () => {
  // The ordinary case. A badge on every construct file here would be a
  // permanent mark, and a permanent mark stops being read.
  const marker = await markerIn(CHECKOUT, { name: "local", local: true, checkout: CHECKOUT }, CATALOG);
  assert.equal(decorationFor(marker, CHECKOUT, "dsl/cognition/queries.memql"), undefined);
  assert.equal(decorationFor(marker, CHECKOUT, "dsl/shop/queries.memql"), undefined);
  assert.deepEqual(writtenSettings.get("files.readonlyInclude"), {});
});

test("a file the cluster never loaded is untouched, in either clone", async () => {
  // The training path: a new file is never marked, and never hinted at either --
  // it reaches a cluster by being promoted, not by sitting in a directory.
  for (const folder of [CHECKOUT, OTHER_CLONE]) {
    const marker = await markerIn(folder, { name: "local", local: true, checkout: CHECKOUT }, CATALOG);
    assert.equal(decorationFor(marker, folder, "dsl/mine/newThing.memql"), undefined, folder);
  }
});

test("with no cluster, nothing is decorated and the settings are cleared", async () => {
  // NOT CONNECTED is `update(undefined, undefined)` -- an absent catalog, not an
  // empty one and not a catalog with no cluster beside it. It is the call
  // extension.ts makes when the dispatcher is gone or the fetch failed, and
  // getting it wrong locks a developer's checkout on the authority of a call
  // that did not answer.
  const marker = await markerIn(OTHER_CLONE, undefined, undefined);
  assert.equal(decorationFor(marker, OTHER_CLONE, "dsl/cognition/queries.memql"), undefined);
  assert.deepEqual(writtenSettings.get("files.readonlyInclude"), {});
});

test("a remote cluster writes both spellings of every read-only path", async () => {
  // The catalog reports `cognition/queries.memql`; this checkout holds it at
  // `dsl/cognition/queries.memql`. `files.readonlyInclude` matches what VS Code
  // sees, and a pattern for a path that does not exist is inert.
  const marker = await markerIn(OTHER_CLONE, { name: "staging", local: false }, CATALOG);
  assert.deepEqual(writtenSettings.get("files.readonlyInclude"), {
    "cognition/queries.memql": true,
    "dsl/cognition/queries.memql": true,
    "dsl/shop/queries.memql": true,
    "shop/queries.memql": true,
  });
  assert.ok(marker !== undefined);
});

test("a key the operator added by hand survives a rewrite", async () => {
  // It writes into somebody's repository, so it removes exactly the keys it
  // wrote last time and leaves every other one alone.
  writtenSettings.clear();
  writtenSettings.set("files.readonlyInclude", { "notes/**": true });
  workspace.workspaceFolders = [{ uri: Uri.file(OTHER_CLONE) }];
  const marker = new ReadonlyMarker(fakeMemento());

  await marker.update(CATALOG, { name: "staging", local: false });
  assert.equal((writtenSettings.get("files.readonlyInclude") as Record<string, boolean>)["notes/**"], true);

  // ...and selecting the local cluster withdraws ours, keeping theirs.
  await marker.update(CATALOG, { name: "local", local: true, checkout: CHECKOUT });
  assert.deepEqual(writtenSettings.get("files.readonlyInclude"), { "notes/**": true });
});
