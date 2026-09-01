import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUp, Mic } from "lucide-react";

import type { AskHandle, AskTransport } from "./askController";
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

interface Exchange {
  id: number;
  prompt: string;
  answer: string;
  state: "streaming" | "done" | "error";
  error?: string;
}

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
      // is not configured" is what a cluster with no voice node answers, and
      // a friendlier paraphrase would drop the one fact that helps.
      return problem.message;
  }
}

export function AskSurface({
  transport,
  voicePorts = null,
  settings = DEFAULT_ASK_SETTINGS,
  context = null,
  variant,
  autoFocus = false,
}: {
  transport: AskTransport;
  /** Absent = this window has no voice wiring; the control says so. */
  voicePorts?: VoicePorts | null;
  settings?: AskSettings;
  context?: string | null;
  variant: "sheet" | "widget";
  autoFocus?: boolean;
}) {
  const [draft, setDraft] = useState("");
  const [exchanges, setExchanges] = useState<Exchange[]>([]);
  const handleRef = useRef<AskHandle | null>(null);
  const nextIdRef = useRef(1);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const logRef = useRef<HTMLDivElement | null>(null);
  const micRef = useRef<HTMLButtonElement | null>(null);
  const reducedMotion = useReducedMotion();

  const voice = useVoice(voicePorts, {
    onTranscript: (text) => setDraft(text),
    onUtterance: (text) => {
      if (settings.commit === "send") {
        setDraft("");
        send(text);
      } else {
        setDraft(text);
        inputRef.current?.focus();
      }
    },
  });
  const controls = voice?.controls ?? null;
  const phase = voice?.state.phase ?? "idle";
  const live = phase === "listening" || phase === "transcribing";

  useEffect(() => {
    if (autoFocus) inputRef.current?.focus();
    return () => handleRef.current?.cancel();
  }, [autoFocus]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
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

  function send(prompt: string) {
    const id = nextIdRef.current;
    nextIdRef.current += 1;
    setExchanges((prev) => [...prev, { id, prompt, answer: "", state: "streaming" }]);
    const patch = (p: Partial<Exchange>) =>
      setExchanges((prev) => prev.map((e) => (e.id === id ? { ...e, ...p } : e)));
    handleRef.current = transport.ask(prompt, context, {
      delta: (text) => setExchanges((prev) => prev.map((e) => (e.id === id ? { ...e, answer: e.answer + text } : e))),
      done: () => patch({ state: "done" }),
      error: (message) => patch({ state: "error", error: message }),
    });
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
    setDraft("");
    send(prompt);
  }

  const wired = voicePorts !== null;
  const note = voiceNote(voice?.state, settings, wired);

  return (
    <div className="os-ask" data-os-ask={variant}>
      {context ? <span className="os-ask-context">{context}</span> : null}
      <div className="os-ask-log" ref={logRef} aria-live="polite">
        {exchanges.length === 0 ? (
          <p className="os-caption os-ask-hint">
            {variant === "widget" ? "Ask the OS anything." : "Ask about this cluster, an app, or anything on your desk."}
          </p>
        ) : null}
        {exchanges.map((e) => (
          <div key={e.id} className="os-ask-exchange" data-state={e.state}>
            <p className="os-ask-prompt">{e.prompt}</p>
            {e.answer ? <p className="os-ask-answer">{e.answer}</p> : null}
            {e.state === "error" ? (
              <p className="os-ask-error">
                {e.error ?? "Something went wrong."}{" "}
                <button type="button" className="os-link" onClick={() => send(e.prompt)}>
                  Retry
                </button>
              </p>
            ) : null}
          </div>
        ))}
      </div>
      <form className="os-ask-input" onSubmit={onSubmit}>
        <button
          ref={micRef}
          type="button"
          className="os-ask-mic"
          data-voice={phase}
          aria-label={live ? "Stop listening" : "Ask by voice"}
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
        <input
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
        <button
          type="submit"
          className="os-ask-send"
          aria-label={live ? "Finish" : "Send"}
          disabled={!live && !draft.trim()}
        >
          <ArrowUp size={15} aria-hidden />
        </button>
      </form>
      {note ? (
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
