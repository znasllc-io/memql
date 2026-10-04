import { getDocument, GlobalWorkerOptions, type PDFDocumentProxy, type PDFDocumentLoadingTask, type RenderTask } from "pdfjs-dist/legacy/build/pdf.mjs";
declare function acquireVsCodeApi(): { postMessage(value: unknown): void };
const vscode = acquireVsCodeApi();
GlobalWorkerOptions.workerSrc = (globalThis as unknown as { memqlPDFWorker: string }).memqlPDFWorker;
const canvas = document.getElementById("canvas") as HTMLCanvasElement;
const error = document.getElementById("error")!;
const pageLabel = document.getElementById("page")!;
let pdf: PDFDocumentProxy | undefined;
let loading: PDFDocumentLoadingTask | undefined;
let page = 0;
let rendering: RenderTask | undefined;
let revision = 0;
let documentRevision = 0;
const assets = (globalThis as unknown as { memqlPDFAssets: string }).memqlPDFAssets;
async function render() {
  if (!pdf) return;
  rendering?.cancel();
  const current = ++revision;
  const selected = await pdf.getPage(page + 1);
  if (current !== revision) return;
  const viewport = selected.getViewport({ scale: 1.5 });
  canvas.width = viewport.width; canvas.height = viewport.height;
  pageLabel.textContent = `${page + 1} / ${pdf.numPages}`;
  rendering = selected.render({ canvas, canvasContext: canvas.getContext("2d")!, viewport });
  try { await rendering.promise; if (current === revision) vscode.postMessage({ type: "rendered", width: canvas.width, height: canvas.height }); }
  catch (e) { if ((e as Error)?.name !== "RenderingCancelledException") error.textContent = "This page could not be rendered."; }
}
window.addEventListener("message", async event => {
  if (event.data?.type !== "document" || !Array.isArray(event.data.bytes)) return;
  const currentDocument = ++documentRevision;
  try {
    rendering?.cancel(); revision++;
    await loading?.destroy();
    // Bytes are supplied by the extension. PDF.js never fetches a document URL.
    if (currentDocument !== documentRevision) return;
    loading = getDocument({ data: new Uint8Array(event.data.bytes), useSystemFonts: true,
      cMapUrl: assets + "cmaps/", cMapPacked: true, standardFontDataUrl: assets + "standard_fonts/", wasmUrl: assets + "wasm/" });
    const loaded = await loading.promise;
    if (currentDocument !== documentRevision) return;
    pdf = loaded;
    page = Math.min(page, pdf.numPages - 1);
    error.textContent = "";
    await render();
  } catch { if (currentDocument !== documentRevision) return; error.textContent = "This PDF could not be opened. It may be encrypted or damaged."; }
});
document.getElementById("previous")!.onclick = () => { if (page > 0) { page--; void render(); } };
document.getElementById("next")!.onclick = () => { if (pdf && page < pdf.numPages - 1) { page++; void render(); } };
document.getElementById("rotate")!.onclick = () => vscode.postMessage({ type: "edit", change: { kind: "rotate", page } });
document.getElementById("add")!.onclick = () => {
  const text = (document.getElementById("text") as HTMLInputElement).value;
  vscode.postMessage({ type: "edit", change: { kind: "text", page, text, x: 36, y: 36 } });
};
vscode.postMessage({ type: "ready" });
