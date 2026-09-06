import { useEffect, useState } from "react";

import { Button, Chip, Chips, Notice } from "../../kit";
import { useArtifactLabels } from "./actions/labels";

// The inspector's label editor (epic memql#5009).
//
// ===========================================================================
// THE ROW IS THE AUTHORITY; THE OVERLAY ONLY COVERS THE ROUND TRIP
// ===========================================================================
// `v1:library:artifact` broadcasts `updated`, so the real answer arrives on
// the feed the browse and this panel both read. The overlay exists because
// the control here is TYPED rather than toggled: a chip that appears only
// after the echo reads as a keystroke that was dropped. So an edit shows
// immediately, the echo replaces it the moment the two agree, and a REFUSAL
// puts the list back exactly as it was with the server's own sentence beside
// it -- a label that appears and is silently gone at the next reload is worse
// than one that visibly refuses.
//
// ONE WRITE AT A TIME. Every control is disabled while one is in flight,
// which is what makes the revert exact: the value captured before the edit is
// still the value to go back to when the answer lands.
//
// THE FREE-TEXT NOTE IS SAID ONCE (DESIGN.md rule 7), under the control that
// takes the text -- not repeated on every chip, and not in the browse's own
// facet, which is asking a different question about the same field.

/** Value-identity for a label list, so an effect can compare two arrays. */
function labelKey(labels: readonly string[]): string {
  return JSON.stringify(labels);
}

export function LabelEditor({
  artifactId,
  labels: rowLabels,
}: {
  artifactId: string;
  /** The labels the row itself carries -- the authority. */
  labels: readonly string[];
}) {
  const write = useArtifactLabels();
  // null = no local overlay, render the row. A list = an edit whose echo has
  // not arrived yet.
  const [overlay, setOverlay] = useState<string[] | null>(null);
  const [draft, setDraft] = useState("");
  const labels = overlay ?? [...rowLabels];
  const rowKey = labelKey(rowLabels);

  // STALENESS RESOLVES TOWARD THE ROW. Once the broadcast carries what the
  // overlay was claiming, the overlay has nothing left to say -- and dropping
  // it is what lets a change made in another tab reach this panel.
  useEffect(() => {
    setOverlay((held) => (held !== null && labelKey(held) === rowKey ? null : held));
  }, [rowKey]);

  // A different file: the overlay belonged to the previous one.
  useEffect(() => {
    setOverlay(null);
    setDraft("");
  }, [artifactId]);

  async function add() {
    const label = draft.trim();
    // BLANK IS REFUSED RATHER THAN SENT. An empty label is the one value the
    // browse's facet uses as its "no constraint" sentinel, and it is not a
    // name anybody meant to type.
    if (label === "") return;
    setDraft("");
    if (labels.includes(label)) return;
    const before = overlay;
    setOverlay([...labels, label]);
    const ok = await write.add(artifactId, label);
    if (!ok) setOverlay(before);
  }

  async function remove(label: string) {
    const before = overlay;
    setOverlay(labels.filter((one) => one !== label));
    const ok = await write.remove(artifactId, label);
    if (!ok) setOverlay(before);
  }

  const inputId = `files-label-${artifactId}`;

  return (
    <div className="os-files-labeledit">
      <Chips label="Labels">
        {labels.length === 0 ? (
          <span className="os-caption">None yet.</span>
        ) : (
          labels.map((label) => (
            <span key={label} className="os-chip-editable" role="listitem">
              <Chip>{label}</Chip>
              <button
                type="button"
                className="os-chip-remove"
                aria-label={`Remove label ${label}`}
                disabled={write.busy}
                onClick={() => void remove(label)}
              >
                &times;
              </button>
            </span>
          ))
        )}
      </Chips>

      <div className="os-form-row">
        <label className="os-sr-only" htmlFor={inputId}>
          Add a label
        </label>
        <input
          id={inputId}
          className="os-input"
          value={draft}
          disabled={write.busy}
          placeholder="Add a label"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter") return;
            // The inspector is not a form, but a stray submit from a future
            // one would reload the window -- cheap to refuse now.
            e.preventDefault();
            void add();
          }}
        />
        <Button onClick={() => void add()} disabled={write.busy || draft.trim() === ""}>
          Add
        </Button>
      </div>

      <p className="os-caption">
        Labels are free text -- your own, or added by an agent you talked to. They are how you
        find a file again from the Refine control at the top of the list.
      </p>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The label was not changed."
          next="This file carries the labels shown above, which is what the cluster holds."
          detail={write.error}
        />
      )}

      <p role="status" className="os-sr-only">
        {write.announcement}
      </p>
    </div>
  );
}
