import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { ErrorMessage } from "../components/StatusMessage";
import type { WriteState } from "./useAdminConsole";

// What happened to the last write, and the row that records it.
//
// ===========================================================================
// THE AUDIT ID IS SURFACED, NOT SWALLOWED
// ===========================================================================
// Every write on this console emits a v1:identity:auditEvent and the reply
// carries its id — on a REFUSAL too, because the refusal is itself an audited
// event (`admin_auth_forbidden`). A status line that said only "Saved." would
// throw away the one durable artefact of the action.
//
// So the id is shown, monospace, next to a link into the Audit view. It is the
// thing an operator quotes in an incident thread and the thing they hand a
// colleague who asks "who changed this". Same convention as the deploy
// console's ActionResult.auditEventId.
//
// A REFUSAL READS AS A REFUSAL. The message comes back from the cluster
// verbatim -- "requires the owner or admin role (you hold "writer")" -- rather
// than being re-phrased here, because the server's answer is the authoritative
// one and a console that paraphrases it will eventually paraphrase it wrongly.
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
          <AuditLink id={state.auditEventId} />
        </p>
      ) : (
        <ErrorMessage>
          {state.error}
          <AuditLink id={state.auditEventId} />
        </ErrorMessage>
      )}
    </div>
  );
}

function AuditLink({ id }: { id: string }): ReactNode {
  if (id === "") return null;
  return (
    <>
      {" "}
      <span className="text-xs text-muted">
        Audited as <span className="font-mono break-all">{id}</span> —{" "}
        <Link to="/views/audit" className="underline hover:text-fg">
          open the trail
        </Link>
      </span>
    </>
  );
}
