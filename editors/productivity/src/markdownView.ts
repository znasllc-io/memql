import { renderProblem } from "./problemView.js";
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
// VS Code otherwise adds Cut, Copy and Paste to noneditable rendered text.
// Resolve each textarea's current state before the host builds its menu, also
// covering per-item fields and inputs temporarily locked during dictation.
document.addEventListener("contextmenu", event => {
  const inDocument = event.target instanceof Node && content.contains(event.target);
  if (inDocument) captureSelection();
  contextSelection = inDocument && !historyPreview && connected && selection
    ? { anchor: selection, rect: selectionRect } : undefined;
  document.body.dataset.vscodeContext = JSON.stringify({webviewSection:"markdownReadOnly",
    preventDefaultContextMenuItems:true,memqlMarkdownFeedback:mode==="review"&&!revisionLocked()&&!!contextSelection,memqlMarkdownNote:!!contextSelection});
  if (!((event.target instanceof HTMLTextAreaElement || event.target instanceof HTMLInputElement))) return;
  const readOnly = event.target.readOnly || event.target.disabled;
  event.target.dataset.vscodeContext = JSON.stringify({
    webviewSection: readOnly ? "markdownReadOnly" : "markdownInput",
    preventDefaultContextMenuItems: readOnly,
  });
}, true);
type Anchor = { kind?: "markdown"; intent?: "extend"; scope?: "section"; sectionPath?: string[]; sourceQuote?: string; startLine: number; endLine: number; quote: string; startBlock: number; endBlock: number; startTextOffset: number; endTextOffset: number; prefix?: string; suffix?: string } | { kind: "document-end"; quote: string; intent?: never; scope?: never; sectionPath?: never } | { kind: "document"; quote: string; intent?: never; scope?: never; sectionPath?: never };
type Attachment = { artifactId:string; version:number; revision:string; name:string; mimeType:string; size:number; uri:string };
type ReviewRow = { attachments?:Attachment[]; id: string; body: string; outdated?: boolean; anchor: Anchor };
const state = (api.getState() ?? {}) as { attachments?:Attachment[]; draft?: string; purpose?: "feedback"|"note"; anchor?: Anchor; draftVersion?: number; source?: string; instruction?: string; reviewOpen?: boolean; included?: string[]; seen?: string[]; decisions?: Record<string, "accepted" | "declined">; modifications?: Record<string,string>; expanded?: Record<string,boolean>; reviewSource?: string };
let attachments: Attachment[] = state.attachments ?? [];
const attachmentPreviews = new Map<string,string>();
const requestedPreviews = new Set<string>();
const uploading = new Map<string,string>();
const attachmentInput = byId("attachment-input") as HTMLInputElement;
let readingAttachments = false;
function attachmentRow(ref:Attachment, removable=false): HTMLElement {
  const row=textElement("div","","attachment");
  if(ref.mimeType.startsWith("image/")) {
    const preview=attachmentPreviews.get(ref.uri);
    if(preview) { const img=document.createElement("img");img.src=preview;img.alt="";img.loading="lazy";img.decoding="async";row.append(img); }
    else if(!requestedPreviews.has(ref.uri)){requestedPreviews.add(ref.uri);api.postMessage({type:"attachmentPreview",uri:ref.uri});}
  }
  const name=textElement("span",ref.name,"attachment-name");
  row.append(name,textElement("span",ref.mimeType==="text/markdown"?"Markdown":"Image","attachment-kind"));
  if(removable) {
    const remove=document.createElement("button");remove.className="icon-button";remove.textContent="×";remove.setAttribute("aria-label",`Remove ${ref.name}`);
    remove.disabled=commentBusy||revisionLocked();
    remove.addEventListener("click",()=>{attachments=attachments.filter(item=>item.artifactId!==ref.artifactId);renderAttachments();saveState();controls();byId("attach").focus();});
    row.append(remove);
  }
  return row;
}
function renderAttachments() {
  byId("attachments").replaceChildren(...attachments.map(ref=>attachmentRow(ref,true)));
  const status=byId("attachment-status");
  status.hidden=!uploading.size&&!readingAttachments;
  status.textContent=uploading.size?`Uploading ${[...uploading.values()].join(", ")}…`:readingAttachments?"Reading attachments…":"";
}
function fileBase64(file:File):Promise<string> {
  return new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(String(reader.result).split(",")[1]);reader.onerror=()=>reject(new Error("This file could not be read. Choose it again."));reader.readAsDataURL(file);});
}
byId("attach").addEventListener("click",()=>attachmentInput.click());
attachmentInput.addEventListener("change",async()=>{
  const files=Array.from(attachmentInput.files??[]);attachmentInput.value="";
  if(!files.length||composerPurpose!=="feedback"||revisionLocked()||commentBusy||uploading.size||readingAttachments)return;
  byId("composer-status").textContent="";
  let total=attachments.reduce((sum,ref)=>sum+ref.size,0),markdown=attachments.filter(ref=>ref.mimeType==="text/markdown").reduce((sum,ref)=>sum+ref.size,0);
  try {
    if(attachments.length+files.length>8)throw new Error("Attach up to eight images or Markdown files.");
    for(const file of files){const md=/\.md$/i.test(file.name);if(!/\.(md|png|jpe?g|gif|webp)$/i.test(file.name)||!file.size||file.size>(md?32*1024:8*1024*1024))throw new Error("Choose PNG, JPEG, GIF or WebP images up to 8 MiB, or Markdown files up to 32 KiB.");total+=file.size;if(md)markdown+=file.size;}
    if(total>16*1024*1024||markdown>64*1024)throw new Error("Attach up to 16 MiB in total, including up to 64 KiB of Markdown.");
    readingAttachments=true;renderAttachments();controls();
    for(const file of files){const base64=await fileBase64(file);const uploadId=crypto.randomUUID();uploading.set(uploadId,file.name);api.postMessage({type:"uploadAttachment",uploadId,name:file.name,base64});}
  } catch(error){byId("composer-status").textContent=(error as Error).message;}
  finally{readingAttachments=false;renderAttachments();controls();saveState();}
});
let restored = false;
let composerPurpose:"feedback"|"note"=state.purpose??"feedback";
let notesOpen=false;
let personalNotes:ReviewRow[]=[];
let activeNote="";
let mode = "reading";
let historyOpen=false, historyBusy=false, historyPreview=false;
let historyData: Record<string,any>={}, historyVersions: Record<string,any>[]=[];
let latestDocument: any;
let previewVersion: number|undefined;
let historyRetry: Record<string,unknown>={type:"history"};
const branchName=byId("branch-name") as HTMLInputElement;
function historyRequest(message:Record<string,unknown>) {
  if(historyBusy)return;
  historyRetry=message;historyBusy=true;byId("history-status").textContent=message.type==="historyFork"?"Creating branch…":"Loading…";
  byId("history-retry").hidden=true;historyControls();api.postMessage(message);
}
function historyControls() {
  for(const button of Array.from(document.querySelectorAll<HTMLButtonElement>("#history-panel button:not(#history-close)")))button.disabled=historyBusy;
  (byId("history-fork") as HTMLButtonElement).disabled=historyBusy||!branchName.value.trim()||!latestDocument?.connected;
  branchName.disabled=historyBusy;
  (byId("history-toggle") as HTMLButtonElement).disabled=!latestDocument?.historyAvailable;
  for(const button of views??[])button.disabled=historyPreview||button.id==="source"&&revisionLocked();
  (byId("notes-toggle") as HTMLButtonElement).disabled=historyPreview||!latestDocument?.historyAvailable;
}
function showHistory(open:boolean) {
  historyOpen=open;document.body.classList.toggle("history-open",open);byId("history-panel").hidden=!open;
  byId("history-toggle").setAttribute("aria-expanded",String(open));
  if(open){showNotes(false);closeComposer();byId("selection-tools").hidden=true;document.body.classList.remove("review-open");byId("review-panel").hidden=true;byId("review-toggle").setAttribute("aria-expanded","false");}
  else showReview(reviewRequested);
  renderNoteMarkers();
}
function renderHistory() {
  const list=byId("history-versions");list.replaceChildren();
  for(const entry of historyVersions){
    const button=textElement("button","","history-version") as HTMLButtonElement;
    button.setAttribute("aria-label",`Open version ${entry.version}`);button.setAttribute("aria-pressed",String(historyPreview&&entry.version===previewVersion));
    button.append(textElement("strong",`Version ${entry.version}${entry.current?" · Current":""}`));
    const date=entry.createdAt?new Date(entry.createdAt).toLocaleString():"";
    button.append(textElement("small",[date,entry.authorKind==="user"?"Human edit":entry.authorKind==="assistant"?"AI revision":entry.authorKind==="system"?"Snapshot":""].filter(Boolean).join(" · ")));
    if(entry.note)button.append(textElement("p",entry.note));
    button.addEventListener("click",()=>historyRequest({type:"historyVersion",version:entry.version}));list.append(button);
  }
  if(!historyVersions.length)list.append(textElement("p","No saved versions are available.","empty"));
  byId("history-more").hidden=!historyData.hasMore;
  const family=byId("history-family");family.replaceChildren();
  const link=(id:string,name:string,label:string)=>{const button=textElement("button",label,"passage");button.addEventListener("click",()=>historyRequest({type:"historyOpen",artifactId:id,name}));family.append(button);};
  if(historyData.parentArtifactId){family.append(textElement("small",`${historyData.branchName} · branched from version ${historyData.parentVersion}`));link(historyData.parentArtifactId,"Document","Open parent document");}
  for(const branch of historyData.branches??[])link(branch.artifactId,branch.name,branch.name);
  if(historyData.branchesHasMore)family.append(textElement("small","Showing the 100 most recent branches. All branches remain available in Files."));
  historyControls();
}
function syncChromeLayout(){document.documentElement.style.setProperty("--document-tools-offset",`${byId("document-chrome").getBoundingClientRect().height}px`);}
if(typeof ResizeObserver!=="undefined")new ResizeObserver(()=>{syncChromeLayout();renderNoteMarkers();}).observe(byId("document-chrome"));
const findQuery=byId("find-query") as HTMLInputElement;
let findRanges:Range[]=[],findIndex=-1;
function showFind(open:boolean) {
 byId("document-find").hidden=!open;syncChromeLayout();
 if(open){closeComposer();byId("selection-tools").hidden=true;findQuery.focus();findQuery.select();updateFind(false);}
 else {highlight("memql-find",[]);highlight("memql-find-active",[]);byId("find-marker").hidden=true;content.focus({preventScroll:true});}
 renderNoteMarkers();
}
function updateFind(scroll=true) {
 if(byId("document-find").hidden)return;
 const nodes:{node:Text;start:number;end:number}[]=[];let text="";
 const walker=document.createTreeWalker(content,NodeFilter.SHOW_TEXT);let node:Node|null;
 while((node=walker.nextNode())){if(node.parentElement?.closest("button"))continue;const value=node.textContent??"";nodes.push({node:node as Text,start:text.length,end:text.length+value.length});text+=value;}
 findRanges=[];findIndex=-1;
 if(findQuery.value){
  const escaped=findQuery.value.replace(/[.*+?^${}()|[\]\\]/g,"\\$&");
  const pattern=new RegExp(escaped,"giu");let match:RegExpExecArray|null;let startNode=0,endNode=0;
  while((match=pattern.exec(text))&&findRanges.length<5000){
   while(startNode<nodes.length&&nodes[startNode].end<=match.index)startNode++;
   while(endNode<nodes.length&&nodes[endNode].end<match.index+match[0].length)endNode++;
   const start=nodes[startNode],end=nodes[endNode];
   if(start&&end){const range=document.createRange();range.setStart(start.node,match.index-start.start);range.setEnd(end.node,match.index+match[0].length-end.start);findRanges.push(range);}
  }
 }
 if(findRanges.length)findIndex=0;showFindMatch(scroll);
}
function showFindMatch(scroll:boolean) {
 highlight("memql-find",findRanges,2);const active=findRanges[findIndex];highlight("memql-find-active",active?[active]:[],3);
 byId("find-count").textContent=!findQuery.value?"":findRanges.length?`${findIndex+1} of ${findRanges.length}${findRanges.length===5000?"+":""}`:"No matches";
 (byId("find-previous") as HTMLButtonElement).disabled=(byId("find-next") as HTMLButtonElement).disabled=!findRanges.length;
 if(scroll&&active){
  mapped(active.startContainer)?.scrollIntoView?.({block:"center"});
  const rect=active.getBoundingClientRect?.();
  if(rect){const top=byId("document-find").getBoundingClientRect().bottom+24;const bottom=document.body.classList.contains("review-open")&&window.innerWidth<1000?window.innerHeight*.5:window.innerHeight-24;
   if(rect.top<top||rect.bottom>bottom)window.scrollBy?.({top:rect.top-(top+bottom)/2,behavior:"instant"});}
 }
 positionFindMarker();
}
function positionFindMarker(){
 const marker=byId("find-marker"),active=findRanges[findIndex];marker.hidden=byId("document-find").hidden||!active;
 if(marker.hidden)return;marker.textContent=String(findIndex+1);
 const rect=active.getClientRects?.()[0],main=content.closest("main")!.getBoundingClientRect();if(!rect)return;
 marker.style.top=`${rect.top-main.top+(rect.height-18)/2}px`;
 marker.style.left=`${Math.max(2,content.getBoundingClientRect().left-main.left-28)}px`;
}
function nextFind(direction:number){if(findRanges.length){findIndex=(findIndex+direction+findRanges.length)%findRanges.length;showFindMatch(true);}}
byId("find").addEventListener("click",()=>showFind(true));
byId("find-close").addEventListener("click",()=>showFind(false));
byId("find-next").addEventListener("click",()=>nextFind(1));
byId("find-previous").addEventListener("click",()=>nextFind(-1));
findQuery.addEventListener("input",()=>updateFind());
findQuery.addEventListener("keydown",event=>{if(event.key==="Enter"){event.preventDefault();event.stopPropagation();nextFind(event.shiftKey?-1:1);}if(event.key==="Escape"){event.preventDefault();event.stopPropagation();showFind(false);}});

byId("history-toggle").addEventListener("click",()=>{showHistory(!historyOpen);if(historyOpen)historyRequest({type:"history"});});
byId("history-close").addEventListener("click",()=>{showHistory(false);byId("history-toggle").focus();});
byId("history-more").addEventListener("click",()=>historyRequest({type:"history",beforeVersion:historyData.beforeVersion}));
byId("history-retry").addEventListener("click",()=>historyRequest(historyRetry));
byId("history-current").addEventListener("click",()=>api.postMessage({type:"historyCurrent"}));
byId("history-fork").addEventListener("click",()=>historyRequest({type:"historyFork",name:branchName.value.trim()}));
branchName.addEventListener("input",historyControls);

let dictationPhase="idle", dictatedBase="", dictationTarget="feedback", dictationError="", dictationReference="";
let dictationAvailable=false;
let reviewRequested = state.reviewOpen ?? false;
let activeAnchor: Anchor | undefined;
const decisions = state.decisions ?? {};
const modifications = state.modifications ?? {};
const expanded = state.expanded ?? {};
let modifying = "";
let reviewSource = state.reviewSource ?? "";
let selection: Anchor | undefined;
let draftAnchor = state.anchor;
let selectionRect: DOMRect | undefined;
let selectionRange: Range | undefined;
let contextSelection: { anchor: Anchor; rect?: DOMRect } | undefined;
let version = 0, sourceIdentity = "";
let connected = false, commentBusy = false, revisionBusy = false, preparing = false;
let revision: Record<string, any> | undefined;
let emptyDocument = false, documentLoadFailed = false;
let pendingAnchors: Anchor[] | undefined;
let progressKey = "";
let progressWasWhole = false;
let draftKey="";
function revisionLocked() { return preparing || revisionBusy || !!activeRun(); }
function documentProcessing() {
  return preparing || !!pendingAnchors && revisionBusy || !!activeRun() && (revision?.status !== "waiting" || ["retry","replan","repair"].includes(revision?.waitingOn?.kind));
}
function reveal(element: HTMLElement) {
  if (!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) element.animate?.([{opacity:.35},{opacity:1}],{duration:180,easing:"ease-out"});
}
function renderDraftProgress() {
  const parts: {before:string;after:string;html:string;complete:boolean}[] = revision?.draftParts ?? [];
  const show=mode==="review" && !historyPreview && parts.length>0 && !revision?.result?.applied;
  const whole=emptyDocument || (revision?.proposal?.comments ?? []).some((row:ReviewRow)=>row.anchor?.kind==="document");
  byId("draft-preview").hidden=!show;
  document.body.classList.toggle("draft-whole",show&&whole);
  document.body.classList.toggle("has-draft",show);
  if(!show)return;
  const running=documentProcessing();
  const ready=revision?.status==="waiting" && revision?.approvalId && !revision?.decision && !!revision?.items?.length;
  byId("draft-caption").textContent=running?"Draft in progress · More content is being prepared" : ready?"Proposed draft · Review changes before applying" : "Draft paused · Incomplete content is saved below. Open the review for next steps.";
  const key=JSON.stringify(parts);
  if(key!==draftKey){
    const container=byId("draft-parts");
    for(let index=0;index<parts.length;index++){
      let part=container.children[index] as HTMLElement|undefined;
      if(!part){part=document.createElement("section");container.append(part);reveal(part);}
      if(part.dataset.source!==parts[index].after){part.innerHTML=parts[index].html;part.dataset.source=parts[index].after;}
    }
    while(container.children.length>parts.length)container.lastElementChild?.remove();
    draftKey=key;
  }
}
function renderDocumentProgress(force = false) {
  renderDraftProgress();
  const loading = !latestDocument && !documentLoadFailed;
  const processing = !historyPreview && mode === "review" && documentProcessing();
  const requested: ReviewRow[] = Array.isArray(revision?.proposal?.comments) ? revision!.proposal.comments : rows.filter(row => revision?.proposal?.commentIds?.includes(row.id));
  const anchors: Anchor[] = processing ? pendingAnchors ?? requested.filter(row => !row.outdated).map(row => row.anchor) : [];
  const whole = loading || processing && (emptyDocument || anchors.some(anchor => anchor.kind === "document"));
  const atEnd = processing && !whole && anchors.some(anchor => anchor.kind === "document-end");
  const ranges = whole ? [] : anchors.map(rangeFor).filter((range): range is Range => !!range);
  const key = JSON.stringify([loading,processing,whole,atEnd,anchors,version,historyPreview,mode,draftKey]);
  if (!force && key === progressKey) return;
  progressKey = key;
  const wrap = byId("content-wrap"), full = byId("document-progress"), passages = byId("passage-progress");
  const wasBusy = content.getAttribute("aria-busy") === "true";
  const wasWhole = progressWasWhole; progressWasWhole = whole;
  wrap.classList.toggle("loading-document",whole);
  content.setAttribute("aria-busy",String(loading || processing));
  byId("progress-status").textContent = loading ? "Loading document" : processing ? whole ? "Preparing document" : "Preparing selected changes" : "";
  full.hidden = !whole; full.replaceChildren();
  const retiring = wasBusy && !loading && !processing && !wasWhole && !window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? [...passages.children] : [];
  passages.replaceChildren();
  for (const old of retiring) { const node=old as HTMLElement;passages.append(node);const animation=node.animate?.([{opacity:1},{opacity:0}],{duration:180,easing:"ease-out"});if(animation)animation.onfinish=()=>node.remove();else node.remove(); }
  if (whole) {
    const count = Math.min(400, Math.max(10, Math.ceil(wrap.getBoundingClientRect().height / 28)));
    for (let i=0; i<count; i++) { const line=textElement("div","","skeleton-line");line.style.width=`${[60,96,89,72,94,83,55][i%7]}%`;full.append(line); }
  } else {
    const origin=wrap.getBoundingClientRect();
    const rectangles=ranges.flatMap(range => Array.from(range.getClientRects?.() ?? []));
    if(atEnd){const end=content.getBoundingClientRect();for(let i=0;i<3;i++)rectangles.push({left:end.left,top:end.bottom+16+i*28,width:end.width*(i===2?.6:.95),height:14} as DOMRect);}
    const distinct=new Set<string>();
    for(const rect of rectangles){
      if(rect.width<=0||rect.height<=0)continue;
      const bounds=[rect.left-origin.left,rect.top-origin.top,rect.width,rect.height];const id=bounds.join(":");if(distinct.has(id))continue;distinct.add(id);
      const line=textElement("div","","skeleton-line skeleton-passage");
      line.style.left=`${bounds[0]}px`;line.style.top=`${bounds[1]}px`;line.style.width=`${bounds[2]}px`;line.style.height=`${bounds[3]}px`;passages.append(line);
    }
  }
  if(wasBusy && !loading && !processing && wasWhole && !document.body.classList.contains("draft-whole")) reveal(content);
}
// Geometry follows wrapping, review/search panels, images and window resizing.
if(typeof ResizeObserver!=="undefined")new ResizeObserver(()=>renderDocumentProgress(true)).observe(byId("content-wrap"));
let rows: ReviewRow[] = [];
const selected = new Set<string>(state.included ?? []);
const seen = new Set<string>(state.seen ?? []);
feedback.value = state.draft ?? ""; instruction.value = state.instruction ?? "";
function saveState() { const saved = { draft: feedback.value, attachments, purpose:composerPurpose, anchor: draftAnchor, source: sourceIdentity || state.source, instruction: instruction.value, reviewOpen: reviewRequested, included: [...selected], seen: [...seen], decisions, modifications, expanded, reviewSource }; api.setState(saved); if (restored) api.postMessage({type:"draftState",state:saved}); }
function activeRun() { return revision && !revision.cancelRequested && !["succeeded", "failed", "cancelled"].includes(revision.status); }
function renderEmptyDocument() {
  const empty = !!latestDocument && emptyDocument && !historyPreview;
  document.body.classList.toggle("document-empty-view", empty);
  byId("document-empty").hidden = !empty;
  // The two document-boundary controls only make sense around real content.
  byId("document-feedback").hidden = byId("extend").hidden = !latestDocument || emptyDocument;
  if (!empty) return;
  const reviewing = mode === "review", running = preparing || !!activeRun();
  const awaiting = running && revision?.status === "waiting" && !!revision.approvalId && !revision.decision;
  const paused = running && revision?.status === "waiting" && !awaiting;
  const hasRequest = rows.some(row => !row.outdated) || running;
  byId("empty-title").textContent = !reviewing ? "This document is empty" : running ? awaiting ? "Your draft is ready" : paused ? "Draft preparation paused" : "Creating your draft" : "Create your document";
  byId("empty-description").textContent = !reviewing ? "Switch to Review to describe a draft, or write directly in Source." : running ? awaiting ? "Review the proposed content before adding it to your document." : paused ? "Open the review to see what needs attention. Your request is saved." : "Your request is being prepared. You can review the draft before it’s added." : hasRequest ? "Your description is saved. Open the review to prepare your draft." : "Describe the topic, audience, and details you want. You’ll review the draft before it’s added.";
  const create = byId("empty-create") as HTMLButtonElement;
  create.textContent = !reviewing ? "Start in Review" : hasRequest ? "Open Review" : "Describe document";
  create.classList.toggle("primary", !reviewing || !hasRequest);
  create.classList.toggle("passage", reviewing && hasRequest);
  create.hidden = !connected;
  create.disabled = dictationPhase !== "idle" || commentBusy;
  byId("empty-source").hidden = running;
  byId("empty-title").classList.toggle("busy", reviewing && running && !awaiting && !paused);
  if (reviewing && !connected) byId("empty-description").textContent = latestDocument.status ?? "Save the document in MemQL to create a draft with AI, or write directly in Source.";
}
function dictate(target: string) {
  if(dictationPhase==="idle") {
    if(!dictationAvailable||!connected)return;
    if(target==="feedback"&&dictationError)byId("composer-status").textContent="";
    dictationTarget=target;dictatedBase=target==="feedback"?feedback.value:modifications[target]??"";dictationError="";dictationPhase="starting";
    controls();renderRevision();api.postMessage({type:"dictationStart"});
  } else if(dictationTarget===target)api.postMessage({type:dictationPhase==="listening"?"dictationStop":"dictationCancel"});
}
function dictationControl(button: HTMLButtonElement, target: string) {
  const active=dictationPhase!=="idle"&&dictationTarget===target;
  button.hidden=!dictationAvailable;button.disabled=!connected||!active&&(revisionBusy||target==="feedback"&&composerPurpose==="feedback"&&revisionLocked())||dictationPhase!=="idle"&&!active;
  button.textContent=active?dictationPhase==="listening"?"Stop":"Cancel":"Dictate";
  button.setAttribute("aria-label",active?dictationPhase==="listening"?"Stop dictation":"Cancel dictation":target==="feedback"?"Dictate feedback":"Dictate direction for this change");
}
function dictationStatus(target: string) {
  return dictationTarget!==target?"":dictationError|| (dictationPhase==="starting"?"Opening microphone…":dictationPhase==="listening"?"Listening…":dictationPhase==="transcribing"?"Transcribing…":"");
}
function controls() {
  renderEmptyDocument();
  renderDocumentProgress();
  const locked = revisionLocked();
  (byId("source") as HTMLButtonElement).disabled = historyPreview || locked;
  instruction.disabled = locked;
  dictationControl(byId("dictate") as HTMLButtonElement,"feedback");
  byId("dictation-status").textContent=dictationTarget==="feedback"&&dictationError?"":dictationStatus("feedback");
  annotate.disabled = locked || dictationPhase!=="idle"&&dictationTarget!=="feedback" || !connected || (!selection && !(feedback.value && draftAnchor));
  annotate.title = selection ? "Add feedback on this selection" : feedback.value && draftAnchor ? "Continue your feedback" : "Select text to add feedback";
  for (const button of Array.from(document.querySelectorAll<HTMLButtonElement>("#extend,#document-feedback,.section-feedback"))) button.disabled = locked||!connected||dictationPhase!=="idle";
  feedback.readOnly=dictationPhase!=="idle" || composerPurpose==="feedback" && locked;
  (byId("attach") as HTMLButtonElement).disabled=locked||!connected||commentBusy||!!uploading.size||readingAttachments||attachments.length>=8;
  for(const button of Array.from(byId("attachments").querySelectorAll<HTMLButtonElement>("button")))button.disabled=locked||commentBusy;
  add.disabled = !!uploading.size || readingAttachments || composerPurpose==="feedback" && locked || dictationPhase!=="idle" || !connected || !draftAnchor || !feedback.value.trim() || commentBusy;
  add.textContent = commentBusy ? "Saving…" : composerPurpose==="note" ? "Save note" : "Add to review";
  (byId("selection-note") as HTMLButtonElement).disabled=!connected;
  (byId("selection-feedback") as HTMLButtonElement).disabled=!connected||locked;
  byId("selection-feedback").hidden=mode!=="review";
  prepare.disabled = dictationPhase!=="idle" || !connected || revisionBusy || !!activeRun() || selected.size === 0;
  prepare.textContent = revisionBusy ? "Submitting…" : "Propose changes";
  byId("review-submit").hidden = !!activeRun() || selected.size === 0;
  byId("review-footer").hidden = byId("review-submit").hidden && !byId("review-actions").childElementCount;
  const count = rows.filter(row => !row.outdated).length;
  byId("note-count").textContent = String(count); byId("note-count").hidden = !count;
}
function showReview(open: boolean) { if(open&&notesOpen)return; if(!open&&dictationPhase!=="idle"&&dictationTarget!=="feedback")api.postMessage({type:"dictationCancel"});reviewRequested = open; open = open && mode === "review" && !historyOpen && !historyPreview; document.body.classList.toggle("review-open", open); byId("review-panel").hidden = !open; byId("review-toggle").setAttribute("aria-expanded", String(open)); if (open) byId("selection-tools").hidden = true; saveState(); renderNoteMarkers(); }
function position(element: HTMLElement, rect?: DOMRect, compact = false) {
  const width = compact ? 294 : Math.min(360, window.innerWidth - 32);
  const height = compact ? 42 : Math.min(340, window.innerHeight - 32);
  element.style.left = `${Math.max(16, Math.min(window.innerWidth - width - 16, rect ? rect.left + rect.width / 2 - width / 2 : (window.innerWidth - width) / 2))}px`;
  const below = rect ? rect.bottom + 10 : Math.max(90, window.innerHeight / 3);
  element.style.top = `${Math.max(16, Math.min(window.innerHeight - height - 16, below + height <= window.innerHeight ? below : (rect?.top ?? below) - height - 10))}px`;
  element.style.maxHeight = `${Math.max(180,window.innerHeight - 32)}px`; element.style.overflowY = "auto";
}
function highlight(name: string, ranges: Range[], priority=0) {
  const css = (window as any).CSS, Constructor = (window as any).Highlight;
  if (css?.highlights && Constructor) { if (ranges.length) {const layer=new Constructor(...ranges);layer.priority=priority;css.highlights.set(name,layer);} else css.highlights.delete(name); }
}
function openComposer(anchor: Anchor, rect?: DOMRect, purpose:"feedback"|"note"="feedback") {
  if (purpose==="feedback" && (mode !== "review" || revisionLocked()) || historyPreview || !connected) return;
  // A second selection must never silently move an unfinished note.
  if ((feedback.value.trim() || attachments.length || uploading.size || readingAttachments) && draftAnchor && (JSON.stringify(draftAnchor) !== JSON.stringify(anchor) || purpose!==composerPurpose)) {
    byId("composer-status").textContent = "Your unfinished note is still attached to its original selection. Save it before starting another.";
  } else { draftAnchor = anchor; composerPurpose=purpose; byId("composer-status").textContent = ""; }
  document.body.classList.toggle("note-composing",composerPurpose==="note");
  const extend = isExtension(draftAnchor);
  const creating = emptyDocument && composerPurpose === "feedback" && draftAnchor?.kind === "document";
  byId("composer-title").textContent = creating ? "Create your document" : composerPurpose==="note" ? "Add note" : draftAnchor?.kind === "document" ? "Revise entire document" : extend ? draftAnchor?.kind === "document-end" ? "Extend document" : draftAnchor?.scope === "section" ? "Extend section" : "Extend passage" : "Add feedback";
  feedback.setAttribute("aria-label", creating ? "Document description" : composerPurpose==="note" ? "Personal note" : extend ? "Extension request" : "Feedback");
  feedback.placeholder = creating ? "What would you like to create? Include the topic, audience, and any details that matter…" : composerPurpose==="note" ? "Write a note for yourself…" : draftAnchor?.kind === "document" ? "Describe a correction to apply throughout, or how you’d like the document rewritten…" : extend ? "What would you like to add here?" : "Describe what you’d like to change, add, move, or research…";
  byId("selected").textContent = draftAnchor?.quote ?? "";
  byId("selected").hidden = draftAnchor?.kind === "document-end" || draftAnchor?.kind === "document";
  byId("composer").hidden = false; byId("selection-tools").hidden = true;
  highlight("memql-active", selectionRange ? [selectionRange] : []);
  position(byId("composer"), rect); renderAttachments(); controls(); saveState(); feedback.focus();
}
function closeComposer() {
  if(dictationPhase!=="idle")api.postMessage({type:"dictationCancel"}); byId("composer").hidden = true; highlight("memql-active", []); saveState(); content.focus({ preventScroll: true }); }
function mapped(node: Node | null): HTMLElement | null {
  return (node?.nodeType === Node.ELEMENT_NODE ? node as Element : node?.parentElement)?.closest<HTMLElement>("[data-start-line][data-end-line]") ?? null;
}
function captureSelection() {
  if (historyPreview) { selection=undefined; byId("selection-tools").hidden=true; return; }
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
  byId("selection-tools").hidden = false; position(byId("selection-tools"),selectionRect,true); controls();
}
document.addEventListener("selectionchange",captureSelection);
for(const id of ["selection-copy","selection-feedback","selection-note"])byId(id).addEventListener("mousedown", event => event.preventDefault());
byId("selection-copy").addEventListener("click",()=>api.postMessage({type:"copy"}));
byId("selection-note").addEventListener("click",()=>{if(selection)openComposer(selection,selectionRect,"note");});
annotate.addEventListener("mousedown",event => event.preventDefault());
for (const id of ["selection-feedback","annotate"]) byId(id).addEventListener("click",() => { const anchor = selection ?? draftAnchor; if (anchor && connected) openComposer(anchor,selectionRect); });
byId("document-feedback").addEventListener("click",()=>{selectionRange=undefined;openComposer({kind:"document",quote:"Entire document"},byId("document-feedback").getBoundingClientRect());});
byId("empty-create").addEventListener("click", () => {
  if (!connected || historyPreview || !emptyDocument) return;
  if (mode !== "review") { api.postMessage({type:"review"}); return; }
  if (preparing || activeRun() || rows.some(row => !row.outdated)) { showNotes(false); showHistory(false); showReview(true); byId("review-close").focus(); return; }
  selectionRange = undefined;
  openComposer({kind:"document",quote:"Entire document"}, byId("empty-create").getBoundingClientRect());
});
byId("draft-open-review").addEventListener("click",()=>{showNotes(false);showHistory(false);showReview(true);});
byId("empty-source").addEventListener("click", () => api.postMessage({type:"source"}));
byId("extend").addEventListener("click",() => openComposer({kind:"document-end",quote:"End of document"},byId("extend").getBoundingClientRect()));
byId("dictate").addEventListener("click",()=>dictate("feedback"));
byId("composer-close").addEventListener("click",closeComposer);
byId("review-toggle").addEventListener("click",() => {const open=byId("review-panel").hidden;showNotes(false);showHistory(false);showReview(open)});
byId("review-close").addEventListener("click",() => { showReview(false); byId("review-toggle").focus(); });
feedback.addEventListener("input",() => { controls(); saveState(); });
instruction.addEventListener("input",saveState);
add.addEventListener("click",() => {
  if (add.disabled || !draftAnchor) return;
  commentBusy = true; controls(); byId("composer-status").textContent = "";
  api.postMessage({type:composerPurpose==="note"?"note":"comment",version,selection:draftAnchor,body:feedback.value,attachments:composerPurpose==="feedback"?attachments:[]});
});
prepare.addEventListener("click",() => {
  if (prepare.disabled) return;
  pendingAnchors = rows.filter(row => selected.has(row.id) && !row.outdated).map(row => row.anchor);
  byId("status").textContent = ""; revisionBusy = true; preparing = true; controls(); renderRevision();
  api.postMessage({type:"prepareRevision",version,commentIds:[...selected],instruction:instruction.value});
});
document.addEventListener("keydown",event => {
  if((event.metaKey||event.ctrlKey)&&event.key.toLowerCase()==="f"){event.preventDefault();event.stopPropagation();showFind(true);return;}
  if (event.key === "Escape") { if(notesOpen){showNotes(false);byId("notes-toggle").focus();return;} if(historyOpen){showHistory(false);byId("history-toggle").focus();return;} if (!byId("composer").hidden) closeComposer(); else if (!byId("review-panel").hidden) { showReview(false); byId("review-toggle").focus(); } else byId("selection-tools").hidden = true; }
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter" && !byId("composer").hidden) { event.preventDefault(); add.click(); }
  if ((event.ctrlKey || event.metaKey) && event.altKey && event.key.toLowerCase() === "m" && selection && connected) { event.preventDefault(); openComposer(selection,selectionRect); }
});
window.addEventListener("scroll",() => { byId("selection-tools").hidden = true; positionFindMarker(); },{passive:true});
window.addEventListener("resize",() => { byId("selection-tools").hidden = true; if (!byId("composer").hidden) position(byId("composer"));renderNoteMarkers(); });
for (const mode of ["source","reading","review"]) byId(mode).addEventListener("click",() => api.postMessage({type:mode}));
const views = Array.from(document.querySelectorAll<HTMLButtonElement>(".views button"));
views.forEach((button,index) => button.addEventListener("keydown",event => {
  const next = event.key === "ArrowRight" ? (index+1)%views.length : event.key === "ArrowLeft" ? (index+views.length-1)%views.length : event.key === "Home" ? 0 : event.key === "End" ? views.length-1 : undefined;
  if (next !== undefined) { event.preventDefault(); views[next].focus(); }
}));
content.addEventListener("click", event => {
  const link = (event.target as Element).closest<HTMLAnchorElement>("a");
  if (!link) return;
  event.preventDefault();
  if (link.dataset.external) { api.postMessage({type:"external",href:link.dataset.external}); return; }
  const href = link.getAttribute("href");
  if (!href?.startsWith("#")) return;
  let anchor: string;
  try { anchor = decodeURIComponent(href.slice(1)); } catch { return; }
  // Search inside the rendered document, never in the editor's controls.
  const target = Array.from(content.querySelectorAll<HTMLElement>("[id]")).find(el => el.dataset.headingAnchor === anchor || el.id === anchor);
  if (!target) return;
  for (const previous of content.querySelectorAll(".anchor-target")) previous.classList.remove("anchor-target");
  target.classList.add("anchor-target"); target.tabIndex = -1;
  target.focus({preventScroll:true}); target.scrollIntoView?.({block:"start",behavior:"instant"});
});
function textElement(tag: string, text: string, className = "") { const element = document.createElement(tag); element.textContent = text; element.className = className; return element; }
function rangeFor(anchor: Anchor): Range | undefined {
  if (anchor.kind === "document-end" || anchor.kind === "document") return;
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
  if (anchor?.kind === "document") return "Entire document";
  if (anchor?.kind === "document-end") return "Document end";
  if (anchor?.scope === "section") return anchor.quote;
  return anchor?.sectionPath?.at(-1) || "Selected passage";
}
function jumpTo(anchor: Anchor, scroll = true) {
  activeAnchor = anchor;
  for (const element of document.querySelectorAll(".document-target")) element.classList.remove("document-target");
  const range = rangeFor(anchor);
  const target = (anchor.kind === "document" || anchor.kind === "document-end") && emptyDocument ? byId("document-empty") : anchor.kind === "document" ? byId("document-feedback") : anchor.kind === "document-end" ? byId("extend") : mapped(range?.startContainer ?? null)
    ?? content.querySelector<HTMLElement>(`[data-start-line="${anchor.startLine}"]`);
  if(scroll)target?.scrollIntoView?.({ block: window.innerWidth < 1000 ? "start" : "center", behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth" });
  target?.classList.add("document-target");
  if (range) highlight("memql-active", [range]);
  // Navigation keeps the review, its scroll position and the invoking control.
}
function locationButton(anchor: Anchor, label = location(anchor)) {
  const jump = textElement("button", label, "passage") as HTMLButtonElement;
  jump.setAttribute("aria-label", `Show ${location(anchor)} in document`);
  jump.addEventListener("click", () => jumpTo(anchor));
  return jump;
}
function showNotes(open:boolean) {
  notesOpen=open&&!historyPreview;document.body.classList.toggle("notes-open",notesOpen);byId("notes-panel").hidden=!notesOpen;byId("notes-toggle").setAttribute("aria-expanded",String(notesOpen));
  if(notesOpen){historyOpen=false;document.body.classList.remove("history-open","review-open");byId("history-panel").hidden=true;byId("history-toggle").setAttribute("aria-expanded","false");byId("review-panel").hidden=true;byId("review-toggle").setAttribute("aria-expanded","false");byId("selection-tools").hidden=true;}
  renderNoteMarkers();
}
function currentNoteAnchor(row:ReviewRow):Anchor|undefined {
  if(historyPreview||row.anchor.kind!=="markdown")return;
  const anchor=row.anchor;
  if(!row.outdated&&rangeFor(anchor)?.toString().trim()===anchor.quote.trim())return anchor;
  if(anchor.sourceQuote){
    const source=sourceIdentity.replace(/\r\n/g,"\n"),offset=source.indexOf(anchor.sourceQuote);
    if(offset>=0&&source.indexOf(anchor.sourceQuote,offset+1)<0){
      const start=source.slice(0,offset).split("\n").length-1,end=start+anchor.sourceQuote.split("\n").length;
      const from=Array.from(content.querySelectorAll<HTMLElement>(`[data-start-line="${start}"]`)).at(-1),to=Array.from(content.querySelectorAll<HTMLElement>(`[data-end-line="${end}"]`)).at(-1);
      if(from&&to){const relocated={...anchor,startLine:start,endLine:end,startBlock:Number(from.dataset.blockId),endBlock:Number(to.dataset.blockId)};if(rangeFor(relocated)?.toString().trim()===anchor.quote.trim())return relocated;}
    }
  }
  // Preserve a note when its exact selected text still has one rendered match,
  // even if a name elsewhere in the paragraph changes. Repeated text is not
  // enough evidence to move an annotation to a different passage.
  const walker=document.createTreeWalker(content,NodeFilter.SHOW_TEXT);
  const nodes:{node:Node;start:number;end:number}[]=[];let text="",node:Node|null;
  while((node=walker.nextNode())){if(node.parentElement?.closest("button"))continue;const value=node.textContent??"";nodes.push({node,start:text.length,end:text.length+value.length});text+=value;}
  const offset=text.indexOf(anchor.quote);if(!anchor.quote||offset<0||text.indexOf(anchor.quote,offset+1)>=0)return;
  const start=nodes.find(part=>part.start<=offset&&part.end>offset),end=nodes.find(part=>part.start<offset+anchor.quote.length&&part.end>=offset+anchor.quote.length);
  if(!start||!end)return;
  const from=mapped(start.node),to=mapped(end.node);if(!from||!to)return;
  const prefix=document.createRange();prefix.selectNodeContents(from);prefix.setEnd(start.node,offset-start.start);
  const suffix=document.createRange();suffix.selectNodeContents(to);suffix.setEnd(end.node,offset+anchor.quote.length-end.start);
  return {...anchor,startLine:Number(from.dataset.startLine),endLine:Number(to.dataset.endLine),startBlock:Number(from.dataset.blockId),endBlock:Number(to.dataset.blockId),startTextOffset:prefix.toString().length,endTextOffset:suffix.toString().length};
}
function openNote(id:string) {
  activeNote=id;showNotes(true);renderNotes();Array.from(byId("personal-notes").querySelectorAll<HTMLElement>("[data-note-id]")).find(element=>element.dataset.noteId===id)?.scrollIntoView?.({block:"nearest"});
}
function renderNotes() {
  const list=byId("personal-notes");list.replaceChildren();
  byId("personal-note-count").textContent=String(personalNotes.length);byId("personal-note-count").hidden=!personalNotes.length;
  if(!personalNotes.length)list.append(textElement("p","Select a passage and choose Add note.","empty"));
  for(const row of personalNotes){
    const card=textElement("article","","personal-note"+(row.id===activeNote?" active":""));card.dataset.noteId=row.id;
    const anchor=currentNoteAnchor(row);
    if(anchor){const button=textElement("button",location(anchor),"passage");button.setAttribute("aria-label","Show note passage in document");button.addEventListener("click",()=>jumpTo(anchor));card.append(button);}
    else card.append(textElement("small","Passage from an earlier version"));
    card.append(textElement("blockquote",row.anchor.quote),textElement("p",row.body));list.append(card);
  }
  renderNoteMarkers();
}
function renderNoteMarkers() {
  const markers=byId("note-markers");markers.replaceChildren();const ranges:Range[]=[];
  if(historyPreview){highlight("memql-notes",[]);return;}
  const groups=new Map<HTMLElement,ReviewRow[]>();
  for(const row of personalNotes){const anchor=currentNoteAnchor(row);if(!anchor)continue;const range=rangeFor(anchor);if(range)ranges.push(range);const block=mapped(range?.startContainer??null);if(block)groups.set(block,[...(groups.get(block)??[]),row]);}
  const top=markers.getBoundingClientRect().top;
  for(const [block,notes] of groups){const button=textElement("button",String(notes.length),"note-marker") as HTMLButtonElement;button.setAttribute("aria-label",`Open ${notes.length===1?"note":notes.length+" notes"} on: ${notes[0].anchor.quote}`);button.title=notes.map(row=>row.body).join("\n\n");button.style.top=`${Math.max(0,block.getBoundingClientRect().top-top)}px`;button.addEventListener("click",()=>openNote(notes[0].id));markers.append(button);}
  highlight("memql-notes",ranges);positionFindMarker();
}
byId("notes-toggle").addEventListener("click",()=>{showNotes(!notesOpen);if(notesOpen)api.postMessage({type:"refreshNotes"});});
byId("notes-close").addEventListener("click",()=>{showNotes(false);byId("notes-toggle").focus();});
byId("notes-retry").addEventListener("click",()=>api.postMessage({type:"refreshNotes"}));
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
    button.className = "section-feedback icon-button";
    button.setAttribute("aria-label", `Feedback on section: ${quote}`);
    button.title = "Give feedback on this section";
    button.innerHTML = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M21 11.5a8.4 8.4 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.4 8.4 0 0 1-3.8-.9L3 21l1.9-5.7a8.4 8.4 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.4 8.4 0 0 1 3.8-.9h.5a8.5 8.5 0 0 1 8 8v.5Z"/></svg>';
    button.addEventListener("click", () => {
      if (!connected) return;
      const anchor: Anchor = { kind:"markdown", scope:"section", quote,
        startLine:Number(heading.dataset.startLine), endLine:Number(heading.dataset.endLine),
        startBlock:Number(heading.dataset.blockId), endBlock:Number(heading.dataset.blockId), startTextOffset:0, endTextOffset:quote.length };
      selectionRange = undefined;
      openComposer(anchor, button.getBoundingClientRect());
    });
    heading.append(button);
  }
}
function requestNote(row: ReviewRow, editable: boolean): HTMLElement {
  const article = textElement("article", "", "note"); article.dataset.reviewKey = `request:${row.id}`;
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
  for(const ref of row.attachments??[])article.append(attachmentRow(ref));
  if (row.anchor?.kind !== "document-end" && row.anchor?.kind !== "document" && row.anchor?.scope !== "section") {
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
  if (!rows.length && !revision) list.append(textElement("p", emptyDocument ? "Describe your document to begin." : "Select text or use the feedback button beside a heading. Describe the change or addition you want.", "empty"));
  if (earlier.length) {
    const details = textElement("details", "", "earlier"); details.append(textElement("summary", `Earlier requests (${earlier.length})`));
    for (const row of earlier) details.append(requestNote(row, false)); list.append(details);
  }
  highlight("memql-notes", (mode === "review" ? rows : []).filter(row => !row.outdated).map(row => rangeFor(row.anchor)).filter((range): range is Range => !!range));
}
function renderRevision() {
  renderEmptyDocument();
  renderDocumentProgress();
  const keyed = () => [...document.querySelectorAll<HTMLDetailsElement>("#review-panel details")].map((element,index) => ({element,key:element.dataset.reviewKey || `${element.closest<HTMLElement>("[data-review-key]")?.dataset.reviewKey ?? "panel"}:${element.className}:${element.querySelector(":scope > summary")?.textContent ?? index}`}));
  for (const {element,key} of keyed()) expanded[key]=element.open;
  const active=document.activeElement as HTMLInputElement | HTMLTextAreaElement | null;
  const focusKey=active?.dataset.focusKey, selectionStart=active?.selectionStart, selectionEnd=active?.selectionEnd;
  const scroll=document.querySelector(".review-scroll")!.scrollTop;
  renderRevisionContent();
  for (const {element,key} of keyed()) { if (key in expanded) element.open=expanded[key]; element.addEventListener("toggle",()=>{expanded[key]=element.open;saveState();}); }
  if(focusKey) { const target=[...document.querySelectorAll<HTMLElement>("[data-focus-key]")].find(el=>el.dataset.focusKey===focusKey);target?.focus({preventScroll:true});if(target instanceof HTMLTextAreaElement && selectionStart!=null && selectionEnd!=null)target.setSelectionRange(selectionStart,selectionEnd); }
  document.querySelector(".review-scroll")!.scrollTop=scroll;
}
function renderRevisionContent() {
  renderComments();
  const root = byId("revision"); root.replaceChildren(); const actions = byId("review-actions"); actions.replaceChildren();
  const pending = rows.some(row => !row.outdated && !revision?.proposal?.commentIds?.includes(row.id));
  document.body.classList.toggle("review-draft", !preparing && !activeRun() && (pending || !revision));
  if (preparing) { root.append(textElement("p", "Preparing changes…", "phase busy")); return; }
  if (!revision) return;
  const status = revision, proposal = status.proposal ?? {};
  const terminal = !!status.cancelRequested || ["succeeded", "failed", "cancelled"].includes(status.status);
  const awaiting = !terminal && !!status.approvalId && !status.decision && status.status === "waiting";
  const retrying = !terminal && !awaiting && (["retry","replan","repair"].includes(status.waitingOn?.kind) || status.status === "running" && Number(status.retryCount)>0);
  const paused = !retrying && !terminal && !awaiting && !status.decision && status.prepared !== false && status.status === "waiting";
  let panel: HTMLElement = root;
  if (terminal && pending) {
    const previous = document.createElement("details"); previous.className = "earlier";
    previous.append(textElement("summary", "Previous review")); root.append(previous); panel = previous;
  }
  const phase = status.cancelRequested ? "Stop requested" : status.decision === "rejected" ? "Changes declined" : status.status === "succeeded" ? status.result?.applied ? "Changes applied" : "No changes needed"
    : status.status === "failed" || status.status === "cancelled" ? "Couldn’t prepare changes" : awaiting ? "Proposed changes" : retrying ? "Retrying automatically…" : paused ? "Preparation paused" : status.decision === "approved" ? "Applying changes…" : "Preparing changes…";
  panel.append(textElement("h3", phase, !terminal && !awaiting && !paused ? "phase busy" : ""));
  if (status.problem && (retrying || paused || ["failed","cancelled"].includes(status.status))) {
    const notice=textElement("div","","review-error");
    renderProblem(notice, {...status.problem, message: retrying ? "The last attempt couldn’t finish. MemQL will retry automatically. Your feedback is saved." : paused ? `${status.problem.message} Your feedback is saved. Stop this attempt, then propose changes again.` : status.problem.message}, value=>api.postMessage(value));
    panel.append(notice);
  }
  const requested: ReviewRow[] = Array.isArray(proposal.comments) ? proposal.comments : rows.filter(row => proposal.commentIds?.includes(row.id));
  const edits: Record<string, any>[] = Array.isArray(proposal.edits) ? proposal.edits.filter((edit: any) => edit.before !== edit.after) : [];
  const addressed = new Set(edits.flatMap(edit => edit.commentIds ?? []));
  const items: Record<string,any>[] = Array.isArray(status.items) ? status.items : [];
  for (const item of items) {
    const related = requested.filter(row => item.commentIds?.includes(row.id));
    const destination = editLocation(item.edits[0]);
    const card = document.createElement("details"); card.className = "change"; card.open = !terminal; card.dataset.reviewKey=item.id;
    const title = textElement("summary", related.length === 1 ? `${isExtension(related[0].anchor) ? "Extend" : "Feedback"}: ${location(destination ?? related[0].anchor)}` : item.edits.length>1 ? "Linked changes" : "Document change");
    title.append(textElement("span", decisions[item.id] === "accepted" ? "Accepted" : decisions[item.id] === "declined" ? "Declined" : "", "item-decision"));card.append(title);
    for (const row of related) { const context=textElement("div","","request-context");context.append(textElement("p",row.body));card.append(context); }
    if (item.edits.length>1) card.append(textElement("p", "These edits belong together and share one decision.", "request-context muted"));
    for (const edit of item.edits) {
      const target = editLocation(edit) ?? related[0]?.anchor;
      if (!terminal && target) {const context=textElement("div","","request-context");context.append(locationButton(target,"Show in document"));card.append(context);}
      for (const [key,label,cls] of [["before","Current","before"],["after","Proposed","after"]]) {
        const block=textElement("div","",cls);block.append(textElement("div",label,"diff-label"),textElement("pre",String(edit[key]??"")||(key==="after"?"Removed":"New content")));card.append(block);
      }
      if(edit.reason){const why=textElement("details","","change-reason");why.dataset.reviewKey=`${item.id}:reason:${item.edits.indexOf(edit)}`;why.append(textElement("summary","Why this change"),textElement("p",String(edit.reason)));card.append(why);}
    }
    const origin=proposal.attribution?.items?.[item.id];
    if(origin){const attribution=textElement("details","","change-reason");attribution.dataset.reviewKey=`${item.id}:attribution`;attribution.append(textElement("summary","Authorship"));
      const models=[...new Set((origin.models??[]).map((call:Record<string,any>)=>[call.provider,call.model].filter(Boolean).join(" · ")).filter(Boolean))];
      attribution.append(textElement("p",models.length?`AI proposal · ${models.join("; ")}`:"AI proposal · model identity was not reported"),textElement("p","Based on your feedback. Unchanged passages keep their previous authorship. Accepting a proposal records your approval; it does not label its text as human-written."));card.append(attribution);
    }
    if(awaiting) {
      const bar=textElement("div","","item-actions");
      for(const [label,value] of [["Accept","accepted"],["Decline","declined"],["Modify with AI","modify"]] as const){
        const button=textElement("button",label,"secondary") as HTMLButtonElement;button.disabled=revisionBusy||dictationPhase!=="idle";button.dataset.focusKey=`${item.id}:${value}`;button.setAttribute("aria-pressed",String(value===decisions[item.id]));
        button.addEventListener("click",()=>{if(value==="modify"){modifying=modifying===item.id?"":item.id;}else{if(decisions[item.id]===value)delete decisions[item.id];else decisions[item.id]=value;}saveState();renderRevision();if(value==="modify")document.querySelector<HTMLTextAreaElement>(".item-modify textarea")?.focus();});bar.append(button);
      }
      card.append(bar);
      if(modifying===item.id){
        const editor=textElement("div","","item-modify"),input=document.createElement("textarea"),send=textElement("button","Request revision","primary") as HTMLButtonElement;
        input.maxLength=8000;input.placeholder="Describe what you’d like instead…";input.setAttribute("aria-label","Direction for this change");input.dataset.focusKey=`${item.id}:instruction`;input.value=modifications[item.id]??"";
        input.readOnly=revisionBusy||dictationPhase!=="idle";send.disabled=revisionBusy||dictationPhase!=="idle"||!input.value.trim();input.addEventListener("input",()=>{modifications[item.id]=input.value;send.disabled=revisionBusy||dictationPhase!=="idle"||!input.value.trim();saveState();});
        send.addEventListener("click",()=>{if(send.disabled)return;pendingAnchors=item.edits.map(editLocation).filter((anchor:Anchor|undefined):anchor is Anchor=>!!anchor);revisionBusy=true;controls();api.postMessage({type:"modifyRevisionItem",approvalId:status.approvalId,itemId:item.id,instruction:input.value});renderRevision();});
        const mic=textElement("button","Dictate","secondary") as HTMLButtonElement;mic.dataset.focusKey=`${item.id}:dictate`;dictationControl(mic,item.id);mic.addEventListener("click",()=>dictate(item.id));
        const voiceStatus=textElement("small",dictationStatus(item.id),"dictation-status");voiceStatus.setAttribute("role","status");
        if(dictationTarget===item.id&&dictationError)renderProblem(voiceStatus,{message:dictationError,reference:dictationReference},value=>api.postMessage(value));
        const actions=textElement("div","","composer-actions");actions.append(send,mic,voiceStatus);editor.append(input,actions);card.append(editor);
      }
    }
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
  const action = (label: string, type: string, decision?: string, primary = false, answer?: Record<string,unknown>) => {
    const button = textElement("button", label, primary ? "primary" : "secondary") as HTMLButtonElement;
    button.disabled = dictationPhase!=="idle" || revisionBusy || (decision === "approved" && !connected);
    button.addEventListener("click", () => { if(type==="decideRevision" && decision==="approved")pendingAnchors=items.filter(item=>decisions[item.id]==="accepted"||status.answer?.acceptedItemIds?.includes(item.id)).flatMap(item=>item.edits.map(editLocation)).filter((anchor:Anchor|undefined):anchor is Anchor=>!!anchor); revisionBusy = true; controls(); renderRevision(); api.postMessage({ type, approvalId:status.approvalId, decision, answer }); }); return button;
  };
  if (typeof proposal.revisedContent === "string") { const compare = action("Compare full document", "compareRevision"); compare.className = "compare secondary"; panel.append(compare); }
  if((status.cancelRequested || ["failed","cancelled"].includes(status.status)) && proposal.amendment)actions.append(action("Try revision again","retryRevisionItem",undefined,true));
  else if (status.prepared === false) actions.append(action("Retry submission", "resumePreparation", undefined, true));
  else if (awaiting) {
    const accepted=items.filter(item=>decisions[item.id]==="accepted").map(item=>item.id),remaining=items.filter(item=>!decisions[item.id]).length;
    actions.append(textElement("p",remaining ? `${remaining} ${remaining===1?"change needs":"changes need"} a decision` : `${accepted.length} accepted · ${items.length-accepted.length} declined`,"review-progress"));
    const apply=action(accepted.length ? `Apply accepted (${accepted.length})` : "Finish review","decideRevision",accepted.length?"approved":"rejected",true,{acceptedItemIds:accepted,proposalHash:status.proposalHash});
    apply.disabled=dictationPhase!=="idle"||revisionBusy||!connected||remaining>0||items.length===0;actions.append(apply);
  }
  else if (!terminal && !retrying && status.approvalId && status.decision === "approved" && status.status === "waiting") {
    actions.append(textElement("p", "Your approval is saved. Resume to apply the accepted changes.", "review-progress"));
    actions.append(action("Resume approved changes", "decideRevision", "approved", true, status.answer));
  }
  else if (!terminal && !status.decision) actions.append(action("Stop preparing", "cancelRevision"));
}
window.addEventListener("message",event=>{
  const message=event.data;
  if(message.type==="selectionNote"&&contextSelection&&connected&&!historyPreview){openComposer(contextSelection.anchor,contextSelection.rect,"note");contextSelection=undefined;}
  if(message.type==="find")showFind(true);
  if(message.type==="notes"){personalNotes=message.rows??[];byId("notes-status").textContent=message.hasMore?"Showing your 500 most recent notes. Older notes remain saved.":"";byId("notes-retry").hidden=true;renderNotes();}
  if(message.type==="notesError"){renderProblem(byId("notes-status"),message,value=>api.postMessage(value));byId("notes-retry").hidden=false;}
  if(message.type==="noteSaved"){commentBusy=false;feedback.value="";draftAnchor=undefined;closeComposer();controls();showNotes(true);}
  if(message.type==="selectionFeedback" && contextSelection && connected && mode==="review") {
    openComposer(contextSelection.anchor,contextSelection.rect);contextSelection=undefined;
  }
  if(message.type==="dictationAvailable"){dictationAvailable=!!message.available;controls();renderRevision();}
  if(message.type==="dictation"){
    dictationPhase=message.phase;dictationError=message.error??"";dictationReference=message.reference??"";
    if(typeof message.text==="string"){
      const text=dictatedBase+(dictatedBase&&!/\s$/.test(dictatedBase)?" ":"")+message.text;
      if(dictationTarget==="feedback")feedback.value=text;else modifications[dictationTarget]=text;
      saveState();
    }
    if(message.error&&dictationTarget==="feedback")renderProblem(byId("composer-status"),{message:message.error,reference:message.reference},value=>api.postMessage(value));
    controls();renderRevision();
  }
  if(message.type==="attachmentUploaded" && uploading.has(message.uploadId)) {
    uploading.delete(message.uploadId);attachments.push(message.attachment);renderAttachments();controls();saveState();
  }
  if(message.type==="attachmentFailed" && uploading.has(message.uploadId)) {
    uploading.delete(message.uploadId);renderAttachments();controls();renderProblem(byId("composer-status"),message,value=>api.postMessage(value));saveState();
  }
  if(message.type==="attachmentPreview") {
    attachmentPreviews.set(message.uri,message.preview);renderAttachments();renderComments();
  }
  if(message.type==="restoreDraft") {
    if(message.state && !restored) {
      Object.assign(state,message.state); attachments=state.attachments??[]; renderAttachments(); Object.assign(decisions,state.decisions??{});Object.assign(modifications,state.modifications??{});Object.assign(expanded,state.expanded??{});reviewSource=state.reviewSource??""; draftAnchor=state.anchor; composerPurpose=state.purpose??"feedback"; feedback.value=state.draft??""; instruction.value=state.instruction??"";
      selected.clear(); seen.clear(); for(const id of state.included??[])selected.add(id); for(const id of state.seen??[])seen.add(id);
      showReview(state.reviewOpen??false); controls();
    }
    restored=true;
  }
  if(message.type==="viewMode") {
    const changed=mode!==message.mode;
    mode=message.mode;if(changed&&mode==="review")showNotes(false);contextSelection=undefined;document.body.classList.toggle("review-mode",mode==="review");
    for(const item of ["source","reading","review"])byId(item).setAttribute("aria-pressed",String(item===mode));
    showReview(changed&&mode==="review" ? true : reviewRequested);
    if(mode!=="review"){if(dictationPhase!=="idle")api.postMessage({type:"dictationCancel"});byId("selection-tools").hidden=true;byId("composer").hidden=true;highlight("memql-active",[]);for(const el of document.querySelectorAll(".document-target"))el.classList.remove("document-target");}
    renderRevision();
  }
  if(message.type==="history"){historyData=message.data;historyVersions=message.append?[...historyVersions,...message.data.versions]:message.data.versions;byId("history-status").textContent="";renderHistory();}
  if(message.type==="historyVersion"){
    showNotes(false);
    historyPreview=true;previewVersion=message.version;connected=false;selection=undefined;selectionRange=undefined;contextSelection=undefined;
    document.body.classList.add("history-preview");content.innerHTML=message.html;highlight("memql-active",[]);updateFind(false);
    byId("history-banner").hidden=false;byId("history-caption").textContent=`Version ${message.version} · Read-only preview`;
    branchName.value=`Branch from v${message.version}`;byId("history-branch").hidden=false;byId("history-status").textContent=latestDocument?.connected?"":"Save your local changes before starting a branch.";
    document.documentElement.scrollTop=0;showReview(reviewRequested);renderHistory();controls();
  }
  if(message.type==="historyCurrent"){
    historyPreview=false;previewVersion=undefined;document.body.classList.remove("history-preview");byId("history-banner").hidden=true;byId("history-branch").hidden=true;
    if(latestDocument)window.dispatchEvent(new MessageEvent("message",{data:latestDocument}));renderHistory();showReview(reviewRequested);
  }
  if(message.type==="historyIdle"){historyBusy=false;historyControls();}
  if(message.type==="historyError"){renderProblem(byId("history-status"),message,value=>api.postMessage(value));byId("history-retry").hidden=false;}
  if(message.type==="document") {
    latestDocument=message;documentLoadFailed=false;historyControls();if(historyPreview)return;
    emptyDocument = typeof message.sourceIdentity === "string" ? !message.sourceIdentity.trim() : !String(message.html ?? "").trim();
    document.body.classList.remove("document-pending");
    contextSelection=undefined;
    const scroll=document.documentElement.scrollTop;
    const incomingChanged = sourceIdentity !== String(message.sourceIdentity??message.version);
    content.innerHTML=message.html; progressKey=""; if(incomingChanged && !documentProcessing())reveal(content); // Host uses the HTML-disabled Markdown renderer.
    sectionTools(); version=message.version;connected=message.connected;
    const incoming=String(message.sourceIdentity??version);
    if(draftAnchor && (sourceIdentity||state.source) && (sourceIdentity||state.source)!==incoming){draftAnchor=undefined;byId("composer-status").textContent="The document changed. Your note is preserved; select its passage again before saving.";}
    if(sourceIdentity && sourceIdentity!==incoming)activeAnchor=undefined;
    sourceIdentity=incoming;if(activeAnchor&&mode==="review")jumpTo(activeAnchor,false);selection=undefined;selectionRange=undefined;byId("selection-tools").hidden=true;
    if(!connected)byId("status").textContent=message.status??"Save the document before sharing feedback.";
    document.documentElement.scrollTop=scroll;updateFind(false);renderRevision();renderNotes();controls();saveState();api.postMessage({type:"rendered",version,text:content.textContent?.slice(0,500)});
  }
  if(message.type==="comments") {
    const next=message.rows??[],different=JSON.stringify(next)!==JSON.stringify(rows);rows=next;
    for(const row of rows){if(!row.outdated&&!seen.has(row.id))selected.add(row.id);if(row.outdated)selected.delete(row.id);seen.add(row.id);}
    const available=new Set(rows.filter(row=>!row.outdated).map(row=>row.id));for(const id of selected)if(!available.has(id))selected.delete(id);
    if(different)renderRevision();controls();saveState();
  }
  if(message.type==="revision") {
    const nextSource=JSON.stringify([message.status?.proposal?.artifactId,message.status?.proposal?.revision,message.status?.proposal?.version]);
    if(reviewSource && nextSource!==reviewSource){for(const id of Object.keys(decisions))delete decisions[id];}
    reviewSource=nextSource;
    // The recorded answer wins over local choices, including when another
    // editor or Nexus applied this proposal and this view is reopening it.
    if(message.status?.decision==="approved" && Array.isArray(message.status.answer?.acceptedItemIds)){
      const accepted=new Set(message.status.answer.acceptedItemIds);
      for(const item of message.status.items??[])decisions[item.id]=accepted.has(item.id)?"accepted":"declined";
    } else if(message.status?.decision==="rejected"){
      for(const item of message.status.items??[])decisions[item.id]="declined";
    }
    const changed=message.status?.approvalId&&message.status.approvalId!==revision?.approvalId;const different=JSON.stringify(message.status)!==JSON.stringify(revision);revision=message.status;if(different)renderRevision();controls();if(changed){byId("status").textContent="";showReview(true);}saveState();}
  if(message.type==="revisionIdle"){revisionBusy=false;preparing=false;pendingAnchors=undefined;renderRevision();controls();}
  if(message.type==="saved"){commentBusy=false;attachments=[];renderAttachments();feedback.value="";draftAnchor=undefined;closeComposer();byId("status").textContent="Added to review.";controls();showReview(true);}
  if(message.type==="error"){if(!latestDocument)documentLoadFailed=true;commentBusy=false;revisionBusy=false;preparing=false;pendingAnchors=undefined;renderRevision();controls();if(!byId("composer").hidden)renderProblem(byId("composer-status"),message,value=>api.postMessage(value));else{renderProblem(byId("status"),message,value=>api.postMessage(value));showReview(true);}}
  if(message.type==="notice")byId("status").textContent=message.message;
});
showReview(state.reviewOpen??false);controls();api.postMessage({type:"ready"});
