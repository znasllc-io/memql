import { RecordList, RecordRow } from "../../kit/RecordRow";
import { useState } from "react";
import { Button, Field, Subhead } from "../../kit/controls";
import { value, type IdentityData } from "../../auth/nativeIdentity";

const rows = (data: IdentityData, key: string): IdentityData[] => Array.isArray(data[key]) ? data[key] as IdentityData[] : [];
export function IdentityAccount({ page, data, busy, error = "", submit, navigate, register }: {
  page: string; data: IdentityData; busy: boolean; error?: string;
  submit: (path: string, form: Record<string, string>) => void;
  navigate: (path: string) => void; register: () => void;
}) {
  const counted = !busy && !error;
  const [label, setLabel] = useState("");
  const [rename, setRename] = useState<Record<string, string>>({});
  const button = (title: string, path: string, form: Record<string, string>) => <Button disabled={busy} onClick={() => submit(path, form)}>{title}</Button>;
  if (page === "me/devices") {
    const warning = data.RevokeWarning as IdentityData | undefined;
    return <>
      {warning && <section role="alert"><h2>{value(warning, "Headline")}</h2><p>{value(warning, "Detail")}</p>
        {button("Revoke passkey", "/me/devices/passkeys/revoke", { id: value(warning, "ID"), confirm: "yes" })}
        <Button onClick={() => navigate("/me/devices")}>Keep passkey</Button></section>}
      <Subhead meta={counted && data.PasskeysEnabled === true && Array.isArray(data.Passkeys) ? rows(data, "Passkeys").length : undefined}>Passkeys</Subhead><p>{value(data, "RoutesSummary")}</p>
      {data.PasskeysEnabled !== true ? <p>Passkey management is unavailable.</p> : <>
        {rows(data, "Passkeys").length === 0 && <p>You have no passkeys yet.</p>}
        <RecordList as="ul" label="Passkeys">{rows(data, "Passkeys").map(row => <RecordRow key={value(row, "ID")} name={value(row, "Label")}
          state={row.Active ? "Active" : "Revoked"} tone={row.Active ? "accent" : "muted"} dim={!row.Active}
          secondary={`${value(row, "Authenticator") || "Authenticator"} · ${value(row, "Backup")}`}
          actions={row.Active === true ? <><Field label="Passkey name"><input className="os-input" aria-label={`Name for ${value(row, "Label")}`} value={rename[value(row, "ID")] ?? value(row, "Label")}
            onChange={e => setRename(names => ({ ...names, [value(row, "ID")]: e.target.value }))} /></Field>
            {button("Save name", "/me/devices/passkeys/rename", { id: value(row, "ID"), label: rename[value(row, "ID")] ?? value(row, "Label") })}
            {button("Review revocation", "/me/devices/passkeys/revoke", { id: value(row, "ID") })}</> : undefined}>
          <span>{value(row, "BackupDetail")}</span><span>Created {value(row, "CreatedAt")}</span><span>Last used {value(row, "LastUsedAt") || "Never"}</span>
        </RecordRow>)}</RecordList><Button disabled={busy} onClick={register}>Add passkey</Button></>}
      <Subhead meta={counted && data.SessionsAvailable === true && Array.isArray(data.Sessions) ? rows(data, "Sessions").length : undefined}>Sessions</Subhead>{data.SessionsAvailable !== true ? <p>Session information is unavailable.</p> : <>
        {rows(data, "Sessions").length === 0 && <p>No active sessions.</p>}
        <RecordList as="ul" label="Sessions">{rows(data, "Sessions").map(row => <RecordRow key={value(row, "ID")} name={value(row, "Label")}
          secondary={value(row, "Origin")} state={row.ThisDevice ? "This device" : "Active"}
          actions={button(row.ThisDevice ? "Sign out here" : "End session", "/me/devices/sessions/revoke", { id: value(row, "ID") })}>
          <span>Signed in {value(row, "FirstSeen")}</span><span>Last seen {value(row, "LastSeen")}</span><span>Expires {value(row, "Expires")}</span>
        </RecordRow>)}</RecordList>
        {button("Sign out on all devices", "/me/devices/revoke-all", {})}</>}
    </>;
  }
  if (page === "me/tokens") return <>
    <p>Personal access tokens give scripts and command-line clients your access. Store them like passwords.</p>
    {data.NewToken && <div role="status"><p>Save this token now. It will not be shown again.</p><code>{value(data, "NewToken")}</code></div>}
    <Field label="Token name"><input className="os-input" aria-label="Token name" value={label} onChange={e => setLabel(e.target.value)} /></Field>
    {label.trim() && button("Generate token", "/me/tokens", { label })}
    <Subhead meta={counted && Array.isArray(data.Tokens) ? rows(data, "Tokens").length : undefined}>Tokens</Subhead>
    <RecordList as="ul" label="Tokens">{rows(data, "Tokens").map(row => <RecordRow key={value(row, "ID")} name={value(row, "Label")}
      state={row.Active ? "Active" : "Revoked"} tone={row.Active ? "accent" : "muted"} dim={!row.Active}
      secondary={`Last used ${value(row, "LastUsedAt") || "Never"}`}
      actions={row.Active === true ? button("Revoke token", "/me/tokens/revoke", { id: value(row, "ID") }) : undefined} />)}</RecordList>
    {data.NextCursor && <Button onClick={() => navigate(`/me/tokens?cursor=${encodeURIComponent(value(data, "NextCursor"))}`)}>Show more</Button>}
  </>;
  const security = data.SignInSecurity as IdentityData | undefined;
  return <>
    <p>Manage how you sign in and which devices can access your account.</p>
    {!security?.Available ? <p>Sign-in policy is unavailable.</p> : <>
      {data.Local === true ? <p>This local installation requires passkeys. Email sign-in is unavailable.</p> : <><h2>Sign-in links</h2><p>{security.PasskeyOnly ? "Email sign-in links are turned off." : "You can sign in with an email link or a passkey."}</p>
      {security.PasskeyOnly ? button("Allow email sign-in links", "/me/settings/sign-in-policy", { policy: "any" })
        : security.PasskeyCountKnown && Number(security.ActivePasskeys) > 0 ? button("Require a passkey", "/me/settings/sign-in-policy", { policy: "passkey_only" })
        : <p>Add a passkey before turning off email sign-in links.</p>}</>}
      <h2>Shared mailbox</h2><p>{security.SharedMailbox ? "This account uses a shared mailbox." : "This account uses a personal mailbox."}</p>
      {button(security.SharedMailbox ? "Mark mailbox as personal" : "Mark mailbox as shared", "/me/settings/shared-mailbox", { shared: security.SharedMailbox ? "false" : "true" })}
    </>}
    <Button onClick={() => navigate("/me/devices")}>Manage passkeys and sessions</Button>
  </>;
}
