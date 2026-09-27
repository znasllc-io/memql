import type { ReactNode } from "react";
import { Dialog } from "../../../kit/Dialog";
import { usePaneActive } from "../paneActivity";

/** Native top-layer modal: the window beneath it stays inert, and browser
 * focus returns to the invoking piece. No activity event opens this dialog.
 *
 * The behaviour is the kit's `Dialog`, promoted from here on its second use
 * (epic memql#5414); this keeps Deployables' look and its pane activity. */
export function DetailDialog({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const active = usePaneActive();
  return (
    <Dialog
      title={title}
      onDismiss={onClose}
      active={active}
      className="deployable-dialog"
      bodyClassName="deployable-dialog-body"
      closeLabel={`Close ${title}`}
    >
      {children}
    </Dialog>
  );
}
