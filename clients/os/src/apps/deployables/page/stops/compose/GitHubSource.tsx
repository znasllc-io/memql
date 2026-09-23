import { useEffect, useRef } from "react";
import { Button, Caption, Notice, RecordList, RecordRow, RefreshButton, Subhead } from "../../../../../kit";
import { ProblemNotice } from "../../../packages/ReportView";
import { toneFor } from "../../../packages/refusals";
import type { CredentialFeedStatus, CredentialRow } from "../../../sources/rows";
import type { useSourceInstallations } from "../../../sources/connections";
import { useGithubConnect } from "../../../sources/useGithubConnect";
import { useGithubApp } from "../../../sources/useGithubApp";
import { GithubAppMissing } from "../../../sources/GithubAppSetup";
import { returnPathFor } from "../../../sources/connectReturn";
import { InstallLink } from "../../../sources/RepositoryPicker";

/** Each body asks one question. Navigation belongs to the wizard's footer. */
export function GitHubAccountStep({ credentials, selectedId, onSelect, feed, disabled }: {
  credentials: readonly CredentialRow[]; selectedId: string; onSelect: (id: string) => void;
  feed?: CredentialFeedStatus; disabled: boolean;
}) {
  const connect = useGithubConnect();
  const app = useGithubApp();
  const ready = !feed || feed.state === "live" && !feed.error;
  const chosen = credentials.find(row => row.id === selectedId);
  return <div className="os-stop-body">
    {!ready ? <>
      <RefreshButton label="Refresh GitHub accounts" busy={feed?.state === "seeding"} onClick={() => feed?.retry()} />
      <Caption>{feed?.error || (feed?.state === "seeding" ? "Reading GitHub accounts…" : "GitHub accounts are unavailable.")}</Caption>
    </> : <>
      <Subhead meta={credentials.length}>Connected accounts</Subhead>
      <RecordList as="ul" label="GitHub accounts">{credentials.map(row => <RecordRow key={row.id}
        name={`@${row.login}`} state={row.status === "active" ? "Connected" : "Reconnect needed"}
        selected={selectedId === row.id} stateExtra={selectedId === row.id ? <span className="os-livelist-tick">Chosen</span> : undefined} disabled={disabled || connect.busy} onOpen={() => onSelect(row.id)} />)}</RecordList>
      {credentials.length === 0 ? <Caption>Connect a GitHub account to see its repositories.</Caption> : null}
      {app.status?.configured === false ? <GithubAppMissing app={app} returnPath={returnPathFor("deployables")} /> : <div className="os-form-row"><Button disabled={disabled} busy={connect.busy} onClick={() => void connect.connect(returnPathFor("deployables"))}>Add GitHub account</Button></div>}
      {chosen?.status === "revoked" ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPathFor("deployables"), chosen.id)}>Reconnect @{chosen.login}</Button> : null}
    </>}
    {connect.refusal ? <ProblemNotice problem={connect.refusal} tone={toneFor(connect.refusal.code)} /> : null}
  </div>;
}

export function GitHubOrganizationStep({ lookup, selectedId, onSelect, disabled }: {
  lookup: ReturnType<typeof useSourceInstallations>; selectedId: string; onSelect: (id: string) => void; disabled: boolean;
}) {
  const app = useGithubApp();
  const followed = useRef(false);
  const read = useRef(lookup.read); read.current = lookup.read;
  useEffect(() => {
    const returned = () => {
      if (!followed.current || document.visibilityState !== "visible") return;
      followed.current = false;
      void read.current();
    };
    window.addEventListener("focus", returned);
    document.addEventListener("visibilitychange", returned);
    return () => { window.removeEventListener("focus", returned); document.removeEventListener("visibilitychange", returned); };
  }, []);
  return <div className="os-stop-body">
    <div className="os-refresh-row"><RefreshButton label="Refresh organizations" busy={lookup.busy} onClick={() => void lookup.read()} /><Subhead meta={lookup.readAt && !lookup.busy && !lookup.refusal ? lookup.installations.length : undefined}>Organizations and personal account</Subhead></div>
    {lookup.refusal ? <ProblemNotice problem={lookup.refusal} tone={toneFor(lookup.refusal.code)} /> : null}
    {lookup.busy ? <Caption>Reading GitHub access…</Caption> : !lookup.readAt && !lookup.refusal ? <Caption>Access has not been read yet.</Caption> : lookup.readAt && lookup.installations.length === 0 && !lookup.refusal ? <Caption>No approved access yet. Install the GitHub app on your account or organization.</Caption> : null}
    <RecordList as="ul" label="Organizations and personal account">{lookup.installations.map(row => <RecordRow key={row.id} name={row.login} secondary={row.accountType === "Organization" ? "Organization" : "Personal account"}
      state={row.suspended ? "Suspended" : undefined} selected={selectedId === row.id} disabled={disabled || row.suspended || lookup.busy || Boolean(lookup.refusal)} onOpen={() => onSelect(row.id)} />)}</RecordList>
    {lookup.pending.map(row => <Notice key={row.login} tone="info" sentence={`${row.login} is awaiting an organization owner's approval.`} />)}
    <div className="os-form-row"><InstallLink installUrl={app.status?.installUrl ?? ""} text onFollow={() => { followed.current = true; }} /></div>
  </div>;
}
