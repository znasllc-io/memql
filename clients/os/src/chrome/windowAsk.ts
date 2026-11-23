import { useCallback, useRef } from "react";

import { useAsk } from "../ask/AskProvider";

/**
 * An app window's two Ask props (OsAppProps.askContext / askAbout).
 *
 * `askContext` NOTES what the person is looking at -- a run, a goal, a file --
 * and never opens anything: selecting a thing is not asking about it. The note
 * rides on the next Ask the person opens: the phone's Ask orb reads the open
 * app's, the desktop dock's Ask reads the focused window's (registered below
 * by WindowFrame; Jesús, Nov 20 2026, after the title bar's Ask went). It used
 * to open Ask outright, which threw the Ask window over Nexus every time a run
 * or goal was opened.
 *
 * `askAbout` opens Ask about one thing, for an explicit "Ask about this" the
 * person chose. It is undefined when the person cannot open Ask (canAsk,
 * the test AskProvider's openAsk makes), so every "Ask about this" control is
 * absent instead of a button that silently does nothing.
 */
export function useWindowAsk(baseTag: string) {
  const { canAsk, openAsk } = useAsk();
  const noted = useRef("");
  const askContext = useCallback((tag: string) => {
    noted.current = tag.trim();
  }, []);
  const askAbout = useCallback((tag: string) => openAsk(tag), [openAsk]);
  const contextTag = useCallback(
    () => (noted.current ? `${baseTag} ${noted.current}` : baseTag),
    [baseTag],
  );
  return { askContext, askAbout: canAsk ? askAbout : undefined, contextTag };
}

/** What Ask opens with for one window: transport context and a short label. */
export interface WindowAskContext {
  context: string;
  label: string;
}

// The dock is one button for every window, so each WindowFrame registers how
// to read its context NOW (not a copy taken at render: a note or the visible
// page can change without a re-render) and the dock asks the focused one.
const windowAskers = new Map<string, () => WindowAskContext>();

export function registerWindowAsk(windowId: string, read: () => WindowAskContext): () => void {
  windowAskers.set(windowId, read);
  return () => {
    if (windowAskers.get(windowId) === read) windowAskers.delete(windowId);
  };
}

/** The context of `windowId`, or undefined when there is no such window. */
export function windowAskContext(windowId: string | null): WindowAskContext | undefined {
  return windowId === null ? undefined : windowAskers.get(windowId)?.();
}
