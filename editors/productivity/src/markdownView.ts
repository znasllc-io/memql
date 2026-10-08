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
type Anchor = { kind?: "markdown"; startLine: number; endLine: number; quote: string; startBlock: number; endBlock: number; startTextOffset: number; endTextOffset: number; prefix?: string; suffix?: string } | { kind: "document-end"; quote: string };
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
  (byId("extend") as HTMLButtonElement).disabled = !connected;
  add.disabled = !connected || !draftAnchor || !feedback.value.trim() || commentBusy;
  add.textContent = commentBusy ? "Adding…" : "Add to review";
  prepare.disabled = !connected || revisionBusy || !!activeRun() || selected.size === 0;
  prepare.textContent = revisionBusy ? "Submitting…" : selected.size ? `Propose changes · ${selected.size}` : "Propose changes";
  byId("review-submit").hidden = !!activeRun() || selected.size === 0;
  byId("review-footer").hidden = byId("review-submit").hidden && !byId("review-actions").childElementCount;
  const count = rows.filter(row => !row.outdated).length;
  byId("note-count").textContent = String(count); byId("note-count").hidden = !count;
}
function showReview(open: boolean) { byId("review-panel").hidden = !open; byId("review-toggle").setAttribute("aria-expanded", String(open)); if (open) byId("selection-tools").hidden = true; saveState(); }
function position(element: HTMLElement, rect?: DOMRect, compact = false) {
  const width = compact ? 154 : Math.min(360, window.innerWidth - 32);
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
  const extend = draftAnchor?.kind === "document-end";
  byId("composer-title").textContent = extend ? "Extend document" : "Add feedback";
  feedback.setAttribute("aria-label", extend ? "Extension request" : "Feedback");
  feedback.placeholder = extend ? "What should come next? Describe the sections, examples or detail to add…" : "Rephrase, remove, move or expand this passage…";
  byId("selected").textContent = draftAnchor?.quote ?? "";
  byId("selected").hidden = extend;
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
byId("selection-feedback").addEventListener("mousedown", event => event.preventDefault());
annotate.addEventListener("mousedown",event => event.preventDefault());
for (const id of ["selection-feedback","annotate"]) byId(id).addEventListener("click",() => { const anchor = selection ?? draftAnchor; if (anchor && connected) openComposer(anchor,selectionRect); });
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
function renderComments() {
  const list = byId("comments"); list.replaceChildren();
  const current = rows.filter(row=>!row.outdated), earlier = rows.filter(row=>row.outdated);
  if (!current.length && !revision) list.append(textElement("p","Select text to leave feedback, or describe what to add at the end of the document.","empty"));
  if (current.length) list.append(textElement("h3",`Feedback · ${current.length}`));
  const note = (row:ReviewRow) => {
    const article=textElement("article","","note"); const head=textElement("div","","note-head");
    const kind=row.anchor?.kind==="document-end" ? "Extension" : "Passage feedback";
    if (!row.outdated) {
      const label=document.createElement("label"),pick=document.createElement("input");pick.type="checkbox";pick.checked=selected.has(row.id);pick.setAttribute("aria-label",`Include ${kind.toLowerCase()}: ${row.body.slice(0,80)}`);
      pick.addEventListener("change",()=>{if(pick.checked)selected.add(row.id);else selected.delete(row.id);controls();saveState();});label.append(pick,kind);head.append(label);
    } else head.textContent=`${kind} · Earlier revision`;
    article.append(head);
    if(row.anchor?.kind!=="document-end") article.append(textElement("blockquote",row.anchor?.quote ?? ""));
    article.append(textElement("p",row.body));
    if(!row.outdated){const jump=document.createElement("button");jump.className="passage";jump.textContent=row.anchor?.kind==="document-end"?"Go to end":"Show passage";jump.addEventListener("click",()=>{const range=rangeFor(row.anchor);showReview(false);const target=row.anchor?.kind==="document-end"?byId("extend"):mapped(range?.startContainer??null);target?.scrollIntoView?.({block:"center",behavior:window.matchMedia?.("(prefers-reduced-motion: reduce)").matches?"instant":"smooth"});if(range){highlight("memql-active",[range]);setTimeout(()=>highlight("memql-active",[]),2500);}});article.append(jump);}
    return article;
  };
  for(const row of current)list.append(note(row));
  if(earlier.length){const details=textElement("details","","earlier");details.append(textElement("summary",`Earlier feedback · ${earlier.length}`));for(const row of earlier)details.append(note(row));list.append(details);}
  highlight("memql-notes",current.map(row=>rangeFor(row.anchor)).filter((range):range is Range=>!!range));
}
function renderRevision() {
  const root=byId("revision");root.replaceChildren();const actions=byId("review-actions");actions.replaceChildren();
  if(preparing){root.append(textElement("h3","Preparing changes"),textElement("p","Submitting your feedback…","phase busy"));return;}
  const pending=rows.some(row=>!row.outdated&&!revision?.proposal?.commentIds?.includes(row.id));
  let panel:HTMLElement=root;
  if(!activeRun()&&pending){
    root.append(textElement("h3","Ready to propose"),textElement("p","Choose Propose changes to preview the edits for your feedback.","phase"));
    if(revision){const previous=document.createElement("details");previous.className="earlier";previous.append(textElement("summary","Previous request"));root.append(previous);panel=previous;}
  }
  if(!revision)return;
  const status=revision,proposal=status.proposal??{};
  const terminal=["succeeded","failed","cancelled"].includes(status.status);
  const awaiting=!!status.approvalId && !status.decision && status.status==="waiting";
  let phase=status.decision==="rejected"?"Changes declined. Your document is unchanged.":status.status==="succeeded"?(status.result?.applied?"Changes applied to the document.":"Review complete. No changes were needed."):status.status==="failed"||status.status==="cancelled"?"This request could not be completed.":awaiting?"Ready for your review":status.decision==="approved"?"Applying your approved changes…":"Analyzing your feedback…";
  panel.append(textElement("h3",awaiting?"Proposed changes":"Change request"),textElement("p",phase,`phase${!terminal&&!awaiting?" busy":""}`));
  if(status.errorMessage)panel.append(textElement("p",String(status.errorMessage),"muted"));
  const summary=proposal.summary??status.result?.summary;if(summary)panel.append(textElement("p",String(summary),"proposal-summary"));
  if(Array.isArray(proposal.edits))for(const [index,edit] of proposal.edits.entries()) {
    if(edit.before===edit.after)continue;
    const card=document.createElement("details");card.className="change";card.open=true;
    const label=edit.after===""?"Remove passage":edit.before===""||edit.after.startsWith(edit.before)?"Add content":"Revise passage";
    card.append(textElement("summary",`${index+1}. ${label}`),textElement("p",String(edit.reason??""),"reason"));
    for(const [key,label,cls] of [["before","Original","before"],["after","Proposed","after"]]) {
      const block=textElement("div","",cls);block.append(textElement("div",label,"diff-label"),textElement("pre",String(edit[key]??"")||(key==="after"?"Removed":"New content")));card.append(block);
    }
    panel.append(card);
  }
  const action=(label:string,type:string,decision?:string,primary=false)=>{const button=document.createElement("button");button.textContent=label;button.className=primary?"primary":"secondary";button.disabled=revisionBusy||(decision==="approved"&&!connected);button.addEventListener("click",()=>{revisionBusy=true;controls();renderRevision();api.postMessage({type,approvalId:status.approvalId,decision});});return button;};
  if(typeof proposal.revisedContent==="string") { const compare=action("Compare full document","compareRevision");compare.className="compare";panel.append(compare); }
  if(status.prepared===false)actions.append(action("Retry submission","resumePreparation",undefined,true));
  else if(awaiting)actions.append(action("Decline","decideRevision","rejected"),action("Approve & apply","decideRevision","approved",true));
  else if(status.decision==="approved"&&status.status==="waiting")actions.append(action("Resume approved changes","decideRevision","approved",true));
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
    version=message.version;connected=message.connected;
    const incoming=String(message.sourceIdentity??version);
    if(draftAnchor && (sourceIdentity||state.source) && (sourceIdentity||state.source)!==incoming){draftAnchor=undefined;byId("composer-status").textContent="The document changed. Your note is preserved; select its passage again before saving.";}
    sourceIdentity=incoming;selection=undefined;selectionRange=undefined;byId("selection-tools").hidden=true;
    if(!connected)byId("status").textContent=message.status??"Save the document before sharing feedback.";
    document.documentElement.scrollTop=scroll;renderComments();controls();saveState();api.postMessage({type:"rendered",version,text:content.textContent?.slice(0,500)});
  }
  if(message.type==="comments") {
    const next=message.rows??[],different=JSON.stringify(next)!==JSON.stringify(rows);rows=next;
    for(const row of rows){if(!row.outdated&&!seen.has(row.id))selected.add(row.id);if(row.outdated)selected.delete(row.id);seen.add(row.id);}
    const available=new Set(rows.filter(row=>!row.outdated).map(row=>row.id));for(const id of selected)if(!available.has(id))selected.delete(id);
    renderComments();if(different)renderRevision();controls();saveState();
  }
  if(message.type==="revision") {const changed=message.status?.approvalId&&message.status.approvalId!==revision?.approvalId;const different=JSON.stringify(message.status)!==JSON.stringify(revision);revision=message.status;if(different)renderRevision();controls();if(changed){byId("status").textContent="";showReview(true);}}
  if(message.type==="revisionIdle"){revisionBusy=false;preparing=false;renderRevision();controls();}
  if(message.type==="saved"){commentBusy=false;feedback.value="";draftAnchor=undefined;closeComposer();byId("status").textContent="Added to review.";controls();showReview(true);}
  if(message.type==="error"){commentBusy=false;revisionBusy=false;preparing=false;renderRevision();controls();if(!byId("composer").hidden)byId("composer-status").textContent=message.message;else{byId("status").textContent=message.message;showReview(true);}}
  if(message.type==="notice")byId("status").textContent=message.message;
});
showReview(state.reviewOpen??false);controls();api.postMessage({type:"ready"});
