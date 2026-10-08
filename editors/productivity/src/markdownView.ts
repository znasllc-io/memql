export {};
declare function acquireVsCodeApi(): { postMessage(message: unknown): void; getState(): unknown; setState(state: unknown): void };
const api = acquireVsCodeApi();
const byId = (id: string) => document.getElementById(id)!;
const content = byId("content");
const feedback = byId("feedback") as HTMLTextAreaElement;
const instruction = byId("revision-instruction") as HTMLTextAreaElement;
const add = byId("add") as HTMLButtonElement;
const annotate = byId("annotate") as HTMLButtonElement;
const prepare = byId("prepare-revision") as HTMLButtonElement;
type Anchor = { kind?: "markdown"; intent?: "extend"; scope?: "section"; sectionPath?: string[]; startLine: number; endLine: number; quote: string; startBlock: number; endBlock: number; startTextOffset: number; endTextOffset: number; prefix?: string; suffix?: string } | { kind: "document-end"; quote: string };
type ReviewRow = { id: string; body: string; outdated?: boolean; anchor: Anchor };
const state = (api.getState() ?? {}) as { draft?: string; anchor?: Anchor; draftVersion?: number; source?: string; instruction?: string; reviewOpen?: boolean; included?: string[]; seen?: string[] };
let restored = false;
let selection: Anchor | undefined;
let draftAnchor = state.anchor;
let selectionRect: DOMRect | undefined;
let selectionRange: Range | undefined;
let version = 0, sourceIdentity = "";
let connected = false, commentBusy = false, revisionBusy = false, preparing = false;
let revision: Record<string, any> | undefined;
let rows: ReviewRow[] = [];
const selected = new Set<string>(state.included ?? []);
const seen = new Set<string>(state.seen ?? []);
feedback.value = state.draft ?? ""; instruction.value = state.instruction ?? "";
function saveState() { const saved = { draft: feedback.value, anchor: draftAnchor, source: sourceIdentity || state.source, instruction: instruction.value, reviewOpen: !byId("review-panel").hidden, included: [...selected], seen: [...seen] }; api.setState(saved); if (restored) api.postMessage({type:"draftState",state:saved}); }
function activeRun() { return revision && !["succeeded", "failed", "cancelled"].includes(revision.status); }
function controls() {
  annotate.disabled = !connected || (!selection && !(feedback.value && draftAnchor));
  annotate.title = selection ? "Add feedback on this selection" : feedback.value && draftAnchor ? "Continue your feedback" : "Select text to add feedback";
  for (const button of Array.from(document.querySelectorAll<HTMLButtonElement>("#extend,.section-extend"))) button.disabled = !connected;
  add.disabled = !connected || !draftAnchor || !feedback.value.trim() || commentBusy;
  add.textContent = commentBusy ? "Adding…" : "Add to review";
  prepare.disabled = !connected || revisionBusy || !!activeRun() || selected.size === 0;
  prepare.textContent = revisionBusy ? "Submitting…" : "Propose changes";
  byId("review-submit").hidden = !!activeRun() || selected.size === 0;
  byId("review-footer").hidden = byId("review-submit").hidden && !byId("review-actions").childElementCount;
  const count = rows.filter(row => !row.outdated).length;
  byId("note-count").textContent = String(count); byId("note-count").hidden = !count;
}
function showReview(open: boolean) { document.body.classList.toggle("review-open", open); byId("review-panel").hidden = !open; byId("review-toggle").setAttribute("aria-expanded", String(open)); if (open) byId("selection-tools").hidden = true; saveState(); }
function position(element: HTMLElement, rect?: DOMRect, compact = false) {
  const width = compact ? 236 : Math.min(360, window.innerWidth - 32);
  const height = compact ? 42 : Math.min(340, window.innerHeight - 32);
  element.style.left = `${Math.max(16, Math.min(window.innerWidth - width - 16, rect ? rect.left + rect.width / 2 - width / 2 : (window.innerWidth - width) / 2))}px`;
  const below = rect ? rect.bottom + 10 : Math.max(90, window.innerHeight / 3);
  element.style.top = `${Math.max(16, Math.min(window.innerHeight - height - 16, below + height <= window.innerHeight ? below : (rect?.top ?? below) - height - 10))}px`;
  element.style.maxHeight = `${Math.max(180,window.innerHeight - 32)}px`; element.style.overflowY = "auto";
}
function highlight(name: string, ranges: Range[]) {
  const css = (window as any).CSS, Constructor = (window as any).Highlight;
  if (css?.highlights && Constructor) { if (ranges.length) css.highlights.set(name, new Constructor(...ranges)); else css.highlights.delete(name); }
}
function openComposer(anchor: Anchor, rect?: DOMRect) {
  // A second selection must never silently move an unfinished note.
  if (feedback.value.trim() && draftAnchor && JSON.stringify(draftAnchor) !== JSON.stringify(anchor)) {
    byId("composer-status").textContent = "Your unfinished note is still attached to its original selection. Save it before starting another.";
  } else { draftAnchor = anchor; byId("composer-status").textContent = ""; }
  const extend = isExtension(draftAnchor);
  byId("composer-title").textContent = extend ? draftAnchor?.kind === "document-end" ? "Extend document" : draftAnchor?.scope === "section" ? "Extend section" : "Extend passage" : "Add feedback";
  feedback.setAttribute("aria-label", extend ? "Extension request" : "Feedback");
  feedback.placeholder = extend ? "What would you like to add here?" : "What would you like to change?";
  byId("selected").textContent = draftAnchor?.quote ?? "";
  byId("selected").hidden = draftAnchor?.kind === "document-end";
  byId("composer").hidden = false; byId("selection-tools").hidden = true;
  highlight("memql-active", selectionRange ? [selectionRange] : []);
  position(byId("composer"), rect); controls(); saveState(); feedback.focus();
}
function closeComposer() { byId("composer").hidden = true; highlight("memql-active", []); saveState(); content.focus({ preventScroll: true }); }
function mapped(node: Node | null): HTMLElement | null {
  return (node?.nodeType === Node.ELEMENT_NODE ? node as Element : node?.parentElement)?.closest<HTMLElement>("[data-start-line][data-end-line]") ?? null;
}
function captureSelection() {
  if (!byId("composer").hidden) return;
  const selected = window.getSelection();
  if (!selected?.rangeCount || selected.isCollapsed) { selection = undefined; selectionRange = undefined; byId("selection-tools").hidden = true; controls(); return; }
  const range = selected.getRangeAt(0);
  if (!content.contains(range.startContainer) || !content.contains(range.endContainer)) { selection = undefined; byId("selection-tools").hidden = true; controls(); return; }
  const start = mapped(range.startContainer), end = mapped(range.endContainer);
  if (!start || !end || !selected.toString().trim()) return;
  const prefix = document.createRange(); prefix.selectNodeContents(start); prefix.setEnd(range.startContainer,range.startOffset);
  const endPrefix = document.createRange(); endPrefix.selectNodeContents(end); endPrefix.setEnd(range.endContainer,range.endOffset);
  selection = {startBlock:Number(start.dataset.blockId),endBlock:Number(end.dataset.blockId),startTextOffset:prefix.toString().length,endTextOffset:endPrefix.toString().length,startLine:Number(start.dataset.startLine),endLine:Number(end.dataset.endLine),quote:selected.toString(),prefix:prefix.toString().slice(-80),suffix:(end.textContent ?? "").slice(endPrefix.toString().length,endPrefix.toString().length+80)};
  selectionRange = range.cloneRange(); selectionRect = typeof range.getBoundingClientRect === "function" ? range.getBoundingClientRect() : undefined;
  byId("selection-tools").hidden = !connected; position(byId("selection-tools"),selectionRect,true); controls();
}
document.addEventListener("selectionchange",captureSelection);
for (const id of ["selection-feedback", "selection-extend"]) byId(id).addEventListener("mousedown", event => event.preventDefault());
annotate.addEventListener("mousedown",event => event.preventDefault());
for (const id of ["selection-feedback","annotate"]) byId(id).addEventListener("click",() => { const anchor = selection ?? draftAnchor; if (anchor && connected) openComposer(anchor,selectionRect); });
byId("selection-extend").addEventListener("click", () => { if (selection && selection.kind !== "document-end" && connected) openComposer({...selection, intent:"extend"}, selectionRect); });
byId("extend").addEventListener("click",() => openComposer({kind:"document-end",quote:"End of document"},byId("extend").getBoundingClientRect()));
byId("composer-close").addEventListener("click",closeComposer);
byId("review-toggle").addEventListener("click",() => showReview(byId("review-panel").hidden));
byId("review-close").addEventListener("click",() => { showReview(false); byId("review-toggle").focus(); });
feedback.addEventListener("input",() => { controls(); saveState(); });
instruction.addEventListener("input",saveState);
add.addEventListener("click",() => {
  if (add.disabled || !draftAnchor) return;
  commentBusy = true; controls(); byId("composer-status").textContent = "";
  api.postMessage({type:"comment",version,selection:draftAnchor,body:feedback.value});
});
prepare.addEventListener("click",() => {
  if (prepare.disabled) return;
  byId("status").textContent = ""; revisionBusy = true; preparing = true; controls(); renderRevision();
  api.postMessage({type:"prepareRevision",version,commentIds:[...selected],instruction:instruction.value});
});
document.addEventListener("keydown",event => {
  if (event.key === "Escape") { if (!byId("composer").hidden) closeComposer(); else if (!byId("review-panel").hidden) { showReview(false); byId("review-toggle").focus(); } else byId("selection-tools").hidden = true; }
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter" && !byId("composer").hidden) { event.preventDefault(); add.click(); }
  if ((event.ctrlKey || event.metaKey) && event.altKey && event.key.toLowerCase() === "m" && selection && connected) { event.preventDefault(); openComposer(selection,selectionRect); }
});
window.addEventListener("scroll",() => { byId("selection-tools").hidden = true; },{passive:true});
window.addEventListener("resize",() => { byId("selection-tools").hidden = true; if (!byId("composer").hidden) position(byId("composer")); });
for (const mode of ["source","reading","split"]) byId(mode).addEventListener("click",() => api.postMessage({type:mode}));
const views = Array.from(document.querySelectorAll<HTMLButtonElement>(".views button"));
views.forEach((button,index) => button.addEventListener("keydown",event => {
  const next = event.key === "ArrowRight" ? (index+1)%views.length : event.key === "ArrowLeft" ? (index+views.length-1)%views.length : event.key === "Home" ? 0 : event.key === "End" ? views.length-1 : undefined;
  if (next !== undefined) { event.preventDefault(); views[next].focus(); }
}));
content.addEventListener("click",event => { const link = (event.target as Element).closest<HTMLElement>("[data-external]"); if (link) { event.preventDefault(); api.postMessage({type:"external",href:link.dataset.external}); } });
function textElement(tag: string, text: string, className = "") { const element = document.createElement(tag); element.textContent = text; element.className = className; return element; }
function rangeFor(anchor: Anchor): Range | undefined {
  if (anchor.kind === "document-end") return;
  const start = content.querySelector<HTMLElement>(`[data-block-id="${anchor.startBlock}"]`), end = content.querySelector<HTMLElement>(`[data-block-id="${anchor.endBlock}"]`);
  if (!start || !end) return;
  const at = (element:HTMLElement,offset:number):[Node,number]|undefined => {
    const walker = document.createTreeWalker(element,NodeFilter.SHOW_TEXT); let node:Node|null;
    while ((node=walker.nextNode())) { const length=node.textContent?.length ?? 0; if (offset<=length) return [node,offset]; offset-=length; }
  };
  const from=at(start,anchor.startTextOffset),to=at(end,anchor.endTextOffset); if(!from || !to)return;
  const range=document.createRange();range.setStart(...from);range.setEnd(...to);return range;
}
function isExtension(anchor?: Anchor) { return anchor?.kind === "document-end" || anchor?.intent === "extend"; }
function location(anchor?: Anchor): string {
  if (anchor?.kind === "document-end") return "Document end";
  if (anchor?.scope === "section") return anchor.quote;
  return anchor?.sectionPath?.at(-1) || "Selected passage";
}
function jumpTo(anchor: Anchor) {
  const range = rangeFor(anchor);
  const target = anchor.kind === "document-end" ? byId("extend") : mapped(range?.startContainer ?? null)
    ?? content.querySelector<HTMLElement>(`[data-start-line="${anchor.startLine}"]`);
  target?.scrollIntoView?.({ block: window.innerWidth < 1000 ? "start" : "center", behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth" });
  if (range) { highlight("memql-active", [range]); setTimeout(() => highlight("memql-active", []), 2500); }
  // Navigation keeps the review, its scroll position and the invoking control.
}
function locationButton(anchor: Anchor, label = location(anchor)) {
  const jump = textElement("button", label, "passage") as HTMLButtonElement;
  jump.setAttribute("aria-label", `Show ${location(anchor)} in document`);
  jump.addEventListener("click", () => jumpTo(anchor));
  return jump;
}
function editLocation(edit: Record<string, any>): Anchor | undefined {
  const before = String(edit.before ?? "");
  if (!before) return {kind:"document-end", quote:"End of document"};
  const source = sourceIdentity.replace(/\r\n/g, "\n");
  const offset = source.indexOf(before);
  if (offset < 0 || source.indexOf(before, offset + 1) >= 0) return;
  const line = source.slice(0, offset).split("\n").length - 1;
  const blocks = Array.from(content.querySelectorAll<HTMLElement>("[data-block-id]"));
  const target = blocks.filter(block => Number(block.dataset.startLine) <= line && Number(block.dataset.endLine) > line).at(-1);
  if (!target) return;
  const heading = blocks.filter(block => /^H[1-6]$/.test(block.tagName) && Number(block.dataset.startLine) <= line).at(-1);
  return {kind:"markdown", quote:target.textContent ?? "", startBlock:Number(target.dataset.blockId), endBlock:Number(target.dataset.blockId),
    startLine:Number(target.dataset.startLine), endLine:Number(target.dataset.endLine), startTextOffset:0, endTextOffset:target.textContent?.length ?? 0,
    sectionPath:heading ? [heading.textContent ?? ""] : []};
}
function sectionTools() {
  for (const heading of Array.from(content.querySelectorAll<HTMLElement>("h1,h2,h3,h4,h5,h6"))) {
    if (!heading.dataset.blockId) continue;
    const quote = heading.textContent?.trim() ?? "";
    const button = document.createElement("button");
    button.className = "section-extend icon-button";
    button.setAttribute("aria-label", `Extend section: ${quote}`);
    button.title = "Extend section";
    button.innerHTML = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>';
    button.addEventListener("click", () => {
      if (!connected) return;
      const anchor: Anchor = { kind:"markdown", intent:"extend", scope:"section", quote,
        startLine:Number(heading.dataset.startLine), endLine:Number(heading.dataset.endLine),
        startBlock:Number(heading.dataset.blockId), endBlock:Number(heading.dataset.blockId), startTextOffset:0, endTextOffset:quote.length };
      selectionRange = undefined;
      openComposer(anchor, button.getBoundingClientRect());
    });
    heading.append(button);
  }
}
function requestNote(row: ReviewRow, editable: boolean): HTMLElement {
  const article = textElement("article", "", "note");
  const head = textElement("div", "", "note-head");
  const meta = textElement("div", "", "note-location");
  meta.append(textElement("span", isExtension(row.anchor) ? "Extend" : "Feedback", "request-kind"));
  if (row.outdated) meta.append(textElement("span", location(row.anchor)));
  else meta.append(locationButton(row.anchor));
  head.append(meta);
  if (editable) {
    const toggle = document.createElement("button");
    toggle.className = "include-toggle";
    toggle.setAttribute("role", "switch");
    toggle.setAttribute("aria-label", `Include request: ${row.body.slice(0, 120)}`);
    const paint = () => { const included = selected.has(row.id); toggle.setAttribute("aria-checked", String(included)); toggle.textContent = included ? "Included" : "Excluded"; article.classList.toggle("excluded", !included); };
    toggle.addEventListener("click", () => { if (selected.has(row.id)) selected.delete(row.id); else selected.add(row.id); paint(); controls(); saveState(); });
    paint(); head.append(toggle);
  }
  article.append(head, textElement("p", row.body));
  if (row.anchor?.kind !== "document-end" && row.anchor?.scope !== "section") {
    const quote = document.createElement("details"); quote.className = "request-quote";
    quote.append(textElement("summary", "Selected text"), textElement("blockquote", row.anchor?.quote ?? "")); article.append(quote);
  }
  return article;
}
function renderComments() {
  const list = byId("comments"); list.replaceChildren();
  const inFlight = preparing || !!activeRun();
  const captured = new Set<string>(preparing ? selected : revision?.proposal?.commentIds ?? []);
  const current = rows.filter(row => !row.outdated && (!inFlight || !captured.has(row.id)));
  const earlier = rows.filter(row => row.outdated && !captured.has(row.id));
  if (current.length) {
    list.append(textElement("h3", inFlight ? "For your next review" : "Requests"));
    if (!inFlight) list.append(textElement("p", "Include the requests you want in this proposal.", "muted request-help"));
    for (const row of current) list.append(requestNote(row, !inFlight));
  }
  if (!rows.length && !revision) list.append(textElement("p", "Select text for feedback, or use + beside a heading to extend a section.", "empty"));
  if (earlier.length) {
    const details = textElement("details", "", "earlier"); details.append(textElement("summary", `Earlier requests (${earlier.length})`));
    for (const row of earlier) details.append(requestNote(row, false)); list.append(details);
  }
  highlight("memql-notes", rows.filter(row => !row.outdated).map(row => rangeFor(row.anchor)).filter((range): range is Range => !!range));
}
function renderRevision() {
  renderComments();
  const root = byId("revision"); root.replaceChildren(); const actions = byId("review-actions"); actions.replaceChildren();
  const pending = rows.some(row => !row.outdated && !revision?.proposal?.commentIds?.includes(row.id));
  document.body.classList.toggle("review-draft", !preparing && !activeRun() && (pending || !revision));
  if (preparing) { root.append(textElement("p", "Preparing changes…", "phase busy")); return; }
  if (!revision) return;
  const status = revision, proposal = status.proposal ?? {};
  const terminal = ["succeeded", "failed", "cancelled"].includes(status.status);
  const awaiting = !!status.approvalId && !status.decision && status.status === "waiting";
  let panel: HTMLElement = root;
  if (terminal && pending) {
    const previous = document.createElement("details"); previous.className = "earlier";
    previous.append(textElement("summary", "Previous review")); root.append(previous); panel = previous;
  }
  const phase = status.decision === "rejected" ? "Changes declined" : status.status === "succeeded" ? status.result?.applied ? "Changes applied" : "No changes needed"
    : status.status === "failed" || status.status === "cancelled" ? "Couldn’t prepare changes" : awaiting ? "Proposed changes" : status.decision === "approved" ? "Applying changes…" : "Preparing changes…";
  panel.append(textElement("h3", phase, !terminal && !awaiting ? "phase busy" : ""));
  if (status.errorMessage) panel.append(textElement("p", String(status.errorMessage), "review-error"));
  const requested: ReviewRow[] = Array.isArray(proposal.comments) ? proposal.comments : rows.filter(row => proposal.commentIds?.includes(row.id));
  const edits: Record<string, any>[] = Array.isArray(proposal.edits) ? proposal.edits.filter((edit: any) => edit.before !== edit.after) : [];
  const addressed = new Set(edits.flatMap(edit => edit.commentIds ?? []));
  for (const edit of edits) {
    const related = requested.filter(row => edit.commentIds?.includes(row.id));
    const destination = editLocation(edit);
    const card = document.createElement("details"); card.className = "change"; card.open = !terminal;
    const label = edit.after === "" ? "Remove passage" : edit.before === "" || edit.after.startsWith(edit.before) ? "Add content" : "Revise passage";
    card.append(textElement("summary", related.length === 1 ? `${isExtension(related[0].anchor) ? "Extend" : "Feedback"}: ${location(destination ?? related[0].anchor)}` : label));
    for (const row of related) {
      const context = textElement("div", "", "request-context");
      context.append(textElement("p", row.body));
      if (!terminal) context.append(locationButton(destination ?? row.anchor, "Show in document"));
      card.append(context);
    }
    for (const [key, label, cls] of [["before", "Current", "before"], ["after", "Proposed", "after"]]) {
      const block = textElement("div", "", cls); block.append(textElement("div", label, "diff-label"), textElement("pre", String(edit[key] ?? "") || (key === "after" ? "Removed" : "New content"))); card.append(block);
    }
    if (edit.reason) { const why = textElement("details", "", "change-reason"); why.append(textElement("summary", "Why this change"), textElement("p", String(edit.reason))); card.append(why); }
    panel.append(card);
  }
  if (awaiting || (!terminal && !edits.length)) {
    for (const row of requested.filter(row => !addressed.has(row.id))) {
      const note = requestNote(row, false);
      if (awaiting) note.append(textElement("p", "No change proposed for this request.", "muted"));
      panel.append(note);
    }
  }
  const summary = proposal.summary ?? status.result?.summary;
  if (summary) {
    if (!edits.length) panel.append(textElement("p", String(summary), "proposal-summary"));
    else { const overview = textElement("details", "", "proposal-summary"); overview.append(textElement("summary", "Summary"), textElement("p", String(summary))); panel.append(overview); }
  }
  const action = (label: string, type: string, decision?: string, primary = false) => {
    const button = textElement("button", label, primary ? "primary" : "secondary") as HTMLButtonElement;
    button.disabled = revisionBusy || (decision === "approved" && !connected);
    button.addEventListener("click", () => { revisionBusy = true; controls(); renderRevision(); api.postMessage({ type, approvalId:status.approvalId, decision }); }); return button;
  };
  if (typeof proposal.revisedContent === "string") { const compare = action("Compare full document", "compareRevision"); compare.className = "compare secondary"; panel.append(compare); }
  if (status.prepared === false) actions.append(action("Retry submission", "resumePreparation", undefined, true));
  else if (awaiting) actions.append(action("Decline", "decideRevision", "rejected"), action("Approve & apply", "decideRevision", "approved", true));
  else if (status.decision === "approved" && status.status === "waiting") actions.append(action("Resume approved changes", "decideRevision", "approved", true));
}
window.addEventListener("message",event=>{
  const message=event.data;
  if(message.type==="restoreDraft") {
    if(message.state && !restored) {
      Object.assign(state,message.state); draftAnchor=state.anchor; feedback.value=state.draft??""; instruction.value=state.instruction??"";
      selected.clear(); seen.clear(); for(const id of state.included??[])selected.add(id); for(const id of state.seen??[])seen.add(id);
      showReview(state.reviewOpen??false); controls();
    }
    restored=true;
  }
  if(message.type==="viewMode")for(const mode of ["source","reading","split"])byId(mode).setAttribute("aria-pressed",String(mode===message.mode));
  if(message.type==="document") {
    const scroll=document.documentElement.scrollTop;
    content.innerHTML=message.html; // Host uses the HTML-disabled Markdown renderer.
    sectionTools(); version=message.version;connected=message.connected;
    const incoming=String(message.sourceIdentity??version);
    if(draftAnchor && (sourceIdentity||state.source) && (sourceIdentity||state.source)!==incoming){draftAnchor=undefined;byId("composer-status").textContent="The document changed. Your note is preserved; select its passage again before saving.";}
    sourceIdentity=incoming;selection=undefined;selectionRange=undefined;byId("selection-tools").hidden=true;
    if(!connected)byId("status").textContent=message.status??"Save the document before sharing feedback.";
    document.documentElement.scrollTop=scroll;renderRevision();controls();saveState();api.postMessage({type:"rendered",version,text:content.textContent?.slice(0,500)});
  }
  if(message.type==="comments") {
    const next=message.rows??[],different=JSON.stringify(next)!==JSON.stringify(rows);rows=next;
    for(const row of rows){if(!row.outdated&&!seen.has(row.id))selected.add(row.id);if(row.outdated)selected.delete(row.id);seen.add(row.id);}
    const available=new Set(rows.filter(row=>!row.outdated).map(row=>row.id));for(const id of selected)if(!available.has(id))selected.delete(id);
    if(different)renderRevision();controls();saveState();
  }
  if(message.type==="revision") {const changed=message.status?.approvalId&&message.status.approvalId!==revision?.approvalId;const different=JSON.stringify(message.status)!==JSON.stringify(revision);revision=message.status;if(different)renderRevision();controls();if(changed){byId("status").textContent="";showReview(true);}}
  if(message.type==="revisionIdle"){revisionBusy=false;preparing=false;renderRevision();controls();}
  if(message.type==="saved"){commentBusy=false;feedback.value="";draftAnchor=undefined;closeComposer();byId("status").textContent="Added to review.";controls();showReview(true);}
  if(message.type==="error"){commentBusy=false;revisionBusy=false;preparing=false;renderRevision();controls();if(!byId("composer").hidden)byId("composer-status").textContent=message.message;else{byId("status").textContent=message.message;showReview(true);}}
  if(message.type==="notice")byId("status").textContent=message.message;
});
showReview(state.reviewOpen??false);controls();api.postMessage({type:"ready"});
