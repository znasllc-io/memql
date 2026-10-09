import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { DocumentDictation } from '../src/dictation.js';
import type { Documents, OpenDocument } from '../src/documents.js';

test('dictation routes PCM to the connected document and cancellation releases capture', {timeout:5000}, async () => {
  const handlers = new Map<string, (...args:any[])=>any>();
  const messages:any[]=[];
  let stopped=0, chunks:number[][]=[], activeID='';
  Object.assign(vscode.commands, {
    registerCommand:(name:string,handler:(...args:any[])=>any)=>{handlers.set(name,handler);return{dispose(){handlers.delete(name);}};},
    getCommands:async()=>['memql.editor.dictation.start'],
    executeCommand:async(name:string,id:string)=>{
      if(name.endsWith('.stop')){stopped++;return;}
      activeID=id;
      handlers.get('memql.productivity.dictationAudio')!({id,ready:true});
      handlers.get('memql.productivity.dictationAudio')!({id:'unrelated-session',chunk:[99]});
      handlers.get('memql.productivity.dictationAudio')!({id,chunk:[0,1,2,3]});
    },
  });
  const panel={webview:{postMessage:async(message:any)=>{messages.push(message);return true;}}} as vscode.WebviewPanel;
  const base={} as OpenDocument;
  let signal!:AbortSignal;
  const files={transcribe:async(document:OpenDocument,audio:ReadableStream<Uint8Array>,abort:AbortSignal,partial:(text:string)=>void)=>{
    assert.equal(document,base);signal=abort;
    const reader=audio.getReader();
    const first=await reader.read();chunks.push(Array.from(first.value!));partial('Clarify this');
    return new Promise<string>((_,reject)=>abort.addEventListener('abort',()=>{void reader.cancel();reject(new Error('cancelled'));},{once:true}));
  }} as Documents;
  const dictation=new DocumentDictation({subscriptions:[]} as unknown as vscode.ExtensionContext,files);
  assert.equal(await dictation.available(),true);
  const started=dictation.start(panel,base);
  while(!messages.some(m=>m.text))await new Promise(resolve=>setTimeout(resolve,0));
  dictation.cancel(panel);await started;
  assert.equal(signal.aborted,true);assert.ok(stopped>0);
  assert.deepEqual(chunks,[[0,1,2,3]]);
  assert.equal(messages.some(m=>m.error),false);
  assert.equal(messages.at(-1).phase,'idle');
  assert.doesNotThrow(()=>handlers.get('memql.productivity.dictationAudio')!({id:activeID,chunk:[5]}));
});


test('a dismissed old microphone prompt cannot interrupt a new dictation', {timeout:5000}, async () => {
 const messages:any[]=[];const captures:Array<{id:string;reject:(error:Error)=>void}>=[];
 Object.assign(vscode.commands,{
  registerCommand:()=>({dispose(){}}),getCommands:async()=>['memql.editor.dictation.start'],
  executeCommand:(name:string,id:string)=>name.endsWith('.stop')?Promise.resolve():new Promise<void>((_,reject)=>captures.push({id,reject})),
 });
 const panel={webview:{postMessage:async(message:any)=>{messages.push(message);return true;}}} as vscode.WebviewPanel;
 const files={transcribe:async(_base:OpenDocument,audio:ReadableStream<Uint8Array>,signal:AbortSignal)=>new Promise<string>((_,reject)=>signal.addEventListener('abort',()=>{void audio.cancel();reject(new Error('cancelled'));},{once:true}))} as Documents;
 const dictation=new DocumentDictation({subscriptions:[]} as unknown as vscode.ExtensionContext,files);
 const first=dictation.start(panel,{} as OpenDocument);
 while(captures.length<1)await new Promise(resolve=>setTimeout(resolve,0));
 dictation.cancel(panel);await first;
 const second=dictation.start(panel,{} as OpenDocument);
 while(captures.length<2)await new Promise(resolve=>setTimeout(resolve,0));
 const count=messages.length;captures[0].reject(new Error('Old permission prompt dismissed'));
 await new Promise(resolve=>setTimeout(resolve,0));assert.equal(messages.length,count);
 captures[1].reject(new Error('Microphone permission denied'));await second;
 assert.equal(messages.at(-1).phase,'idle');assert.equal(messages.at(-1).error,'Microphone permission denied');
});

test('unavailable speech releases capture, gives a recovery action, and permits retry', async () => {
 const messages:any[]=[];let stopped=0,attempts=0;
 Object.assign(vscode.commands,{
  registerCommand:()=>({dispose(){}}),getCommands:async()=>['memql.editor.dictation.start'],
  executeCommand:async(name:string)=>{if(name.endsWith('.stop'))stopped++;},
 });
 const panel={webview:{postMessage:async(message:any)=>{messages.push(message);return true;}}} as vscode.WebviewPanel;
 const files={transcribe:async()=>{
  if(attempts++===0)throw new Error('pushToTalk: failed to finalize streaming transcription: every_door_shut: no door [policy fastLocalFirst]; '+ 'provider HALF-CONFIGURED; '.repeat(80));
  return 'Add a concrete example.';
 }} as unknown as Documents;
 const dictation=new DocumentDictation({subscriptions:[]} as unknown as vscode.ExtensionContext,files);
 await dictation.start(panel,{} as OpenDocument);
 assert.equal(stopped,1);
 assert.match(messages.at(-1).error,/speech recognition model.*Fleet/);
 assert.ok(messages.at(-1).error.length<200);
 assert.equal(messages.some(m=>m.final),false);
 await dictation.start(panel,{} as OpenDocument);
 assert.equal(stopped,2);
 assert.equal(messages.at(-1).text,'Add a concrete example.');
 assert.equal(messages.at(-1).final,true);
});
