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
import { SettingsApp } from "./settings/SettingsApp";
import { StubApp } from "./StubApp";

// The installed roster (spec D12): Settings is real; the five product apps
// are honest stubs replaced by their epics; the Ask widget is the widget
// framework's first resident. Static by design -- runtime app delivery is
// deliberately not a foundation question.

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
    { id: "cluster", name: "Cluster", roles: { min: "admin" } },
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

const fleet = stub(
  {
    id: "fleet",
    name: "Fleet",
    icon: MonitorSmartphone,
    sections: [
      { id: "machines", name: "Machines" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
  },
  4729,
  "The machines running MemQL Cockpit for you: presence, labels, routing -- live.",
);

const users = stub(
  {
    id: "users",
    name: "Users",
    icon: Users,
    roles: { min: "admin" },
    sections: [
      { id: "people", name: "People" },
      { id: "invites", name: "Invites" },
      { id: "settings", name: "Settings" },
    ],
    settingsSection: "settings",
  },
  4733,
  "The people of this cluster: roles, invitations, and a list that updates the moment someone accepts.",
);

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
