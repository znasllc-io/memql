// The versioned connection seam used by MemQL Productivity Tools on both hosts.
// Credentials remain private to MemQL. Every operation carries the connection
// lease it was opened under: selecting A -> B -> A never revives an old save.
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { apiBaseUrlFor } from "./endpoint.js";
import type { ClusterConfig } from "../clusters/model.js";

export interface ConnectionLease { readonly domain: string; readonly name: string; readonly generation: number }
export interface EditorConnectionAPI {
  readonly version: 1;
  current(): ConnectionLease | undefined;
  onDidChange(listener: () => void): { dispose(): void };
  connect(domain: string): Promise<ConnectionLease>;
  execute(lease: ConnectionLease, name: string, generatedCall: string): Promise<Record<string, unknown>[]>;
  readBytes(lease: ConnectionLease, artifactId: string, version?: number): Promise<Uint8Array>;
  uploadFile(lease: ConnectionLease, filename: string, mimeType: string, content: Uint8Array): Promise<{ fileId: string; artifactId: string }>;
  uploadVersion(lease: ConnectionLease, artifactId: string, filename: string, mimeType: string,
    content: Uint8Array, expectedVersion: number): Promise<number>;
}
export interface EditorSession {
  cluster: ClusterConfig;
  query: Pick<QueryClient, "executeNamed">;
  bearer: string;
}
export interface EditorConnectionDeps {
  session(): EditorSession | undefined;
  connect(domain: string): Promise<void>;
  fetch?: typeof fetch;
}
export const MAX_EDITOR_BYTES = 32 * 1024 * 1024;

export class EditorConnection implements EditorConnectionAPI {
  readonly version = 1 as const;
  private generation = 0;
  private previous: EditorSession["query"] | undefined;
  private listeners = new Set<() => void>();
  constructor(private readonly deps: EditorConnectionDeps) {}

  // Called for every lifecycle transition, including disconnect and reconnect.
  changed(): void {
    this.generation++;
    this.previous = this.deps.session()?.query;
    for (const listener of this.listeners) listener();
  }
  current(): ConnectionLease | undefined {
    const session = this.deps.session();
    if (session?.query !== this.previous) this.changed();
    if (!session?.cluster.domain) return undefined;
    return Object.freeze({ domain: session.cluster.domain.toLowerCase(), name: session.cluster.name, generation: this.generation });
  }
  onDidChange(listener: () => void): { dispose(): void } {
    this.listeners.add(listener);
    return { dispose: () => { this.listeners.delete(listener); } };
  }
  async connect(domain: string): Promise<ConnectionLease> {
    domain = validateEditorDomain(domain);
    if (this.current()?.domain !== domain) await this.deps.connect(domain);
    const lease = this.current();
    if (!lease || lease.domain !== domain) throw new Error("Connect to this file's cluster in MemQL first.");
    return lease;
  }
  private require(lease: ConnectionLease): EditorSession {
    const current = this.current();
    if (!current || current.domain !== lease.domain || current.name !== lease.name || current.generation !== lease.generation) {
      throw new Error("The MemQL connection changed. Keep your edits, reconnect to the original cluster, and compare with its latest version before saving.");
    }
    return this.deps.session()!;
  }
  async execute(lease: ConnectionLease, name: string, generatedCall: string): Promise<Record<string, unknown>[]> {
    const result = await this.require(lease).query.executeNamed(name, generatedCall);
    this.require(lease);
    return result.rows();
  }
  async readBytes(lease: ConnectionLease, artifactId: string, version?: number): Promise<Uint8Array> {
    const session = this.require(lease);
    const base = apiBaseUrlFor(session.cluster);
    if (!base) throw new Error("This cluster has no file address.");
    if (version !== undefined && (!Number.isSafeInteger(version) || version < 1)) throw new Error("Invalid file version.");
    const suffix = version === undefined ? "" : `?version=${version}`;
    const response = await (this.deps.fetch ?? fetch)(`${base}/artifacts/${encodeURIComponent(artifactId)}/content${suffix}`, {
      headers: { Authorization: `Bearer ${session.bearer}` }, credentials: "omit", redirect: "error", cache: "no-store",
    });
    this.require(lease);
    if (!response.ok) throw new Error(`Unable to read this file (${response.status}).`);
    if (Number(response.headers.get("content-length")) > MAX_EDITOR_BYTES) throw new Error("This file is larger than the 32 MiB editor limit. Download it from Files.");
    const chunks: Uint8Array[] = [];
    let total = 0;
    const reader = response.body?.getReader();
    if (!reader) throw new Error("This file returned no content stream.");
    try {
      for (;;) {
        const part = await reader.read();
        if (part.done) break;
        total += part.value.length;
        if (total > MAX_EDITOR_BYTES) throw new Error("This file is larger than the 32 MiB editor limit. Download it from Files.");
        chunks.push(part.value);
      }
    } finally { await reader.cancel(); }
    this.require(lease);
    const content = new Uint8Array(total);
    let offset = 0;
    for (const chunk of chunks) { content.set(chunk, offset); offset += chunk.length; }
    return content;
  }
  async uploadVersion(lease: ConnectionLease, artifactId: string, filename: string, mimeType: string,
    content: Uint8Array, expectedVersion: number): Promise<number> {
    const session = this.require(lease);
    const base = apiBaseUrlFor(session.cluster);
    if (!base || content.byteLength > MAX_EDITOR_BYTES) throw new Error("This file cannot be saved through the editor (32 MiB limit).");
    if (!Number.isSafeInteger(expectedVersion) || expectedVersion < 1) throw new Error("Read this file's version before saving.");
    const body = new FormData();
    body.append("file", new Blob([new Uint8Array(content)], { type: mimeType }), filename);
    body.append("name", filename);
    body.append("targetArtifactId", artifactId);
    body.append("expectedVersion", String(expectedVersion));
    const response = await (this.deps.fetch ?? fetch)(`${base}/artifacts`, {
      method: "POST", body, headers: { Authorization: `Bearer ${session.bearer}` }, credentials: "omit", redirect: "error",
    });
    this.require(lease);
    if (response.status === 409) throw new Error("This file changed in MemQL. Compare with the latest version before saving again.");
    if (!response.ok) throw new Error(`Unable to save this file (${response.status}). Your edits are still in the editor.`);
    const result = await response.json() as { versionNumber?: number };
    this.require(lease);
    if (result.versionNumber !== expectedVersion + 1) throw new Error("The cluster did not confirm the expected new version. Keep your edits and refresh the file's history.");
    return result.versionNumber;
  }
  async uploadFile(lease: ConnectionLease, filename: string, mimeType: string, content: Uint8Array): Promise<{ fileId: string; artifactId: string }> {
    const session = this.require(lease);
    const base = apiBaseUrlFor(session.cluster);
    if (!base || content.byteLength > MAX_EDITOR_BYTES || !filename || /[\\/\r\n\0]/.test(filename)) throw new Error("Choose an individual file up to 32 MiB.");
    const body = new FormData();
    body.append("file", new Blob([new Uint8Array(content)], { type: mimeType }), filename);
    body.append("name", filename);
    const response = await (this.deps.fetch ?? fetch)(`${base}/artifacts`, { method: "POST", body,
      headers: { Authorization: `Bearer ${session.bearer}` }, credentials: "omit", redirect: "error" });
    this.require(lease);
    if (!response.ok) throw new Error(`Unable to upload this reference (${response.status}).`);
    const result = await response.json() as Record<string, unknown>;
    this.require(lease);
    if (typeof result.fileId !== "string" || !result.fileId) throw new Error("The cluster did not return a saved file receipt. Check Files before retrying.");
    return { fileId: result.fileId, artifactId: typeof result.artifactId === "string" ? result.artifactId : "" };
  }
}

export function validateEditorDomain(raw: string): string {
  const domain = raw.trim().toLowerCase().replace(/\.$/, "");
  if (domain.length > 253 || !domain.split(".").every(label => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) {
    throw new Error("Enter the cluster domain, without a URL, port, or path.");
  }
  return domain;
}
