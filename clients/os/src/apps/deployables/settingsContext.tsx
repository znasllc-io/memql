import { createContext, useContext, type ReactNode } from "react";

import { DEFAULT_DEPLOYABLES_SETTINGS, type DeployablesSettings } from "./settings";

// The app's settings, readable from anywhere inside it.
//
// WHY A CONTEXT AND NOT PROPS. Two of these preferences are read at the BOTTOM
// of the tree -- the traffic window in the Live stop's panel, four levels below
// the app -- and written from the top. Drilling them would put two props
// through every component in between that has no interest in either.
//
// WHY A CONTEXT AND NOT A SECOND STORE. The obvious alternative is for the
// panel to construct its own LocalDeployablesSettingsStore and read/write the
// same key. That is a read-modify-write race on one document: the app holds
// the settings in state, so a write from the panel would be invisible to it,
// and the app's next `update()` would spread its stale copy back over storage
// and silently undo the person's window. One owner, one writer.
//
// The default value is the DEFAULTS rather than null, so a component rendered
// outside the provider -- a test mounting one panel, say -- reads a coherent
// document instead of throwing.

export interface DeployablesSettingsBridge {
  settings: DeployablesSettings;
  update: (patch: Partial<DeployablesSettings>) => void;
  /**
   * Open or shut one source group.
   *
   * AN OPERATION RATHER THAN A COMPUTED PATCH, and the difference is not
   * stylistic. A caller that read `settings.expandedSources`, added its own id
   * and handed the finished array to `update` would be computing from the
   * document its RENDER closed over -- so two groups toggled in one tick each
   * build their array from the same starting point and the second write drops
   * the first. Making the change a function of the previous value is the only
   * shape that composes; a functional `update` alone does not fix it, because
   * by then the stale array is already the argument.
   */
  toggleSource: (id: string) => void;
}

const Ctx = createContext<DeployablesSettingsBridge>({
  settings: DEFAULT_DEPLOYABLES_SETTINGS,
  // A no-op rather than a throw: outside the provider there is nowhere to
  // save to, and a preference that cannot be remembered is not an error.
  update: () => {},
  toggleSource: () => {},
});

export function DeployablesSettingsProvider({
  value,
  children,
}: {
  value: DeployablesSettingsBridge;
  children: ReactNode;
}) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useDeployablesSettings(): DeployablesSettingsBridge {
  return useContext(Ctx);
}
