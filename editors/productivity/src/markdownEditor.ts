import * as vscode from "vscode";
import { RevisionReview } from "./revisionReview.js";
import { Documents, type OpenDocument } from "./documents.js";
import { anchorStillMatches, escapeHTML, markdownAnchor, renderMarkdown, type MarkdownAnchor } from "./markdown.js";

export class MarkdownEditor implements vscode.CustomTextEditorProvider {
  private active?: { document: vscode.TextDocument; panel: vscode.WebviewPanel };
  private readonly rendered = new Map<string, { version: number; text: string }>();
  private readonly revisions: RevisionReview;
  constructor(private readonly context: vscode.ExtensionContext, private readonly files: Documents,
    private readonly load: (uri: vscode.Uri) => Promise<OpenDocument>) { this.revisions = new RevisionReview(context, files, load); }
  async whenRendered(uri: vscode.Uri, version?: number): Promise<string> {
    const end = Date.now() + 15000;
    while (Date.now() < end) {
      const result = this.rendered.get(uri.toString());
      if (result && (version === undefined || result.version === version)) return result.text;
      await new Promise(resolve => setTimeout(resolve, 40));
    }
    throw new Error("The Markdown reading view did not render this revision.");
  }
  private document(uri?: vscode.Uri): vscode.Uri {
    const target = uri ?? vscode.window.activeTextEditor?.document.uri ?? this.active?.document.uri;
    if (!target) throw new Error("Open a Markdown document first.");
    return target;
  }
  async show(mode: "source" | "reading" | "split", uri?: vscode.Uri): Promise<void> {
    const target = this.document(uri);
    const document = await vscode.workspace.openTextDocument(target);
    if (document.languageId !== "markdown" && !/\.(md|markdown)$/i.test(target.path)) throw new Error("Open a Markdown document first.");
    const current = this.active?.document.uri.toString() === target.toString() ? this.active : undefined;
    if (mode === "source") {
      await vscode.window.showTextDocument(document, { viewColumn: current?.panel.viewColumn, preview: false });
      current?.panel.dispose();
    } else if (mode === "reading") {
      await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.markdown", { viewColumn: vscode.ViewColumn.Active, preview: false });
    } else {
      // Both tabs share VS Code's TextDocument, including unsaved edits and undo.
      if (current) await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.Beside, preview: false });
      else {
        await vscode.window.showTextDocument(document, { preview: false });
        await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.markdown", { viewColumn: vscode.ViewColumn.Beside, preview: false });
      }
    }
  }
  async resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    this.active = { document, panel };
    const root = vscode.Uri.joinPath(this.context.extensionUri, "out");
    panel.webview.options = { enableScripts: true, localResourceRoots: [root] };
    const script = panel.webview.asWebviewUri(vscode.Uri.joinPath(root, "markdownView.js"));
    const nonce = `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
    let disposed = false;
    let commentPending: { fingerprint: string; requestId: string } | undefined;
    let saving = false;
    let generation = 0;
    const error = (e: unknown) => panel.webview.postMessage({ type: "error", message: e instanceof Error ? e.message : "The document could not be loaded." });
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
            !row.anchor || !anchorStillMatches(content, row.anchor as MarkdownAnchor) })) });
        if (review.hasMore) await error(new Error("Showing the first 500 comments. Older feedback remains stored in MemQL."));
      } catch (e) { if (!disposed && generation === ticket) await error(e); }
    };
    const refreshRevision = async () => {
      const status = await this.revisions.status(document);
      if (!disposed) await panel.webview.postMessage({ type: "revision", status });
    };
    const render = async () => {
      try {
        await panel.webview.postMessage({ type: "document", html: renderMarkdown(document.getText()), version: document.version,
          connected: document.uri.scheme === "memql-file" && !document.isDirty,
          status: document.uri.scheme !== "memql-file" ? "Local document. Open its MemQL copy to share feedback."
            : document.isDirty ? "Unsaved changes. Save before adding revision-bound feedback." : "Feedback is saved to MemQL." });
        await refreshComments();
        await refreshRevision();
      } catch (e) { await error(e); }
    };
    const subscriptions = [
      panel.onDidChangeViewState(() => { if (panel.active) this.active = { document, panel }; }),
      vscode.workspace.onDidChangeTextDocument(event => { if (event.document === document) void render(); }),
      vscode.workspace.onDidSaveTextDocument(saved => { if (saved === document) void render(); }),
      panel.webview.onDidReceiveMessage(async message => {
        try {
          if (!message || typeof message !== "object") return;
          if (message.type === "ready") await render();
          else if (message.type === "rendered" && message.version === document.version && typeof message.text === "string") {
            this.rendered.set(document.uri.toString(), { version: message.version, text: message.text });
          } else if (message.type === "source" || message.type === "split") await this.show(message.type, document.uri);
          else if (message.type === "refresh") { await refreshComments(); await refreshRevision(); }
          else if (["prepareRevision", "resumePreparation", "decideRevision", "compareRevision", "applyRevision"].includes(message.type) && !saving) {
            saving = true;
            try {
              if (message.type === "prepareRevision") {
                if (message.version !== document.version || !Array.isArray(message.commentIds) || !message.commentIds.every((id: unknown) => typeof id === "string") || typeof message.instruction !== "string") throw new Error("Select current comments and describe the change.");
                await this.revisions.prepare(document, message.commentIds, message.instruction);
              } else if (message.type === "resumePreparation") await this.revisions.resumePreparation(document);
              else if (message.type === "decideRevision") {
                if (typeof message.approvalId !== "string" || !["approved", "rejected"].includes(message.decision)) return;
                await this.revisions.decide(document, message.approvalId, message.decision);
              } else if (message.type === "compareRevision") {
                await this.revisions.compare(document);
                await panel.webview.postMessage({ type: "revisionCompared" });
              } else { await this.revisions.apply(document); await panel.webview.postMessage({ type: "revisionApplied" }); }
              await refreshRevision();
            } finally { saving = false; await panel.webview.postMessage({ type: "revisionIdle" }); }
          }
          else if (message.type === "external" && typeof message.href === "string" && /^https?:\/\//i.test(message.href)) {
            await vscode.env.openExternal(vscode.Uri.parse(message.href));
          } else if (message.type === "comment" && !saving) {
            if (document.uri.scheme !== "memql-file" || document.isDirty || message.version !== document.version) throw new Error("Save this revision before adding feedback, then select the passage again.");
            if (typeof message.body !== "string" || !message.body.trim() || message.body.length > 16000) throw new Error("Enter a comment of up to 16000 characters.");
            const anchor = markdownAnchor(document.getText(), message.selection);
            const base = await this.load(document.uri);
            if (new TextDecoder().decode(base.content) !== document.getText()) throw new Error("Compare this document with its saved revision before adding feedback.");
            const fingerprint = JSON.stringify([base.revision, anchor, message.body]);
            if (commentPending?.fingerprint !== fingerprint) commentPending = { fingerprint, requestId: globalThis.crypto.randomUUID() };
            saving = true;
            try {
              await this.files.comment(base, { ...anchor }, message.body, commentPending.requestId);
              commentPending = undefined;
              await panel.webview.postMessage({ type: "saved" });
              await refreshComments();
            } finally { saving = false; }
          }
        } catch (e) { await error(e); }
      }),
    ];
    panel.onDidDispose(() => {
      disposed = true; generation++;
      for (const subscription of subscriptions) subscription.dispose();
      if (this.active?.panel === panel) this.active = undefined;
      this.rendered.delete(document.uri.toString());
    });
    panel.webview.html = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src 'none'; form-action 'none'; base-uri 'none'"><title>${escapeHTML(document.fileName)}</title><style>
      body{margin:0;color:var(--vscode-editor-foreground);background:var(--vscode-editor-background);font:var(--vscode-font-size)/1.65 var(--vscode-font-family)}
      nav{position:sticky;top:0;display:flex;gap:8px;align-items:center;padding:10px 20px;background:var(--vscode-editor-background);border-bottom:1px solid var(--vscode-panel-border);z-index:1}
      button,textarea{font:inherit;color:inherit;background:var(--vscode-input-background);border:1px solid var(--vscode-input-border,var(--vscode-panel-border));border-radius:4px;padding:5px 10px}button{cursor:pointer}button:disabled{opacity:.5;cursor:default}
      main{max-width:1180px;margin:0 auto;padding:28px;display:grid;grid-template-columns:minmax(0,1fr) 280px;gap:36px}#content{overflow-wrap:anywhere}h1,h2,h3{line-height:1.3}pre{padding:16px;overflow:auto;background:var(--vscode-textCodeBlock-background)}code{font-family:var(--vscode-editor-font-family)}table{border-collapse:collapse}td,th{border:1px solid var(--vscode-panel-border);padding:6px 12px}blockquote{margin:12px 0;padding-left:14px;border-left:3px solid var(--vscode-textBlockQuote-border);color:var(--vscode-descriptionForeground)}a{color:var(--vscode-textLink-foreground)}
      aside{border-left:1px solid var(--vscode-panel-border);padding-left:20px}aside h2{margin-top:0}@media(max-width:800px){main{display:block}aside{border-left:0;border-top:1px solid var(--vscode-panel-border);margin-top:36px;padding:20px 0}}textarea{display:block;box-sizing:border-box;width:100%;min-height:90px;margin:10px 0}#status,small{color:var(--vscode-descriptionForeground)}article{border-top:1px solid var(--vscode-panel-border);padding:16px 0}#selected{max-height:120px;overflow:auto}#refresh{margin-left:auto}.image-alt{font-style:italic}
      </style></head><body><nav aria-label="Markdown view"><button id="source">Source</button><span aria-current="page">Reading</span><button id="split">Split</button><button id="refresh">Refresh comments</button></nav><main><section id="content" aria-label="Rendered Markdown"></section><aside aria-label="Document feedback"><h2>Feedback</h2><div id="status" role="status"></div><blockquote id="selected">Select a passage to comment.</blockquote><textarea id="feedback" aria-label="Comment" placeholder="Add feedback on this passage"></textarea><button id="add" disabled>Add comment</button><section id="comments" aria-label="Comments"></section><textarea id="revision-instruction" aria-label="Requested change" placeholder="Describe the change for the selected comments"></textarea><button id="prepare-revision" disabled>Review change request</button><section id="revision" aria-label="Revision request"></section></aside></main><script nonce="${nonce}" src="${script}"></script></body></html>`;
  }
}
