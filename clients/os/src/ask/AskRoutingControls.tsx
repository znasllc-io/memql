import { MapPin } from "lucide-react";

import { ExecutionDestinationSelect, type ExecutionDestination } from "../kit/ExecutionDestination";
import { AskRoutePicker } from "./AskRoutePicker";
import { routeSupportsDestination, type AskRouteOption } from "./askRoute";

/** Shared by every Ask surface. Each control changes only its own choice. */
export function AskRoutingControls({ route = "", destination = "auto", routes, onRoute, onDestination, onOpen, onManage }: {
  route?: string;
  destination?: ExecutionDestination;
  routes: readonly AskRouteOption[];
  onRoute: (route: string) => void;
  onDestination: (destination: ExecutionDestination) => void;
  onOpen?: () => void;
  onManage?: () => void;
}) {
  return <>
    <ExecutionDestinationSelect
      tone="quiet" label="Where" icon={<MapPin size={13} />} title="Where the work runs"
      value={destination} onChange={onDestination}
      options={{
        auto: { hint: "Chooses where work runs, within your permissions" },
        workbench: { disabled: !routeSupportsDestination(route, "workbench"), hint: "Isolated workspace; unavailable with Apps" },
        computer: { hint: "Runs on your computer" },
      }}
    />
    <AskRoutePicker
      value={route} destination={destination} routes={routes} onOpen={onOpen} onManage={onManage}
      onChange={next => onRoute(next.kind === "auto" ? "" : next.id)}
    />
  </>;
}
