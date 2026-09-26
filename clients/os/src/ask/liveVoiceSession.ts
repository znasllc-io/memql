import type { Room } from "livekit-client";
import type { AskTransport } from "./askController";
import type { AskActivity, ConversationSession } from "./conversationSession";

export interface LiveVoiceState {
 phase: "off" | "connecting" | "listening" | "transcribing" | "thinking" | "speaking";
 muted: boolean;
 needsPlayback: boolean;
 error: string;
 activity: AskActivity[];
 startedAt: string;
}
/** The sheet and widget observe one microphone, room and playback owner. */
export class LiveVoiceSession {
 private state: LiveVoiceState = { phase: "off", muted: false, needsPlayback: false, error: "", activity: [], startedAt: "" };
 private listeners = new Set<() => void>();
 private room: Room | null = null;
 private abort: AbortController | null = null;
 private audio = new Set<HTMLMediaElement>();
 private generation = 0;
 constructor(private transport: AskTransport, private conversation: ConversationSession) {}
 getSnapshot = () => this.state;
 subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
 private patch(patch: Partial<LiveVoiceState>) { this.state = { ...this.state, ...patch }; this.listeners.forEach(listener => listener()); }
 start = async (context: string | null, voice: "male" | "female") => {
  if (this.state.phase !== "off") return;
  if (!this.transport.startVoice) { this.patch({ error: "Live voice is unavailable in this window." }); return; }
  const generation = ++this.generation;
  const abort = new AbortController(); this.abort = abort;
  this.patch({ phase: "connecting", error: "", activity: [], startedAt: new Date().toISOString() });
  try {
   const conversationId = await this.conversation.beginVoice();
   if (generation !== this.generation) return;
   const credentials = await this.transport.startVoice({ conversationId, pageContext: context ?? "", voice }, abort.signal);
   if (generation !== this.generation) return;
   const { Room, RoomEvent, Track } = await import("livekit-client");
   if (generation !== this.generation) return;
   const room = new Room({ adaptiveStream: false, audioCaptureDefaults: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } });
   this.room = room;
   let connected = false;
   room.on(RoomEvent.TrackSubscribed, (track, _publication, participant) => {
    if (track.kind !== Track.Kind.Audio || participant.identity !== "memql") return;
    const element = track.attach(); element.hidden = true; document.body.append(element); this.audio.add(element);
   });
   room.on(RoomEvent.TrackUnsubscribed, track => { for (const element of track.detach()) { element.remove(); this.audio.delete(element); } });
   room.on(RoomEvent.AudioPlaybackStatusChanged, () => this.patch({ needsPlayback: !room.canPlaybackAudio }));
   room.on(RoomEvent.DataReceived, (data, participant) => {
    if (generation !== this.generation || participant?.identity !== "memql") return;
    try {
     const event = JSON.parse(new TextDecoder().decode(data));
     if (event.type === "state" && ["listening", "transcribing", "thinking", "speaking"].includes(event.state)) this.patch({ phase: event.state });
     if (event.type === "activity" && event.activity) this.patch({ activity: [...this.state.activity.slice(-99), event.activity] });
     if (event.type === "error") this.patch({ error: event.error ?? "Voice turn failed", phase: "listening" });
     this.conversation.voiceEvent(event);
    } catch { /* Non-Ask room data carries no instruction. */ }
   });
   // A failed connect emits Disconnected before rejecting. Let the catch below
   // preserve that error instead of invalidating the attempt and hiding it.
   room.on(RoomEvent.Disconnected, () => { if (connected && generation === this.generation) { this.stop(); this.patch({ error: "Voice connection ended. You can reconnect." }); } });
   await room.connect(credentials.url, credentials.token);
   connected = true;
   if (generation !== this.generation) { await room.disconnect(); return; }
   await room.localParticipant.setMicrophoneEnabled(true);
   if (generation !== this.generation) { await room.disconnect(); return; }
   await room.startAudio().catch(() => this.patch({ needsPlayback: true }));
   this.patch({ phase: "listening", muted: false });
  } catch (error) {
   if (generation !== this.generation) return;
   this.stop(); this.patch({ error: error instanceof Error ? error.message : String(error) });
  }
 };
 stop = () => {
  this.generation++; this.abort?.abort(); this.abort = null;
  const room = this.room; this.room = null;
  if (room) void room.disconnect();
  for (const element of this.audio) { element.pause(); element.srcObject = null; element.remove(); } this.audio.clear();
  this.patch({ phase: "off", muted: false, needsPlayback: false }); this.conversation.endVoice();
 };
 mute = async () => {
  const room = this.room; const generation = this.generation;
  if (!room) return;
  try { const enabled = this.state.muted; await room.localParticipant.setMicrophoneEnabled(enabled); if (generation === this.generation) this.patch({ muted: !enabled }); }
  catch (error) { if (generation === this.generation) this.patch({ error: String(error) }); }
 };
 enablePlayback = async () => {
  const room = this.room; const generation = this.generation; if (!room) return;
  try { await room.startAudio(); if (generation === this.generation) this.patch({ needsPlayback: !room.canPlaybackAudio }); }
  catch (error) { if (generation === this.generation) this.patch({ error: String(error), needsPlayback: true }); }
 };
 dispose = () => { this.stop(); this.listeners.clear(); };
}
