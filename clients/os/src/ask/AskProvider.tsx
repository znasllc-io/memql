import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";

import type { AskTransport } from "./askController";

// One Ask, three entry points (spec D6): the dock orb, the desk widget and
// every window's title bar all open the SAME surface. This context carries
// the transport and the sheet's open/context state; the widget renders the
// surface inline and needs only the transport.

export interface AskSheetState {
  open: boolean;
  context: string | null;
}

interface AskContextValue {
  transport: AskTransport;
  sheet: AskSheetState;
  openAsk: (context?: string | null) => void;
  closeAsk: () => void;
}

const Ctx = createContext<AskContextValue | null>(null);

export function useAsk(): AskContextValue {
  const value = useContext(Ctx);
  if (!value) throw new Error("useAsk outside AskProvider");
  return value;
}

export function AskProvider({ transport, children }: { transport: AskTransport; children: ReactNode }) {
  const [sheet, setSheet] = useState<AskSheetState>({ open: false, context: null });
  const openAsk = useCallback((context: string | null = null) => setSheet({ open: true, context }), []);
  const closeAsk = useCallback(() => setSheet((s) => ({ ...s, open: false })), []);
  const value = useMemo(() => ({ transport, sheet, openAsk, closeAsk }), [transport, sheet, openAsk, closeAsk]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
