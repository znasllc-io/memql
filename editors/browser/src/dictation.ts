import { openMicrophone, browserMicPorts, type MicCapture } from '../../../clients/os/src/ask/micCapture';
import workletURL from '../../../clients/os/public/pcm16-worklet.js?url';

/** The top-level editor owns microphone permission; isolated webviews never do. */
export function registerDictation(api: typeof import('vscode')) {
  type CaptureSession = { id: string; mic?: MicCapture; stopped: boolean; timeout?: ReturnType<typeof setTimeout> };
  let active: CaptureSession | undefined;
  const stop = async (id?: string) => { if (!active || (id && active.id !== id)) return; active.stopped = true; if(active.timeout)clearTimeout(active.timeout); await active.mic?.stop(); };
  api.commands.registerCommand('memql.editor.dictation.available', () => !!navigator.mediaDevices?.getUserMedia);
  api.commands.registerCommand('memql.editor.dictation.stop', stop);
  api.commands.registerCommand('memql.editor.dictation.start', async (id: string) => {
    if (active || !/^[a-zA-Z0-9-]{16,80}$/.test(id)) throw new Error('Another dictation is already active.');
    const session = {id, stopped:false} as CaptureSession;active = session;
    try {
      session.mic = await openMicrophone({...browserMicPorts, workletPath:workletURL});
      if(session.stopped) { await session.mic.stop(); await api.commands.executeCommand('memql.productivity.dictationAudio',{id,end:true}); return; }
      session.timeout=setTimeout(()=>{void stop(id);},300000);
      await api.commands.executeCommand('memql.productivity.dictationAudio',{id,ready:true});
      const reader=session.mic.audio.getReader();
      for(;;){const {done,value}=await reader.read();if(done)break;await api.commands.executeCommand('memql.productivity.dictationAudio',{id,chunk:Array.from(value)});}
      await api.commands.executeCommand('memql.productivity.dictationAudio',{id,end:true});
    } finally { await session.mic?.stop();if(session.timeout)clearTimeout(session.timeout);if(active===session)active=undefined; }
  });
  window.addEventListener('pagehide',()=>{void stop();});
  document.addEventListener('visibilitychange',()=>{if(document.hidden)void stop();});
}
