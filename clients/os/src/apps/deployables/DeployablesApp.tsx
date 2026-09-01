import { useEffect, useMemo, useRef, useState } from "react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import { Head, Panel, roleAdmits } from "../../kit";
import { useSession } from "../../chrome/access";
import { useLiveView } from "../../live/liveView";
import { useArrivals } from "../../live/useArrivals";
import type { OsAppProps } from "../../system/registry";
import { ActionsSection } from "./actions/ActionsSection";
import { MapSection, NO_SELECTION, type MapSelection } from "./map/MapSection";
import type { MapNode } from "./map/layout";
import { PackagesSection } from "./packages/PackagesSection";
import { packageFromRow, type PackageRow } from "./packages/rows";
import { usePackages } from "./packages/usePackages";
import { siteFingerprint, siteFromRow, type SiteRow } from "./rows";
import { SitesSection } from "./SitesSection";
import {
  DEFAULT_DEPLOYABLES_SETTINGS,
  DEPLOYABLES_SECTIONS,
  LIST_DENSITIES,
  LocalDeployablesSettingsStore,
  type DeployablesSettings,
  type DeployablesSettingsStore,
  type ListDensity,
} from "./settings";
import { useSites } from "./useSites";

// Deployables: the sites this cluster serves, the map of what serves where,
// and the two writes that change it (epic memql#4725).
//
// ===========================================================================
// ONE FEED, ONE SELECTION, THREE SURFACES
// ===========================================================================
// The list, the map and the detail panel are readings of a single retained
// LiveCollection, held here rather than inside each section. Two subscriptions
// over one concept would be free to disagree about what the cluster currently
// holds, and "the list and the map disagree" is the one failure this app must
// not have -- it is a picture and a table of the same thing, side by side.
//
// Holding the feed here also means switching sections costs nothing: the
// collection stays retained for the life of the window rather than re-seeding
// every time somebody looks at the map and comes back.
//
// Sections are the app's own navigation. It never opens a window.

// The disconnected snapshot, generic over the row type so the two feeds share
// one rather than casting. A cast here would be the kind of thing that reads
// as harmless and hides a real type change later.
const EMPTY_SNAPSHOT = <T,>(): LiveSnapshot<T> => ({
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
});

export function DeployablesApp({
  sectionId,
  navigate,
  askContext,
  store,
}: OsAppProps & { store?: DeployablesSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalDeployablesSettingsStore(), [store]);
  const [settings, setSettings] = useState<DeployablesSettings>(() => settingsStore.load());
  const { access, config } = useSession();
  // An UNRESOLVED session is not an admin. `roleAdmits` refuses an unrankable
  // role, so "" admits only ungated surfaces -- the right answer while access
  // is still resolving, and the safe one if it never resolves.
  const actorRole = access?.clusterRole ?? "";
  const viewerUserId = access?.userId ?? "";
  const canWrite = roleAdmits(actorRole, { min: "admin" });

  const { source: collection, reseed } = useSites();
  // A SECOND FEED, over a second concept, and deliberately not folded into the
  // one above. `useSites` and `usePackages` read different concepts with
  // different shapes -- one feed cannot carry both -- and each is retained
  // once here for the life of the window so switching sections costs nothing.
  // The rule the app root exists for is one feed PER CONCEPT, not one feed
  // total: what must never happen is two subscriptions over the SAME concept
  // free to disagree about what the cluster holds.
  const { source: packageCollection, reseed: reseedPackages } = usePackages();

  // PROJECT, then narrow, in one pass. The collection holds RAW wire rows --
  // the fold upserts an event payload as the row type with no projection hook
  // -- so every predicate below has to run on a `siteFromRow` result.
  //
  // `deleted` is dropped HERE as well as by `sitesAll`'s own `isNotDeleted`
  // conjunct, because a soft delete arrives as an UPDATE: the read excludes it
  // and the subscription does not, so without this the row would vanish on a
  // reseed and reappear on the next event.
  const sites = useLiveView<Row, SiteRow>(collection, "sites", (rows) =>
    rows.map(siteFromRow).filter((s) => s.id !== "" && !s.deleted),
  );
  const snapshot = sites?.snapshot ?? EMPTY_SNAPSHOT<SiteRow>();

  // The arrival cue, ONCE, for the whole app. The list renders it through
  // LiveList and the map draws it on its nodes; both read the same reducer over
  // the same snapshot, so a publish announces itself identically in both.
  const ticks = useArrivals(snapshot, (s) => s.id, siteFingerprint);

  const packages = useLiveView<Row, PackageRow>(packageCollection, "packages", (rows) =>
    rows.map(packageFromRow).filter((p) => p.id !== ""),
  );
  const packageSnapshot = packages?.snapshot ?? EMPTY_SNAPSHOT<PackageRow>();
  const [selectedPackageId, setSelectedPackageId] = useState("");

  const [selection, setSelection] = useState<MapSelection>(NO_SELECTION);
  const selectedSiteId = selection.siteIds.length === 1 ? (selection.siteIds[0] ?? "") : "";

  function selectSite(siteId: string) {
    setSelection(siteId === "" ? NO_SELECTION : { nodeId: `site:${siteId}`, siteIds: [siteId] });
  }

  function selectNode(node: MapNode) {
    setSelection((held) =>
      held.nodeId === node.id ? NO_SELECTION : { nodeId: node.id, siteIds: [...node.siteIds] },
    );
  }

  function update(patch: Partial<DeployablesSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- Fleet's pattern,
  // and its reasoning holds unchanged. The shell opens an app on its manifest's
  // FIRST section, so an app-level "open me here" can only be the app
  // navigating itself on the first render of this component instance.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on a
    // named section was opened by somebody who said where they wanted to be --
    // the Settings apps index deep-linking to this app's own settings, say --
    // and a preference that overrode that would make the deep link silently not
    // work (memql#4743).
    const shellDefault = DEPLOYABLES_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW. Re-running on a section change
    // would drag somebody back to their default the moment they navigated away.
  }, []);

  if (sectionId === "settings") {
    return <DeployablesSettingsSection settings={settings} update={update} actorRole={actorRole} />;
  }
  if (sectionId === "actions") {
    return <ActionsSection domain={config.domain} />;
  }
  if (sectionId === "packages") {
    return (
      <PackagesSection
        source={packages}
        snapshot={packageSnapshot}
        selectedPackageId={selectedPackageId}
        onSelect={setSelectedPackageId}
        viewerUserId={viewerUserId}
        domain={config.domain}
        canWrite={canWrite}
        onReseed={reseedPackages}
        onAsk={askContext}
      />
    );
  }
  if (sectionId === "sites") {
    return (
      <SitesSection
        source={sites}
        snapshot={snapshot}
        density={settings.density}
        selectedSiteId={selectedSiteId}
        onSelectSite={selectSite}
        viewerUserId={viewerUserId}
        canPublish={canWrite}
        clusterDomain={config.domain}
        canManage={canWrite}
        onAsk={askContext}
        onReseed={reseed}
      />
    );
  }
  return (
    <MapSection
      sites={snapshot.rows}
      snapshot={snapshot}
      ticks={ticks}
      selection={selection}
      onSelectNode={selectNode}
      onSelectSite={selectSite}
      viewerUserId={viewerUserId}
      canPublish={canWrite}
      clusterDomain={config.domain}
      onAsk={askContext}
      onReseed={reseed}
    />
  );
}

function DeployablesSettingsSection({
  settings,
  update,
  actorRole,
}: {
  settings: DeployablesSettings;
  update: (patch: Partial<DeployablesSettings>) => void;
  actorRole: string;
}) {
  // OFFER ONLY WHAT THIS SESSION CAN OPEN. A preference naming a section the
  // reader is not admitted to would silently do nothing -- WindowFrame falls
  // back to the first admitted section -- which reads as a broken setting
  // rather than as one that does not apply.
  const offered = DEPLOYABLES_SECTIONS.filter((s) => roleAdmits(actorRole, s.roles));

  return (
    <div className="os-settings">
      <Head title="Deployables settings" />
      <Panel label="Deployables settings">
        <fieldset className="os-field-group">
          <legend>Open Deployables on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {offered.map((section) => (
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
            The map is the default because "what serves where" is a shape rather than a table.
            Applies the next time a Deployables window opens; it does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>List density</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="List density">
            {LIST_DENSITIES.map((density) => (
              <button
                key={density}
                type="button"
                role="radio"
                aria-checked={settings.density === density}
                className="os-choice"
                onClick={() => update({ density: density as ListDensity })}
              >
                {density}
              </button>
            ))}
          </div>
          <p className="os-caption">
            A view setting, not a filter: it changes how tightly the Sites list packs and nothing
            about which deployables are read or shown.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          preference can never cost you your desks. The defaults are{" "}
          {DEFAULT_DEPLOYABLES_SETTINGS.defaultSection} at{" "}
          {DEFAULT_DEPLOYABLES_SETTINGS.density} density.
        </p>
      </Panel>
    </div>
  );
}
