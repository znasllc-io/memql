import {
  GraduationCap,
  Library,
  MonitorSmartphone,
  Rocket,
  Settings as SettingsIcon,
  Sparkles,
  Users,
} from "lucide-react";

import { AskSurface } from "../ask/AskSurface";
import { useAsk } from "../ask/AskProvider";
import type { OsAppManifest, OsRegistry, OsWidgetManifest } from "../system/registry";
import { FleetApp } from "./fleet/FleetApp";
import { FLEET_SECTIONS } from "./fleet/settings";
import { SettingsApp } from "./settings/SettingsApp";
import { UsersApp } from "./users/UsersApp";
import { USERS_SECTIONS } from "./users/settings";
import { StubApp } from "./StubApp";

// The installed roster (spec D12). Settings, Fleet and Users are real; the
// remaining product apps are honest stubs replaced by their epics, and the Ask
// widget is the widget framework's first resident. Static by design --
// runtime app delivery is deliberately not a foundation question.
//
// The `stub` helper stays for as long as anything uses it, and goes with the
// last one: keeping a factory nothing calls is how a file grows a shape
// nobody can explain.

function stub(manifest: Omit<OsAppManifest, "component">, epicIssue: number, summary: string): OsAppManifest {
  const full: OsAppManifest = {
    ...manifest,
    component: () => <StubApp manifest={full} epicIssue={epicIssue} summary={summary} />,
  };
  return full;
}

const settings: OsAppManifest = {
  id: "settings",
  name: "Settings",
  icon: SettingsIcon,
  sections: [
    { id: "about", name: "About" },
    { id: "appearance", name: "Appearance" },
    { id: "apps", name: "Apps" },
    { id: "cluster", name: "Cluster", roles: { min: "admin" } },
    { id: "diagnostics", name: "Diagnostics" },
  ],
  settingsSection: "appearance",
  component: SettingsApp,
};

const artifacts = stub(
  {
    id: "artifacts",
    name: "Artifacts",
    icon: Library,
    sections: [
      { id: "browse", name: "Browse" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
  },
  4721,
  "The Library on the desktop: browse and search everything MemQL holds for you, send files to the desk, open them in VS Code.",
);

const deployables = stub(
  {
    id: "deployables",
    name: "Deployables",
    icon: Rocket,
    sections: [
      { id: "sites", name: "Sites" },
      { id: "map", name: "Map" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
  },
  4725,
  "Your hosted sites and the deploy map: what serves where, bound to which artifact, live at which host.",
);

// Fleet, in full (epic #4729). Machines is first and therefore the section a
// window opens on; the app's own settings can point it elsewhere, which it
// does by navigating itself on open.
//
// The section list is FLEET_SECTIONS rather than a literal, because the
// settings section offers a "open Fleet on" picker over exactly these ids and
// a second copy of the list is one that can disagree -- a preference naming a
// section the manifest does not declare would leave the window on Machines
// with the nav highlighting nothing.
const fleet: OsAppManifest = {
  id: "fleet",
  name: "Fleet",
  icon: MonitorSmartphone,
  sections: FLEET_SECTIONS,
  settingsSection: "settings",
  component: FleetApp,
};

// Users, in full (epic #4733). People is first and is therefore the section a
// window opens on; the app's own settings can point it elsewhere, which it
// does by navigating itself on open.
//
// The section list is USERS_SECTIONS rather than a literal, for the reason
// FLEET_SECTIONS is: the settings section offers an "open Users on" picker
// over exactly these ids, and a second copy of the list is one that can
// disagree -- a preference naming a section the manifest does not declare
// leaves the window on People with the nav highlighting nothing.
//
// `roles: { min: "admin" }` is PRESENTATION (spec section E). The engine's
// `requiresOwnerOrAdmin` specs, `adminops.authorize` and row admission remain
// the authority on every read and write this app makes.
const users: OsAppManifest = {
  id: "users",
  name: "Users",
  icon: Users,
  roles: { min: "admin" },
  sections: USERS_SECTIONS,
  settingsSection: "settings",
  component: UsersApp,
};

const training = stub(
  {
    id: "training",
    name: "Training",
    icon: GraduationCap,
    roles: { min: "writer" },
    sections: [
      { id: "upload", name: "Upload" },
      { id: "review", name: "Review" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
  },
  4737,
  "Teach MemQL from files: drop documents in, watch analysis run, review what it identified.",
);

function AskWidgetBody() {
  const { transport } = useAsk();
  return <AskSurface transport={transport} variant="widget" />;
}

const askWidget: OsWidgetManifest = {
  id: "ask",
  name: "Ask",
  icon: Sparkles,
  size: { w: 3, h: 2 },
  component: AskWidgetBody,
};

export const OS_REGISTRY: OsRegistry = {
  apps: [artifacts, deployables, fleet, users, training, settings],
  widgets: [askWidget],
};
