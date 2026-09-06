import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { Head, Panel } from "../../kit";
import { AgentsSection } from "./agents/AgentsSection";
import { AuditSection } from "./audit/AuditSection";
import { ModulesSection } from "./modules/ModulesSection";
import { OriginsSection } from "./origins/OriginsSection";
import { ReadinessSection } from "./readiness/ReadinessSection";
import {
  CLUSTER_SECTIONS,
  DEFAULT_CLUSTER_SETTINGS,
  LocalClusterSettingsStore,
  type ClusterSettings,
  type ClusterSettingsStore,
} from "./settings";

// Cluster: what this cluster is made of, and how it is going.
//
// Readiness answers "can this thing do work at all"; Modules answers "what is
// it made of"; Data origins answers "what does it own and what does it
// mirror"; Agents answers "what does it run"; the Audit trail answers "what
// has been decided here". None of them is a wizard and none of them blocks
// anything -- the portal's first-run gate is deliberately not rebuilt here
// (see readiness/ReadinessSection.tsx).
//
// Sections are the app's own navigation. It never opens a window.

/** The concepts this app owns, for its Logs section: a line about the data
 *  origins, a connector's health, an outbox entry, an agent, a grant or an
 *  audit decision is this app's line. */
const CLUSTER_LOG_CONCEPTS = [
  Concepts.PLATFORM_DATA_ORIGIN,
  Concepts.PLATFORM_SYNC_STATE,
  Concepts.PLATFORM_OUTBOX_ENTRY,
  Concepts.AGENTS_AGENT,
  Concepts.AGENTS_AGENT_AUTHORIZATION,
  Concepts.IDENTITY_AUDIT_EVENT,
] as const;

export function ClusterApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: ClusterSettingsStore }) {
  // The store is injectable for tests, which is the whole reason the
  // parameter exists -- nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalClusterSettingsStore(), [store]);
  const [settings, setSettings] = useState<ClusterSettings>(() => settingsStore.load());

  function update(patch: Partial<ClusterSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW.
  //
  // The shell opens an app on its manifest's FIRST section: a window carries
  // no section until something navigates it, and WindowFrame resolves that to
  // sections[0]. So an app-level "open me here" preference can only be the
  // app navigating itself, immediately, on the first render of this
  // component instance.
  //
  // Once per WINDOW is exactly right, and the ref is what makes it so: this
  // component stays mounted while the section changes (only its props do), so
  // the guard fires on open and never again -- an operator who then clicks
  // Readiness is not dragged back to their default.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on
    // a named section -- the Settings apps index deep-linking to this app's
    // own settings, say -- was opened by somebody who said where they wanted
    // to be, and a preference that overrode that would make the deep-link
    // silently not work (memql#4743).
    const shellDefault = CLUSTER_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // AN EMPTY DEP LIST IS THE POINT: this runs once per mount, which is
    // once per window. Re-running it on a section change would drag an
    // operator back to their default the moment they navigated away.
  }, []);

  if (sectionId === "settings") {
    return <ClusterSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="cluster"
        subjectConcepts={CLUSTER_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "modules") return <ModulesSection />;
  if (sectionId === "origins") return <OriginsSection />;
  if (sectionId === "agents") return <AgentsSection showInactive={settings.showInactiveAgents} />;
  if (sectionId === "audit") return <AuditSection />;
  return <ReadinessSection />;
}

function ClusterSettingsSection({
  settings,
  update,
}: {
  settings: ClusterSettings;
  update: (patch: Partial<ClusterSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Cluster settings" />
      <Panel label="Cluster settings">
        <fieldset className="os-field-group">
          <legend>Open Cluster on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {CLUSTER_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time a Cluster window opens. It does not move the window you are
            looking at, and a section your role cannot reach simply does not appear in the nav.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Inactive agents</legend>
          <label className="os-check">
            <input
              type="checkbox"
              checked={settings.showInactiveAgents}
              onChange={(e) => update({ showInactiveAgents: e.target.checked })}
            />
            <span>List agents that are not active</span>
          </label>
          <p className="os-caption">
            Off by default. An inactive agent is a template nothing will run, and the standing
            question the Agents list answers is what this cluster does run. Turning this on reads a
            different query, so the list restarts.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser. They travel with the roaming desktop when epic #4746
          lands; until then a different browser starts from the defaults ({" "}
          {DEFAULT_CLUSTER_SETTINGS.defaultSection}, inactive agents hidden).
        </p>
      </Panel>
    </div>
  );
}
