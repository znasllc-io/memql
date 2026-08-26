import type { ReactNode } from "react";

import { useCanAdminister } from "../cluster/roles";

// How this product tells somebody that something failed (decision D5).
//
// ===========================================================================
// TWO AUDIENCES, ONE BOX
// ===========================================================================
// An engine error string is written for whoever is going to fix the engine.
// "rpc error: code = PermissionDenied desc = row admission refused for
// v1:worker:registration" is exactly right in a log and exactly wrong as the
// only thing on a person's screen: it names a mechanism they cannot act on,
// in vocabulary they have no reason to know, and it strongly implies the
// product is broken when the answer may be "ask an owner for the role".
//
// So every error the portal shows is a PLAIN SENTENCE saying what happened
// and, where there is one, what to do next -- and the raw string is kept,
// verbatim, behind a collapsed disclosure that only an owner or admin sees.
// Nothing is thrown away; it is filed where the person who can use it will
// look.
//
// The gate is a COURTESY, not a boundary. The raw string is already in the
// browser this component is running in -- a reader who opens devtools can see
// it, and nothing here pretends otherwise. What the gate buys is that the
// default screen for the eighty percent of people who cannot act on a stack
// trace is not a stack trace. (src/cluster/roles.ts owns the predicate; every
// real gate in this product is server-side, per stream and per row.)
//
// ===========================================================================
// THE VOICE RULES, RESTATED WHERE THEY BITE
// ===========================================================================
//   * State what happened, then what to do. "Could not read the machines."
//     "Ask a cluster owner to change your role."
//   * Never apologise and never hedge. "Sorry, something went wrong" tells a
//     person nothing and costs them the sentence they needed.
//   * No engine internals in the visible sentence: no concept ids, no env
//     keys, no Go identifiers, no gRPC codes. Those are what `detail` is for.
//   * Do not paraphrase the raw string INTO the sentence. Write the sentence
//     from what the CALL was trying to do -- the call site knows that, and it
//     is the fact the reader needs. A paraphrase of an error you have not read
//     is how a console ends up confidently wrong.
//
// ui/README.md carries the full set. This component is the single seam the
// repo-root error-discipline gate checks against, so keeping its API the
// obvious default is what keeps that gate's allowlist short.

export function ErrorNotice({
  sentence,
  next,
  detail,
}: {
  // What happened, in the interface's voice. A complete sentence.
  sentence: ReactNode;
  // What to do about it. Omitted when there is nothing honest to suggest --
  // an invented next step is worse than none.
  next?: ReactNode;
  // The raw string the cluster (or the browser) produced. Kept verbatim,
  // shown to owners and admins only. Omitted where the failure produced no
  // machine-readable detail worth filing.
  detail?: string;
}): ReactNode {
  const canAdminister = useCanAdminister();
  const showDetail = canAdminister && detail !== undefined && detail !== "";

  return (
    <div
      role="alert"
      className="rounded border border-danger bg-danger-subtle px-3 py-2 text-sm text-fg"
    >
      <p>
        {sentence}
        {next === undefined ? null : <> {next}</>}
      </p>
      {showDetail ? (
        <details className="mt-2">
          <summary className="motion-wash cursor-pointer text-xs text-muted hover:text-fg">
            Technical details
          </summary>
          <p className="mt-1 font-mono text-xs break-all whitespace-pre-wrap text-muted">
            {detail}
          </p>
        </details>
      ) : null}
    </div>
  );
}
