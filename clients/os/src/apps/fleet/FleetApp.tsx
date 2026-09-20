import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { TaskRouting } from "./TaskRouting";
import { accessAdmits } from "../../system/registry";
import { AppsSection } from "./apps/AppsSection";
import { useAddMachineFlow } from "./addMachine/useAddMachineFlow";
import { FleetOverview } from "./overview/FleetOverview";
import { FleetWorkspace, type FleetSelection } from "./FleetWorkspace";
import { ModelsSection } from "./models/ModelsSection";
import { RoutingSection } from "./routing/RoutingSection";
import { WorkbenchesSection } from "./workbenches/WorkbenchesSection";
import {
  FLEET_SECTIONS,
  LocalFleetSettingsStore,
  type FleetSettings,
  type FleetSettingsStore, FLEET_REQUIRES, FLEET_WANTS } from "./settings";
import { Panel, Head, SetupGroup, Switch } from "../../kit";
import { useSession } from "../../chrome/access";

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
  navigation,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: FleetSettingsStore }) {
  // The store is injectable for tests, which is the whole reason the
  // parameter exists -- nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalFleetSettingsStore(), [store]);
  const [settings, setSettings] = useState<FleetSettings>(() => settingsStore.load());

  // THE GUIDED INSTALL'S STATE LIVES HERE, NOT IN THE MACHINES SECTION (design
  // record 2026-09-08-cockpit-install-wizard, D7). This component stays
  // mounted while the section changes; the section does not. A person who
  // clicks Routing while a download runs on their machine and comes back
  // finds the token still on screen and the cluster still listening.
  const addMachine = useAddMachineFlow();
  const [sessionTarget, setSessionTarget] = useState<{ id: string; revision: number }>();
  const [selection, select] = useState<FleetSelection>({ machineId: "", view: "equipment" });

  useEffect(() => {
    // CLICKING THE MACHINES TAB BRINGS UP THE LIST. It used to keep the held
    // machine and reset only its view, because there was no list to return
    // to. Opening a machine from the Overview map is a CONTENT navigation, not
    // a peer one, so it still lands on that machine.
    if (navigation?.origin === "peer" && sectionId === "machines") select({ machineId: "", view: "equipment" });
  }, [navigation?.revision]);

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

  const [visited, setVisited] = useState<string[]>([sectionId || "overview"]);
  useEffect(() => { setVisited(held => held.includes(sectionId) ? held : [...held, sectionId]); }, [sectionId]);
  const panes = Array.from(new Set([...visited, sectionId]));
  return <div className="fleet-app">
    {panes.map(id => {
      const section = FLEET_SECTIONS.find(s => s.id === id);
      if (!section || !accessAdmits(section.requires)) return null;
      const active = id === sectionId;
      return <div className="fleet-page" data-fleet-section={id} key={id} hidden={!active}>
        {id === "overview" ? <FleetOverview onOpenMachine={machineId => { select({ machineId, view: "equipment" }); navigate("machines", { fromContent: true }); }} />
          : id === "settings" ? <FleetSettingsSection settings={settings} update={update} />
          : id === "logs" ? <AppLogsSection app="fleet" subjectConcepts={FLEET_LOG_CONCEPTS} intent={active ? intent : undefined} consumeIntent={consumeIntent} />
          : id === "policies" ? <TaskRouting />
          : id === "models" ? <ModelsSection onHome={() => navigate("machines", { fromContent: true })} />
          : id === "routing" ? <RoutingSection />
          : id === "workbenches" ? <WorkbenchesSection />
          : id === "apps" ? <AppsSection sessionTarget={sessionTarget} navigation={active ? navigation : undefined} />
          : <FleetWorkspace onOpenSession={id => { setSessionTarget(held => ({ id, revision: (held?.revision ?? 0) + 1 })); navigate("apps", { fromContent: true }); }} selection={selection} select={select} navigate={navigate}
              showRevoked={settings.showRevoked} flow={addMachine}
              intent={active ? intent : undefined} consumeIntent={consumeIntent} />}
      </div>;
    })}
  </div>;
}

function FleetSettingsSection({
  settings,
  update,
}: {
  settings: FleetSettings;
  update: (patch: Partial<FleetSettings>) => void;
}) {
  const { readiness } = useSession();
  return (
    <div className="os-settings">
      <Head title="Fleet settings" />
      {/* THE SET UP GROUP sits above the preferences on purpose: it is the
          reason a person was sent here from an unconfigured surface, and the
          first thing they need is what to configure and where. Rule 4 puts
          micro-preferences in Settings; it never said they come first. */}
      <SetupGroup
        app="Fleet"
        requires={FLEET_REQUIRES}
        wants={FLEET_WANTS}
        readiness={readiness}
      />
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
          <Switch checked={settings.showRevoked} onChange={showRevoked => update({ showRevoked })}>List revoked machines</Switch>
          <p className="os-caption">
            Include machines you have removed. They remain in your history and cannot accept work.
          </p>
        </fieldset>

        <p className="os-caption">
          These preferences apply in this browser. A different browser starts on Machines with revoked machines hidden.
        </p>
      </Panel>
    </div>
  );
}
