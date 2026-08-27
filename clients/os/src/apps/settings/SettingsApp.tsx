import { useEffect, useState } from "react";

import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { readStoredTheme, setTheme, type ThemeChoice } from "../../app/theme";
import type { OsAppProps } from "../../system/registry";

// Settings (spec D12): the app that PROVES the sections pattern. About and
// Appearance are real; Cluster is the admin-gated stub the settings-app
// epic (#4741) fills. Every app's title-bar gear deep-links here-shaped
// sections in its own manifest -- this app is the reference.

export function SettingsApp({ sectionId }: OsAppProps) {
  if (sectionId === "appearance") return <AppearanceSection />;
  if (sectionId === "cluster") return <ClusterSection />;
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
        <dd>MemQL OS foundation (epic #4710)</dd>
      </dl>
      <p className="os-caption">
        Versions, identity facts and diagnostics arrive with epic #4741.
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
          <button
            type="button"
            role="radio"
            aria-checked={state.themePack === "graphite"}
            className="os-choice"
            onClick={() => actions.setThemePack("graphite")}
          >
            Graphite (built in)
          </button>
        </div>
        <p className="os-caption">The theme marketplace arrives with epic #4745.</p>
      </fieldset>
    </div>
  );
}

function ClusterSection() {
  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Cluster</h3>
      <p className="os-stub-summary">
        Domain and front-door facts, engine and bundle versions, provider and
        mail status -- read-only, no secrets.
      </p>
      <p className="os-caption">Arrives with epic #4741. Visible to admins and owners only.</p>
    </div>
  );
}
