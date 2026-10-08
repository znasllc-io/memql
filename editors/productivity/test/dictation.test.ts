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
