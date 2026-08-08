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

export async function upsertCluster(file: string, cluster: ClusterConfig): Promise<void> {
  const doc = await loadDocument(file);
  let seq = doc.get("clusters", true) as YAMLSeq | undefined;
  if (seq === undefined || !Array.isArray(seq.items)) {
    doc.set("clusters", []);
    seq = doc.get("clusters", true) as YAMLSeq;
  }

  const existing = seq.items.find((item) => {
    const map = item as YAMLMap;
    return typeof map?.get === "function" && map.get("name", false) === cluster.name;
  }) as YAMLMap | undefined;

  if (existing !== undefined) {
    // Set only the fields we were given. Every other key on the node -- including
    // one a newer cockpit wrote and this version does not model -- is left alone.
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      if (v !== undefined) existing.set(wireKey, v);
    }
  } else {
    const fresh: Record<string, string> = {};
    for (const [modelKey, wireKey] of FIELD_MAP) {
      const v = cluster[modelKey];
      if (v !== undefined) fresh[wireKey] = v;
    }
    seq.add(fresh);
  }

  await saveDocument(file, doc);
}
