// The vscode half of Library artifacts: a content provider over the bff's
// artifact content route, the open-and-show helper, and the save dialog.
// Every decision lives in artifactDocument.ts and artifactContent.ts; this
// converts (memql#4748).
//
// THE CACHE DIES WITH THE CONNECTION, for the reason ClusterDocumentProvider's
// does: bytes fetched from one cluster must not be presented as current after a
// switch or a drop, so invalidate() clears the cache on every connection-state
// change and fires onDidChange for every open document of this scheme. VS Code
// re-asks, and the provider answers with the not-connected notice until the
// stream is back.
//
// A RE-FETCH HAS NO CALLER, which is why every failure path is INSIDE the
// provider. invalidate() makes VS Code ask again with nobody awaiting the
// promise, so a rejection would surface as the editor's own raw error text --
// unclassified, unredacted, and nowhere in the MemQL Connection channel. The
// provider answers with a notice and hands the raw detail to the host through
// `onError` instead.
//
// READ-ONLY BY CONSTRUCTION. A TextDocumentContentProvider document cannot be
// saved, so there is nothing here to disable; see artifactDocument.ts's header
// for why write-back is a separate decision rather than a missing feature.
//
// Refs: #4748 #4248

import * as os from "node:os";
import * as path from "node:path";

import * as vscode from "vscode";

import type { ConnectionManager } from "../connection/manager.js";
import {
  ARTIFACT_BUFFER_LIMIT_BYTES,
  ARTIFACT_DOCUMENT_SCHEME,
  artifactContentUrl,
  artifactDocumentUri,
  fetchFailedNotice,
  noAddressNotice,
  noContentNotice,
  notConnectedNotice,
  parseArtifactDocumentUri,
  tooLargeNotice,
  type ArtifactDocumentRef,
} from "./artifactDocument.js";
import {
  defaultArtifactFetch,
  fetchArtifactText,
  saveArtifactToPath,
  type ArtifactContentFailure,
  type ArtifactFetch,
} from "./artifactContent.js";

/**
 * What the provider needs, declared STRUCTURALLY rather than as the concrete
 * ConnectionManager -- the same shape ClusterDocumentDeps takes, and for the
 * same reason: the dependency is the capability, which is what makes the fetch
 * path drivable from a plain Node process.
 */
export interface ArtifactDocumentDeps {
  connections: Pick<ConnectionManager, "state" | "bearer" | "onDidChangeState">;
  /**
   * The https base for a REGISTERED cluster's HTTP edge, by registry name.
   *
   * A CALLBACK RATHER THAN A SECOND COPY OF THE ADDRESS IN THE URI. The uri
   * carries the cluster's registry NAME, and clusters.yaml is what turns that
   * into an address -- so putting the domain in the uri as well would be two
   * facts about one cluster that are free to disagree, with the disagreement
   * surfacing as bytes fetched from a host the tab does not name.
   */
  apiBaseUrl: (clusterName: string) => Promise<string | undefined>;
  fetch?: ArtifactFetch;
  /**
   * Where a failed fetch is REPORTED, given the headline and the raw detail.
   *
   * Optional for the reason ClusterDocumentDeps.onError is: a caller with no
   * output channel is a legitimate one, and the document is already honest
   * without it.
   */
  onError?: (headline: string, detail: string) => void;
}

export class ArtifactDocumentProvider implements vscode.TextDocumentContentProvider {
  private readonly changed = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this.changed.event;
  private readonly cache = new Map<string, string>();
  private readonly unsubscribe: () => void;

  constructor(private readonly deps: ArtifactDocumentDeps) {
    this.unsubscribe = deps.connections.onDidChangeState(() => this.invalidate());
  }

  async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
    const key = uri.toString();
    const cached = this.cache.get(key);
    if (cached !== undefined) return cached;
    const ref = parseArtifactDocumentUri({ authority: uri.authority, path: uri.path, query: uri.query });
    if (ref === undefined) return "// Not a Library artifact document.\n";

    const state = this.deps.connections.state;
    const bearer = this.deps.connections.bearer;
    // The CLUSTER NAME is checked as well as the status, exactly as it is for a
    // cluster document: a stream to a different cluster is not this document's
    // source, and the bearer it holds authenticates against a different graph.
    if (state.status !== "connected" || state.clusterName !== ref.cluster || bearer === undefined) {
      return notConnectedNotice(ref.cluster);
    }
    const base = await this.deps.apiBaseUrl(ref.cluster);
    if (base === undefined || base === "") return noAddressNotice(ref.cluster);

    const result = await fetchArtifactText(this.deps.fetch ?? defaultArtifactFetch, {
      url: artifactContentUrl(base, ref.artifactId),
      bearer,
      limitBytes: ARTIFACT_BUFFER_LIMIT_BYTES,
    });
    if (result.ok) {
      // Only a real answer is cached. A notice is a statement about right now,
      // and caching one would outlive the condition that produced it.
      this.cache.set(key, result.text);
      return result.text;
    }
    return this.noticeFor(ref, result);
  }

  private noticeFor(ref: ArtifactDocumentRef, failure: ArtifactContentFailure): string {
    switch (failure.reason) {
      case "noContent":
        return noContentNotice(ref.cluster, ref.fileName);
      case "tooLarge":
        return tooLargeNotice(ref.fileName, failure.bytes);
      case "unauthorized":
        // Not a fetch failure to report as one: the stream is up and the
        // credential behind it is no longer accepted, which is a sign-in
        // problem the connection surface already knows how to explain.
        this.deps.onError?.(`reading ${ref.fileName} from "${ref.cluster}" was refused`, failure.detail);
        return notConnectedNotice(ref.cluster);
      default:
        this.deps.onError?.(`reading ${ref.fileName} from "${ref.cluster}" failed`, failure.detail);
        return fetchFailedNotice(ref.cluster, ref.fileName);
    }
  }

  invalidate(): void {
    const open = vscode.workspace.textDocuments.filter((d) => d.uri.scheme === ARTIFACT_DOCUMENT_SCHEME);
    this.cache.clear();
    for (const d of open) this.changed.fire(d.uri);
  }

  dispose(): void {
    this.unsubscribe();
    this.changed.dispose();
  }
}

/**
 * Opens an artifact as a read-only editor document.
 *
 * `preview: false`, unlike a cluster document's `preview: true`. A preview tab
 * is replaced by the next thing opened, which is right for browsing a catalog
 * and wrong here: somebody double-clicked a file in another application and is
 * about to switch back to it, and a tab that has quietly vanished by the time
 * they return reads as the handoff having failed.
 *
 * The language is set only when the metadata NAMES one and VS Code has not
 * already worked out something from the filename's extension. It is wrapped
 * because `setTextDocumentLanguage` rejects an id no extension contributed, and
 * a language that cannot be applied is not worth failing an open over.
 */
export async function openArtifactDocument(
  ref: ArtifactDocumentRef,
  languageId: string | undefined,
): Promise<vscode.TextEditor> {
  const uri = vscode.Uri.parse(artifactDocumentUri(ref));
  const document = await vscode.workspace.openTextDocument(uri);
  if (languageId !== undefined && document.languageId !== languageId) {
    try {
      await vscode.languages.setTextDocumentLanguage(document, languageId);
    } catch {
      // The buffer is open and readable either way; highlighting is not worth a
      // failed handoff.
    }
  }
  return vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.One, preview: false });
}

/** What a save-to-disk offer ended in. */
export type ArtifactSaveOutcome =
  | { outcome: "saved"; path: string }
  | { outcome: "cancelled" }
  | { outcome: "failed"; failure: ArtifactContentFailure };

/**
 * Offers an artifact as a file on disk, and streams it to wherever it lands.
 *
 * CANCELLING IS AN ANSWER, NOT AN ERROR. The dialog is the one place in this
 * handoff where the person is asked to decide something, and dismissing it
 * means "not now" -- so it returns its own outcome rather than a failure, and
 * the caller shows nothing modal.
 */
export async function offerArtifactSave(params: {
  fetch?: ArtifactFetch;
  url: string;
  bearer: string;
  fileName: string;
}): Promise<ArtifactSaveOutcome> {
  const target = await vscode.window.showSaveDialog({
    defaultUri: defaultSaveUri(params.fileName),
    saveLabel: "Save artifact",
  });
  if (target === undefined) return { outcome: "cancelled" };
  const result = await saveArtifactToPath(params.fetch ?? defaultArtifactFetch, {
    url: params.url,
    bearer: params.bearer,
    destPath: target.fsPath,
  });
  return result.ok ? { outcome: "saved", path: target.fsPath } : { outcome: "failed", failure: result };
}

/**
 * Where the save dialog opens.
 *
 * The first workspace folder when there is one, the home directory otherwise:
 * a person who has a project open almost always means "into this project", and
 * a person who does not has no folder this extension could infer.
 */
function defaultSaveUri(fileName: string): vscode.Uri {
  const folder = vscode.workspace.workspaceFolders?.[0];
  const base = folder !== undefined ? folder.uri.fsPath : os.homedir();
  return vscode.Uri.file(path.join(base, fileName));
}
