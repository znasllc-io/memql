import { useEffect, useMemo, useRef } from "react";

import { Button, Caption, Chip, Chips, Fact, Facts, Subhead } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import type { PackageRow } from "../packages/rows";
import { ProblemNotice } from "../packages/ReportView";
import { ConnectGitHub } from "../sources/ConnectGitHub";
import { ConnectedAccountCard } from "../sources/ConnectedAccountCard";
import {
  connectSucceeded,
  returnPathFor,
  type ConnectReturn,
} from "../sources/connectReturn";
import {
  credentialIsRevoked,
  githubGrantOf,
  pastedCredentials,
  type CredentialRow,
} from "../sources/rows";
import { useCredentialRevoke, useGithubConnect } from "../sources/useGithubConnect";

// Settings > Sources: where a person's deployables fetch from (epic
// memql#4915).
//
// ===========================================================================
// TWO WAYS TO REACH A PRIVATE REPOSITORY, AND NEITHER IS HIDDEN
// ===========================================================================
// A GitHub connection is the recommended one and a pasted token is the
// fallback -- for a self-hosted host, an organisation that will not install
// an app, or somebody who simply prefers it. So the token path is not behind
// "Advanced": calling it that would be a judgement about the person rather
// than a fact about the choice.
//
// NOTHING HERE SHOWS A VALUE. Every credential reaches this browser as a
// CARD (`sources/rows.ts`), so there is no chip, fact or tooltip on this
// surface that could print a token -- there is no type that could hold one.
//
// ONE HOOK PER CONTROL. Disconnecting the connection and revoking a pasted
// credential are two acts in one group, and each owns its own busy/refusal
// pair so a server sentence never lands under a button nobody pressed.
//
// The SECTION id this group is mounted under, so the connect callback brings
// somebody back to the surface that asked.
const SETTINGS_SECTION = "settings";

export function SourcesGroup({
  credentials,
  packages,
  connectResult = null,
}: {
  credentials: readonly CredentialRow[];
  /** Read to name, by name, what a disconnect will break. */
  packages: readonly PackageRow[];
  /** The answer carried back from GitHub, when this window was opened by one. */
  connectResult?: ConnectReturn | null;
}) {
  const connect = useGithubConnect();
  // A SECOND INSTANCE, for the install link and nothing else. It shares no
  // busy flag and no refusal with the control above, which is what stops a
  // background lookup greying out Reconnect or printing "Not connected to
  // the cluster" under a button nobody pressed.
  const install = useGithubConnect();
  const disconnect = useCredentialRevoke();
  const grant = useMemo(() => githubGrantOf(credentials), [credentials]);
  const pasted = useMemo(() => pastedCredentials(credentials), [credentials]);
  const returnPath = returnPathFor(SETTINGS_SECTION);

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
    <section className="os-field-group" aria-label="Sources">
      <h4 className="os-subhead">Sources</h4>
      <Caption>
        Where your deployables fetch their code. A GitHub connection lets you pick a repository from a
        list; a pasted token is the fallback for a host or an organisation that will not take the app.
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
               "Settings > Sources", so the control it names has to be
               here -- a sentence pointing at a button that does not exist
               is worse than no sentence. */
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

      <div className="os-files-group">
        <Subhead>Tokens you pasted</Subhead>
        {pasted.length === 0 ? (
          <Caption>
            None. A token is only needed for a host the GitHub connection does not cover; you add one on a
            deployable&apos;s Source stop.
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Your pasted source credentials">
            {pasted.map((card) => (
              <li className="os-field-group" key={card.id}>
                <PastedCredential card={card} packages={packages} />
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

/**
 * One pasted credential: the card, and the one act that ends it.
 *
 * ITS OWN WRITE HOOK, because a refusal belongs beside the button that
 * produced it -- a pair shared across the list would put one credential's
 * server sentence under another's Revoke.
 *
 * Revoking NAMES WHAT IT BREAKS, exactly as Disconnect does: the sources
 * fetching under this credential refuse at their next fetch until somebody
 * switches them, and that is worth knowing before rather than after.
 */
function PastedCredential({ card, packages }: { card: CredentialRow; packages: readonly PackageRow[] }) {
  const revoke = useCredentialRevoke();
  const revoked = credentialIsRevoked(card);
  const using = packages.filter((p) => p.credentialId === card.id).map((p) => p.name || p.id);
  return (
    <>
      <Chips label={`${card.label || "Credential"} state`}>
        <Chip tone="muted" title="A digest of the value. The value itself is read only at fetch time and is never shown.">
          {card.label || "unnamed"} <span>{card.fingerprint}</span>
        </Chip>
        {revoked ? (
          <span className="os-deploy-status" data-tone="warn">
            revoked
          </span>
        ) : null}
      </Chips>
      <Facts>
        <Fact label="Host" value={card.host} />
        <Fact label="Added" value={formatMoment(card.createdAt)} />
        <Fact label="Used by" value={using.length === 0 ? "" : using.join(", ")} />
      </Facts>
      {revoked ? (
        <Caption>
          Revoked. The row stays as the record of what fetched under it; nothing is deleted.
        </Caption>
      ) : (
        <div className="os-form-row">
          <Button tone="danger" busy={revoke.busy} onClick={() => void revoke.revoke(card.id)}>
            Revoke
          </Button>
          <Caption>
            {using.length === 0
              ? "Nothing fetches under this today."
              : `${using.join(", ")} will refuse at the next fetch until switched to another credential.`}
          </Caption>
        </div>
      )}
      {revoke.refusal ? <ProblemNotice problem={revoke.refusal} tone="error" /> : null}
    </>
  );
}
