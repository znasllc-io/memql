export {};
declare function acquireVsCodeApi(): { postMessage(message: unknown): void; getState(): unknown; setState(state: unknown): void };
const api = acquireVsCodeApi();
const content = document.getElementById("content")!;
const comments = document.getElementById("comments")!;
const feedbackStatus = document.getElementById("status")!;
const feedback = document.getElementById("feedback") as HTMLTextAreaElement;
const add = document.getElementById("add") as HTMLButtonElement;
let selection: { startLine: number; endLine: number; quote: string; startBlock: number; endBlock: number; startTextOffset: number; endTextOffset: number } | undefined;
let version = 0;
let connected = false;
const state = (api.getState() ?? {}) as { draft?: string };
feedback.value = state.draft ?? "";
feedback.addEventListener("input", () => api.setState({ draft: feedback.value }));
function mapped(node: Node | null): HTMLElement | null {
  const element = node?.nodeType === Node.ELEMENT_NODE ? node as Element : node?.parentElement;
  return element?.closest<HTMLElement>("[data-start-line][data-end-line]") ?? null;
}
function captureSelection() {
  const selected = window.getSelection();
  if (!selected?.rangeCount || selected.isCollapsed) return;
  const range = selected.getRangeAt(0);
  if (!content.contains(range.startContainer) || !content.contains(range.endContainer)) return;
  const start = mapped(range.startContainer), end = mapped(range.endContainer);
  if (!start || !end) return;
  const startPrefix = document.createRange(); startPrefix.selectNodeContents(start); startPrefix.setEnd(range.startContainer, range.startOffset);
  const endPrefix = document.createRange(); endPrefix.selectNodeContents(end); endPrefix.setEnd(range.endContainer, range.endOffset);
  selection = { startBlock: Number(start.dataset.blockId), endBlock: Number(end.dataset.blockId), startTextOffset: startPrefix.toString().length, endTextOffset: endPrefix.toString().length, startLine: Number(start.dataset.startLine), endLine: Number(end.dataset.endLine), quote: selected.toString() };
  document.getElementById("selected")!.textContent = selection.quote;
  add.disabled = !connected;
}
document.addEventListener("selectionchange", captureSelection);
add.addEventListener("click", () => {
  if (!selection || !feedback.value.trim()) return;
  add.disabled = true;
  api.postMessage({ type: "comment", version, selection, body: feedback.value });
});
for (const mode of ["source", "split"]) document.getElementById(mode)!.addEventListener("click", () => api.postMessage({ type: mode }));
document.getElementById("refresh")!.addEventListener("click", () => api.postMessage({ type: "refresh" }));
content.addEventListener("click", event => {
  const link = (event.target as Element).closest<HTMLElement>("[data-external]");
  if (link) { event.preventDefault(); api.postMessage({ type: "external", href: link.dataset.external }); }
});
window.addEventListener("message", event => {
  const message = event.data;
  if (message.type === "document") {
    const scroll = document.documentElement.scrollTop;
    content.innerHTML = message.html; // HTML comes only from the host's HTML-disabled Markdown renderer.
    version = message.version; connected = message.connected;
    selection = undefined; add.disabled = true;
    document.getElementById("selected")!.textContent = "Select a passage to comment.";
    feedbackStatus.textContent = message.status;
    document.documentElement.scrollTop = scroll;
    api.postMessage({ type: "rendered", version, text: content.textContent?.slice(0, 500) });
  }
  if (message.type === "comments") {
    comments.replaceChildren();
    for (const row of message.rows) {
      const article = document.createElement("article");
      const label = document.createElement("small"); label.textContent = `${row.authorUserId} · ${row.outdated ? "Earlier revision" : "Current revision"}`;
      const quote = document.createElement("blockquote"); quote.textContent = row.anchor?.quote ?? "";
      const body = document.createElement("p"); body.textContent = row.body;
      article.append(label, quote, body);
      if (!row.outdated && row.anchor && Number.isInteger(row.anchor.startBlock)) {
        const passage = document.createElement("button"); passage.textContent = "Show passage";
        passage.addEventListener("click", () => showPassage(row.anchor)); article.append(passage);
      }
      comments.append(article);
    }
  }
  if (message.type === "saved") { feedback.value = ""; api.setState({ draft: "" }); feedbackStatus.textContent = "Comment saved to MemQL."; }
  if (message.type === "error") { feedbackStatus.textContent = message.message; add.disabled = !connected || !selection; }
});
api.postMessage({ type: "ready" });

function showPassage(anchor: {startBlock:number;endBlock:number;startTextOffset:number;endTextOffset:number}) {
  const start = content.querySelector<HTMLElement>(`[data-block-id="${anchor.startBlock}"]`);
  const end = content.querySelector<HTMLElement>(`[data-block-id="${anchor.endBlock}"]`);
  if (!start || !end) return;
  function at(element: HTMLElement, offset: number): [Node, number] | undefined {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    let node: Node | null;
    while ((node = walker.nextNode())) {
      const length = node.textContent?.length ?? 0;
      if (offset <= length) return [node, offset];
      offset -= length;
    }
  }
  const from = at(start, anchor.startTextOffset), to = at(end, anchor.endTextOffset);
  if (!from || !to) return;
  const range = document.createRange(); range.setStart(...from); range.setEnd(...to);
  const selected = window.getSelection(); selected?.removeAllRanges(); selected?.addRange(range);
  start.scrollIntoView({ block: "center", behavior: "smooth" });
}
