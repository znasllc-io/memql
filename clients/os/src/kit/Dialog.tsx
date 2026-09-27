import { useEffect, useId, useRef, type ReactNode } from "react";
import { X } from "lucide-react";

import { Button } from "./controls";

// Dialog -- the shell's native modal (kit).
//
// ===========================================================================
// PROMOTED ON ITS SECOND USE
// ===========================================================================
// It was `apps/deployables/page/DetailDialog`, which is now a thin wrapper
// over this. Nexus's composer ("Run again", "Branch from here") is the second
// surface that needs a modal holding a draft, and the rule in controls.tsx is
// that the second use promotes -- waiting for a third means the second has
// already forked. Fleet's share dialog, which predates this, carries the same
// guards written out by hand; it is the precedent for the floor.
//
// ===========================================================================
// WHAT THE PLATFORM ALREADY DOES, IT DOES
// ===========================================================================
// `showModal()` puts the dialog in the top layer, makes the window beneath it
// inert and contains focus, so there is no focus trap here. Focus goes back
// to the control that opened it on the way out -- however it was closed.
//
// ===========================================================================
// ESCAPE NEVER THROWS A DRAFT AWAY, AND NEVER WALKS AWAY FROM A WRITE
// ===========================================================================
// Escape and the platform's own close request both land on `onDismiss`, and
// the CALLER decides what dismissing means: the composer keeps its draft, so
// an Escape pressed by habit costs nobody the prompt they were rewriting.
// While `busy`, neither closes anything: the write's answer is either
// success -- which closes the dialog anyway -- or a refusal, and a refusal
// that arrived after the dialog had gone would have nowhere to be read.

export function Dialog({
  title,
  subtitle,
  onDismiss,
  busy = false,
  active = true,
  className = "os-dialog",
  bodyClassName = "os-dialog-body",
  closeLabel,
  children,
  floor,
  initialFocus,
}: {
  title: string;
  /** Which thing, under the act's own name -- a dialog that did not say would invite the change to the wrong one. */
  subtitle?: ReactNode;
  /** Escape, the platform's close request, or the close button. */
  onDismiss: () => void;
  /** A write is in flight: nothing closes until it answers. */
  busy?: boolean;
  /** False while the pane holding it is hidden: the dialog steps out of the top layer and comes back with it. */
  active?: boolean;
  className?: string;
  bodyClassName?: string;
  /** Draw a close button in the header, named this. A dialog with a floor carries its way out there instead. */
  closeLabel?: string;
  children: ReactNode;
  /** The floor, pinned below the scrolling body: the ActionBar with the one act. */
  floor?: ReactNode;
  /**
   * Where focus goes when it opens. The platform's own choice is the FIRST
   * focusable thing, which in a form can be a one-click act rather than the
   * first field -- and an Enter pressed by habit would then take it.
   */
  initialFocus?: (dialog: HTMLDialogElement) => HTMLElement | null;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const dismissRef = useRef(onDismiss);
  dismissRef.current = onDismiss;
  const busyRef = useRef(busy);
  busyRef.current = busy;
  const focusRef = useRef(initialFocus);
  focusRef.current = initialFocus;
  // Set while THIS component is closing the dialog, so the platform's queued
  // `close` event is not mistaken for a close somebody asked for.
  const closing = useRef(false);

  useEffect(() => {
    const dialog = ref.current;
    if (dialog === null || !active) return undefined;
    // Reset on every open: StrictMode runs this effect, its cleanup and the
    // effect again, and a flag left set by that cleanup would make a later
    // close by the platform look like one of ours.
    closing.current = false;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    // Feature-tested for environments that have a <dialog> element with no
    // behaviour behind it.
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.setAttribute("open", "");
    focusRef.current?.(dialog)?.focus();
    return () => {
      closing.current = true;
      if (dialog.hasAttribute("open")) {
        if (typeof dialog.close === "function") dialog.close();
        else dialog.removeAttribute("open");
      }
      if (opener?.isConnected) opener.focus({ preventScroll: true });
    };
  }, [active]);

  function dismiss(): void {
    if (busyRef.current) return;
    dismissRef.current();
  }

  return (
    <dialog
      ref={ref}
      className={className}
      aria-labelledby={titleId}
      data-busy={busy || undefined}
      onKeyDown={(event) => {
        if (event.key !== "Escape") return;
        // Handled here, so the platform's close request never fires and no
        // shell listener further up -- a window that closes on Escape -- hears it.
        event.preventDefault();
        event.stopPropagation();
        dismiss();
      }}
      onCancel={(event) => {
        // Any other close request (a back gesture): the same guard.
        event.preventDefault();
        dismiss();
      }}
      onClose={(event) => {
        // The platform closed it without asking. Keep the page's state in step
        // with what is on screen -- ONLY IF IT IS STILL CLOSED: the event is
        // queued, and the one from StrictMode's rehearsal cleanup lands after
        // the dialog has been opened again.
        if (!closing.current && !event.currentTarget.open) dismissRef.current();
      }}
    >
      <header>
        <div className="os-dialog-titles">
          <h3 id={titleId}>{title}</h3>
          {subtitle ? <p className="os-dialog-subtitle">{subtitle}</p> : null}
        </div>
        {closeLabel ? (
          <Button tone="quiet" ariaLabel={closeLabel} onClick={dismiss}>
            <X size={16} aria-hidden />
          </Button>
        ) : null}
      </header>
      <div className={bodyClassName}>{children}</div>
      {floor}
    </dialog>
  );
}
