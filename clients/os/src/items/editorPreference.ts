export type EditorPreference = "browser" | "vscode" | "cursor";

const EDITOR_PREFERENCE_KEY = "memql-os-editor-v1";

export function readEditorPreference(): EditorPreference {
  try {
    const value = globalThis.localStorage?.getItem(EDITOR_PREFERENCE_KEY);
    return value === "vscode" || value === "cursor" ? value : "browser";
  } catch {
    return "browser";
  }
}

export function saveEditorPreference(preference: EditorPreference): void {
  try {
    globalThis.localStorage?.setItem(EDITOR_PREFERENCE_KEY, preference);
  } catch {
    // A blocked preference store must not prevent opening the file.
  }
}

/** ZIP content is always downloaded intact, including a renamed ZIP. */
export function isZipArtifact(file: { title?: string; name?: string; mimeType?: string }): boolean {
  const mime = (file.mimeType ?? "").split(";", 1)[0]!.trim().toLowerCase();
  return mime === "application/zip" || mime === "application/x-zip-compressed" ||
    /\.zip\s*$/i.test(file.name ?? file.title ?? "");
}

/** Generated document titles need an extension so VS Code selects the right viewer. */
export function editorFilename(file: { title?: string; name?: string; kind?: string; format?: string; mimeType?: string }): string {
  const name = file.name || file.title || "document";
  if (file.kind === "file" || /\.[a-z0-9]{1,12}$/i.test(name)) return name;
  const format = file.format || ({ "text/markdown": "markdown", "application/pdf": "pdf", "text/plain": "text" } as Record<string, string>)[file.mimeType ?? ""];
  const extension = ({ markdown: "md", pdf: "pdf", text: "txt" } as Record<string, string>)[format ?? ""];
  return extension ? `${name}.${extension}` : name;
}

export function editorArtifactURI(domain: string, artifactId: string, name = "document"): string {
  const filename = name.replace(/[\\/\u0000-\u001f\u007f]/g, "_").slice(0, 180) || "document";
  return `memql-file://${encodeURIComponent(domain)}/artifacts/${encodeURIComponent(artifactId)}/${encodeURIComponent(filename)}`;
}

export function editorArtifactURL(domain: string, artifactId: string, name = "document", preference = readEditorPreference()): string {
  const resource = editorArtifactURI(domain, artifactId, name);
  if (preference === "browser") {
    // VS Code's web workbench consumes openFile from the startup payload.
    // Only a resource reference travels here. Its file provider authenticates
    // through the MemQL extension; no file bytes or credential enter the URL.
    return `https://vscode.dev/?payload=${encodeURIComponent(JSON.stringify([["openFile", resource]]))}`;
  }
  return `${preference}://znasllc.memql-productivity-tools/open?resource=${encodeURIComponent(resource)}`;
}
