import * as vscode from "vscode";
import { Documents, type OpenDocument } from "./documents.js";

interface SavedRequest { fingerprint: string; requestId: string }

export function assertRevisionBase(base: OpenDocument, source: string, proposal: Record<string, unknown>): void {
  if (base.resource.id !== proposal.artifactId || base.revision !== proposal.revision || base.version !== proposal.version || source !== proposal.content) {
    throw new Error("This document changed after the proposal. Prepare a new request against its current revision.");
  }
}

// UI for the existing Nexus approval. Connection leases, persistence and job
// execution stay in the core extension and cluster; this class owns no worker.
export class RevisionReview {
  private readonly snapshots = new Map<string, string>();
  private readonly recoveries = new Map<string, Promise<void>>();
  private count = 0;
  constructor(private readonly context: vscode.ExtensionContext, private readonly files: Documents,
    private readonly load: (uri: vscode.Uri) => Promise<OpenDocument>) {
    context.subscriptions.push(vscode.workspace.registerTextDocumentContentProvider("memql-revision-preview", {
      provideTextDocumentContent: uri => this.snapshots.get(uri.toString()) ?? "",
    }), vscode.workspace.onDidCloseTextDocument(doc => { this.snapshots.delete(doc.uri.toString()); }));
  }
  private key(uri: vscode.Uri): string { return `memql.documentRevision:${uri.toString()}`; }
  private async recover(document: vscode.TextDocument, base: OpenDocument): Promise<void> {
    const key = this.key(document.uri);
    let recovery = this.recoveries.get(key);
    if (!recovery) {
      recovery = (async () => {
        const saved = this.context.workspaceState.get<SavedRequest>(key);
        const requestId = (await this.files.review(base)).requestId;
        if (typeof requestId !== "string" || !requestId || requestId === saved?.requestId) return;
        if (saved) {
          // An uncertain local submission keeps its idempotency identity until
          // its receipt is known. A confirmed older receipt can be superseded.
          try { await this.files.revision(base, saved.requestId); } catch { return; }
        }
        const current = await this.files.revision(base, requestId);
        const proposal = current.proposal as Record<string, unknown>;
        if (typeof proposal.revision !== "string" || typeof proposal.version !== "number" || !Array.isArray(proposal.commentIds) || !proposal.commentIds.every(id => typeof id === "string") || typeof proposal.instruction !== "string") throw new Error("The stored review request is incomplete.");
        if (this.context.workspaceState.get<SavedRequest>(key)?.requestId !== saved?.requestId) return;
        await this.context.workspaceState.update(key, { requestId,
          fingerprint: JSON.stringify([proposal.revision, proposal.version, [...proposal.commentIds].sort(), proposal.instruction.trim()]) });
      })();
      this.recoveries.set(key, recovery);
    }
    try { await recovery; } catch (error) { this.recoveries.delete(key); throw error; }
  }
  async prepare(document: vscode.TextDocument, commentIds: string[], instruction: string): Promise<Record<string, unknown>> {
    if (document.isDirty || document.uri.scheme !== "memql-file") throw new Error("Save the MemQL document before requesting a revision.");
    const base = await this.load(document.uri);
    await this.recover(document, base);
    if (new TextDecoder().decode(base.content) !== document.getText()) throw new Error("Compare with the saved document before requesting changes.");
    const fingerprint = JSON.stringify([base.revision, base.version, [...commentIds].sort(), instruction.trim()]);
    let pending = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    // A deliberate new submission may retry a terminal attempt. An uncertain
    // submission retains its identity until its durable status is known.
    const previous = pending?.fingerprint === fingerprint ? await this.files.revision(base, pending.requestId).catch(() => undefined) : undefined;
    if (pending?.fingerprint !== fingerprint || (previous && ["failed", "cancelled", "succeeded"].includes(String(previous.status)))) {
      pending = { fingerprint, requestId: globalThis.crypto.randomUUID() };
      // Persist before submitting so a lost response or editor restart retries
      // the same request, including after only the first server write landed.
      await this.context.workspaceState.update(this.key(document.uri), pending);
    }
    await this.files.requestRevision(base, commentIds, instruction.trim(), pending.requestId);
    return this.files.revision(base, pending.requestId);
  }
  async status(document: vscode.TextDocument): Promise<Record<string, unknown> | undefined> {
    if (document.uri.scheme !== "memql-file") return undefined;
    const base = await this.load(document.uri);
    await this.recover(document, base);
    const saved = this.context.workspaceState.get<SavedRequest>(this.key(document.uri));
    if (!saved) return undefined;
    return this.files.revision(base, saved.requestId);
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
    if (typeof proposal.revisedContent !== "string") throw new Error("The proposed changes are not ready yet.");
    const sourceURI = vscode.Uri.parse(`memql-revision-preview:/source-${++this.count}.md`);
    const draftURI = vscode.Uri.parse(`memql-revision-preview:/draft-${this.count}.md`);
    this.snapshots.set(sourceURI.toString(), String(proposal.content));
    this.snapshots.set(draftURI.toString(), proposal.revisedContent);
    await vscode.commands.executeCommand("vscode.diff", sourceURI, draftURI, "Reviewed source ↔ Revised draft", { preview: false });
  }
}
