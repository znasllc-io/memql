import type { ReactNode } from "react";

import { Button, Caption } from "../../../kit";
import type { Refusal } from "../packages/actions";
import { toneFor } from "../packages/refusals";
import { ProblemNotice } from "../packages/ReportView";

// Connect GitHub: one button and one sentence (epic memql#4915).
//
// ===========================================================================
// THIS SURFACE SPENDS NO BOLDNESS, AND THAT IS THE DECISION
// ===========================================================================
// The win here is subtractive -- a typing task becomes a choosing task -- so
// there is no splash, no vendor mark and no black button. The vendor's brand
// is not this shell's brand, and a logo here would be the only logo in the
// OS. The measure of this control is that a connected person never notices
// it was there.
//
// It is `quiet` for the same reason. `primary` means "the one thing this
// surface is for" (kit/controls.tsx), and pasting a token remains a
// legitimate first choice for a self-hosted host or an organization that
// will not install an app -- so this is the recommended path, not the only
// one, and the tone says so.
//
// PROP-DRIVEN ON PURPOSE. The write hook belongs to whatever mounts this --
// the Sources settings group today, the Source stop when its rebuild lands
// -- so a refusal renders beside the control that produced it and this file
// knows nothing about either surface.

export function ConnectGitHub({
  label = "Connect GitHub",
  caption = "Pick repositories from a list instead of pasting a URL and a token.",
  busy,
  refusal,
  onConnect,
}: {
  /** "Connect GitHub", or "Reconnect GitHub" where a grant has lapsed. */
  label?: string;
  /** The one sentence under the button. */
  caption?: ReactNode;
  busy: boolean;
  refusal: Refusal | null;
  onConnect: () => void;
}) {
  return (
    <>
      <div className="os-form-row">
        {/* BUSY, AND THEREFORE DISABLED, FOR THE WHOLE CALL. Beginning a
            connect mints a state row bound to this person, so a double click
            is two rows and the second one is the only one that can ever be
            consumed. The kit's Button disables itself while busy; the
            busyLabel says what is happening, because the page is about to
            navigate away and a control that looked idle for a second would
            read as one that did nothing. */}
        <Button busy={busy} busyLabel="Opening GitHub" onClick={onConnect}>
          {label}
        </Button>
        <Caption>{caption}</Caption>
      </div>
      {/* IN PLACE, NEVER A TOAST. `github_app_not_configured` is an
          operator's condition rather than this person's, and its copy says
          so -- which only works if it is rendered where they just clicked.
          It is also why the TONE is read from the code (`toneFor`) rather
          than fixed to error: a cluster with no GitHub App is somebody's next
          step, and the fault colour would say this person broke it. */}
      {refusal ? <ProblemNotice problem={refusal} tone={toneFor(refusal.code)} /> : null}
    </>
  );
}
