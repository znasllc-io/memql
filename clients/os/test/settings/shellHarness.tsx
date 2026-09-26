import { fireEvent, render, screen, within } from "@testing-library/react";
import { vi } from "vitest";

import { Shell } from "../../src/chrome/Shell";
import { StubAskTransport } from "../ask/stubTransport";
import { LocalDesktopStore } from "../../src/system/store";
import type { OsRuntimeConfig } from "../../src/cluster/config";
import { appTileName } from "../appTile";
import { installSeededAccess } from "../seededAccess";

// The Settings suite renders the REAL shell against the REAL registry, for
// the reason the apps index exists: what it does -- open by id, focus an
// existing window, navigate that window to a section -- is shell behaviour,
// and a test that mocked the shell would assert only that the component
// called the functions it calls.

export const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.com",
};

export const OWNER = { userId: "u-1", primaryEmail: "owner@example.com", role: "owner", roleName: "", rank: 0 };
export const ADMIN = { userId: "u-3", primaryEmail: "admin@example.com", role: "admin", roleName: "", rank: 0 };
export const READER = { userId: "u-2", primaryEmail: "reader@example.com", role: "reader", roleName: "", rank: 0 };

export function memStorage(): Pick<Storage, "getItem" | "setItem"> & { dump: () => string } {
  const data = new Map<string, string>();
  return {
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
    dump: () => JSON.stringify([...data.entries()]),
  };
}

export function renderShell({
  access = OWNER,
  storage = memStorage(),
}: { access?: typeof OWNER; storage?: ReturnType<typeof memStorage> } = {}) {
  // THE EFFECTIVE SET FOLLOWS THE ROLE THE HARNESS SIGNS IN AS (epic
  // memql#5289). The shell reads it from the cluster; with the connection
  // disabled nothing does, so the harness installs the role's seeded set --
  // what a cluster with no grants resolves for that person -- before the
  // first render, and a suite that signs in as a reader measures a reader.
  installSeededAccess(access.role);
  render(
    <Shell
      layout="desktop"
      onSignOut={vi.fn()}
      access={access}
      config={CONFIG}
      ports={{
        store: new LocalDesktopStore(storage),
        disableConnection: true,
        askTransport: new StubAskTransport(), askVoice: null,
      }}
    />,
  );
  return { storage };
}

export function openFromLauncher(name: string) {
  fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
  fireEvent.click(
    within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name: appTileName(name) }),
  );
}

/** Click a section button in a window's own nav. */
export function gotoSection(app: string, section: string) {
  if (section === "Logs") { fireEvent.click(screen.getByRole("button", { name: `${app} logs` })); return; }
  if (section === "Settings") { fireEvent.click(screen.getByRole("button", { name: new RegExp(`^${app} settings`) })); return; }
  const nav = screen.getByRole("navigation", { name: `${app} sections` });
  fireEvent.click(within(nav).getByRole("button", { name: section }));
}
