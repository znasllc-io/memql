import { useState } from "react";

import { Button, Caption, Chip, formatFreshness, Head, Notice, Panel, Row, Subhead, useNow } from "../../kit";
import { useSession } from "../../chrome/access";
import type { RoleRequirement } from "../../system/roles";
import { useTokenFacts, MAX_PEOPLE_SCANNED, type NodeTokenRow, type TokenRow } from "./tokenFacts";
import { useSettingsWrites } from "./settingsWrites";

// Tokens (epic memql#4984): every credential in this cluster that is not a
// person's browser session, and the one act that can be taken on one.
//
// ADMIN. The reads gate themselves on `requiresOwnerOrAdmin` and adminops
// refuses the revokes below that, so the floor here matches what the cluster
// will actually do.
//
// FOUR RULES:
//
//  1. TWO POPULATIONS, NOT ONE LIST. A personal access token is a person
//     acting as themselves from a CLI; a node credential is a process the
//     cluster bootstrapped. Different ops revoke them, they fail differently,
//     and somebody hunting a leaked key already knows which they are after.
//  2. REVOKE IS ABSENT ON A REVOKED ROW, never disabled (DESIGN.md rule 12).
//     A disabled control advertises an act whose only explanation is a
//     refusal; the state chip beside the row already says why there is
//     nothing to press.
//  3. THE CONFIRM NAMES WHAT BREAKS. "Revoke?" and "Revoke the credential for
//     bff-2, which cannot rejoin until it is re-bootstrapped?" are different
//     questions, and the second is the one being asked. In surface, never
//     `window.confirm` -- a modal the desktop did not draw is the loudest tell
//     that this is a tab, and a refusal inside a dialog that then closes is a
//     refusal nobody can re-read.
//  4. THE SECTION REPORTS ITS OWN COVERAGE. There is no cluster-wide PAT
//     query, so the personal half is a fan-out over the people list with a
//     ceiling. A surface that quietly examined 200 of 900 people and drew a
//     complete-looking list would be worse than one that examined 200 and said
//     so -- an operator would conclude a token they cannot find does not
//     exist.

/** The section's role floor. Presentation only; every gate is server-side. */
export const TOKENS_SECTION_ROLE: RoleRequirement = { min: "admin" };

export function TokensSection() {
  const { access } = useSession();
  const facts = useTokenFacts(true);
  const writes = useSettingsWrites();
  const [asking, setAsking] = useState("");
  // Elapsed, not an ISO string. A raw `2026-09-05T18:22:00Z` is the value the
  // row carries and not the answer to the question somebody opens this section
  // with, which is "is anything still using it".
  const now = useNow();

  const live = facts.tokens.filter((t) => t.active).length;
  const liveNodes = facts.nodeTokens.filter((t) => t.active).length;

  return (
    <div className="os-settings">
      <Head
        title="Tokens"
        meta={
          facts.loading
            ? "Reading"
            : `${live} personal, ${liveNodes} node, in use`
        }
      />
      <p className="os-caption">
        Every credential that is not a browser session. Revoking one takes
        effect on the next request it is used for, and cannot be undone -- a
        replacement is a new token, never the same one back.
      </p>

      {facts.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.clusterRole || "your role"}.`}
          detail={facts.error}
        />
      ) : null}

      {writes.refusal ? (
        <Notice
          tone="error"
          sentence={
            writes.refusal.denied
              ? "The cluster refused that, and nothing was revoked."
              : "That did not go through, and nothing was revoked."
          }
          next={
            writes.refusal.auditEventId
              ? `Recorded as audit event ${writes.refusal.auditEventId}.`
              : undefined
          }
          detail={writes.refusal.detail}
        />
      ) : null}
      {writes.done ? <Notice tone="info" sentence={writes.done} /> : null}

      <Panel label="Personal access tokens">
        <Subhead>Personal access tokens</Subhead>
        {facts.tokens.length === 0 ? (
          <Caption>
            {facts.loading
              ? "Reading across the people in this cluster"
              : "Nobody has issued a personal access token. They are minted from the CLI, and a cluster can run without one."}
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Personal access tokens">
            {facts.tokens.map((token) => (
              <li key={token.id}>
                <TokenLine
                  token={token}
                  now={now}
                  asking={asking === token.id}
                  busy={writes.busyKey === token.id}
                  onAsk={() => {
                    writes.clear();
                    setAsking(token.id);
                  }}
                  onCancel={() => setAsking("")}
                  onConfirm={() => {
                    setAsking("");
                    void writes.revokeToken(token.id, `${token.label} (${token.owner})`);
                  }}
                />
              </li>
            ))}
          </ul>
        )}
        <Caption>
          {facts.capped
            ? `Read across the first ${MAX_PEOPLE_SCANNED} of ${facts.people} people. The cluster publishes no all-people token read, so this list is a fan-out with a ceiling -- a token belonging to somebody past that point is not shown and does not appear here as absent.`
            : facts.people === 0
              ? "No people to read across yet."
              : `Read across all ${facts.people} people in this cluster.`}
        </Caption>
      </Panel>

      <Panel label="Node credentials">
        <Subhead>Node credentials</Subhead>
        {facts.nodeTokens.length === 0 ? (
          <Caption>
            {facts.loading ? "Reading the credential list" : "No node has bootstrapped a credential."}
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Node credentials">
            {facts.nodeTokens.map((token) => (
              <li key={token.id}>
                <NodeTokenLine
                  token={token}
                  now={now}
                  asking={asking === token.id}
                  busy={writes.busyKey === token.id}
                  onAsk={() => {
                    writes.clear();
                    setAsking(token.id);
                  }}
                  onCancel={() => setAsking("")}
                  onConfirm={() => {
                    setAsking("");
                    void writes.revokeNodeToken(token.id, token.node);
                  }}
                />
              </li>
            ))}
          </ul>
        )}
      </Panel>

      <div className="os-refresh-row">
        <Button onClick={facts.reload} busy={facts.loading} busyLabel="Reading">
          Refresh
        </Button>
        <Caption>
          {facts.fetchedAt === null
            ? "Not read yet."
            : `Read at ${new Date(facts.fetchedAt).toISOString()}.`}
        </Caption>
      </div>
    </div>
  );
}

function TokenLine({
  token,
  now,
  asking,
  busy,
  onAsk,
  onCancel,
  onConfirm,
}: {
  token: TokenRow;
  now: Date;
  asking: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <>
      <Row
        name={token.label}
        current={token.active}
        dim={!token.active}
        // ONE STATE AND ONE ACT, and the count is the point. Two chips plus a
        // button in the trailing cluster wrapped "In use" onto two lines and
        // "Agents may use it" onto three at the measure a Settings section
        // actually renders at, which left three rows at three different
        // heights. Everything that is a FACT rather than a state moved into
        // the quiet middle below, where facts live.
        state={
          <>
            <Chip tone={token.active ? "accent" : "muted"}>
              {token.active ? "In use" : "Revoked"}
            </Chip>
            {/* ABSENT, not disabled, on a revoked row. */}
            {token.active ? (
              <Button tone="danger" onClick={onAsk} busy={busy} busyLabel="Revoking" ariaLabel={`Revoke ${token.label}`}>
                Revoke
              </Button>
            ) : null}
          </>
        }
      >
        {/* ONE ELEMENT, not a fragment. `Row` renders its children as direct
            siblings inside a flex row with a gap, so a fragment of four pieces
            becomes four flex items -- the sentence arrives as columns with
            gutters between them and the trailing chip loses the width it needs
            to stay on one line. Every other caller in the shell passes one
            span; this is why. */}
        <span className="os-caption" title={token.lastUsedAt || undefined}>
          <span className="os-mono">{token.owner}</span> -- last used{" "}
          {formatFreshness(token.lastUsedAt, now)}
          {token.usableByAgents ? " -- agents may use it" : ""}
        </span>
      </Row>
      {asking ? (
        // THE CONSEQUENCE IS ABOVE THE ACTS, not below them. `Notice` renders
        // `children` before `next`, so putting the buttons in `children` and
        // the consequence in `next` gave the reading order question -> buttons
        // -> what happens, and somebody who has already decided by the time
        // they reach the sentence has not been told anything. Seen in a
        // browser; jsdom has no reading order to get wrong.
        <Notice tone="warn" sentence={`Revoke ${token.label}, held by ${token.owner}?`}>
          <p className="os-caption">
            Anything using it stops working on its next request. A replacement
            is a new token, minted from the CLI.
          </p>
          <div className="os-refresh-row">
            <Button tone="danger" onClick={onConfirm}>
              Revoke it
            </Button>
            <Button onClick={onCancel}>Keep it</Button>
          </div>
        </Notice>
      ) : null}
    </>
  );
}

function NodeTokenLine({
  token,
  now,
  asking,
  busy,
  onAsk,
  onCancel,
  onConfirm,
}: {
  token: NodeTokenRow;
  now: Date;
  asking: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <>
      <Row
        name={token.node}
        current={token.active}
        dim={!token.active}
        // Same one-state-one-act rule as the personal half above. The node
        // TYPE is a fact and reads below.
        state={
          <>
            <Chip tone={token.active ? "accent" : "muted"}>
              {token.active ? "In use" : "Revoked"}
            </Chip>
            {token.active ? (
              <Button
                tone="danger"
                onClick={onAsk}
                busy={busy}
                busyLabel="Revoking"
                ariaLabel={`Revoke the credential for ${token.node}`}
              >
                Revoke
              </Button>
            ) : null}
          </>
        }
      >
        <span className="os-caption" title={token.lastConnectAt || undefined}>
          <span className="os-mono">{token.nodeType}</span>, minted by {token.mintedBy} -- last
          connected {formatFreshness(token.lastConnectAt, now)}
          {token.expiresAt ? ` -- expires ${token.expiresAt}` : ""}
        </span>
      </Row>
      {asking ? (
        <Notice tone="warn" sentence={`Revoke the credential for ${token.node}?`}>
          <p className="os-caption">
            That node loses its place in the mesh on its next connect and cannot
            rejoin until it is re-bootstrapped. If it is serving traffic now, it
            will stop.
          </p>
          <div className="os-refresh-row">
            <Button tone="danger" onClick={onConfirm}>
              Revoke it
            </Button>
            <Button onClick={onCancel}>Keep it</Button>
          </div>
        </Notice>
      ) : null}
    </>
  );
}
