import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { PACKAGE_CONCEPT, packageFingerprint, packageFromRow } from "../../src/apps/deployables/packages/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, emit, fakeConnection, type as typeInto, withSession, type FakeConnection } from "./harness";

// The Packages section, end to end through the real LiveCollection.
//
// Everything here goes through `connection.query` and `connection.subscriptions`
// exactly as production does, so the real retain/seed path, the real
// projections and the real generated builders all run -- which is the whole
// reason this harness answers at `executeNamed` rather than stubbing the
// generated methods.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

const ACME: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
} as unknown as Row;

const RETIRED: Row = { ...(ACME as object), id: "pkg-old", name: "retired", status: "archived" } as unknown as Row;

function mount(connection: FakeConnection | null, opts: { role?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId="packages"
        navigate={() => {}}
        askContext={() => {}}
        store={memStore()}
      />,
      { role: opts.role ?? "admin" },
    ),
  );
}

beforeEach(() => {
  h.connection = null;
});

describe("the packages list", () => {
  it("renders what the cluster returned", async () => {
    const connection = fakeConnection({ packages: [ACME] });
    mount(connection);
    expect(await screen.findByText("acme")).toBeTruthy();
    // The source, in the words somebody added it with.
    expect(screen.getByText(/acme\/storefront/)).toBeTruthy();
  });

  it("a package created while watching arrives without a reload", async () => {
    const connection = fakeConnection({ packages: [ACME] });
    mount(connection);
    await screen.findByText("acme");

    await emit(connection, PACKAGE_CONCEPT, { ...(ACME as object), id: "pkg-new", name: "beta" } as unknown as Row, "NODE_CREATED");
    expect(await screen.findByText("beta")).toBeTruthy();
  });

  it("an updateAvailable flip lights the update cue live", async () => {
    const connection = fakeConnection({ packages: [ACME] });
    mount(connection);
    await screen.findByText("acme");
    expect(screen.queryByText("update")).toBeNull();

    await emit(connection, PACKAGE_CONCEPT, {
      ...(ACME as object),
      latestKnownVersion: "bbbbbbbbbbbbbbbbbbbb",
      updateAvailable: true,
    } as unknown as Row);

    expect(await screen.findByText("update")).toBeTruthy();
  });

  it("the Archived filter reveals archived packages, and the default list excludes them", async () => {
    const connection = fakeConnection({ packages: [ACME, RETIRED] });
    mount(connection);
    await screen.findByText("acme");
    expect(screen.queryByText("retired")).toBeNull();

    await click(screen.getByRole("button", { name: /Show archived/ }));
    expect(await screen.findByText("retired")).toBeTruthy();
    // An archive is a PLACE: the active list is the one that is now hidden.
    expect(screen.queryByText("acme")).toBeNull();
  });

  it("says what to do when there is nothing yet", async () => {
    const connection = fakeConnection({ packages: [] });
    mount(connection);
    expect(await screen.findByText(/Add a repository or a zip/)).toBeTruthy();
  });
});

describe("the confirm gate", () => {
  const PARKED: Row = {
    id: "dep-1",
    packageId: "pkg-acme",
    sourceVersion: "cccccccccccccccccccc",
    status: "awaiting_confirm",
    report: {
      name: "acme",
      formatVersion: 1,
      deployables: [{ name: "storefront", kind: "spa", path: "clients/web", buildPlan: "npm ci && npm run build", output: "dist", prebuilt: false }],
      dslDomains: [],
      problems: [],
      ok: true,
    },
    dslVersion: "",
    deployables: [],
    error: null,
    requestedBy: "u-me",
    startedAt: "2026-09-01T12:00:00Z",
    finishedAt: "",
    createdAt: "2026-09-01T12:00:00Z",
  } as unknown as Row;

  it("blocks the deploy until it is confirmed, and shows the report first", async () => {
    const connection = fakeConnection({ packages: [ACME], deployments: { "pkg-acme": [PARKED] } });
    mount(connection);
    await click(await screen.findByText("acme"));

    // The report is on the page BEFORE anything can be confirmed. Scoped to
    // the Web apps section, because the deployable's name deliberately appears
    // twice on this surface -- once as what will be deployed and once as the
    // thing being given an address.
    const apps = (await screen.findByText("Web apps")).closest("section");
    expect(apps).toBeTruthy();
    expect(within(apps as HTMLElement).getByText("storefront")).toBeTruthy();
    expect(within(apps as HTMLElement).getByText(/npm ci && npm run build/)).toBeTruthy();

    // And nothing has been deployed: the only packageDeploy calls so far are
    // the unconfirmed ones this surface makes on open, of which there are none.
    expect(connection.callsNamed("packageDeploy").filter((c) => c.includes("confirm: true"))).toHaveLength(0);
  });

  it("asks for a hostname only on a deployable's first deploy, and previews it", async () => {
    const connection = fakeConnection({ packages: [ACME], deployments: { "pkg-acme": [PARKED] } });
    mount(connection);
    await click(await screen.findByText("acme"));

    await screen.findByText("Where each app will live");
    // The suggestion is derived from the names already in front of the person,
    // and the preview is the whole hostname rather than the label.
    expect(await screen.findByText("storefront.memql.example.com")).toBeTruthy();
  });

  it("refuses a reserved name client-side, exactly as the server would", async () => {
    const connection = fakeConnection({ packages: [ACME], deployments: { "pkg-acme": [PARKED] } });
    mount(connection);
    await click(await screen.findByText("acme"));

    await typeInto(screen.getByLabelText("Address for storefront") as HTMLInputElement, "portal");

    // The rule is MIRRORED for a keystroke-rate answer, not re-decided: the
    // authority is the Go hostname policy, and cluster-wide uniqueness is
    // deliberately NOT mirrored because a browser cannot answer it.
    expect(await screen.findByText(/reserved/)).toBeTruthy();
  });
});

describe("refusals", () => {
  it("renders the server's own sentence, in surface, with no toast", async () => {
    const connection = fakeConnection({
      packages: [ACME],
      deployments: { "pkg-acme": [] },
      deployError:
        "dsl_requires_cluster_owner: this package ships MemQL DSL (acme), and deploying DSL changes what this whole cluster can do -- so it is reserved to a cluster owner.",
    });
    mount(connection);
    await click(await screen.findByText("acme"));

    await click(await screen.findByRole("button", { name: /Check what deploying acme would do/ }));

    // The HEADLINE is this build's copy for the code...
    expect(await screen.findByText("Deploying MemQL is a cluster owner's decision")).toBeTruthy();
    // ...and the server's own sentence is rendered verbatim beneath it, which
    // is the half that names the domain.
    expect(screen.getByText(/reserved to a cluster owner/)).toBeTruthy();
  });

  it("keeps an unrecognised failure's own message, under a neutral heading", async () => {
    const connection = fakeConnection({
      packages: [ACME],
      deployments: { "pkg-acme": [] },
      deployError: "the cluster fell over in a way nobody has named",
    });
    mount(connection);
    await click(await screen.findByText("acme"));
    await click(await screen.findByRole("button", { name: /Check what deploying acme would do/ }));

    // Inventing a friendly sentence for an unknown fault is how a real failure
    // gets mistaken for somebody's mistake.
    expect(await screen.findByText("This cluster refused")).toBeTruthy();
    expect(screen.getByText(/fell over in a way nobody has named/)).toBeTruthy();
  });
});

describe("the arrival cue's fingerprint", () => {
  // Both directions, pinned. The rule the OS states is that anything named in
  // a fingerprint announces itself, so a liveness field turns a list into a
  // strobe -- and a fingerprint that misses a real change makes the list go
  // quiet exactly when somebody needed telling.
  const base = packageFromRow(ACME);

  it("fires on what a person would call a change", () => {
    const changes: Partial<typeof base>[] = [
      { name: "renamed" },
      { repoRef: "release" },
      { repoUrl: "https://github.com/acme/other" },
      { deployedVersion: "dddddddddddddddddddd" },
      { latestKnownVersion: "eeeeeeeeeeeeeeeeeeee" },
      { updateAvailable: true },
      { status: "archived" },
      { credentialId: "cred-1" },
    ];
    for (const change of changes) {
      expect(packageFingerprint({ ...base, ...change })).not.toBe(packageFingerprint(base));
    }
  });

  it("stays silent for everything that is not one", () => {
    // `createdAt` and `ownerUserId` do not move, but naming either would still
    // be wrong -- and `id` least of all, since the cue is already keyed on it.
    expect(packageFingerprint({ ...base, createdAt: "2027-01-01T00:00:00Z" })).toBe(packageFingerprint(base));
    expect(packageFingerprint({ ...base, id: "pkg-other" })).toBe(packageFingerprint(base));
  });
});

describe("what the packages section does not do", () => {
  it("never handles a token value -- the secret field takes a NAME", async () => {
    const connection = fakeConnection({ packages: [ACME] });
    mount(connection);
    await click(await screen.findByRole("button", { name: /Add a package/ }));

    const field = await screen.findByLabelText("Name of the stored secret holding the access token");
    expect(field).toBeTruthy();
    // The copy has to say so on the surface: a field labelled "token" gets a
    // token pasted into it by anyone who did not read further.
    expect(screen.getByText(/not the token itself/)).toBeTruthy();
  });

  it("mounts no toast container anywhere", async () => {
    const connection = fakeConnection({ packages: [ACME] });
    const { container } = mount(connection);
    await screen.findByText("acme");
    expect(container.querySelector("[data-toast], .os-toast")).toBeNull();
  });
});
