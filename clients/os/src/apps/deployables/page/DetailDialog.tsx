import { useEffect, useId, useRef, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "../../../kit";
import { usePaneActive } from "../paneActivity";

/** Native top-layer modal: the window beneath it stays inert, and browser
 * focus returns to the invoking piece. No activity event opens this dialog. */
export function DetailDialog({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const active = usePaneActive();
  const ref = useRef<HTMLDialogElement>(null);
  const id = useId();
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const dialog = ref.current;
    if (!dialog || !active) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (dialog.showModal) dialog.showModal();
    else dialog.setAttribute("open", "");
    return () => {
      if (dialog.open) {
        if (dialog.close) dialog.close();
        else dialog.removeAttribute("open");
      }
      if (opener?.isConnected) opener.focus({ preventScroll: true });
    };
  }, [active]);
  return <dialog ref={ref} className="deployable-dialog" aria-labelledby={id} onCancel={(event) => {
    event.preventDefault(); closeRef.current();
  }}>
    <header><h3 id={id}>{title}</h3><Button tone="quiet" ariaLabel={`Close ${title}`} onClick={onClose}><X size={16} aria-hidden /></Button></header>
    <div className="deployable-dialog-body">{children}</div>
  </dialog>;
}
