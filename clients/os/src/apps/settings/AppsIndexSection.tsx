import { Caption, Head, RecordList, RecordRow } from "../../kit";
import { useOs } from "../../chrome/state";
import { appsFor, sectionsFor, settingsSectionProblem } from "../../system/registry";
import type { OsAppManifest } from "../../system/registry";

// The apps index (memql#4743): a DIRECTORY of every installed app's own
// settings, not a host for them.
//
// The distinction is the whole design. Settings could embed each app's
// settings UI, and then every app epic would have to keep a second surface
// working inside a window it does not own -- role gating, live reads, Ask
// context and all. Instead an entry OPENS the app on its own settings
// section, through the same open-by-id path the dock and launcher use. One
// app, one place its settings live, and this list only points at them.

export function AppsIndexSection() {
  const { registry, actorRole, actions } = useOs();
  // The one role predicate, through the registry's selector -- the same call
  // the launcher grid and the dock make. An app hidden from this session is
  // hidden here too, and for exactly one reason rather than two.
  const apps = appsFor(registry);

  return (
    <div className="os-settings">
      <Head title="Apps" meta={apps.length} />
      <p className="os-stub-summary">
        Every app installed on this cluster that you can open. Each one keeps
        its own settings; these entries take you there.
      </p>
      <RecordList as="ul" label="Installed apps">
        {apps.map((app) => (
          <AppEntry key={app.id} app={app} actorRole={actorRole} onOpen={openAppSettings} />
        ))}
      </RecordList>
      <Caption>
        Presentation gating only: apps you cannot see are hidden here, but the
        engine's row admission is the authority on every read.
      </Caption>
    </div>
  );

  function openAppSettings(app: OsAppManifest): void {
    // Open-by-id first: this focuses the app's existing window when it is
    // already running (spec A) rather than opening a second one, and it is
    // the path that carries the `canOpen` admission check. Only then do we
    // navigate -- the window id comes back on the effect, and a refused or
    // absent placement carries none, so there is nothing to navigate and we
    // correctly do nothing.
    const effect = actions.openApp(app.id, app.settingsSection);
    if (effect.kind === "refused-full" || effect.kind === "none") return;
    // A NEW window was already created on the right section by the call
    // above; this is for the focus-existing case, where the window is
    // already open on whatever the person last looked at.
    if (effect.kind === "focused-existing") {
      actions.navigateSection(effect.windowId, app.settingsSection);
    }
  }
}

function AppEntry({
  app,
  actorRole,
  onOpen,
}: {
  app: OsAppManifest;
  actorRole: string;
  onOpen: (app: OsAppManifest) => void;
}) {
  const Icon = app.icon;
  const admitted = sectionsFor(app);
  const target = admitted.find((s) => s.id === app.settingsSection);
  // A manifest defect and a role gate look identical from here -- both end
  // with no reachable target -- so they are told apart and worded apart. The
  // first is a bug in the shipped app; the second is a correct answer about
  // this session.
  const problem = settingsSectionProblem(app);

  return (
    <RecordRow name={app.name} icon={<Icon size={18} aria-hidden />} secondary={target ? undefined : problem ?? `Requires a higher role than ${actorRole || "unknown"}`} actions={target ? <button type="button" className="os-choice" onClick={() => onOpen(app)}>{target.name}</button> : undefined} />
  );
}
