import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { markdownPage } from "../src/markdownPage.js";
import { renderMarkdown } from "../src/markdown.js";

function fixture(initial: any = {}) {
  const dom = new JSDOM(markdownPage("Review.md", "", "test"), {runScripts:"outside-only"});
  const messages: any[]=[];let state=initial;
  const highlights=new Map<string,Set<Range>>();Object.assign(dom.window,{CSS:{highlights},Highlight:class extends Set<Range>{priority=0;constructor(...ranges:Range[]){super(ranges);}}});
  Object.assign(dom.window,{acquireVsCodeApi:()=>({postMessage:(message:any)=>messages.push(message),getState:()=>state,setState:(value:any)=>{state=value;}})});
  dom.window.eval(readFileSync("dist-test/markdown-view.js","utf8"));
  const doc=dom.window.document,el=(id:string)=>doc.getElementById(id)!;
  const send=(data:any)=>{if(data.type==="revision" && data.status?.proposal?.edits && !data.status.items)data={...data,status:{...data.status,proposalHash:"test-hash",items:data.status.proposal.edits.map((edit:any,index:number)=>({id:`item-${index}-${edit.after}`,edits:[edit],commentIds:edit.commentIds??[]}))}};return dom.window.dispatchEvent(new dom.window.MessageEvent("message",{data}));};
  send({type:"viewMode",mode:"review"});
  const document=(source="# Title\n\nKeep **this selection** and the rest.\n",version=17,connected=true)=>send({type:"document",html:renderMarkdown(source),version,sourceIdentity:source,connected,status:"Save first"});
  const select=(selector="strong")=>{const range=doc.createRange();range.selectNodeContents(doc.querySelector(selector)!);dom.window.getSelection()!.removeAllRanges();dom.window.getSelection()!.addRange(range);doc.dispatchEvent(new dom.window.Event("selectionchange"));};
  const input=(id:string,value:string)=>{(el(id) as HTMLTextAreaElement).value=value;el(id).dispatchEvent(new dom.window.Event("input"));};
  return {dom,doc,el,send,document,select,input,messages,highlights,state:()=>state};
}
test("section and footnote links focus and highlight their destination without closing review", () => {
  const f = fixture();
  f.document("# Paper\n\n[Methods](#methods) and evidence.[^e]\n\n## Methods\n\nA procedure.\n\n[^e]: A source.\n");
  if (f.el("review-panel").hidden) f.el("review-toggle").click();
  f.doc.querySelector<HTMLAnchorElement>('a[data-internal="methods"]')!.click();
  assert.equal(f.doc.activeElement?.id, "heading-methods");
  assert.ok(f.doc.querySelector("#heading-methods.anchor-target"));
  assert.equal(f.el("review-panel").hidden, false);
  f.doc.querySelector<HTMLAnchorElement>(".footnote-ref a")!.click();
  assert.equal(f.doc.activeElement?.id, "fn1");
  f.doc.querySelector<HTMLAnchorElement>(".footnote-backref")!.click();
  assert.equal(f.doc.activeElement?.id, "fnref1");
  assert.equal(f.messages.filter(m => m.type === "external").length, 0);
  f.dom.window.close();
});
test("empty documents replace boundary buttons with one creation flow and retain dictation and drafts", () => {
  const f = fixture();
  assert.equal(f.el("document-empty").hidden, true, "loading must not look empty");
  f.document(" \n\t");
  assert.equal(f.el("document-empty").hidden, false);
  assert.equal(f.el("document-feedback").hidden, true);
  assert.equal(f.el("extend").hidden, true);
  f.el("empty-create").click();
  assert.equal(f.el("composer-title").textContent, "Create your document");
  assert.equal(f.el("feedback").getAttribute("aria-label"), "Document description");
  f.input("feedback", "Research membranes for a technical audience.");
  f.send({type:"dictationAvailable",available:true});
  f.el("dictate").click();
  assert.equal(f.messages.at(-1).type,"dictationStart");
  f.send({type:"dictation",phase:"idle",text:"Include primary sources."});
  f.el("add").click();
  const message=f.messages.find(m=>m.type==="comment");
  assert.equal(message.selection.kind,"document");
  assert.match(message.body,/Include primary sources/);
  f.send({type:"saved"});
  f.send({type:"comments",rows:[{id:"brief",body:message.body,anchor:message.selection}]});
  f.el("review-close").click();
  f.el("empty-create").click();
  assert.equal(f.el("review-panel").hidden,false);
  assert.equal(f.el("composer").hidden,true,"a saved request must not start another draft");
  f.el("prepare-revision").click();
  assert.equal(f.messages.at(-1).type,"prepareRevision");
  assert.deepEqual(Array.from(f.messages.at(-1).commentIds),["brief"]);
  f.dom.window.close();
});
test("empty-state progress opens the current review without starting another generation", () => {
  const f=fixture();f.document("");
  f.send({type:"revision",status:{status:"running",proposal:{}}});
  f.send({type:"revisionIdle"});
  assert.equal(f.el("empty-title").textContent,"Creating your draft");
  f.el("review-close").click();const before=f.messages.length;
  f.el("empty-create").click();assert.equal(f.el("review-panel").hidden,false);
  assert.equal(f.messages.slice(before).some(m=>["comment","prepareRevision"].includes(m.type)),false);
  f.send({type:"revision",status:{status:"waiting",prepared:true,proposal:{}}});
  assert.equal(f.el("empty-title").textContent,"Draft preparation paused");
  f.send({type:"revision",status:{status:"waiting",approvalId:"approval",proposal:{}}});
  assert.equal(f.el("empty-title").textContent,"Your draft is ready");
  f.document("# Generated document\n\nEvidence.",18);
  assert.equal(f.el("document-empty").hidden,true);
  assert.equal(f.el("extend").hidden,false);
  assert.equal(f.el("document-feedback").hidden,false);
  f.dom.window.close();
});
test("empty reading and history views keep creation behind the appropriate mode", () => {
  const f=fixture();f.document("");f.send({type:"viewMode",mode:"reading"});
  f.el("empty-create").click();assert.equal(f.messages.at(-1).type,"review");
  assert.equal(f.el("composer").hidden,true);
  f.el("empty-source").click();assert.equal(f.messages.at(-1).type,"source");
  f.document("",18,false);assert.equal(f.el("empty-create").hidden,true);
  f.send({type:"historyVersion",version:1,html:""});
  assert.equal(f.el("document-empty").hidden,true);
  f.send({type:"historyCurrent"});
  assert.equal(f.el("document-empty").hidden,false);
  f.dom.window.close();
});
test("selection tools appear contextually and preserve exact rendered offsets",()=>{
  const f=fixture();f.document();assert.equal(f.doc.querySelector("#feedback-toggle"),null);assert.equal((f.el("annotate") as HTMLButtonElement).disabled,true);
  f.select();assert.equal(f.el("selection-tools").hidden,false);f.el("selection-feedback").click();assert.equal(f.el("composer").hidden,false);
  f.input("feedback","Rephrase just these words.");f.el("add").click();
  const message=f.messages.find(m=>m.type==="comment");assert.equal(message.version,17);assert.equal(message.selection.quote,"this selection");assert.equal(message.selection.startTextOffset,5);assert.equal(message.selection.endTextOffset,19);assert.equal(message.selection.prefix,"Keep ");assert.equal(message.selection.suffix," and the rest.");
  f.send({type:"saved"});assert.equal(f.el("composer").hidden,true);assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"");f.dom.window.close();
});
test("collapsed selection disables the tool; keyboard selection can still open it",()=>{
  const f=fixture();f.document();f.select();f.dom.window.getSelection()!.removeAllRanges();f.doc.dispatchEvent(new f.dom.window.Event("selectionchange"));assert.equal(f.el("selection-tools").hidden,true);assert.equal((f.el("annotate") as HTMLButtonElement).disabled,true);
  f.select();f.doc.dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"m",ctrlKey:true,altKey:true}));assert.equal(f.el("composer").hidden,false);assert.equal(f.doc.activeElement?.id,"feedback");f.dom.window.close();
});
test("context-menu feedback keeps the selected passage through menu focus and refuses stale or reading selections",()=>{
  const f=fixture();f.document();f.select();
  const context=()=>{f.doc.querySelector("strong")!.dispatchEvent(new f.dom.window.MouseEvent("contextmenu",{bubbles:true}));return JSON.parse(f.doc.body.dataset.vscodeContext!);};
  assert.equal(context().memqlMarkdownFeedback,true);
  f.dom.window.getSelection()!.removeAllRanges();f.doc.dispatchEvent(new f.dom.window.Event("selectionchange"));
  f.send({type:"selectionFeedback"});assert.equal(f.el("composer").hidden,false);assert.equal(f.el("selected").textContent,"this selection");
  f.el("composer-close").click();f.select();context();f.send({type:"viewMode",mode:"reading"});f.send({type:"selectionFeedback"});assert.equal(f.el("composer").hidden,true);
  assert.equal(context().memqlMarkdownFeedback,false);
  f.send({type:"viewMode",mode:"review"});f.select();context();f.document("Changed source",18);f.send({type:"selectionFeedback"});assert.equal(f.el("composer").hidden,true);
  f.document(undefined,19,false);f.select();assert.equal(context().memqlMarkdownFeedback,false);f.dom.window.close();
});
test("clipboard menus follow feedback-field editability, including dynamic and dictation-locked fields",()=>{
  const f=fixture();f.document();f.el("extend").click();
  const input=f.el("feedback") as HTMLTextAreaElement;
  const context=(target:HTMLTextAreaElement)=>{target.dispatchEvent(new f.dom.window.MouseEvent("contextmenu",{bubbles:true}));return JSON.parse(target.dataset.vscodeContext!);};
  assert.equal(context(input).preventDefaultContextMenuItems,false);
  f.send({type:"dictation",phase:"listening"});assert.equal(context(input).preventDefaultContextMenuItems,true);
  f.send({type:"dictation",phase:"idle"});assert.equal(context(input).preventDefaultContextMenuItems,false);
  const dynamic=f.doc.createElement("textarea");f.el("revision").append(dynamic);assert.equal(context(dynamic).preventDefaultContextMenuItems,false);
  dynamic.disabled=true;assert.equal(context(dynamic).preventDefaultContextMenuItems,true);f.dom.window.close();
});
test("end-of-document extension has its own intent and preserves drafts on errors",()=>{
  const f=fixture();f.document();f.el("extend").click();assert.equal(f.el("composer-title").textContent,"Extend document");f.input("feedback","Add examples and next steps.");f.el("add").click();
  const message=f.messages.find(m=>m.type==="comment");assert.equal(message.selection.kind,"document-end");assert.equal(message.body,"Add examples and next steps.");
  f.send({type:"error",message:"Connection lost; retry"});assert.equal(f.state().draft,"Add examples and next steps.");assert.equal((f.el("add") as HTMLButtonElement).disabled,false);assert.equal(f.el("composer-status").textContent,"Connection lost; retry");
  f.el("composer-close").click();assert.equal(f.state().draft,"Add examples and next steps.");f.dom.window.close();
});
test("a second selection cannot silently retarget an unfinished note, and a new source requires reanchoring",()=>{
  const f=fixture();f.document();f.select();f.el("annotate").click();f.input("feedback","Keep my note");f.el("composer-close").click();f.el("extend").click();assert.equal(f.state().anchor.kind,undefined);assert.match(f.el("composer-status").textContent!,/original selection/);
  f.document("Changed source",18);assert.equal((f.el("add") as HTMLButtonElement).disabled,true);assert.equal(f.state().draft,"Keep my note");f.dom.window.close();
});
test("multiple notes submit together; actual proposed changes precede approval and render safely",()=>{
  const f=fixture();f.document();f.send({type:"comments",rows:[{id:"a",body:"Rephrase",outdated:false,anchor:{quote:"word"}},{id:"b",body:"Add examples",outdated:false,anchor:{kind:"document-end"}},{id:"old",body:"Earlier",outdated:true,anchor:{quote:"old"}}]});
  assert.equal(f.doc.querySelectorAll('[role="switch"]').length,2);f.el("prepare-revision").click();const request=f.messages.find(m=>m.type==="prepareRevision");assert.deepEqual(Array.from(request.commentIds),["a","b"]);assert.equal(request.instruction,"");
  f.send({type:"revision",status:{status:"running",proposal:{}}});f.send({type:"revisionIdle"});assert.ok(!f.el("revision").textContent!.includes("Approve & apply"));
  const status={prepared:true,approvalId:"exact-approval",status:"waiting",decision:"",proposal:{summary:"Move and expand",revisedContent:"Revised",edits:[{before:"<script>private()</script>",after:"<img src=x>",reason:"Move this"},{before:"Destination",after:"Destination\n\nMoved text.",reason:"Insert there"}]}};
  f.send({type:"revision",status});assert.equal(f.doc.querySelectorAll("#revision script,#revision img").length,0);assert.equal(f.doc.querySelectorAll(".change").length,2);assert.equal(f.el("review-panel").hidden,false);assert.equal(f.el("review-submit").hidden,true);assert.equal(f.el("review-footer").hidden,false);
  const approve=f.el("review-actions").lastElementChild;f.send({type:"revision",status});assert.equal(f.el("review-actions").lastElementChild,approve,"polling must preserve focus and expanded changes");
  for(const button of [...f.doc.querySelectorAll<HTMLButtonElement>(".item-actions button")].filter(b=>b.textContent==="Accept")){const key=button.dataset.focusKey;[...f.doc.querySelectorAll<HTMLButtonElement>("[data-focus-key]")].find(b=>b.dataset.focusKey===key)!.click();}
  [...f.doc.querySelectorAll<HTMLButtonElement>("#review-actions button")].find(b=>b.textContent==="Apply accepted (2)")!.click();assert.equal(f.messages.at(-1).approvalId,"exact-approval");assert.equal(f.messages.at(-1).decision,"approved");
  f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",result:{applied:true}}});f.send({type:"revisionIdle"});assert.match(f.el("revision").textContent!,/Changes applied/);f.dom.window.close();
});
test("selection choices survive refresh and views retain accessible keyboard controls",()=>{
  const f=fixture();f.document();const rows=[{id:"a",body:"One",anchor:{kind:"document-end"}},{id:"b",body:"Two",anchor:{kind:"document-end"}}];f.send({type:"comments",rows});(f.doc.querySelector('[role="switch"]') as HTMLInputElement).click();f.send({type:"comments",rows});assert.equal((f.doc.querySelector('[role="switch"]') as HTMLButtonElement).getAttribute("aria-checked"),"false");
  f.el("review").click();assert.equal(f.messages.at(-1).type,"review");f.send({type:"viewMode",mode:"review"});assert.equal(f.el("review").getAttribute("aria-pressed"),"true");f.el("source").dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"ArrowRight",bubbles:true}));assert.equal(f.doc.activeElement?.id,"reading");
  f.document("Local edits",18,false);assert.equal((f.el("prepare-revision") as HTMLButtonElement).disabled,true);assert.equal((f.el("extend") as HTMLButtonElement).disabled,true);f.dom.window.close();
});

test("unfinished feedback survives switching out of Read and back, without retargeting",()=>{
  const first=fixture();first.send({type:"restoreDraft"});first.document();first.select();first.el("annotate").click();first.input("feedback","Keep this unfinished note");
  const saved=first.messages.filter(m=>m.type==="draftState").at(-1).state;first.dom.window.close();
  const next=fixture();next.send({type:"restoreDraft",state:saved});next.document();next.el("annotate").click();
  assert.equal((next.el("feedback") as HTMLTextAreaElement).value,"Keep this unfinished note");assert.equal(next.state().anchor.quote,"this selection");assert.equal((next.el("add") as HTMLButtonElement).disabled,false);next.dom.window.close();
});

test("standalone extensions replace the previous review with the same diff and approval controls",()=>{
  const f=fixture();f.document();
  const prior={status:"succeeded",result:{applied:true},proposal:{commentIds:["old"],summary:"Earlier edit",edits:[{before:"Old",after:"Earlier",reason:"Earlier edit"}]}};
  f.send({type:"revision",status:prior});f.el("extend").click();f.input("feedback","Add next steps");
  assert.equal(f.el("add").textContent,"Add to review");f.el("add").click();
  f.send({type:"saved"});f.send({type:"comments",rows:[{id:"extension",body:"Add next steps",anchor:{kind:"document-end"}}]});
  assert.equal(f.el("review-panel").hidden,false);
  assert.match(f.el("comments").textContent!,/Requests/);
  const previous=f.doc.querySelector<HTMLDetailsElement>("#revision > details")!;
  assert.equal(previous.open,false);assert.match(previous.textContent!,/Earlier edit/);
  f.el("prepare-revision").click();assert.deepEqual(Array.from(f.messages.at(-1).commentIds),["extension"]);
  assert.match(f.el("revision").textContent!,/Preparing changes/);assert.ok(!f.el("revision").textContent!.includes("Earlier edit"));
  f.send({type:"revision",status:{status:"waiting",prepared:true,approvalId:"extension-approval",proposal:{commentIds:["extension"],summary:"Add next steps",revisedContent:"Existing\n\n## Next steps",edits:[{before:"Existing",after:"Existing\n\n## Next steps",reason:"Extend"}]}}});
  f.send({type:"revisionIdle"});
  assert.equal(f.el("review-footer").hidden,false);assert.equal(f.el("review-submit").hidden,true);
  assert.equal(f.doc.querySelectorAll(".change").length,1);assert.match(f.el("revision").textContent!,/## Next steps/);
  f.doc.querySelector<HTMLButtonElement>(".item-actions button")!.click();
  assert.ok([...f.doc.querySelectorAll<HTMLButtonElement>("#review-actions button")].some(button=>button.textContent==="Apply accepted (1)"&&!button.disabled));f.dom.window.close();
});


test("one feedback action carries addition requests at precise section and passage locations",()=>{
  const f=fixture();f.document("# Guide\n\n## Examples\n\nKeep **this selection** and the rest.\n\n## Examples\n\nLater content.\n");
  const sections=f.doc.querySelectorAll<HTMLButtonElement>('button[aria-label="Feedback on section: Examples"]');
  sections[1].click();assert.equal(f.el("composer-title").textContent,"Add feedback");f.input("feedback","Add an exercise");f.el("add").click();
  const section=f.messages.find(m=>m.type==="comment").selection;
  assert.equal(section.intent,undefined);assert.equal(section.scope,"section");assert.equal(section.startLine,6);assert.equal(section.quote,"Examples");assert.equal(section.endTextOffset,8);
  f.send({type:"saved"});f.select();assert.equal(f.doc.querySelectorAll("#selection-tools button").length,3);f.el("selection-feedback").click();assert.equal(f.el("composer-title").textContent,"Add feedback");f.input("feedback","Add a concrete example");f.el("add").click();
  const passage=f.messages.filter(m=>m.type==="comment").at(-1).selection;
  assert.equal(passage.intent,undefined);assert.equal(passage.scope,undefined);assert.equal(passage.quote,"this selection");assert.equal(passage.startTextOffset,5);assert.equal(passage.endTextOffset,19);
  f.dom.window.close();
});

test("request toggles persist through polling and only included requests enter the proposal",()=>{
  const f=fixture();f.document();const rows=[{id:"a",body:"One",anchor:{kind:"document-end"}},{id:"b",body:"Two",anchor:{kind:"document-end"}}];f.send({type:"comments",rows});
  const toggle=f.doc.querySelector<HTMLButtonElement>('[role="switch"]')!;toggle.focus();toggle.click();
  f.send({type:"comments",rows});assert.equal(f.doc.activeElement,toggle);assert.equal(toggle.getAttribute("aria-checked"),"false");
  assert.equal(f.doc.querySelectorAll('input[type="checkbox"]').length,0);
  f.el("prepare-revision").click();assert.deepEqual(Array.from(f.messages.at(-1).commentIds),["b"]);assert.equal(f.doc.querySelectorAll('[role="switch"]').length,0);
  assert.ok(!f.el("comments").textContent!.includes("Two"),"submitted request must not appear under next review");
  f.send({type:"revision",status:{status:"waiting",approvalId:"ap",proposal:{commentIds:["b"],comments:[rows[1]],edits:[{before:"rest.",after:"rest. More detail.",commentIds:["b"]}]}}});f.send({type:"revisionIdle"});
  assert.equal(f.doc.querySelectorAll('[role="switch"]').length,0);assert.match(f.doc.querySelector(".request-context")!.textContent!,/Two/);assert.match(f.el("comments").textContent!,/One/);
  f.dom.window.close();
});

test("navigation keeps review open and uses the proposed insertion location, including middle-of-document extensions",()=>{
  const f=fixture({reviewOpen:true});f.document("# Workshop\n\n## Schedule\n\n- Lunch break\n\n## Activities\n\nRead an excerpt.\n");
  let target:Element|undefined;f.dom.window.HTMLElement.prototype.scrollIntoView=function(){target=this;};
  const rows=[{id:"ext",body:"Add questions after lunch",anchor:{kind:"document-end",quote:"End of document"}}];f.send({type:"comments",rows});
  const end=f.doc.querySelector<HTMLButtonElement>('#comments .passage')!;end.focus();end.click();assert.equal(target,f.el("extend"));assert.equal(f.el("review-panel").hidden,false);assert.equal(f.doc.activeElement,end);
  f.send({type:"revision",status:{status:"waiting",approvalId:"ap",proposal:{commentIds:["ext"],comments:rows,edits:[{before:"- Lunch break",after:"- Lunch break\n- Questions",commentIds:["ext"]}]}}});
  assert.equal(f.doc.querySelector('.change>summary')!.textContent,"Extend: Schedule");
  f.doc.querySelector<HTMLButtonElement>('.request-context .passage')!.click();assert.equal(target?.tagName,"LI");assert.match(target?.textContent??"",/Lunch break/);assert.equal(f.el("review-panel").hidden,false);assert.equal(f.state().reviewOpen,true);
  f.dom.window.close();
});

test("submission failure preserves requests, retry controls and unaddressed request context",()=>{
  const f=fixture({reviewOpen:true});f.document();const rows=[{id:"ext",body:"Add examples",anchor:{kind:"document-end"}}];f.send({type:"comments",rows});f.el("prepare-revision").click();
  f.send({type:"error",message:"Connection interrupted"});assert.equal((f.el("prepare-revision") as HTMLButtonElement).disabled,false);assert.match(f.el("comments").textContent!,/Add examples/);
  f.send({type:"revision",status:{status:"waiting",prepared:false,proposal:{commentIds:["ext"],comments:rows}}});
  assert.equal(f.el("review-actions").textContent,"Retry submission");assert.match(f.el("revision").textContent!,/Add examples/);
  f.send({type:"revision",status:{status:"waiting",prepared:true,approvalId:"ap",proposal:{commentIds:["ext"],comments:rows,edits:[]}}});
  assert.match(f.el("revision").textContent!,/No change proposed for this request/);assert.equal(f.doc.querySelectorAll('[role="switch"]').length,0);f.dom.window.close();
});

test("polling preserves explanations, item decisions, focus and modification drafts",()=>{
 const f=fixture();f.document();
 const status={status:"waiting",approvalId:"approval",proposal:{artifactId:"doc",revision:"v1",version:1,edits:[{before:"this selection",after:"better words",reason:"Use clear wording",commentIds:["note"]}],comments:[{id:"note",body:"Rephrase",anchor:{kind:"document-end"}}]}};
 f.send({type:"revision",status});
 const why=f.doc.querySelector<HTMLDetailsElement>(".change-reason")!;why.open=true;
 [...f.doc.querySelectorAll<HTMLButtonElement>(".item-actions button")].find(b=>b.textContent==="Modify with AI")!.click();
 const input=f.doc.querySelector<HTMLTextAreaElement>(".item-modify textarea")!;input.value="Research this claim first";input.dispatchEvent(new f.dom.window.Event("input"));input.focus();input.setSelectionRange(5,9);
 f.send({type:"revision",status:{...status,heartbeat:2}});f.send({type:"revisionIdle"});
 assert.equal(f.doc.querySelector<HTMLDetailsElement>(".change-reason")!.open,true);
 const restored=f.doc.querySelector<HTMLTextAreaElement>(".item-modify textarea")!;assert.equal(restored.value,"Research this claim first");assert.equal(f.doc.activeElement,restored);assert.equal(restored.selectionStart,5);assert.equal(restored.selectionEnd,9);
 f.doc.querySelector<HTMLButtonElement>(".item-modify button")!.click();assert.equal(f.messages.at(-1).type,"modifyRevisionItem");assert.equal(f.messages.at(-1).instruction,"Research this claim first");f.dom.window.close();
});

test("Read is a quiet reading mode and restores Review without losing the draft",()=>{
 const f=fixture();f.document();f.select();f.el("annotate").click();f.input("feedback","Keep this draft");
 f.send({type:"viewMode",mode:"reading"});assert.equal(f.doc.body.classList.contains("review-mode"),false);assert.equal(f.el("review-panel").hidden,true);assert.equal(f.el("composer").hidden,true);
 f.select();assert.equal(f.el("selection-tools").hidden,false);assert.equal(f.el("selection-feedback").hidden,true);
 f.send({type:"revision",status:{status:"waiting",approvalId:"new",proposal:{}}});assert.equal(f.el("review-panel").hidden,true);
 f.send({type:"viewMode",mode:"review"});assert.equal(f.el("review-panel").hidden,false);f.el("annotate").click();assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Keep this draft");f.dom.window.close();
});

test("dictation appends cumulative transcripts once and leaves submission to the person",()=>{
 const f=fixture();f.document();f.el("extend").click();f.input("feedback","Please");f.send({type:"dictationAvailable",available:true});f.el("dictate").click();assert.equal(f.messages.at(-1).type,"dictationStart");
 f.send({type:"dictation",phase:"listening",text:"add"});f.send({type:"dictation",phase:"listening",text:"add examples"});assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Please add examples");assert.equal((f.el("add") as HTMLButtonElement).disabled,true);
 f.el("dictate").click();assert.equal(f.messages.at(-1).type,"dictationStop");f.send({type:"dictation",phase:"idle",text:"add examples.",final:true});assert.equal((f.el("add") as HTMLButtonElement).disabled,false);assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Please add examples.");assert.ok(!f.messages.some(m=>m.type==="comment"));f.dom.window.close();
});

test("a paused preparation does not look like an active model call",()=>{
 const f=fixture();f.document();f.send({type:"revision",status:{prepared:true,status:"waiting",problem:{message:"The AI stopped responding.",reference:"review-run-test"},proposal:{comments:[],edits:[]}}});
 assert.equal(f.doc.querySelector("#revision h3")!.textContent,"Preparation paused");assert.equal(f.doc.querySelector("#revision .busy"),null);assert.match(f.el("revision").textContent!,/stopped responding/);assert.match(f.el("review-actions").textContent!,/Stop preparing/);f.dom.window.close();
});

test("resume applies the recorded approval once and disappears for stopped or completed reviews",()=>{
 const f=fixture();f.document();
 const answer={acceptedItemIds:["kept"],proposalHash:"reviewed-hash"};
 const status={prepared:true,status:"waiting",approvalId:"approval",decision:"approved",answer,proposal:{}};
 const resume=()=>[...f.doc.querySelectorAll<HTMLButtonElement>("#review-actions button")].find(button=>button.textContent==="Resume approved changes");
 f.send({type:"revision",status});assert.ok(resume());resume()!.click();
 assert.equal(f.messages.at(-1).type,"decideRevision");assert.equal(f.messages.at(-1).approvalId,"approval");assert.equal(f.messages.at(-1).answer,answer);
 assert.equal(resume()!.disabled,true);resume()!.click();assert.equal(f.messages.filter(message=>message.type==="decideRevision").length,1);
 f.send({type:"revisionIdle"});
 for(const change of [{cancelRequested:true},{status:"cancelled"},{status:"failed"},{status:"succeeded",result:{applied:true}}]){
   f.send({type:"revision",status:{...status,...change}});assert.equal(resume(),undefined);
 }
 f.dom.window.close();
});

test("reopening a decided proposal restores the recorded subset rather than local guesses",()=>{
 const f=fixture({decisions:{kept:"declined",omitted:"accepted"}});f.document();
 f.send({type:"revision",status:{status:"succeeded",decision:"approved",answer:{acceptedItemIds:["kept"]},items:[{id:"kept",edits:[{before:"Keep",after:"Retain"}]},{id:"omitted",edits:[{before:"rest",after:"other"}]}],proposal:{artifactId:"doc",revision:"v1",edits:[]},result:{applied:true}}});
 assert.deepEqual([...f.doc.querySelectorAll('.item-decision')].map(el=>el.textContent),["Accepted","Declined"]);f.dom.window.close();
});


test("item dictation survives polling, stays on its item and requires explicit revision submission",()=>{
 const f=fixture();f.document();f.input("feedback","Separate note");
 const status={status:"waiting",approvalId:"approval",proposal:{artifactId:"doc",revision:"v1",version:1,edits:[{before:"this selection",after:"better words",reason:"Clear wording",commentIds:["note"]}],comments:[{id:"note",body:"Rephrase",anchor:{kind:"document-end"}}]}};
 f.send({type:"revision",status});
 [...f.doc.querySelectorAll<HTMLButtonElement>(".item-actions button")].find(b=>b.textContent==="Modify with AI")!.click();
 const mic=()=>f.doc.querySelector<HTMLButtonElement>('.item-modify [data-focus-key$=":dictate"]')!;
 assert.equal(mic().hidden,true);f.send({type:"dictationAvailable",available:true});assert.equal(mic().hidden,false);
 const input=f.doc.querySelector<HTMLTextAreaElement>(".item-modify textarea")!;input.value="Please";input.dispatchEvent(new f.dom.window.Event("input"));
 mic().click();assert.equal(f.messages.at(-1).type,"dictationStart");
 f.send({type:"dictation",phase:"listening",text:"verify"});f.send({type:"revision",status:{...status,heartbeat:2}});f.send({type:"dictation",phase:"listening",text:"verify the source"});
 assert.equal(f.doc.querySelector<HTMLTextAreaElement>(".item-modify textarea")!.value,"Please verify the source");
 assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Separate note");
 assert.equal(f.doc.querySelector<HTMLButtonElement>(".item-modify .primary")!.disabled,true);
 mic().click();assert.equal(f.messages.at(-1).type,"dictationStop");f.send({type:"dictation",phase:"idle",text:"verify the source.",final:true});
 assert.ok(!f.messages.some(m=>m.type==="modifyRevisionItem"));f.doc.querySelector<HTMLButtonElement>(".item-modify .primary")!.click();
 assert.equal(f.messages.at(-1).type,"modifyRevisionItem");assert.equal(f.messages.at(-1).instruction,"Please verify the source.");f.dom.window.close();
});

test("switching to quiet Read cancels microphone capture and preserves the partial note",()=>{
 const f=fixture();f.document();f.el("extend").click();f.send({type:"dictationAvailable",available:true});f.el("dictate").click();
 f.send({type:"dictation",phase:"listening",text:"Add examples"});f.send({type:"viewMode",mode:"reading"});
 assert.ok(f.messages.some(m=>m.type==="dictationCancel"));assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Add examples");assert.equal(f.el("composer").hidden,true);assert.ok(!f.messages.some(m=>m.type==="comment"));f.dom.window.close();
});

test("speech failure appears once, preserves typed feedback and clears on retry",()=>{
 const f=fixture();f.document();f.select();f.el("selection-feedback").click();f.input("feedback","Add an example.");
 f.send({type:"dictationAvailable",available:true});f.el("dictate").click();
 const error="No speech recognition model is available. Check Fleet.";
 f.send({type:"dictation",phase:"idle",error});
 assert.equal(f.el("composer-status").textContent,error);assert.equal(f.el("dictation-status").textContent,"");
 assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Add an example.");
 assert.equal((f.el("add") as HTMLButtonElement).disabled,false);
 f.el("dictate").click();assert.equal(f.el("composer-status").textContent,"");assert.equal(f.messages.at(-1).type,"dictationStart");
 assert.ok(!f.messages.some(m=>m.type==="comment"));f.dom.window.close();
});

test("document-wide feedback and extension remain different scopes without prescribing a rewrite",()=>{
 const f=fixture();f.document();f.el("document-feedback").click();assert.equal(f.el("composer-title").textContent,"Revise entire document");
 f.input("feedback","Replace Alice with Morgan throughout.");f.el("add").click();assert.equal(f.messages.at(-1).selection.kind,"document");
 f.send({type:"saved"});f.el("extend").click();f.input("feedback","Add an example.");f.el("add").click();assert.equal(f.messages.at(-1).selection.kind,"document-end");
 f.el("find").click();assert.equal(f.el("document-find").hidden,false);f.dom.window.close();
});
test("history preview isolates source and feedback, survives polling and returns to the current document",()=>{
 const f=fixture({reviewOpen:true});f.document();f.send({type:"document",html:renderMarkdown("Current text"),version:17,sourceIdentity:"Current text",connected:true,historyAvailable:true});
 f.el("history-toggle").click();assert.equal(f.messages.at(-1).type,"history");assert.equal(f.el("review-panel").hidden,true);
 f.send({type:"history",data:{versions:[{version:7,current:true},{version:0}],hasMore:true,beforeVersion:0,branches:[]}});f.send({type:"historyIdle"});
 f.doc.querySelector<HTMLButtonElement>('[aria-label="Open version 0"]')!.click();assert.equal(f.messages.at(-1).version,0);
 f.send({type:"historyVersion",version:0,revision:"initial",html:renderMarkdown("Earlier text")});f.send({type:"historyIdle"});
 assert.equal(f.el("content").textContent?.trim(),"Earlier text");assert.equal((f.el("source") as HTMLButtonElement).disabled,true);assert.equal((f.el("extend") as HTMLButtonElement).disabled,true);
 f.document("Latest after polling",18);assert.equal(f.el("content").textContent?.trim(),"Earlier text");
 f.input("branch-name","Alternative");f.el("history-fork").click();assert.equal(f.messages.at(-1).name,"Alternative");
 f.send({type:"historyError",message:"Connection interrupted"});f.send({type:"historyIdle"});assert.equal(f.el("history-retry").hidden,false);f.el("history-retry").click();assert.equal(f.messages.at(-1).type,"historyFork");
 f.send({type:"historyIdle"});f.send({type:"historyCurrent"});assert.equal(f.el("content").textContent?.trim(),"Latest after polling");assert.equal((f.el("source") as HTMLButtonElement).disabled,false);
 f.dom.window.close();
});

test("Read selection offers copy and private notes; markers open the separate notes panel",()=>{
 const f=fixture();f.document();f.send({type:"viewMode",mode:"reading"});f.select();
 assert.equal(f.el("selection-tools").hidden,false);assert.equal(f.el("selection-feedback").hidden,true);
 f.el("selection-copy").click();assert.equal(f.messages.at(-1).type,"copy");f.el("selection-note").click();
 assert.equal(f.el("composer-title").textContent,"Add note");f.input("feedback","Remember this for the meeting.");f.el("add").click();
 const saved=f.messages.at(-1);assert.equal(saved.type,"note");assert.equal(saved.selection.quote,"this selection");
 const anchor={...saved.selection,kind:"markdown",sourceQuote:"Keep **this selection** and the rest."};
 f.send({type:"notes",rows:[{id:"private-1",body:"Remember this for the meeting.",anchor}]});f.send({type:"noteSaved"});
 assert.equal(f.el("notes-panel").hidden,false);assert.equal(f.el("review-panel").hidden,true);assert.equal(f.doc.querySelectorAll(".note-marker").length,1);
 f.el("notes-close").click();f.doc.querySelector<HTMLButtonElement>(".note-marker")!.click();assert.equal(f.el("notes-panel").hidden,false);assert.match(f.el("personal-notes").textContent!,/Remember this/);assert.equal(f.doc.querySelectorAll("#comments .note").length,0);
 // A changed passage stays listed but must not acquire a misleading marker.
 f.document("# Title\n\nA completely different passage.\n",18);f.send({type:"notes",rows:[{id:"private-1",body:"Remember this for the meeting.",anchor,outdated:true}]});
 assert.equal(f.doc.querySelectorAll(".note-marker").length,0);assert.match(f.el("personal-notes").textContent!,/earlier version/);
 f.document("# Title\n\nNew introduction.\n\nKeep **this selection** and the rest.\n",19);assert.equal(f.doc.querySelectorAll(".note-marker").length,1);
 f.document("# Title\n\nKeep **this selection** with a corrected name.\n",20);assert.equal(f.doc.querySelectorAll(".note-marker").length,1);
 f.document("# Title\n\nKeep **this selection**.\n\nRepeat **this selection**.\n",21);assert.equal(f.doc.querySelectorAll(".note-marker").length,0);
 f.dom.window.close();
});

test("document search matches across inline formatting, wraps, and never searches notes or review text",()=>{
 const f=fixture();f.document("# Guide\n\nAlice labels **every box**.\n\nAsk Alice to close.\n");f.send({type:"viewMode",mode:"reading"});
 f.doc.dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"f",metaKey:true,cancelable:true,bubbles:true}));assert.equal(f.el("document-find").hidden,false);
 f.input("find-query","Alice");assert.equal(f.el("find-count").textContent,"1 of 2");f.el("find-next").click();assert.equal(f.el("find-count").textContent,"2 of 2");f.el("find-next").click();assert.equal(f.el("find-count").textContent,"1 of 2");f.el("find-previous").click();assert.equal(f.el("find-count").textContent,"2 of 2");
 f.input("find-query","labels every box");assert.equal(f.el("find-count").textContent,"1 of 1");
 f.input("find-query","[.*]");assert.equal(f.el("find-count").textContent,"No matches");assert.equal((f.el("find-next") as HTMLButtonElement).disabled,true);
 f.send({type:"comments",rows:[{id:"a",body:"Only in feedback",anchor:{kind:"document"}}]});f.input("find-query","Only in feedback");assert.equal(f.el("find-count").textContent,"No matches");
 f.input("find-query","Alice");f.document("# Replacement\n\nNo occurrences.\n",18);assert.equal(f.el("find-count").textContent,"No matches");f.el("find-close").click();assert.equal(f.el("document-find").hidden,true);f.dom.window.close();
});

test("search highlights all matches and keeps an indexed active match through navigation and closing",()=>{
 const f=fixture();f.document("# Guide\n\nAlice labels boxes.\n\nAsk Alice to close.\n");f.el("find").click();f.input("find-query","Alice");
 assert.equal(f.highlights.get("memql-find")?.size,2);assert.equal(f.highlights.get("memql-find-active")?.size,1);
 assert.equal([...f.highlights.get("memql-find-active")!][0].startContainer.textContent,"Alice labels boxes.");assert.equal(f.el("find-marker").textContent,"1");
 f.el("find-next").click();assert.equal([...f.highlights.get("memql-find-active")!][0].startContainer.textContent,"Ask Alice to close.");assert.equal(f.el("find-marker").textContent,"2");
 f.input("find-query","absent");assert.equal(f.highlights.has("memql-find-active"),false);assert.equal(f.el("find-marker").hidden,true);
 f.input("find-query","Alice");f.el("find-close").click();assert.equal(f.highlights.has("memql-find"),false);assert.equal(f.highlights.has("memql-find-active"),false);assert.equal(f.el("find-marker").hidden,true);f.dom.window.close();
});

test("recovered and applied proposals do not retain an error from an earlier model attempt",()=>{
 const f=fixture();f.document();
 const status={prepared:true,status:"waiting",approvalId:"ready",errorMessage:"Earlier attempt timed out",proposal:{comments:[],edits:[{before:"old",after:"new"}]}};
 f.send({type:"revision",status});assert.equal(f.doc.querySelector("#revision .review-error"),null);assert.match(f.el("revision").textContent!,/Proposed changes/);
 f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",result:{applied:true}}});assert.equal(f.doc.querySelector("#revision .review-error"),null);assert.match(f.el("revision").textContent!,/Changes applied/);f.dom.window.close();
});


test("automatic retries have a stable details panel and never offer manual resume",async()=>{
 const f=fixture();f.document();
 const status={prepared:true,status:"waiting",approvalId:"approved",decision:"approved",waitingOn:{kind:"retry"},problem:{message:"The AI stopped responding.",reference:"review-run-test"},proposal:{comments:[],edits:[]}};
 f.send({type:"revision",status});assert.match(f.el("revision").textContent!,/Retrying automatically/);assert.doesNotMatch(f.el("review-actions").textContent!,/Resume/);
 const details=f.doc.querySelector<HTMLDetailsElement>(".problem-details")!;details.open=true;
 await new Promise(resolve=>setTimeout(resolve,0));
 f.send({type:"revision",status:{...status,retryCount:2}});assert.equal(f.doc.querySelector<HTMLDetailsElement>(".problem-details")!.open,true);
 f.doc.querySelector<HTMLButtonElement>(".problem-details button")!.click();assert.deepEqual(JSON.parse(JSON.stringify(f.messages.at(-1))),{type:"copyProblemReference",reference:"review-run-test"});
 f.send({type:"revision",status:{...status,status:"running",waitingOn:null,retryCount:1}});assert.match(f.el("revision").textContent!,/Retrying automatically/);
 f.send({type:"revision",status:{...status,status:"running",waitingOn:null,retryCount:0}});assert.equal(f.doc.querySelector(".problem-details"),null);
 f.dom.window.close();
});


test("whole-document skeleton tracks work without replacing source and ends for review, failure and Read",()=>{
 const f=fixture();assert.equal(f.el("document-progress").hidden,false);
 f.document();const html=f.el("content").querySelector("p")!.innerHTML;
 const row={id:"whole",body:"Rewrite",anchor:{kind:"document",quote:"Entire document"}};
 f.send({type:"comments",rows:[row]});f.el("prepare-revision").click();
 assert.equal(f.el("document-progress").hidden,false);assert.equal(f.el("content").querySelector("p")!.innerHTML,html);
 for(const id of ["source","document-feedback","extend","prepare-revision"])assert.equal((f.el(id) as HTMLButtonElement).disabled,true,id);
 f.send({type:"revision",status:{status:"running",proposal:{comments:[row]}}});f.send({type:"revisionIdle"});
 const line=f.el("document-progress").firstChild;f.send({type:"revision",status:{status:"running",proposal:{comments:[row]}}});assert.equal(f.el("document-progress").firstChild,line,"polls do not restart the shimmer");
 f.send({type:"viewMode",mode:"reading"});assert.equal(f.el("document-progress").hidden,true);
 f.send({type:"viewMode",mode:"review"});assert.equal(f.el("document-progress").hidden,false);
 f.send({type:"revision",status:{status:"waiting",approvalId:"approve",proposal:{comments:[row]}}});assert.equal(f.el("document-progress").hidden,true);assert.equal(f.el("content").getAttribute("aria-busy"),"false");
 f.send({type:"revision",status:{status:"failed",proposal:{comments:[row]}}});assert.equal((f.el("source") as HTMLButtonElement).disabled,false);assert.equal((f.el("document-feedback") as HTMLButtonElement).disabled,false);
 f.dom.window.close();
});
test("passage skeleton uses exact selection rectangles and never changes adjacent text or source offsets",()=>{
 const f=fixture();f.document();f.select();f.el("selection-feedback").click();f.input("feedback","Clarify");f.el("add").click();
 const anchor=f.messages.find(m=>m.type==="comment").selection;f.send({type:"saved"});
 const measured:string[]=[];
 (f.dom.window.Range.prototype as any).getClientRects=function(){measured.push(this.toString());return [{left:40,top:70,width:100,height:18},{left:40,top:98,width:60,height:18}];};
 f.send({type:"comments",rows:[{id:"part",body:"Clarify",anchor}]});const html=f.el("content").querySelector("p")!.innerHTML;
 f.el("prepare-revision").click();
 assert.equal(f.el("document-progress").hidden,true);assert.equal(f.el("passage-progress").children.length,2);assert.ok(measured.every(text=>text==="this selection"));assert.equal(f.el("content").querySelector("p")!.innerHTML,html);
 assert.equal((f.el("selection-feedback") as HTMLButtonElement).disabled,true);assert.equal((f.el("selection-copy") as HTMLButtonElement).disabled,false);
 f.el("composer-close").click();f.doc.dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"m",ctrlKey:true,altKey:true}));assert.equal(f.el("composer").hidden,true,"shortcut also respects processing lock");
 f.send({type:"error",message:"Could not prepare"});assert.equal(f.el("passage-progress").children.length,0);assert.equal((f.el("source") as HTMLButtonElement).disabled,false);
 f.dom.window.close();
});
test("loading stops on human pauses and historical views and resumes only for active retry",()=>{
 const f=fixture();f.document("");
 const proposal={comments:[{id:"whole",anchor:{kind:"document"}}]};
 f.send({type:"revision",status:{status:"running",proposal}});assert.equal(f.el("document-progress").hidden,false);
 f.send({type:"revision",status:{status:"waiting",waitingOn:{kind:"question"},proposal}});assert.equal(f.el("document-progress").hidden,true);
 f.send({type:"revision",status:{status:"waiting",waitingOn:{kind:"retry"},proposal}});assert.equal(f.el("document-progress").hidden,false);
 f.send({type:"historyVersion",version:1,html:"<p>Earlier</p>"});assert.equal(f.el("document-progress").hidden,true);
 f.send({type:"historyCurrent"});assert.equal(f.el("document-progress").hidden,false);
 f.send({type:"revision",status:{status:"running",cancelRequested:true,proposal}});assert.equal(f.el("document-progress").hidden,true);
 f.dom.window.close();
});
test("reduced motion skips reveal animation and document polls do not animate unchanged content",()=>{
 const f=fixture();let animations=0;
 (f.dom.window.HTMLElement.prototype as any).animate=()=>{animations++;return {};};
 f.dom.window.matchMedia=(()=>({matches:true})) as any;f.document();assert.equal(animations,0);
 f.dom.window.matchMedia=(()=>({matches:false})) as any;f.document("New content",18);assert.equal(animations,1);f.document("New content",18);assert.equal(animations,1);
 assert.match(f.doc.querySelector("style")!.textContent!,/prefers-reduced-motion:reduce.*skeleton-line/s);f.dom.window.close();
});

test("partial draft remains visibly incomplete after a pause and disappears only after application",()=>{
 const f=fixture();f.document("");
 const draftParts=[{before:"",after:"# First section",html:"<h1>First section</h1>",complete:false}];
 f.send({type:"revision",status:{status:"running",draftParts,proposal:{comments:[{anchor:{kind:"document"}}]}}});
 assert.equal(f.el("draft-preview").hidden,false);assert.match(f.el("draft-caption").textContent!,/in progress/);assert.equal(f.el("document-progress").hidden,false);
 const part=f.el("draft-parts").firstChild;
 f.send({type:"revision",status:{status:"waiting",draftParts,proposal:{}}});assert.equal(f.el("draft-parts").firstChild,part);assert.match(f.el("draft-caption").textContent!,/Incomplete/);assert.equal(f.el("document-progress").hidden,true);
 f.send({type:"viewMode",mode:"reading"});assert.equal(f.el("draft-preview").hidden,true);
 f.send({type:"viewMode",mode:"review"});assert.equal(f.el("draft-preview").hidden,false);
 f.send({type:"revision",status:{status:"succeeded",draftParts,result:{applied:true},proposal:{}}});assert.equal(f.el("draft-preview").hidden,true);f.dom.window.close();
});
