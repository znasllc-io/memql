import {
  buildLibraryArtifactById, buildLibraryFileById, buildGeneratedOutputById,
  buildDocumentVersions, buildEditDocument, buildLibraryDocumentReview, buildLibraryAddDocumentComment,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

export interface Resource { domain: string; kind: "artifacts" | "templates"; id: string; name: string }
export function resourceFrom(uri: string): Resource {
  const parsed = new URL(uri);
  const segments = parsed.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parsed.protocol !== "memql-file:" || parsed.username || parsed.password || parsed.port || parsed.search || parsed.hash ||
      !parsed.hostname || segments.length !== 3 || !["artifacts", "templates"].includes(segments[0]) || !/^[\w-]{1,160}$/.test(segments[1])) {
    throw new Error("Invalid MemQL file link.");
  }
  if (!segments[2] || /[\\/\u0000-\u001f]/.test(segments[2])) throw new Error("Invalid file name.");
  return { domain: parsed.hostname.toLowerCase(), kind: segments[0] as Resource["kind"], id: segments[1], name: segments[2] };
}
export function isZip(name: string, mime = "", bytes?: Uint8Array): boolean {
  return /\.zip$/i.test(name) || /^(application\/zip|application\/x-zip-compressed)(;|$)/i.test(mime) ||
    !!(bytes && bytes[0] === 0x50 && bytes[1] === 0x4b && ((bytes[2] === 3 && bytes[3] === 4) || (bytes[2] === 5 && bytes[3] === 6) || (bytes[2] === 7 && bytes[3] === 8)));
}
export interface OpenDocument {
  resource: Resource; lease: ConnectionLease; content: Uint8Array; mime: string;
  sourceId: string; kind: string; version: number; revision?: string;
}
const text = (row: Record<string, unknown>, field: string) => typeof row[field] === "string" ? row[field] as string : "";

export class Documents {
  constructor(private readonly api: EditorConnectionAPI) {}
  async read(uri: string): Promise<OpenDocument> {
    const resource = resourceFrom(uri);
    const lease = await this.api.connect(resource.domain);
    if (resource.kind !== "artifacts") throw new Error("This template link requires the campaign template editor capability.");
    const artifact = (await this.api.execute(lease, "libraryArtifactById", buildLibraryArtifactById({ artifactId: resource.id })))[0];
    if (!artifact || artifact.archived) throw new Error("File not found or unavailable to your account.");
    const sourceId = text(artifact, "sourceConceptRef");
    const kind = text(artifact, "kind");
    let mime = text(artifact, "mimeType");
    let version = 0;
    let revision: string | undefined;
    let content: Uint8Array;
    if (kind === "file") {
      const file = (await this.api.execute(lease, "libraryFileById", buildLibraryFileById({ fileId: sourceId })))[0];
      if (!file) throw new Error("File not found or unavailable to your account.");
      mime = text(file, "mimeType") || mime;
      resource.name = text(file, "name") || resource.name;
      version = typeof file.versionNumber === "number" && file.versionNumber > 0 ? file.versionNumber : 1;
      content = await this.api.readBytes(lease, resource.id, version);
      revision = `file:${version}`;
    } else if (kind === "generated_output") {
      const output = (await this.api.execute(lease, "generatedOutputById", buildGeneratedOutputById({ outputId: sourceId })))[0];
      if (!output) throw new Error("Document not found or unavailable to your account.");
      const versions = await this.api.execute(lease, "documentVersions", buildDocumentVersions({ documentId: sourceId }));
      version = Math.max(0, ...versions.map(row => Number(row.versionNumber) || 0));
      content = new TextEncoder().encode(text(output, "body"));
      revision = text(output, "createdAt");
      if (!revision) throw new Error("The cluster did not return this document's revision.");
    } else {
      content = await this.api.readBytes(lease, resource.id);
    }
    return { resource, lease, sourceId, kind, content, mime, version, revision };
  }
  async review(document: OpenDocument): Promise<Record<string, unknown>> {
    const rows = await this.api.execute(document.lease, "libraryDocumentReview", buildLibraryDocumentReview({ artifactId: document.resource.id }));
    if (!rows[0]) throw new Error("The cluster did not return document feedback.");
    return rows[0];
  }
  async comment(document: OpenDocument, anchor: Record<string, unknown>, body: string, requestId: string): Promise<void> {
    const result = await this.api.execute(document.lease, "libraryAddDocumentComment", buildLibraryAddDocumentComment({
      artifactId: document.resource.id, expectedVersion: document.version, expectedRevision: document.revision ?? "",
      anchor, body, requestId,
    }));
    if (!result[0]?.saved) throw new Error("The cluster did not confirm this comment. Retry to recover its receipt.");
  }
  async save(document: OpenDocument, content: Uint8Array): Promise<void> {
    if (isZip(document.resource.name, document.mime, document.content)) throw new Error("ZIP files are downloads; they cannot be edited or extracted here.");
    if (document.kind === "file") {
      document.version = await this.api.uploadVersion(document.lease, document.resource.id, document.resource.name,
        document.mime || "application/octet-stream", content, document.version);
      document.revision = `file:${document.version}`;
    } else if (document.kind === "generated_output") {
      const body = new TextDecoder("utf-8", { fatal: true }).decode(content);
      const result = (await this.api.execute(document.lease, "editDocument", buildEditDocument({
        documentId: document.sourceId, content: body, expectedVersion: document.version, expectedRevision: document.revision, authorKind: "user",
      })))[0];
      if (!result || result.conflict) throw new Error("This document changed. Compare with its latest version before saving.");
      if (Number(result.newVersion) !== document.version + 1) throw new Error("The cluster did not confirm the saved version.");
      document.version = Number(result.newVersion);
      document.revision = String(result.revision || "");
    } else throw new Error("This kind of record cannot be edited as a file yet.");
    document.content = new Uint8Array(content);
  }
}
