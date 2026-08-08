// Reading and writing ~/.memql/clusters.yaml.
//
// This file is SHARED with the memQL Cockpit, which is the reason every write
// goes through the yaml Document API rather than a parse-and-serialize round
// trip. A naive rewrite would strip the operator's comments and drop any key a
// newer cockpit writes that this version does not model -- silent data loss on
// something as routine as selecting a cluster.
//
// Writes are read-modify-write against the file as it is on disk at write
// time, never against a cached parse, so a concurrent cockpit edit is merged
// rather than clobbered.

import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { Document, parseDocument, type YAMLMap, type YAMLSeq } from "yaml";

import type { ClusterConfig, ClustersFile } from "./model.js";

export function defaultClustersPath(): string {
  return path.join(os.homedir(), ".memql", "clusters.yaml");
}

// IDENTITY_FIELD is the one field every other reference in this file resolves
// a node BY: findByName matches on it, selected_cluster points at it, and the
// tree's select/edit commands carry it. It is therefore exempt from the
// "" -> delete rule below (see upsertCluster), and the exemption is enforced
// at the boundary -- an empty name is rejected outright rather than written.
const IDENTITY_FIELD: keyof ClusterConfig = "name";

// Wire spellings. The YAML uses snake_case; the model uses camelCase.
const FIELD_MAP: ReadonlyArray<readonly [keyof ClusterConfig, string]> = [
  ["name", "name"],
  ["displayName", "display_name"],
  ["domain", "domain"],
  ["endpoint", "endpoint"],
  ["issuer", "issuer"],
  ["clientId", "client_id"],
  ["pat", "pat"],
];

async function loadDocument(file: string): Promise<Document> {
  let raw: string;
  try {
    raw = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return new Document({ clusters: [] });
    }
    throw err;
  }
  if (raw.trim() === "") {
    return new Document({ clusters: [] });
  }
  const doc = parseDocument(raw);
  if (doc.errors.length > 0) {
    throw new Error(
      `clusters.yaml at ${file} is malformed: ${doc.errors[0]?.message ?? "parse error"}`,
    );
  }
  return doc;
}

function stringAt(map: YAMLMap, key: string): string | undefined {
  const v = map.get(key, false);
  return typeof v === "string" ? v : undefined;
}

function clusterFromNode(node: unknown): ClusterConfig | null {
  const map = node as YAMLMap;
  if (typeof map?.get !== "function") return null;
  const name = stringAt(map, "name");
  if (name === undefined) return null;
  const out: ClusterConfig = { name, endpoint: stringAt(map, "endpoint") ?? "" };
  for (const [modelKey, wireKey] of FIELD_MAP) {
    if (modelKey === "name" || modelKey === "endpoint") continue;
    const v = stringAt(map, wireKey);
    if (v !== undefined) {
      (out as unknown as Record<string, unknown>)[modelKey] = v;
    }
  }
  return out;
}

export async function readClustersFile(file: string): Promise<ClustersFile> {
  const doc = await loadDocument(file);
  const seq = doc.get("clusters", true) as YAMLSeq | undefined;
  const items = Array.isArray(seq?.items) ? seq.items : [];
  const clusters: ClusterConfig[] = [];
  for (const item of items) {
    const c = clusterFromNode(item);
    if (c !== null) clusters.push(c);
  }
  const selected = doc.get("selected_cluster", false);
  return {
    clusters,
    selectedCluster: typeof selected === "string" ? selected : "",
  };
}

// ReadClustersResult is the outcome of a "read clusters, or describe the
// failure" attempt -- a pure, vscode-free helper so a UI layer that must
// never let this rejection reach it (e.g. a TreeDataProvider.getChildren,
// which VS Code has no built-in way to surface an unhandled rejection from)
// can render the failure instead of erroring silently. readClustersFile
// deliberately throws on a malformed file (see the "rejects a malformed
// file" test); this wraps that call so a caller with no try/catch story of
// its own gets a value instead of a rejection.
export type ReadClustersResult =
  | { ok: true; file: ClustersFile }
  | { ok: false; error: string };

export async function readClustersFileSafe(file: string): Promise<ReadClustersResult> {
  try {
    return { ok: true, file: await readClustersFile(file) };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

async function saveDocument(file: string, doc: Document): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.writeFile(file, doc.toString(), "utf8");
}

export async function setSelectedCluster(file: string, name: string): Promise<void> {
  const doc = await loadDocument(file);
  doc.set("selected_cluster", name);
  await saveDocument(file, doc);
}

function findByName(seq: YAMLSeq, name: string): YAMLMap | undefined {
  return seq.items.find((item) => {
    const map = item as YAMLMap;
    return typeof map?.get === "function" && map.get("name", false) === name;
  }) as YAMLMap | undefined;
}

// upsertCluster writes `cluster` into the registry.
//
// `originalName` identifies WHICH node to update, and must be supplied by an
// edit flow whose name field the user can change. Without it the node is
// matched by the NEW name, so renaming during an edit matched nothing and
// appended a SECOND entry -- leaving the original orphaned and possibly still
// referenced by selected_cluster.
//
// Per-field semantics, which the caller has to be deliberate about:
//   undefined -> the field was not supplied; whatever is on disk is left alone.
//   ""        -> the field was explicitly CLEARED; the key is DELETED.
// The distinction is not cosmetic. Treating "" as "not supplied" meant a user
// who cleared the PAT input to revoke a token kept the old token on disk while
// the UI showed the field empty -- "I removed my token from the extension" was
// simply false.
//
// The IDENTITY FIELD is exempt from that rule, and the exemption is enforced
// here at the boundary rather than as a skip inside the write loop. `name` is
// what every other reference resolves a node by, so deleting it does not
// clear a value -- it orphans the whole node: `upsertCluster(f, {name: ""},
// "local")` used to find "local", delete its `name` key, and leave an
// unreachable, unselectable entry still holding that cluster's endpoint and
// PAT. No caller has a use for it (the add/edit form validates the name box
// non-empty, so this is unreachable through the UI), and there is no
// interpretation of an anonymous cluster that is better than a refusal, so an
// empty name is rejected outright -- which means the delete branch below can
// never see one.
export async function upsertCluster(
  file: string,
  cluster: ClusterConfig,
  originalName?: string,
): Promise<void> {
  if (cluster[IDENTITY_FIELD] === "") {
    throw new Error(
      "a cluster name is required: an empty name would orphan the node's endpoint and PAT behind an entry nothing can resolve",
    );
  }
  const doc = await loadDocument(file);
  let seq = doc.get("clusters", true) as YAMLSeq | undefined;
  if (seq === undefined || !Array.isArray(seq.items)) {
    doc.set("clusters", []);
    seq = doc.get("clusters", true) as YAMLSeq;
  }

  const renaming = originalName !== undefined && originalName !== cluster.name;
  // Refuse to merge two nodes into one. Writing the rename anyway would leave
  // two entries sharing a name, and every lookup here matches the FIRST -- so
  // the loser becomes unreachable and unselectable rather than visibly wrong.
  if (renaming && findByName(seq, cluster.name) !== undefined) {
    throw new Error(
      `cannot rename cluster "${originalName}" to "${cluster.name}": that name is already taken`,
    );
  }

  const existing = findByName(seq, originalName ?? cluster.name);

  if (existing !== undefined) {
    // Set only the fields we were given. Every other key on the node -- including
    // one a newer cockpit wrote and this version does not model -- is left alone.
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      if (v === undefined) continue;
      if (v === "") existing.delete(wireKey);
      else existing.set(wireKey, v);
    }
    // A rename moves the identity the rest of the file refers to by name.
    // selected_cluster is the one such reference; leaving it behind would
    // point the selection at a cluster that no longer exists.
    if (renaming && doc.get("selected_cluster", false) === originalName) {
      doc.set("selected_cluster", cluster.name);
    }
  } else {
    const fresh: Record<string, string> = {};
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      // "" on a NEW node has nothing to clear, so it is simply not written --
      // same end state as a delete, without an empty key in fresh YAML.
      if (v !== undefined && v !== "") fresh[wireKey] = v;
    }
    seq.add(fresh);
  }

  await saveDocument(file, doc);
}

// addCluster is upsertCluster's ADD-only front door: it refuses a name that
// already exists instead of updating that entry.
//
// The distinction matters because "Add Cluster" and "Edit Cluster" collect
// the SAME four-field form, and add has no originalName to pass. So a user who
// typed an existing name into the add flow -- a plausible slip, since the add
// form shows no existing names -- landed in upsertCluster's update branch, and
// every field the add form left blank (the PAT they did not retype, the domain
// they skipped) arrived as "" and was DELETED off the real cluster. "I tried to
// add a cluster" silently revoked the token on the one already there.
//
// Refusing is the right half of the fix rather than merging-without-deleting:
// add and edit are different intents, and quietly turning an add into an edit
// is exactly how the destructive case arose. The message points at the edit
// flow, which is where the user can see and change every field.
//
// The existence check re-reads the file, which upsertCluster then reads again.
// That is the file's model, not an oversight: every write here is
// read-modify-write against the bytes on disk at write time (the cockpit
// writes this file too), so no read is authoritative for longer than the call
// that made it, and caching one would not make the check any less racy.
export async function addCluster(file: string, cluster: ClusterConfig): Promise<void> {
  const existing = await readClustersFile(file);
  if (existing.clusters.some((c) => c.name === cluster.name)) {
    throw new Error(
      `a cluster named "${cluster.name}" already exists; edit it instead of adding it again`,
    );
  }
  await upsertCluster(file, cluster);
}
