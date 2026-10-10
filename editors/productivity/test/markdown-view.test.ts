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
  f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",result:{applied:true}}});f.send({type:"revisionIdle"});assert.equal(f.el("review-tab-history").getAttribute("aria-selected"),"true");assert.equal(f.el("review-footer").hidden,true);f.dom.window.close();
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
  assert.equal(f.el("review-tab-requests").getAttribute("aria-selected"),"true");
  assert.equal(f.doc.querySelector("#revision > details"),null);assert.doesNotMatch(f.el("comments").textContent!,/Earlier edit/);
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
 f.doc.querySelector<HTMLButtonElement>(".item-modify .primary")!.click();assert.equal(f.messages.at(-1).type,"modifyRevisionItem");assert.equal(f.messages.at(-1).instruction,"Research this claim first");f.dom.window.close();
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
 f.send({type:"revision",status:{status:"succeeded",decision:"approved",answer:{acceptedItemIds:["kept"]},items:[{id:"kept",edits:[{before:"Keep",after:"Retain"}]},{id:"omitted",edits:[{before:"rest",after:"other"}]}],proposal:{artifactId:"doc",revision:"v1",version:1,edits:[]},result:{applied:true}}});
 f.send({type:"history",data:{versions:[{version:2,current:true}]}});f.doc.querySelector<HTMLButtonElement>('[aria-label="Review version 2"]')!.click();
 assert.deepEqual({...f.state().decisions},{kept:"accepted",omitted:"declined"});
 assert.deepEqual([...f.doc.querySelectorAll('.item-decision')].map(el=>el.textContent),["Applied"]);f.dom.window.close();
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
 f.send({type:"viewMode",mode:"reading"});f.el("history-toggle").click();assert.equal(f.messages.at(-1).type,"history");assert.equal(f.el("review-panel").hidden,true);
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
 f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",result:{applied:true}}});assert.equal(f.doc.querySelector("#revision .review-error"),null);assert.equal(f.el("review-tab-history").getAttribute("aria-selected"),"true");assert.equal(f.el("review-footer").hidden,true);f.dom.window.close();
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
test("end extensions reserve their own placeholder until processing ends, without masking saved text",()=>{
 const f=fixture();f.document();const original=f.el("content").querySelector("p")!.innerHTML;
 const row={id:"extension",body:"Add next steps",anchor:{kind:"document-end",quote:"End of document"}};
 const proposal={comments:[row]};
 const extension=f.el("extension-progress");
 assert.equal(extension.hidden,true);
 f.send({type:"comments",rows:[row]});f.el("prepare-revision").click();
 assert.equal(extension.hidden,false,"reserve space as soon as preparation starts");
 assert.equal(f.el("passage-progress").children.length,0,"append placeholders are not positioned over the page");
 assert.ok(extension.compareDocumentPosition(f.el("extend")) & f.dom.window.Node.DOCUMENT_POSITION_FOLLOWING);
 assert.equal((f.el("extend") as HTMLButtonElement).disabled,true);
 f.send({type:"revision",status:{status:"running",proposal}});f.send({type:"revisionIdle"});
 const first=extension.firstChild;
 f.send({type:"revision",status:{status:"running",proposal}});assert.equal(extension.firstChild,first,"polls preserve shimmer nodes");
 f.send({type:"viewMode",mode:"reading"});assert.equal(extension.hidden,true);
 f.send({type:"viewMode",mode:"review"});assert.equal(extension.hidden,false);
 f.send({type:"historyVersion",version:1,html:"<p>Earlier</p>"});assert.equal(extension.hidden,true);
 f.send({type:"historyCurrent"});assert.equal(extension.hidden,false);
 for(const end of [{status:"waiting",approvalId:"approve"},{status:"waiting",waitingOn:{kind:"question"}},{status:"failed"},{status:"cancelled"},{status:"succeeded"}]){
  f.send({type:"revision",status:{status:"running",proposal}});assert.equal(extension.hidden,false);
  f.send({type:"revision",status:{...end,proposal}});assert.equal(extension.hidden,true,"release space on review, pause or completion");
 }
 f.send({type:"revision",status:{status:"running",proposal:{comments:[row,{id:"whole",anchor:{kind:"document"}}]}}});
 assert.equal(extension.hidden,true,"whole-document preparation does not duplicate the extension skeleton");
 assert.equal(f.el("content").querySelector("p")!.innerHTML,original,"the saved document is untouched");
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
test("passage loading masks only its annotations and restores them for review, pauses and failure",()=>{
 const f=fixture();f.document();f.select();f.el("selection-feedback").click();f.input("feedback","Clarify");f.el("add").click();
 const anchor={...f.messages.find(m=>m.type==="comment").selection,kind:"markdown"};f.send({type:"saved"});
 const row={id:"part",body:"Clarify",anchor};
 const other={id:"other",body:"Keep this request",anchor:{...anchor,startTextOffset:0,endTextOffset:4,quote:"Keep"}};
 f.send({type:"comments",rows:[row,other]});f.send({type:"notes",rows:[{...row,id:"note",body:"Personal note"}]});
 const proposal={comments:[row],commentIds:[row.id]};
 const maskedText=()=>[...(f.highlights.get("memql-processing")??[])].map(range=>range.toString());
 for(const end of [{status:"waiting",approvalId:"approval"},{status:"waiting",waitingOn:{kind:"question"}},{status:"failed"},{status:"cancelled"},{status:"running",cancelRequested:true}]){
  f.send({type:"revision",status:{status:"running",proposal}});
  assert.deepEqual(maskedText(),["this selection"],"only the busy passage is masked, including after a retry");
  const mask=f.highlights.get("memql-processing")!;
  assert.ok((mask as any).priority>(f.highlights.get("memql-feedback") as any).priority);
  f.send({type:"revision",status:{status:"running",proposal}});assert.equal(f.highlights.get("memql-processing"),mask,"unchanged polls keep the same layer");
  f.send({type:"revision",status:{...end,proposal}});
  assert.equal(f.highlights.has("memql-processing"),false);
  assert.deepEqual([...f.highlights.get("memql-feedback")!].map(range=>range.toString()),["this selection","Keep"]);
  assert.equal([...f.highlights.get("memql-notes")!][0].toString(),"this selection");
 }
 f.send({type:"revision",status:{status:"running",proposal}});
 f.send({type:"viewMode",mode:"reading"});assert.deepEqual(maskedText(),[]);
 f.send({type:"viewMode",mode:"review"});assert.deepEqual(maskedText(),["this selection"]);
 f.send({type:"historyVersion",version:1,html:"<p>Earlier</p>"});assert.deepEqual(maskedText(),[]);
 f.send({type:"historyCurrent"});assert.deepEqual(maskedText(),["this selection"]);
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

test("feedback attachments survive reload, stay with their anchor and clear only after a saved receipt", () => {
  const ref={artifactId:"ref",version:1,revision:"file:1",name:"Evidence.md",mimeType:"text/markdown",size:40,uri:"memql-file://cluster/artifacts/ref/Evidence.md"};
  const f=fixture({draft:"Use the attached reference",anchor:{kind:"document",quote:"Entire document"},attachments:[ref]});f.document();
  f.el("document-feedback").click();
  assert.match(f.el("attachments").textContent??"",/Evidence.md/);
  f.el("extend").click();f.el("add").click();
  const posted=f.messages.find(m=>m.type==="comment");assert.equal(posted.attachments[0].artifactId,"ref");assert.equal(posted.selection.kind,"document");
  f.send({type:"error",message:"Try again"});assert.equal(f.state().attachments.length,1);assert.equal((f.el("feedback") as HTMLTextAreaElement).value,"Use the attached reference");
  f.send({type:"saved"});assert.equal(f.state().attachments.length,0);assert.equal(f.el("attachments").childElementCount,0);f.dom.window.close();
});
test("attachment uploads disable submission, preserve feedback on failure and remove without deleting files",async()=>{
  const f=fixture();f.document();f.el("document-feedback").click();f.input("feedback","Use this image");
  const input=f.el("attachment-input") as HTMLInputElement;
  Object.defineProperty(input,"files",{configurable:true,value:[new f.dom.window.File(["# reference"],"reference.md",{type:"text/markdown"})]});
  input.dispatchEvent(new f.dom.window.Event("change"));assert.equal((f.el("add") as HTMLButtonElement).disabled,true);
  for(let n=0;n<30&&!f.messages.some(m=>m.type==="uploadAttachment");n++)await new Promise(resolve=>setTimeout(resolve,5));
  const upload=f.messages.find(m=>m.type==="uploadAttachment");assert.ok(upload);assert.equal(f.state().draft,"Use this image");
  f.send({type:"attachmentFailed",uploadId:upload.uploadId,message:"Upload failed"});assert.equal((f.el("add") as HTMLButtonElement).disabled,false);assert.equal(f.state().draft,"Use this image");
  input.dispatchEvent(new f.dom.window.Event("change"));
  for(let n=0;n<30&&f.messages.filter(m=>m.type==="uploadAttachment").length<2;n++)await new Promise(resolve=>setTimeout(resolve,5));
  const next=f.messages.filter(m=>m.type==="uploadAttachment").at(-1);
  f.send({type:"attachmentUploaded",uploadId:next.uploadId,attachment:{artifactId:"ref",version:1,revision:"file:1",name:"reference.md",mimeType:"text/markdown",size:11,uri:"memql-file://cluster/artifacts/ref/reference.md"}});
  assert.equal(f.state().attachments.length,1);f.el("attachments").querySelector<HTMLButtonElement>("button")!.click();assert.equal(f.state().attachments.length,0);assert.equal(f.messages.some(m=>m.type==="deleteFile"),false);f.dom.window.close();
});

test("feedback and extensions delete only after confirmation and a server receipt",()=>{
  const f=fixture();f.document();
  const anchor={kind:"markdown",startLine:2,endLine:3,startBlock:1,endBlock:1,startTextOffset:5,endTextOffset:19,quote:"this selection"};
  const rows=[{id:"feedback",body:"Clarify",anchor,canRemove:true},{id:"extension",body:"Add references",anchor:{kind:"document-end",quote:"End of document"},canRemove:true}];
  f.send({type:"comments",rows});
  f.doc.querySelector<HTMLButtonElement>('[aria-label="Delete feedback"]')!.click();
  assert.equal(f.messages.some(m=>m.type==="removeAnnotation"),false);
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="keep:feedback"]')!.click();
  assert.equal(f.doc.activeElement?.getAttribute("aria-label"),"Delete feedback");
  f.doc.querySelector<HTMLButtonElement>('[aria-label="Delete extension request"]')!.click();
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="confirm-delete:extension"]')!.click();
  assert.equal(f.messages.at(-1).type,"removeAnnotation");assert.equal(f.messages.at(-1).id,"extension");
  assert.ok(f.el("comments").textContent?.includes("Add references"),"kept until confirmed");
  assert.equal((f.el("prepare-revision") as HTMLButtonElement).disabled,true);
  f.send({type:"annotationRemoveError",message:"Connection lost. Try again."});
  assert.match(f.el("comments").textContent!,/Connection lost/);
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="confirm-delete:extension"]')!.click();
  f.send({type:"annotationRemoved",id:"extension",purpose:"feedback"});
  assert.equal(f.el("comments").textContent?.includes("Add references"),false);
  assert.deepEqual(Array.from(f.state().included),["feedback"]);
  f.select();f.send({type:"annotationRemoved",id:"feedback",purpose:"feedback"});
  assert.equal(f.dom.window.getSelection()?.toString(),"");
  assert.equal(f.highlights.get("memql-active")?.size??0,0);
  assert.equal((f.el("prepare-revision") as HTMLButtonElement).disabled,true);
  f.dom.window.close();
});
test("deleting a proposed request hides its obsolete proposal and keeps other requests",()=>{
  const f=fixture();f.document();
  const row={id:"one",body:"Clarify",canRemove:true,anchor:{kind:"document",quote:"Entire document"}};
  const next={...row,id:"two",body:"Add sources"};
  f.send({type:"comments",rows:[row,next]});
  f.send({type:"revision",status:{runId:"run",status:"waiting",approvalId:"approval",proposal:{commentIds:["one","two"],comments:[row,next],edits:[{before:"this selection",after:"a clear selection",commentIds:["one"]}]}}});
  const control=f.el("revision").querySelector<HTMLButtonElement>('[aria-label="Delete feedback"]')!;control.click();
  assert.match(f.el("revision").textContent!,/stop this proposal/);
  // Polling must preserve confirmation and keyboard focus.
  f.send({type:"revision",status:{runId:"run",status:"waiting",approvalId:"approval",proposal:{commentIds:["one","two"],comments:[row,next],edits:[{before:"this selection",after:"a clear selection",commentIds:["one"]}]}}});
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="confirm-delete:one"]')!.click();
  assert.equal(f.messages.at(-1).runId,"run");
  f.send({type:"annotationRemoved",id:"one",purpose:"feedback"});
  assert.equal(f.doc.querySelector(".change"),null);
  assert.match(f.el("comments").textContent!,/Add sources/);
  assert.equal((f.el("prepare-revision") as HTMLButtonElement).disabled,false);
  f.el("prepare-revision").click();assert.deepEqual(Array.from(f.messages.at(-1).commentIds),["two"]);
  f.dom.window.close();
});
test("personal notes delete in read mode without touching feedback or other note markers",()=>{
  const f=fixture();f.document();f.send({type:"viewMode",mode:"reading"});
  const row={id:"note",body:"A private thought",canRemove:true,anchor:{kind:"markdown",startLine:2,endLine:3,startBlock:1,endBlock:1,startTextOffset:5,endTextOffset:19,quote:"this selection"}};
  f.send({type:"notes",rows:[row,{...row,id:"other",body:"Another thought"}]});
  f.el("note-markers").querySelector<HTMLButtonElement>("button")!.click();
  f.doc.querySelector<HTMLButtonElement>('[aria-label="Delete note"]')!.click();
  const keep=f.doc.querySelector<HTMLButtonElement>('[data-focus-key="keep:note"]')!;
  keep.focus();f.send({type:"notes",rows:[row,{...row,id:"other",body:"Another thought"}]});
  assert.equal(f.doc.activeElement,keep,"unchanged note polling must preserve confirmation focus");
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="confirm-delete:note"]')!.click();
  assert.equal(f.messages.at(-1).purpose,"note");
  f.send({type:"annotationRemoved",id:"note",purpose:"note"});
  assert.equal(f.el("notes-panel").hidden,false);
  assert.equal(f.doc.querySelectorAll(".personal-note").length,1);
  assert.equal(f.highlights.get("memql-notes")?.size,1);
  f.send({type:"annotationRemoved",id:"other",purpose:"note"});
  assert.equal(f.highlights.get("memql-notes")?.size??0,0);assert.equal(f.el("note-markers").children.length,0);
  f.dom.window.close();
});

test("applied feedback and extensions lose Delete even before refreshed permissions arrive",()=>{
  const f=fixture();f.document();
  const feedback={id:"feedback",body:"Clarify",canRemove:true,anchor:{kind:"document",quote:"Entire document"}};
  const extension={...feedback,id:"extension",body:"Add sources",anchor:{kind:"document-end",quote:"End of document"}};
  const declined={...feedback,id:"declined",body:"Try another title"};
  const items=[feedback,extension,declined].map(row=>({id:`item-${row.id}`,commentIds:[row.id],edits:[{before:"this selection",after:row.body,commentIds:[row.id]}]}));
  const proposal={commentIds:[feedback.id,extension.id,declined.id],comments:[feedback,extension,declined],edits:items.flatMap(item=>item.edits)};
  f.send({type:"comments",rows:[feedback,extension,declined]});
  f.send({type:"revision",status:{runId:"run",status:"waiting",approvalId:"approval",proposal,items}});
  f.doc.querySelector<HTMLButtonElement>('[data-focus-key="delete:feedback"]')!.click();
  assert.ok(f.doc.querySelector('[data-focus-key="confirm-delete:feedback"]'));
  f.send({type:"revision",status:{runId:"run",status:"succeeded",decision:"approved",result:{applied:true},answer:{acceptedItemIds:["item-feedback","item-extension"]},proposal,items}});
  assert.equal(f.doc.querySelector('[data-focus-key="confirm-delete:feedback"]'),null,"close stale confirmation");
  for(const id of ["feedback","extension"])assert.equal(f.doc.querySelector(`[data-focus-key="delete:${id}"]`),null,id);
  assert.ok(f.doc.querySelector('[data-focus-key="delete:declined"]'),"declined request remains removable");
  // A newer review and an old selection do not undo the server's applied flag.
  f.send({type:"comments",rows:[{...feedback,applied:true,canRemove:false,outdated:true},{...extension,applied:true,canRemove:false,outdated:true},{...declined,outdated:true}]});
  f.send({type:"revision",status:{runId:"new-run",status:"running",proposal:{commentIds:[]}}});
  for(const id of ["feedback","extension"])assert.equal(f.doc.querySelector(`[data-focus-key="delete:${id}"]`),null,id);
  assert.ok(f.doc.querySelector('[data-focus-key="delete:declined"]'));
  f.send({type:"notes",rows:[{...feedback,id:"note",body:"Personal thought"}]});
  assert.ok(f.doc.querySelector('[data-focus-key="delete:note"]'));
  f.dom.window.close();
});

test("review tabs separate requests, proposals and saved history without losing choices",()=>{
 const f=fixture();f.document();
 const current={id:"new",body:"Clarify this",anchor:{kind:"document"}};
 const applied={id:"old",body:"Already applied",applied:true,outdated:true,anchor:{kind:"document"}};
 const stale={id:"stale",body:"Not done",outdated:true,canRemove:true,anchor:{kind:"document-end"}};
 f.send({type:"comments",rows:[current,applied,stale]});
 assert.doesNotMatch(f.el("comments").textContent!,/Already applied|Earlier requests/);
 assert.match(f.el("comments").textContent!,/Not applied/);
 f.el("prepare-revision").click();assert.equal(f.el("review-tab-changes").getAttribute("aria-selected"),"true");
 const status={status:"waiting",approvalId:"approval",proposal:{version:1,commentIds:[current.id],comments:[current],edits:[{before:"Old",after:"Clear",reason:"Precise",commentIds:[current.id]}]}};
 f.send({type:"revision",status});f.send({type:"revisionIdle"});
 f.doc.querySelector<HTMLButtonElement>(".item-actions button")!.click();
 const reason=f.doc.querySelector<HTMLDetailsElement>(".change-reason")!;reason.open=true;
 f.el("review-tab-requests").click();assert.equal(f.el("review-footer").hidden,true);assert.doesNotMatch(f.el("comments").textContent!,/Clarify this/);
 f.send({type:"revision",status:{...status,retryCount:0}});
 assert.equal(f.el("review-tab-requests").getAttribute("aria-selected"),"true","polling must not change tabs");
 f.el("review-tab-changes").click();assert.equal(f.doc.querySelector<HTMLDetailsElement>(".change-reason")!.open,true);
 assert.ok(Object.values(f.state().decisions).includes("accepted"));
 assert.equal(f.el("review-footer").hidden,false);f.dom.window.close();
});

test("history tabs support keyboard navigation, paging, saved comparisons and recoverable failures",()=>{
 const f=fixture();f.send({type:"document",html:renderMarkdown("Current text"),sourceIdentity:"Current text",version:4,connected:true,historyAvailable:true});
 f.el("review-tab-requests").dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"End",bubbles:true}));
 assert.equal(f.doc.activeElement?.id,"review-tab-history");assert.equal(f.messages.at(-1).type,"history");
 assert.equal(f.el("review-history-content").getAttribute("aria-busy"),"true");assert.ok(f.doc.querySelector(".review-skeleton"));
 f.send({type:"history",data:{versions:[{version:3,current:true,note:"Expanded examples",authorKind:"assistant"}],hasMore:true,beforeVersion:3}});f.send({type:"historyIdle"});
 const row=()=>f.doc.querySelector<HTMLButtonElement>('[aria-label="Review version 3"]')!;row().click();
 assert.equal(f.doc.querySelectorAll(".review-history-row").length,0,"detail replaces list");
 const compare=()=>[...f.el("review-history-content").querySelectorAll<HTMLButtonElement>("button")].find(b=>b.textContent==="Compare with previous")!;
 compare().click();assert.equal(f.messages.at(-1).type,"historyCompare");assert.equal(f.messages.at(-1).version,3);
 f.send({type:"historyError",message:"Previous version is unavailable",reference:"tools-test"});f.send({type:"historyIdle"});
 assert.match(f.el("review-history-content").textContent!,/Expanded examples/);assert.match(f.el("review-history-status").textContent!,/Previous version/);
 f.el("review-history-status").querySelector<HTMLButtonElement>("button.passage")!.click();assert.equal(f.messages.at(-1).type,"historyCompare");f.send({type:"historyIdle"});
 f.doc.querySelector<HTMLButtonElement>(".review-history-back")!.click();assert.equal(f.doc.activeElement,row());
 [...f.el("review-history-content").querySelectorAll<HTMLButtonElement>("button")].find(b=>b.textContent==="Load earlier versions")!.click();assert.equal(f.messages.at(-1).beforeVersion,3);
 f.send({type:"history",append:true,data:{versions:[{version:1,note:"First retained snapshot"}],hasMore:false}});f.send({type:"historyIdle"});assert.equal(f.doc.querySelectorAll(".review-history-row").length,2);
 f.doc.querySelector<HTMLButtonElement>('[aria-label="Review version 1"]')!.click();assert.doesNotMatch(f.el("review-history-content").textContent!,/Compare with previous/,"do not offer a comparison to an unretained version zero");
 f.el("review-tab-history").dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"Home",bubbles:true}));assert.equal(f.doc.activeElement?.id,"review-tab-requests");f.dom.window.close();
});

test("completed proposals become history and retain only the actually applied comparison",()=>{
 const f=fixture();f.send({type:"document",html:renderMarkdown("Old"),sourceIdentity:"Old",version:1,connected:true,historyAvailable:true});
 const rows=[{id:"kept",body:"Clarify",versionNumber:1,anchor:{kind:"document"}},{id:"omitted",body:"Extend",versionNumber:1,anchor:{kind:"document-end"}}];
 f.send({type:"comments",rows});
 const status={runId:"run",status:"waiting",approvalId:"approval",proposal:{version:1,summary:"Clarify wording",commentIds:rows.map(r=>r.id),comments:rows},items:[{id:"a",commentIds:["kept"],edits:[{before:"Old",after:"Clear",reason:"Clarity"}]},{id:"b",commentIds:["omitted"],edits:[{before:"More",after:"Much more"}]}]};
 f.send({type:"revision",status});
 assert.equal(f.el("requests-count").hidden,true,"captured requests are counted only in Changes");
 f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",answer:{acceptedItemIds:["a"]},result:{applied:true}}});
 assert.equal(f.el("review-tab-history").getAttribute("aria-selected"),"true");assert.equal(f.el("review-footer").hidden,true);
 f.send({type:"history",data:{versions:[{version:2,current:true,note:"Clarify"}]}});f.send({type:"historyIdle"});
 assert.match(f.doc.querySelector<HTMLButtonElement>('[aria-label="Review version 2"]')!.textContent!,/1 change was applied/);
 f.doc.querySelector<HTMLButtonElement>('[aria-label="Review version 2"]')!.click();
 assert.match(f.el("review-history-content").textContent!,/BeforeOldAfterClear/);assert.doesNotMatch(f.el("review-history-content").textContent!,/Much more|Accept|Delete/);
 assert.match(f.el("review-history-content").textContent!,/1 change was applied from this review/);
 const why=f.doc.querySelector<HTMLDetailsElement>("#review-history-content .change-reason")!;why.open=true;
 f.send({type:"historyIdle"});assert.equal(f.doc.querySelector<HTMLDetailsElement>("#review-history-content .change-reason")!.open,true);
 f.el("review-tab-requests").click();assert.doesNotMatch(f.el("comments").textContent!,/Clarify/);assert.match(f.el("comments").textContent!,/Extend/);
 assert.ok(!Array.from(f.state().included).includes("kept"));
 f.el("history-toggle").click();assert.equal(f.el("review-tab-history").getAttribute("aria-selected"),"true");assert.equal(f.el("history-panel").hidden,true);
 [...f.el("review-history-content").querySelectorAll<HTMLButtonElement>("button")].find(b=>b.textContent==="Open saved version")!.click();
 assert.equal(f.messages.at(-1).type,"historyVersion");assert.equal(f.el("history-panel").hidden,false);
 f.send({type:"historyVersion",version:2,html:renderMarkdown("Clear")});f.send({type:"historyIdle"});
 f.send({type:"historyCurrent"});assert.equal(f.el("history-panel").hidden,true);assert.equal(f.el("review-panel").hidden,false);assert.equal(f.el("review-tab-history").getAttribute("aria-selected"),"true");f.dom.window.close();
});
