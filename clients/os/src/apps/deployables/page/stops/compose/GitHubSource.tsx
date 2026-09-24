import { useEffect, useRef } from "react";
import { Button, Caption, Notice, RecordList, RecordRow } from "../../../../../kit";
import { AddButton } from "../../../../../kit/AddButton";
import { WizardStepHeader } from "../../../../../kit/WizardStepHeader";
import { ProblemNotice } from "../../../packages/ReportView";
import { toneFor } from "../../../packages/refusals";
import type { CredentialFeedStatus, CredentialRow } from "../../../sources/rows";
import type { useSourceInstallations } from "../../../sources/connections";
import { useGithubConnect } from "../../../sources/useGithubConnect";
import { useGithubApp } from "../../../sources/useGithubApp";
import { GithubAppMissing } from "../../../sources/GithubAppSetup";
import { returnPathFor } from "../../../sources/connectReturn";
import { InstallLink } from "../../../sources/RepositoryPicker";

/** A row answers one question and advances; Back belongs to the wizard footer. */
export function GitHubAccountStep({ credentials, selectedId, onSelect, feed, disabled }: {
  credentials: readonly CredentialRow[]; selectedId: string; onSelect: (id: string) => void;
  feed?: CredentialFeedStatus; disabled: boolean;
}) {
  const connect = useGithubConnect();
  const app = useGithubApp();
  const ready = !feed || feed.state === "live" && !feed.error;
  const connected = credentials.filter(row => row.status === "active");
  return <div className="os-stop-body">
    <WizardStepHeader count={ready ? connected.length : undefined}>
      {ready && app.status?.configured !== false ? <AddButton label="Add GitHub account" disabled={disabled || connect.busy} aria-busy={connect.busy} onClick={() => void connect.connect(returnPathFor("deployables"))} /> : null}
    </WizardStepHeader>
    {!ready ? <>
      <Caption>{feed?.error || (feed?.state === "seeding" ? "Reading GitHub accounts…" : "GitHub accounts are unavailable.")}</Caption>
      {feed && feed.state !== "seeding" ? <Button onClick={() => feed.retry()}>Try again</Button> : null}
    </> : <>
      <RecordList as="ul" label="GitHub accounts">{connected.map(row => <RecordRow key={row.id}
        name={`@${row.login}`} state="Connected"
        selected={selectedId === row.id} disabled={disabled || connect.busy} onOpen={() => onSelect(row.id)} />)}</RecordList>
      {connected.length === 0 ? <Caption>Connect a GitHub account to see its repositories.</Caption> : null}
      {app.status?.configured === false ? <GithubAppMissing app={app} returnPath={returnPathFor("deployables")} /> : null}
    </>}
    {connect.refusal ? <ProblemNotice problem={connect.refusal} tone={toneFor(connect.refusal.code)} /> : null}
  </div>;
}

export function GitHubOrganizationStep({ lookup, selectedId, onSelect, disabled, pending = false }: {
  lookup: ReturnType<typeof useSourceInstallations>; selectedId: string; onSelect: (id: string) => void; disabled: boolean; pending?: boolean;
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
    <WizardStepHeader count={lookup.readAt && !lookup.busy && !lookup.refusal ? lookup.installations.length : undefined}>
      <InstallLink compact installUrl={app.status?.installUrl ?? ""} onFollow={() => { followed.current = true; }} />
    </WizardStepHeader>
    {lookup.refusal ? <><ProblemNotice problem={lookup.refusal} tone={toneFor(lookup.refusal.code)} /><Button disabled={lookup.busy} onClick={() => void lookup.read()}>Try again</Button></> : null}
    {pending ? <Caption>Checking organization access…</Caption> : null}
    {lookup.busy ? <Caption>Reading GitHub access…</Caption> : !lookup.readAt && !lookup.refusal ? <Caption>Access has not been read yet.</Caption> : lookup.readAt && lookup.installations.length === 0 && !lookup.refusal ? <Caption>No approved access yet. Install the GitHub app on your account or organization.</Caption> : null}
    <RecordList as="ul" label="Organizations and personal account">{lookup.installations.map(row => <RecordRow key={row.id} name={row.login} secondary={row.accountType === "Organization" ? "Organization" : "Personal account"}
      state={row.suspended ? "Suspended" : pending && selectedId === row.id ? "Checking access" : undefined} selected={selectedId === row.id} disabled={disabled || pending || row.suspended || lookup.busy || Boolean(lookup.refusal)} onOpen={() => onSelect(row.id)} />)}</RecordList>
    {lookup.pending.map(row => <Notice key={row.login} tone="info" sentence={`${row.login} is awaiting an organization owner's approval.`} />)}
  </div>;
}
