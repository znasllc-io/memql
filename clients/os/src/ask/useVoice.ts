// The React binding for VoiceSession. Thin on purpose: every decision lives
// in the pure session beside it, so this file holds only the things React
// owns -- an instance that survives re-render, callbacks that do not restart
// it, and teardown on unmount.

import { useEffect, useMemo, useRef, useState } from "react";

import { IDLE, VoiceSession, type VoicePorts, type VoiceState } from "./voiceSession";

export interface VoiceHandlers {
  /** Replaces the field. The wire's deltas are cumulative, never increments. */
  onTranscript(text: string): void;
  onUtterance(text: string): void;
}

/**
 * The session's methods, and NOTHING that changes.
 *
 * Split from `state` so the object is STABLE for the life of the surface. The
 * level ring is a rAF loop in an effect that depends on this; if the whole
 * thing were one object rebuilt per render, every transcript delta -- which
 * arrives several times a second while somebody is speaking -- would tear the
 * loop down and reschedule it, and the ring would drop frames exactly while it
 * is the thing being watched. The same object keeps the Space-to-talk listener
 * from being removed and re-added on every keystroke.
 */
export interface VoiceControls {
  press(): void;
  release(): void;
  commit(): void;
  cancel(): void;
  clearProblem(): void;
  /** Read on a frame clock. Never put this in state -- see AskSurface. */
  level(): number;
}

export interface Voice {
  state: VoiceState;
  controls: VoiceControls;
}

export function useVoice(ports: VoicePorts | null, handlers: VoiceHandlers): Voice | null {
  const [state, setState] = useState<VoiceState>(IDLE);
  const sessionRef = useRef<VoiceSession | null>(null);
  // THE SESSION MUST NOT BE REBUILT WHEN A CALLBACK IDENTITY CHANGES. The
  // handlers close over `draft`, so they are new on every keystroke; keying
  // the session on them would tear down the microphone mid-sentence. The
  // session holds this ref, not the functions.
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;

  if (ports && !sessionRef.current) {
    sessionRef.current = new VoiceSession(ports, {
      onState: setState,
      onTranscript: (text) => handlersRef.current.onTranscript(text),
      onUtterance: (text) => handlersRef.current.onUtterance(text),
    });
  }

  useEffect(() => {
    // Unmount closes the device. A sheet dismissed mid-sentence must not
    // leave the browser's recording indicator lit.
    return () => sessionRef.current?.cancel();
  }, []);

  const session = sessionRef.current;
  const controls = useMemo<VoiceControls | null>(
    () =>
      session
        ? {
            press: () => session.press(),
            release: () => session.release(),
            commit: () => session.commit(),
            cancel: () => session.cancel(),
            clearProblem: () => session.clearProblem(),
            level: () => session.level(),
          }
        : null,
    [session],
  );

  if (!ports || !session || !controls) return null;
  return { state, controls };
}

/**
 * Whether this reader has asked for less movement.
 *
 * Local rather than promoted to `kit/`: the wallpaper's own check is inside a
 * canvas paint loop rather than a hook, so there is one hook-shaped caller,
 * and a shared abstraction inferred from one caller is not shared vocabulary.
 */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(() => matchReduced()?.matches ?? false);
  useEffect(() => {
    const query = matchReduced();
    if (!query) return;
    const onChange = () => setReduced(query.matches);
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);
  return reduced;
}

function matchReduced(): MediaQueryList | null {
  try {
    return globalThis.matchMedia?.("(prefers-reduced-motion: reduce)") ?? null;
  } catch {
    return null;
  }
}
