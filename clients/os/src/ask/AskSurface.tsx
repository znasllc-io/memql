import { ContentSkeleton } from "../kit/ContentSkeleton";
import { Mark } from "../chrome/Mark";
import { AccountAvatar } from "../chrome/AccountAvatar";
import { AskLiveVoice } from "./AskLiveVoice";
import type { LiveVoiceSession } from "./liveVoiceSession";
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore, type FormEvent } from "react";
import { ArrowUp, Mic, Plus, X, Activity, MessageCircle, MessagesSquare, Maximize2, Minimize2, Square, AudioLines } from "lucide-react";

import { conversationFeed } from "./conversationFeed";
import { ConversationSession } from "./conversationSession";
import { AskWait } from "./AskWait";
import { AskWorkQueue } from "./AskWorkQueue";
import { AskActivityLog } from "./AskActivityLog";
import { AskConversations } from "./AskConversations";
import { AskMessage } from "./AskMessage";
import type { AskTransport } from "./askController";
import { CHECKING_ASK, type AskAvailability } from "./useAskReadiness";
import { useReducedMotion, useVoice } from "./useVoice";
import type { VoicePorts, VoiceProblem, VoiceState } from "./voiceSession";
import { DEFAULT_ASK_SETTINGS, type AskSettings } from "../apps/settings/askSettings";

// THE Ask surface (spec C): one component behind all three entry points.
// Input row (text, mic toggle, send), streamed answer log, context chip.
// Errors render here in the surface's own voice -- never a toast.
//
// ===========================================================================
// VOICE (epic memql#4747)
// ===========================================================================
// The mic control's GEOMETRY does not change now that voice is real -- spec C
// promised that when it shipped the button inert, and it is the reason the
// level indicator is a ring ON the existing 30px circle rather than a meter
// beside it. It also puts voice in the shell's existing cue language: the
// arrival cue is "a box-shadow, never a background", and so is this.
//
// The level never enters React state. It moves at the frame rate, and putting
// it in state would re-render the streaming answer log sixty times a second
// to animate one ring; it is written to a CSS custom property on the button
// instead, from a rAF loop that only runs while the mic is live.


/** What the caption says, per phase. Exported so the tests read the copy. */
export const ASK_VOICE_HOLD = "Listening -- let go to send.";
export const ASK_VOICE_LATCHED = "Listening -- tap the mic when you are done.";
export const ASK_VOICE_HOLD_REVIEW = "Listening -- let go to put it in the box.";
export const ASK_VOICE_LATCHED_REVIEW = "Listening -- tap the mic to put it in the box.";
export const ASK_VOICE_FINISHING = "Finishing the transcript.";
/**
 * Rendered when this window was built with no voice wiring at all.
 *
 * The control stays PRESENT and disabled rather than disappearing: the row's
 * geometry is the same row every other window shows, and a control that is
 * missing in one place and present in another is a bug report. A disabled
 * control with no account of itself is the thing this shell does not do, so
 * the sentence stands under it rather than hiding in a tooltip.
 */
export const ASK_VOICE_UNAVAILABLE = "Voice is not wired up in this window. Typing works.";

/**
 * A refusal, in the surface's own voice: what happened, then what to do.
 *
 * `denied` covers both a person saying no and a Permissions-Policy that
 * forbids the page from asking -- the browser reports them identically -- so
 * the sentence names the browser rather than accusing the reader of a choice
 * they may not have made.
 */
export function voiceProblemSentence(problem: VoiceProblem): string {
  switch (problem.kind) {
    case "denied":
      return "The browser is blocking the microphone for this site. Allow it in the address bar, then press the mic again.";
    case "no-device":
      return "No microphone is connected.";
    case "device-busy":
      return "Another app is using the microphone. Close it and try again.";
    case "unsupported":
      return "This browser cannot record audio. Typing works.";
    default:
      // The server's own sentence names the fix -- "streaming transcription
      // is not configured" is what a cluster answers when no node is serving
      // it, and a friendlier paraphrase would drop the one fact that helps.
      return problem.message;
  }
}

export function AskSurface({
  transport,
  conversation: providedConversation,
  liveVoice,
  onOpenFile,
  onOpenWork,
  onOpenFleet,
  onClose,
  onToggleMaximize,
  maximized = false,
  voicePorts = null,
  settings = DEFAULT_ASK_SETTINGS,
  context = null,
  contextLabel,
  variant,
  autoFocus = false,
  availability = CHECKING_ASK,
  draft: providedDraft,
  onDraftChange,
}: {
  transport: AskTransport;
  conversation?: ConversationSession;
  liveVoice?: LiveVoiceSession;
  onClose?: () => void;
  onToggleMaximize?: () => void;
  maximized?: boolean;
  availability?: AskAvailability;
  onOpenFleet?: () => void;
  draft?: string;
  onDraftChange?: (draft: string) => void;
  /** Absent = this window has no voice wiring; the control says so. */
  voicePorts?: VoicePorts | null;
  settings?: AskSettings;
  context?: string | null;
  contextLabel?: string;
  variant: "sheet" | "widget";
  autoFocus?: boolean;
  onOpenFile?: (fileId: string) => void;
  onOpenWork?: (runId?: string, conversationId?: string) => void;
}) {
  const localConversation = useRef<ConversationSession | null>(null);
  if (!providedConversation && !localConversation.current) localConversation.current = new ConversationSession(transport);
  const conversation = providedConversation ?? localConversation.current!;
  const state = useSyncExternalStore(conversation.subscribe, conversation.getSnapshot);
  const [panel, setPanel] = useState<"conversation" | "activity" | "conversations">("conversation");
  const showActivity = panel === "activity";
  const showConversations = panel === "conversations";
  const activityRef = useRef<HTMLButtonElement | null>(null);
  const conversationsRef = useRef<HTMLButtonElement | null>(null);
  const closePanel = useCallback(() => {
    setPanel("conversation");
    (panel === "conversations" ? conversationsRef : activityRef).current?.focus();
  }, [panel]);
  const returnToComposer = useRef(false);
  useEffect(() => {
    if (panel === "conversation" && returnToComposer.current) {
      returnToComposer.current = false;
      inputRef.current?.focus();
    }
  });
  const draft = providedDraft ?? state.draft;
  const setDraft = onDraftChange ?? conversation.setDraft;
  const exchanges = state.turns;
  const busy = state.busy;
  const ready = availability.state === "ready";
  const nearBottom = useRef(true);
  const inputRef = useRef<HTMLTextAreaElement | null>(null);
  const logRef = useRef<HTMLDivElement | null>(null);
  const conversationScroll = useRef(0);
  useLayoutEffect(() => {
    const el = logRef.current;
    if (el) el.scrollTop = nearBottom.current ? el.scrollHeight : conversationScroll.current;
  }, [panel, maximized]);
  const micRef = useRef<HTMLButtonElement | null>(null);
  const reducedMotion = useReducedMotion();

  const voice = useVoice(voicePorts, {
    onTranscript: (text) => setDraft(text),
    onActivity: conversation.recordDictation,
    onUtterance: (text) => {
      if (settings.commit === "send") {
        setDraft(send(text) ? "" : text);
      } else {
        setDraft(text);
        inputRef.current?.focus();
      }
    },
  });
  const controls = voice?.controls ?? null;
  useEffect(() => { if (state.voiceActive) controls?.cancel(); }, [state.voiceActive, controls]);
  const phase = voice?.state.phase ?? "idle";
  const live = phase === "listening" || phase === "transcribing";

  useEffect(() => {
    if (autoFocus) inputRef.current?.focus();
    return () => { if (!providedConversation) localConversation.current?.dispose(); };
  }, [autoFocus]);

  useEffect(() => {
    if (availability.state === "disconnected") {
      conversation.detach("Connection to the cluster was lost. Completed actions remain in Activity.");
    }
  }, [availability.state]);

  useEffect(() => {
    if (nearBottom.current) logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [exchanges]);

  // The level ring. Runs only while the mic is live, writes only a CSS
  // variable. Reduced motion holds it at a readable constant rather than
  // dropping the cue: "no animation" would leave those readers with no way to
  // tell a live microphone from a dead one, which is a different failure from
  // the one the setting asks about.
  useEffect(() => {
    const el = micRef.current;
    if (!el || !controls) return;
    if (phase !== "listening") {
      el.style.setProperty("--os-mic-level", "0");
      return;
    }
    if (reducedMotion) {
      el.style.setProperty("--os-mic-level", "0.55");
      return;
    }
    let frame = 0;
    const tick = () => {
      el.style.setProperty("--os-mic-level", controls.level().toFixed(3));
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [phase, reducedMotion, controls]);

  // Esc unwinds one layer at a time: it stops a live utterance, and only a
  // second press closes the sheet. Capture phase so it runs before AskSheet's
  // own window listener, which would otherwise close the sheet out from under
  // a person who only meant to stop talking.
  useEffect(() => {
    if (!controls || !live) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.stopPropagation();
      controls.cancel();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [controls, live]);

  // Hold Space to talk -- SHEET ONLY. The sheet is a surface somebody
  // deliberately opened; the desk widget is always on screen, and Space on a
  // desktop must not start recording. The mic button's own Space still works
  // in both, because a focused button is an explicit target.
  useEffect(() => {
    if (!controls || variant !== "sheet" || !settings.spaceToTalk) return;
    const down = (event: KeyboardEvent) => {
      if (event.code !== "Space" || event.repeat) return;
      if (isTypingTarget(document.activeElement) || document.activeElement === micRef.current) return;
      event.preventDefault();
      controls.press();
    };
    const up = (event: KeyboardEvent) => {
      if (event.code !== "Space") return;
      if (document.activeElement === micRef.current) return;
      controls.release();
    };
    window.addEventListener("keydown", down);
    window.addEventListener("keyup", up);
    return () => {
      window.removeEventListener("keydown", down);
      window.removeEventListener("keyup", up);
    };
  }, [controls, variant, settings.spaceToTalk]);

  function send(prompt: string): boolean {
    if (!ready) return false;
    nearBottom.current = true;
    return conversation.send(prompt, context);
  }

  function onSubmit(event: FormEvent) {
    event.preventDefault();
    // Send while the mic is live means "I am done", not "send the half of it
    // you have so far" -- otherwise the question goes and the microphone
    // stays open behind it.
    if (controls && live) {
      controls.commit();
      return;
    }
    const prompt = draft.trim();
    if (!prompt) return;
    if (send(prompt)) setDraft("");
  }

  const wired = voicePorts !== null;
  const note = voiceNote(voice?.state, settings, wired);

  return (
    <div className="os-ask" data-os-ask={variant} onKeyDown={event => { if (event.key === "Escape" && panel !== "conversation") { event.stopPropagation(); closePanel(); } }}>
      <header className="os-window-bar os-ask-header">
        <div className="os-ask-heading"><strong>MemQL</strong><span>{contextLabel || "Ask"}</span></div>
        <div className="os-window-controls os-ask-actions"><div className="os-window-control-group">
          <button type="button" className="os-icon-button" title="New conversation" aria-label="New conversation" disabled={busy || state.voiceActive} onClick={() => { conversation.newConversation(); setPanel("conversation"); returnToComposer.current = true; }}><Plus size={14} /></button>
          </div><div className="os-window-control-group">
          <button ref={conversationsRef} type="button" className="os-icon-button" title={showConversations ? "Conversation" : "Conversations"} aria-label={showConversations ? "Conversation" : "Conversations"} onClick={() => { if (showConversations) { closePanel(); return; } controls?.cancel(); setPanel("conversations"); void conversation.refresh(); }}>{showConversations ? <MessageCircle size={14} /> : <MessagesSquare size={14} />}</button>
          <button ref={activityRef} type="button" className="os-icon-button" title={showActivity ? "Conversation" : "Activity"} aria-label={showActivity ? "Conversation" : "Activity"} onClick={() => { if (showActivity) { closePanel(); return; } controls?.cancel(); setPanel("activity"); }}>{showActivity ? <MessageCircle size={14} /> : <Activity size={14} />}</button>
          </div>{onClose || onToggleMaximize ? <div className="os-window-control-group">
          {onToggleMaximize ? <button type="button" className="os-icon-button" title={maximized ? "Restore Ask" : "Maximize Ask"} aria-label={maximized ? "Restore Ask" : "Maximize Ask"} onClick={onToggleMaximize}>{maximized ? <Minimize2 size={14} /> : <Maximize2 size={14} />}</button> : null}
          {onClose ? <button type="button" className="os-icon-button os-window-close" title="Close Ask" aria-label="Close Ask" onClick={onClose}><X size={14} /></button> : null}</div> : null}
        </div>
      </header>
      {showActivity ? <div className="os-ask-content"><AskActivityLog turns={exchanges} dictation={state.dictationActivity} onClose={closePanel} onStop={busy ? () => conversation.stop() : undefined} /></div> : showConversations ? <div className="os-ask-content"><AskConversations conversation={conversation} onOpen={changed => { if (changed) { nearBottom.current = true; conversationScroll.current = 0; } setPanel("conversation"); returnToComposer.current = true; }} /></div> : state.voiceActive && liveVoice ? <div className="os-ask-content"><AskLiveVoice session={liveVoice} /></div> : <div className="os-ask-content">
        <div className="os-ask-log" ref={logRef} role="log" aria-label="Conversation" aria-live="polite" onScroll={() => { const el = logRef.current; if (el) { conversationScroll.current = el.scrollTop; nearBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 72; } }}>
          {state.loading ? <ContentSkeleton kind="conversation" label="Opening conversation" /> : null}
          {exchanges.length === 0 && !state.loading ? <div className="os-ask-empty"><strong>What would you like to do?</strong><p>Ask a question, explore your workspace, or let MemQL help you get something done.</p></div> : null}
          {conversationFeed(exchanges).map(({ turn, completion, key }) => <div key={key} className="os-ask-exchange" data-state={turn.state}>
            {!completion ? <div className="os-ask-message"><AccountAvatar className="os-ask-avatar" /><div><div className="os-ask-byline"><strong>You</strong><time dateTime={turn.startedAt}>{messageTime(turn.startedAt)}</time></div><p>{turn.prompt}</p></div></div> : null}
            <div className="os-ask-message"><span className="os-avatar os-ask-avatar os-ask-avatar-memql" aria-hidden><Mark size={22} /></span><div><div className="os-ask-byline"><strong>MemQL</strong>{turn.state === "done" && turn.endedAt ? <time dateTime={turn.endedAt}>{messageTime(turn.endedAt)}</time> : null}</div>
              {completion ? <p className="os-ask-result-context">{turn.workTitle || turn.prompt}</p> : null}
              {turn.answer ? <AskMessage text={turn.answer} /> : null}
              {onOpenFile ? turn.activity.filter(event => event.kind === "artifact" && event.phase === "completed" && typeof event.arguments?.fileId === "string").map(event => <button key={event.id} type="button" className="os-ask-retry" onClick={() => onOpenFile(event.arguments!.fileId as string)}>Open {event.name || "file"}</button>) : null}
              {turn.state === "streaming" ? <AskWait activity={turn.activity} startedAt={turn.startedAt} hasText={Boolean(turn.answer)} /> : null}
              {turn.error ? <div className="os-ask-error"><p role="alert">{askErrorSummary(turn.error)}</p>{askErrorSummary(turn.error) !== turn.error ? <details><summary>Details</summary><p>{turn.error}</p></details> : null}{!busy ? <button type="button" className="os-ask-retry" onClick={() => { setDraft(turn.prompt); inputRef.current?.focus(); }}>Edit and try again</button> : null}{onOpenFleet ? <button type="button" className="os-ask-retry" onClick={onOpenFleet}>Open Fleet</button> : null}</div> : null}
            </div></div>
          </div>)}
        </div>
      </div>
      }
      {state.error ? <p className="os-ask-error" role="alert">{askErrorSummary(state.error)}</p> : null}
      {liveVoice && !state.voiceActive ? <AskLiveVoice session={liveVoice} errorsOnly /> : null}
      {!state.voiceActive && panel === "conversation" ? <AskWorkQueue turns={exchanges} onOpen={runId => onOpenWork?.(runId, state.selectedId ?? undefined)} /> : null}
      {!state.voiceActive && panel === "conversation" ? <form className="os-ask-input" onSubmit={onSubmit}>
        <button
          ref={micRef}
          type="button"
          className="os-ask-mic"
          data-voice={phase}
          title={live ? "Stop dictation" : "Dictate a message"}
          aria-label={live ? "Stop listening" : "Dictate a message"}
          aria-pressed={live}
          disabled={!wired}
          onPointerDown={(event) => {
            if (!controls) return;
            event.preventDefault();
            try {
              event.currentTarget.setPointerCapture(event.pointerId);
            } catch {
              // jsdom and some touch stacks have no capture; the gesture
              // still works, it just does not survive leaving the button.
            }
            controls.clearProblem();
            controls.press();
          }}
          onPointerUp={(event) => {
            if (!controls) return;
            try {
              event.currentTarget.releasePointerCapture(event.pointerId);
            } catch {
              /* see above */
            }
            controls.release();
          }}
          // CANCEL, NOT RELEASE. `pointercancel` means the browser took the
          // gesture over -- on touch, that is a finger sliding off the mic
          // into a scroll. Treating it as a release would LATCH, leaving a
          // hot microphone behind a gesture the person abandoned; cancelling
          // closes the device and sends nothing, which is the reading that
          // asserts least about what they meant.
          onPointerCancel={() => controls?.cancel()}
          onKeyDown={(event) => {
            if (!controls || (event.key !== " " && event.key !== "Enter")) return;
            // preventDefault suppresses the synthetic click a button fires on
            // Space, which would otherwise press the mic a second time.
            event.preventDefault();
            if (!event.repeat) {
              controls.clearProblem();
              controls.press();
            }
          }}
          onKeyUp={(event) => {
            if (!controls || (event.key !== " " && event.key !== "Enter")) return;
            event.preventDefault();
            controls.release();
          }}
        >
          <Mic size={15} aria-hidden />
        </button>
        {liveVoice ? <button type="button" className="os-ask-mic" title="Talk with MemQL" aria-label="Talk with MemQL" disabled={!ready || busy || live} onClick={() => { controls?.cancel(); void liveVoice.start(context, settings.voice ?? "female"); }}><AudioLines size={17} /></button> : null}
        <textarea
          rows={1}
          onKeyDown={event => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); if (draft.trim() && send(draft)) setDraft(""); } }}
          ref={inputRef}
          className="os-ask-field"
          placeholder={live ? "Listening" : "Ask"}
          aria-label="Ask"
          value={draft}
          // Read-only, never disabled, and only while the mic is writing it:
          // the next delta carries the WHOLE transcript, so a character typed
          // here would vanish on the following word. It stays focusable and
          // selectable, and it comes straight back the moment voice ends.
          readOnly={live}
          onChange={(event) => setDraft(event.target.value)}
        />
        {busy ? <button type="button" className="os-ask-send" aria-label="Stop reply" title="Stop this work" onClick={() => conversation.stop()}><Square size={13} /></button> : <button
          type="submit"
          className="os-ask-send"
          aria-label={live ? "Finish" : "Send"}
          disabled={!live && (!ready || busy || !draft.trim())}
        >
          <ArrowUp size={15} aria-hidden />
        </button>}
      </form> : null}
      {phase === "transcribing" && !state.voiceActive && panel === "conversation" ? <AskWait activity={state.dictationActivity} startedAt={state.dictationActivity.at(-1)?.at ?? new Date().toISOString()} hasText={false} label="Transcribing" /> : null}
      {note && phase !== "transcribing" && !state.voiceActive && panel === "conversation" ? (
        <p
          className="os-caption os-ask-micnote"
          data-note={!wired || voice?.state.problem ? "problem" : "state"}
        >
          {note}
        </p>
      ) : null}
    </div>
  );
}

/** The one sentence under the input, or null when there is nothing to say. */
export function voiceNote(
  state: VoiceState | undefined,
  settings: AskSettings,
  wired: boolean,
): string | null {
  if (!wired) return ASK_VOICE_UNAVAILABLE;
  if (!state) return null;
  if (state.problem) return voiceProblemSentence(state.problem);
  if (state.phase === "transcribing") return ASK_VOICE_FINISHING;
  if (state.phase === "listening") {
    if (settings.commit === "review") {
      return state.latched ? ASK_VOICE_LATCHED_REVIEW : ASK_VOICE_HOLD_REVIEW;
    }
    return state.latched ? ASK_VOICE_LATCHED : ASK_VOICE_HOLD;
  }
  // `starting` says nothing on purpose. With permission already granted it
  // lasts a few milliseconds, and a caption that appears and vanishes reads
  // as a glitch; with permission not yet granted, the browser's own prompt is
  // on screen saying it better than this line could.
  return null;
}

function isTypingTarget(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  return el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable;
}

/** Keep diagnostic context available without making the conversation a log viewer. */
function askErrorSummary(message: string): string {
  if (message.length > 240 || message.includes("every_door_shut") || message.includes("\n")) {
    return "The cluster could not complete this reply. Retry, or check the available models in Fleet.";
  }
  return message;
}

function messageTime(value: string): string { const date = new Date(value); return Number.isFinite(date.valueOf()) ? date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : ""; }
