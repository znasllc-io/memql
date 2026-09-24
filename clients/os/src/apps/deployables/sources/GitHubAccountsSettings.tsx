import { useState } from "react";
import { UserRound } from "lucide-react";
import { Button, Caption, EmptyState, Fact, Facts, Head, Notice, RecordList, RecordRow, Subhead } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { sourceName } from "../list";
import type { PackageRow } from "../packages/rows";
import { ProblemNotice } from "../packages/ReportView";
import type { CredentialFeedStatus, CredentialRow } from "./rows";
import { useSourceConnections, useSourceInstallations } from "./connections";
import { useCredentialRevoke, useGithubConnect } from "./useGithubConnect";
import { useGithubApp } from "./useGithubApp";
import { GithubAppMissing } from "./GithubAppSetup";
import { DisconnectGitHub } from "./ConnectedAccountCard";
import { InstallLink } from "./RepositoryPicker";
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
    onBack={() => setSelectedId("")} retry={feed.retry} installUrl={app.status?.installUrl ?? ""} />;

  return <section className="os-settings os-settings-wide deployable-settings" aria-label="GitHub accounts settings">
    <Head title="GitHub accounts">
      {app.status?.configured !== false ? <Button busy={connect.busy} busyLabel="Opening GitHub" onClick={() => void connect.connect(returnPathFor("settings"))}>Connect GitHub account</Button> : null}
    </Head>
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
  </section>;
}

function GitHubAccountDetails({ account, sourceNames, onBack, retry, installUrl }: {
  account: CredentialRow; sourceNames: string[]; onBack: () => void; retry: () => void; installUrl: string;
}) {
  const connect = useGithubConnect();
  const revoke = useCredentialRevoke();
  const connections = useSourceConnections();
  const connected = account.status === "active" && revoke.revokedCredentialId !== account.id;
  const access = useSourceInstallations(connected ? account.id : "");
  const name = `@${account.login || account.label}`;
  return <section className="os-settings os-settings-wide deployable-settings" aria-label={`GitHub account ${name}`}>
    <Head title={name} back={{ label: "GitHub accounts", onSelect: onBack }} breadcrumbs={[{ label: "GitHub accounts", onSelect: onBack }, { label: name }]}>
      {!connected ? <Button busy={connect.busy} onClick={() => void connect.connect(returnPathFor("settings"), account.id)}>Reconnect GitHub account</Button> : null}
    </Head>
    <Facts>
      <Fact label="Connection" value={connected ? "Connected" : "Disconnected"} />
      {account.createdAt ? <Fact label="Connected since" value={formatMoment(account.createdAt)} /> : null}
    </Facts>
    {connect.refusal ? <ProblemNotice problem={connect.refusal} tone="error" /> : null}
    {connected ? <>
      <Subhead>Repository access</Subhead>
      <RecordList as="ul" label="Organizations and personal account">{access.installations.map(installation => <RecordRow key={installation.id}
        name={installation.login} secondary={installation.accountType === "Organization" ? "Organization" : "Personal account"}
        state={installation.suspended ? "Suspended" : "Available"} tone={installation.suspended ? "warn" : "muted"} />)}</RecordList>
      {access.busy ? <Caption>Reading repository access…</Caption> : null}
      {access.refusal ? <><ProblemNotice problem={access.refusal} tone="error" /><Button onClick={() => void access.read()}>Try again</Button></> : null}
      {access.readAt && !access.busy && !access.refusal && !access.installations.length ? <Caption>No repository access has been approved yet.</Caption> : null}
      {access.pending.map(organization => <Caption key={organization.login}>{organization.login} is awaiting an organization owner's approval.</Caption>)}
      <InstallLink installUrl={installUrl} />
      <DisconnectGitHub sourceNames={sourceNames} busy={revoke.busy} refusal={revoke.refusal} onDisconnect={() => {
        void revoke.revoke(account.id).then(done => { if (done) { connections.noteCredentialRevoked(account.id); retry(); } });
      }} />
    </> : null}
    {revoke.remoteRevoked === false ? <Notice tone="warn" sentence="Disconnected here, but GitHub did not confirm the authorization ended." next="Remove the authorization under Applications in your GitHub settings." /> : null}
    {!connected ? <Caption>Sources and deployables are kept. Reconnect this account to restore repository access.</Caption> : null}
  </section>;
}
