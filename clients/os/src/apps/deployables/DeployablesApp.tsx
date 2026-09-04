import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts, type LiveSnapshot, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Head, Panel, roleAdmits } from "../../kit";
import { useSession } from "../../chrome/access";
import { useLiveView, type LiveView } from "../../live/liveView";
import { useArrivals } from "../../live/useArrivals";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { DeployablesSection } from "./DeployablesSection";
import { MapSection, NO_SELECTION, type MapSelection } from "./map/MapSection";
import type { MapNode } from "./map/layout";
import { deploymentFromRow, packageFromRow, type DeploymentRow, type PackageRow } from "./packages/rows";
import { useAwaitingConfirm } from "./packages/useAwaitingConfirm";
import { usePackages } from "./packages/usePackages";
import { siteFingerprint, siteFromRow, type SiteRow } from "./rows";
import { SourcesGroup } from "./settings/SourcesGroup";
import type { ConnectReturn } from "./sources/connectReturn";
import { credentialFromRow, type CredentialRow } from "./sources/rows";
import { useSourceCredentials } from "./sources/useSourceCredentials";
import {
  DEFAULT_DEPLOYABLES_SETTINGS,
  DEPLOYABLES_SECTIONS,
  LIST_DENSITIES,
  LocalDeployablesSettingsStore,
  type DeployablesSettings,
  type DeployablesSettingsStore,
  type ListDensity,
} from "./settings";
import { DeployablesSettingsProvider } from "./settingsContext";
import { useSites } from "./useSites";

// Deployables: the things this cluster serves, the map of what serves where,
// and the one flow that makes a new one (epic memql#4725, rebuilt by
// memql#4885 around one list and one page).
//
// ===========================================================================
// ONE FEED PER CONCEPT, ONE SELECTION, THREE SURFACES
// ===========================================================================
// The list, the map and the page are readings of collections retained HERE
// rather than inside each section. Two subscriptions over one concept would
// be free to disagree about what the cluster currently holds, and "the list
// and the map disagree" is the one failure this app must not have -- it is a
// picture and a table of the same thing, side by side.
//
// Holding the feeds here also means switching sections costs nothing: each
// collection stays retained for the life of the window rather than re-seeding
// every time somebody looks at the map and comes back.
//
// Sections are the app's own navigation. It never opens a window.

// The disconnected snapshot, generic over the row type so the feeds share one
// rather than casting. A cast here would be the kind of thing that reads as
// harmless and hides a real type change later.
const EMPTY_SNAPSHOT = <T,>(): LiveSnapshot<T> => ({
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
});

/** The concepts this app owns, for its Logs section: what serves, where it
 *  came from, each attempt to deploy it, and a client's own domain on it. */
const DEPLOYABLES_LOG_CONCEPTS = [
  Concepts.PLATFORM_SITE,
  Concepts.PLATFORM_PACKAGE,
  Concepts.PLATFORM_PACKAGE_DEPLOYMENT,
  Concepts.PLATFORM_CUSTOM_DOMAIN,
] as const;

export function DeployablesApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
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
  // Rank >= 200 under the one ladder (epic memql#4832, D1) -- {admin,
  // developer, owner}, which is the set the engine's deploy gate already
  // uses. Before the flip this excluded developer, so the deploy tier saw a
  // read-only Deployables app.
  const canWrite = roleAdmits(actorRole, { min: "admin" });
  // The OWNER rung, under the same ladder: a client's own domain is a
  // cluster owner's act (memql#4805, D1), and the page renders the Domains
  // content for one and for nobody else. Presentation; the concept's
  // clusterOwner tier is the gate.
  const isClusterOwner = roleAdmits(actorRole, { min: "owner" });

  const { source: collection, reseed } = useSites();
  // A SECOND FEED, over a second concept, and deliberately not folded into the
  // one above. `useSites` and `usePackages` read different concepts with
  // different shapes -- one feed cannot carry both -- and each is retained
  // once here for the life of the window so switching sections costs nothing.
  // The rule the app root exists for is one feed PER CONCEPT, not one feed
  // total: what must never happen is two subscriptions over the SAME concept
  // free to disagree about what the cluster holds.
  const { source: packageCollection, reseed: reseedPackages } = usePackages();
  // A THIRD FEED, over a third concept (epic memql#4885): the caller's own
  // source credentials, read once here as CARDS and passed down so the
  // Source stop's chip and, later, the Sources settings group are two
  // readings of one feed rather than two subscriptions free to disagree.
  const { source: credentialCollection } = useSourceCredentials();
  // A FOURTH FEED, and the ONE recorded exception to clients/os/README.md's
  // rule that a package's deployment timeline is retained by the page and
  // never by the root (that rule guards against subscribing a window to
  // every deploy in the cluster to render one). This holds PARKED RUNS ONLY
  // -- deployments at `awaiting_confirm`, a handful of rows a person needs to
  // see before they open anything, because the list's waiting mark ("a
  // deploy is waiting for you") is how somebody who closed the window
  // mid-compose finds their run again. It never holds a timeline, and a run
  // that moves on leaves it on its own event. The whole account is in
  // `packages/useAwaitingConfirm.ts`.
  const { source: awaitingCollection, reseed: reseedAwaiting } = useAwaitingConfirm();

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

  const credentials = useLiveView<Row, CredentialRow>(credentialCollection, "credentials", (rows) =>
    rows.map(credentialFromRow).filter((c) => c.id !== ""),
  );
  const credentialRows = credentials?.snapshot.rows ?? [];

  // `awaiting_confirm` is held HERE as well as by the feed's `inScope`: the
  // seed and the events both narrow to it, and the projection says so once
  // more so a row this view renders can never be a run that has moved on.
  const parked = useLiveView<Row, DeploymentRow>(awaitingCollection, "awaitingConfirm", (rows) =>
    rows.map(deploymentFromRow).filter((d) => d.id !== "" && d.status === "awaiting_confirm"),
  );
  const parkedSnapshot = parked?.snapshot ?? EMPTY_SNAPSHOT<DeploymentRow>();

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

  function reseedAll() {
    reseed();
    reseedPackages();
    reseedAwaiting();
  }

  function update(patch: Partial<DeployablesSettings>) {
    // THE FUNCTIONAL FORM, because a patch is applied to the LATEST document
    // rather than to the one this render closed over. With one preference per
    // screen that distinction never showed; the per-group open/shut set made
    // it visible immediately -- two toggles in a tick and the second wrote its
    // own id over the first's, so opening two sources left one open.
    //
    // The save sits inside the updater so it cannot be handed a stale value
    // either. It is idempotent -- the same document written twice is the same
    // document -- which is what makes it safe under StrictMode's double
    // invocation.
    setSettings((held) => {
      const next = { ...held, ...patch, version: 1 as const };
      settingsStore.save(next);
      return next;
    });
  }

  /** Open or shut one source group, as a function of the LATEST open set. */
  function toggleSource(id: string) {
    setSettings((held) => {
      const open = held.expandedSources.includes(id)
        ? held.expandedSources.filter((s) => s !== id)
        : [...held.expandedSources, id];
      const next = { ...held, expandedSources: open, version: 1 as const };
      settingsStore.save(next);
      return next;
    });
  }

  // THE ANSWER FROM A GITHUB CONNECT, delivered as a window intent (epic
  // memql#4915). It is held here and RENDERED BY THE SOURCES GROUP rather
  // than by this component, because the result belongs on the surface that
  // asked for the connection -- a page of its own would be a toast with more
  // pixels. Consumed by id, so acting on a stale render can never eat a
  // newer instruction, and an unrecognised payload is consumed and ignored
  // rather than left standing to re-fire on every render.
  const [connectResult, setConnectResult] = useState<ConnectReturn | null>(null);
  useEffect(() => {
    if (!intent) return;
    const carried = intent.payload["connect"];
    if (carried !== null && typeof carried === "object") {
      const answer = carried as Partial<ConnectReturn>;
      setConnectResult({
        reason: typeof answer.reason === "string" ? answer.reason : "",
        section: typeof answer.section === "string" ? answer.section : sectionId,
      });
    }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent, sectionId]);

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
    return (
      <DeployablesSettingsSection
        settings={settings}
        update={update}
        actorRole={actorRole}
        credentials={credentials}
        packages={packageSnapshot.rows}
        connectResult={connectResult}
      />
    );
  }
  // The app's slice of the cluster's logs (epic memql#4895). It survived the
  // compose restructure while Sites, Packages and Actions did not, and the
  // difference is whose section it is: those three were this app's own reading
  // of its subject and became one, and this is a shell convention every app
  // carries at the log store's own admin floor.
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="deployables"
        subjectConcepts={DEPLOYABLES_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "deployables") {
    return (
      // THE ONE BRANCH THAT NEEDS IT. The traffic window is read four levels
      // down (Live stop -> traffic panel) and the open-source set is read by
      // the list itself; both are written here. The settings section takes
      // `settings` and `update` directly because it is one level away, and the
      // map has neither.
      <DeployablesSettingsProvider value={{ settings, update, toggleSource }}>
        <DeployablesSection
          sites={sites}
          packages={packages}
          parked={parked}
          feedError={snapshot.error || packageSnapshot.error || parkedSnapshot.error}
          density={settings.density}
          selectedSiteId={selectedSiteId}
          onSelectSite={selectSite}
          viewerUserId={viewerUserId}
          canWrite={canWrite}
          isClusterOwner={isClusterOwner}
          clusterDomain={config.domain}
          credentials={credentialRows}
          onAsk={askContext}
          onReseed={reseedAll}
        />
      </DeployablesSettingsProvider>
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
      // THE MAP POINTS, THE SECTION OWNS THE PAGE (rule 11). Choosing a
      // deployable on the map selects it -- the two surfaces share ONE
      // selection -- and navigates to the section that owns its page, which
      // opens on that selection.
      onOpenDeployable={(siteId) => {
        selectSite(siteId);
        navigate("deployables");
      }}
      packages={packageSnapshot.rows}
      onReseed={reseed}
    />
  );
}

function DeployablesSettingsSection({
  settings,
  update,
  actorRole,
  credentials,
  packages,
  connectResult,
}: {
  settings: DeployablesSettings;
  update: (patch: Partial<DeployablesSettings>) => void;
  actorRole: string;
  /** The app root's one credentials feed, for the Sources group. */
  credentials: LiveView<CredentialRow> | null;
  /** The app root's package rows, joined onto each credential by `credentialId`. */
  packages: readonly PackageRow[];
  /** The answer from a GitHub connect, rendered by the group that asked. */
  connectResult: ConnectReturn | null;
}) {
  // OFFER ONLY WHAT THIS SESSION CAN OPEN. A preference naming a section the
  // reader is not admitted to would silently do nothing -- WindowFrame falls
  // back to the first admitted section -- which reads as a broken setting
  // rather than as one that does not apply. No section carries a role today,
  // so this is every one of the three; the filter stays for the day one does.
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
            A view setting, not a filter: it changes how tightly the Deployables list packs and
            nothing about which deployables are read or shown.
          </p>
        </fieldset>

        {/* THE SOURCES GROUP IS NOT A PREFERENCE, and it is here anyway: the
            two credential acts a person takes outside a compose flow --
            add one ahead of time, revoke one that leaked -- have nowhere
            else to live, and Settings is where an app keeps what is about
            the app rather than about one row (DESIGN.md rule 4's home, one
            step out). The two above ARE preferences and stay above it. */}
        <SourcesGroup credentials={credentials} packages={packages} connectResult={connectResult} />

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
