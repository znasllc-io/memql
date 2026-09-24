import { useEffect, useRef, useState } from "react";
import { Building2 } from "lucide-react";
import { Button, Caption, EmptyState, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { AddButton } from "../../../kit/AddButton";
import { useWrite, type Refusal } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import { createSourceConnection, removeSourceConnection, useSourceConnections, useSourceInstallations, type SourceConnectionRow, type SourceInstallation } from "./connections";
import { InstallLink } from "./RepositoryPicker";
import { useGithubApp } from "./useGithubApp";
import { GitHubOrganizationDetails } from "./GitHubOrganizationDetails";
import type { CredentialRow } from "./rows";

type Page = { kind: "account" } | { kind: "add" } | { kind: "organization"; installation: SourceInstallation; connection?: SourceConnectionRow; from: "account" | "add" };

export function GitHubAccountDetails({ account, sourceNames, onBack, busy, refusal, onDisconnect }: {
  account: CredentialRow; sourceNames: string[]; onBack: () => void;
  busy: boolean; refusal: Refusal | null; onDisconnect: () => void;
}) {
  const [page, setPage] = useState<Page>({ kind: "account" });
  const [armed, setArmed] = useState(false);
  const connections = useSourceConnections();
  const access = useSourceInstallations(account.id);
  const app = useGithubApp();
  const write = useWrite();
  const followedInstall = useRef(false);
  const readAccess = useRef(access.read); readAccess.current = access.read;
  useEffect(() => {
    const returned = () => {
      if (!followedInstall.current || document.visibilityState !== "visible") return;
      followedInstall.current = false;
      void readAccess.current();
    };
    window.addEventListener("focus", returned);
    document.addEventListener("visibilitychange", returned);
    return () => { window.removeEventListener("focus", returned); document.removeEventListener("visibilitychange", returned); };
  }, []);
  const name = `@${account.login || account.label}`;
  const linked = connections.rows.filter(row => row.credentialId === account.id);
  const loading = (!access.readAt && !access.refusal) || connections.state === "seeding";
  const pending = busy || write.busy;
  const open = (next: Page) => { setArmed(false); write.clear(); setPage(next); };
  const back = () => page.kind === "account" ? onBack() : open({ kind: page.kind === "organization" ? page.from : "account" });
  const saveOrganization = async () => {
    if (page.kind !== "organization") return;
    const selected = page;
    const done = await write.run(async query => selected.connection
      ? removeSourceConnection(query, selected.connection.id)
      : createSourceConnection(query, account.id, selected.installation.id));
    if (done !== null) { connections.retry(); open({ kind: "account" }); }
  };
  const backAct: Act = { label: "Back", text: true, busy: pending, onAct: back };
  const cancelAct: Act = { label: "Cancel", text: true, busy: pending, onAct: () => setArmed(false) };
  let acts: Act[] = [backAct];
  let state = loading ? "Loading organizations" : "Connected";
  if (page.kind === "account") {
    state = busy ? "Disconnecting" : armed ? "Confirm disconnect" : state;
    acts = [armed ? cancelAct : backAct, { label: "Disconnect", busy, tone: armed ? "danger" : "quiet", onAct: armed ? onDisconnect : () => setArmed(true) }];
  } else state = loading ? "Loading organizations" : "Choose an organization";
  const available = access.installations.filter(installation => !linked.some(row => row.installationId === installation.id));
  const title = page.kind === "account" ? name : page.kind === "add" ? "Add organization" : page.installation.login;

  if (page.kind === "organization") return <GitHubOrganizationDetails key={page.installation.id}
    account={account} installation={page.installation} connection={page.connection}
    busy={write.busy} refusal={write.refusal} onSave={() => void saveOrganization()}
    onBack={back} onAccount={() => open({ kind: "account" })} onSettings={onBack} />;

  return <section className="os-action-pane" aria-label={`GitHub account ${name}`}>
    <div className="os-action-body os-app-stack">
      <Head title={title} back={{ label: page.kind === "account" ? "Settings" : name, onSelect: back }}
        breadcrumbs={[{ label: "Settings", onSelect: onBack }, ...(page.kind === "account" ? [{ label: name }] : [{ label: name, onSelect: () => open({ kind: "account" }) }, { label: title }])]} />
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {write.refusal ? <ProblemNotice problem={write.refusal} tone="error" /> : null}
      {armed ? <Notice tone="warn" sentence="Revoke this account's access here and at GitHub? Sources and deployables are kept."
        detail={sourceNames.length ? `${sourceNames.length} sources will need this account reconnected to fetch updates.` : undefined} /> : null}
      {page.kind === "account" ? <>
        <Panel label="Connection"><Subhead>Connection</Subhead><Facts><Fact label="Host" value={account.host} /></Facts></Panel>
        <Panel label="Organizations">
          <div className="os-head"><Subhead>Organizations</Subhead><div className="os-head-actions"><AddButton label="Add organization" onClick={() => open({ kind: "add" })} /></div></div>
          <div className="os-record-slot">
            {loading ? <RecordListSkeleton label="Loading organizations" /> : <RecordList as="ul" label="Organizations">{linked.map(connection => {
              const installation = access.installations.find(row => row.id === connection.installationId);
              return <RecordRow key={connection.id} icon={<Building2 size={18} aria-hidden />} name={connection.accountLogin}
                secondary={connection.accountType === "Organization" ? "Organization" : "Personal account"}
                state={installation?.suspended ? "Suspended" : installation ? "Available" : access.refusal ? "Unknown" : "Access unavailable"}
                tone={installation?.suspended ? "warn" : installation ? "accent" : "muted"}
                onOpen={() => open({ kind: "organization", connection, from: "account", installation: installation ?? {
                  id: connection.installationId, login: connection.accountLogin, providerAccountId: connection.providerAccountId,
                  accountType: connection.accountType, repositorySelection: "", suspended: false } })} label={`Manage organization ${connection.accountLogin}`} />;
            })}</RecordList>}
            {!loading && connections.state === "live" && linked.length === 0 ? <EmptyState icon={Building2} title="No organizations connected">Use the plus button to add an organization.</EmptyState> : null}
          </div>
          {connections.error ? <Notice tone="warn" sentence="Organizations could not be read."><Button onClick={connections.retry}>Try again</Button></Notice> : null}
          {access.refusal ? <><ProblemNotice problem={access.refusal} tone="error" /><Button onClick={() => void access.read()}>Try again</Button></> : null}
        </Panel>
      </> : <Panel label="Available organizations">
        <div className="os-head"><Subhead>Organizations</Subhead><div className="os-head-actions"><InstallLink compact installUrl={app.status?.installUrl ?? ""} onFollow={() => { followedInstall.current = true; }} /></div></div>
        <div className="os-record-slot">
          {loading ? <RecordListSkeleton label="Loading organizations" /> : <RecordList as="ul" label="Available organizations">{available.map(installation => <RecordRow key={installation.id}
            icon={<Building2 size={18} aria-hidden />} name={installation.login} state={installation.suspended ? "Suspended" : "Available"}
            tone={installation.suspended ? "warn" : "accent"} disabled={installation.suspended}
            onOpen={() => open({ kind: "organization", installation, from: "add" })} label={`Select organization ${installation.login}`} />)}</RecordList>}
          {!loading && access.readAt && !access.refusal && available.length === 0 ? <EmptyState icon={Building2} title="No additional organizations">Use the plus button to grant access at GitHub.</EmptyState> : null}
        </div>
        {access.refusal ? <><ProblemNotice problem={access.refusal} tone="error" /><Button onClick={() => void access.read()}>Try again</Button></> : null}
        {access.pending.map(organization => <Caption key={organization.login}>{organization.login} is awaiting an organization owner's approval.</Caption>)}
      </Panel>}
    </div>
    <ActionBar state={state} tone={pending || loading ? "busy" : armed ? "paused" : "live"} live acts={acts} />
  </section>;
}
