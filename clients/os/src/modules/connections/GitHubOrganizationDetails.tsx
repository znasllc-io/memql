import { useEffect, useState } from "react";
import { Button, Fact, Facts, Head, Notice, Panel, Subhead } from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import type { Refusal } from "../../apps/deployables/packages/actions";
import { ProblemNotice } from "../../apps/deployables/packages/ReportView";
import type { SourceConnectionRow, SourceInstallation } from "./connections";
import { RepositoryPicker } from "../../apps/deployables/sources/RepositoryPicker";
import type { RepositoryRow } from "../../apps/deployables/sources/repositories";
import type { CredentialRow } from "../../apps/deployables/sources/rows";
import { useSourceRepositories } from "../../apps/deployables/sources/useGithubConnect";

/** Settings browses the same authorized repository list as the wizard. Only
 * the wizard selects a source for analysis; browsing never creates a source. */
export function GitHubOrganizationDetails({ account, installation, connection, busy, refusal, onSave, onBack, onAccount, onSettings }: {
  account: CredentialRow; installation: SourceInstallation; connection?: SourceConnectionRow;
  busy: boolean; refusal: Refusal | null; onSave: () => void;
  onBack: () => void; onAccount: () => void; onSettings: () => void;
}) {
  const repositories = useSourceRepositories();
  const [repository, setRepository] = useState<RepositoryRow | null>(null);
  const [armed, setArmed] = useState(false);
  const { read } = repositories;
  useEffect(() => {
    if (connection) void read(account.id, 1, connection.id);
  }, [account.id, connection?.id, read]);
  const name = `@${account.login || account.label}`;
  const back = repository ? () => setRepository(null) : onBack;
  const acts: Act[] = [{ label: armed ? "Cancel" : "Back", text: true, busy, onAct: armed ? () => setArmed(false) : back }];
  if (!repository) acts.push({ label: connection ? "Disconnect" : "Connect", busy,
    tone: armed ? "danger" : connection ? "quiet" : "primary",
    onAct: connection && !armed ? () => setArmed(true) : onSave });
  const state = repository ? "Read only" : busy ? connection ? "Disconnecting" : "Connecting" : armed ? "Confirm disconnect" : connection ? "Connected" : "Available";

  return <section className="os-action-pane" aria-label={`GitHub organization ${installation.login}`}>
    <div className="os-action-body os-app-stack">
      <Head title={repository?.name || installation.login} back={{ label: repository ? installation.login : name, onSelect: back }}
        breadcrumbs={[{ label: "Settings", onSelect: onSettings }, { label: name, onSelect: onAccount },
          { label: installation.login, onSelect: repository ? () => setRepository(null) : undefined },
          ...(repository ? [{ label: repository.name }] : [])]} />
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {armed ? <Notice tone="warn" sentence="Disconnect this organization from your MemQL account? Saved sources and running sites are kept." /> : null}
      {repository ? <>
        <Panel label="Repository"><Subhead>Repository</Subhead><Facts>
          <Fact label="Name" value={repository.fullName} />
          <Fact label="Visibility" value={repository.visibility || (repository.private ? "Private" : "Public")} />
          <Fact label="Organization" value={installation.login} />
          <Fact label="GitHub account" value={name} />
        </Facts></Panel>
        <Panel label="Source"><Subhead>Source</Subhead><Facts>
          <Fact label="Default branch" value={repository.defaultBranch || "Unavailable"} />
          <Fact label="Last push" value={repository.pushedAt && Number.isFinite(Date.parse(repository.pushedAt)) ? new Date(repository.pushedAt).toLocaleString() : "Unavailable"} />
        </Facts></Panel>
      </> : null}
      {/* Keep the picker mounted so Back preserves search and loaded pages. */}
      <div hidden={Boolean(repository)}><div className="os-app-stack">
        <Panel label="Organization"><Subhead>Organization</Subhead><Facts>
          <Fact label="GitHub account" value={name} />
          <Fact label="Repository access" value={installation.repositorySelection === "all" ? "All repositories" : installation.repositorySelection === "selected" ? "Selected repositories" : "Unavailable"} />
        </Facts></Panel>
        {connection ? <Panel label="Repositories"><Subhead>Repositories</Subhead>
          <RepositoryPicker page={repositories.page} readAt={repositories.readAt} busy={repositories.busy} refusal={repositories.refusal}
            showRefresh={false} showChosenLabel={false} showGroupHeading={false} idPrefix="os-settings-repositories" onChoose={setRepository}
            onLookAgain={() => void read(account.id, 1, connection.id)}
            onReadMore={() => void read(account.id, repositories.page.nextPage, connection.id)} />
          {repositories.refusal ? <Button onClick={() => void read(account.id, 1, connection.id)}>Try again</Button> : null}
        </Panel> : null}
      </div></div>
    </div>
    <ActionBar state={state} tone={repository ? "none" : busy ? "busy" : armed ? "paused" : "live"} live acts={acts} />
  </section>;
}
