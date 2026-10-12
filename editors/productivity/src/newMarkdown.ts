import * as vscode from "vscode";
import { fileNameProblem, fileNameStem, materialize, MaterializeCancelled } from "@znasllc-io/memql-sdk-core/client";
import { UserInputError } from "./problems.js";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

// One creation per request id at a time: a repeat while the first is still
// running gets the first's result instead of asking again, and a finished one is
// forgotten. A retry after a request that failed to START reuses its id; after a
// composition that FAILED, use a new id, because the cluster answers a repeat of
// an id with the composition it already has, the failed one included.
const inFlight = new Map<string, Promise<vscode.Uri>>();

/** The name without the extension. The cluster adds ".md" itself when it files the document. */
const stemOf = (name: string): string => fileNameStem(name, "markdown");

/**
 * What is wrong with a name as it is typed, or undefined when it can be used.
 * A name must not be a place (a folder path), and must have something before its
 * extension: ".md" on its own would file a document with no name. The rule and
 * its words are the SDK's fileNameProblem, which the MemQL OS composer checks a
 * name with too, so the two say the same thing about the same name.
 */
export function markdownNameProblem(value: string): string | undefined {
  return fileNameProblem(value, "markdown");
}

/**
 * Creates a blank Markdown document in the connected cluster and returns the
 * link the editor opens it by. `requestId` identifies this one ask: mint a new
 * one every time the person asks for a document, and reuse it only to repeat
 * that same ask. `signal` stops the creation: the returned promise then rejects
 * with MaterializeCancelled, unless it is already in Files, when it is
 * returned. `onStatus` hears where the creation is ("Saved; waiting for its
 * Files entry" while the Library catches up).
 */
export function createMarkdown(api: EditorConnectionAPI, lease: ConnectionLease, name: string, requestId: string,
  follow: { signal?: AbortSignal; onStatus?: (status: string) => void } = {}): Promise<vscode.Uri> {
  const existing = inFlight.get(requestId);
  if (existing) return existing;
  const stem = stemOf(name);
  if (!stem) return Promise.reject(new UserInputError("Enter a file name."));
  const run = materialize({ execute: (construct, call) => api.execute(lease, construct, call), signal: follow.signal, onStatus: follow.onStatus },
    { name: stem, format: "markdown", requestId })
    .then(result => {
      // Link the name the cluster filed it under, which can differ from the one
      // asked for (it trims stray dots and spaces and caps the length).
      const filed = result.name.trim() || stem;
      const filename = /\.(md|markdown)$/i.test(filed) ? filed : `${filed}.md`;
      return vscode.Uri.from({ scheme: "memql-file", authority: lease.domain, path: `/artifacts/${result.artifactId}/${filename.replace(/[\\/]/g, "_")}` });
    })
    .finally(() => inFlight.delete(requestId));
  inFlight.set(requestId, run);
  return run;
}

/**
 * The New Markdown command: asks for a name, creates the document in a
 * notification that says where the creation is and that the person can stop,
 * and hands its link to `show`. Stopping it is a quiet end, not an error. A
 * failure is thrown for the caller to report.
 */
export async function newMarkdown(api: EditorConnectionAPI, show: (uri: vscode.Uri) => Promise<void>): Promise<void> {
  const lease = api.current();
  if (!lease) throw new UserInputError("Connect to your cluster in MemQL before creating a document.");
  const name = await vscode.window.showInputBox({
    title: "New Markdown document", value: "Untitled.md", prompt: `Save in ${lease.name}. Add content in Source or request it in Review.`,
    validateInput: markdownNameProblem,
  });
  if (!name) return;
  const filename = /\.(md|markdown)$/i.test(name) ? name : `${name}.md`;
  // A new request id for each document asked for: the cluster uses it to recognise a repeat of this one ask.
  const requestId = globalThis.crypto.randomUUID();
  const uri = await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: `Creating ${filename}`, cancellable: true }, async (progress, cancel) => {
    // The person's stop becomes the SDK's: it cancels the composition if it is still being made.
    const stop = new AbortController();
    const subscription = cancel.onCancellationRequested(() => stop.abort());
    if (cancel.isCancellationRequested) stop.abort();
    try {
      // Where the creation is, in the notification: the composition's status, and
      // "Saved; waiting for its Files entry" while the Library catches up.
      return await createMarkdown(api, lease, filename, requestId, { signal: stop.signal, onStatus: status => progress.report({ message: status }) });
    } catch (error) {
      // Stopping the document, from here or from elsewhere, is not a failure.
      if (error instanceof MaterializeCancelled) return undefined;
      throw error;
    } finally {
      subscription.dispose();
    }
  });
  if (uri) await show(uri);
}
