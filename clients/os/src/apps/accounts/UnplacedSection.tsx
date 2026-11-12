import { useState } from "react";

import {
  Button,
  Caption,
  DetailsSection,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
} from "../../kit";
import { InPlaceAsk } from "../../kit/pressHold";
import type { Reading } from "../../cluster/reading";
import { areaLabel, rowsPhrase } from "./inventory";
import { kindName, type OpenConflict, type Unplaced, type UnplacedKind } from "./moves";
import { useMoveCalls } from "./useAccounts";

// THE ROWS THAT AWAIT A DECISION, AND THE ROWS IN NO ACCOUNT (design record
// 2026-11-12-account-export, section I), on the primary account's Data tab
// only: they belong to no account yet, and the primary account's page is the
// cluster's own.
//
// ONE CALL FEEDS BOTH (`accountUnplaced`), so one refusal speaks for both, in
// the first section, and the second waits for an answer.
//
// DRAWN ONLY FOR A HOLDER of `execute app:accounts/move`; the Data tab decides.
//
// A CONFLICT IS RESOLVED ONE OF TWO WAYS: placed in an account (the move
// wizard, its candidates offered first), which carries what follows it; or left
// out for good, which records the decision and leaves the row in no account.
// Leaving out cannot be taken back here, so it asks in place, naming the row.

/** A conflict's code in words. Anything else reads as the engine sent it. */
function conflictSentence(conflict: OpenConflict): string {
  if (conflict.detail !== "") return conflict.detail;
  switch (conflict.conflict) {
    case "store_bound_twice":
      return "Its store is bound to more than one account.";
    case "library_fence_refused":
      return "Its folder and its own account disagree.";
    default:
      return conflict.conflict;
  }
}

export function UnplacedSection({
  reading,
  onPlace,
  onOpenKind,
  onLeftOut,
}: {
  reading: Reading<Unplaced>;
  /** Opens the wizard on one conflict's row. */
  onPlace: (conflict: OpenConflict) => void;
  /** Opens a kind's rows in no account. */
  onOpenKind: (kind: UnplacedKind) => void;
  /** A conflict was left out: the pages behind read again. */
  onLeftOut: () => void;
}) {
  if (reading.state === "failed") {
    return (
      <DetailsSection title="Awaiting a decision">
        <Notice tone="error" sentence="The rows awaiting a decision were not read." next="Nothing below is current." detail={reading.error}>
          <Button onClick={reading.reread}>Try again</Button>
        </Notice>
      </DetailsSection>
    );
  }
  if (reading.value === null) {
    return (
      <DetailsSection title="Awaiting a decision">
        {reading.state === "unread" ? (
          <Caption>Not connected to the cluster.</Caption>
        ) : (
          <RecordListSkeleton label="Loading rows awaiting a decision" rows={2} />
        )}
      </DetailsSection>
    );
  }
  const { conflicts, kinds } = reading.value;
  return (
    <>
      <DetailsSection title="Awaiting a decision">
        {conflicts.length === 0 ? (
          <Caption>No row awaits a decision.</Caption>
        ) : (
          <RecordList as="ul" label="Rows awaiting a decision">
            {conflicts.map((c) => (
              <li key={`${c.concept} ${c.rowId} ${c.conflict}`}>
                <ConflictLine conflict={c} onPlace={() => onPlace(c)} onLeftOut={onLeftOut} />
              </li>
            ))}
          </RecordList>
        )}
      </DetailsSection>
      <DetailsSection title="In no account">
        {kinds.length === 0 ? (
          <Caption>Every row is in an account.</Caption>
        ) : (
          <RecordList as="ul" label="Kinds with rows in no account">
            {kinds.map((k) => (
              <li key={k.concept}>
                <RecordRow
                  name={k.name}
                  secondary={areaLabel(k.namespace)}
                  onOpen={() => onOpenKind(k)}
                  label={`${k.name}, ${rowsPhrase(k.rows)} in no account`}
                >
                  <span className="os-caption">{rowsPhrase(k.rows)}</span>
                </RecordRow>
              </li>
            ))}
          </RecordList>
        )}
      </DetailsSection>
    </>
  );
}

function ConflictLine({ conflict, onPlace, onLeftOut }: { conflict: OpenConflict; onPlace: () => void; onLeftOut: () => void }) {
  const calls = useMoveCalls();
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function leaveOut() {
    if (calls === null || busy) return;
    setBusy(true);
    setError("");
    try {
      await calls.leaveOut(conflict);
      setAsking(false);
      onLeftOut();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <RecordRow
        name={conflict.rowId}
        secondary={kindName(conflict.concept)}
        actions={asking ? undefined : (
          <>
            <Button onClick={onPlace} ariaLabel={`Place ${conflict.rowId} in an account`}>Place</Button>
            <Button onClick={() => { setError(""); setAsking(true); }} ariaLabel={`Leave out ${conflict.rowId}`}>Leave out</Button>
          </>
        )}
      >
        <span className="os-caption">{conflictSentence(conflict)}</span>
      </RecordRow>
      {asking ? (
        // The question above its buttons, the buttons on the right edge
        // (rule 8), all of it held a moment after it appears (rule 12).
        <InPlaceAsk role="group" aria-label={`Leave out ${conflict.rowId}?`}>
          <Caption>Leave out {conflict.rowId}? It stays in no account for good, and exports stop waiting for it.</Caption>
          {error === "" ? null : <Notice tone="error" sentence="It was not left out." detail={error} />}
          <div className="os-panel-actions">
            <Button onClick={() => { setAsking(false); setError(""); }}>Keep it</Button>
            <Button tone="danger" busy={busy} onClick={() => void leaveOut()}>
              {error === "" ? "Leave out" : "Try again"}
            </Button>
          </div>
        </InPlaceAsk>
      ) : null}
    </>
  );
}
