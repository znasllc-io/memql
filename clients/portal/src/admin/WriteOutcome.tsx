import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { ErrorNotice } from "../ui";
import type { WriteState } from "./useAdminConsole";

// What happened to the last write, and the row that records it.
//
// ===========================================================================
// THE AUDIT ID IS SURFACED, NOT SWALLOWED
// ===========================================================================
// Every write on this console emits a v1:identity:auditEvent and the reply
// carries its id -- on a REFUSAL too, because the refusal is itself an audited
// event (`admin_auth_forbidden`). A status line that said only "Saved." would
// throw away the one durable artefact of the action.
//
// So the id is shown as a QUIET MONO CHIP (memql#4653) beside a link into the
// Audit view. It is evidence and it stays -- what changed is that it stopped
// competing with the sentence it belongs to. It is the thing an operator
// quotes in an incident thread and hands a colleague who asks "who changed
// this". Same convention as the deploy console's ActionResult.auditEventId.
//
// ===========================================================================
// A REFUSAL STILL READS AS A REFUSAL -- the one place D5 is inverted
// ===========================================================================
// Everywhere else in the portal the raw string goes behind ErrorNotice's
// disclosure and a plain sentence takes its place. Here the raw string IS the
// plain sentence: the cluster answers "requires the owner or admin role (you
// hold \"writer\")", which is precisely what the person needs to read, and
// re-phrasing it in this file is how a console eventually paraphrases it
// wrongly. Passing it as `sentence` rather than as `detail` is therefore
// deliberate, and it keeps this component on the single ErrorNotice seam
// rather than in the error-discipline gate's allowlist.

export function WriteOutcome({ state }: { state: WriteState }): ReactNode {
  if (state.message === "" && state.error === "") return null;

  return (
    <div className="mt-3">
      {state.error === "" ? (
        <p
          role="status"
          className="rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg"
        >
          {state.message}
          <AuditChip id={state.auditEventId} />
        </p>
      ) : (
        <ErrorNotice sentence={state.error} next={<AuditChip id={state.auditEventId} />} />
      )}
    </div>
  );
}

function AuditChip({ id }: { id: string }): ReactNode {
  if (id === "") return null;
  return (
    <>
      {" "}
      <span className="inline-flex items-center gap-1.5 rounded border border-line bg-raised px-1.5 py-0.5 align-middle text-xs">
        <span className="font-mono break-all text-muted">{id}</span>
        <Link to="/views/audit" className="motion-wash text-muted hover:text-fg hover:underline">
          trail
        </Link>
      </span>
    </>
  );
}
