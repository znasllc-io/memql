import * as vscode from "vscode";
import { Documents, type OpenDocument } from "./documents.js";

interface SavedRequest { fingerprint: string; requestId: string }
interface ComparedDraft { base: OpenDocument; content: Uint8Array; proposal: Record<string, unknown> }

export function assertRevisionBase(base: OpenDocument, source: string, proposal: Record<string, unknown>): void {
  if (base.resource.id !== proposal.artifactId || base.revision !== proposal.revision || base.version !== proposal.version || source !== proposal.content) {
    throw new Error("This document changed after the proposal. Prepare a new request against its current revision.");
  }
}

// UI for the existing Nexus approval. Connection leases, persistence and job
// execution stay in the core extension and cluster; this class owns no worker.
export class RevisionReview {
  private readonly snapshots = new Map<string, string>();
  private readonly compared = new Map<string, ComparedDraft>();
  private count = 0;
  constructor(private readonly context: vscode.ExtensionContext, private readonly files: Documents,
    private readonly load: (uri: vscode.Uri) => Promise<OpenDocument>) {
    context.subscriptions.push(vscode.workspace.registerTextDocumentContentProvider("memql-revision-preview", {
      provideTextDocumentContent: uri => this.snapshots.get(uri.toString()) ?? "",
    }), vscode.workspace.onDidCloseTextDocument(doc => { this.snapshots.delete(doc.uri.toString()); }));
  }
  private key(uri: vscode.Uri): string { return `memql.documentRevision:${uri.toString()}`; }
  async prepare(document: vscode.TextDocument, commentIds: string[], instruction: string): Promise<Record<string, unknown>> {
    if (document.isDirty || document.uri.scheme !== "memql-file") throw new Error("Save the MemQL document before requesting a revision.");
    const base = await this.load(document.uri);
    if (new TextDecoder().decode(base.content) !== document.getText()) throw new Error("Compare with the saved document before requesting changes.");
    const fingerprint = JSON.stringify([base.revision, base.version, [...commentIds].sort(), instruction.trim()]);
    let pending = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (pending?.fingerprint !== fingerprint) {
      pending = { fingerprint, requestId: globalThis.crypto.randomUUID() };
      // Persist before submitting so a lost response or editor restart retries
      // the same request, including after only the first server write landed.
      await this.context.workspaceState.update(this.key(document.uri), pending);
    }
    await this.files.requestRevision(base, commentIds, instruction.trim(), pending.requestId);
    this.compared.delete(document.uri.toString());
    return this.files.revision(base, pending.requestId);
  }
  async status(document: vscode.TextDocument): Promise<Record<string, unknown> | undefined> {
    const saved = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (!saved || document.uri.scheme !== "memql-file") return undefined;
    return this.files.revision(await this.load(document.uri), saved.requestId);
  }
  async resumePreparation(document: vscode.TextDocument): Promise<void> {
    const saved = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (!saved) throw new Error("Prepare a revision request first.");
    const base = await this.load(document.uri);
    const status = await this.files.revision(base, saved.requestId);
    const proposal = status.proposal as Record<string, unknown>;
    if (typeof proposal.version !== "number" || typeof proposal.revision !== "string" || !Array.isArray(proposal.commentIds) || !proposal.commentIds.every(id => typeof id === "string") || typeof proposal.instruction !== "string") throw new Error("The stored proposal is incomplete.");
    await this.files.requestRevision({ ...base, version: proposal.version, revision: proposal.revision }, proposal.commentIds as string[], proposal.instruction, saved.requestId);
  }
  async decide(document: vscode.TextDocument, approvalId: string, decision: "approved" | "rejected"): Promise<void> {
    const saved = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (!saved) throw new Error("Prepare and review a revision request first.");
    const base = await this.load(document.uri);
    if (decision === "approved") {
      if (document.isDirty) throw new Error("Save or discard local edits before approving this request.");
      const status = await this.files.revision(base, saved.requestId);
      assertRevisionBase(base, document.getText(), status.proposal as Record<string, unknown>);
    }
    await this.files.decideRevision(base, saved.requestId, approvalId, decision);
  }
  async compare(document: vscode.TextDocument): Promise<void> {
    const saved = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (!saved) throw new Error("Prepare a revision request first.");
    const base = await this.load(document.uri);
    const status = await this.files.revision(base, saved.requestId);
    const proposal = status.proposal as Record<string, unknown>;
    const draft = await this.files.revisionDraft(base, status);
    const sourceURI = vscode.Uri.parse(`memql-revision-preview:/source-${++this.count}.md`);
    const draftURI = vscode.Uri.parse(`memql-revision-preview:/draft-${this.count}.md`);
    this.snapshots.set(sourceURI.toString(), String(proposal.content));
    this.snapshots.set(draftURI.toString(), new TextDecoder("utf-8", { fatal: true }).decode(draft.content));
    this.compared.set(document.uri.toString(), { base, content: new Uint8Array(draft.content), proposal });
    await vscode.commands.executeCommand("vscode.diff", sourceURI, draftURI, "Reviewed source ↔ Revised draft");
  }
  async apply(document: vscode.TextDocument): Promise<void> {
    const compared = this.compared.get(document.uri.toString());
    if (!compared) throw new Error("Compare the revised draft before applying it.");
    if (document.isDirty) throw new Error("Save or discard your local edits before applying the compared draft.");
    const base = await this.load(document.uri);
    assertRevisionBase(base, document.getText(), compared.proposal);
    const replacement = new TextDecoder("utf-8", { fatal: true }).decode(compared.content);
    const edit = new vscode.WorkspaceEdit();
    edit.replace(document.uri, new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length)), replacement);
    if (!await vscode.workspace.applyEdit(edit)) throw new Error("The editor could not apply the compared draft.");
    // The file provider retains the original version and connection lease.
    // A concurrent cluster save refuses here and preserves the local edits.
    if (!await document.save()) throw new Error("The draft is in the editor but was not saved. Keep your edits and compare with the latest cluster version.");
    this.compared.delete(document.uri.toString());
  }
}
