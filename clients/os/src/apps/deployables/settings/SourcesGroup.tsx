import { useState } from "react";
import { KeyRound } from "lucide-react";

import { Button, Caption, Chip, LiveList, Row as ListRow, formatFreshness, useNow } from "../../../kit";
import type { LiveView } from "../../../live/liveView";
import { useCredentialActions } from "../packages/actions";
import { sourceLabel, type PackageRow } from "../packages/rows";
import { AddCredential, cardName } from "../sources/CredentialField";
import { SOURCE_HOST } from "../sources/probe";
import { credentialFingerprint, credentialIsRevoked, type CredentialRow } from "../sources/rows";

// Settings -> Sources: every credential this person holds, and what fetches
// under it (epic memql#4885, design section D).
//
// ===========================================================================
// A CREDENTIAL IS ONLY LEGIBLE WITH ITS DEPENDANTS BESIDE IT
// ===========================================================================
// "Revoke" is a decision about other things: every source fetching under this
// credential refuses at its next fetch until somebody switches it. So the
// sources are joined onto the row, by `credentialId` over the package feed
// the app root already retains -- not fetched, and not a second subscription.
// The revoke sentence names the consequence in those terms and the confirm
// sits in the surface, because a dialog would take the list of dependants off
// the screen at exactly the moment it matters.
//
// ===========================================================================
// LAST USED IS DISPLAYED CONTINUOUSLY AND FINGERPRINTED NEVER
// ===========================================================================
// `lastUsedAt` is written by every fetch under this credential, the ten-minute
// poll included. It is the exact field the arrival-cue rule
// (clients/os/README.md) is about: naming it in a fingerprint would ring the
// row on a timer forever. `credentialFingerprint` leaves it out, and this
// surface shows it against a ticking clock instead -- the right home for
// something that is always true and never news.
//
// ===========================================================================
// A REVOKED CREDENTIAL STAYS LISTED
// ===========================================================================
// The row is never deleted: it is the history of what fetched under it. It
// stays on the list, marked, and stays offerable as the CURRENT value of a
// source's picker so somebody can switch away from it by name.

export function SourcesGroup({
  credentials,
  packages,
}: {
  /** The app root's one credentials feed. */
  credentials: LiveView<CredentialRow> | null;
  /** The app root's package rows, for the join. Read-only: this surface writes no package. */
  packages: readonly PackageRow[];
}) {
  const [adding, setAdding] = useState(false);
  const now = useNow();

  return (
    <fieldset className="os-field-group">
      <legend>Sources</legend>
      <Caption>
        A credential lets this cluster fetch a private repository. The value is sealed here and read only at fetch
        time -- nothing, including this page, can show it again.
      </Caption>

      <LiveList<CredentialRow>
        source={credentials}
        rowId={(c) => c.id}
        fingerprint={credentialFingerprint}
        label="Your source credentials"
        emptyText="No credentials yet. A public repository needs none; add one when a private repository asks for it."
        renderRow={(card) => <CredentialLine card={card} packages={packages} now={now} />}
      />

      {adding ? (
        <AddCredential
          id="os-sources-add"
          host={SOURCE_HOST}
          onAdded={() => setAdding(false)}
          onCancel={() => setAdding(false)}
        />
      ) : (
        <div className="os-form-row">
          <Button onClick={() => setAdding(true)}>Add a credential</Button>
          <Caption>Only {SOURCE_HOST} today.</Caption>
        </div>
      )}
    </fieldset>
  );
}

function CredentialLine({
  card,
  packages,
  now,
}: {
  card: CredentialRow;
  packages: readonly PackageRow[];
  now: Date;
}) {
  const actions = useCredentialActions();
  const [confirming, setConfirming] = useState(false);
  const revoked = credentialIsRevoked(card);
  const fetching = packages.filter((p) => p.credentialId === card.id);

  return (
    <div className="os-source-credential">
      <ListRow
        icon={<KeyRound size={16} aria-hidden />}
        name={cardName(card)}
        current={!revoked}
        dim={revoked}
        state={
          <>
            <Chip tone="muted">{card.host}</Chip>
            {/* A HEARTBEAT, displayed and never fingerprinted. */}
            <span className="os-source-used">used {formatFreshness(card.lastUsedAt, now)}</span>
            {revoked ? (
              <span className="os-deploy-status" data-tone="warn">
                revoked
              </span>
            ) : (
              /* THE ACT BELONGS TO THE ROW. Beneath it, a lone button read as
                 a control of the whole group rather than of the credential it
                 revokes -- and the second one under the second row said so
                 twice. */
              <Button onClick={() => setConfirming(true)} ariaLabel={`Revoke ${cardName(card)}`}>
                Revoke
              </Button>
            )}
          </>
        }
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
      {actions.refusal ? <p className="os-ask-error">{actions.refusal.message}</p> : null}
    </div>
  );
}
