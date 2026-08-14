// Pushing the cluster catalog to the language server (memql#3759).
//
// The rule every case here circles is NULL IS NOT AN EMPTY LIST. `null` means
// there is no cluster and the server answers `unknown` for every construct,
// which renders as nothing; `[]` means a connected cluster that has loaded
// nothing and the server answers `untrained` for everything, which renders as a
// file full of Promote affordances. The two are one character apart on the wire
// and worlds apart on screen, and NEITHER of them throws, logs, or looks wrong
// from inside this process -- so the only place the difference can be caught is
// a test that asserts on the bytes.
//
// The second theme is that every failure publishes `null`. A catalog read that
// threw is not a catalog, and the honest report of "I could not find out" is the
// same as "there is nothing to find out".

import test from "node:test";
import assert from "node:assert/strict";

import {
  CLUSTER_CATALOG_METHOD,
  ClusterCatalogPublisher,
  clusterCatalogParams,
  type CatalogSourceConstruct,
} from "../src/constructs/clusterCatalog.js";
import { TRAINING_STATE_CAPABILITY } from "../src/state/training.js";

/** A catalog entry with the fields the SDK's `Construct` carries that matter. */
function entry(name: string, kind: string, origin: string, sourceHash: string): CatalogSourceConstruct {
  return { name, kind, origin, sourceHash };
}

/** A recording client that always advertises the capability. */
function recordingClient(): {
  client: {
    sendNotification(method: string, params: unknown): Promise<void>;
    experimentalCapabilities(): Record<string, unknown> | undefined;
  };
  sent: { method: string; params: unknown }[];
} {
  const sent: { method: string; params: unknown }[] = [];
  return {
    sent,
    client: {
      sendNotification: async (method: string, params: unknown) => {
        sent.push({ method, params });
      },
      experimentalCapabilities: () => ({ [TRAINING_STATE_CAPABILITY]: true }),
    },
  };
}

test("an absent catalog and an empty one are different payloads", () => {
  assert.deepEqual(clusterCatalogParams(undefined), { catalog: null });
  assert.deepEqual(clusterCatalogParams([]), { catalog: [] });
  // Stated as its own assertion because this is the whole point: if these two
  // ever became equal, a disconnection would decorate every construct in the
  // open file as untrained and nothing anywhere would report an error.
  assert.notDeepEqual(clusterCatalogParams(undefined), clusterCatalogParams([]));
});

test("the payload carries exactly the four fields the state rules read", () => {
  const params = clusterCatalogParams([
    // Extra fields on the source object stand in for the rest of the SDK's
    // Construct -- description, args, runnable, originPath. None may travel:
    // the catalog runs to four figures and this payload is pushed whole.
    {
      ...entry("v1:cognition:space", "concept", "core", "abc123"),
      description: "a space",
      args: [],
    } as CatalogSourceConstruct,
  ]);
  assert.deepEqual(params, {
    catalog: [{ name: "v1:cognition:space", kind: "concept", origin: "core", sourceHash: "abc123" }],
  });
});

test("a connected cluster's catalog is pushed on the agreed method", async () => {
  const { client, sent } = recordingClient();
  const publisher = new ClusterCatalogPublisher(async () => [
    entry("spaceParticipants", "query", "core", "hash-a"),
    entry("promotedThing", "mutation", "promoted", "hash-b"),
  ]);
  publisher.setClient(client);
  await publisher.refresh();

  assert.ok(sent.length >= 1, "nothing was pushed");
  const last = sent[sent.length - 1];
  assert.equal(last.method, CLUSTER_CATALOG_METHOD);
  assert.deepEqual(last.params, {
    catalog: [
      { name: "spaceParticipants", kind: "query", origin: "core", sourceHash: "hash-a" },
      { name: "promotedThing", kind: "mutation", origin: "promoted", sourceHash: "hash-b" },
    ],
  });
});

test("no cluster pushes null, not an empty catalog", async () => {
  const { client, sent } = recordingClient();
  const publisher = new ClusterCatalogPublisher(async () => undefined);
  publisher.setClient(client);
  await publisher.refresh();

  assert.ok(sent.length >= 1, "nothing was pushed");
  assert.deepEqual(sent[sent.length - 1].params, { catalog: null });
});

test("a catalog read that throws pushes null", async () => {
  const { client, sent } = recordingClient();
  const publisher = new ClusterCatalogPublisher(async () => {
    throw new Error("the cluster went away mid-read");
  });
  publisher.setClient(client);
  // The assertion is as much that this does not reject: it is driven by
  // connection events, which have nowhere to report an error to.
  await publisher.refresh();

  assert.ok(sent.length >= 1, "a failed read pushed nothing at all");
  assert.deepEqual(sent[sent.length - 1].params, { catalog: null });
});

test("a connected cluster that has loaded nothing pushes an empty list", async () => {
  const { client, sent } = recordingClient();
  const publisher = new ClusterCatalogPublisher(async () => []);
  publisher.setClient(client);
  await publisher.refresh();

  assert.deepEqual(sent[sent.length - 1].params, { catalog: [] });
});

test("nothing is pushed with no client, or to a server that does not advertise the capability", async () => {
  const sent: unknown[] = [];
  const record = async (_method: string, params: unknown) => {
    sent.push(params);
  };

  const noClient = new ClusterCatalogPublisher(async () => []);
  await noClient.refresh();
  assert.equal(sent.length, 0, "pushed with no client attached");

  const older = new ClusterCatalogPublisher(async () => []);
  older.setClient({
    sendNotification: record,
    // An older memql-lsp on the PATH. Not an error worth a popup -- an
    // unhandled notification is silently dropped on both sides -- so the
    // surface is simply absent.
    experimentalCapabilities: () => ({}),
  });
  await older.refresh();
  assert.equal(sent.length, 0, "pushed to a server that never advertised the method");

  const preInitialize = new ClusterCatalogPublisher(async () => []);
  preInitialize.setClient({ sendNotification: record, experimentalCapabilities: () => undefined });
  await preInitialize.refresh();
  assert.equal(sent.length, 0, "pushed before initialize returned any capabilities");
});

test("the newest refresh wins when two overlap", async () => {
  // A cluster switch is a disconnect immediately followed by a connect, so two
  // refreshes overlapping is the ordinary case rather than a race worth
  // shrugging at. The loser landing last would leave the server holding the
  // catalog of a cluster this editor is no longer talking to.
  const { client, sent } = recordingClient();
  let release: (() => void) | undefined;
  const slow = new Promise<void>((resolve) => {
    release = resolve;
  });

  let call = 0;
  const publisher = new ClusterCatalogPublisher(async () => {
    call += 1;
    if (call === 1) {
      await slow;
      return [entry("fromTheOldCluster", "query", "core", "old")];
    }
    return [entry("fromTheNewCluster", "query", "core", "new")];
  });
  publisher.setClient(client);
  sent.length = 0;

  const first = publisher.refresh();
  const second = publisher.refresh();
  await second;
  release?.();
  await first;

  assert.equal(sent.length, 1, "the superseded refresh pushed anyway");
  assert.deepEqual(sent[0].params, {
    catalog: [{ name: "fromTheNewCluster", kind: "query", origin: "core", sourceHash: "new" }],
  });
});

test("a disposed publisher pushes nothing", async () => {
  const { client, sent } = recordingClient();
  const publisher = new ClusterCatalogPublisher(async () => [entry("q", "query", "core", "h")]);
  publisher.setClient(client);
  await publisher.refresh();
  const before = sent.length;

  publisher.dispose();
  await publisher.refresh();
  assert.equal(sent.length, before, "pushed after dispose");
});

test("a send that fails is swallowed", async () => {
  // The server has gone away or is restarting. A restarted server holds no
  // catalog, which is `unknown`, which renders as nothing -- so there is
  // nothing to repair and nobody to tell.
  const publisher = new ClusterCatalogPublisher(async () => []);
  publisher.setClient({
    sendNotification: async () => {
      throw new Error("connection closed");
    },
    experimentalCapabilities: () => ({ [TRAINING_STATE_CAPABILITY]: true }),
  });
  await publisher.refresh();
});
