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
let revisionBusy = false;
let revisionCompared = false;
let revisionStatus: Record<string, any> | undefined;
const selectedComments = new Set<string>();
const instruction = document.getElementById("revision-instruction") as HTMLTextAreaElement;
const prepareRevision = document.getElementById("prepare-revision") as HTMLButtonElement;
function updateRevisionControls() { prepareRevision.disabled = revisionBusy || !connected || selectedComments.size === 0 || !instruction.value.trim(); }
instruction.addEventListener("input", updateRevisionControls);
prepareRevision.addEventListener("click", () => {
  revisionBusy = true; revisionCompared = false; updateRevisionControls();
  api.postMessage({ type: "prepareRevision", version, commentIds: [...selectedComments], instruction: instruction.value });
});
function renderRevision() {
  const panel = document.getElementById("revision")!; panel.replaceChildren();
  const status = revisionStatus; if (!status) return;
  const heading = document.createElement("h3"); heading.textContent = "Change request";
  const phase = document.createElement("p");
  phase.textContent = status.errorMessage || status.failureReason || (status.prepared === false ? "Request saved; finish preparing it for approval" : status.outputArtifactId ? "Draft ready for comparison" : status.decision === "approved" ? `Approved · ${status.status}. Refresh to check progress.` : status.decision === "rejected" ? "Request declined" : "Waiting for your approval");
  const proposal = status.proposal || {};
  const revision = document.createElement("small"); revision.textContent = `Saved revision: ${proposal.revision ?? ""}`;
  const requested = document.createElement("p"); requested.textContent = proposal.instruction ?? "";
  const selected = document.createElement("div");
  for (const comment of proposal.comments ?? []) {
    const quote = document.createElement("blockquote"); quote.textContent = comment.anchor?.quote ?? "";
    const body = document.createElement("p"); body.textContent = comment.body ?? ""; selected.append(quote, body);
  }
  const source = document.createElement("details");
  const summary = document.createElement("summary"); summary.textContent = "Review captured source";
  const code = document.createElement("pre"); code.textContent = proposal.content ?? ""; source.append(summary, code);
  panel.append(heading, phase, revision, requested, selected, source);
  const button = (label: string, type: string, decision?: string) => {
    const el = document.createElement("button"); el.textContent = label; el.disabled = revisionBusy;
    el.addEventListener("click", () => { revisionBusy = true; updateRevisionControls(); renderRevision(); api.postMessage({ type, approvalId: status.approvalId, decision }); }); panel.append(el);
  };
  if (status.prepared === false) button("Finish preparing request", "resumePreparation");
  else if (!status.decision) { button("Approve draft job", "decideRevision", "approved"); button("Decline", "decideRevision", "rejected"); }
  else if (status.decision === "approved" && status.status === "waiting") button("Resume approved job", "decideRevision", "approved");
  if (status.outputArtifactId && status.compositionStatus === "ready") {
    button("Compare draft", "compareRevision");
    if (revisionCompared) button("Apply compared draft", "applyRevision");
  }
}
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
    selection = undefined; add.disabled = true; updateRevisionControls();
    document.getElementById("selected")!.textContent = "Select a passage to comment.";
    feedbackStatus.textContent = message.status;
    document.documentElement.scrollTop = scroll;
    api.postMessage({ type: "rendered", version, text: content.textContent?.slice(0, 500) });
  }
  if (message.type === "comments") {
    comments.replaceChildren(); selectedComments.clear(); updateRevisionControls();
    for (const row of message.rows) {
      const article = document.createElement("article");
      const label = document.createElement("small"); label.textContent = `${row.authorUserId} · ${row.outdated ? "Earlier revision" : "Current revision"}`;
      const quote = document.createElement("blockquote"); quote.textContent = row.anchor?.quote ?? "";
      const body = document.createElement("p"); body.textContent = row.body;
      article.append(label, quote, body);
      if (!row.outdated && typeof row.id === "string") {
        const pick = document.createElement("input"); pick.type = "checkbox";
        const pickLabel = document.createElement("label"); pickLabel.append(pick, " Include in change request"); article.append(pickLabel);
        pick.addEventListener("change", () => { if (pick.checked) selectedComments.add(row.id); else selectedComments.delete(row.id); updateRevisionControls(); });
      }
      if (!row.outdated && row.anchor && Number.isInteger(row.anchor.startBlock)) {
        const passage = document.createElement("button"); passage.textContent = "Show passage";
        passage.addEventListener("click", () => showPassage(row.anchor)); article.append(passage);
      }
      comments.append(article);
    }
  }
  if (message.type === "revision") { revisionStatus = message.status; renderRevision(); }
  if (message.type === "revisionApplied") { revisionCompared = false; renderRevision(); }
  if (message.type === "revisionCompared") { revisionCompared = true; renderRevision(); }
  if (message.type === "revisionIdle") { revisionBusy = false; updateRevisionControls(); renderRevision(); }
  if (message.type === "saved") { feedback.value = ""; api.setState({ draft: "" }); feedbackStatus.textContent = "Comment saved to MemQL."; }
  if (message.type === "error") { revisionBusy = false; updateRevisionControls(); renderRevision(); feedbackStatus.textContent = message.message; add.disabled = !connected || !selection; }
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
