import { fireEvent, render, screen, within } from "@testing-library/react";
import { vi } from "vitest";

import { Shell } from "../../src/chrome/Shell";
import { StubAskTransport } from "../../src/ask/askController";
import { LocalDesktopStore } from "../../src/system/store";
import type { OsRuntimeConfig } from "../../src/cluster/config";

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

export const OWNER = { userId: "u-1", primaryEmail: "owner@example.com", clusterRole: "owner" };
export const ADMIN = { userId: "u-3", primaryEmail: "admin@example.com", clusterRole: "admin" };
export const READER = { userId: "u-2", primaryEmail: "reader@example.com", clusterRole: "reader" };

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
    within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name }),
  );
}

/** Click a section button in a window's own nav. */
export function gotoSection(app: string, section: string) {
  const nav = screen.getByRole("navigation", { name: `${app} sections` });
  fireEvent.click(within(nav).getByRole("button", { name: section }));
}
