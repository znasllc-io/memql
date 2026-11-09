import { Select } from "../../kit";
import type { ClientScope } from "./clientScope";

/**
 * The client picker beside a statistics page's PeriodControl: the kit's quiet
 * select, labelled "Client". Nothing at all when there is no choice to offer
 * (spec D13), so a member of one organization never sees a control, nor the
 * name of any other organization.
 */
export function ScopeControl({ id, scope }: { id: string; scope: ClientScope }) {
  if (!scope.showControl) return null;
  return (
    <Select tone="quiet" id={id} label="Client" title="Client" value={scope.value} onChange={scope.set}>
      {scope.options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </Select>
  );
}
