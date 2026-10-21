import { useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Button } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useOsIfPresent } from "../../../chrome/state";
import { accessAdmits } from "../../../system/registry";
import { appLabel } from "../rows";
import "./appRouting.css";

export function RetryAppAvailability({ registrationId, app, disabled }: { registrationId: string; app: "claude-code" | "codex"; disabled: boolean }) {
  const connection = useOsConnection();
  const os = useOsIfPresent();
  const pending = useRef(false);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [announcement, setAnnouncement] = useState("");
  async function retry() {
    if (disabled || pending.current || !connection) return;
    pending.current = true; setBusy(true); setNotice("");
    try {
      const result = await connection.query.fleetRetryAppAvailability({ registrationId, app });
      const raw = result.rows()[0];
      if (!raw || typeof flatten(raw)["retriedAt"] !== "string") throw new Error("App retry was not confirmed.");
      setAnnouncement(`${appLabel(app)} can be tried on the next call.`);
    } catch (error) {
      console.error("App availability retry failed", registrationId, app, error);
      setNotice("App availability could not be retried.");
    } finally { pending.current = false; setBusy(false); }
  }
  return <>
    <Button disabled={disabled || !connection} busy={busy} ariaLabel={`Retry ${appLabel(app)} availability`} onClick={() => void retry()}>Retry availability</Button>
    <span role="status" className="os-sr-only">{announcement}</span>
    {notice ? createPortal(<div role="status" className="os-app-routing-toast"><span>{notice}</span>
      {os && accessAdmits("app:logs") ? <Button onClick={() => { os.actions.openApp("logs", "search"); setNotice(""); }}>Open logs</Button> : null}
      <Button onClick={() => setNotice("")}>Dismiss</Button>
    </div>, document.body) : null}
  </>;
}
