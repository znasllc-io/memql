import { useId, useRef, type ReactNode } from "react";
import { Info, X } from "lucide-react";

/** Supporting context without hiding the consequence beside an action.
 * Native modal behavior supplies focus containment, Escape and focus return. */
export function InfoDetail({ title, children }: { title: string; children: ReactNode }) {
  const id = useId();
  const dialog = useRef<HTMLDialogElement>(null);
  return (
    <span className="os-info-detail">
      <button type="button" className="os-icon-button" aria-label={`About ${title}`} aria-haspopup="dialog" onClick={() => dialog.current?.showModal()}>
        <Info size={14} aria-hidden />
      </button>
      <dialog ref={dialog} className="os-info-dialog" aria-labelledby={id} onClick={(event) => {
        if (event.target === event.currentTarget) dialog.current?.close();
      }}>
        <div className="os-info-dialog-body">
          <header><h3 id={id}>{title}</h3><button type="button" className="os-icon-button" aria-label={`Close ${title}`} onClick={() => dialog.current?.close()}><X size={16} aria-hidden /></button></header>
          <div>{children}</div>
        </div>
      </dialog>
    </span>
  );
}
