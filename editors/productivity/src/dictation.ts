import { reportProblem, UserInputError } from "./problems.js";
import * as vscode from 'vscode';
import type { Documents, OpenDocument } from './documents.js';

// Audio capture belongs to the browser host. Authentication and the shared Ask
// transcription stream stay in the core extension; no credential enters a view.
export class DocumentDictation {
  private active?: { id:string; panel:vscode.WebviewPanel; controller:ReadableStreamDefaultController<Uint8Array>; abort:AbortController; bytes:number; ended:boolean; base:OpenDocument };
  constructor(context:vscode.ExtensionContext, private readonly files:Documents) {
    context.subscriptions.push(vscode.commands.registerCommand('memql.productivity.dictationAudio',(message:any)=>{
      const session=this.active;if(!session||message?.id!==session.id||session.ended)return;
      if(message.ready){void session.panel.webview.postMessage({type:'dictation',phase:'listening'});return;}
      if(message.end){session.ended=true;session.controller.close();void session.panel.webview.postMessage({type:'dictation',phase:'transcribing'});return;}
      if(!Array.isArray(message.chunk)||message.chunk.length>65536||!message.chunk.every((n:unknown)=>typeof n==='number'&&Number.isInteger(n)&&n>=0&&n<=255)){this.failCapture(session,new Error('Invalid microphone audio.'));return;}
      session.bytes+=message.chunk.length;
      if(session.bytes>16000*2*300) {this.failCapture(session,new UserInputError('Dictation is limited to five minutes. Start a new recording to continue.'));return;}
      session.controller.enqueue(Uint8Array.from(message.chunk));
    }),{dispose:()=>this.cancel()});
  }
  async available():Promise<boolean>{return (await vscode.commands.getCommands(true)).includes('memql.editor.dictation.start');}
  async start(panel:vscode.WebviewPanel,base:OpenDocument):Promise<void>{
    if(this.active)throw new UserInputError('Finish the current dictation first.');
    if(!await this.available())throw new UserInputError('This VS Code host cannot capture a microphone. Open the document in the MemQL browser editor to dictate.');
    let controller!:ReadableStreamDefaultController<Uint8Array>;
    const audio=new ReadableStream<Uint8Array>({start(value){controller=value;},cancel:()=>{if(this.active?.controller===controller)this.active.ended=true;}});
    const session={id:globalThis.crypto.randomUUID(),panel,controller,abort:new AbortController(),bytes:0,ended:false,base};this.active=session;
    await panel.webview.postMessage({type:'dictation',phase:'starting'});
    const transcript=this.files.transcribe(base,audio,session.abort.signal,text=>{if(this.active===session&&!session.abort.signal.aborted)void panel.webview.postMessage({type:'dictation',phase:session.ended?'transcribing':'listening',text});});
    // Attach the rejection handler before waiting for a permission prompt.
    const finished=transcript.then(text=>({text}),error=>({error}));
    const capture=vscode.commands.executeCommand('memql.editor.dictation.start',session.id).then(()=>undefined,error=>{
      if(this.active===session&&!session.abort.signal.aborted)void panel.webview.postMessage({type:'dictation',phase:'idle',...dictationProblem(error,base)});
      session.abort.abort();
    });
    try {
      const result=await finished;
      if('error' in result)throw result.error;
      if(!session.abort.signal.aborted)await panel.webview.postMessage({type:'dictation',phase:'idle',text:result.text,final:true});
    } catch(error) {
      if(!session.abort.signal.aborted)await panel.webview.postMessage({type:'dictation',phase:'idle',...dictationProblem(error,base)});
    } finally {
      session.abort.abort();
      if(this.active===session)this.active=undefined;
      await Promise.resolve(vscode.commands.executeCommand('memql.editor.dictation.stop',session.id)).catch(error=>{reportProblem(error, 'stop dictation', base.lease);});
      void capture;
    }
  }
  private failCapture(session:NonNullable<DocumentDictation["active"]>,error:unknown):void {
    this.cancel(session.panel);
    void session.panel.webview.postMessage({type:'dictation',phase:'idle',...dictationProblem(error,session.base)});
  }
  async stop(panel:vscode.WebviewPanel):Promise<void>{if(this.active?.panel===panel)await vscode.commands.executeCommand('memql.editor.dictation.stop',this.active.id);}
  cancel(panel?:vscode.WebviewPanel):void {const session=this.active;if(!session||panel&&session.panel!==panel)return;session.abort.abort();if(!session.ended){session.ended=true;session.controller.close();}void Promise.resolve(vscode.commands.executeCommand('memql.editor.dictation.stop',session.id)).catch(error=>{reportProblem(error,'stop dictation',session.base.lease);});this.active=undefined;void session.panel.webview.postMessage({type:'dictation',phase:'idle'});}
}

function dictationProblem(error: unknown, base: OpenDocument) {
  const problem=reportProblem(error, "transcribe your feedback", base.lease);
  return {error:problem.message,reference:problem.reference};
}
