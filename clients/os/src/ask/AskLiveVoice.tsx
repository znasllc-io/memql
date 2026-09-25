import { useSyncExternalStore } from "react";
import { Mic, MicOff, PhoneOff, Volume2 } from "lucide-react";
import { Mark } from "../chrome/Mark";
import { AskWait } from "./AskWait";
import type { LiveVoiceSession } from "./liveVoiceSession";
export function AskLiveVoice({ session, errorsOnly = false }: { session: LiveVoiceSession; errorsOnly?: boolean }) {
 const state = useSyncExternalStore(session.subscribe, session.getSnapshot);
 if(errorsOnly) return state.error ? <p className="os-ask-error" role="alert">{state.error}</p> : null;
 return <div className="os-ask-live" data-state={state.phase}>
  <div className="os-ask-live-mark"><Mark size={72} /></div>
  <strong>MemQL</strong>
  <p role="status">{state.muted ? "Microphone muted" : ({off:"Voice ended",connecting:"Connecting…",listening:"Listening",transcribing:"Transcribing…",thinking:"Thinking…",speaking:"Speaking"})[state.phase]}</p>
  {state.phase === "transcribing" || state.phase === "thinking" ? <AskWait activity={state.activity} startedAt={state.startedAt} hasText={false} /> : null}
  {state.error ? <p className="os-ask-error" role="alert">{state.error}</p> : null}
  {state.needsPlayback ? <button type="button" className="os-button" onClick={() => void session.enablePlayback()}><Volume2 size={16} /> Enable sound</button> : null}
  <div className="os-ask-live-controls">
   <button type="button" className="os-ask-mic" aria-label={state.muted ? "Unmute microphone" : "Mute microphone"} disabled={state.phase === "connecting"} onClick={() => void session.mute()}>{state.muted ? <MicOff size={19} /> : <Mic size={19} />}</button>
   <button type="button" className="os-ask-live-end" aria-label="End voice conversation" onClick={session.stop}><PhoneOff size={19} /> End</button>
  </div>
  <span className="os-caption">Your conversation is saved. Speak to interrupt.</span>
 </div>;
}
