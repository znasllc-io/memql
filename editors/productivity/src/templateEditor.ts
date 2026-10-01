import * as vscode from "vscode";
import { escapeHTML } from "./markdown.js";
import { readTemplate } from "./templates.js";

export class TemplateEditor implements vscode.CustomTextEditorProvider {
  private active?: { document: vscode.TextDocument; panel: vscode.WebviewPanel };
  private readonly rendered = new Map<string, { version: number; subject: string }>();
  constructor(private readonly context: vscode.ExtensionContext) {}
  async whenRendered(uri: vscode.Uri, version?: number): Promise<string> {
    const end = Date.now() + 15000;
    while (Date.now() < end) {
      const value = this.rendered.get(uri.toString());
      if (value && (version === undefined || value.version === version)) return value.subject;
      await new Promise(resolve => setTimeout(resolve, 40));
    }
    throw new Error("Email preview did not render this revision.");
  }
  async show(mode: "source" | "preview" | "split", uri?: vscode.Uri): Promise<void> {
    const target = uri ?? vscode.window.activeTextEditor?.document.uri ?? this.active?.document.uri;
    if (!target) throw new Error("Open an email template first.");
    const document = await vscode.workspace.openTextDocument(target);
    readTemplate(document.getText());
    const current = this.active?.document.uri.toString() === target.toString() ? this.active : undefined;
    if (mode === "source") {
      await vscode.window.showTextDocument(document, { viewColumn: current?.panel.viewColumn, preview: false });
      current?.panel.dispose();
    } else if (mode === "preview") await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.email", { preview: false });
    else if (current) await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.Beside, preview: false });
    else {
      await vscode.window.showTextDocument(document, { preview: false });
      await vscode.commands.executeCommand("vscode.openWith", target, "memql.productivity.email", { viewColumn: vscode.ViewColumn.Beside, preview: false });
    }
  }
  async resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    this.active = { document, panel };
    const root = vscode.Uri.joinPath(this.context.extensionUri, "out");
    panel.webview.options = { enableScripts: true, localResourceRoots: [root] };
    const script = panel.webview.asWebviewUri(vscode.Uri.joinPath(root, "templateView.js"));
    const nonce = globalThis.crypto.randomUUID().replace(/-/g, "");
    const render = async () => {
      try { await panel.webview.postMessage({ type: "document", template: readTemplate(document.getText()), version: document.version }); }
      catch (error) { await panel.webview.postMessage({ type: "error", message: (error as Error).message }); }
    };
    const subscriptions = [
      panel.onDidChangeViewState(() => { if (panel.active) this.active = { document, panel }; }),
      vscode.workspace.onDidChangeTextDocument(event => { if (event.document === document) void render(); }),
      panel.webview.onDidReceiveMessage(async message => {
        try {
          if (message?.type === "ready") await render();
          else if (message?.type === "source" || message?.type === "split") await this.show(message.type, document.uri);
          else if (message?.type === "examples") await vscode.commands.executeCommand("memql.productivity.templateFromExamples");
          else if (message?.type === "rendered" && message.version === document.version && typeof message.subject === "string") this.rendered.set(document.uri.toString(), { version: message.version, subject: message.subject });
        } catch (error) { void vscode.window.showErrorMessage((error as Error).message); }
      }),
    ];
    panel.onDidDispose(() => {
      for (const subscription of subscriptions) subscription.dispose();
      this.rendered.delete(document.uri.toString());
      if (this.active?.panel === panel) this.active = undefined;
    });
    panel.webview.html = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; frame-src 'self'; form-action 'none'; base-uri 'none'"><title>${escapeHTML(document.fileName)}</title><style>
body{margin:0;color:var(--vscode-editor-foreground);background:var(--vscode-editor-background);font:var(--vscode-font-size)/1.5 var(--vscode-font-family)}nav{display:flex;align-items:center;gap:12px;padding:12px 20px;border-bottom:1px solid var(--vscode-panel-border)}button{font:inherit;color:inherit;background:var(--vscode-button-secondaryBackground);border:0;border-radius:4px;padding:6px 12px;cursor:pointer}#examples{margin-left:auto}main{max-width:900px;margin:auto;padding:24px}#subject{font-size:20px;font-weight:500}#status{color:var(--vscode-descriptionForeground);margin-bottom:16px}iframe{width:100%;height:70vh;background:white;border:1px solid var(--vscode-panel-border)}pre{white-space:pre-wrap}details{margin-top:16px}
</style></head><body><nav aria-label="Email template view"><button id="source">Source</button><span aria-current="page">Preview</span><button id="split">Split</button><button id="examples">Create from examples</button></nav><main><h1 id="subject"></h1><p id="status" role="status"></p><iframe id="email" title="Email preview" sandbox></iframe><details><summary>Plain-text version</summary><pre id="text"></pre></details></main><script nonce="${nonce}" src="${script}"></script></body></html>`;
  }
}
