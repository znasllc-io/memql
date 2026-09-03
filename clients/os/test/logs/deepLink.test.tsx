import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { useOs } from "../../src/chrome/state";
import { resetIdsForTest } from "../../src/system/desks";
import { SHOP, click, fakeConnection, withSession, type FakeConnection } from "../deployables/harness";

// The deep links (epic memql#4895, spec H "Deep links"): Deployables' site
// and package details carry a quiet "Logs" action that opens the Logs app on
// Search with `{ subject, subjectConcept }`, and hide it below admin.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

/** What the shell holds, read off the provider the app really mounts under. */
function WindowsProbe() {
  const { state } = useOs();
  return <pre data-testid="windows">{JSON.stringify(Object.values(state.shell.windows))}</pre>;
}

function windows(): Array<{ appId: string; sectionId: string; intent?: { payload: Record<string, unknown> } }> {
  return JSON.parse(screen.getByTestId("windows").textContent ?? "[]");
}

const ACME: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  repoTokenRef: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

const DONE: Row = {
  id: "dep-1",
  packageId: "pkg-acme",
  ownerUserId: "u-me",
  status: "succeeded",
  sourceVersion: "aaaaaaaaaaaaaaaaaaaa",
  startedAt: "2026-09-01T10:05:00Z",
  createdAt: "2026-09-01T10:05:00Z",
  deployables: [],
  buildLogTail: "",
} as unknown as Row;

function mount(connection: FakeConnection, section: string, role = "owner") {
  h.connection = connection;
  return render(
    withSession(
      <>
        <DeployablesApp sectionId={section} navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />
        <WindowsProbe />
      </>,
      { role },
    ),
  );
}

beforeEach(() => {
  resetIdsForTest();
  h.connection = null;
});

describe("the deployable's Logs action", () => {
  it("opens the Logs app on Search carrying the site as the subject", async () => {
    mount(fakeConnection({ sites: [SHOP] }), "sites");
    await click(await screen.findByText("shop.memql.example.com"));
    await click(screen.getByRole("button", { name: "Logs for shop.memql.example.com" }));

    const opened = windows();
    expect(opened).toHaveLength(1);
    expect(opened[0]).toMatchObject({ appId: "logs", sectionId: "search" });
    expect(opened[0]?.intent?.payload).toEqual({ subject: "site-shop", subjectConcept: "v1:platform:site" });
  });

  it("focuses an already-open Logs window rather than opening a second", async () => {
    mount(fakeConnection({ sites: [SHOP] }), "sites");
    await click(await screen.findByText("shop.memql.example.com"));
    await click(screen.getByRole("button", { name: "Logs for shop.memql.example.com" }));
    await click(screen.getByRole("button", { name: "Logs for shop.memql.example.com" }));
    expect(windows().filter((w) => w.appId === "logs")).toHaveLength(1);
  });

  it("is absent below admin -- a writer sees the detail and no Logs action", async () => {
    mount(fakeConnection({ sites: [SHOP] }), "sites", "writer");
    await click(await screen.findByText("shop.memql.example.com"));
    expect(screen.queryByRole("button", { name: /^Logs for / })).toBeNull();
    expect(windows()).toHaveLength(0);
  });
});

describe("the deployment's Logs action", () => {
  it("opens the Logs app on Search carrying the deployment as the subject", async () => {
    mount(fakeConnection({ packages: [ACME], deployments: { "pkg-acme": [DONE] } }), "packages", "admin");
    await click(await screen.findByText("acme"));
    const attempts = await screen.findByRole("list", { name: "Deployments of acme" });
    await click(within(attempts).getByRole("button", { name: /^Logs of the .* deploy$/ }));

    const opened = windows();
    expect(opened[0]).toMatchObject({ appId: "logs", sectionId: "search" });
    expect(opened[0]?.intent?.payload).toEqual({
      subject: "dep-1",
      subjectConcept: "v1:platform:packageDeployment",
    });
  });
});
