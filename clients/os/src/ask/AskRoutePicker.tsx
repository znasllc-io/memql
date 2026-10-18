import { useId } from "react";
import { Route } from "lucide-react";

import { Select } from "../kit/controls";
import { pickerValue, type AskRouteOption } from "./askRoute";

/** The picker's own entry for "take me to where routes are edited". */
export const MANAGE_ROUTES = "manage:routes";

/**
 * The message field's route menu: Auto, Local, Apps, then the person's own
 * under "Yours", then Manage routes.
 *
 * A route that is not ready is SHOWN and cannot be chosen, with its reason
 * under its name. Hiding it would leave somebody looking for the Apps they
 * know exists; offering it would send a message the cluster refuses.
 *
 * Manage routes is an entry rather than a separate link because the menu is
 * where somebody is when they find the route they want is missing; it never
 * becomes the value.
 */
export function AskRoutePicker({ value, routes, onChange, onOpen, onManage }: {
  /** The chosen route id; "" is Auto. */
  value: string;
  routes: readonly AskRouteOption[];
  onChange: (route: AskRouteOption) => void;
  /** The menu is about to open: a moment to read readiness again. */
  onOpen?: () => void;
  onManage?: () => void;
}) {
  const id = useId();
  const cluster = routes.filter(route => route.scope === "cluster");
  const mine = routes.filter(route => route.scope === "personal");
  // A choice the list no longer holds (a route deleted elsewhere) still
  // shows by name rather than as an id, and is not offered again.
  const current = pickerValue(value, routes);
  return (
    <span className="os-ask-route" onPointerDownCapture={onOpen} onKeyDownCapture={event => { if (event.key === "Enter" || event.key === " " || event.key === "ArrowDown") onOpen?.(); }}>
      <Select
        id={id}
        label="Route"
        tone="quiet"
        icon={<Route size={13} />}
        title="Route: which models answer"
        value={current}
        onChange={next => {
          if (next === MANAGE_ROUTES) { onManage?.(); return; }
          const route = routes.find(item => item.id === next);
          if (route && route.ready) onChange(route);
        }}
      >
        {cluster.map(route => (
          <option key={route.id} value={route.id} disabled={!route.ready} data-hint={route.reason || undefined}>{route.name}</option>
        ))}
        {mine.length > 0 ? (
          <optgroup label="Yours">
            {mine.map(route => (
              <option key={route.id} value={route.id} disabled={!route.ready} data-hint={route.reason || undefined}>{route.name}</option>
            ))}
          </optgroup>
        ) : null}
        {onManage ? <option value={MANAGE_ROUTES} data-separated>Manage routes</option> : null}
      </Select>
    </span>
  );
}
