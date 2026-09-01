import {
  Building2,
  Files as FilesIcon,
  GraduationCap,
  MonitorSmartphone,
  Rocket,
  Settings as SettingsIcon,
  Sparkles,
  Users,
} from "lucide-react";

import { AskSurface } from "../ask/AskSurface";
import { useAsk } from "../ask/AskProvider";
import type { OsAppManifest, OsRegistry, OsWidgetManifest } from "../system/registry";
import { AccountsApp } from "./accounts/AccountsApp";
import { ACCOUNTS_SECTIONS } from "./accounts/settings";
import { DeployablesApp } from "./deployables/DeployablesApp";
import { DEPLOYABLES_SECTIONS } from "./deployables/settings";
import { FilesApp } from "./files/FilesApp";
import { FILES_SECTIONS } from "./files/settings";
import { FleetApp } from "./fleet/FleetApp";
import { FLEET_SECTIONS } from "./fleet/settings";
import { SettingsApp } from "./settings/SettingsApp";
import { TrainingApp } from "./training/TrainingApp";
import { TRAINING_SECTIONS } from "./training/settings";
import { UsersApp } from "./users/UsersApp";
import { USERS_SECTIONS } from "./users/settings";

// The installed roster (spec D12). Every app is real now -- Files (epic
// #4721) replaced the last stub, and the `stub` helper and StubApp went with
// it, as the note that used to sit here promised they would. The Ask widget
// is the widget framework's first resident. Static by design -- runtime app
// delivery is deliberately not a foundation question.

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

// Files, in full (epic #4721). Browse is first and is therefore the section
// a window opens on: the Library as a folder tree, live, with the inspector
// telling each file's provenance story. The app's own settings hold the
// list defaults and the archive confirm.
//
// The section list is FILES_SECTIONS rather than a literal, for the reason
// its four siblings are: the gear and the manifest must offer the same set,
// and a second copy of the list is one that can disagree.
//
// NO manifest role: `v1:library:artifact` and `v1:library:folder` declare
// the composite tier (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so
// every signed-in person has a Library of their own to read and the engine
// decides how far every read reaches. Gating here would be presentation
// pretending to be authorization.
const files: OsAppManifest = {
  id: "files",
  name: "Files",
  icon: FilesIcon,
  sections: FILES_SECTIONS,
  settingsSection: "settings",
  component: FilesApp,
};

// Deployables, in full (epic #4725). MAP is first and is therefore the section
// a window opens on: what serves where is a shape rather than a table, and the
// map is the surface this app was asked for. The app's own settings can point
// it at the list instead, which it does by navigating itself on open.
//
// The section list is DEPLOYABLES_SECTIONS rather than a literal, for the
// reason FLEET_SECTIONS and USERS_SECTIONS are: the settings section offers an
// "open Deployables on" picker over exactly these ids, and a second copy of the
// list is one that can disagree -- a preference naming a section the manifest
// does not declare leaves the window on the first section with the nav
// highlighting nothing.
//
// THE APP ITSELF CARRIES NO ROLE, and its Actions section carries admin+. That
// split is the point: `v1:platform:site` declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person
// has deployables of their own to read and there is nothing to gate -- the
// engine decides how far the list reaches. The section role is PRESENTATION
// (spec section E) over writes the Go hostname policy and
// `sitePublishFromArtifact` remain the authority on.
const deployables: OsAppManifest = {
  id: "deployables",
  name: "Deployables",
  icon: Rocket,
  sections: DEPLOYABLES_SECTIONS,
  settingsSection: "settings",
  component: DeployablesApp,
};

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

// Training, in full (epic #4737). Upload is first and is therefore the section
// a window opens on: this app is for teaching MemQL from files, and the
// dropzone is the thing it is for. The app's own settings can point it
// elsewhere, which it does by navigating itself on open.
//
// The section list is TRAINING_SECTIONS rather than a literal, for the reason
// FLEET_SECTIONS, USERS_SECTIONS and DEPLOYABLES_SECTIONS are: the settings
// section offers an "open Training on" picker over exactly these ids, and a
// second copy of the list is one that can disagree -- a preference naming a
// section the manifest does not declare leaves the window on Upload with the
// nav highlighting nothing.
//
// `roles: { min: "writer" }` is PRESENTATION (spec section E). It is on the
// APP rather than on any section because every surface here reads or writes
// the same two populations -- there is no line inside the app where the answer
// changes. The engine remains the authority: row admission decides every read,
// the attachment handler checks space ownership before it parses a byte, and
// `setChunkValidationStatus` is admitted for any authenticated caller because
// `v1:knowledge:documentChunk` declares no tier (the standing residual its
// sibling mutations already sit on, recorded in the per-row-authz audit).
const training: OsAppManifest = {
  id: "training",
  name: "Training",
  icon: GraduationCap,
  roles: { min: "writer" },
  sections: TRAINING_SECTIONS,
  settingsSection: "settings",
  component: TrainingApp,
};

// Accounts, in full (epic memql#4800). The client registry: who this instance
// does work for, and what of the cluster's is theirs. Accounts is first and is
// therefore the section a window opens on -- there are two, and the other is
// the app's own settings.
//
// The section list is ACCOUNTS_SECTIONS rather than a literal, for the reason
// its five siblings are: the gear and the manifest must offer the same set,
// and a second copy of the list is one that can disagree.
//
// NO manifest role. `v1:accounts:account` declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person
// has accounts of their own to read and the engine decides how far the list
// reaches. Gating here would be presentation pretending to be authorization --
// and the one surface inside that IS gated, the guest-invitation rollup, is
// gated by the engine's own `requiresOwnerOrAdmin` and renders the refusal
// rather than hiding the band.
//
// NOT always-docked. That is the Bin's distinction (#4784); this is an
// ordinary app that opens from the launcher like every other one.
const accounts: OsAppManifest = {
  id: "accounts",
  name: "Accounts",
  icon: Building2,
  sections: ACCOUNTS_SECTIONS,
  settingsSection: "settings",
  component: AccountsApp,
};

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
  apps: [accounts, files, deployables, fleet, users, training, settings],
  widgets: [askWidget],
};
