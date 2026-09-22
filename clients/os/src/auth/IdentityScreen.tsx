import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Fingerprint } from "lucide-react";
import { useAuth } from "./AuthProvider";
import { nativeIdentity, oauthFields, value, type IdentityData, type IdentityPage } from "./nativeIdentity";
import { OwnershipWizard } from "./OwnershipWizard";
import { loginWithPasskey, registerPasskey } from "./passkeys";
import { Field, Button, Head } from "../kit/controls";
import { IdentityAccount } from "../apps/identity/IdentityAccount";
import "./identity.css";

export function IdentityScreen({ initialPath, embedded = false }: { initialPath: string; embedded?: boolean }) {
  const { config, authSource, status } = useAuth();
  const [page, setPage] = useState<IdentityPage>();
  const [path, setPath] = useState(initialPath);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});
  const csrf = useRef("");
  const generation = useRef(0);

  const load = useCallback(async (next: string, form?: Record<string, string>) => {
    const revision = ++generation.current;
    setBusy(true); setError("");
    try {
      const bearer = status === "signed-in" ? await authSource.bearer() : null;
      let result = await nativeIdentity(config, next, { form, csrf: csrf.current, bearer: bearer || undefined });
      for (let hop = 0; result.redirect && hop < 8; hop++) {
        const target = new URL(result.redirect, config.identityUrl);
        if (target.origin !== new URL(config.identityUrl).origin) {
          // A redirect is emitted only by identity after its OAuth validation.
          window.location.assign(target.toString()); return;
        }
        next = target.pathname + target.search;
        result = await nativeIdentity(config, next, { bearer: bearer || undefined });
      }
      if (revision !== generation.current) return;
      if (result.redirect || !result.page) throw new Error("Identity could not open this page");
      csrf.current = result.csrf || csrf.current;
      if (form && result.page === "error") throw new Error(value(result.data || {}, "Message") || "Identity could not complete this request");
      setPath(next); setPage(result);
      setFields({ email: value(result.data || {}, "PrefillEmail"), user_code: value(result.data || {}, "PrefillCode") });
    } catch (err) {
      if (revision === generation.current) setError(err instanceof Error ? err.message : "Identity is unavailable");
    } finally { if (revision === generation.current) setBusy(false); }
  }, [config, authSource, status]);

  useEffect(() => { void load(initialPath); return () => { generation.current++; }; }, [initialPath, load]);
  const data = page?.data || {};
  const submit = (next: string, form: Record<string, string>) => { if (!busy) void load(next, form); };
  const run = async (action: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await action(); } catch (err) { setError(err instanceof Error ? err.message : "Identity operation failed"); }
    finally { setBusy(false); }
  };
  const field = (name: string, label: string, type = "text") => <Field label={label}><input className="os-input" aria-label={label}
    type={type} value={fields[name] || ""} onChange={e => setFields(f => ({ ...f, [name]: e.target.value }))} /></Field>;
  const act = (label: string, onClick: () => void) => <Button disabled={busy} onClick={onClick}>{label}</Button>;

  // Poll only for the lifetime reported by identity; an outage is an error,
  // never evidence that an email was approved or ownership was acquired.
  useEffect(() => {
    const request = value(page?.data || {}, "RequestId");
    if (page?.page !== "check_email" || !request) return;
    const deadline = Date.now() + Number(page.data?.PollSeconds || 0) * 1000;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (stopped) return;
      if (Date.now() >= deadline) { setError("This sign-in link has expired. Request a new link."); return; }
      try {
        const response = await nativeIdentity(config, `/auth/magic-link/status?request=${encodeURIComponent(request)}`);
        if (stopped) return;
        const state = response.state as string;
        if (state === "approved") { void load("/auth/magic-link/finish", { request }); return; }
        if (state === "consumed") { window.location.assign("/"); return; }
        if (state === "expired") { setError("This sign-in link has expired."); return; }
        timer = setTimeout(poll, 2000);
      } catch { if (!stopped) setError("Verification status is unavailable. Retry when the connection returns."); }
    };
    timer = setTimeout(poll, 2000);
    return () => { stopped = true; clearTimeout(timer); };
  }, [page, config, load]);

  if (page?.page === "setup_wizard") return <div className="os-identity-gate"><OwnershipWizard data={data} busy={busy} error={error} submit={submit} /></div>;

  let title = data.Layout?.Title || "Identity";
  let body: ReactNode;
  switch (page?.page) {
    case "login": {
      const stage = value(data, "Stage");
      title = stage === "waitlist_signup" ? "Request access" : stage === "needs_invite" ? "Use your invitation" : "Sign in to MemQL OS";
      body = <>
        {data.AuthorizeMode === true && <div><p>Continue to <strong>{value(data, "ClientName")}</strong></p>
          {data.ClientSelfRegistered === true && <p>This app’s name is self-registered and has not been verified.</p>}
          <p>Client: <code>{value(data, "ClientID")}</code></p><p>Return address: <code>{value(data, "RedirectURI")}</code></p></div>}
        {field("email", "Email", "email")}
        {stage === "waitlist_signup" && <>{field("name", "Your name")}{field("additional_context", "Why would you like access?")}</>}
        {stage === "needs_invite" && field("invitation", "Invitation token")}
        <div className="os-identity-actions">{act(stage === "waitlist_signup" ? "Request access" : stage === "needs_invite" ? "Use invitation" : "Send sign-in link",
          () => submit("/login", { ...oauthFields(data), ...fields, form: stage === "waitlist_signup" ? "waitlist" : stage === "needs_invite" ? "invite" : "email" }))}
          {stage === "email" && act("Use a passkey", () => void run(async () => window.location.assign(await loginWithPasskey(config, oauthFields(data)))))}
        </div><p>By continuing you agree to the <button className="os-link" onClick={() => void load("/legal/tos")}>Terms of Service</button> and <button className="os-link" onClick={() => void load("/legal/privacy")}>Privacy Notice</button>.</p>
      </>; break;
    }
    case "check_email":
      title = "Check your email";
      body = <><p>{data.Action === "access_request_created" ? "Your access request has been received. An administrator will follow up at" : "Open the verification link sent to"} <strong>{value(data, "Email")}</strong>.</p>
        {data.Action !== "access_request_created" && <p>Return to this browser to continue. The link expires in {value(data, "ExpiresIn")}.</p>}
        {data.SharedMailboxHint === true && <p>Anyone who can read this shared mailbox can use its sign-in links. A passkey can keep access personal.</p>}
        {act("Check again", () => void load(path))}</>; break;
    case "landing":
      title = value(data, "Problem") || (data.Approved ? "Sign-in confirmed" : "Confirm sign-in");
      body = <><p>{value(data, "Message") || `Sign in as ${value(data, "MaskedEmail")}.`}</p>
        {data.Approved === true ? <p>Return to the device where you requested the link.</p> : !data.Problem && act("Continue", () => submit("/auth/landing", { token: value(data, "MagicToken") }))}</>; break;
    case "enroll":
      title = value(data, "Heading") || value(data, "LiveHeading") || "Set up your passkey";
      body = <><p>{value(data, "Message") || `Add a passkey for ${value(data, "AccountLabel")}.`}</p><p>{value(data, "NextStep") || value(data, "SingleUseNote")}</p>
        {!data.Rejection && <>{field("label", "Passkey name")}{act("Add passkey", () => void run(async () => {
          const code = new URL(path, config.identityUrl).searchParams.get("code") || "";
          await registerPasskey(config, `${value(data, "AuthScheme") || "Enrolment"} ${code}`, fields.label || "My passkey");
          setPage({ page: "message", data: { Layout: { Title: "Passkey added", BrandName: "MemQL" }, Message: "Your passkey is ready. You can now sign in." } });
        }))}</>}</>; break;
    case "invitation":
      title = value(data, "Heading") || "Accept invitation";
      body = <><p>{value(data, "Message") || `${value(data, "InviterName") || "An administrator"} invited ${value(data, "InviteeEmail")} to join.`}</p>
        <p>{value(data, "NextStep") || `Role: ${value(data, "Role") || "Cluster default"}. Expires in ${value(data, "ExpiresIn")}.`}</p>
        {data.StepUp === true && <p>We’ll verify your email before you receive this privileged role.</p>}
        {!data.Rejection && act("Accept invitation", () => submit("/invitation/accept", { code: value(data, "Code") }))}</>; break;
    case "device": {
      const pending = data.Pending as IdentityData | null;
      body = data.Done ? <p>{data.DoneApproved ? "Device approved. Return to your device." : "Device request denied."}</p>
        : pending ? <><p>Approve only a device you are signing in on.</p><p>{value(pending, "ClientName")} · {value(pending, "ClientId")}</p>
          {pending.ClientSelfRegistered === true && <p>The client name has not been verified.</p>}
          <p>Code: {value(pending, "UserCode")}</p><p>Source: {value(pending, "SourceIP")}</p><p>{value(pending, "UserAgent")}</p>
          <p>Requested {value(pending, "RequestedAt")} · Expires {value(pending, "ExpiresAt")}</p>
          {act("Deny", () => submit("/device", { action: "deny", user_code: value(pending, "UserCode") }))}
          {act("Approve device", () => submit("/device", { action: "approve", user_code: value(pending, "UserCode") }))}</>
          : <>{field("user_code", "Device code")}{act("Review device", () => submit("/device", { action: "lookup", user_code: fields.user_code || "" }))}</>;
      break;
    }
    case "me/profile":
      body = <dl>{Object.entries((data.Profile || {}) as Record<string, string>).map(([label, text]) => <div key={label}><dt>{label}</dt><dd>{text || "Not set"}</dd></div>)}</dl>; break;
    case "me/devices": case "me/settings": case "me/tokens": case "me/dashboard":
      body = <IdentityAccount page={page.page} data={data} busy={busy} submit={submit} navigate={next => void load(next)} register={() => void run(async () => {
        const bearer = await authSource.bearer(); if (!bearer) throw new Error("Sign in again to add a passkey");
        await registerPasskey(config, `Bearer ${bearer}`, "My passkey"); await load("/me/devices");
      })} />; break;
    case "legal_view": body = <pre className="os-identity-legal">{value(data, "Body")}</pre>; break;
    default: body = page ? <p>{value(data, "Message") || "You can continue to MemQL OS."}</p> : <p>{busy ? "Opening identity…" : "Identity could not be opened."}</p>;
  }
  return <div className={embedded ? "os-identity-panel" : "os-identity-gate"}><div className="os-identity-page">
    <Head title={title} />{!embedded && <Fingerprint size={32} aria-hidden="true" />}
    {data.Flash && <p role="status">{data.Flash.Message}</p>}{error && <p role="alert">{error}</p>}
    <div className="os-identity-fields">{body}</div>
    {!embedded && <p><a href="/">Return to MemQL OS</a></p>}
    {!page && !busy && act("Retry", () => void load(initialPath))}
  </div></div>;
}
