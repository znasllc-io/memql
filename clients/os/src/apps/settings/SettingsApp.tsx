import { useEffect, useState } from "react";

import { useAsk } from "../../ask/AskProvider";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { readStoredTheme, setTheme, type ThemeChoice } from "../../app/theme";
import { themePacks } from "../../themes/registry";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { AppsIndexSection } from "./AppsIndexSection";
import { ClusterSection } from "./ClusterSection";
import { DiagnosticsSection } from "./DiagnosticsSection";
import { IntegrationsSection } from "./IntegrationsSection";
import { ConnectionHistoryProvider } from "./useConnectionHistory";

// Settings (spec D12): the app that PROVES the sections pattern, and since
// the settings-app epic (memql#4741) the cluster's control room -- About,
// Appearance, the Apps index, admin-gated Cluster facts, and Diagnostics.
// Every app's title-bar gear deep-links to a section in its own manifest;
// this app is the reference, and its Apps section is the directory.
//
// Everything here is READ-ONLY against the engine. The one write the
// settings-app epic introduced is the theme-pack choice, which goes to
// DesktopStore and never to the cluster; Integrations (issue #4826) reads the
// node's integration registry and states in surface why it does not yet
// save.

export function SettingsApp({ sectionId, intent, consumeIntent }: OsAppProps) {
  // The history provider wraps the WHOLE app, not the Diagnostics section:
  // the buffer has to cover the window's lifetime, and a person navigating
  // to Diagnostics because they saw the dot change is the case it is for.
  return (
    <ConnectionHistoryProvider>
      {sectionFor(sectionId, intent, consumeIntent)}
    </ConnectionHistoryProvider>
  );
}

function sectionFor(sectionId: string, intent: OsAppProps["intent"], consumeIntent: OsAppProps["consumeIntent"]) {
  if (sectionId === "appearance") return <AppearanceSection />;
  if (sectionId === "ask") return <AskSection />;
  if (sectionId === "apps") return <AppsIndexSection />;
  if (sectionId === "cluster") return <ClusterSection />;
  if (sectionId === "diagnostics") return <DiagnosticsSection />;
  if (sectionId === "integrations") return <IntegrationsSection />;
  // No owned concepts: the shell's own lines are tagged with no app, and
  // this app's slice is what its surfaces logged under "settings".
  if (sectionId === "logs") return <AppLogsSection app="settings" intent={intent} consumeIntent={consumeIntent} />;
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

/**
 * Ask, the section beside Appearance (epic memql#4747).
 *
 * Two settings, and the list is short on purpose. Both describe how the SHELL
 * behaves; neither duplicates something the browser already offers.
 *
 * There is no microphone PICKER, and that is the deliberate cut: Chrome and
 * Safari both expose a per-site input-device choice in the address bar, and a
 * second one here would be a control that disagrees with the browser's own
 * half the time. There is no input LANGUAGE either -- see askSettings.ts: the
 * engine pins it cluster-wide and discards what a client sends, so the
 * control would change nothing and blame the reader for the result.
 */
function AskSection() {
  const { settings, updateSettings } = useAsk();
  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Ask</h3>
      <fieldset className="os-field-group">
        <legend>When you stop talking</legend>
        <div className="os-choice-row" role="radiogroup" aria-label="When you stop talking">
          {(
            [
              ["send", "Send it"],
              ["review", "Put it in the box"],
            ] as const
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              role="radio"
              aria-checked={settings.commit === value}
              className="os-choice"
              onClick={() => updateSettings({ commit: value })}
            >
              {label}
            </button>
          ))}
        </div>
        <p className="os-caption">
          The transcript appears in the box while you speak either way, so you
          have read it before you let go.
        </p>
      </fieldset>
      <fieldset className="os-field-group">
        <legend>Keyboard</legend>
        <label className="os-check">
          <input
            type="checkbox"
            checked={settings.spaceToTalk}
            onChange={(event) => updateSettings({ spaceToTalk: event.target.checked })}
          />
          <span>Hold Space to talk</span>
        </label>
        <p className="os-caption">
          Works in the Ask panel when the caret is not in a text box -- Space
          still types a space wherever you are typing. The mic button always
          works: hold it to talk, tap it to keep listening.
        </p>
      </fieldset>
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
          {themePacks(state.installedPacks).map((pack) => (
            <button
              key={pack.id}
              type="button"
              role="radio"
              aria-checked={state.themePack === pack.id}
              className="os-choice"
              onClick={() => actions.setThemePack(pack.id)}
            >
              {pack.builtIn ? `${pack.name} (built in)` : pack.name}
            </button>
          ))}
        </div>
        {/* A LIST, NOT A STORE. This is the fast way to change back to a
            theme you already have, from a window you were already in. It
            deliberately does not preview: previewing means restyling the
            desktop behind the panel, and this panel is a window ON that
            desktop -- it would be previewing itself. That is the marketplace
            drawer's job, and the sentence below says where it is rather than
            leaving an absent affordance to look like an oversight. */}
        <p className="os-caption">
          Themes in the Launcher lets you try one on before you keep it, and is
          where you add your own.
        </p>
      </fieldset>
    </div>
  );
}
