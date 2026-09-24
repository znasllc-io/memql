import { Fingerprint } from "lucide-react";
import { Mark } from "../chrome/Mark";
import { Button, Field } from "../kit/controls";
import { value, type IdentityData } from "./nativeIdentity";

export interface SignInProblem { message: string; next: string }

/** Protocol responses remain available in the network/server diagnostics. Never
 * echo an unknown response into the signed-out product surface. */
export function signInProblem(error: unknown): SignInProblem {
  const message = error instanceof Error ? error.message : String(error);
  const name = typeof error === "object" && error !== null && "name" in error && typeof error.name === "string" ? error.name : "";
  if (message.includes("no passkey matches the asserted credential")) return {
    message: "This passkey couldn’t be recognized.",
    next: "Try again with a passkey you’ve used for this MemQL installation.",
  };
  if (["NotAllowedError", "AbortError"].includes(name) || message === "No passkey was selected") return {
    message: "Sign-in wasn’t completed.",
    next: "The passkey request was canceled or timed out. You can try again.",
  };
  if (message === "Use a current browser with passkey support to sign in.") return {
    message: "This browser can’t use passkeys here.",
    next: "Try an updated browser with passkey support.",
  };
  return { message: "Sign-in couldn’t be completed.", next: "Try again. If it keeps happening, contact your cluster administrator." };
}

/** Hiding redundant OS consent text never changes the OAuth fields sent back.
 * A self-registered name is not identity, even when it calls itself “os”. */
function isOsDestination(data: IdentityData, clientId: string): boolean {
  if (data.ClientSelfRegistered === true || value(data, "ClientID") !== clientId) return false;
  try {
    const destination = new URL(value(data, "RedirectURI"));
    return destination.origin === window.location.origin && destination.pathname === "/auth/callback" && !destination.username && !destination.password;
  } catch { return false; }
}

export function SignInPage({ data, clientId, fields, busy, passkeyPending, problem, onField, onSubmit, onPasskey, onLegal }: {
  data: IdentityData; clientId: string; fields: Record<string, string>;
  busy: boolean; passkeyPending: boolean; problem?: SignInProblem;
  onField: (name: string, text: string) => void;
  onSubmit: () => void; onPasskey: () => void; onLegal: (path: string) => void;
}) {
  const stage = value(data, "Stage");
  const local = data.Local === true;
  const title = stage === "waitlist_signup" ? "Request access" : stage === "needs_invite" ? "Use your invitation" : "Sign in to MemQL OS";
  const emailAction = stage === "waitlist_signup" ? "Request access" : stage === "needs_invite" ? "Use invitation" : "Send sign-in link";
  const field = (name: string, label: string, type = "text") => <Field label={label}>
    <input className="os-input" aria-label={label} type={type} name={name} required={name !== "additional_context"}
      autoComplete={name === "email" ? "email" : name === "name" ? "name" : "off"}
      value={fields[name] || ""} disabled={busy} onChange={event => onField(name, event.target.value)} />
  </Field>;
  return <main className="os-signin" aria-labelledby="os-signin-title">
    <section className="os-signin-content">
      <span className="os-wizard-mark os-signin-mark"><Mark size={34} /></span>
      <header className="os-signin-heading">
        <h1 id="os-signin-title">{title}</h1>
        <p>{local ? "Use your passkey to continue on this device." : stage === "waitlist_signup" ? "Tell us a little about yourself to request access." : stage === "needs_invite" ? "Enter the invitation you received to continue." : "Use your email or a passkey to continue."}</p>
      </header>
      {data.AuthorizeMode === true && !isOsDestination(data, clientId) ? <div className="os-signin-destination">
        <p>Sign in to continue to <strong>{value(data, "ClientName") || "the requesting app"}</strong>.</p>
        <p>Destination: <span className="os-mono">{value(data, "RedirectURI")}</span></p>
        {data.ClientSelfRegistered === true ? <p>This app’s name has not been verified. Continue only if you recognize this destination.</p> : null}
      </div> : null}
      {data.Flash ? <p className="os-caption" role="status">{data.Flash.Message}</p> : null}
      {problem ? <div className="os-signin-problem" role="alert" aria-atomic="true">
        <p>{problem.message}</p><p>{problem.next}</p>
      </div> : null}
      {!local ? <form className="os-signin-form" onSubmit={event => { event.preventDefault(); if (!busy) onSubmit(); }}>
        {field("email", "Email", "email")}
        {stage === "waitlist_signup" ? <>{field("name", "Your name")}{field("additional_context", "Why would you like access?")}</> : null}
        {stage === "needs_invite" ? field("invitation", "Invitation token") : null}
        <Button type="submit" tone="primary" disabled={busy} busy={busy && !passkeyPending} busyLabel="Continuing…">{emailAction}</Button>
      </form> : null}
      {stage === "email" ? <Button tone={local ? "primary" : "quiet"} disabled={busy} busy={passkeyPending} busyLabel="Waiting for your passkey…" onClick={onPasskey}>
        <Fingerprint size={20} aria-hidden="true" />{problem ? "Try passkey again" : "Sign in with a passkey"}
      </Button> : null}
      {passkeyPending ? <p className="os-signin-pending" role="status">Follow your browser’s prompt to use your passkey.</p> : null}
      <p className="os-signin-legal">By continuing, you agree to the <button type="button" className="os-link" disabled={busy} onClick={() => onLegal("/legal/tos")}>Terms of Service</button> and <button type="button" className="os-link" disabled={busy} onClick={() => onLegal("/legal/privacy")}>Privacy Notice</button>.</p>
    </section>
  </main>;
}
