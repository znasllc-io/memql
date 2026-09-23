import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  OsConnectionProvider: ({ children }: { children: unknown }) => children,
  osBridgePath: "/_memql/ws",
  bridgePathFor: () => "/_memql/ws",
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { bare, deployedByLabel, deployerOf, namesFrom } from "../../src/apps/deployables/people";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// WHO DEPLOYED WHAT (epic memql#5289, task memql#5306; design section 4
// "Attribution"). The fact is on the rows the list and the page already
// read; only the name is looked up, and only where the roster is readable.
// Two people share the cluster the owner asked this for -- the owner and a
// developer, both able to read the roster -- so the names resolve for them;
// a reader's rows keep their ownership chip and say nothing more.

const ADA: Row = { id: "u-ada", displayName: "Ada Lovelace", primaryEmail: "ada@example.com", role: "developer", active: true } as Row;
const ME: Row = { id: "u-me", displayName: "", primaryEmail: "owner@example.com", role: "owner", active: true } as Row;

const MINE = siteRow({ id: "site-mine", hostname: "mine.memql.example.com", ownerUserId: "u-me" });
const ADAS = siteRow({ id: "site-ada", hostname: "ada.memql.example.com", ownerUserId: "u-ada" });

const PACKAGE: Row = {
  id: "pkg-acme",
  ownerUserId: "u-ada",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "",
  latestKnownVersion: "",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

function memStore() {
  const data = new Map<string, string>();
  // The source group is seeded OPEN, as deployables.test.tsx seeds it: these
  // cases are about what a row says, not about the disclosure.
  data.set("memql-os-deployables-v1", JSON.stringify({ version: 1, density: "comfortable", expandedSources: ["pkg:pkg-acme"] }));
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection, opts: { role?: string; section?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId={opts.section ?? "deployables"} navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

async function live(): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
}

/** The list row carrying `hostname`, as the element its attribution sits in. */
async function rowOf(hostname: string): Promise<HTMLElement> {
  await live();
  const text = await screen.findByText(hostname);
  const row = text.closest("li") ?? text.closest("[class*=os-row]") ?? text.parentElement?.parentElement;
  if (row === null || row === undefined) throw new Error(`no row for ${hostname}`);
  return row as HTMLElement;
}

async function openPage(hostname: string): Promise<HTMLElement> {
  await live();
  await click((await screen.findByText(hostname)).closest("button"));
  return screen.findByRole("region", { name: `Deployable ${hostname}` });
}

beforeEach(() => {
  h.connection = null;
});

describe("the pure half", () => {
  it("reads the deployer off the run first, then the site, then the source", () => {
    const run = { requestedBy: "v1:identity:user:u-run" } as never;
    const site = { ownerUserId: "u-site" } as never;
    const pkg = { ownerUserId: "u-pkg" } as never;
    expect(deployerOf(run, site, pkg)).toBe("u-run");
    expect(deployerOf(null, site, pkg)).toBe("u-site");
    expect(deployerOf(null, null, pkg)).toBe("u-pkg");
    expect(deployerOf(null, null, null)).toBe("");
    // A cluster-owned site (empty owner) falls through to the source.
    expect(deployerOf(null, { ownerUserId: "" } as never, pkg)).toBe("u-pkg");
  });

  it("says 'you' for the viewer, the name the roster gave, and nothing otherwise", () => {
    const names = namesFrom([ADA, ME]);
    const nameOf = (id: string) => names.get(id) ?? "";
    expect(deployedByLabel("u-me", "u-me", nameOf)).toBe("you");
    expect(deployedByLabel("v1:identity:user:u-me", "u-me", nameOf)).toBe("you");
    expect(deployedByLabel("u-ada", "u-me", nameOf)).toBe("Ada Lovelace");
    // No display name: the email is the name.
    expect(deployedByLabel("u-me", "u-other", nameOf)).toBe("owner@example.com");
    // Nobody the roster knows: nothing -- never the id.
    expect(deployedByLabel("u-ghost", "u-me", nameOf)).toBe("");
    expect(deployedByLabel("", "u-me", nameOf)).toBe("");
    expect(bare("v1:identity:user:u-1")).toBe("u-1");
  });
});

describe("the list row", () => {
  it("says 'deployed by you' on the viewer's own row and names the colleague on theirs", async () => {
    const seed: FakeSeed = { sites: [MINE, ADAS], people: [ADA, ME] };
    mount(fakeConnection(seed));
    await waitFor(async () => {
      expect(within(await rowOf("mine.memql.example.com")).getByText("deployed by you")).toBeTruthy();
    });
    expect(within(await rowOf("ada.memql.example.com")).getByText("deployed by Ada Lovelace")).toBeTruthy();
  });

  it("says nothing on a colleague's row when the roster is not readable -- never an id", async () => {
    const seed: FakeSeed = { sites: [MINE, ADAS], peopleError: "requires the \"developer\" role or above" };
    mount(fakeConnection(seed), { role: "reader" });
    await live();
    expect(within(await rowOf("mine.memql.example.com")).getByText("deployed by you")).toBeTruthy();
    const theirs = await rowOf("ada.memql.example.com");
    expect(within(theirs).queryByText(/deployed by/)).toBeNull();
    expect(theirs.textContent).not.toContain("u-ada");
  });
});

describe("the deployable's page and the source's page", () => {
  it("carries the attribution beside the ownership chip, the run's requester outranking the site's owner", async () => {
    // Ada's site, but the newest run was requested by the viewer: "you".
    const run: Row = {
      id: "dep-1",
      packageId: "pkg-acme",
      status: "succeeded",
      requestedBy: "u-me",
      createdAt: "2026-09-02T10:00:00Z",
    } as Row;
    const site = siteRow({
      id: "site-store",
      hostname: "store.memql.example.com",
      ownerUserId: "u-ada",
      packageId: "pkg-acme",
      packageDeployableName: "storefront",
    });
    const seed: FakeSeed = { sites: [site], packages: [PACKAGE], deployments: { "pkg-acme": [run] }, people: [ADA, ME] };
    mount(fakeConnection(seed));
    const page = await openPage("store.memql.example.com");
    await waitFor(() => {
      expect(within(page).getByText("deployed by you")).toBeTruthy();
    });
  });

  it("names the deployer on the page and on the source's page from the rows already read", async () => {
    const site = siteRow({
      id: "site-store",
      hostname: "store.memql.example.com",
      ownerUserId: "u-ada",
      packageId: "pkg-acme",
      packageDeployableName: "storefront",
    });
    const seed: FakeSeed = { sites: [site], packages: [PACKAGE], people: [ADA, ME] };
    mount(fakeConnection(seed));
    const page = await openPage("store.memql.example.com");
    await waitFor(() => {
      expect(within(page).getByText("deployed by Ada Lovelace")).toBeTruthy();
    });
    // The source's page: the Fact reads the source's owner, there being no run.
    const line = within(page)
      .getAllByRole("button")
      .find((b) => b.classList.contains("os-rail-line") && (b.textContent ?? "").startsWith("Source"));
    if (line !== undefined && line.getAttribute("aria-expanded") !== "true") await click(line);
    await click(within(page).getByRole("button", { name: /^Open / }));
    const source = await screen.findByRole("region", { name: /^Repository / });
    expect(within(source).getByText("Deployed by")).toBeTruthy();
    expect(within(source).getByText("Ada Lovelace")).toBeTruthy();
  });
});
