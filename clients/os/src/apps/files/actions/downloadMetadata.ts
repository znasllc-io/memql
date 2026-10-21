/** Prefer the byte response's name, including rendered bodies and old versions. */
export function downloadMetadata(response: Response, fallbackName: string): { name: string; mimeType: string; size: number } {
  const disposition = response.headers?.get("Content-Disposition") ?? "";
  if (/^text\/html(?:;|$)/i.test(response.headers?.get("Content-Type") ?? "") && !/^attachment(?:;|$)/i.test(disposition)) {
    throw new Error("The site returned a web page instead of the requested file.");
  }
  let name = "";
  const extended = /(?:^|;)\s*filename\*\s*=\s*UTF-8''([^;]+)/i.exec(disposition);
  if (extended) {
    try { name = decodeURIComponent(extended[1]!.trim()); } catch { /* Use the compatible filename below. */ }
  }
  if (!name) {
    const plain = /(?:^|;)\s*filename\s*=\s*(?:"((?:\\.|[^"\\])*)"|([^;]+))/i.exec(disposition);
    name = plain ? (plain[1]?.replace(/\\(.)/g, "$1") ?? plain[2]!.trim()) : fallbackName;
  }
  name = name.replace(/[\\/\u0000-\u001f\u007f]/g, "_").trim() || "download";
  const size = Number(response.headers?.get("Content-Length"));
  return { name, mimeType: response.headers?.get("Content-Type") || "application/octet-stream", size: Number.isSafeInteger(size) && size > 0 ? size : 0 };
}
