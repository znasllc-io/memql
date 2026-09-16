import { createContext, useContext, type ReactNode } from "react";

const PaneActivity = createContext(true);

export function ActivePane({ active, children }: { active: boolean; children: ReactNode }) {
  const parentActive = useContext(PaneActivity);
  return <PaneActivity.Provider value={parentActive && active}>{children}</PaneActivity.Provider>;
}

export function usePaneActive(): boolean {
  return useContext(PaneActivity);
}
