import { Button, Field } from "../kit/controls";
import { value, type IdentityData } from "./nativeIdentity";

export function deviceTitle(data: IdentityData): string {
  return data.Done ? data.DoneApproved ? "Device connected" : "Request denied"
    : data.Pending ? "Connect this device?" : "Connect a device";
}

export function DeviceApproval({ data, code, busy, blocked = false, onCode, submit }: {
  data: IdentityData; code: string; busy: boolean; blocked?: boolean;
  onCode: (code: string) => void; submit: (form: Record<string, string>) => void;
}) {
  if (data.Done) return <p role="status">{data.DoneApproved ? "Return to the app to continue." : "This device has not been given access."}</p>;
  const pending = data.Pending as IdentityData | undefined;
  if (!pending) return <form className="os-signin-form" onSubmit={event => { event.preventDefault(); if (!busy && !blocked) submit({ action: "lookup", user_code: code }); }}>
    <Field label="Device code"><input className="os-input" aria-label="Device code" autoComplete="off" autoCapitalize="characters" spellCheck={false}
      required value={code} disabled={busy || blocked} onChange={event => onCode(event.target.value)} /></Field>
    <Button type="submit" tone="primary" disabled={busy || blocked || !code.trim()}>Review device</Button>
  </form>;
  return <>
    <div className="os-device-app"><strong>{value(pending, "ClientName")}</strong>
      <p>This app will have your MemQL account access.</p>
      {pending.ClientSelfRegistered === true && <p className="os-identity-warning">This app’s name has not been verified.</p>}
    </div>
    <div className="os-device-code"><span>Match this code in the app</span><strong>{value(pending, "UserCode")}</strong></div>
    <p className="os-identity-hint">Approve only if you started this sign-in.</p>
    <details className="os-identity-details"><summary>Request details</summary>
      <dl>{[["App ID", "ClientId"], ["Source", "SourceIP"], ["Browser or device", "UserAgent"], ["Requested", "RequestedAt"], ["Expires", "ExpiresAt"]].map(([label, key]) =>
        <div key={key}><dt>{label}</dt><dd>{value(pending, key!) || "Not available"}</dd></div>)}</dl>
    </details>
    <div className="os-identity-actions">
      <Button disabled={busy || blocked} onClick={() => submit({ action: "deny", user_code: value(pending, "UserCode") })}>Deny</Button>
      <Button tone="primary" disabled={busy || blocked} busy={busy} busyLabel="Continuing…" onClick={() => submit({ action: "approve", user_code: value(pending, "UserCode") })}>Approve device</Button>
    </div>
  </>;
}
