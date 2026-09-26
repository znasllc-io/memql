import { useEffect, useRef, useState } from "react";
import { canCoordinateIdentityRefresh } from "../auth/identityClient";
import { Mark } from "./Mark";
import { Button } from "../kit/controls";

export function SignIn({
  status,
  onSignIn,
}: {
  status: "signed-out" | "unavailable";
  onSignIn: () => Promise<void>;
}) {
  const supportedBrowser = canCoordinateIdentityRefresh();
  const started = useRef(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (status !== "signed-out" || !supportedBrowser || started.current) return;
    started.current = true;
    void onSignIn().catch(() => setFailed(true));
  }, [status, supportedBrowser, onSignIn]);

  if (supportedBrowser && status === "signed-out" && !failed) return <div className="os-boot" role="status">Opening sign-in…</div>;

  return (
    <main className="os-signin" aria-labelledby="os-signin-unavailable" data-status={status}>
      <section className="os-signin-content">
        <span className="os-wizard-mark os-signin-mark"><Mark size={34} /></span>
        <header className="os-signin-heading"><h1 id="os-signin-unavailable">Sign-in is unavailable</h1></header>
        <p role="alert">
          {!supportedBrowser
            ? "Update your browser to keep sign-in in sync across tabs. MemQL OS requires Web Locks support."
            : failed
            ? "We couldn’t open sign-in. Allow this site to store sign-in data in your browser, then try again."
            : "Identity or ownership status is unavailable. Reconnect and try again."}
        </p>
        {supportedBrowser && <Button tone="primary" onClick={() => window.location.reload()}>Retry</Button>}
      </section>
    </main>
  );
}
