import { libraryFileFacts, rowBool, rowString, sameEntityId, type LibraryFileLookup, type Row } from "@znasllc-io/memql-sdk-core/client";
import { readableReference } from "./make";

export interface FileOption { id: string; name: string; kind: string; createdAt: string }
export function fileWord(name: string): string {
  const extension = name.slice(name.lastIndexOf(".")).toLowerCase();
  return ({ ".md": "Markdown", ".markdown": "Markdown", ".txt": "Text", ".css": "Text", ".html": "Web page", ".htm": "Web page", ".csv": "CSV", ".json": "JSON", ".zip": "ZIP", ".docx": "Word document", ".pdf": "PDF" } as Record<string, string>)[extension] || "Picture";
}
export function fileOptions(rows: readonly Row[]): FileOption[] {
  return rows.filter(row => !rowBool(row, "archived") && !rowBool(row, "deleted") && readableReference(rowString(row, "name")))
    .map(row => ({ id: rowString(row, "id"), name: rowString(row, "name"), kind: fileWord(rowString(row, "name")), createdAt: rowString(row, "createdAt") }))
    .filter(file => file.id).sort((a, b) => b.createdAt.localeCompare(a.createdAt) || a.id.localeCompare(b.id));
}
/** Keep archived rows too: the path predictor must see their current facts. */
export function fileLookup(rows: readonly Row[]): LibraryFileLookup {
  return id => libraryFileFacts(rows.find(row => sameEntityId(rowString(row, "id"), id)));
}
