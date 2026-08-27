import { Mark } from "./Mark";
import { ModeSwitcher } from "./ModeSwitcher";

export function SignIn({
  status,
  onSignIn,
}: {
  status: "signed-out" | "unavailable";
  onSignIn: () => void;
}) {
  return (
    <div className="os-signin" data-os-signin data-status={status}>
      <div className="os-signin-card">
        <Mark className="os-mark os-mark-lg" />
        <h1>MemQL OS</h1>
        <p>
          {status === "unavailable"
            ? "This cluster has not published a sign-in configuration."
            : "Sign in with the same passkey or magic link you use for the portal."}
        </p>
        {status === "signed-out" ? (
          <button type="button" className="os-primary" data-sign-in onClick={onSignIn}>
            Sign in
          </button>
        ) : null}
        <ModeSwitcher />
      </div>
    </div>
  );
}
