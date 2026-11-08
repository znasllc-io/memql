import { createContext, useContext, type ReactNode } from "react";
import { LiveReadVisibility } from "../../live/useGraphRevision";

const PaneActivity = createContext(true);

export function ActivePane({ active, children }: { active: boolean; children: ReactNode }) {
  const parentActive = useContext(PaneActivity);
  const parentVisible = useContext(LiveReadVisibility);
  return <PaneActivity.Provider value={parentActive && active}><LiveReadVisibility.Provider value={parentVisible && parentActive && active}>{children}</LiveReadVisibility.Provider></PaneActivity.Provider>;
}

export function usePaneActive(): boolean {
  return useContext(PaneActivity);
}
