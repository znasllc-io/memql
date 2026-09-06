import { useState } from "react";
import { ArrowLeft } from "lucide-react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Notice, Panel, Subhead } from "../../kit";
import { cardFor, fieldText } from "./displayCard";
import { rowIdOf, type ConceptRowsWalk } from "./useConceptRows";

// A concept's rows: the walk, the band, and one row.
//
// The row detail REPLACES the list inside this column (rule 11) rather than
// sitting under it, and carries its own quiet back control.
//
// It renders the row the WALK holds rather than issuing a fresh read of it.
// A second authoritative read would be a different snapshot from the list
// beside it, so opening a row could show one thing while the line it came
// from showed another, with nothing on screen to say why. The band above is
// what tells somebody the cluster has moved on, and the reload is how they
// ask for the newer answer -- one refresh point rather than two that
// disagree.

export function RowsPanel({ concept, walk }: { concept: Concept; walk: ConceptRowsWalk }) {
  const [openId, setOpenId] = useState("");

  if (openId !== "") {
    const row = walk.rows.find((r) => rowIdOf(r) === openId);
    if (row) {
      return <RowDetail concept={concept} row={row} onBack={() => setOpenId("")} />;
    }
    // The row went out of the walk under us (a reload). Fall through to the
    // list rather than rendering an empty detail.
  }

  return (
    <Panel label="Rows">
      <Subhead>Rows</Subhead>

      {/* THE BAND. New rows are counted, never spliced -- see
          useConceptRows' header for why a keyset walk cannot absorb them. */}
      {walk.arrivedCount > 0 || walk.changedCount > 0 ? (
        <div className="os-rows-band">
          <span>
            {walk.arrivedCount > 0
              ? `${walk.arrivedCount} new since you opened this`
              : "Nothing new"}
            {walk.changedCount > 0
              ? `, ${walk.changedCount} loaded ${walk.changedCount === 1 ? "row has" : "rows have"} changed`
              : ""}
          </span>
          <Button tone="quiet" onClick={walk.reload}>
            Reload the rows
          </Button>
        </div>
      ) : null}

      {walk.status === "failed" ? (
        <Notice
          tone="error"
          sentence="These rows could not be read."
          detail={walk.error}
          next="The concept may be gated to a role this account does not hold."
        />
      ) : null}

      {walk.rows.length === 0 && walk.status === "loading" ? (
        <Caption>Reading rows from the cluster.</Caption>
      ) : null}

      {walk.rows.length === 0 && walk.status === "exhausted" ? (
        // NOT "this concept is empty". Row admission decides what reaches
        // this browser, so an empty answer means "none that you may read",
        // and the stronger sentence would be this window inventing a fact
        // about the cluster.
        <Caption>No rows of this concept are readable by this account.</Caption>
      ) : null}

      {walk.rows.length === 0 ? null : (
        <ul className="os-rows-list">
          {walk.rows.map((row) => {
            const card = cardFor(concept, row);
            return (
              <li key={card.id} className="os-rows-row">
                <button
                  type="button"
                  className="os-rows-open"
                  onClick={() => setOpenId(card.id)}
                >
                  <span className="os-rows-primary">{card.primary}</span>
                  {card.secondary === "" ? null : (
                    <span className="os-rows-secondary">{card.secondary}</span>
                  )}
                  {card.tertiary === "" ? null : (
                    <span className="os-rows-tertiary">{card.tertiary}</span>
                  )}
                </button>
                {card.status === "" ? null : <Chip tone="muted">{card.status}</Chip>}
              </li>
            );
          })}
        </ul>
      )}

      {/* FOUR STATES, EACH SAYING SOMETHING DIFFERENT. "Loaded N" with no
          more to come and "loaded N, more available" are the two a reader
          most needs told apart: the first means the number below is the
          whole answer. */}
      <div className="os-rows-foot">
        {walk.status === "loading" && walk.rows.length > 0 ? (
          <Caption>Reading more rows.</Caption>
        ) : null}
        {walk.status === "more" ? (
          <>
            <Caption>{walk.rows.length} rows loaded, more available.</Caption>
            <Button tone="quiet" onClick={walk.loadMore}>
              Load more
            </Button>
          </>
        ) : null}
        {walk.status === "exhausted" && walk.rows.length > 0 ? (
          <Caption>
            All {walk.rows.length} readable rows loaded. That is the whole concept as far as
            this account can see it.
          </Caption>
        ) : null}
        {walk.status === "failed" ? (
          <Button tone="quiet" onClick={walk.reload}>
            Try again
          </Button>
        ) : null}
      </div>
    </Panel>
  );
}

function RowDetail({
  concept,
  row,
  onBack,
}: {
  concept: Concept;
  row: Row;
  onBack: () => void;
}) {
  const card = cardFor(concept, row);
  // The RAW row, nesting kept: a generic inspector needs the intrinsics that
  // flattening drops, and `payload` / `provenance` / the intrinsics staying
  // distinguishable is the difference between reading a row and reading a
  // map of strings.
  const entries = Object.entries(row).filter(([key]) => key !== "payload");
  const payload = row["payload"];
  const payloadEntries =
    payload !== null && typeof payload === "object" && !Array.isArray(payload)
      ? Object.entries(payload as Record<string, unknown>)
      : [];

  return (
    <Panel label="Row">
      <div className="os-rows-detail-head">
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Rows
        </Button>
        <Subhead>{card.primary}</Subhead>
      </div>

      {payloadEntries.length > 0 ? (
        <dl className="os-row-fields">
          {payloadEntries.map(([key, value]) => (
            <div key={key} className="os-row-field">
              <dt>{key}</dt>
              <dd>{longFieldText(value)}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <Caption>This row carries no payload fields.</Caption>
      )}

      {entries.length === 0 ? null : (
        <>
          <Subhead>Row intrinsics</Subhead>
          <dl className="os-row-fields os-row-intrinsics">
            {entries.map(([key, value]) => (
              <div key={key} className="os-row-field">
                <dt>{key}</dt>
                <dd>{longFieldText(value)}</dd>
              </div>
            ))}
          </dl>
        </>
      )}
    </Panel>
  );
}

/**
 * A value written out in full, for the detail.
 *
 * Unlike the card's `fieldText`, this does NOT summarise an object as
 * `{ ... }`: the detail is where somebody came to read the whole row, and a
 * summary there is the surface refusing the one question it was opened to
 * answer.
 */
function longFieldText(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "object") {
    try {
      return JSON.stringify(value, null, 2);
    } catch {
      // A cycle cannot come off the wire, but a stringify that throws must
      // not take the row's other fields down with it.
      return fieldText(value);
    }
  }
  return fieldText(value);
}
