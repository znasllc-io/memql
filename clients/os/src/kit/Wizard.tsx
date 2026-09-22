import { useEffect, useRef, type ReactNode } from "react";

import { ActionBar, type Act, type ActionBarTone } from "./ActionBar";
import type { Breadcrumb } from "./Breadcrumbs";
import { Head } from "./controls";
import { Rail, stopIsReachable, type Stop } from "./Rail";
import { useWide } from "./useWide";

// THE WIZARD -- one way to ADD a thing, whatever the thing is.
//
// ===========================================================================
// WHY IT EXISTS
// ===========================================================================
// Pressing + ran three different flows. A deployable opened a horizontal
// numbered stepper with its form indented beneath it; a machine opened a
// second horizontal stepper, numbered differently, inside a bordered card;
// a domain opened one text field with its own button, and the setup that
// followed lived on another page. Three left edges, three step languages, and
// the forward act in three different places.
//
// This is the first-run gate's composition -- the orb, the title, one sentence,
// then a column of stops -- because that was the one guided surface in the
// shell that read as a single thought. What an ADD needs beyond it is ORDER
// (no command without a token, no records without a binding) and a FLOOR.
//
// ===========================================================================
// THE FLOOR SAYS WHOSE TURN IT IS
// ===========================================================================
// Every step here belongs to one of two parties. The person answers, runs a
// command, creates a DNS record; the cluster mints, listens, checks, issues.
// The bar on the window's bottom edge (`ActionBar`, rule 12) states which, in
// the same place on every wizard: the state in words on the left, and on the
// right the way out followed by the ONE forward act, primary last.
//
//   YOUR TURN        the forward act is there. If it is not there yet, the
//                    words on the left say what is still missing -- an act
//                    that is not legal is ABSENT, never disabled.
//   THE CLUSTER'S    there is no forward act, because there is nothing for a
//                    person to press. The bar's top edge becomes a moving
//                    thread, the dot becomes a spinner, and `meta` measures
//                    the wait. A wait somebody can miss is a page that looks
//                    finished or broken, and they leave either way.
//
// THE FORWARD ACT LIVES NOWHERE ELSE. A step's body holds what is being
// answered -- fields, a command to copy, records to create -- and never the
// button that moves on, so nobody has to find it twice.
//
// ===========================================================================
// STEPS ARE THE RAIL, AT PAGE SCALE
// ===========================================================================
// The same `Rail` every other surface draws, with the same closed state set:
// `done` is a check, `open` is a held ring (waiting on you), `current` is the
// pulse (the cluster is working), `ahead` dims. Every step is one line --
// name, then its answer -- so a person can read back what they have said, in
// order, without leaving the page; the open one carries its body beneath its
// line, and the connector runs down the body's left edge, which is what holds
// the body to its step without a box around it.
//
// A reachable step reopens on click, to be READ again. Whether it can still be
// CHANGED is the flow's to say: a name that has been minted into a token is a
// fact, and its step draws the fact.
//
// ===========================================================================
// IT TAKES THE WINDOW (rule 9: real estate belongs to content)
// ===========================================================================
// The first version was the gate's centred 38rem column, and in a wide window
// it painted a third of the pane and left the rest empty -- while the one thing
// on the page that WANTS width, a DNS record's value, wrapped onto two lines
// inside it. A list page in the same window runs edge to edge.
//
// So, given room, the wizard is TWO PANES on the list page's own gutters:
//
//   THE STEPS    on the left, still a vertical rail, and now standing still:
//                a body that opens no longer pushes the steps beneath it down
//                the page, so the rail is a map a person can keep an eye on.
//   THE STAGE    the open step, with the rest of the width. Its name and what
//                it asks head the pane; records and tables take the room,
//                prose keeps a readable measure and a field keeps a field's
//                width -- width is for what needs it, not for stretching a
//                text box across a monitor.
//
// Without room (a small window, a phone) it is the single column it was, with
// the open step's body beneath its own line. That is a different PLACE in the
// tree, not a different style, which is why the choice is measured
// (`useWide`) rather than left to a container query.
//
// ===========================================================================
// ADDING ONLY
// ===========================================================================
// A wizard creates. Once the thing exists it has a page of its own, and that
// page is where it is read and changed -- by whoever is allowed to. Nothing
// here edits.

export interface WizardStatus {
  /** The state, in the words a person uses: "Name the domain", "Waiting for DNS". */
  word: string;
  /** What that state means or still needs, in one clause. */
  detail?: string;
  /** `busy` is the cluster's turn, and is what makes the wait visible. */
  tone?: ActionBarTone;
  /** The measure of a wait -- "0:42", "checked 40s ago". */
  meta?: string;
}

export interface WizardProps {
  /** What is being added, drawn in the orb. The first-run gate wears the MemQL mark; an add wears its subject. */
  icon: ReactNode;
  /** Optional companion mark, in a matching orb immediately before the subject. */
  leadingIcon?: ReactNode;
  /** The same words as the control that opened this: "Add a domain". */
  title: string;
  /** One sentence on what adding this does. Optional, and never a second title. */
  lead?: string;
  /** Published to the window's one trail row. */
  breadcrumbs?: readonly Breadcrumb[];
  back?: { label: string; onSelect: () => void };
  /** The step list's accessible name. */
  label: string;
  steps: readonly Stop[];
  /** The step whose body is drawn. */
  open: string;
  onOpen: (stepId: string) => void;
  status: WizardStatus;
  /** The way out first and quiet; the forward act last and primary. At most three. */
  acts?: readonly Act[];
  /** A question that replaces the acts' prose -- "Leave?" -- drawn in the bar. */
  confirm?: ReactNode;
  /** Refusals and warnings that belong to the whole flow rather than to a step. */
  notices?: ReactNode;
  /** Drawn beneath the steps: the one line a flow has to say that no step owns. */
  children?: ReactNode;
  /** Where the person is, for Ask. */
  context?: Record<string, unknown>;
  className?: string;
  /**
   * The arrangement, when it must not be measured: a test has no layout to
   * measure. Left alone it is two panes from `SPLIT_AT` pixels up.
   */
  layout?: "split" | "stack";
}

/** The width from which the steps and the stage stand side by side. */
export const SPLIT_AT = 760;

export function Wizard({
  icon,
  leadingIcon,
  title,
  lead,
  breadcrumbs,
  back,
  label,
  steps,
  open,
  onOpen,
  status,
  acts = [],
  confirm,
  notices,
  children,
  context,
  className,
  layout,
}: WizardProps) {
  const column = useRef<HTMLElement>(null);
  const pane = useRef<HTMLDivElement>(null);
  const measuredWide = useWide(pane, SPLIT_AT);
  const split = layout === undefined ? measuredWide : layout === "split";

  // FOLLOW THE FLOW. When a step opens because something HAPPENED -- a token
  // was minted, a machine registered -- it may open below the fold of the step
  // that just closed above it. `nearest`, so a step already in view does not
  // move under somebody's cursor.
  //
  // NEVER ON THE WAY IN. A wizard opens at its top -- the orb, the title and
  // the sentence that says what this is -- and a first step taller than the
  // window would otherwise be "brought into view" straight past all three.
  const opened = useRef(false);
  useEffect(() => {
    if (!opened.current) {
      opened.current = true;
      return;
    }
    if (open === "") return;
    // Side by side the rail does not move, so it is the STAGE that starts again
    // at its top; in one column it is the step's own line that may be out of view.
    const target = column.current?.querySelector<HTMLElement>('.os-wizard-stage') ?? column.current?.querySelector<HTMLElement>('.os-rail-stage[data-open="true"]');
    target?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [open]);

  // THE FIRST QUESTION TAKES THE CURSOR. Somebody pressed Add and the next
  // thing they do is type a name; making them click the one field on the page
  // first is a step the wizard added. Only a text field, and only ON THE WAY
  // IN -- a step that opens because the flow moved on must not pull focus out
  // from under whatever the person is doing.
  //
  // "ON THE WAY IN" IS UNTIL THEY TOUCH IT, NOT "ON MOUNT". The arrangement is
  // measured after the first commit, and going side by side moves the open
  // step's body to another place in the tree -- so the field that was focused
  // on mount is unmounted a moment later and the cursor went with it. Found by
  // typing into a freshly opened wizard in a wide window and watching nothing
  // land. So it is done again for the arrangement that settles, and never once
  // a key or a pointer has said the person is here.
  const touched = useRef(false);
  useEffect(() => {
    if (touched.current) return;
    column.current
      ?.querySelector<HTMLInputElement>('.os-wizard-body input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]):not(:disabled)')
      ?.focus({ preventScroll: true });
  }, [split]);

  const opened_ = steps.find((step) => step.id === open);
  const drawn: Stop[] = steps.map((step) => ({
    ...step,
    // A STEP NOBODY HAS REACHED IS ITS NAME. What it will ask is said when it
    // opens; said in advance, four times over, it is a paragraph of grey text
    // between the person and the one step they can answer.
    //
    // SIDE BY SIDE, what the open step asks heads the stage instead, so the
    // rail's line is the name and the answer and nothing is said twice.
    sentence: split ? undefined : stopIsReachable(step.state) ? step.sentence : undefined,
    // ...and the body is the stage's, not the line's.
    body: split || step.body === undefined || step.body === null ? undefined : <div className="os-wizard-body">{step.body}</div>,
  }));

  const rail = (
    <Rail
      scale="page"
      stops={drawn}
      label={label}
      openStop={open}
      // ONE STEP IS ALWAYS OPEN. The rail's own grammar closes the open
      // stop on a second click, which is right for a record somebody is
      // browsing and wrong here: a wizard with nothing open is a list of
      // names and a button, and the button would act on a form nobody
      // can see.
      onOpenStop={(id) => {
        if (id !== "") onOpen(id);
      }}
    />
  );

  return (
    <div
      ref={pane}
      className={className ? `os-deploy-pane os-wizard ${className}` : "os-deploy-pane os-wizard"}
      data-layout={split ? "split" : "stack"}
      data-os-page-context={context ? JSON.stringify(context) : undefined}
    >
      <div className="os-deploy-scroll os-wizard-scroll">
        <section
          ref={column}
          className="os-wizard-column"
          aria-label={title}
          onPointerDownCapture={() => { touched.current = true; }}
          onKeyDownCapture={() => { touched.current = true; }}
        >
          {split ? (
            <>
              <div className="os-wizard-aside">
                <div className="os-wizard-marks" aria-hidden>
                  {leadingIcon ? <span className="os-wizard-mark">{leadingIcon}</span> : null}
                  <span className="os-wizard-mark">{icon}</span>
                </div>
                <Head title={title} breadcrumbs={breadcrumbs} back={back} />
                {lead ? <p className="os-wizard-lead">{lead}</p> : null}
                {steps.length > 0 ? rail : null}
              </div>
              <div className="os-wizard-stage">
                {notices}
                {opened_ === undefined ? null : (
                  <>
                    {/* NAMED AGAIN, ON PURPOSE. The rail says which step is open;
                        a pane of fields with nothing over it says nothing about
                        what they are for, and the rail may be a screen away. */}
                    <h4 className="os-wizard-stage-title">{opened_.name}</h4>
                    {opened_.sentence ? <p className="os-wizard-stage-lead">{opened_.sentence}</p> : null}
                    {opened_.body === undefined || opened_.body === null ? null : <div className="os-wizard-body">{opened_.body}</div>}
                  </>
                )}
                {children}
              </div>
            </>
          ) : (
            <>
              <div className="os-wizard-marks" aria-hidden>
                {leadingIcon ? <span className="os-wizard-mark">{leadingIcon}</span> : null}
                <span className="os-wizard-mark">{icon}</span>
              </div>
              <Head title={title} breadcrumbs={breadcrumbs} back={back} />
              {lead ? <p className="os-wizard-lead">{lead}</p> : null}
              {notices}
              {rail}
              {children}
            </>
          )}
        </section>
      </div>

      <ActionBar state={status.word} detail={status.detail} tone={status.tone} meta={status.meta} live acts={acts}>
        {confirm}
      </ActionBar>
    </div>
  );
}
