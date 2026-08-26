import { useCallback, useState, type FormEvent, type ReactNode } from "react";

import { Button, TextInput } from "../ui";
import { ErrorMessage } from "../components/StatusMessage";

// "Regenerate this view" (epic memql#4661, task memql#4669).
//
// ===========================================================================
// WHERE EPIC 1's SYNAPSE AFFORDANCE GOES
// ===========================================================================
// The design puts this control on Epic 1's shared Synapse affordance -- the
// one with the token float. That component is not in this repository yet, so
// this is the control's own implementation, built to the SAME SHAPE so
// swapping it is a change to this file and nothing else: a trigger, an
// optional typed hint, a cost reading, a busy state and a failure that leaves
// the page alone.
//
// Written as a stub-behind-its-interface rather than deferred, because task
// memql#4669's own note says to do exactly that, and because a regenerate
// button that does not exist is indistinguishable from the two years the
// suggest domain spent unregistered.
//
// ===========================================================================
// THE HINT IS OPTIONAL AND THAT IS THE DEFAULT
// ===========================================================================
// Regenerate is a BUTTON before it is a conversation. Somebody who wants "more
// visual, lead with the chart" can say so; somebody who wants a different
// arrangement presses the button. Making the hint required would turn a
// one-click action into a writing task, and most of the time there is nothing
// to say.
//
// ===========================================================================
// THE COST READING IS AN ESTIMATE UNTIL A PROVIDER SAYS OTHERWISE
// ===========================================================================
// AiSuggestResult carries real usage where the provider reports it (task
// memql#4667); absent that, this says "about" and means it. A confident figure
// nobody measured is worse than an honest approximation, so the two are
// worded differently rather than rendered the same.

export interface RegenerateActionProps {
  onRun: (hint: string) => void;
  busy: boolean;
  error: string;
  onDismiss: () => void;
  // The last measured cost, when a provider reported one. Absent means no call
  // has been made yet on this page, or the provider said nothing.
  usedTokens?: number;
}

// ESTIMATED_TOKENS is the fallback reading: roughly what one arrangement call
// costs for a page of a few sections. A round number said as a round number,
// which is the honest way to show a figure derived from nothing measured.
const ESTIMATED_TOKENS = 4000;

export function RegenerateAction({
  onRun,
  busy,
  error,
  onDismiss,
  usedTokens,
}: RegenerateActionProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [hint, setHint] = useState("");

  const submit = useCallback(
    (event: FormEvent) => {
      event.preventDefault();
      onRun(hint.trim());
      setOpen(false);
    },
    [hint, onRun],
  );

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex items-center gap-2">
        <span className="text-xs text-subtle" aria-hidden="true">
          {usedTokens === undefined
            ? `about ${ESTIMATED_TOKENS.toLocaleString()} tokens`
            : `${usedTokens.toLocaleString()} tokens last run`}
        </span>
        <Button
          size="xs"
          tone="quiet"
          onClick={() => (busy ? undefined : setOpen((v) => !v))}
          disabled={busy}
        >
          {busy ? "Regenerating…" : "Regenerate"}
        </Button>
      </div>

      {open && !busy ? (
        <form onSubmit={submit} className="flex items-center gap-2">
          {/* The kit's TextInput, not a bare <input>: it carries the shared
              control height, the focus ring and the disabled treatment, which
              is what lets it share a row with the button beside it. */}
          <TextInput
            value={hint}
            onChange={setHint}
            placeholder="more visual, lead with the chart"
            ariaLabel="What should change about this page"
          />
          <Button size="xs" tone="primary" onClick={() => onRun(hint.trim())}>
            Go
          </Button>
        </form>
      ) : null}

      {error !== "" ? (
        <div className="max-w-md">
          <ErrorMessage>
            {error} The page below is unchanged.
            <span className="ml-2 inline-block">
              <Button size="xs" onClick={onDismiss}>
                Dismiss
              </Button>
            </span>
          </ErrorMessage>
        </div>
      ) : null}
    </div>
  );
}
