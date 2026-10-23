import { useEffect, useRef, useState } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Fact, Facts, Notice, Subhead } from "../../../kit";
import { installationCall, type InstallationStatus } from "./InstallationUpdates";

export interface ProposedRelease { candidateId: string; catalogDigest: string; label: string }
export interface InstallationProposal {
  proposalId: string; installationId: string; candidateId: string; catalogDigest: string;
  state: "prepared" | "proposed" | "merged"; pullRequestURL: string; overlayRevision: string;
}
export function readInstallationProposal(value: unknown): InstallationProposal {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The update proposal could not be read.");
  const row = value as Record<string, unknown>;
  const required = (key: string) => { if (typeof row[key] !== "string" || !row[key]) throw new Error("The update proposal was not fully reported."); return row[key] as string; };
  const state = required("state");
  if (state !== "prepared" && state !== "proposed" && state !== "merged") throw new Error("The update proposal has an unrecognized state.");
  return { proposalId: required("proposalId"), installationId: required("installationId"), candidateId: required("candidateId"), catalogDigest: required("catalogDigest"), state,
    pullRequestURL: typeof row.pullRequestURL === "string" ? row.pullRequestURL : "", overlayRevision: typeof row.overlayRevision === "string" ? row.overlayRevision : "" };
}
function reviewURL(value: string): string | null {
  try { const url = new URL(value); return url.protocol === "https:" && !url.username && !url.password ? url.href : null; } catch { return null; }
}

/** A proposal records the selected immutable release. It never applies or merges it. */
export function InstallationProposals({ query, status, fresh, release, clearRelease, refresh }: {
  query: Pick<QueryClient, "executeNamed"> | null; status: InstallationStatus; fresh: boolean;
  release: ProposedRelease | null; clearRelease?: () => void; refresh: () => void;
}) {
  const [saved, setSaved] = useState<InstallationProposal | null>(null);
  const [busy, setBusy] = useState(""); const [error, setError] = useState("");
  const inFlight = useRef(false), request = useRef<{ key: string; id: string } | null>(null);
  useEffect(() => setSaved(null), [status]);
  const proposals = saved ? [saved, ...status.proposals.filter(p => p.proposalId !== saved.proposalId)] : status.proposals;
  const existing = release ? proposals.find(p => p.candidateId === release.candidateId && p.catalogDigest === release.catalogDigest) : null;
  async function perform(proposal?: InstallationProposal) {
    if (!query || !fresh || inFlight.current) return;
    if (!proposal && (!release || !status.proposalBaseRevision || error)) return;
    inFlight.current = true; setBusy(proposal?.proposalId ?? "prepare"); setError("");
    try {
      if (proposal) {
        setSaved(readInstallationProposal((await installationCall(query, "releaseObserveInstallationProposal", { proposalId: proposal.proposalId })).single()));
      } else if (release) {
        const key = JSON.stringify([status.installationId, release.candidateId, release.catalogDigest, status.proposalBaseRevision]);
        if (request.current?.key !== key) request.current = { key, id: crypto.randomUUID() };
        setSaved(readInstallationProposal((await installationCall(query, "releaseProposeInstallation", {
          installationId: status.installationId, requestId: request.current.id, candidateId: release.candidateId,
          catalogDigest: release.catalogDigest, expectedBaseRevision: status.proposalBaseRevision,
        })).single()));
      }
      refresh();
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); refresh(); }
    finally { inFlight.current = false; setBusy(""); }
  }
  if (!release && proposals.length === 0) return null;
  return <section className="os-app-stack" aria-label="Installation update proposals">
    {release ? <><Subhead>{release.label}</Subhead><Caption>This creates a draft pull request for the installation’s saved configuration. Review and merge that proposal before preparing an installation plan.</Caption>
      {!status.proposalBaseRevision && !status.proposalWarning ? <Notice sentence="Update proposals are not configured for this installation." /> : null}
      <div className="os-panel-actions">{!existing && status.proposalBaseRevision ? <Button tone="primary" disabled={!fresh || !!busy || !!error} busy={busy === "prepare"} onClick={() => void perform()}>Prepare update pull request</Button> : null}<Button tone="quiet" disabled={!!busy} onClick={clearRelease}>Clear selection</Button></div>
    </> : <Subhead>Update proposals</Subhead>}
    {proposals.map(proposal => { const url = reviewURL(proposal.pullRequestURL); return <section className="os-app-stack" key={proposal.proposalId}>
      <Caption>{proposal.state === "merged" ? "This update proposal was merged and verified. Its release is available for an installation plan." : proposal.state === "proposed" ? "The draft update pull request is ready for review." : "The update proposal is prepared. Check its progress to read the saved pull request."}</Caption>
      <div className="os-panel-actions">{url ? <a className="os-link" href={url} target="_blank" rel="noreferrer">Review update pull request</a> : null}{proposal.state !== "merged" ? <Button tone="quiet" busy={busy === proposal.proposalId} disabled={!fresh || !!busy} onClick={() => void perform(proposal)}>Check proposal progress</Button> : null}</div>
      <details className="nexus-technical"><summary>Proposal details</summary><Facts><Fact label="Proposal reference" value={proposal.proposalId} /><Fact label="Release candidate" value={proposal.candidateId} /><Fact label="Catalog digest" value={proposal.catalogDigest} />{proposal.overlayRevision ? <Fact label="Merged configuration revision" value={proposal.overlayRevision} /> : null}</Facts></details>
    </section>; })}
    {error ? <Notice tone="error" sentence="The proposal operation could not be confirmed." detail={error}><Button disabled={!!busy} onClick={() => { setError(""); refresh(); }}>Read saved proposals</Button></Notice> : null}
  </section>;
}
