import { Check, Minus } from "lucide-react";

import { GRID_VERBS, cellIsHeld, cellLockReason, cellState, gridResources } from "./grid";

// THE PERMISSIONS GRID: the five verbs across, the seeded resource kinds down.
//
// ===========================================================================
// A DASH IS NOT AN UNCHECKED BOX
// ===========================================================================
// Three cell states, and the difference between the first two is the whole
// reason this is a grid rather than a list of checkboxes:
//
//   a check somebody may toggle    this role holds it, or could
//   a check somebody may not       shown, locked, with the reason as a title
//   a dash                         no seeded role gates this pair at all
//
// An unchecked box says "off", which is an answer. A dash says the question is
// not asked -- and a checkbox in its place would write a grant no resolver
// ever reads, so the person would see a permission that does nothing.
//
// The rows and the pairs come from `ROLE_GRID_VOCABULARY`, pinned to
// dsl/rbac/seeds.memql by component/memql/role_grid_os_parity_test.go.

export function Grid({
  held,
  callerHolds,
  editable,
  onToggle,
  label,
}: {
  /** `resource:verb` pairs this role holds. */
  held: readonly string[];
  /** `resource:verb` pairs the CALLER holds, which bounds what they may grant. */
  callerHolds: readonly string[];
  /** A custom role, and the caller holds update on role. */
  editable: boolean;
  onToggle?: (pair: string, next: boolean) => void;
  label: string;
}) {
  const heldSet = new Set(held);
  const callerSet = new Set(callerHolds);

  return (
    <table className="os-grid" aria-label={label}>
      <thead>
        <tr>
          {/* No caption over the first column: the rows name themselves, and a
              "Resource" header would be a label on a label. */}
          <th scope="col">
            <span className="os-visually-hidden">What the permission is about</span>
          </th>
          {GRID_VERBS.map((verb) => (
            <th key={verb.verb} scope="col">
              {verb.label}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {gridResources().map((resource) => (
          <tr key={resource.resource}>
            <th scope="row">{resource.label}</th>
            {GRID_VERBS.map((verb) => {
              const pair = `${resource.resource}:${verb.verb}`;
              const state = cellState({
                resource: resource.resource,
                verb: verb.verb,
                held: heldSet.has(pair),
                callerHolds: callerSet.has(pair),
                editable,
              });
              const on = cellIsHeld(state);
              const reason = cellLockReason(state, editable);
              const name = `${verb.label} ${resource.label}`;

              if (state === "absent") {
                return (
                  <td key={pair} className="os-grid-cell" data-state="absent">
                    <Minus size={12} aria-hidden />
                    <span className="os-visually-hidden">{name}: not a permission this cluster gates</span>
                  </td>
                );
              }
              return (
                <td key={pair} className="os-grid-cell" data-state={state}>
                  <button
                    type="button"
                    className="os-grid-mark"
                    role="checkbox"
                    aria-checked={on}
                    aria-label={name}
                    title={reason === "" ? undefined : reason}
                    disabled={reason !== ""}
                    onClick={() => onToggle?.(pair, !on)}
                  >
                    {on ? <Check size={12} aria-hidden /> : null}
                  </button>
                </td>
              );
            })}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
