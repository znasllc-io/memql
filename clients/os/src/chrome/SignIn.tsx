import { useEffect, useRef, useState } from "react";
import { canCoordinateIdentityRefresh } from "../auth/identityClient";
import { IdentityFrame } from "../auth/IdentityFrame";
import { EntryPending } from "../kit/EntryPending";
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

  if (supportedBrowser && status === "signed-out" && !failed) return <EntryPending label="Opening sign-in" />;

  return (
    <IdentityFrame title="Sign-in is unavailable">
        <p role="alert">
          {!supportedBrowser
            ? "Update your browser to keep sign-in in sync across tabs. MemQL OS requires Web Locks support."
            : failed
            ? "We couldn’t open sign-in. Allow this site to store sign-in data in your browser, then try again."
            : "Identity or ownership status is unavailable. Reconnect and try again."}
        </p>
        {supportedBrowser && <Button tone="primary" onClick={() => window.location.reload()}>Retry</Button>}
    </IdentityFrame>
  );
}
