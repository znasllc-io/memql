import { useEffect, useState, type ReactNode } from "react";
import { UserRound } from "lucide-react";
import { Button, EmptyState, Head, Notice, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { sourceName } from "../../apps/deployables/list";
import type { PackageRow } from "../../apps/deployables/packages/rows";
import { ProblemNotice } from "../../apps/deployables/packages/ReportView";
import type { CredentialFeedStatus, CredentialRow } from "../../apps/deployables/sources/rows";
import { useSourceConnections } from "./connections";
import { useCredentialRevoke, useGithubConnect } from "../../apps/deployables/sources/useGithubConnect";
import { useGithubApp } from "../../apps/deployables/sources/useGithubApp";
import { GithubAppMissing } from "../../apps/deployables/sources/GithubAppSetup";
import { GitHubAccountDetails } from "./GitHubAccountDetails";
import { returnPathFor, type ConnectReturn } from "../../apps/deployables/sources/connectReturn";
import { ConnectReturnNotice } from "../../apps/deployables/sources/ConnectReturnNotice";

export function GitHubAccountsSettings({ accounts, packages, feed, connectResult, header, returnPath = returnPathFor("settings") }: {
  header?: ReactNode;
  returnPath?: string;
  accounts: readonly CredentialRow[];
  packages: readonly PackageRow[];
  feed: CredentialFeedStatus;
  connectResult: ConnectReturn | null;
}) {
  const [selectedId, setSelectedId] = useState("");
  const connect = useGithubConnect();
  const app = useGithubApp();
  const revoke = useCredentialRevoke();
  const connections = useSourceConnections();
  const connectedAccounts = accounts.filter(account => account.status === "active");
  const selected = connectedAccounts.find(account => account.id === selectedId);
  const ready = feed.state === "live" && !feed.error;

  useEffect(() => {
    if (ready && selectedId && !selected) setSelectedId("");
  }, [ready, selectedId, selected]);

  if (selected) return <GitHubAccountDetails key={selected.id} account={selected}
    sourceNames={packages.filter(pkg => pkg.credentialId === selected.id && pkg.status !== "archived").map(sourceName)}
    onBack={() => setSelectedId("")} busy={revoke.busy} refusal={revoke.refusal} onDisconnect={() => {
      void revoke.revoke(selected.id).then(done => {
        if (done) { connections.noteCredentialRevoked(selected.id); setSelectedId(""); feed.retry(); }
      });
    }} />;

  return <section className="os-settings os-settings-wide deployable-settings" aria-label="Connection settings">
    {header ?? <Head title="Settings" />}
    <section className="os-field-group" aria-label="GitHub accounts settings">
      <div className="os-head">
        <Subhead>GitHub Accounts</Subhead>
        <div className="os-head-actions">
          {app.status?.configured !== false ? <AddButton label="Add GitHub account" disabled={connect.busy} aria-busy={connect.busy} onClick={() => { revoke.clear(); void connect.connect(returnPath); }} /> : null}
        </div>
      </div>
      <ConnectReturnNotice result={connectResult} />
      {connect.refusal ? <ProblemNotice problem={connect.refusal} tone="error" /> : null}
      {revoke.remoteRevoked === false ? <Notice tone="warn" sentence="Disconnected here, but GitHub did not confirm the authorization ended." next="Remove the authorization under Applications in your GitHub settings." /> : null}
      {feed.state === "seeding" && connectedAccounts.length === 0 ? <RecordListSkeleton label="Reading GitHub accounts" /> : null}
      <RecordList as="ul" label="GitHub accounts">{connectedAccounts.map(account => <RecordRow key={account.id}
        icon={<UserRound size={18} aria-hidden />} name={`@${account.login || account.label}`}
        secondary="GitHub" state="Connected" tone="accent" current
        onOpen={() => { revoke.clear(); setSelectedId(account.id); }} label={`Manage GitHub account ${account.login || account.label}`}
      />)}</RecordList>
      {ready && connectedAccounts.length === 0 ? <EmptyState icon={UserRound} title="No GitHub accounts connected">Use the plus button to connect an account.</EmptyState> : null}
      {!ready && feed.state !== "seeding" ? <Notice sentence="GitHub accounts could not be read." detail={feed.error || undefined}>
        <Button onClick={feed.retry}>Try again</Button>
      </Notice> : null}
      {app.status?.configured === false ? <GithubAppMissing app={app} returnPath={returnPath} /> : null}
    </section>
  </section>;
}
