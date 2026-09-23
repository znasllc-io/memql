import { useState } from "react";
import { KeyRound } from "lucide-react";
import { Button, Caption, LiveList, RecordRow as ListRow, listCount, Subhead, formatFreshness, useNow } from "../../../kit";
import { useLiveView, type LiveView } from "../../../live/liveView";
import { useCredentialActions } from "../packages/actions";
import { toneFor } from "../packages/refusals";
import { ProblemNotice } from "../packages/ReportView";
import { sourceLabel, type PackageRow } from "../packages/rows";
import { cardName } from "../sources/CredentialField";
import { GithubAppBlock } from "../sources/GithubAppSetup";
import { useGithubApp } from "../sources/useGithubApp";
import type { ConnectReturn } from "../sources/connectReturn";
import { bare, usePeopleNames } from "../people";
import { credentialFingerprint, credentialIsRevoked, pastedCredentials, type CredentialRow } from "../sources/rows";
import { SourceConnections } from "../sources/SourceConnections";

/** Source management is shared with Sources and the deployable wizard.
 * Stored tokens and operator oversight remain reachable for existing fetches. */
export function SourcesGroup({ credentials, packages, viewerUserId, isClusterOwner, connectResult = null, onRetryCredentials = () => {} }: {
  credentials: LiveView<CredentialRow> | null;
  packages: readonly PackageRow[];
  viewerUserId: string;
  isClusterOwner: boolean;
  connectResult?: ConnectReturn | null;
  onRetryCredentials?: () => void;
}) {
  const viewer = bare(viewerUserId);
  const now = useNow();
  const nameOf = usePeopleNames();
  const githubApp = useGithubApp();
  const pasted = useLiveView<CredentialRow, CredentialRow>(credentials, `pasted:${viewer}`, rows => pastedCredentials(rows.filter(row => bare(row.ownerUserId) === viewer)));
  const others = useLiveView<CredentialRow, CredentialRow>(credentials, `others:${viewer}`, rows => rows.filter(row => bare(row.ownerUserId) !== viewer).slice().sort((a, b) => b.createdAt.localeCompare(a.createdAt)));
  return <section className="os-field-group" aria-label="Source settings">
    <SourceConnections mode="manage" credentials={credentials?.snapshot.rows ?? []} returnSection="settings" connectResult={connectResult}
      credentialFeed={{ state: credentials?.snapshot.state ?? "disconnected", error: credentials?.snapshot.error ?? "", retry: onRetryCredentials }} />
    <section className="os-field-group" aria-label="Access tokens">
      <Subhead meta={listCount(pasted?.snapshot)}>Access tokens</Subhead>
      <Caption>Stored tokens remain available to existing repositories. Token values are never shown.</Caption>
      <LiveList<CredentialRow> source={pasted} rowId={row => row.id} fingerprint={credentialFingerprint} label="Your source credentials" emptyText="No stored access tokens."
        renderRow={card => <CredentialLine card={card} packages={packages} now={now} />} />
    </section>
    {isClusterOwner && (others?.snapshot.rows.length ?? 0) > 0 ? <section className="os-field-group" aria-label="Other people's connections">
      <Subhead meta={listCount(others?.snapshot)}>Other people's connections</Subhead>
      <Caption>Review existing repository access. These credentials cannot be chosen as your source.</Caption>
      <LiveList<CredentialRow> source={others} rowId={row => row.id} fingerprint={credentialFingerprint} label="Other people's source credentials" emptyText="Nobody else holds a credential."
        renderRow={card => <CredentialLine card={card} packages={packages} now={now} owner={nameOf(card.ownerUserId)} />} />
    </section> : null}
    {isClusterOwner ? <GithubAppBlock app={githubApp} /> : null}
  </section>;
}

function CredentialLine({
  card,
  packages,
  now,
  owner = "",
}: {
  card: CredentialRow;
  packages: readonly PackageRow[];
  now: Date;
  /** The owner's name on somebody else's row, "" for the viewer's own or when the roster gave none. */
  owner?: string;
}) {
  const actions = useCredentialActions();
  const [confirming, setConfirming] = useState(false);
  const revoked = credentialIsRevoked(card);
  const fetching = packages.filter((p) => p.credentialId === card.id);

  return (
    <div className="os-source-credential">
      <ListRow
        icon={<KeyRound size={16} aria-hidden />}
        /* MONO FOR THE DIGEST, NOT FOR THE NAME. `cardName` is one string --
           the label somebody chose plus the fingerprint that tells two cards
           apart -- and the two are different kinds of thing: an id reads in
           the code face everywhere else in this shell, and a name somebody
           typed does not. */
        name={
          <>
            {card.label.trim() === "" ? card.id : card.label.trim()}
            {card.fingerprint.trim() === "" ? null : (
              <> <span className="os-mono">{card.fingerprint.trim()}</span></>
            )}
          </>
        }
        current={!revoked}
        dim={revoked}
        secondary={card.host}
        state={revoked ? "revoked" : undefined}
        tone={revoked ? "warn" : "muted"}
        stateExtra={<>
          {owner === "" ? null : <span className="os-deploy-by">{owner}'s</span>}
          <span className="os-source-used">used {formatFreshness(card.lastUsedAt, now)}</span>
        </>}
        actions={revoked ? null : <Button onClick={() => setConfirming(true)} ariaLabel={`Revoke ${cardName(card)}`}>Revoke</Button>}

      />

      {/* WHAT FETCHES UNDER IT gets its own line beneath the row rather than
          the row's middle slot. It is the fact that makes a revoke legible --
          an ellipsis here would hide the very thing somebody is about to
          break -- and a row already holding a name, a host, a heartbeat and
          an act has no room to give it. */}
      <p className="os-source-fetching">
        {fetching.length === 0 ? "nothing fetches under it" : fetching.map((p) => sourceLabel(p)).join(", ")}
      </p>

      {confirming && !revoked ? (
        <div className="os-source-confirm">
          <Caption>
            Sources fetching under it will refuse at their next fetch until you switch them. The credential stays
            listed; nothing is deleted.
          </Caption>
          <div className="os-form-row">
            <Button tone="quiet" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button tone="danger" busy={actions.busy} onClick={() => void actions.revoke(card.id)}>
              Revoke
            </Button>
          </div>
        </div>
      ) : null}
      {/* THROUGH `ProblemNotice`, LIKE EVERY OTHER REFUSAL ON THIS SURFACE:
          the OS headline above and the server's own sentence beneath, and the
          TONE read from the code -- a revoked credential and a cluster with
          no GitHub App are somebody's next step, and the fault colour would
          say this person broke something. */}
      {actions.refusal ? (
        <ProblemNotice problem={actions.refusal} tone={toneFor(actions.refusal.code)} />
      ) : null}
    </div>
  );
}
