import { useState } from "react";
import { UserRound } from "lucide-react";
import { Button, Caption, EmptyState, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { sourceName } from "../list";
import type { PackageRow } from "../packages/rows";
import { ProblemNotice } from "../packages/ReportView";
import type { CredentialFeedStatus, CredentialRow } from "./rows";
import { useSourceConnections, useSourceInstallations } from "./connections";
import { useCredentialRevoke, useGithubConnect } from "./useGithubConnect";
import { useGithubApp } from "./useGithubApp";
import { GithubAppMissing } from "./GithubAppSetup";
import { DisconnectGitHub } from "./ConnectedAccountCard";
import { returnPathFor, type ConnectReturn } from "./connectReturn";
import { ConnectReturnNotice } from "./ConnectReturnNotice";

export function GitHubAccountsSettings({ accounts, packages, feed, connectResult }: {
  accounts: readonly CredentialRow[];
  packages: readonly PackageRow[];
  feed: CredentialFeedStatus;
  connectResult: ConnectReturn | null;
}) {
  const [selectedId, setSelectedId] = useState("");
  const connect = useGithubConnect();
  const app = useGithubApp();
  const selected = accounts.find(account => account.id === selectedId);
  const ready = feed.state === "live" && !feed.error;

  if (selected) return <GitHubAccountDetails key={selected.id} account={selected}
    sourceNames={packages.filter(pkg => pkg.credentialId === selected.id && pkg.status !== "archived").map(sourceName)}
    onBack={() => setSelectedId("")} retry={feed.retry} />;

  return <section className="os-settings os-settings-wide deployable-settings" aria-label="Deployables settings">
    <Head title="Settings" />
    <section className="os-field-group" aria-label="GitHub accounts settings">
      <div className="os-head">
        <Subhead>GitHub accounts</Subhead>
        <div className="os-head-actions">
          {app.status?.configured !== false ? <AddButton label="Add GitHub account" disabled={connect.busy} aria-busy={connect.busy} onClick={() => void connect.connect(returnPathFor("settings"))} /> : null}
        </div>
      </div>
      <ConnectReturnNotice result={connectResult} />
      {connect.refusal ? <ProblemNotice problem={connect.refusal} tone="error" /> : null}
      <RecordList as="ul" label="GitHub accounts">{accounts.map(account => <RecordRow key={account.id}
        icon={<UserRound size={18} aria-hidden />} name={`@${account.login || account.label}`}
        secondary="GitHub" state={account.status === "active" ? "Connected" : "Disconnected"}
        tone={account.status === "active" ? "accent" : "muted"} current={account.status === "active"}
        onOpen={() => setSelectedId(account.id)} label={`Manage GitHub account ${account.login || account.label}`}
      />)}</RecordList>
      {ready && accounts.length === 0 ? <EmptyState title="No GitHub accounts connected">Connect an account to choose its organizations and repositories when adding a deployable.</EmptyState> : null}
      {!ready ? <Notice sentence={feed.state === "seeding" ? "Reading GitHub accounts…" : "GitHub accounts could not be read."} detail={feed.error || undefined}>
        {feed.state !== "seeding" ? <Button onClick={feed.retry}>Try again</Button> : null}
      </Notice> : null}
      {app.status?.configured === false ? <GithubAppMissing app={app} returnPath={returnPathFor("settings")} /> : null}
    </section>
  </section>;
}

function GitHubAccountDetails({ account, sourceNames, onBack, retry }: {
  account: CredentialRow; sourceNames: string[]; onBack: () => void; retry: () => void;
}) {
  const connect = useGithubConnect();
  const revoke = useCredentialRevoke();
  const connections = useSourceConnections();
  const connected = account.status === "active" && revoke.revokedCredentialId !== account.id;
  const access = useSourceInstallations(connected ? account.id : "");
  const name = `@${account.login || account.label}`;
  return <section className="os-app-stack" aria-label={`GitHub account ${name}`}>
    <Head title={name} back={{ label: "Settings", onSelect: onBack }} breadcrumbs={[{ label: "Settings", onSelect: onBack }, { label: name }]}>
      {!connected ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPathFor("settings"), account.id)}>Reconnect GitHub account</Button> : null}
    </Head>
    {connect.refusal ? <ProblemNotice problem={connect.refusal} tone="error" /> : null}
    <Panel label="Connection">
      <Subhead>Connection</Subhead>
      <Facts>
        <Fact label="Status" value={<span className="os-record-status" data-tone={connected ? "accent" : "muted"}>{connected ? "Connected" : "Disconnected"}</span>} />
        <Fact label="Host" value={account.host} />
      </Facts>
      {connected ? <DisconnectGitHub inline sourceNames={sourceNames} busy={revoke.busy} refusal={revoke.refusal} onDisconnect={() => {
        void revoke.revoke(account.id).then(done => { if (done) { connections.noteCredentialRevoked(account.id); retry(); } });
      }} /> : null}
    </Panel>
    {connected ? <Panel label="Repository access">
      <Subhead>Repository access</Subhead>
      <RecordList as="ul" label="Organizations and personal account">{access.installations.map(installation => <RecordRow key={installation.id}
        name={installation.login} secondary={installation.accountType === "Organization" ? "Organization" : "Personal account"}
        state={installation.suspended ? "Suspended" : "Available"} tone={installation.suspended ? "warn" : "accent"} />)}</RecordList>
      {access.busy ? <Caption>Reading repository access…</Caption> : null}
      {access.refusal ? <><ProblemNotice problem={access.refusal} tone="error" /><Button onClick={() => void access.read()}>Try again</Button></> : null}
      {access.readAt && !access.busy && !access.refusal && !access.installations.length ? <Caption>No repository access has been approved yet.</Caption> : null}
      {access.pending.map(organization => <Caption key={organization.login}>{organization.login} is awaiting an organization owner's approval.</Caption>)}
    </Panel> : null}
    {revoke.remoteRevoked === false ? <Notice tone="warn" sentence="Disconnected here, but GitHub did not confirm the authorization ended." next="Remove the authorization under Applications in your GitHub settings." /> : null}
  </section>;
}
