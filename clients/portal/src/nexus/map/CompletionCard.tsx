import type { ReactNode } from "react";

import { Badge, Button, DataText, Panel } from "../../ui";
import { formatElapsed, type Receipt } from "../scene/receipt";

// The receipt (design D8).
//
// It materializes under the goal when the goal ends, and it is the only
// "reward" this surface has: no points, no streaks, no badge. What a
// professional wants at the end of a piece of work is the record of it --
// how long, how much, what got built and what it cost.
//
// A FAILED goal gets the same card, with the failure and the last task that
// was running when it stopped. That is deliberate: the cost is real whatever
// the outcome, and a card that appears only on success is a scoreboard rather
// than a record.
//
// Rendered in the portal's own Panel and DataText rather than drawn into the
// scene, for two reasons. It is TEXT -- numbers next to labels -- and text in
// a WebGL canvas is a texture that cannot be selected, copied, read by a
// screen reader or found by ctrl-F. And it has to be present in Replay at the
// moment of success (memql#4376) where the canvas may be showing a different
// moment entirely.

function Reading({ label, value, title }: { label: string; value: ReactNode; title?: string }): ReactNode {
  return (
    <div className="min-w-24">
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-0.5 text-sm font-semibold" {...(title === undefined ? {} : { title })}>
        {value}
      </div>
    </div>
  );
}

export function CompletionCard({
  receipt,
  onDismiss,
}: {
  receipt: Receipt;
  onDismiss?: () => void;
}): ReactNode {
  const tone = receipt.outcome === "succeeded" ? "ok" : receipt.outcome === "failed" ? "danger" : "neutral";
  const elapsed = formatElapsed(receipt.elapsedMs);

  return (
    // A named REGION, not a bare panel. The receipt is the one part of this
    // surface a person may want to jump straight to -- it is the answer to
    // "what did this cost" -- and a canvas has no landmarks of its own, so
    // this is the only one the page can offer. It is also what lets a test
    // say "inside the receipt" rather than matching a word that the tab
    // strip happens to use too.
    <section aria-label="Goal receipt">
      <Panel>
        <div className="flex flex-col gap-3">
        <div className="flex items-baseline justify-between gap-3">
          <div className="flex items-center gap-2">
            <Badge tone={tone}>
              {receipt.outcome === "succeeded"
                ? "Goal reached"
                : receipt.outcome === "failed"
                  ? "Goal failed"
                  : "Goal cancelled"}
            </Badge>
            {/* An elapsed time the row could not date is ABSENT rather than
                rendered as a zero. A receipt is read as a record. */}
            {elapsed === "" ? null : <span className="text-sm text-muted">in {elapsed}</span>}
          </div>
          {onDismiss === undefined ? null : (
            <Button size="xs" tone="quiet" onClick={onDismiss}>
              Dismiss
            </Button>
          )}
        </div>

        <div className="flex flex-wrap gap-x-8 gap-y-3">
          <Reading label="Tasks" value={<DataText kind="number">{receipt.tasks}</DataText>} />
          <Reading
            label="Steps"
            value={<DataText kind="number">{receipt.attempts}</DataText>}
            title="Distinct steps; a step that was retried counts once."
          />
          <Reading label="Agents raised" value={<DataText kind="number">{receipt.agents}</DataText>} />
          <Reading label="Artifacts" value={<DataText kind="number">{receipt.artifacts}</DataText>} />
          <Reading label="Constructs" value={<DataText kind="number">{receipt.constructs}</DataText>} />
          <Reading
            label="Tokens"
            value={<DataText kind="number">{receipt.tokensSpent.toLocaleString()}</DataText>}
          />
          {/* Absent, not zero, when the engine does not carry the field --
              "the subscription covered nothing" and "this cluster does not
              record what the subscription covered" are different facts
              (epic memql#4358). */}
          {receipt.subscriptionCovered === null ? null : (
            <Reading
              label="Covered by subscription"
              value={
                <DataText kind="number">{receipt.subscriptionCovered.toLocaleString()}</DataText>
              }
            />
          )}
        </div>

        {receipt.outcome === "failed" ? (
          <p className="text-sm text-muted">
            {receipt.failure === "" ? "The goal stopped without recording a reason." : receipt.failure}
            {receipt.lastRunningTask === "" ? null : (
              <>
                {" "}
                Last running: <DataText kind="string">{receipt.lastRunningTask}</DataText>.
              </>
            )}
          </p>
        ) : null}

        {receipt.outcome === "cancelled" ? (
          <p className="text-sm text-muted">
            Cancelled
            {receipt.cancelledBy === "" ? "" : " by "}
            {receipt.cancelledBy === "" ? null : <DataText kind="id">{receipt.cancelledBy}</DataText>}
            {"."}
          </p>
        ) : null}
        </div>
      </Panel>
    </section>
  );
}
