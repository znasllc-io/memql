import { useEffect, useMemo, useRef, useState } from "react";
import { KeyRound } from "lucide-react";

import { Button, Caption, Chip, LiveList, Row as ListRow, Subhead, formatFreshness, useNow } from "../../../kit";
import { useLiveView, type LiveView } from "../../../live/liveView";
import { useCredentialActions } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import { sourceLabel, type PackageRow } from "../packages/rows";
import { AddCredential, cardName } from "../sources/CredentialField";
import { ConnectedAccountCard } from "../sources/ConnectedAccountCard";
import { ConnectGitHub } from "../sources/ConnectGitHub";
import { connectSucceeded, returnPathFor, type ConnectReturn } from "../sources/connectReturn";
import { SOURCE_HOST } from "../sources/probe";
import {
  credentialFingerprint,
  credentialIsRevoked,
  githubGrantOf,
  pastedCredentials,
  type CredentialRow,
} from "../sources/rows";
import { useCredentialRevoke, useGithubConnect } from "../sources/useGithubConnect";

// Settings -> Sources: where a person's deployables fetch from -- the GitHub
// connection, and every credential they hold (epic memql#4885 design section
// D, widened for the grant by memql#4915).
//
// ===========================================================================
// TWO WAYS TO REACH A PRIVATE REPOSITORY, AND NEITHER IS HIDDEN
// ===========================================================================
// A GitHub connection is the recommended one and a pasted token is the
// fallback -- for a self-hosted host, an organisation that will not install
// an app, or somebody who simply prefers it. So the token half is not behind
// "Advanced": calling it that would be a judgement about the person rather
// than a fact about the choice.
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
// the screen at exactly the moment it matters. Disconnect says the same thing
// about the whole connection, in the same words, for the same reason.
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
//
// NOTHING HERE SHOWS A VALUE. Every credential reaches this browser as a
// CARD (`sources/rows.ts`), so there is no chip, fact or tooltip on this
// surface that could print a token -- there is no type that could hold one.
//
// ONE HOOK PER CONTROL. Disconnecting the connection, revoking a pasted
// credential and adding one are three acts in one group, and each owns its
// own busy/refusal pair so a server sentence never lands under a button
// nobody pressed.

/** The SECTION id this group is mounted under, so the connect callback brings
 *  somebody back to the surface that asked. */
const SETTINGS_SECTION = "settings";

export function SourcesGroup({
  credentials,
  packages,
  connectResult = null,
}: {
  /** The app root's one credentials feed. */
  credentials: LiveView<CredentialRow> | null;
  /** The app root's package rows, for the join. Read-only: this surface writes no package. */
  packages: readonly PackageRow[];
  /** The answer carried back from GitHub, when this window was opened by one. */
  connectResult?: ConnectReturn | null;
}) {
  const [adding, setAdding] = useState(false);
  const now = useNow();
  const connect = useGithubConnect();
  // A SECOND INSTANCE, for the install link and nothing else. It shares no
  // busy flag and no refusal with the control above, which is what stops a
  // background lookup greying out Reconnect or printing "Not connected to
  // the cluster" under a button nobody pressed.
  const install = useGithubConnect();
  const disconnect = useCredentialRevoke();
  const returnPath = returnPathFor(SETTINGS_SECTION);

  // THE CONNECTION IS READ OFF THE FEED THE LIST RENDERS, never a second
  // subscription: a card saying "connected as @octocat" beside a list that
  // had not heard about the grant yet would be one app contradicting itself.
  const held = credentials?.snapshot.rows ?? [];
  const grant = useMemo(() => githubGrantOf(held), [held]);
  // ...and the list is that same feed NARROWED, because a grant is already
  // the card above and a row that appeared in both would be one credential
  // offering two different acts. `useLiveView` is exactly what LiveList's
  // source seam is for, so the pasted rows keep their arrival cues and their
  // live-state caption.
  const pasted = useLiveView<CredentialRow, CredentialRow>(credentials, "pasted", pastedCredentials);

  const sourceNames = useMemo(
    () =>
      grant === null
        ? []
        : packages.filter((p) => p.credentialId === grant.id).map((p) => p.name || p.id),
    [packages, grant],
  );

  // THE INSTALL LINK'S URL, ASKED FOR ONCE PER GRANT.
  //
  // "Install on another organisation" has to be a real anchor with a real
  // href, and `githubConnectBegin` is the only call that answers where that
  // is -- the credential row projects installation IDS, never the app's
  // installation page. So it is asked for exactly once, keyed on the grant's
  // id in a ref rather than in the dependency list: `learn` is stable only
  // while the connection is, and a dependency on it alone would ask again
  // every time the socket redialled.
  //
  // Only for somebody who is already CONNECTED, which is the only place the
  // link is offered. A person with no grant sees Connect, whose click makes
  // this same call and navigates with what it answers -- so the common path
  // asks the cluster nothing until somebody presses something.
  //
  // MARKED ASKED ONLY WHEN IT WAS ANSWERED. A browser that had not finished
  // dialling when this mounted would otherwise record the question as asked
  // and never ask it again, and the link would be missing for the rest of
  // the session; `learn` says which happened. The in-flight ref is what
  // keeps a re-render during the call from sending a second one.
  //
  // A LOOKUP THAT FAILS COSTS THE LINK AND NOTHING ELSE, which is why its
  // refusal is not rendered. It is an enhancement on a card that is already
  // telling the truth, and the refusals that matter here -- the ones a click
  // produces -- still land beside the control that produced them.
  const learnedFor = useRef("");
  const learning = useRef(false);
  const learn = install.learn;
  useEffect(() => {
    if (grant === null || grant.id === learnedFor.current || learning.current) return;
    const wanted = grant.id;
    learning.current = true;
    void learn(returnPath).then((answered) => {
      learning.current = false;
      if (answered) learnedFor.current = wanted;
    });
  }, [grant, learn, returnPath]);

  return (
    <fieldset className="os-field-group">
      <legend>Sources</legend>
      <Caption>
        Where your deployables fetch their code. A GitHub connection lets you pick a repository from a list; a
        pasted token is the fallback for a host or an organisation that will not take the app.
      </Caption>

      {/* THE ANSWER FROM GITHUB, ON THE SURFACE THAT ASKED. A successful
          connection says NOTHING here: the card arrives on the credential
          feed's own broadcast with the standard arrival ring, and this shell
          has no toasts. Only a refusal has something to add. */}
      {connectResult !== null && !connectSucceeded(connectResult) ? (
        <ProblemNotice
          problem={{
            code: connectResult.reason,
            message: "GitHub sent you back without completing the connection.",
          }}
          tone="error"
        />
      ) : null}

      {grant === null ? (
        <ConnectGitHub
          busy={connect.busy}
          refusal={connect.refusal}
          onConnect={() => void connect.connect(returnPath)}
        />
      ) : (
        <>
          <ConnectedAccountCard
            grant={grant}
            installUrl={install.installUrl}
            sourceNames={sourceNames}
            busy={disconnect.busy}
            refusal={disconnect.refusal}
            onDisconnect={() => void disconnect.revoke(grant.id)}
          />
          {credentialIsRevoked(grant) ? (
            /* The copy for `reconnect_required` sends a person to
               "Settings > Sources", so the control it names has to be here --
               a sentence pointing at a button that does not exist is worse
               than no sentence. */
            <ConnectGitHub
              label="Reconnect GitHub"
              caption="This connection was disconnected. Reconnecting puts your sources back to fetching under it."
              busy={connect.busy}
              refusal={connect.refusal}
              onConnect={() => void connect.connect(returnPath)}
            />
          ) : null}
        </>
      )}

      {/* THE TOKEN HALF, NAMED, because there is now a connection above it:
          an unheaded list would read as belonging to that card rather than as
          the other way in. The sealing sentence sits here, over the values it
          is about, rather than over a group that is half connection. */}
      <Subhead>Tokens you pasted</Subhead>
      <Caption>
        A credential lets this cluster fetch a private repository. The value is sealed here and read only at fetch
        time -- nothing, including this page, can show it again.
      </Caption>

      <LiveList<CredentialRow>
        source={pasted}
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
