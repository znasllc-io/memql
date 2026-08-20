import { useEffect, useRef, type ReactNode } from "react";

import { Button, type ButtonTone } from "./Button";

// The confirm layer. The portal had zero modals; destructive actions like
// revoke confirmed inline, which buries "are you sure" in the same visual
// plane as the thing it guards. Built on the native <dialog> element:
// showModal() gives focus trapping, Escape, ::backdrop and inert-behind for
// free, which is more accessibility than a div re-implementation earns.
//
// jsdom implements HTMLDialogElement but not every environment does; the
// open-attribute fallback keeps tests honest without a mock.

export function Dialog({
  open,
  onClose,
  labelledBy,
  children,
}: {
  open: boolean;
  // Fired for every close path: Escape, backdrop, and programmatic.
  onClose: () => void;
  labelledBy: string;
  children: ReactNode;
}): ReactNode {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) {
      if (typeof el.showModal === "function") el.showModal();
      else el.setAttribute("open", "");
    } else if (!open && el.open) {
      if (typeof el.close === "function") el.close();
      else el.removeAttribute("open");
    }
  }, [open]);

  if (!open) return null;

  return (
    <dialog
      ref={ref}
      aria-labelledby={labelledBy}
      onClose={onClose}
      onClick={(event) => {
        // A click on the backdrop lands on the <dialog> element itself; a
        // click inside lands on a descendant. Backdrop dismisses -- the
        // dialog's own confirm/cancel controls are the deliberate paths.
        if (event.target === event.currentTarget) onClose();
      }}
      className="m-auto w-full max-w-md rounded-lg border border-line bg-surface p-0 text-fg backdrop:bg-bg/60 backdrop:backdrop-blur-sm"
    >
      {children}
    </dialog>
  );
}

// ConfirmDialog is the one shape almost every dialog in a console is: a
// title, a body that states exactly what will happen, and two buttons whose
// labels are the action's own verbs -- never "OK".
export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel,
  cancelLabel = "Cancel",
  tone = "primary",
  busy = false,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  // The consequence, stated plainly. This is the reader's last chance to
  // learn what the button does; write it as facts, not reassurance.
  children: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  tone?: ButtonTone;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}): ReactNode {
  return (
    <Dialog open={open} onClose={onCancel} labelledBy="confirm-dialog-title">
      <div className="flex flex-col gap-3 p-5">
        <h2 id="confirm-dialog-title" className="text-base font-semibold">
          {title}
        </h2>
        <div className="text-sm text-muted">{children}</div>
        <div className="mt-2 flex justify-end gap-2">
          <Button tone="quiet" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </Button>
          <Button tone={tone} onClick={onConfirm} busy={busy}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
