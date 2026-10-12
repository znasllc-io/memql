import * as vscode from "vscode";
import type { Documents, OpenDocument } from "./documents.js";
import { imageMarkdown, imageMime, MAX_IMAGE_BYTES } from "./markdownAssets.js";
import { UserInputError } from "./problems.js";
import { showProblem } from "./problemUI.js";

// "Insert image": the person picks a picture with the open dialog, it is saved to MemQL Files, and
// its Markdown link goes into the source at the cursor. That is an upload of bytes the person
// already has, not a way to make a file, so this is one of the few modules the one-creation-path
// guard (test/onePathCreate.test.ts) lets upload. It has a module of its own so the guard can allow
// exactly this and scan extension.ts, which registers the command, like any other file.
export function insertImage(files: Pick<Documents, "createFile">, load: (uri: vscode.Uri) => Promise<Pick<OpenDocument, "lease">>): () => Promise<void> {
  return async () => {
    try {
      const editor = vscode.window.activeTextEditor;
      if (!editor || editor.document.languageId !== "markdown" || editor.document.uri.scheme !== "memql-file") throw new UserInputError("Open a MemQL Markdown document in Source to insert an image.");
      const document = editor.document, version = document.version, selection = editor.selection;
      const base = await load(document.uri);
      const selected = await vscode.window.showOpenDialog({ title: "Insert image", openLabel: "Insert image", canSelectMany: false, canSelectFiles: true, canSelectFolders: false,
        defaultUri: vscode.Uri.file("/"), filters: { Images: ["png", "jpg", "jpeg", "gif", "webp"] } });
      if (!selected?.[0]) return;
      if ((await vscode.workspace.fs.stat(selected[0])).size > MAX_IMAGE_BYTES) throw new UserInputError("Choose an image smaller than 8 MiB.");
      const bytes = await vscode.workspace.fs.readFile(selected[0]);
      const name = selected[0].path.split("/").pop()!;
      const mime = imageMime(name, bytes);
      const alt = await vscode.window.showInputBox({ title: "Image description", prompt: "Describe what the image shows for readers using assistive technology.", value: name.replace(/\.[^.]+$/, "").replace(/[-_]/g, " ") });
      if (alt === undefined) return;
      if (document.isClosed || document.version !== version) throw new UserInputError("The document changed. Insert the image again at the intended position.");
      const uri = await files.createFile(base.lease, name, mime, bytes);
      // Upload is durable. If an edit raced it, offer the saved link without
      // applying a stale selection or uploading a duplicate.
      if (document.isClosed || document.version !== version || !await editor.edit(edit => edit.replace(selection, imageMarkdown(alt, uri)))) {
        await vscode.env.clipboard.writeText(imageMarkdown(alt, uri));
        void vscode.window.showInformationMessage("The image was saved. Its Markdown link is copied; paste it at the intended position in Source.");
      }
    } catch (error) { void showProblem(error, "insert the image"); }
  };
}
