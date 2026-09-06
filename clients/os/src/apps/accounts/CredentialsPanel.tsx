import { useState } from "react";
import { KeyRound } from "lucide-react";
import type { AccountTokenMintResult } from "@znasllc-io/memql-sdk-core/identity";

import {
  Button,
  Caption,
  Chip,
  Fact,
  Facts,
  Field,
  Input,
  Notice,
  Panel,
  Row,
  Subhead,
  formatMoment,
} from "../../kit";
import { useAccountTokenActions } from "./actions";
import { accountTokenIsRevoked, accountTokenLabel, type AccountTokenRow } from "./credentials";
import { accountName, type AccountRow } from "./rows";
import { useAccountTokens } from "./useAccountTokens";

// Credentials issued on behalf of one client: issue, reveal once, revoke.
//
// ===========================================================================
// PER-CLIENT, IN THE CLIENT'S OWN DETAIL
// ===========================================================================
// A credential is minted AGAINST one client and listed BY one client, so a
// top-level Credentials section would have to ask "which client" before it
// could do anything -- which is the question this panel is already standing
// inside the answer to. It sits between the ledger (what is theirs) and the
// archive (the exit), because it is the third thing an operator has about a
// client: what they are, what is theirs, and what can act on their behalf.
//
// ===========================================================================
// WHAT THIS CREDENTIAL IS, AND THE SENTENCE THE COPY MUST NOT WRITE
// ===========================================================================
// It is issued TO A USER ON BEHALF OF A CLIENT. The authenticated subject is
// the operator's own user row -- the server echoes `subjectUserId` back on
// the mint reply precisely so a client cannot render "signed in as this
// client" without contradicting a field it was handed -- so the subject is
// shown, on the reveal and on every listed row. Nothing authenticates as a
// client; `accountId` is a binding for attribution and grouped revocation.
//
// And nothing on this cluster admits one of these yet. That is stated where
// somebody is about to issue one, because a credential that grants nothing is
// a fact the person holding it needs BEFORE they wire it into something, not
// after they discover it by trying.
//
// ===========================================================================
// THE ISSUE CONTROL IS PRESENT EVEN THOUGH THE ENGINE MAY REFUSE IT
// ===========================================================================
// DESIGN.md rule 12 is that an act which is not legal is ABSENT, and the
// revoke below obeys it exactly: a revoked credential offers no Revoke,
// because that is a fact this surface HOLDS. Whether this operator may issue
// one is not. The handler's gate is `query accountById` run as the caller
// against a concept declaring `@rowAuthz(owner="ownerUserId")`, with no
// cluster-owner escape -- so it is not a role, not a rank, and not anything a
// browser can compute. Guessing it would teach an operator a restriction the
// cluster may not have, and hiding a control that would have worked leaves
// them no way to discover otherwise. So the control stands and the refusal is
// the server's own sentence, rendered beside it.

export function CredentialsPanel({ account }: { account: AccountRow }) {
  const feed = useAccountTokens(account.id);
  const actions = useAccountTokenActions();
  const [label, setLabel] = useState("");
  // SHOWN ONCE. It exists nowhere else -- the cluster kept only its SHA-256
  // digest and no later call can retrieve it -- so it lives in component
  // state that dies with the panel, and in nothing that outlives it: not
  // localStorage, not sessionStorage, not a URL, not a row, not a log line.
  // The OS persists a great deal to localStorage, which is exactly why the
  // exception has to be written down: the habit of the code around it is to
  // persist.
  const [minted, setMinted] = useState<AccountTokenMintResult | null>(null);
  const [copied, setCopied] = useState("");
  const [confirming, setConfirming] = useState("");

  const inputId = `os-account-credential-${account.id}`;
  const minting = actions.busyKey === "mint";

  async function issue(): Promise<void> {
    const wanted = label.trim();
    if (wanted === "" || minting) return;
    const result = await actions.mint(account.id, wanted);
    if (result === null) return;
    setMinted(result);
    setLabel("");
    setCopied("");
    // The list is a read, not a feed -- `v1:identity:identity` carries no
    // routing rule -- so this is what puts the new credential on screen.
    feed.reload();
  }

  async function revoke(identityId: string): Promise<void> {
    const ok = await actions.revoke(identityId);
    if (!ok) return;
    setConfirming("");
    feed.reload();
  }

  // Best-effort. A clipboard that refuses leaves the value on screen and
  // selectable, which is why the failure is a caption rather than an error:
  // nothing was lost.
  function copy(value: string): void {
    const clipboard = globalThis.navigator?.clipboard;
    if (!clipboard) {
      setCopied("This browser offered no clipboard -- select the credential and copy it.");
      return;
    }
    void clipboard
      .writeText(value)
      .then(() => setCopied("Credential copied."))
      .catch(() => setCopied("The browser refused the copy -- select the credential and copy it."));
  }

  return (
    <Panel label="Credentials">
      <Subhead>Credentials</Subhead>

      <p className="os-caption">
        A credential issued here authenticates as <strong>you</strong>, on behalf of{" "}
        {accountName(account)}. Nothing authenticates as a client: the client is recorded on the
        credential so it can be attributed to the work it was issued for, and revoked as a group.
      </p>
      <p className="os-caption">
        Nothing on this cluster accepts one of these yet. It is a real credential with a real
        revoke, and no endpoint admits it -- so issuing one now grants nothing until something
        does.
      </p>

      <Field label="What is this credential for">
        <Input
          id={inputId}
          label="What is this credential for"
          value={label}
          onChange={setLabel}
          placeholder="Nightly export job"
          disabled={minting}
          onEnter={() => void issue()}
        />
        <Button
          tone="primary"
          busy={minting}
          busyLabel="Issuing..."
          disabled={label.trim() === ""}
          onClick={() => void issue()}
        >
          Issue a credential
        </Button>
      </Field>
      <Caption>
        The label is the only handle a revoke has, so make it say what will hold the credential.
        The cluster refuses an unlabelled one for that reason.
      </Caption>

      {minted === null ? null : (
        <Notice
          tone="warn"
          sentence="Here is the credential. This is the only time it is shown."
          next="The cluster kept only its hash, so there is nowhere to look it up. Done discards it -- if it is lost, issue another one and revoke this."
        >
          <div className="os-account-secret">
            <code className="os-mono os-account-secret-value">{minted.plainToken}</code>
            <Button onClick={() => copy(minted.plainToken)} ariaLabel="Copy the credential">
              Copy
            </Button>
            <Button
              tone="primary"
              onClick={() => {
                setMinted(null);
                setCopied("");
              }}
            >
              Done
            </Button>
          </div>
          {copied === "" ? null : <p className="os-caption">{copied}</p>}
          <Facts>
            {/* THE SUBJECT, on the server's own word for it. This is what
                stops the surface implying the client is the principal. */}
            <Fact label="Authenticates as" value={minted.subjectUserId} mono />
            <Fact label="On behalf of" value={minted.accountId} mono />
            <Fact label="Credential" value={minted.identityId} mono />
            <Fact label="Audited as" value={minted.auditEventId} mono />
          </Facts>
        </Notice>
      )}

      {actions.refusal === null ? null : (
        <Notice
          tone="error"
          sentence="The cluster refused that."
          next={
            actions.refusal.auditEventId === ""
              ? "Nothing changed."
              : `Nothing changed. Audited as ${actions.refusal.auditEventId}.`
          }
          detail={actions.refusal.detail}
        />
      )}

      <CredentialList
        feed={feed}
        confirming={confirming}
        setConfirming={setConfirming}
        busyKey={actions.busyKey}
        revoke={revoke}
      />
    </Panel>
  );
}

function CredentialList({
  feed,
  confirming,
  setConfirming,
  busyKey,
  revoke,
}: {
  feed: ReturnType<typeof useAccountTokens>;
  confirming: string;
  setConfirming: (id: string) => void;
  busyKey: string;
  revoke: (identityId: string) => Promise<void>;
}) {
  // A REFUSAL IS NOT AN EMPTY LIST, the rule the ledger's bands state. "No
  // credentials" and "the cluster would not tell you" are different answers,
  // and rendering the second as the first is this window inventing a fact.
  if (feed.state === "error") {
    return (
      <Notice
        tone="error"
        sentence="The credentials for this client were not returned."
        next="This is not the same as there being none -- nothing below was read."
        detail={feed.error}
      />
    );
  }

  if (feed.state === "idle" || feed.state === "loading") {
    return <p className="os-caption">Reading</p>;
  }

  if (feed.tokens.length === 0) {
    return <p className="os-caption">No credentials have been issued for this client.</p>;
  }

  return (
    <>
      <ul className="os-account-credentials" aria-label="Credentials issued for this client">
        {feed.tokens.map((token) => (
          <li key={token.id}>
            <CredentialRow
              token={token}
              busy={busyKey === token.id}
              confirming={confirming === token.id}
              onAsk={() => setConfirming(token.id)}
              onKeep={() => setConfirming("")}
              onRevoke={() => void revoke(token.id)}
            />
          </li>
        ))}
      </ul>
      {feed.readAt === "" ? null : (
        <Caption>
          Read at {new Date(feed.readAt).toLocaleTimeString()}. This list is not live -- it
          re-reads when a credential is issued or revoked here.
        </Caption>
      )}
    </>
  );
}

function CredentialRow({
  token,
  busy,
  confirming,
  onAsk,
  onKeep,
  onRevoke,
}: {
  token: AccountTokenRow;
  busy: boolean;
  confirming: boolean;
  onAsk: () => void;
  onKeep: () => void;
  onRevoke: () => void;
}) {
  const revoked = accountTokenIsRevoked(token);
  const name = accountTokenLabel(token);

  return (
    <>
      <Row
        icon={<KeyRound size={16} aria-hidden />}
        name={name}
        // `current` is the row's own liveness; a revoked credential keeps its
        // record and loses its ink. A REVOKED ONE IS STILL LISTED -- it is the
        // record of a credential that existed, and hiding it would make a
        // revocation impossible to see having taken effect.
        current={!revoked}
        dim={revoked}
        state={
          <>
            {revoked ? <Chip tone="muted">revoked</Chip> : null}
            {/* DESIGN.md rule 12: an act that is not legal is ABSENT, never
                disabled. A revoked credential cannot be revoked again, so
                there is no control here at all -- not a greyed one, which
                would be a question this surface refuses to answer. */}
            {revoked || confirming ? null : (
              <Button tone="danger" onClick={onAsk} ariaLabel={`Revoke ${name}`}>
                Revoke
              </Button>
            )}
          </>
        }
      >
        <span className="os-caption os-mono" title="The credential's authenticated subject">
          {token.subjectUserId}
        </span>
        <span className="os-caption">issued {formatMoment(token.createdAt)}</span>
        <span className="os-caption">
          {token.expiresAt === "" ? "no expiry" : `expires ${formatMoment(token.expiresAt)}`}
        </span>
        <span className="os-caption">
          {token.lastUsedAt === "" ? "never used" : `last used ${formatMoment(token.lastUsedAt)}`}
        </span>
      </Row>

      {/* AN IN-SURFACE CONFIRM THAT NAMES THE CREDENTIAL, never a browser
          dialog: window.confirm blocks the whole shell, and a generic "are you
          sure" invites the mistake it exists to prevent -- an operator with
          four credentials listed confirms the wrong one because nothing on the
          dialog said which. The app's archive confirm states the same rule. */}
      {confirming ? (
        <div className="os-account-credential-confirm" role="group" aria-label={`Revoke ${name}`}>
          <p className="os-account-credential-confirm-line">
            Revoke <strong>{name}</strong>? It stops working immediately and cannot be restored.
            The row stays listed, marked revoked, as the record that it existed.
          </p>
          <div className="os-account-form-actions">
            <Button
              tone="danger"
              busy={busy}
              busyLabel="Revoking..."
              onClick={onRevoke}
              ariaLabel={`Revoke ${name} now`}
            >
              Revoke it
            </Button>
            <Button disabled={busy} onClick={onKeep}>
              Keep it
            </Button>
          </div>
        </div>
      ) : null}
    </>
  );
}
