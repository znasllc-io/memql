import { UserInputError } from "./problems.js";
import {
  buildLibraryArtifactForFile,
  buildLibraryDocumentNotes, buildLibraryAddDocumentNote, buildLibraryDocumentHistory, buildLibraryDocumentVersion, buildLibraryForkDocumentVersion, buildCancelGoal, buildLibraryModifyRevisionItem, buildLibraryArtifactById, buildLibraryFileById, buildGeneratedOutputById,
  buildTemplateById, buildCampaignSaveTemplate, buildDocumentVersions, buildEditDocument, buildLibraryDocumentReview, buildLibraryAddDocumentComment, buildLibraryRequestDocumentRevision, buildLibraryDocumentRevisionStatus, buildDecideApproval,
} from "@znasllc-io/memql-sdk-core/client";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

export interface Resource { domain: string; kind: "artifacts" | "templates"; id: string; name: string }
export function resourceFrom(uri: string): Resource {
  const parsed = new URL(uri);
  const segments = parsed.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parsed.protocol !== "memql-file:" || parsed.username || parsed.password || parsed.port || parsed.search || parsed.hash ||
      !parsed.hostname || segments.length !== 3 || !["artifacts", "templates"].includes(segments[0]) || !/^[\w-]{1,160}$/.test(segments[1])) {
    throw new UserInputError("Invalid MemQL file link.");
  }
  if (!segments[2] || /[\\/\u0000-\u001f]/.test(segments[2])) throw new UserInputError("Invalid file name.");
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
  async createFile(lease: ConnectionLease, name: string, mime: string, content: Uint8Array): Promise<string> {
    if (!name.trim() || name.length > 160 || /[\\/\u0000-\u001f]/.test(name)) throw new UserInputError("Use a file name without slashes or control characters.");
    const result = await this.api.uploadFile(lease, name, mime, content);
    // The upload receipt precedes the asynchronously indexed Files artifact.
    // Resolve that exact receipt; never upload again while waiting for its link.
    let artifactId = result.artifactId;
    for (let attempt = 0; !artifactId && attempt < 10; attempt++) {
      const rows = await this.api.execute(lease, "libraryArtifactForFile", buildLibraryArtifactForFile({ fileId: result.fileId }));
      artifactId = text(rows[0] ?? {}, "id");
      if (!artifactId) await new Promise(resolve => setTimeout(resolve, 500));
    }
    if (!artifactId) throw new UserInputError("The file is saved in Files, but its editor link is still being prepared. Open it from Files when it appears.");
    if (!/^[\w-]{1,160}$/.test(artifactId)) throw new Error("The cluster returned an invalid file link. The uploaded file remains in Files.");
    const uri = `memql-file://${lease.domain}/artifacts/${artifactId}/${encodeURIComponent(name)}`;
    resourceFrom(uri);
    return uri;
  }
  async read(uri: string, openedLease?: ConnectionLease): Promise<OpenDocument> {
    const resource = resourceFrom(uri);
    const lease = openedLease ?? await this.api.connect(resource.domain);
    if (lease.domain !== resource.domain) throw new UserInputError("This file belongs to another cluster.");
    if (resource.kind === "templates") {
      const row = (await this.api.execute(lease, "templateById", buildTemplateById({ templateId: resource.id })))[0];
      if (!row) throw new UserInputError("Template not found or unavailable to your organization.");
      const revision = text(row, "createdAt");
      if (!revision) throw new Error("The cluster did not return the template revision.");
      const content = new TextEncoder().encode(JSON.stringify({ subject: text(row,"subject"), textBody: text(row,"textBody"), htmlBody: text(row,"htmlBody") }, null, 2) + "\n");
      return { resource, lease, content, mime: "application/json", sourceId: resource.id, kind: "campaign_template", version: 0, revision,
        template: { name: text(row,"name"), accountId: text(row,"accountId"), status: text(row,"status") } };
    }
    const artifact = (await this.api.execute(lease, "libraryArtifactById", buildLibraryArtifactById({ artifactId: resource.id })))[0];
    if (!artifact || artifact.archived) throw new UserInputError("File not found or unavailable to your account.");
    const sourceId = text(artifact, "sourceConceptRef");
    const kind = text(artifact, "kind");
    let mime = text(artifact, "mimeType");
    let version = 0;
    let revision: string | undefined;
    let content: Uint8Array;
    if (kind === "file") {
      const file = (await this.api.execute(lease, "libraryFileById", buildLibraryFileById({ fileId: sourceId })))[0];
      if (!file) throw new UserInputError("File not found or unavailable to your account.");
      mime = text(file, "mimeType") || mime;
      resource.name = text(file, "name") || resource.name;
      version = typeof file.versionNumber === "number" && file.versionNumber > 0 ? file.versionNumber : 1;
      content = await this.api.readBytes(lease, resource.id, version);
      revision = `file:${version}`;
    } else if (kind === "generated_output") {
      const output = (await this.api.execute(lease, "generatedOutputById", buildGeneratedOutputById({ outputId: sourceId })))[0];
      if (!output) throw new UserInputError("Document not found or unavailable to your account.");
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
  async history(document: OpenDocument, beforeVersion?: number): Promise<Record<string, unknown>> {
    const rows = await this.api.execute(document.lease, "libraryDocumentHistory", buildLibraryDocumentHistory({artifactId:document.resource.id,beforeVersion}));
    if (!rows[0]) throw new Error("Version history could not be loaded.");
    return rows[0];
  }
  async version(document: OpenDocument, versionNumber: number): Promise<{version:number;revision:string;content:string}> {
    const row = (await this.api.execute(document.lease,"libraryDocumentVersion",buildLibraryDocumentVersion({artifactId:document.resource.id,versionNumber})))[0];
    if (!row || row.version !== versionNumber || typeof row.content !== "string" || typeof row.revision !== "string") throw new Error("This version could not be opened.");
    return row as {version:number;revision:string;content:string};
  }
  async fork(document: OpenDocument, versionNumber:number, revision:string, name:string, requestId:string):Promise<string> {
    const row=(await this.api.execute(document.lease,"libraryForkDocumentVersion",buildLibraryForkDocumentVersion({artifactId:document.resource.id,versionNumber,revision,name,requestId})))[0];
    if (!row || typeof row.artifactId!=="string" || !/^[\w-]{1,160}$/.test(row.artifactId) || typeof row.name!=="string") throw new Error("The branch was not confirmed. Retry to recover it.");
    return this.artifactURI(document, row.artifactId, row.name);
  }
  artifactURI(document: OpenDocument, id:string, name:string):string {
    if (!/^[\w-]{1,160}$/.test(id)) throw new UserInputError("Invalid document link.");
    const filename=/\.(md|markdown)$/i.test(name)?name:name+".md";
    return `memql-file://${document.resource.domain}/artifacts/${id}/${encodeURIComponent(filename)}`;
  }
  async notes(document:OpenDocument):Promise<Record<string,unknown>> {
    const row=(await this.api.execute(document.lease,"libraryDocumentNotes",buildLibraryDocumentNotes({artifactId:document.resource.id})))[0];
    if(!row)throw new Error("Your notes could not be loaded.");return row;
  }
  async note(document:OpenDocument,anchor:Record<string,unknown>,body:string,requestId:string):Promise<void>{
    const row=(await this.api.execute(document.lease,"libraryAddDocumentNote",buildLibraryAddDocumentNote({artifactId:document.resource.id,expectedVersion:document.version,expectedRevision:document.revision??"",anchor,body,requestId})))[0];
    if(!row?.saved)throw new Error("Your note was not confirmed. Retry to recover it.");
  }
  async review(document: OpenDocument): Promise<Record<string, unknown>> {
    const rows = await this.api.execute(document.lease, "libraryDocumentReview", buildLibraryDocumentReview({ artifactId: document.resource.id }));
    if (!rows[0]) throw new Error("The cluster did not return document feedback.");
    return rows[0];
  }
  async transcribe(document:OpenDocument,audio:ReadableStream<Uint8Array>,signal:AbortSignal,onPartial:(text:string)=>void):Promise<string>{
    return this.api.transcribe(document.lease,audio,signal,onPartial);
  }
  async cancelRevision(document:OpenDocument,requestId:string):Promise<void>{
    const status=await this.revision(document,requestId);
    if(typeof status.goalId!=="string")throw new Error("This revision has no work receipt.");
    await this.api.execute(document.lease,"cancelGoal",buildCancelGoal({goalId:status.goalId,reason:"Stopped document revision preparation."}));
  }
  async comment(document: OpenDocument, anchor: Record<string, unknown>, body: string, requestId: string, attachments: Record<string, unknown>[] = []): Promise<void> {
    const result = await this.api.execute(document.lease, "libraryAddDocumentComment", buildLibraryAddDocumentComment({
      artifactId: document.resource.id, expectedVersion: document.version, expectedRevision: document.revision ?? "",
      anchor, body, requestId, attachments,
    }));
    if (!result[0]?.saved) throw new Error("The cluster did not confirm this comment. Retry to recover its receipt.");
  }
  async requestRevision(document: OpenDocument, commentIds: string[], instruction: string, requestId: string): Promise<Record<string, unknown>> {
    const result = (await this.api.execute(document.lease, "libraryRequestDocumentRevision", buildLibraryRequestDocumentRevision({
      artifactId: document.resource.id, expectedVersion: document.version, expectedRevision: document.revision ?? "", commentIds, instruction, requestId,
    })))[0];
    if (!result?.runId || !result.proposal) throw new Error("The cluster did not confirm the review request. Retry to recover it.");
    return result;
  }
  async revision(document: OpenDocument, requestId: string): Promise<Record<string, unknown>> {
    const result = (await this.api.execute(document.lease, "libraryDocumentRevisionStatus", buildLibraryDocumentRevisionStatus({ requestId })))[0];
    const proposal = result?.proposal as Record<string, unknown> | undefined;
    if (!proposal || proposal.artifactId !== document.resource.id) throw new UserInputError("This revision request does not belong to the open document.");
    return result;
  }
  async modifyRevision(document: OpenDocument, requestId: string, approvalId: string, itemId: string, instruction: string, newRequestId: string): Promise<void> {
    const result = (await this.api.execute(document.lease, "libraryModifyRevisionItem", buildLibraryModifyRevisionItem({requestId, approvalId, itemId, instruction, newRequestId})))[0];
    if (result?.requestId !== newRequestId) throw new Error("The cluster did not confirm the modified request. Retry to recover it.");
  }
  async decideRevision(document: OpenDocument, requestId: string, approvalId: string, decision: "approved" | "rejected", answer?: Record<string, unknown>): Promise<void> {
    const current = await this.revision(document, requestId);
    if (current.approvalId !== approvalId) throw new UserInputError("The approval changed. Review the request again.");
    if (current.cancelRequested || current.status === "cancelled") throw new UserInputError("This review was stopped. Submit a new proposal to continue.");
    if (["succeeded", "failed"].includes(String(current.status))) throw new UserInputError("This review has finished. Refresh to see its result.");
    const result = (await this.api.execute(document.lease, "decideApproval", buildDecideApproval({ approvalId, decision, answer })))[0];
    if (result?.decision !== decision) throw new Error("The cluster did not confirm your decision. Refresh or retry to recover it.");
    if (result.resumeError) throw new UserInputError("Your decision was saved, but the job could not start. Retry to recover the same job.");
  }
  async save(document: OpenDocument, content: Uint8Array): Promise<void> {
    if (isZip(document.resource.name, document.mime, document.content)) throw new UserInputError("ZIP files are downloads; they cannot be edited or extracted here.");
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
      if (!result || result.conflict) throw new UserInputError("This document changed. Compare with its latest version before saving.");
      if (Number(result.newVersion) !== document.version + 1) throw new Error("The cluster did not confirm the saved version.");
      document.version = Number(result.newVersion);
      document.revision = String(result.revision || "");
    } else throw new UserInputError("This kind of record cannot be edited as a file yet.");
    document.content = new Uint8Array(content);
  }
}
