import { ContentSkeleton } from "../kit/ContentSkeleton";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Fingerprint } from "lucide-react";
import { useAuth } from "./AuthProvider";
import { nativeIdentity, oauthFields, value, type IdentityData, type IdentityPage } from "./nativeIdentity";
import { Wizard } from "../kit/Wizard";
import { OwnershipWizard } from "./OwnershipWizard";
import { loginWithPasskey, registerPasskey } from "./passkeys";
import { Field, Button, Head } from "../kit/controls";
import { IdentityAccount } from "../apps/identity/IdentityAccount";
import { SignInPage, signInProblem, type SignInProblem } from "./SignInPage";
import "./identity.css";

export function IdentityScreen({ initialPath, embedded = false }: { initialPath: string; embedded?: boolean }) {
  const { config, authSource, status } = useAuth();
  const [page, setPage] = useState<IdentityPage>();
  const [path, setPath] = useState(initialPath);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [loginProblem, setLoginProblem] = useState<SignInProblem>();
  const [passkeyPending, setPasskeyPending] = useState(false);
  const actionPending = useRef(false);
  const [fields, setFields] = useState<Record<string, string>>({});
  const csrf = useRef("");
  const generation = useRef(0);

  const load = useCallback(async (next: string, form?: Record<string, string>) => {
    const revision = ++generation.current;
    setBusy(true); setError(""); setLoginProblem(undefined);
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
  const run = async (action: () => Promise<void>, signingIn = false) => {
    if (actionPending.current || busy) return;
    actionPending.current = true;
    setBusy(true); setError(""); setLoginProblem(undefined); setPasskeyPending(signingIn);
    try { await action(); } catch (err) {
      if (signingIn) setLoginProblem(signInProblem(err));
      else setError(err instanceof Error ? err.message : "Identity operation failed");
    } finally { actionPending.current = false; setBusy(false); setPasskeyPending(false); }
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

  if (page?.page === "setup_passkey") {
    const steps = ["Cluster owner", "Your installation", "Account access", ...(data.Local === true ? [] : ["Verify email"]), "Register passkey"];
    const complete = () => void run(async () => {
      const destination = await registerPasskey(config, `Bootstrap ${value(data, "EnrollmentToken")}`, "Owner passkey");
      if (!destination) throw new Error("Setup is still incomplete. Retry this step.");
      window.location.assign(destination);
    });
    return <div className="os-identity-gate"><Wizard icon={<Fingerprint />} title="Finish ownership setup"
      lead={data.Local === true ? "Your passkey is required. No email verification is used on this local installation." : "Email verified. Register a passkey before you can enter MemQL OS."}
      label="Ownership setup" open="passkey" onOpen={() => {}}
      steps={steps.map((name, i) => ({ id: i === steps.length - 1 ? "passkey" : String(i), name, state: i === steps.length - 1 ? "open" : "done", body: <><p>Create a passkey with your device or security key. Your browser will ask you to confirm.</p><p>Canceling leaves setup incomplete. You can retry this step.</p></> }))}
      notices={error ? <p role="alert">{error}</p> : undefined}
      status={{ word: busy ? "Waiting for passkey" : "Passkey required", tone: busy ? "busy" : "none" }}
      acts={busy ? [] : [{ label: data.HasProof ? "Finish setup" : "Create passkey and finish", tone: "primary", onAct: complete }, ...(error ? [{ label: "Reload setup", text: true, onAct: () => window.location.assign("/") }] : [])]} /></div>;
  }

  if (page?.page === "login") return <SignInPage data={data} clientId={config.oauthClientId} fields={fields}
    busy={busy} passkeyPending={passkeyPending} problem={loginProblem ?? (error ? signInProblem(error) : undefined)}
    onField={(name, text) => setFields(held => ({ ...held, [name]: text }))}
    onSubmit={() => submit("/login", { ...oauthFields(data), ...fields, form: data.Stage === "waitlist_signup" ? "waitlist" : data.Stage === "needs_invite" ? "invite" : "email" })}
    onPasskey={() => void run(async () => window.location.assign(await loginWithPasskey(config, oauthFields(data))), true)}
    onLegal={next => void load(next)} />;

  let title = data.Layout?.Title || "Identity";
  let body: ReactNode;
  switch (page?.page) {
    case "setup_resume":
      title = "Resume ownership setup";
      body = <><p>Ownership setup has started but is not complete.</p>
        {data.HasProof === true && act("Resume with your passkey", () => void run(async () => window.location.assign(await loginWithPasskey(config, {}))))}
        {data.Local !== true && <>{field("email", "Original owner email", "email")}{act("Verify email again", () => submit("/auth/setup/resume", fields))}</>}
        {data.Local === true && <p>Use the passkey already created for this setup. A different browser cannot replace that claim.</p>}</>;
      break;
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
      body = <IdentityAccount page={page.page} data={data} busy={busy} error={error} submit={submit} navigate={next => void load(next)} register={() => void run(async () => {
        const bearer = await authSource.bearer(); if (!bearer) throw new Error("Sign in again to add a passkey");
        await registerPasskey(config, `Bearer ${bearer}`, "My passkey"); await load("/me/devices");
      })} />; break;
    case "legal_view": body = <pre className="os-identity-legal">{value(data, "Body")}</pre>; break;
    default: body = page ? <p>{value(data, "Message") || "You can continue to MemQL OS."}</p> : busy ? <ContentSkeleton kind="form" label="Opening identity" /> : <p>Identity could not be opened.</p>;
  }
  return <div className={embedded ? "os-identity-panel" : "os-identity-gate"}><div className="os-identity-page">
    <Head title={title} />{!embedded && <Fingerprint size={32} aria-hidden="true" />}
    {data.Flash && <p role="status">{data.Flash.Message}</p>}{error && <p role="alert">{error}</p>}
    <div className="os-identity-fields">{body}</div>
    {!embedded && <p><a href="/">Return to MemQL OS</a></p>}
    {!page && !busy && act("Retry", () => void load(initialPath))}
  </div></div>;
}
