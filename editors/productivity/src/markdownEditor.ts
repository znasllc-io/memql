import { revisionDraftParts } from "./revisionDraft.js";
import { reportProblem, UserInputError } from "./problems.js";
import { DocumentDictation } from "./dictation.js";
import * as vscode from "vscode";
import { RevisionReview } from "./revisionReview.js";
import { Documents, type OpenDocument } from "./documents.js";
import { anchorStillMatches, markdownAnchor, renderMarkdown, type MarkdownAnchor } from "./markdown.js";

import { markdownPage } from "./markdownPage.js";
import { refreshQueue } from "./refreshQueue.js";
import { feedbackAttachmentMime, MAX_IMAGE_BYTES, markdownImageURI } from "./markdownAssets.js";

export class MarkdownEditor implements vscode.CustomTextEditorProvider {
  private active?: { document: vscode.TextDocument; panel: vscode.WebviewPanel };
  private readonly panels = new Set<{ document: vscode.TextDocument; panel: vscode.WebviewPanel }>();
  private readonly rendered = new Map<string, { version: number; text: string }>();
  private readonly revisions: RevisionReview;
  private readonly dictation: DocumentDictation;
  constructor(private readonly context: vscode.ExtensionContext, private readonly files: Documents,
    private readonly load: (uri: vscode.Uri) => Promise<OpenDocument>,
    private readonly refreshRemote: (document: vscode.TextDocument) => Promise<boolean> = async () => false) { this.revisions = new RevisionReview(context, files, load); this.dictation = new DocumentDictation(context,files); }
  async whenRendered(uri: vscode.Uri, version?: number): Promise<string> {
    const end = Date.now() + 15000;
    while (Date.now() < end) {
      const result = this.rendered.get(uri.toString());
      if (result && (version === undefined || result.version === version)) return result.text;
      await new Promise(resolve => setTimeout(resolve, 40));
    }
    throw new UserInputError("The Markdown reading view did not render this revision.");
  }
  private document(uri?: vscode.Uri): vscode.Uri {
    const target = uri ?? vscode.window.activeTextEditor?.document.uri ?? this.active?.document.uri;
    if (!target) throw new UserInputError("Open a Markdown document first.");
    return target;
  }
  async feedbackSelection(type="selectionFeedback"): Promise<void> {
    if (this.active?.panel.visible) await this.active.panel.webview.postMessage({type});
  }
  async show(mode: "source" | "reading" | "review" | "split", uri?: vscode.Uri): Promise<void> {
    const target = this.document(uri);
    if(mode!=="review")for(const entry of this.panels)this.dictation.cancel(entry.panel);
    const document = await vscode.workspace.openTextDocument(target);
    if (mode !== "source" && mode !== "split") await this.context.workspaceState.update(`memql.markdownMode:${target.toString()}`, mode);
    if (document.languageId !== "markdown" && !/\.(md|markdown)$/i.test(target.path)) throw new UserInputError("Open a Markdown document first.");
    const current = [...this.panels].find(entry => entry.document.uri.toString() === target.toString() && entry.panel.visible)
      ?? [...this.panels].find(entry => entry.document.uri.toString() === target.toString());
    const source = vscode.window.visibleTextEditors.find(editor => editor.document.uri.toString() === target.toString());
    if (mode === "source") {
      await vscode.window.showTextDocument(document, { viewColumn: source?.viewColumn ?? current?.panel.viewColumn, preview: false });
      for (const entry of this.panels) if (entry.document === document) entry.panel.dispose();
    } else if (mode === "reading" || mode === "review") {
      await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.markdown", { viewColumn: source?.viewColumn ?? current?.panel.viewColumn ?? vscode.ViewColumn.Active, preview: false });
      this.closeOtherPreviews(document);
      // Reveal in the source's group instead of closing its tab. Desktop VS Code
      // can save a dirty TextDocument when its text tab closes, even while the
      // custom editor remains open. A hidden source tab preserves that buffer.
    } else {
      // Reuse the existing pair. Repeated Split must not add editor groups.
      if (source && current?.panel.visible && source.viewColumn !== current.panel.viewColumn) {
        current.panel.reveal(current.panel.viewColumn);
      } else {
        await vscode.window.showTextDocument(document, { viewColumn: source?.viewColumn ?? current?.panel.viewColumn, preview: false });
        await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.markdown", { viewColumn: vscode.ViewColumn.Beside, preview: false });
        this.closeOtherPreviews(document);
      }
    }
    this.updateModes();
  }
  private closeOtherPreviews(document: vscode.TextDocument): void {
    // VS Code may clone a custom editor when opening it in another group.
    // Keep the newly activated view, not a second reading pane beside it.
    const keep = [...this.panels].find(entry => entry.document === document && entry.panel.active);
    if (!keep) return;
    for (const entry of this.panels) if (entry.document === document && entry !== keep) entry.panel.dispose();
  }
  private updateModes(): void {
    for (const { document, panel } of this.panels) {
      const split = panel.visible && vscode.window.visibleTextEditors.some(editor =>
        editor.document === document && editor.viewColumn !== panel.viewColumn);
      void panel.webview.postMessage({ type: "viewMode", mode: this.context.workspaceState.get(`memql.markdownMode:${document.uri.toString()}`, "reading"), split });
    }
  }

  async resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    this.active = { document, panel };
    const entry = this.active;
    this.panels.add(entry);
    const root = vscode.Uri.joinPath(this.context.extensionUri, "out");
    const imageRoot = document.uri.scheme === "memql-file"
      ? document.uri.with({ path: "/artifacts", query: "", fragment: "" }) : vscode.Uri.joinPath(document.uri, "..");
    panel.webview.options = { enableScripts: true, localResourceRoots: [root, imageRoot] };
    const renderDocumentHTML = (source: string) => renderMarkdown(source, { image: src => {
      const uri = markdownImageURI(src, document.uri.toString());
      return uri ? panel.webview.asWebviewUri(vscode.Uri.parse(uri)).toString() : undefined;
    } });
    const script = panel.webview.asWebviewUri(vscode.Uri.joinPath(root, "markdownView.js"));
    const nonce = `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
    const draftKey = `memql.markdownReviewDraft:${document.uri.toString()}`;
    let disposed = false;
    let commentPending: { fingerprint: string; requestId: string } | undefined;
    let saving = false;
    let historyBusy=false, historyGeneration=0;
    let preview: {version:number;revision:string;content:string}|undefined;
    const branchKey=`memql.markdownBranch:${document.uri.toString()}`;
    let branchPending=this.context.workspaceState.get<{fingerprint:string;requestId:string}>(branchKey);
    let generation = 0, notesGeneration = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let refreshedRun = "";
    const error = (e: unknown) => panel.webview.postMessage({ type: "error", ...reportProblem(e, "complete this document action") });
    const refreshNotes = async () => {
      if(document.uri.scheme!=="memql-file")return;
      const ticket=++notesGeneration;
      try { const result=await this.files.notes(await this.load(document.uri));if(!disposed&&ticket===notesGeneration)await panel.webview.postMessage({type:"notes",rows:result.notes??[],hasMore:result.hasMore}); }
      catch(e){if(!disposed&&ticket===notesGeneration)await panel.webview.postMessage({type:"notesError",...reportProblem(e,"load your notes")});}
    };
    const refreshComments = async () => {
      if (document.uri.scheme !== "memql-file") return;
      const ticket = ++generation;
      try {
        const base = await this.load(document.uri);
        const review = await this.files.review(base);
        if (disposed || generation !== ticket) return;
        const rows = Array.isArray(review.comments) ? review.comments as Record<string, unknown>[] : [];
        const content = document.getText();
        await panel.webview.postMessage({ type: "comments", rows: rows.map(row => ({ ...row,
          outdated: row.outdated || document.isDirty || row.revision !== base.revision ||
            !row.anchor || (!["document-end","document"].includes(String((row.anchor as Record<string,unknown>).kind)) && !anchorStillMatches(content, row.anchor as MarkdownAnchor)) })) });
        if (review.hasMore) await error(new UserInputError("Showing the first 500 comments. Older feedback remains stored in MemQL."));
      } catch (e) { if (!disposed && generation === ticket) await error(e); }
    };
    const refreshRevision = refreshQueue(async current => {
      if (disposed) return;
      if (timer) clearTimeout(timer);
      try {
        const status = await this.revisions.status(document);
        if (disposed || !current()) return;
        const awaiting = status?.status === "waiting" && status.approvalId && !status.decision;
        const problem = status?.errorMessage && !awaiting && ["waiting","failed","cancelled"].includes(String(status.status))
          ? reportProblem(new Error(String(status.errorMessage)), status.decision === "approved" ? "apply the approved changes" : "prepare changes", (await this.load(document.uri)).lease, String(status.runId)) : undefined;
        const draft = status?.draft as Record<string,unknown> | undefined;
        const draftParts = revisionDraftParts(draft?.text).map(part => ({...part, html:renderDocumentHTML(part.after)}));
        await panel.webview.postMessage({ type: "revision", status: status ? {...status, draft:undefined, draftParts, errorMessage:undefined, problem} : status });
        if (status?.status === "succeeded" && (status.result as Record<string,unknown> | undefined)?.applied && refreshedRun !== status.runId) {
          if (await this.refreshRemote(document)) { refreshedRun = String(status.runId); await refreshComments(); }
          else await panel.webview.postMessage({type:"notice",message:"Changes are saved in MemQL. Your local edits are preserved; compare with the latest revision before saving."});
        }
        if (current() && panel.visible) {
          // Keep discovering reviews submitted from another editor, including
          // after this panel's previous run stopped or finished.
          const inactive = !status || status.cancelRequested || ["succeeded","failed","cancelled"].includes(String(status.status));
          timer = setTimeout(() => { void refreshComments().then(refreshNotes).then(refreshRevision).catch(error); }, inactive ? 10000 : status.status === "waiting" ? 5000 : 2000);
        }
      } catch (e) {
        if (disposed || !current()) return;
        if (!disposed && panel.visible) timer = setTimeout(() => { void refreshRevision().catch(error); }, 10000);
        throw e;
      }
    });
    const render = async () => {
      try {
        await panel.webview.postMessage({ type: "document", html: renderDocumentHTML(document.getText()), version: document.version, sourceIdentity: document.getText(),
          historyAvailable: document.uri.scheme === "memql-file", connected: document.uri.scheme === "memql-file" && !document.isDirty,
          status: document.uri.scheme !== "memql-file" ? "Local document. Open its MemQL copy to share feedback."
            : document.isDirty ? "Unsaved changes. Save before adding revision-bound feedback." : "Feedback is saved to MemQL." });
        await refreshComments();
        await refreshNotes();
        await refreshRevision();
      } catch (e) { await error(e); }
    };
    const subscriptions = [
      panel.onDidChangeViewState(() => { if (panel.active) this.active = entry; if(!panel.visible)this.dictation.cancel(panel); this.updateModes(); if (panel.visible) void refreshRevision().catch(error); else if (timer) clearTimeout(timer); }),
      vscode.window.onDidChangeVisibleTextEditors(() => this.updateModes()),
      vscode.workspace.onDidChangeTextDocument(event => { if (event.document === document) void render(); }),
      vscode.workspace.onDidSaveTextDocument(saved => { if (saved === document) void render(); }),
      panel.webview.onDidReceiveMessage(async message => {
        try {
          if (!message || typeof message !== "object") return;
          if (message.type === "ready") {
            await panel.webview.postMessage({type:"restoreDraft", state:this.context.workspaceState.get(draftKey)});
            await panel.webview.postMessage({type:"dictationAvailable",available:await this.dictation.available()});
            this.updateModes(); await render();
          } else if (message.type === "draftState" && message.state && typeof message.state === "object" && JSON.stringify(message.state).length <= 300000) {
            await this.context.workspaceState.update(draftKey, message.state);
          }
          else if (message.type === "rendered" && message.version === document.version && typeof message.text === "string") {
            this.rendered.set(document.uri.toString(), { version: message.version, text: message.text });
          } else if(message.type === "copy") { await vscode.commands.executeCommand("editor.action.clipboardCopyAction");
          } else if(message.type === "refreshNotes") { await refreshNotes();
          } else if(message.type === "find") { await panel.webview.postMessage({type:"find"});
          } else if (["history", "historyVersion", "historyCurrent", "historyFork", "historyOpen"].includes(message.type)) {
            if (document.uri.scheme !== "memql-file") throw new UserInputError("Open a saved MemQL document to use history.");
            if(message.type === "historyCurrent") { preview=undefined; historyGeneration++; await panel.webview.postMessage({type:"historyCurrent"}); await render(); return; }
            if(historyBusy)return;
            historyBusy=true; const ticket=++historyGeneration;
            try {
              const base=await this.load(document.uri);
              if(message.type === "history") {
                const data=await this.files.history(base,Number.isInteger(message.beforeVersion)?message.beforeVersion:undefined);
                if(ticket===historyGeneration)await panel.webview.postMessage({type:"history",data,append:Number.isInteger(message.beforeVersion)});
              } else if(message.type === "historyVersion") {
                if(!Number.isInteger(message.version)||message.version<0)throw new UserInputError("Choose a saved version.");
                const snapshot=await this.files.version(base,message.version);
                if(ticket===historyGeneration){preview=snapshot;this.dictation.cancel(panel);await panel.webview.postMessage({type:"historyVersion",...snapshot,html:renderDocumentHTML(snapshot.content)});}
              } else if(message.type === "historyFork") {
                if(!preview || typeof message.name!=="string" || document.isDirty)throw new UserInputError("Save your local changes before starting a branch.");
                const fingerprint=JSON.stringify([preview.version,preview.revision,message.name]);
                if(branchPending?.fingerprint!==fingerprint){branchPending={fingerprint,requestId:globalThis.crypto.randomUUID()};await this.context.workspaceState.update(branchKey,branchPending);}
                const uri=await this.files.fork(base,preview.version,preview.revision,message.name,branchPending.requestId);
                await this.show("review",vscode.Uri.parse(uri));
              } else if(message.type === "historyOpen" && typeof message.artifactId==="string") {
                await this.show("reading",vscode.Uri.parse(this.files.artifactURI(base,message.artifactId,String(message.name||"Document"))));
              }
            } catch(e) {await panel.webview.postMessage({type:"historyError",...reportProblem(e,"load version history")});}
            finally {historyBusy=false;await panel.webview.postMessage({type:"historyIdle"});}
          } else if (message.type === "copyProblemReference" && typeof message.reference === "string" && /^(tools-|review-run-)[a-zA-Z0-9-]{1,100}$/.test(message.reference)) { await vscode.env.clipboard.writeText(message.reference);
          } else if (preview && !["external","dictationCancel"].includes(message.type)) return;
          else if (message.type === "source" || message.type === "reading" || message.type === "review" || message.type === "split") await this.show(message.type, document.uri);
          else if (message.type === "uploadAttachment") {
            try {
              if (document.uri.scheme !== "memql-file" || document.isDirty || typeof message.name !== "string" || typeof message.base64 !== "string" || message.base64.length > Math.ceil(MAX_IMAGE_BYTES / 3) * 4 || !/^[A-Za-z0-9+/]*={0,2}$/.test(message.base64)) throw new UserInputError("Save the document, then attach a supported image or Markdown file.");
              const bytes = Uint8Array.from(atob(message.base64), c => c.charCodeAt(0));
              const mime = feedbackAttachmentMime(message.name, bytes);
              const base = await this.load(document.uri);
              const uri = await this.files.createFile(base.lease, message.name, mime, bytes);
              const file = await this.files.read(uri, base.lease);
              await panel.webview.postMessage({type:"attachmentUploaded", uploadId:message.uploadId, attachment:{artifactId:file.resource.id,version:file.version,revision:file.revision,name:file.resource.name,mimeType:mime,size:bytes.length,uri}});
            } catch(e) { await panel.webview.postMessage({type:"attachmentFailed",uploadId:message.uploadId,...reportProblem(e,"attach reference file")}); }
          }
          else if (message.type === "attachmentPreview" && typeof message.uri === "string") {
            const uri = markdownImageURI(message.uri, document.uri.toString());
            if (uri) await panel.webview.postMessage({type:"attachmentPreview",uri:message.uri,preview:String(panel.webview.asWebviewUri(vscode.Uri.parse(uri)))});
          }
          else if (message.type === "dictationStart") { if(document.uri.scheme!=="memql-file"||document.isDirty)throw new UserInputError("Save the MemQL document before dictating feedback.");void this.dictation.start(panel,await this.load(document.uri)).catch(error); }
          else if (message.type === "dictationStop") await this.dictation.stop(panel);
          else if (message.type === "dictationCancel") this.dictation.cancel(panel);
          else if (message.type === "removeAnnotation") {
            if (saving) { await panel.webview.postMessage({type:"annotationRemoveError",message:"Wait for the current action to finish, then try again."}); return; }
            saving = true; generation++; notesGeneration++;
            try {
              if (document.uri.scheme !== "memql-file" || typeof message.id !== "string" || !["feedback","note"].includes(message.purpose)) throw new UserInputError("Choose saved feedback or a note to delete.");
              const base = await this.load(document.uri);
              const collection = message.purpose === "note" ? (await this.files.notes(base)).notes : (await this.files.review(base)).comments;
              const row = (collection as Record<string,unknown>[] | undefined)?.find(row => row.id === message.id);
              if (row && row.canRemove !== true) throw new UserInputError("Only the author can delete this feedback or note.");
              if (message.purpose === "feedback" && row) {
                const status = await this.revisions.status(document);
                const proposal = status?.proposal as Record<string,any> | undefined;
                if (proposal?.commentIds?.includes(message.id) && !["succeeded","failed","cancelled"].includes(String(status?.status))) {
                  if (status?.decision === "approved") throw new UserInputError("Wait for the approved changes to finish, then delete this feedback.");
                  if (status?.runId !== message.runId) throw new UserInputError("The review changed. Open it and try deleting this request again.");
                  if (!status?.cancelRequested) await this.files.cancelRevision(base, proposal.requestId);
                }
              }
              await this.files.removeAnnotation(base,message.id);
              generation++; notesGeneration++;
              await panel.webview.postMessage({type:"annotationRemoved",id:message.id,purpose:message.purpose});
              await refreshComments(); await refreshNotes(); await refreshRevision();
            } catch(e) { await panel.webview.postMessage({type:"annotationRemoveError",...reportProblem(e,"delete this feedback or note")}); }
            finally { saving = false; }
          }
          else if (message.type === "refresh") { await refreshComments(); await refreshRevision(); }
          else if (["prepareRevision", "resumePreparation", "decideRevision", "compareRevision", "modifyRevisionItem", "retryRevisionItem", "cancelRevision"].includes(message.type) && !saving) {
            saving = true;
            try {
              if (message.type === "prepareRevision") {
                if (message.version !== document.version || !Array.isArray(message.commentIds) || !message.commentIds.every((id: unknown) => typeof id === "string") || typeof message.instruction !== "string") throw new UserInputError("Select current comments and describe the change.");
                await this.revisions.prepare(document, message.commentIds, message.instruction);
              } else if (message.type === "cancelRevision") await this.revisions.cancel(document);
              else if (message.type === "resumePreparation") await this.revisions.resumePreparation(document);
              else if (message.type === "decideRevision") {
                if (typeof message.approvalId !== "string" || !["approved", "rejected"].includes(message.decision)) return;
                await this.revisions.decide(document, message.approvalId, message.decision, message.answer);
              } else if (message.type === "retryRevisionItem") await this.revisions.retryModification(document);
              else if (message.type === "modifyRevisionItem") {
                if (typeof message.itemId !== "string" || typeof message.instruction !== "string" || typeof message.approvalId !== "string") throw new UserInputError("Choose a change and describe the revision.");
                await this.revisions.modify(document, message.approvalId, message.itemId, message.instruction);
              } else if (message.type === "compareRevision") {
                await this.revisions.compare(document);
              }
              await refreshRevision();
            } catch (e) {
              // A stale action can race a remote stop or a newer review. Show
              // the current receipt alongside the actionable error.
              await refreshRevision().catch(() => undefined);
              throw e;
            } finally { saving = false; await panel.webview.postMessage({ type: "revisionIdle" }); }
          }
          else if (message.type === "external" && typeof message.href === "string" && /^https?:\/\//i.test(message.href)) {
            await vscode.env.openExternal(vscode.Uri.parse(message.href));
          } else if ((message.type === "comment" || message.type === "note") && !saving) {
            if (document.uri.scheme !== "memql-file" || document.isDirty || message.version !== document.version) throw new UserInputError("Save this revision before adding feedback, then select the passage again.");
            if (typeof message.body !== "string" || !message.body.trim() || message.body.length > 16000) throw new UserInputError("Enter a comment of up to 16000 characters.");
            const anchor = message.selection?.kind === "document" ? {kind:"document",quote:"Entire document"} : message.selection?.kind === "document-end" ? {kind:"document-end", quote:"End of document"} : markdownAnchor(document.getText(), message.selection);
            const base = await this.load(document.uri);
            if (new TextDecoder().decode(base.content) !== document.getText()) throw new UserInputError("Compare this document with its saved revision before adding feedback.");
            const attachments = message.type === "comment" && Array.isArray(message.attachments) ? message.attachments : [];
            if (attachments.length > 8 || attachments.some((ref: any) => !ref || typeof ref.artifactId !== "string" || typeof ref.version !== "number" || typeof ref.revision !== "string")) throw new UserInputError("Attach up to eight images or Markdown files.");
            const references = attachments.map((ref: any) => ({artifactId:ref.artifactId,version:ref.version,revision:ref.revision}));
            const fingerprint = JSON.stringify([message.type, base.revision, anchor, message.body, references]);
            if (commentPending?.fingerprint !== fingerprint) commentPending = { fingerprint, requestId: globalThis.crypto.randomUUID() };
            saving = true;
            try {
              await (message.type === "note" ? this.files.note(base, { ...anchor }, message.body, commentPending.requestId) : this.files.comment(base, { ...anchor }, message.body, commentPending.requestId, references));
              commentPending = undefined;
              await refreshComments();
              await refreshNotes();
              // Re-enable actions only after their new request is visible and
              // this handler has released the write guard. An immediate
              // Propose click must not be silently discarded as another save.
              saving = false;
              await panel.webview.postMessage({ type: message.type==="note"?"noteSaved":"saved" });
            } finally { saving = false; }
          }
        } catch (e) { await error(e); }
      }),
    ];
    panel.onDidDispose(() => {
      this.dictation.cancel(panel);
      disposed = true; generation++; notesGeneration++; if (timer) clearTimeout(timer);
      this.panels.delete(entry);
      for (const subscription of subscriptions) subscription.dispose();
      if (this.active?.panel === panel) this.active = undefined;
      this.rendered.delete(document.uri.toString());
    });
    panel.webview.html = markdownPage(document.fileName, String(script), nonce, panel.webview.cspSource);
  }
}
