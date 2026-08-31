import { useEffect, useState } from "react";

import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { readStoredTheme, setTheme, type ThemeChoice } from "../../app/theme";
import { themePacks } from "../../styles/themes";
import type { OsAppProps } from "../../system/registry";
import { AppsIndexSection } from "./AppsIndexSection";
import { ClusterSection } from "./ClusterSection";
import { DiagnosticsSection } from "./DiagnosticsSection";
import { ConnectionHistoryProvider } from "./useConnectionHistory";

// Settings (spec D12): the app that PROVES the sections pattern, and since
// the settings-app epic (memql#4741) the cluster's control room -- About,
// Appearance, the Apps index, admin-gated Cluster facts, and Diagnostics.
// Every app's title-bar gear deep-links to a section in its own manifest;
// this app is the reference, and its Apps section is the directory.
//
// Everything here is READ-ONLY against the engine. The one write the epic
// introduces is the theme-pack choice, which goes to DesktopStore and never
// to the cluster.

export function SettingsApp({ sectionId }: OsAppProps) {
  // The history provider wraps the WHOLE app, not the Diagnostics section:
  // the buffer has to cover the window's lifetime, and a person navigating
  // to Diagnostics because they saw the dot change is the case it is for.
  return <ConnectionHistoryProvider>{sectionFor(sectionId)}</ConnectionHistoryProvider>;
}

function sectionFor(sectionId: string) {
  if (sectionId === "appearance") return <AppearanceSection />;
  if (sectionId === "apps") return <AppsIndexSection />;
  if (sectionId === "cluster") return <ClusterSection />;
  if (sectionId === "diagnostics") return <DiagnosticsSection />;
  return <AboutSection />;
}

function AboutSection() {
  const { access, config } = useSession();
  return (
    <div className="os-settings">
      <h3 className="os-settings-title">About this OS</h3>
      <dl className="os-facts">
        <dt>Cluster</dt>
        <dd className="os-mono">{config.domain || "unknown"}</dd>
        <dt>Signed in as</dt>
        <dd>{access?.primaryEmail || "unknown"}</dd>
        <dt>Cluster role</dt>
        <dd className="os-mono">{access?.clusterRole || "unknown"}</dd>
        <dt>Shell</dt>
        <dd>MemQL OS</dd>
        <dt>Shell build</dt>
        <dd className="os-mono">{__OS_BUILD__}</dd>
      </dl>
      <p className="os-caption">
        Cluster versions and identity facts are in Cluster; connection and
        permission facts are in Diagnostics.
      </p>
    </div>
  );
}

function AppearanceSection() {
  const { state, actions } = useOs();
  const [mode, setMode] = useState<ThemeChoice>(() => readStoredTheme());

  useEffect(() => {
    setTheme(mode);
  }, [mode]);

  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Appearance</h3>
      <fieldset className="os-field-group">
        <legend>Mode</legend>
        <div className="os-choice-row" role="radiogroup" aria-label="Mode">
          {(["light", "dark", "system"] as const).map((choice) => (
            <button
              key={choice}
              type="button"
              role="radio"
              aria-checked={mode === choice}
              className="os-choice"
              onClick={() => setMode(choice)}
            >
              {choice === "light" ? "Light" : choice === "dark" ? "Dark" : "System"}
            </button>
          ))}
        </div>
      </fieldset>
      <fieldset className="os-field-group">
        <legend>Theme</legend>
        <div className="os-choice-row" role="radiogroup" aria-label="Theme">
          {themePacks().map((pack) => (
            <button
              key={pack.id}
              type="button"
              role="radio"
              aria-checked={state.themePack === pack.id}
              className="os-choice"
              onClick={() => actions.setThemePack(pack.id)}
            >
              {pack.tokensHref ? pack.name : `${pack.name} (built in)`}
            </button>
          ))}
        </div>
        <p className="os-caption">
          More themes -- marketplace coming soon.
        </p>
      </fieldset>
    </div>
  );
}
