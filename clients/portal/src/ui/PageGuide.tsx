import { useState, type ReactNode } from "react";

import { useCanAdminister } from "../cluster/roles";
import { guideFor, type GuideEntry } from "../guides";
import { Button } from "./Button";
import { Constellation } from "./Constellation";
import { Dialog } from "./Dialog";
import { Eye } from "./icons";

// The per-page guide (spec section F): a small quiet Eye beside the page
// title that opens what this page is, in words.
//
// ===========================================================================
// WHY A GUIDE RATHER THAN MORE COPY ON THE PAGE
// ===========================================================================
// An operations console is read by two people at once: somebody who lives in
// it and wants the screen to be mostly data, and somebody arriving for the
// first time who needs to know what they are looking at. Serving the second
// with permanent body copy taxes the first on every visit -- which is exactly
// how this portal ended up with env vars in blurbs and design rationale in
// error callouts.
//
// So the explanation is one click away and it is the SAME click on every
// page. Nothing is hidden that a person needs to act; what moved here is what
// they need ONCE.
//
// ===========================================================================
// THE VIDEO SLOT EXISTS BEFORE THE VIDEOS DO
// ===========================================================================
// The 16:9 area is there from the first render, with the Constellation as its
// poster. That is deliberate: a guide video lands later keyed by page id and
// costs no code change, and building the slot afterwards would mean every
// guide's layout shifts on the day the first video ships.
//
// ===========================================================================
// TECHNICAL DETAILS IS THE OTHER HALF OF THE COPY SWEEP
// ===========================================================================
// Concept ids, env keys and doc paths live here rather than on the page
// (decision D5), collapsed, and rendered only for owner and admin. Same
// courtesy-not-a-boundary reasoning as ErrorNotice: the gate decides what is
// OFFERED, and every real gate in this product is server-side.

export function PageGuide({ pageId }: { pageId: string }): ReactNode {
  const [open, setOpen] = useState(false);
  const guide = guideFor(pageId);

  // No entry, no button. An Eye that opens an empty dialog teaches a person
  // the control does nothing -- and the coverage gate is what makes this
  // branch unreachable for a real destination.
  if (guide === undefined) return null;

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label={`What is ${guide.title}?`}
        title={`What is ${guide.title}?`}
        className="motion-wash -my-1 shrink-0 rounded p-1 text-subtle hover:text-accent"
      >
        <Eye size={16} aria-hidden="true" />
      </button>
      {/* Mounted only while open, which is what keeps the role read off every
          page's render path -- the button itself asks the cluster nothing. */}
      {open ? <GuideDialog guide={guide} onClose={() => setOpen(false)} /> : null}
    </>
  );
}

function GuideDialog({
  guide,
  onClose,
}: {
  guide: GuideEntry;
  onClose: () => void;
}): ReactNode {
  const canAdminister = useCanAdminister();
  const technical = guide.technical;

  return (
    <Dialog open onClose={onClose} labelledBy="page-guide-title" size="xl">
      <div className="flex flex-col gap-5 p-5">
        <h2 id="page-guide-title" className="text-lg font-semibold tracking-tight">
          {guide.title}
        </h2>

        <div className="aspect-video w-full overflow-hidden rounded-lg border border-line bg-raised">
          {guide.videoRef === undefined ? (
            <div className="flex h-full flex-col items-center justify-center gap-3 text-center">
              <span className="text-accent opacity-70">
                <Constellation size="sm" />
              </span>
              <p className="text-xs text-subtle">Guide video coming soon</p>
            </div>
          ) : (
            <video className="h-full w-full" controls src={guide.videoRef} />
          )}
        </div>

        <section>
          <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">
            What you&rsquo;re looking at
          </h3>
          <p className="mt-2 max-w-prose text-sm text-fg">{guide.body}</p>
        </section>

        <section>
          <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">
            How it works
          </h3>
          <ul className="mt-2 flex max-w-prose list-disc flex-col gap-1.5 pl-5 text-sm text-muted">
            {guide.how.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
        </section>

        {canAdminister && technical !== undefined ? (
          <details className="rounded border border-line bg-raised px-3 py-2">
            <summary className="motion-wash cursor-pointer text-xs font-semibold tracking-wide text-muted uppercase hover:text-fg">
              Technical details
            </summary>
            <div className="mt-3 flex flex-col gap-3">
              <TechnicalList label="Concepts" items={technical.concepts} mono />
              <TechnicalList label="Environment" items={technical.env} mono />
              <TechnicalList label="Documentation" items={technical.docs} mono />
              <TechnicalList label="Notes" items={technical.notes} />
            </div>
          </details>
        ) : null}

        <div className="flex justify-end">
          <Button tone="quiet" onClick={onClose}>
            Close
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

function TechnicalList({
  label,
  items,
  mono = false,
}: {
  label: string;
  items?: readonly string[];
  mono?: boolean;
}): ReactNode {
  if (items === undefined || items.length === 0) return null;
  return (
    <div>
      <h4 className="text-xs font-medium text-subtle">{label}</h4>
      <ul className={"mt-1 flex flex-col gap-1 text-xs text-muted" + (mono ? " font-mono break-all" : "")}>
        {items.map((item) => (
          <li key={item}>{item}</li>
        ))}
      </ul>
    </div>
  );
}
