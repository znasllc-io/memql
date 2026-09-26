import { ContentSkeleton } from "../../../../../kit/ContentSkeleton";
import { ManageGitHub } from "./GitHubSource";
import { useEffect, useRef } from "react";
import { Caption, Field, Notice, RefreshButton, Select } from "../../../../../kit";
import { WizardStepHeader } from "../../../../../kit/WizardStepHeader";
import { shortRepo } from "../../../packages/rows";
import { RepositoryPicker } from "../../../sources/RepositoryPicker";
import type { RepositoryRow } from "../../../sources/repositories";
import type { SourceConnectionRow } from "../../../../../modules/connections/connections";
import { useSourceRepositories } from "../../../sources/useGithubConnect";
import { probeNote, probeParks } from "../../../sources/probe";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";

const accessRefusals = ["credential_not_found", "credential_revoked", "reconnect_required", "source_connection_unavailable", "source_repository_mismatch", "repository_not_accessible"];

export type ConnectionNeed = "" | "connect" | "reconnect" | "setup" | "unavailable";

/** A repository belongs to the explicitly chosen GitHub identity/installation.
 * The parent retains that choice; a responsive remount cannot pick another grant. */
export function RepositorySource({ connection, draft, onSelected, probe, onConnectionNeed, onSettings }: {
  onSettings?: () => void;
  connection: SourceConnectionRow;
  draft: ComposeDraft;
  onSelected: (patch: Partial<ComposeDraft>) => void;
  probe: SourceProbeHandle;
  onConnectionNeed?: (need: ConnectionNeed) => void;
}) {
  const repositories = useSourceRepositories();
  const readFor = useRef("");
  const read = repositories.read;
  useEffect(() => {
    if (readFor.current === connection.id) return;
    let current = true;
    void read(connection.credentialId, 1, connection.id).then(answered => {
      if (current && answered) readFor.current = connection.id;
    });
    return () => { current = false; };
  }, [connection.id, connection.credentialId, read]);
  const needsRepair = accessRefusals.includes(repositories.refusal?.code ?? "") || accessRefusals.includes(probe.reply?.reason ?? "");
  useEffect(() => {
    onConnectionNeed?.(needsRepair ? "reconnect" : repositories.busy || repositories.refusal || !repositories.readAt ? "unavailable" : "");
  }, [needsRepair, repositories.busy, repositories.refusal, repositories.readAt, onConnectionNeed]);

  function choose(repo: RepositoryRow) {
    if (repo.installationId !== connection.installationId || needsRepair || repositories.busy || repositories.refusal) return;
    onSelected({ repoUrl: repo.url, repoRef: draft.repoUrl === repo.url ? draft.repoRef : "", credentialId: connection.credentialId,
      sourceConnectionId: connection.id,
      name: draft.name || suggestName({ ...draft, choice: "repo", repoUrl: repo.url }, "") });
    probe.clear();
    void probe.probe(repo.url, connection.credentialId, connection.id);
  }
  return <>
    <WizardStepHeader count={repositories.readAt && !repositories.busy && !repositories.refusal ? repositories.page.repositories.length : undefined}>

    </WizardStepHeader>
    <Caption>Repositories from {connection.accountLogin}.</Caption>
    {accessRefusals.includes(repositories.refusal?.code ?? "") ? <Notice tone="warn" sentence="This source needs attention." next="Go Back to choose another organization or GitHub account." /> : null}
    <RepositoryPicker showRefresh={false} showChosenLabel={false} page={repositories.page} readAt={repositories.readAt} busy={repositories.busy}
      refusal={repositories.refusal} installUrl=""
      chosen={draft.repoUrl ? shortRepo(draft.repoUrl) : ""} idPrefix="os-compose-repo"
      onChoose={choose} onLookAgain={() => void read(connection.credentialId, 1, connection.id)}
      onReadMore={() => void read(connection.credentialId, repositories.page.nextPage, connection.id)} />
    <RepositoryProbeStatus draft={draft} probe={probe} />
    <ManageGitHub onSettings={onSettings} />

  </>;
}

export function RepositoryProbeStatus({ draft, probe }: { draft: ComposeDraft; probe: SourceProbeHandle }) {
  if (!draft.repoUrl) return null;
  const parked = probeParks(probe.reply?.reason ?? "");
  return <>
    {probe.busy ? <ContentSkeleton kind="detail" label="Loading the repository" /> : null}
    {accessRefusals.includes(probe.reply?.reason ?? "") ? <Notice tone="warn" sentence="This source needs attention." next="Go Back to choose another organization or GitHub account." /> : null}
    {probe.reply && probeNote(probe.reply) ? <p className="os-stop-verdict" data-tone={probe.reply.reason === "ok" ? "ok" : "warn"} role="status">{probeNote(probe.reply)}</p> : null}
    {probe.error ? <Notice tone="warn" sentence="This cluster could not check the repository just now." detail={probe.error} /> : null}
    {probe.error || parked ? <RefreshButton label="Check repository again" busy={probe.busy} onClick={() => void probe.probe(draft.repoUrl, draft.credentialId, draft.sourceConnectionId)} /> : null}
  </>;
}

export function RepositoryConfiguration({ draft, onDraft, branches }: { draft: ComposeDraft; onDraft: (patch: Partial<ComposeDraft>) => void; branches: readonly string[] }) {
  return <><RefField draft={draft} onDraft={onDraft} branches={branches} /><NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} /></>;
}

function RefField({
  draft,
  onDraft,
  branches,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  branches: readonly string[];
}) {
  if (branches.length === 0) return null;
  return (
    <Field label="Branch or tag">
      <Select
        id="os-compose-repo-branch"
        label="Which branch or tag to deploy"
        value={draft.repoRef}
        onChange={(repoRef) => onDraft({ repoRef })}
      >
        <option value="">Follow the default branch</option>
        {branches.map((branch) => (
          <option key={branch} value={branch}>
            {branch}
          </option>
        ))}
      </Select>
    </Field>
  );
}
