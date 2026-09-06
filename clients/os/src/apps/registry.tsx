import {
  Boxes,
  Building2,
  Files as FilesIcon,
  GraduationCap,
  Layers,
  MonitorSmartphone,
  Rocket,
  ScrollText,
  Send,
  Settings as SettingsIcon,
  Shapes,
  Store,
  Sparkles,
  Trash2,
  Users,
  Waypoints,
} from "lucide-react";

import { AskSurface } from "../ask/AskSurface";
import { useAsk } from "../ask/AskProvider";
import { useMakeGoal } from "../ask/useMakeGoal";
import type { OsAppManifest, OsRegistry, OsWidgetManifest } from "../system/registry";
import { AccountsApp } from "./accounts/AccountsApp";
import { ACCOUNTS_SECTIONS } from "./accounts/settings";
import { BinApp } from "./bin/BinApp";
import { BIN_APP_ID, BIN_SECTIONS } from "./bin/concepts";
import { CampaignsApp } from "./campaigns/CampaignsApp";
import { ClusterApp } from "./cluster/ClusterApp";
import { CLUSTER_SECTIONS } from "./cluster/settings";
import { ConceptsApp } from "./concepts/ConceptsApp";
import { CONCEPTS_SECTIONS } from "./concepts/settings";
import { CAMPAIGNS_SECTIONS } from "./campaigns/settings";
import { DeployablesApp } from "./deployables/DeployablesApp";
import { DEPLOYABLES_SECTIONS } from "./deployables/settings";
import { FilesApp } from "./files/FilesApp";
import { FILES_SECTIONS } from "./files/settings";
import { FleetApp } from "./fleet/FleetApp";
import { FLEET_SECTIONS } from "./fleet/settings";
import { LogsApp } from "./logs/LogsApp";
import { LOGS_SECTIONS } from "./logs/settings";
import { MaterializerApp } from "./materializer/MaterializerApp";
import { MATERIALIZER_SECTIONS } from "./materializer/settings";
import { SettingsApp } from "./settings/SettingsApp";
import { StoresApp } from "./stores/StoresApp";
import { STORES_SECTIONS } from "./stores/settings";
import { TrainingApp } from "./training/TrainingApp";
import { TRAINING_SECTIONS } from "./training/settings";
import { UsersApp } from "./users/UsersApp";
import { USERS_SECTIONS } from "./users/settings";
import { NexusApp } from "./nexus/NexusApp";
import { NEXUS_SECTIONS } from "./nexus/settings";

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
    // Benchmarks (epic memql#4993). BESIDE Diagnostics rather than inside it:
    // Diagnostics is three panels about THIS SESSION, and folding a fact about
    // the deployment across releases into it would change what its "copy
    // diagnostics" button means.
    //
    // { min: "admin" } matches Cluster and Logs, and it is presentation over a
    // gate the engine already holds: v1:bench:run and v1:bench:sample declare
    // @rowAuthz(clusterOwner), so a reader below the floor would see an empty
    // section with no explanation rather than a refusal.
    { id: "benchmarks", name: "Benchmarks", roles: { min: "admin" } },
    // OWNER OR DEVELOPER, AND EXPLICITLY NOT ADMIN (program decision P6).
    // This is the first section in the shell whose gate a ladder MINIMUM
    // cannot express: `{ min: "developer" }` would admit admin, and the
    // decision is that configuring an integration is a developer's job and an
    // owner's prerogative rather than a rank. `roleAdmits`' `any` form is what
    // says exactly that, and it is presentation over a gate the status
    // capability's own `statusAuthorized` remains the authority on.
    { id: "integrations", name: "Integrations", roles: { any: ["owner", "developer"] } },
    // The three that arrived when the portal's admin console was retired
    // (epic memql#4984). Each floor is the one the ENGINE will actually
    // apply, not a rounder number: `providerAuthStatus` and the two
    // provider writes are owner-gated, so offering Providers to an admin
    // would be a section whose every control answers with a refusal, while
    // the Tokens reads and the revokes are owner-or-admin and Keys reads a
    // PUBLIC feed that needs no role at all -- floored at admin because
    // knowing which keys a cluster signs with is operator business.
    { id: "providers", name: "AI providers", roles: { min: "owner" } },
    { id: "tokens", name: "Tokens", roles: { min: "admin" } },
    { id: "keys", name: "Keys", roles: { min: "admin" } },
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

// Cluster, in full (epic memql#5009 / memql#5011). What this cluster is made
// of and how it is going: its modules, where its data comes from, who acts in
// it, what happened, and whether it is ready.
//
// SETTINGS IS WHAT YOU SET; THIS IS WHAT THE CLUSTER IS. That is the split
// against Settings' own Cluster section, which holds policy and this
// session's diagnostics. Two surfaces may share a name where they are the
// same subject at two scopes -- the precedent is the Logs app and the Logs
// section every other app carries.
//
// Readiness is first and is therefore the section a window opens on: for a
// healthy cluster it says so in a line and gets out of the way, and for a new
// one it is the thing somebody came to find out. The app's own settings can
// point a window elsewhere.
//
// THE SECTION FLOORS ARE MIRRORS OF ENGINE GATES, ONE BY ONE, and two of them
// correct the issue that asked for this app:
//
//   Modules       {owner, admin}   AuthorizeModuleRead -> auth.AtLeastAdmin
//                                  -> roleHasCapability("create","principal"),
//                                  which rbac_model.go withholds from
//                                  developer. Under the one ladder developer
//                                  ranks 300, ABOVE admin's 200, so
//                                  { min: "admin" } would admit exactly the
//                                  role the engine refuses -- the second
//                                  genuinely non-monotonic gate in the shell
//                                  after Settings -> Integrations.
//   Data origins  owner            syncStatesAll filters
//                                  actor.isClusterOwner==true, and
//                                  v1:platform:syncState declares
//                                  @rowAuthz(clusterOwner).
//   Audit trail   owner            AND IT IS THE ONLY MECHANISM THAT CAN STOP
//                                  THIS SURFACE LYING. Row admission returns
//                                  ZERO ROWS, not an error, so a non-owner
//                                  calling recentAuditEvents gets an empty
//                                  list indistinguishable from a cluster
//                                  where nothing happened -- there is no
//                                  refusal for the surface to render. The
//                                  portal shipped this list with no
//                                  client-side gate at all.
//   Agents        the app's        v1:agents:agent declares NO row-authz tier,
//                 floor            so its named queries return every agent in
//                                  the cluster to any authenticated caller.
//                                  This floor is therefore EDITORIAL, not a
//                                  mirror; it is presentation over an ungated
//                                  read and the section says so rather than
//                                  implying a gate that is not there.
//
// The app floor is { min: "admin" } = {admin, developer, owner}, matching
// Users, Accounts and Logs.
//
// THE ICON IS A SET OF BOXES, which is what this app inventories: the parts
// a cluster is assembled from.
const cluster: OsAppManifest = {
  id: "cluster",
  name: "Cluster",
  icon: Boxes,
  roles: { min: "admin" },
  sections: CLUSTER_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: ClusterApp,
};

// Concepts, in full (epic memql#5009 / memql#5010). Every kind of thing this
// cluster knows, what each one declares, and the rows it holds. Registry is
// first and is therefore the section a window opens on; the app's own
// settings can point it elsewhere.
//
// The section list is CONCEPTS_SECTIONS rather than a literal, for the
// reason its siblings are: the gear and the manifest must offer the same
// set, and a second copy of the list is one that can disagree.
//
// THE CONCEPT-AGNOSTIC SURFACE. It is the one place a person reaches a
// concept nobody built a screen for, which is why the VS Code extension's
// "browse rows" handoff lands here: the portal answered `/concepts/:id` and
// nothing did after it was retired.
//
// `roles: { min: "admin" }` -- rank >= 200 = {admin, developer, owner} -- and
// IT IS AN EDITORIAL FLOOR RATHER THAN A MIRROR, which is the unusual part
// and the reason it is written down.
//
// There is no engine gate to mirror. `ConceptsListMsg` and its follow
// subscription carry NO authorization check at all: the schema catalog is
// published to any connected session, deliberately, because a declaration is
// not data. And the rows a concept holds are decided by that concept's own
// `@rowAuthz` tier through row admission, which this app cannot widen and
// does not try to -- `browseConceptPage` is an unbound read and gets exactly
// what the caller's other reads would.
//
// So the floor is a decision about who this app is FOR, and about one thing
// it would otherwise change in practice: the standing long tail of concepts
// that declare no tier at all is readable by everyone TODAY through their
// own named queries, and a generic browser makes that long tail trivially
// DISCOVERABLE rather than merely reachable. Those are not the same fact.
// Hiding the app from a reader does not gate anything -- the engine remains
// the only authority -- and this comment is here so the next reader does not
// mistake it for one.
//
// THE ICON IS A SET OF SHAPES, which is what a concept registry is: the
// kinds of thing, not the things.
const concepts: OsAppManifest = {
  id: "concepts",
  name: "Concepts",
  icon: Shapes,
  roles: { min: "admin" },
  sections: CONCEPTS_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: ConceptsApp,
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

// Nexus, in full (epic memql#4785, sub-project B of the work spine). THE GOAL
// SURFACE: what you asked the system to do, drawn as a place the work arrives
// at -- and the places it had to stop and ask you.
//
// IT REPLACES THE WORK APP RATHER THAN SITTING BESIDE IT (design record D1,
// owner-decided 2026-09-05). Sub-project A shipped a Work app over the same
// `v1:work:*` rows; two apps both listing goals and both holding an approvals
// queue is the shape ten manifests in this file write down against, and here
// it would be worse than a stale label -- an approvals inbox two windows
// disagree about is a run somebody thinks they unparked. Everything the Work
// app earned is kept; what changed is that the GOAL, rather than the run, is
// what the app is about.
//
// GOALS IS FIRST and is therefore the section a window opens on: a goal is what
// this app is for, and runs, automations and approvals are all things a goal
// produced. The app's own settings can point a window at Approvals instead,
// which is the choice somebody who lives in this app all day makes -- a run
// parked on a question does not move until a person answers it.
//
// The section list is NEXUS_SECTIONS rather than a literal, for the reason its
// ten siblings are: the gear and the manifest must offer the same set, and a
// second copy of the list is one that can disagree.
//
// NO MANIFEST ROLE. Every `v1:work:*` concept declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person
// has goals of their own to read and the engine decides how far each list
// reaches. Gating here would be presentation pretending to be authorization --
// the same reading Files, Deployables and Campaigns record. The writes carry
// their gate in the engine: each `integration.work.*` builtin repeats its own,
// because a builtin's annotation set carries no `@requiresRank`. The one floor
// in the app is on its Logs section, and that one IS a mirror: every read on
// the log store is admin-and-above in the Go handler.
//
// THE ICON IS A PATH WITH STOPS ON IT, which is what a run is and what this app
// draws: the road from you to the goal, thin between the steps that cost
// nothing and thick where the machine had to think.
const nexus: OsAppManifest = {
  id: "nexus",
  name: "Nexus",
  icon: Waypoints,
  sections: NEXUS_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: NexusApp,
};

// THE MATERIALIZER (epic memql#4977): where a person and the model compose
// data from the memory graph into a file.
//
// NO MANIFEST ROLE. Every `v1:compose:*` concept declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person
// has compositions of their own and the engine decides how far each list
// reaches. Gating here would be presentation pretending to be authorization --
// the reading Files, Deployables, Campaigns and Work all record. The writes
// carry their gate in the engine: each `integration.compose.*` builtin repeats
// its own, because a builtin's annotation set carries no `@requiresRank`.
//
// THE APP IS `materializer` WHILE THE NAMESPACE IS `compose`, and that is the
// epic's own split rather than a slip: `materializer` already names the
// engine's boot seeder, so the ROWS could not take the word -- but
// `v1:work:goal.requestedVia` has carried the string "materializer" for the
// surface since before this app existed.
//
// THE ICON IS STACKED PLANES, which is what a composition is: rows, a
// template and a format pressed into one file.
const materializer: OsAppManifest = {
  id: "materializer",
  name: "Materializer",
  icon: Layers,
  sections: MATERIALIZER_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: MaterializerApp,
};

// Stores, in full (epic memql#5009 / memql#5012). The Shopify connector's
// operator surface: every configured store with its health, one store's
// scopes, subscriptions and mirror sync state, and the two acts that are
// about a STORE.
//
// ITS OWN APP RATHER THAN A CLUSTER SECTION, because its subject is a live
// integration rather than the cluster itself -- the one deferred surface
// that is.
//
// `roles: { min: "owner" }` is PRESENTATION over gates the engine holds:
// every query in dsl/shopify/overlay/queries.memql filters
// `actor.isClusterOwner==true`, and `v1:shopify:store` declares
// `@rowAuthz(clusterOwner)`. Owner rather than admin, which is what the
// portal gated on too -- only the cluster owner may list, add or change a
// store.
//
// THE PER-DOMAIN ACTS ARE DELIBERATELY NOT HERE. Backfill, per-domain pause,
// retry and discard are the generic sync runtime (`datasyncStartBackfill`
// and friends) and they act on a (concept, connector) pair rather than on a
// store; they live in the Cluster app's Data origins section. What lives
// here is the store-wide pause and the Shopify-specific subscription
// reconcile. Two pages carrying the same three buttons is the duplication
// that split exists to avoid, and the store page names where the others are
// rather than leaving somebody to hunt.
const stores: OsAppManifest = {
  id: "stores",
  name: "Stores",
  icon: Store,
  roles: { min: "owner" },
  sections: STORES_SECTIONS,
  settingsSection: "settings",
  logsSection: "logs",
  component: StoresApp,
};

function AskWidgetBody() {
  const { transport, voice, settings } = useAsk();
  // The widget hands a prompt off exactly as the sheet does (epic memql#4785).
  // One Ask, three entry points, and an act that exists on one of them is an
  // act somebody learns and then cannot find.
  const makeGoal = useMakeGoal();
  return (
    <AskSurface
      transport={transport}
      voicePorts={voice}
      settings={settings}
      variant="widget"
      makeGoal={makeGoal}
    />
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
  apps: [
    accounts,
    campaigns,
    cluster,
    concepts,
    files,
    deployables,
    fleet,
    logs,
    materializer,
    users,
    training,
    nexus,
    settings,
    stores,
    bin,
  ],
  widgets: [askWidget],
};
