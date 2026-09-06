import {
  Building2,
  Files as FilesIcon,
  GraduationCap,
  MonitorSmartphone,
  Rocket,
  ScrollText,
  Send,
  Settings as SettingsIcon,
  Sparkles,
  Trash2,
  Users,
} from "lucide-react";

import { AskSurface } from "../ask/AskSurface";
import { useAsk } from "../ask/AskProvider";
import type { OsAppManifest, OsRegistry, OsWidgetManifest } from "../system/registry";
import { AccountsApp } from "./accounts/AccountsApp";
import { ACCOUNTS_SECTIONS } from "./accounts/settings";
import { BinApp } from "./bin/BinApp";
import { BIN_APP_ID, BIN_SECTIONS } from "./bin/concepts";
import { CampaignsApp } from "./campaigns/CampaignsApp";
import { CAMPAIGNS_SECTIONS } from "./campaigns/settings";
import { DeployablesApp } from "./deployables/DeployablesApp";
import { DEPLOYABLES_SECTIONS } from "./deployables/settings";
import { FilesApp } from "./files/FilesApp";
import { FILES_SECTIONS } from "./files/settings";
import { FleetApp } from "./fleet/FleetApp";
import { FLEET_SECTIONS } from "./fleet/settings";
import { LogsApp } from "./logs/LogsApp";
import { LOGS_SECTIONS } from "./logs/settings";
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
    { id: "ask", name: "Ask" },
    { id: "apps", name: "Apps" },
    // Widened by the ladder flip (epic memql#4832, D1): under the shell's
    // old ordering this admitted {admin, owner}, and it now admits
    // {admin, developer, owner}. Correct in the new direction -- cluster
    // diagnostics is engineering surface, and developer is the engineering
    // tier -- so this is a line the flip fixes rather than one it endangers.
    { id: "cluster", name: "Cluster", roles: { min: "admin" } },
    { id: "diagnostics", name: "Diagnostics" },
    // OWNER OR DEVELOPER, AND EXPLICITLY NOT ADMIN (program decision P6).
    // This is the first section in the shell whose gate a ladder MINIMUM
    // cannot express: `{ min: "developer" }` would admit admin, and the
    // decision is that configuring an integration is a developer's job and an
    // owner's prerogative rather than a rank. `roleAdmits`' `any` form is what
    // says exactly that, and it is presentation over a gate the status
    // capability's own `statusAuthorized` remains the authority on.
    { id: "integrations", name: "Integrations", roles: { any: ["owner", "developer"] } },
    // The shell's own lines (epic memql#4895): what the OS front end
    // recorded under no app, plus everything the Settings surfaces logged.
    // Last rather than before a settings section, because this app HAS no
    // settings section -- every section here is one.
    { id: "logs", name: "Logs", roles: { min: "admin" } },
  ],
  settingsSection: "appearance",
  logsSection: "logs",
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
  logsSection: "logs",
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
// NO SECTION CARRIES A ROLE, and that is the point: `v1:platform:site`
// declares the composite tier (`@rowAuthz(owner="ownerUserId",
// clusterOwner)`), so every signed-in person has deployables of their own to
// read and there is nothing to gate -- the engine decides how far the list
// reaches. The write half is gated INSIDE the one section instead (New
// deployable at rank >= 200, epic memql#4885), and that gate is PRESENTATION
// (spec section E) over writes the Go hostname policy and
// `sitePublishFromArtifact` remain the authority on.
const deployables: OsAppManifest = {
  id: "deployables",
  name: "Deployables",
  icon: Rocket,
  sections: DEPLOYABLES_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
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
  logsSection: "logs",
  component: FleetApp,
};

// Logs, in full (epic memql#4895 / #4897). ONE app over everything every
// node wrote: Stream is first and is therefore the section a window opens
// on -- the store, following -- and Search is the same store asked about a
// window. The app's own settings can point a window at Search instead.
//
// The section list is LOGS_SECTIONS rather than a literal, for the reason
// its siblings are: the settings picker offers an "open Logs on" choice over
// exactly these ids, and a second copy of the list is one that can disagree.
//
// `roles: { min: "admin" }` is PRESENTATION over a floor the ENGINE enforces
// (spec L3): every read on the log store is admin-and-above in the Go
// handler, and `logsSweep` and `logsArchiveRestore` are owner-only there.
// Rank >= 200 under the one ladder is {admin, developer, owner}, which is
// the set the engine admits. Because the floor is on the app, no section
// carries one of its own, and `logsSection` names the Stream -- the one app
// whose Logs section is not called "Logs", because the whole app is.
const logs: OsAppManifest = {
  id: "logs",
  name: "Logs",
  icon: ScrollText,
  roles: { min: "admin" },
  sections: LOGS_SECTIONS,
  settingsSection: "settings",
  logsSection: "stream",
  component: LogsApp,
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
// specs, `adminops`' two gates and row admission remain the authority on every
// read and write this app makes.
//
// A FLOOR, AND IT HAS BEEN WRONG TWICE IN OPPOSITE DIRECTIONS, so the
// reasoning is worth keeping.
//
// It was written when the shell ranked admin ABOVE developer, so it meant
// {admin, owner}. Epic memql#4832 made the engine's ordering the only one and
// developer sits at 300 above admin's 200, so the same line silently came to
// mean {admin, developer, owner}. At that moment it was WRONG: every gate
// inside was `auth.AtLeastAdmin`, the create-on-principal capability, which
// developer does not hold. A developer was offered the app, served two empty
// lists, and pointed at an Invite form that answered PERMISSION_DENIED. The
// repair was briefly to state the set `{ any: ["admin", "owner"] }`.
//
// Then the owner decided the premise was wrong for this cluster (memql#4917):
// a developer helping an owner stand a cluster up SHOULD be able to invite
// people. Developer gained a capability of its own, create-on-admission, which
// covers user invitations and enrolment links and nothing else.
//
// That makes the admitted set {admin, developer, owner} = rank >= 200: a
// CONTIGUOUS TOP, and therefore a floor. roles.ts states the rule -- a set
// that is really a contiguous top is a `min` written the long way, and it
// silently stops admitting whatever rung is added above it. The set form is
// still right for Settings > Integrations, which leaves a rung out of the
// MIDDLE.
//
// A DEVELOPER DOES NOT GET THE WHOLE APP, and this floor is not claiming
// otherwise. They read the people list, issue and revoke invitations, and mint
// enrolment links. Changing a role and re-enabling sign-in links stay
// owner/admin and are not rendered for them -- see PersonDetail.
const users: OsAppManifest = {
  id: "users",
  name: "Users",
  icon: Users,
  roles: { min: "admin" },
  sections: USERS_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: UsersApp,
};

// Training, in full (epic #4737), re-keyed to the Library in epic memql#4970.
// Teach is first and is therefore the section a window opens on: this app is
// for teaching MemQL from files, and that section is where a file becomes
// something a domain has learned. The app's own settings can point it
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
// the same populations -- there is no line inside the app where the answer
// changes. The engine remains the authority, and the re-key TIGHTENED what
// that means: `v1:library:file` and `v1:work:run` both declare the composite
// owner tier, so row admission gates the subscriptions as well as the reads
// and nobody else's rows reach this browser at all -- which the plan feed
// this replaced could not say. `libraryTrainFile` re-reads the file under the
// caller's own actor and checks the domain write authorizer before it ingests
// anything. `setChunkValidationStatus` remains admitted for any authenticated
// caller because `v1:knowledge:documentChunk` declares no tier (the standing
// residual its sibling mutations already sit on, recorded in the
// per-row-authz audit).
const training: OsAppManifest = {
  id: "training",
  name: "Training",
  icon: GraduationCap,
  roles: { min: "writer" },
  sections: TRAINING_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
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
// `roles: { min: "admin" }` -- AND IT IS A MIRROR, NOT THE GATE (epic
// memql#4832 D6, memql#4837).
//
// This manifest carried NO role until now, and the reasoning was sound at the
// time: gating here would have been "presentation pretending to be
// authorization", because a launcher filter is the only thing this file can
// do and its own predicate says so.
//
// What changed is that there is now something real to mirror. The accounts
// constructs declare `@requiresRank("admin")`, which the ENGINE enforces, so
// a person below that rank is refused server-side whether or not this line
// exists. Both halves are permanent and neither stands in for the other:
// hiding an app somebody cannot reach beats letting them open it and read a
// refusal, and that is a different job from refusing it.
//
// `admin` is rank >= 200 = {admin, developer, owner}, which is the set
// memql#4837 spells out. The issue's title says "developer-and-above" and
// means the same set -- that was the OS ladder's way of saying it, back when
// developer sat BELOW admin. `min: "developer"` would read like the issue and
// lock out every admin.
//
// `TestAppManifestMirrorsTheEngineFloor` (component/auth) fails the build if
// this value and the DSL floor ever disagree, which is the whole point of
// calling it a mirror.
//
// NOT always-docked. That is the Bin's distinction (#4784); this is an
// ordinary app that opens from the launcher like every other one.
const accounts: OsAppManifest = {
  id: "accounts",
  name: "Accounts",
  icon: Building2,
  roles: { min: "admin" },
  sections: ACCOUNTS_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: AccountsApp,
};

// Campaigns, in full (epic memql#4827 / #4828 / #4830). Writing mail, sending
// it, and knowing what happened. Campaigns is first and is therefore the
// section a window opens on: a campaign is what this app is for, and the other
// four are the things a campaign is made of.
//
// The section list is CAMPAIGNS_SECTIONS rather than a literal, for the reason
// its six siblings are: the gear and the manifest must offer the same set, and
// a second copy of the list is one that can disagree.
//
// NO manifest role. Every operator-facing campaigns concept declares the
// composite tier (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every
// signed-in person has campaigns of their own to read and the engine decides
// how far each list reaches. Gating here would be presentation pretending to
// be authorization -- and the writes that DO carry a gate carry it in the
// engine: the send builtins authorize by reading the campaign under the
// caller's own actor before any preflight runs.
const campaigns: OsAppManifest = {
  id: "campaigns",
  name: "Campaigns",
  icon: Send,
  sections: CAMPAIGNS_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: CampaignsApp,
};

// The Bin, in full (memql#4784). ALWAYS DOCKED, which is the whole distinction
// its sibling manifests point at: `dockFixture` puts it in the dock in every
// session and keeps it out of the pin list, so it cannot be unpinned, dragged
// out of the strip, or lost to a desktop document written before it existed.
//
// The reason is the gesture rather than the app. Archiving has a destination
// now -- you drag a file onto it, exactly as on every desktop anybody has used
// -- and a destination that a person can remove is one that stops being there
// on the day they need it. Ordinary apps stay pinnable; this is the one
// fixture, and the flag exists for it rather than as a capability.
//
// NO manifest role. `v1:library:artifact` and `v1:library:folder` declare the
// composite tier, so every signed-in person has a Bin of their own and the
// engine decides how far its reads go. Gating here would be presentation
// pretending to be authorization.
const bin: OsAppManifest = {
  id: BIN_APP_ID,
  name: "Bin",
  icon: Trash2,
  sections: BIN_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  dockFixture: true,
  component: BinApp,
};

function AskWidgetBody() {
  const { transport, voice, settings } = useAsk();
  return (
    <AskSurface transport={transport} voicePorts={voice} settings={settings} variant="widget" />
  );
}

const askWidget: OsWidgetManifest = {
  id: "ask",
  name: "Ask",
  icon: Sparkles,
  size: { w: 3, h: 2 },
  component: AskWidgetBody,
};

export const OS_REGISTRY: OsRegistry = {
  apps: [accounts, campaigns, files, deployables, fleet, logs, users, training, settings, bin],
  widgets: [askWidget],
};
