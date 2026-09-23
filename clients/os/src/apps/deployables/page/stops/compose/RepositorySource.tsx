import { useEffect, useRef } from "react";
import { Caption, Field, Notice, RefreshButton, Select } from "../../../../../kit";
import { ProblemNotice } from "../../../packages/ReportView";
import { shortRepo } from "../../../packages/rows";
import { RepositoryPicker } from "../../../sources/RepositoryPicker";
import type { RepositoryRow } from "../../../sources/repositories";
import type { SourceConnectionRow } from "../../../sources/connections";
import { useSourceRepositories } from "../../../sources/useGithubConnect";
import { probeNote, probeParks } from "../../../sources/probe";
import type { SourceProbeHandle } from "../../../sources/useProbes";
import { suggestName, type ComposeDraft } from "../../compose";
import { NameField } from "./fields";

export type ConnectionNeed = "" | "connect" | "reconnect" | "setup" | "unavailable";

/** A repository belongs to the explicitly chosen GitHub identity/installation.
 * The parent retains that choice; a responsive remount cannot pick another grant. */
export function RepositorySource({ connection, draft, onDraft, probe, onConnectionNeed }: {
  connection: SourceConnectionRow;
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
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
  const refused = ["credential_not_found", "credential_revoked", "reconnect_required", "source_connection_unavailable", "source_repository_mismatch", "repository_not_accessible"];
  const needsRepair = refused.includes(repositories.refusal?.code ?? "") || refused.includes(probe.reply?.reason ?? "");
  useEffect(() => {
    onConnectionNeed?.(needsRepair ? "reconnect" : "");
    return () => onConnectionNeed?.("");
  }, [needsRepair, onConnectionNeed]);

  function choose(repo: RepositoryRow) {
    if (repo.installationId !== connection.installationId || needsRepair) return;
    onDraft({ repoUrl: repo.url, repoRef: "", credentialId: connection.credentialId,
      sourceConnectionId: connection.id,
      name: draft.name || suggestName({ ...draft, choice: "repo", repoUrl: repo.url }, "") });
    probe.clear();
    void probe.probe(repo.url, connection.credentialId, connection.id);
  }
  return <>
    <Caption>Repositories from {connection.accountLogin}.</Caption>
    {needsRepair ? <Notice tone="warn" sentence="This source needs attention." next="Return to Source to reconnect or choose another." /> : null}
    <RepositoryPicker page={repositories.page} readAt={repositories.readAt} busy={repositories.busy}
      refusal={repositories.refusal} installUrl=""
      chosen={draft.repoUrl ? shortRepo(draft.repoUrl) : ""} idPrefix="os-compose-repo"
      onChoose={choose} onLookAgain={() => void read(connection.credentialId, 1, connection.id)}
      onReadMore={() => void read(connection.credentialId, repositories.page.nextPage, connection.id)} />
    {draft.repoUrl ? <>
      {probe.busy ? <Caption>Checking the repository…</Caption> : null}
      {probe.reply && !probeParks(probe.reply.reason) && probeNote(probe.reply) ?
        <p className="os-stop-verdict" data-tone={probe.reply.reason === "ok" ? "ok" : "warn"} role="status">{probeNote(probe.reply)}</p> : null}
      {probe.error ? <Notice tone="warn" sentence="This cluster could not check the repository just now." detail={probe.error}>
        <RefreshButton label="Check repository again" busy={probe.busy} onClick={() => void probe.probe(draft.repoUrl, connection.credentialId, connection.id)} />
      </Notice> : null}
      {repositories.refusal && needsRepair ? <ProblemNotice problem={repositories.refusal} tone="warn" /> : null}
      <RefField draft={draft} onDraft={onDraft} branches={probe.reply?.branches ?? []} />
      <NameField draft={draft} onDraft={onDraft} label="Call it" placeholderFrom={suggestName(draft, "")} />
    </> : null}
  </>;
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
