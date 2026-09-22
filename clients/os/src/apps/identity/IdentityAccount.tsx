import { useState } from "react";
import { Button, Field } from "../../kit/controls";
import { value, type IdentityData } from "../../auth/nativeIdentity";

const rows = (data: IdentityData, key: string): IdentityData[] => Array.isArray(data[key]) ? data[key] as IdentityData[] : [];
export function IdentityAccount({ page, data, busy, submit, navigate, register }: {
  page: string; data: IdentityData; busy: boolean;
  submit: (path: string, form: Record<string, string>) => void;
  navigate: (path: string) => void; register: () => void;
}) {
  const [label, setLabel] = useState("");
  const [rename, setRename] = useState<Record<string, string>>({});
  const button = (title: string, path: string, form: Record<string, string>) => <Button disabled={busy} onClick={() => submit(path, form)}>{title}</Button>;
  if (page === "me/devices") {
    const warning = data.RevokeWarning as IdentityData | undefined;
    return <>
      {warning && <section role="alert"><h2>{value(warning, "Headline")}</h2><p>{value(warning, "Detail")}</p>
        {button("Revoke passkey", "/me/devices/passkeys/revoke", { id: value(warning, "ID"), confirm: "yes" })}
        <Button onClick={() => navigate("/me/devices")}>Keep passkey</Button></section>}
      <h2>Passkeys</h2><p>{value(data, "RoutesSummary")}</p>
      {data.PasskeysEnabled !== true ? <p>Passkey management is unavailable.</p> : <>
        {rows(data, "Passkeys").length === 0 && <p>You have no passkeys yet.</p>}
        {rows(data, "Passkeys").map(row => <div className="os-identity-record" key={value(row, "ID")}>
          <strong>{value(row, "Label")}</strong><p>{row.Active ? "Active" : "Revoked"} · {value(row, "Authenticator") || "Authenticator"} · {value(row, "Backup")}</p>
          <p>{value(row, "BackupDetail")}</p><p>Created {value(row, "CreatedAt")} · Last used {value(row, "LastUsedAt") || "Never"}</p>
          {row.Active === true && <><Field label="Passkey name"><input className="os-input" aria-label={`Name for ${value(row, "Label")}`} value={rename[value(row, "ID")] ?? value(row, "Label")}
            onChange={e => setRename(names => ({ ...names, [value(row, "ID")]: e.target.value }))} /></Field>
            {button("Save name", "/me/devices/passkeys/rename", { id: value(row, "ID"), label: rename[value(row, "ID")] ?? value(row, "Label") })}
            {button("Review revocation", "/me/devices/passkeys/revoke", { id: value(row, "ID") })}</>}
        </div>)}<Button disabled={busy} onClick={register}>Add passkey</Button></>}
      <h2>Sessions</h2>{data.SessionsAvailable !== true ? <p>Session information is unavailable.</p> : <>
        {rows(data, "Sessions").length === 0 && <p>No active sessions.</p>}
        {rows(data, "Sessions").map(row => <div className="os-identity-record" key={value(row, "ID")}><strong>{value(row, "Label")}</strong>
          <p>{value(row, "Origin")}{row.ThisDevice ? " · This device" : ""}</p><p>Signed in {value(row, "FirstSeen")} · Last seen {value(row, "LastSeen")} · Expires {value(row, "Expires")}</p>
          {button(row.ThisDevice ? "Sign out here" : "End session", "/me/devices/sessions/revoke", { id: value(row, "ID") })}</div>)}
        {button("Sign out on all devices", "/me/devices/revoke-all", {})}</>}
    </>;
  }
  if (page === "me/tokens") return <>
    <p>Personal access tokens give scripts and command-line clients your access. Store them like passwords.</p>
    {data.NewToken && <div role="status"><p>Save this token now. It will not be shown again.</p><code>{value(data, "NewToken")}</code></div>}
    <Field label="Token name"><input className="os-input" aria-label="Token name" value={label} onChange={e => setLabel(e.target.value)} /></Field>
    {label.trim() && button("Generate token", "/me/tokens", { label })}
    {rows(data, "Tokens").map(row => <div className="os-identity-record" key={value(row, "ID")}><strong>{value(row, "Label")}</strong>
      <p>{row.Active ? "Active" : "Revoked"} · Last used {value(row, "LastUsedAt") || "Never"}</p>
      {row.Active === true && button("Revoke token", "/me/tokens/revoke", { id: value(row, "ID") })}</div>)}
    {data.NextCursor && <Button onClick={() => navigate(`/me/tokens?cursor=${encodeURIComponent(value(data, "NextCursor"))}`)}>Show more</Button>}
  </>;
  const security = data.SignInSecurity as IdentityData | undefined;
  return <>
    <p>Manage how you sign in and which devices can access your account.</p>
    {!security?.Available ? <p>Sign-in policy is unavailable.</p> : <>
      <h2>Sign-in links</h2><p>{security.PasskeyOnly ? "Email sign-in links are turned off." : "You can sign in with an email link or a passkey."}</p>
      {security.PasskeyOnly ? button("Allow email sign-in links", "/me/settings/sign-in-policy", { policy: "any" })
        : security.PasskeyCountKnown && Number(security.ActivePasskeys) > 0 ? button("Require a passkey", "/me/settings/sign-in-policy", { policy: "passkey_only" })
        : <p>Add a passkey before turning off email sign-in links.</p>}
      <h2>Shared mailbox</h2><p>{security.SharedMailbox ? "This account uses a shared mailbox." : "This account uses a personal mailbox."}</p>
      {button(security.SharedMailbox ? "Mark mailbox as personal" : "Mark mailbox as shared", "/me/settings/shared-mailbox", { shared: security.SharedMailbox ? "false" : "true" })}
    </>}
    <Button onClick={() => navigate("/me/devices")}>Manage passkeys and sessions</Button>
  </>;
}
