// The vscode half of cluster documents: a content provider over ReadPackFile,
// one header lens, and the open-and-reveal helper. Every decision lives in
// clusterDocument.ts; this converts.
//
// THE CACHE DIES WITH THE CONNECTION. A document fetched from one cluster must
// not be presented as current after a switch or a drop, so invalidate() clears
// it on every connection-state change and fires onDidChange for every open
// document of this scheme -- VS Code re-asks, and the provider answers with the
// not-connected notice until the stream is back.
//
// Refs: #4248

import * as vscode from "vscode";
import { PackClient } from "@znasllc-io/memql-sdk-core/pack";

import type { ConnectionManager } from "../connection/manager.js";
import { signatureLine } from "./signature.js";
import {
  CLUSTER_DOCUMENT_SCHEME,
  clusterDocumentUri,
  notConnectedNotice,
  notFoundNotice,
  packLocator,
  parseClusterDocumentUri,
  type ClusterDocumentRef,
} from "./clusterDocument.js";

export class ClusterDocumentProvider implements vscode.TextDocumentContentProvider {
  private readonly changed = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this.changed.event;
  private readonly cache = new Map<string, string>();
  private readonly unsubscribe: () => void;

  constructor(private readonly deps: { connections: ConnectionManager }) {
    this.unsubscribe = deps.connections.onDidChangeState(() => this.invalidate());
  }

  async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
    const key = uri.toString();
    const cached = this.cache.get(key);
    if (cached !== undefined) return cached;
    const ref = parseClusterDocumentUri({ authority: uri.authority, path: uri.path, query: uri.query });
    if (ref === undefined) return "// Not a cluster document.\n";
    const state = this.deps.connections.state;
    const dispatcher = this.deps.connections.dispatcher;
    // The CLUSTER NAME is checked as well as the status: a stream to a
    // different cluster is not this document's source, and answering from it
    // would put one cluster's bytes under another's name.
    if (dispatcher === undefined || state.status !== "connected" || state.clusterName !== ref.cluster) {
      return notConnectedNotice(ref.cluster);
    }
    const locator = packLocator(ref.originPath);
    if (locator === undefined) return notFoundNotice(ref.cluster, ref.originPath);
    const file = await new PackClient(dispatcher).readFile(locator.domain, locator.path);
    const text = file.found ? file.source : notFoundNotice(ref.cluster, ref.originPath);
    // Only a real answer is cached. A notice is a statement about right now,
    // and caching one would outlive the condition that produced it.
    if (file.found) this.cache.set(key, text);
    return text;
  }

  invalidate(): void {
    const open = vscode.workspace.textDocuments.filter((d) => d.uri.scheme === CLUSTER_DOCUMENT_SCHEME);
    this.cache.clear();
    for (const d of open) this.changed.fire(d.uri);
  }

  dispose(): void {
    this.unsubscribe();
    this.changed.dispose();
  }
}

/** One lens at line 0: where the bytes came from, and the way to the detail page. */
export class ClusterDocumentLens implements vscode.CodeLensProvider {
  provideCodeLenses(document: vscode.TextDocument): vscode.CodeLens[] {
    const ref = parseClusterDocumentUri({ authority: document.uri.authority, path: document.uri.path, query: document.uri.query });
    if (ref === undefined) return [];
    return [
      new vscode.CodeLens(new vscode.Range(0, 0, 0, 0), {
        title: `From ${ref.cluster} -- read-only -- Open construct details`,
        command: "memql.constructs.showDetails",
        arguments: [{ kind: ref.kind, name: ref.name }],
      }),
    ];
  }
}

export async function openClusterDocument(ref: ClusterDocumentRef): Promise<vscode.TextEditor> {
  const uri = vscode.Uri.parse(clusterDocumentUri(ref));
  const document = await vscode.workspace.openTextDocument(uri);
  const editor = await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.One, preview: true });
  const line = signatureLine(document.getText(), ref.kind, ref.name);
  if (line >= 0) {
    const at = new vscode.Position(line, 0);
    editor.selection = new vscode.Selection(at, at);
    editor.revealRange(new vscode.Range(at, at), vscode.TextEditorRevealType.InCenter);
  }
  return editor;
}
