// A cluster is named the way the Clusters view names it, on every surface.
//
// THE DEFECT. The registry key of every local cluster is "local" -- a slot
// name, not a name anyone gave it. The Clusters view shows the display name
// (`memql.localhost`), and the Deployment page's head, its tab, the Deployments
// view's description, the construct page's action bar ("Loaded on local"), the
// read-only lens on a cluster document and the refusals all said "local". One
// cluster, two names, on screens a person moves between in one click.
//
// THE RULE these cases hold: compare by key, SAY the label. Every act still
// carries the key, because two clusters can share a label and never a key.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext, TextDocument, Uri } from "vscode";

import type { ClustersFile } from "../src/clusters/model.js";
import { detailsRefusal, panelClusterRefusal } from "../src/constructs/clusterDocument.js";
import { ClusterDocumentLens } from "../src/constructs/clusterDocuments.js";
import { LocalRuns, doneTitle } from "../src/deploy/localRun.js";
import { localInstance, instanceLabel, remoteInstance } from "../src/state/deployments.js";
import { selectedViewDescription } from "../src/state/deploymentsCatalog.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import { ConstructPanel } from "../src/webview/constructPanel.js";
import { DeploymentPanel } from "../src/webview/deploymentPanel.js";
import { recorded, resetRecorded } from "./support/vscodeStub.js";

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;
const LABEL = "memql.localhost";

// ---------------------------------------------------------------------------
// the instance's label
// ---------------------------------------------------------------------------

test("a listed local cluster is labelled by its display name, and keeps its key", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: null,
    registered: { name: "local", displayName: LABEL, domain: LABEL },
    connected: true,
  });
  assert.equal(instance.name, "local", "the key is what everything compares");
  assert.equal(instanceLabel(instance), LABEL);
});

test("an installed local cluster that is not listed is labelled by its domain", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: null,
    registered: { domain: LABEL },
    connected: false,
  });
  assert.equal(instanceLabel(instance), LABEL);
});

test("with nothing to go on, a local cluster is called that rather than by its key", () => {
  const instance = localInstance({ presence: "absent", receipt: null, connected: false });
  assert.equal(instance.name, "local");
  assert.equal(instanceLabel(instance), "Local cluster");
});

test("a remote cluster is labelled by its display name, else its key", () => {
  assert.equal(instanceLabel(remoteInstance({ name: "staging", displayName: "Staging", reachable: true, connected: true })), "Staging");
  assert.equal(instanceLabel(remoteInstance({ name: "staging", reachable: true, connected: true })), "staging");
});

test("the Deployments view's description leads with the label", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: null,
    registered: { name: "local", displayName: LABEL, domain: LABEL, version: "v0.23.5" },
    connected: true,
  });
  const description = selectedViewDescription(instance, undefined, "connected");
  assert.ok(description.startsWith(`${LABEL} · `), description);
  assert.doesNotMatch(description, /^local\b/);
});

// ---------------------------------------------------------------------------
// the Deployment page
// ---------------------------------------------------------------------------

const LISTED: ClustersFile = {
  clusters: [
    { name: "local", displayName: LABEL, endpoint: `api.${LABEL}:443`, domain: LABEL, local: true, version: "v0.23.5" },
  ],
  selectedCluster: "local",
};

async function settle(): Promise<void> {
  for (let i = 0; i < 20; i += 1) await new Promise((resolve) => setImmediate(resolve));
}

test("the Deployment page's head and tab name the cluster by its label", async () => {
  resetRecorded();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memql-names-"));
  DeploymentPanel.show(CONTEXT, {
    catalog: {
      clustersPath: path.join(dir, "clusters.yaml"),
      receiptPath: path.join(dir, "install-receipt.json"),
      runsDir: path.join(dir, "runs"),
      presence: async () => ({ verdict: "installed-healthy", evidence: { receipt: true, registry: true, liveCluster: false }, endpoint: "" }),
      readClusters: async () => ({ ok: true as const, file: LISTED }),
      readReceiptFile: async () => null,
      listRunsIn: async () => [],
    },
    installRoot: dir,
    receiptFile: path.join(dir, "install-receipt.json"),
    runsDir: path.join(dir, "runs"),
    refreshTree: () => undefined,
    openInstallFlow: () => undefined,
    runs: new LocalRuns(),
    home: dir,
  });
  await settle();
  const panel = recorded.webviews.at(-1)!;
  assert.equal(panel.title, LABEL, "the tab");
  const head = /<h1 class="mq-title">([^<]*)<\/h1>/.exec(panel.html)?.[1];
  assert.equal(head, LABEL, "the head");
  panel.close();
});

test("a run's done title says the label, and falls back to the key only without one", () => {
  // The run is filed under the key; its sentences use the label. The toast a
  // run raises when its page is hidden is this title.
  assert.equal(doneTitle({ kind: "update", instance: "local", label: LABEL, from: "v0.23.5", to: "v0.24.0" }), `${LABEL} is on v0.24.0`);
  assert.equal(doneTitle({ kind: "changeVersion", instance: "local", from: "v0.23.5", to: "v0.23.4" }), "local is on v0.23.4");
});

// ---------------------------------------------------------------------------
// the construct page, the lens, the refusals
// ---------------------------------------------------------------------------

const QUERY: CatalogConstruct = {
  kind: "query",
  name: "spaceParticipants",
  namespace: "cognition",
  origin: "bundle",
  originPath: "cognition/queries.memql",
  description: "",
  runnable: true,
  runnableKind: "query",
  args: [],
  boundConcept: "",
  sourceHash: "",
  source: "",
} as unknown as CatalogConstruct;

test("the construct page's action bar says where it is loaded by the label", () => {
  resetRecorded();
  ConstructPanel.open(
    CONTEXT,
    QUERY,
    {
      viewSourceFromCluster: async () => undefined,
      browseRows: async () => undefined,
      openInOs: async () => undefined,
      run: async () => undefined,
      clusterLabel: (name) => (name === "local" ? LABEL : name),
    },
    "local",
  );
  const panel = recorded.webviews.at(-1)!;
  assert.match(panel.html, new RegExp(`Loaded on ${LABEL.replace(".", "\\.")}`));
  assert.doesNotMatch(panel.html, /Loaded on local\b/);
  panel.close();
});

test("the read-only lens names the label, and its act still carries the key", () => {
  const document = {
    uri: { authority: "local", path: "/cognition/queries.memql", query: "kind=query&name=spaceParticipants" } as unknown as Uri,
  } as unknown as TextDocument;
  const lenses = new ClusterDocumentLens((name) => (name === "local" ? LABEL : name)).provideCodeLenses(document);
  assert.equal(lenses[0]?.command?.title, `From ${LABEL} (read-only)`);
  assert.deepEqual(lenses[1]?.command?.arguments, [{ cluster: "local", kind: "query", name: "spaceParticipants" }]);
});

test("a refusal compares keys and names labels", () => {
  const label = (name: string): string => ({ local: LABEL, staging: "Staging" })[name] ?? name;
  assert.equal(detailsRefusal("local", "local", undefined, label), undefined, "same key, no refusal");
  const crossed = String(detailsRefusal("local", "staging", undefined, label));
  assert.match(crossed, /This is from memql\.localhost, but you're connected to Staging\./);
  assert.doesNotMatch(crossed, /\blocal\b/);
  const gone = String(panelClusterRefusal("local", undefined, "read its source", label));
  assert.match(gone, /Connect to memql\.localhost to read its source/);
});
