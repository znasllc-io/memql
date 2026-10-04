import { act, render } from "@testing-library/react";

import { MachinesProvider } from "../../src/live/machines";
import { RoutingSection } from "../../src/apps/fleet/routing/RoutingSection";
import { installSeededAccess } from "../seededAccess";
import { withSession } from "./harness";

// Mounting Fleet > Routing for the suite. The connection double and its
// fixtures are routingFixtures.ts, shared with the browser QA harness.
export * from "./routingFixtures";

export async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

/** Fleet > Routing as FleetApp mounts it, as a role. */
export async function renderRouting(role = "owner", props: Partial<Parameters<typeof RoutingSection>[0]> = {}) {
  installSeededAccess(role);
  const view = render(withSession(<MachinesProvider><RoutingSection {...props} /></MachinesProvider>));
  await settle();
  return view;
}
