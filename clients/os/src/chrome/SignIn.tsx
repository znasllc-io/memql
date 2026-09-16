import { canCoordinateIdentityRefresh } from "../auth/identityClient";
import { Mark } from "./Mark";

export function SignIn({
  status,
  onSignIn,
}: {
  status: "signed-out" | "unavailable";
  onSignIn: () => void;
}) {
  const supportedBrowser = canCoordinateIdentityRefresh();
  return (
    <div className="os-signin" data-os-signin data-status={status}>
      <div className="os-signin-card">
        <Mark className="os-mark os-mark-lg" />
        <h1>MemQL OS</h1>
        <p>
          {!supportedBrowser
            ? "Update your browser to keep sign-in in sync across tabs. MemQL OS requires Web Locks support."
            : status === "unavailable"
            ? "This cluster has not published a sign-in configuration."
            : "Sign in to MemQL with your passkey or a magic link."}
        </p>
        {supportedBrowser && status === "signed-out" ? (
          <button type="button" className="os-primary" data-sign-in onClick={onSignIn}>
            Sign in
          </button>
        ) : null}
      </div>
    </div>
  );
}
