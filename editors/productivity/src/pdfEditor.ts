import { reportProblem, UserInputError } from "./problems.js";
import { showProblem } from "./problemUI.js";
import * as vscode from "vscode";
import { changePDF, type PDFChange } from "./pdf.js";

export interface PDFBase { version: number; revision?: string; sourceId: string }
export interface PDFFileAccess {
  base(uri: vscode.Uri): Promise<PDFBase>;
  recover(uri: vscode.Uri, base: PDFBase): Promise<void>;
  refresh(uri: vscode.Uri): Promise<void>;
  release(uri: vscode.Uri): void;
}
const backupMetadata = (uri: vscode.Uri) => uri.with({ path: `${uri.path}.memql-base.json` });

export class PDFDocument implements vscode.CustomDocument {
  readonly panels = new Set<vscode.WebviewPanel>();
  constructor(readonly uri: vscode.Uri, public bytes: Uint8Array, public base: PDFBase | undefined,
    public recovered: boolean, private readonly closed: () => void) {}
  dispose(): void { this.panels.clear(); this.closed(); }
  update(bytes: Uint8Array): void {
    this.bytes = bytes;
    for (const panel of this.panels) void panel.webview.postMessage({ type: "document", bytes: Array.from(bytes) });
  }
}

export class PDFEditor implements vscode.CustomEditorProvider<PDFDocument> {
  private readonly opened = new Set<string>();
  hasOpen(uri: vscode.Uri): boolean { return this.opened.has(uri.toString()); }
  private readonly rendered = new Set<string>();
  private readonly previewState = new Map<string, { panel: vscode.WebviewPanel; ready: number; sent?: boolean; error?: string }>();
  private readonly waiting = new Map<string, (() => void)[]>();
  whenRendered(uri: vscode.Uri): Promise<void> {
    const key = uri.toString();
    if (this.rendered.has(key)) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        const state = this.previewState.get(key);
        reject(new Error(`PDF renderer did not produce a page (${JSON.stringify({
          ready: state?.ready, sent: state?.sent, visible: state?.panel.visible, error: state?.error,
        })}).`));
      }, 20000);
      this.waiting.set(key, [...(this.waiting.get(key) ?? []), () => { clearTimeout(timer); resolve(); }]);
    });
  }
  private readonly change = new vscode.EventEmitter<vscode.CustomDocumentEditEvent<PDFDocument>>();
  readonly onDidChangeCustomDocument = this.change.event;
  constructor(private readonly context: vscode.ExtensionContext, private readonly files: PDFFileAccess) { context.subscriptions.push(this.change); }
  async openCustomDocument(uri: vscode.Uri, backup: vscode.CustomDocumentOpenContext): Promise<PDFDocument> {
    const bytes = await vscode.workspace.fs.readFile(backup.backupId ? vscode.Uri.parse(backup.backupId) : uri);
    if (bytes.byteLength > 32 * 1024 * 1024) throw new UserInputError("This PDF exceeds the 32 MiB editor limit.");
    let base: PDFBase | undefined;
    if (uri.scheme === "memql-file") {
      if (backup.backupId) {
        try { base = JSON.parse(new TextDecoder().decode(await vscode.workspace.fs.readFile(backupMetadata(vscode.Uri.parse(backup.backupId))))); }
        catch { /* Older backups remain viewable and can be saved locally. */ }
      } else base = await this.files.base(uri);
    }
    this.opened.add(uri.toString());
    return new PDFDocument(uri, bytes, base, !!backup.backupId, () => {
      this.opened.delete(uri.toString()); this.rendered.delete(uri.toString()); this.files.release(uri);
    });
  }
  async resolveCustomEditor(document: PDFDocument, panel: vscode.WebviewPanel): Promise<void> {
    document.panels.add(panel);
    const state: { panel: vscode.WebviewPanel; ready: number; sent?: boolean; error?: string } = { panel, ready: 0 };
    const key = document.uri.toString();
    this.previewState.set(key, state);
    panel.onDidDispose(() => {
      document.panels.delete(panel);
      if (this.previewState.get(key) === state) this.previewState.delete(key);
    });
    const media = vscode.Uri.joinPath(this.context.extensionUri, "out");
    panel.webview.options = { enableScripts: true, localResourceRoots: [media] };
    const script = panel.webview.asWebviewUri(vscode.Uri.joinPath(media, "pdfView.js"));
    const assets = panel.webview.asWebviewUri(media).toString() + "/";
    const worker = panel.webview.asWebviewUri(vscode.Uri.joinPath(media, "pdf.worker.mjs"));
    const nonce = Array.from(crypto.getRandomValues(new Uint8Array(16)), x => x.toString(16).padStart(2, "0")).join("");
    panel.webview.onDidReceiveMessage(async message => {
      if(message?.type === "copyProblemReference" && typeof message.reference === "string" && /^tools-[a-zA-Z0-9-]{1,100}$/.test(message.reference)) { await vscode.env.clipboard.writeText(message.reference); return; }
      if (message?.type === "rendered" && Number(message.width) > 0 && Number(message.height) > 0) {
        const key = document.uri.toString(); this.rendered.add(key);
        for (const done of this.waiting.get(key) ?? []) done(); this.waiting.delete(key); return;
      }
      if (message?.type === "renderError" && typeof message.message === "string") { state.error = message.message; await panel.webview.postMessage({type: "problem", ...reportProblem(new Error(message.message), "display this PDF")}); return; }
      if (message?.type === "ready") {
        state.ready++;
        state.sent = await panel.webview.postMessage({ type: "document", bytes: Array.from(document.bytes) });
        return;
      }
      if (message?.type !== "edit" || !message.change || !["rotate", "text"].includes(message.change.kind)) return;
      try {
        // Edits are serialized against the exact in-memory revision too.
        const before = document.bytes;
        const after = await changePDF(before, message.change as PDFChange);
        if (before !== document.bytes) throw new UserInputError("The PDF changed while this edit was being prepared. Try again.");
        document.update(after);
        this.change.fire({ document, label: message.change.kind === "rotate" ? "Rotate page" : "Add text",
          undo: async () => document.update(before), redo: async () => document.update(after) });
      } catch (error) { void showProblem(error, "edit the PDF"); }
    });
    panel.webview.html = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; worker-src blob: ${panel.webview.cspSource}; connect-src ${panel.webview.cspSource}; style-src 'unsafe-inline'; img-src data: blob:;"><style>
      body{font:var(--vscode-font-size) var(--vscode-font-family);color:var(--vscode-foreground);background:var(--vscode-editor-background);margin:0}
      nav{position:sticky;top:0;display:flex;gap:8px;align-items:center;padding:12px;background:var(--vscode-editor-background);border-bottom:1px solid var(--vscode-panel-border)}
      button,input{font:inherit;color:var(--vscode-input-foreground);background:var(--vscode-input-background);border:1px solid var(--vscode-input-border);padding:5px 8px}button{cursor:pointer}main{text-align:center;padding:16px}canvas{max-width:100%;box-shadow:0 1px 8px #0003}#error{padding:16px}
      </style></head><body><nav><button id="previous" aria-label="Previous page">‹</button><span id="page"></span><button id="next" aria-label="Next page">›</button><button id="rotate">Rotate</button><input id="text" aria-label="Text to add" placeholder="Text to add"><button id="add">Add text</button></nav><div id="error" role="status"></div><main><canvas id="canvas"></canvas></main><script nonce="${nonce}">globalThis.memqlPDFWorker=${JSON.stringify(worker.toString())};globalThis.memqlPDFAssets=${JSON.stringify(assets)};</script><script nonce="${nonce}" src="${script}"></script></body></html>`;
  }
  async saveCustomDocument(document: PDFDocument): Promise<void> {
    if (document.recovered && document.uri.scheme === "memql-file") {
      if (!document.base) throw new UserInputError("This backup has no original revision. Save As a local PDF to preserve it, then compare with the latest MemQL file.");
      await this.files.recover(document.uri, document.base);
    }
    await vscode.workspace.fs.writeFile(document.uri, document.bytes);
    document.recovered = false;
    if (document.uri.scheme === "memql-file") document.base = await this.files.base(document.uri);
  }
  async saveCustomDocumentAs(document: PDFDocument, destination: vscode.Uri): Promise<void> {
    if (destination.toString() === document.uri.toString()) return this.saveCustomDocument(document);
    await vscode.workspace.fs.writeFile(destination, document.bytes);
  }
  async revertCustomDocument(document: PDFDocument): Promise<void> {
    if (document.uri.scheme === "memql-file") await this.files.refresh(document.uri);
    document.update(await vscode.workspace.fs.readFile(document.uri));
    if (document.uri.scheme === "memql-file") document.base = await this.files.base(document.uri);
    document.recovered = false;
  }
  async backupCustomDocument(document: PDFDocument, context: vscode.CustomDocumentBackupContext): Promise<vscode.CustomDocumentBackup> {
    await vscode.workspace.fs.writeFile(context.destination, document.bytes);
    const metadata = backupMetadata(context.destination);
    if (document.base) await vscode.workspace.fs.writeFile(metadata, new TextEncoder().encode(JSON.stringify(document.base)));
    return { id: context.destination.toString(), delete: async () => {
      for (const uri of [context.destination, metadata]) { try { await vscode.workspace.fs.delete(uri); } catch {} }
    } };
  }
}
