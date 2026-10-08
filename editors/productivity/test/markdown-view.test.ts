import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { markdownPage } from "../src/markdownPage.js";
import { renderMarkdown } from "../src/markdown.js";

function fixture(initial: any = {}) {
  const dom = new JSDOM(markdownPage("Review.md", "", "test"), {runScripts:"outside-only"});
  const messages: any[]=[];let state=initial;
  Object.assign(dom.window,{acquireVsCodeApi:()=>({postMessage:(message:any)=>messages.push(message),getState:()=>state,setState:(value:any)=>{state=value;}})});
  dom.window.eval(readFileSync("dist-test/markdown-view.js","utf8"));
  const doc=dom.window.document,el=(id:string)=>doc.getElementById(id)!;
  const send=(data:unknown)=>dom.window.dispatchEvent(new dom.window.MessageEvent("message",{data}));
  const document=(source="# Title\n\nKeep **this selection** and the rest.\n",version=17,connected=true)=>send({type:"document",html:renderMarkdown(source),version,sourceIdentity:source,connected,status:"Save first"});
  const select=(selector="strong")=>{const range=doc.createRange();range.selectNodeContents(doc.querySelector(selector)!);dom.window.getSelection()!.removeAllRanges();dom.window.getSelection()!.addRange(range);doc.dispatchEvent(new dom.window.Event("selectionchange"));};
  const input=(id:string,value:string)=>{(el(id) as HTMLTextAreaElement).value=value;el(id).dispatchEvent(new dom.window.Event("input"));};
  return {dom,doc,el,send,document,select,input,messages,state:()=>state};
}
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
  [...f.doc.querySelectorAll<HTMLButtonElement>("#review-actions button")].find(b=>b.textContent==="Approve & apply")!.click();assert.equal(f.messages.at(-1).approvalId,"exact-approval");assert.equal(f.messages.at(-1).decision,"approved");
  f.send({type:"revision",status:{...status,status:"succeeded",decision:"approved",result:{applied:true}}});f.send({type:"revisionIdle"});assert.match(f.el("revision").textContent!,/Changes applied/);f.dom.window.close();
});
test("selection choices survive refresh and views retain accessible keyboard controls",()=>{
  const f=fixture();f.document();const rows=[{id:"a",body:"One",anchor:{kind:"document-end"}},{id:"b",body:"Two",anchor:{kind:"document-end"}}];f.send({type:"comments",rows});(f.doc.querySelector('[role="switch"]') as HTMLInputElement).click();f.send({type:"comments",rows});assert.equal((f.doc.querySelector('[role="switch"]') as HTMLButtonElement).getAttribute("aria-checked"),"false");
  f.el("split").click();assert.equal(f.messages.at(-1).type,"split");f.send({type:"viewMode",mode:"split"});assert.equal(f.el("split").getAttribute("aria-pressed"),"true");f.el("source").dispatchEvent(new f.dom.window.KeyboardEvent("keydown",{key:"ArrowRight",bubbles:true}));assert.equal(f.doc.activeElement?.id,"reading");
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
  assert.ok([...f.doc.querySelectorAll<HTMLButtonElement>("#review-actions button")].some(button=>button.textContent==="Approve & apply"&&!button.disabled));f.dom.window.close();
});


test("section and selected-passage extensions preserve their precise starting locations",()=>{
  const f=fixture();f.document("# Guide\n\n## Examples\n\nKeep **this selection** and the rest.\n\n## Examples\n\nLater content.\n");
  const sections=f.doc.querySelectorAll<HTMLButtonElement>('button[aria-label="Extend section: Examples"]');
  sections[1].click();assert.equal(f.el("composer-title").textContent,"Extend section");f.input("feedback","Add an exercise");f.el("add").click();
  const section=f.messages.find(m=>m.type==="comment").selection;
  assert.equal(section.intent,"extend");assert.equal(section.scope,"section");assert.equal(section.startLine,6);assert.equal(section.quote,"Examples");assert.equal(section.endTextOffset,8);
  f.send({type:"saved"});f.select();f.el("selection-extend").click();assert.equal(f.el("composer-title").textContent,"Extend passage");f.input("feedback","Add a concrete example");f.el("add").click();
  const passage=f.messages.filter(m=>m.type==="comment").at(-1).selection;
  assert.equal(passage.intent,"extend");assert.equal(passage.scope,undefined);assert.equal(passage.quote,"this selection");assert.equal(passage.startTextOffset,5);assert.equal(passage.endTextOffset,19);
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
  const end=f.doc.querySelector<HTMLButtonElement>('.passage')!;end.focus();end.click();assert.equal(target,f.el("extend"));assert.equal(f.el("review-panel").hidden,false);assert.equal(f.doc.activeElement,end);
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
