import { useEffect, useState } from "react";
import { useConnectionRecovery, useConnectionStatus } from "../chrome/connection";

/** Shared, non-blocking connection feedback. Brief browser suspensions are
 * silent. The grace period starts when visible, not while Safari is asleep. */
export function ConnectionNotice({ className = "" }: { className?: string }) {
  const status = useConnectionStatus();
  const { managed, retry } = useConnectionRecovery();
  const recovering = managed && status !== "connected";
  const [phase, setPhase] = useState<"quiet" | "connecting" | "unavailable">("quiet");
  useEffect(() => {
    let reveal: ReturnType<typeof setTimeout> | undefined;
    let failure: ReturnType<typeof setTimeout> | undefined;
    const reset = () => {
      clearTimeout(reveal); clearTimeout(failure); setPhase("quiet");
      if (!recovering || document.visibilityState === "hidden") return;
      reveal = setTimeout(() => setPhase("connecting"), 2_000);
      failure = setTimeout(() => setPhase("unavailable"), 30_000);
    };
    reset();
    document.addEventListener("visibilitychange", reset);
    return () => { clearTimeout(reveal); clearTimeout(failure); document.removeEventListener("visibilitychange", reset); };
  }, [recovering]);
  if (!managed || status === "connected" || phase === "quiet") return null;
  return <div className={`os-connection-notice ${className}`}>
    <span role="status" aria-live="polite">
      {phase === "connecting" ? <><span className="os-connection-spinner" aria-hidden="true" />Reconnecting…</> : "Can’t reconnect"}
    </span>
    {phase === "unavailable" ? <button type="button" className="os-button" data-tone="quiet" onClick={retry}>Retry connection</button> : null}
  </div>;
}
