import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { AppsSection } from "./apps/AppsSection";
import { MachinesSection } from "./machines/MachinesSection";
import { RoutingSection } from "./routing/RoutingSection";
import { WorkbenchesSection } from "./workbenches/WorkbenchesSection";
import {
  DEFAULT_FLEET_SETTINGS,
  FLEET_SECTIONS,
  LocalFleetSettingsStore,
  type FleetSettings,
  type FleetSettingsStore,
} from "./settings";
import { Panel, Head } from "../../kit";

// Fleet: the machines you own, how work is routed to them, and the
// workbenches that run the work that does not need them (epic memql#4729).
//
// The foundation shipped this app as its live exemplar -- a read-only machine
// list proving the substrate end to end. Everything here is the promotion of
// that exemplar into the app: rename, operator labels, revoke and per-machine
// detail; the routing policy and the per-call routing record; the workbench
// workspaces and the add-machine entry.
//
// Sections are the app's own navigation. It never opens a window.

/** The concepts this app owns, for its Logs section: a line about a
 *  machine, a routing policy, a workspace, a delegation policy or a delegated
 *  run is this app's line. */
const FLEET_LOG_CONCEPTS = [
  Concepts.WORKER_REGISTRATION,
  Concepts.WORKER_ROUTING_POLICY,
  Concepts.WORKBENCH_WORKSPACE,
  // The Apps section's two (epic memql#5009), by the same reading as the
  // three above: this app is where they are set and read.
  Concepts.WORKER_DELEGATION_POLICY,
  Concepts.WORKER_APP_SESSION,
] as const;

export function FleetApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: FleetSettingsStore }) {
  // The store is injectable for tests, which is the whole reason the
  // parameter exists -- nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalFleetSettingsStore(), [store]);
  const [settings, setSettings] = useState<FleetSettings>(() => settingsStore.load());

  function update(patch: Partial<FleetSettings>) {
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
  // Machines is not dragged back to their default.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened
    // on a named section -- the Settings apps index deep-linking to this
    // app's own settings, say -- was opened by somebody who said where they
    // wanted to be, and a preference that overrode that would make the
    // deep-link silently not work (memql#4743).
    const shellDefault = FLEET_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // AN EMPTY DEP LIST IS THE POINT: this runs once per mount, which is
    // once per window. Re-running it on a section change would drag an
    // operator back to their default the moment they navigated away.
  }, []);

  if (sectionId === "settings") {
    return <FleetSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="fleet"
        subjectConcepts={FLEET_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "routing") return <RoutingSection />;
  if (sectionId === "workbenches") return <WorkbenchesSection />;
  if (sectionId === "apps") return <AppsSection />;
  return <MachinesSection showRevoked={settings.showRevoked} />;
}

function FleetSettingsSection({
  settings,
  update,
}: {
  settings: FleetSettings;
  update: (patch: Partial<FleetSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Fleet settings" />
      <Panel label="Fleet settings">
        <fieldset className="os-field-group">
          <legend>Open Fleet on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {FLEET_SECTIONS.map((section) => (
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
            Applies the next time a Fleet window opens. It does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Revoked machines</legend>
          <label className="os-check">
            <input
              type="checkbox"
              checked={settings.showRevoked}
              onChange={(e) => update({ showRevoked: e.target.checked })}
            />
            <span>List revoked machines</span>
          </label>
          <p className="os-caption">
            Off by default. A revoked registration is a credential that no longer works, and the
            standing question the Machines list answers is which machines do. Revoked entries are
            marked and never show as online, whatever their last heartbeat says.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser. They travel with the roaming desktop when epic #4746
          lands; until then a different browser starts from the defaults ({" "}
          {DEFAULT_FLEET_SETTINGS.defaultSection}, revoked machines hidden).
        </p>
      </Panel>
    </div>
  );
}
