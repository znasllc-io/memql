import { createContext, useContext, type ReactNode } from "react";
import { createPortal } from "react-dom";

export const WizardStepHeaderContext = createContext<HTMLElement | null>(null);

/** A step owns its live controls; the wizard owns their visible title row.
 * Portaling keeps the same hook state and handlers in wide and narrow layouts. */
export function WizardStepHeader({ count, children }: { count?: number; children?: ReactNode }) {
  const target = useContext(WizardStepHeaderContext);
  if (!target) return null;
  return createPortal(<>
    {count === undefined ? null : <span className="os-head-meta">{count}</span>}
    {children ? <span className="os-head-actions">{children}</span> : null}
  </>, target);
}
