import {
  buildLibraryArtifactById, buildLibraryFileById, buildGeneratedOutputById,
  buildTemplateById, buildCampaignSaveTemplate, buildDocumentVersions, buildEditDocument, buildLibraryDocumentReview, buildLibraryAddDocumentComment, buildLibraryRequestDocumentRevision, buildLibraryDocumentRevisionStatus, buildDecideApproval,
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
  template?: { name: string; accountId: string; status: string };
}
const text = (row: Record<string, unknown>, field: string) => typeof row[field] === "string" ? row[field] as string : "";

export class Documents {
  constructor(private readonly api: EditorConnectionAPI) {}
  async read(uri: string, openedLease?: ConnectionLease): Promise<OpenDocument> {
    const resource = resourceFrom(uri);
    const lease = openedLease ?? await this.api.connect(resource.domain);
    if (lease.domain !== resource.domain) throw new Error("This file belongs to another cluster.");
    if (resource.kind === "templates") {
      const row = (await this.api.execute(lease, "templateById", buildTemplateById({ templateId: resource.id })))[0];
      if (!row) throw new Error("Template not found or unavailable to your organization.");
      const revision = text(row, "createdAt");
      if (!revision) throw new Error("The cluster did not return the template revision.");
      const content = new TextEncoder().encode(JSON.stringify({ subject: text(row,"subject"), textBody: text(row,"textBody"), htmlBody: text(row,"htmlBody") }, null, 2) + "\n");
      return { resource, lease, content, mime: "application/json", sourceId: resource.id, kind: "campaign_template", version: 0, revision,
        template: { name: text(row,"name"), accountId: text(row,"accountId"), status: text(row,"status") } };
    }
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
  async requestRevision(document: OpenDocument, commentIds: string[], instruction: string, requestId: string): Promise<Record<string, unknown>> {
    const result = (await this.api.execute(document.lease, "libraryRequestDocumentRevision", buildLibraryRequestDocumentRevision({
      artifactId: document.resource.id, expectedVersion: document.version, expectedRevision: document.revision ?? "", commentIds, instruction, requestId,
    })))[0];
    if (!result?.approvalId || !result.proposal) throw new Error("The cluster did not confirm the review request. Retry to recover it.");
    return result;
  }
  async revision(document: OpenDocument, requestId: string): Promise<Record<string, unknown>> {
    const result = (await this.api.execute(document.lease, "libraryDocumentRevisionStatus", buildLibraryDocumentRevisionStatus({ requestId })))[0];
    const proposal = result?.proposal as Record<string, unknown> | undefined;
    if (!proposal || proposal.artifactId !== document.resource.id) throw new Error("This revision request does not belong to the open document.");
    return result;
  }
  async decideRevision(document: OpenDocument, requestId: string, approvalId: string, decision: "approved" | "rejected"): Promise<void> {
    const current = await this.revision(document, requestId);
    if (current.approvalId !== approvalId) throw new Error("The approval changed. Review the request again.");
    const result = (await this.api.execute(document.lease, "decideApproval", buildDecideApproval({ approvalId, decision })))[0];
    if (result?.decision !== decision) throw new Error("The cluster did not confirm your decision. Refresh or retry to recover it.");
    if (result.resumeError) throw new Error("Your decision was saved, but the job could not start. Retry to recover the same job.");
  }
  async revisionDraft(document: OpenDocument, status: Record<string, unknown>): Promise<OpenDocument> {
    if (status.compositionStatus !== "ready" || typeof status.outputArtifactId !== "string") throw new Error("The revised draft is not ready yet. Refresh its status.");
    const name = typeof status.outputName === "string" ? status.outputName : "revised.md";
    return this.read(`memql-file://${document.resource.domain}/artifacts/${encodeURIComponent(status.outputArtifactId)}/${encodeURIComponent(name.replace(/[\\/]/g, "_"))}`, document.lease);
  }
  async save(document: OpenDocument, content: Uint8Array): Promise<void> {
    if (isZip(document.resource.name, document.mime, document.content)) throw new Error("ZIP files are downloads; they cannot be edited or extracted here.");
    if (document.kind === "campaign_template" && document.template) {
      const result = (await this.api.execute(document.lease, "campaignSaveTemplate", buildCampaignSaveTemplate({
        templateId: document.sourceId, accountId: document.template.accountId, name: document.template.name,
        content: JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(content)), expectedRevision: document.revision || "", action: "save",
      })))[0];
      if (!result?.saved || typeof result.revision !== "string") throw new Error("The cluster did not confirm the saved template revision.");
      document.revision = result.revision;
      document.template.status = "draft";
    } else if (document.kind === "file") {
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
