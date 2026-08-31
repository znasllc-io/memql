import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

import { useOsConnection } from "../../live/connection";
import { EMPTY_HISTORY, recordTransition, type ConnectionHistory } from "./connectionHistory";

// Mounted by the Settings app itself rather than by the Diagnostics section,
// so the buffer covers the WINDOW's lifetime (memql#4744). A person who
// opens Settings, notices the dock dot go amber, and then navigates to
// Diagnostics is exactly the person this panel is for -- and a buffer that
// started when they arrived at the section would have nothing to show them.

const ConnectionHistoryContext = createContext<ConnectionHistory>(EMPTY_HISTORY);

export function ConnectionHistoryProvider({ children }: { children: ReactNode }) {
  const connection = useOsConnection();
  const [history, setHistory] = useState<ConnectionHistory>(EMPTY_HISTORY);

  useEffect(() => {
    if (!connection) return;
    // Keyed on the connection's IDENTITY and nothing else. The shell dials
    // once and the handle changes exactly once (null -> dialed), so adding
    // anything that merely arrives late -- the actor, the runtime config --
    // would tear this subscription down and lose the buffer with it.
    return connection.onStatusChange((event) => {
      setHistory((prior) => recordTransition(prior, event, Date.now()));
    });
  }, [connection]);

  return (
    <ConnectionHistoryContext.Provider value={history}>{children}</ConnectionHistoryContext.Provider>
  );
}

export function useConnectionHistory(): ConnectionHistory {
  return useContext(ConnectionHistoryContext);
}
