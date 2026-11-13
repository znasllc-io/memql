import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../logs/AppLogsSection";
import { accessAdmits, type OsAppProps } from "../../system/registry";
import { Details, DetailsSection, Head, Select, SettingRow, Switch, RecordList, RecordRow, Subhead, useAppReach } from "../../kit";
import { AgentsSection } from "./agents/AgentsSection";
import { MeshSection } from "./mesh/MeshSection";
import { ModulesSection } from "./modules/ModulesSection";
import { AutomationsSection } from "./automations/AutomationsSection";
import { holds } from "../../system/roles";
import type { ProposedRelease } from "./updates/InstallationProposals";
import { UpdatesSection } from "./updates/UpdatesSection";
import { ReadinessSection } from "./readiness/ReadinessSection";
import { CapacitySection } from "./capacity/CapacitySection";
import { ClusterSection } from "../settings/ClusterSection";
import { BenchmarksSection } from "../settings/BenchmarksSection";
import {
  CLUSTER_SECTIONS,
  LocalClusterSettingsStore,
  type ClusterSettings,
  type ClusterSettingsStore,
} from "./settings";

// Cluster: what this cluster is made of, and how it is going.
//
// Readiness answers "can this thing do work at all"; Modules answers "what is
// it made of"; Mesh answers "does every node hear what the cluster
// broadcasts"; Agents answers "what does it run"; facts and benchmarks
// report what is recorded and measured. None of them is a wizard and none
// of them blocks anything -- the portal's first-run gate is deliberately not rebuilt here
// (see readiness/ReadinessSection.tsx).
//
// Sections are the app's own navigation. It never opens a window.

/** The agent runtime this app owns. Data-origin logs belong to Settings;
 * identity decisions belong to Identity. */
const CLUSTER_LOG_CONCEPTS = [
  Concepts.AGENTS_AGENT,
  Concepts.AGENTS_AGENT_AUTHORIZATION,
] as const;

export function ClusterApp(props: OsAppProps & { store?: ClusterSettingsStore }) {
  // Moved sections keep their original capability doors in the registry.
  // A facts-only grant must not mount runtime/readiness reads on its way in.
  if (props.sectionId === "facts") return <ClusterSection />;
  if (props.sectionId === "benchmarks") return <BenchmarksSection />;
  return <ClusterRuntimeApp {...props} />;
}

function ClusterRuntimeApp({
  sectionId,
  windowVisible = true,
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
  const [releaseSelection, setReleaseSelection] = useState<ProposedRelease | null>(null);
  useEffect(() => {
    const release = intent?.payload.release;
    if (!release || typeof release !== "object" || Array.isArray(release)) return;
    const selected = release as Record<string, unknown>;
    if (typeof selected.candidateId !== "string" || !selected.candidateId || typeof selected.catalogDigest !== "string" || !/^sha256:[a-f0-9]{64}$/.test(selected.catalogDigest) || typeof selected.label !== "string") return;
    applied.current = true;
    setReleaseSelection({ candidateId: selected.candidateId, catalogDigest: selected.catalogDigest, label: selected.label });
    navigate("updates");
    consumeIntent?.(intent!.id);
  }, [intent, navigate, consumeIntent]);
  const pipelines = useAppReach("pipelines");
  useEffect(() => {
    if (intent?.payload.storageWarning !== true) return;
    applied.current = true;
    navigate("readiness");
    consumeIntent?.(intent.id);
  }, [intent, navigate, consumeIntent]);
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

  if (sectionId === "overview") return <><ReadinessSection title="Overview" />{accessAdmits("app:settings") && accessAdmits("app:settings/cluster") ? <ClusterSection inline /> : null}<div className="os-cluster"><Subhead>Explore</Subhead><RecordList as="ul" label="Cluster details"><RecordRow name="Runtime" secondary="Nodes, capacity and installed capabilities" onOpen={() => navigate("runtime", { fromContent: true })} />{holds("read", "app:cluster/updates") ? <RecordRow name="Updates" secondary="Published versions available to this installation" onOpen={() => navigate("updates", { fromContent: true })} /> : null}</RecordList></div></>;
  if (sectionId === "runtime") return <div className="os-cluster os-app-stack"><Head title="Runtime" /><RecordList as="ul" label="Runtime components">{CLUSTER_SECTIONS.filter(section => section.parent === "runtime" && (!section.requires || holds("read", section.requires))).map(section => <RecordRow key={section.id} name={section.name} onOpen={() => navigate(section.id, { fromContent: true })} />)}</RecordList></div>;
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
  if (sectionId === "releases") return <div className="os-cluster"><Head title="Releases" /><RecordList as="ul" label="Release management"><RecordRow name="Open releases in Pipelines" onOpen={() => pipelines.open("releases")} /></RecordList></div>;
  if (sectionId === "capacity") return <CapacitySection visible={windowVisible} />;
  if (sectionId === "updates") return <UpdatesSection visible={windowVisible} releaseSelection={releaseSelection} clearReleaseSelection={() => setReleaseSelection(null)} />;
  if (sectionId === "modules") return <ModulesSection />;
  if (sectionId === "mesh") return <MeshSection intent={intent} consumeIntent={consumeIntent} />;
  if (sectionId === "automations") return <AutomationsSection />;
  if (sectionId === "agents") return <AgentsSection showInactive={settings.showInactiveAgents} />;
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
    <div className="os-cluster">
      <Head title="Settings" />
      <Details>
        <DetailsSection title="General">
          <SettingRow label="Start page">
            <Select id="cluster-start-page" label="Open Cluster on" value={settings.defaultSection} onChange={defaultSection => update({ defaultSection })}>
              {CLUSTER_SECTIONS.map(section => <option key={section.id} value={section.id}>{section.name}</option>)}
            </Select>
            <p className="os-caption">Used the next time you open Cluster.</p>
          </SettingRow>
          <SettingRow label="Agents">
            <Switch checked={settings.showInactiveAgents} onChange={showInactiveAgents => update({ showInactiveAgents })}>Show inactive agents</Switch>
          </SettingRow>
        </DetailsSection>
      </Details>
    </div>
  );
}
