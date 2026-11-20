import { useCallback, useRef } from "react";

import { useAsk } from "../ask/AskProvider";

/**
 * An app window's two Ask props (OsAppProps.askContext / askAbout).
 *
 * `askContext` NOTES what the person is looking at -- a run, a goal, a file --
 * and never opens anything: selecting a thing is not asking about it. The
 * phone's Ask orb carries the note; the desktop's carrier was the title bar's
 * Ask button, removed Nov 2026 (Ask lives in the dock), so there it is unread.
 * It used to open Ask outright, which threw the Ask window over Nexus every
 * time a run or goal was opened.
 *
 * `askAbout` opens Ask about one thing, for an explicit "Ask about this" the
 * person chose.
 */
export function useWindowAsk(baseTag: string) {
  const { openAsk } = useAsk();
  const noted = useRef("");
  const askContext = useCallback((tag: string) => {
    noted.current = tag.trim();
  }, []);
  const askAbout = useCallback((tag: string) => openAsk(tag), [openAsk]);
  const contextTag = useCallback(
    () => (noted.current ? `${baseTag} ${noted.current}` : baseTag),
    [baseTag],
  );
  return { askContext, askAbout, contextTag };
}
