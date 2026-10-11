import * as vscode from "vscode";
import { materialize } from "@znasllc-io/memql-sdk-core/client";
import { UserInputError } from "./problems.js";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

// One creation per request id at a time: a repeat while the first is still
// running gets the first's result instead of asking again. A finished one is
// forgotten. The cluster answers a later repeat of an id with the same
// composition, a failed one included, so a retry after a failure needs a new id.
const inFlight = new Map<string, Promise<vscode.Uri>>();

/**
 * Creates a blank Markdown document in the connected cluster and returns the
 * link the editor opens it by. `requestId` identifies this one ask: mint a new
 * one every time the person asks for a document, and reuse it only to repeat
 * that same ask.
 */
export function createMarkdown(api: EditorConnectionAPI, lease: ConnectionLease, name: string, requestId: string): Promise<vscode.Uri> {
  const existing = inFlight.get(requestId);
  if (existing) return existing;
  // The cluster adds the extension itself when it files the document.
  const stem = name.trim().replace(/\.(md|markdown)$/i, "").trim();
  if (!stem) return Promise.reject(new UserInputError("Enter a file name."));
  const run = materialize({ execute: (construct, call) => api.execute(lease, construct, call) }, { name: stem, format: "markdown", requestId })
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
