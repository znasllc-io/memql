import { createContext, useContext } from "react";

// The dock's connection dot reads this. PR A has no SDK connection yet, so
// the default is honest: "disconnected". The live substrate task (#4719)
// provides the real value from the sdk-core Connection without touching
// any consumer.

export type ShellConnectionStatus = "connected" | "reconnecting" | "disconnected";

export const ConnectionStatusContext = createContext<ShellConnectionStatus>("disconnected");

export function useConnectionStatus(): ShellConnectionStatus {
  return useContext(ConnectionStatusContext);
}
