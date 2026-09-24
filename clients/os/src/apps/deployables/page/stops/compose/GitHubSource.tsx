import { UserRound, Building2 } from "lucide-react";
import { Button, Caption, RecordList, RecordListSkeleton, RecordRow } from "../../../../../kit";
import { WizardStepHeader } from "../../../../../kit/WizardStepHeader";
import type { CredentialFeedStatus, CredentialRow } from "../../../sources/rows";
import type { SourceConnectionRow } from "../../../sources/connections";

export function ManageGitHub({ onSettings }: { onSettings?: () => void }) {
  return onSettings ? <button type="button" className="os-link" onClick={onSettings}>Add or manage GitHub accounts and organizations in Settings</button> : null;
}

export function GitHubAccountStep({ credentials, selectedId, onSelect, feed, disabled, onSettings }: {
  credentials: readonly CredentialRow[]; selectedId: string; onSelect: (id: string) => void;
  feed?: CredentialFeedStatus; disabled: boolean; onSettings?: () => void;
}) {
  const ready = !feed || feed.state === "live" && !feed.error;
  const connected = credentials.filter(row => row.status === "active");
  return <div className="os-stop-body">
    <WizardStepHeader count={ready ? connected.length : undefined} />
    {!ready ? feed?.state === "seeding" ? <RecordListSkeleton label="Reading GitHub accounts" /> : <>
      <Caption>{feed?.error || "GitHub accounts are unavailable."}</Caption>
      <Button onClick={() => feed?.retry()}>Try again</Button>
    </> : <>
      <RecordList as="ul" label="GitHub accounts">{connected.map(row => <RecordRow key={row.id}
        icon={<UserRound size={18} aria-hidden />} name={`@${row.login}`} state="Connected"
        selected={selectedId === row.id} disabled={disabled} onOpen={() => onSelect(row.id)} />)}</RecordList>
      {connected.length === 0 ? <Caption>No GitHub accounts connected.</Caption> : null}
    </>}
    <ManageGitHub onSettings={onSettings} />
  </div>;
}

export function GitHubOrganizationStep({ organizations, selectedId, onSelect, disabled, feed, onSettings }: {
  organizations: readonly SourceConnectionRow[]; selectedId: string; onSelect: (id: string) => void;
  disabled: boolean; feed: { state: string; error: string; retry: () => void }; onSettings?: () => void;
}) {
  const ready = feed.state === "live" && !feed.error;
  return <div className="os-stop-body">
    <WizardStepHeader count={ready ? organizations.length : undefined} />
    {!ready ? feed.state === "seeding" ? <RecordListSkeleton label="Reading organizations" /> : <>
      <Caption>{feed.error || "Organizations are unavailable."}</Caption><Button onClick={feed.retry}>Try again</Button>
    </> : <>
      <RecordList as="ul" label="Organizations and personal account">{organizations.map(row => <RecordRow key={row.id}
        icon={<Building2 size={18} aria-hidden />} name={row.accountLogin}
        secondary={row.accountType === "Organization" ? "Organization" : "Personal account"}
        selected={selectedId === row.id} disabled={disabled} onOpen={() => onSelect(row.id)} />)}</RecordList>
      {organizations.length === 0 ? <Caption>No organizations connected.</Caption> : null}
    </>}
    <ManageGitHub onSettings={onSettings} />
  </div>;
}
