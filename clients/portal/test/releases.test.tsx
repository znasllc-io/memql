// The Releases card on the Deployments view (epic memql#4434): owner-only
// rendering, the typed confirm phrase, the two setup states, and the three
// honest answers a check can give.
//
// RENDERED THROUGH THE REAL VIEW, not by mounting the card with hand-built
// props. The property most worth holding here is that a non-owner never sees
// the card at all -- and that decision lives in DeploymentsView, so a test that
// mounted ReleasesCard directly would assert nothing about it while looking
// like it did.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { CUT_CONFIRM_PHRASE } from "../src/deploy/releases/ReleasesCard";
import { setupFrom } from "../src/deploy/releases/useReleases";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

const PLAN = {
  version: "v0.20.0",
  previousTag: "v0.19.1",
  baseSha: "abcdef1234567890",
  repository: "acme/widget",
  bump: "patch",
  dryRun: true,
  status: "dry_run",
};

const CUT_ROWS = [
  {
    id: "v1:cluster:releaseCut:v0.19.1",
    version: "v0.19.1",
    bump: "patch",
    status: "dispatched",
    baseSha: "1234567890abcdef",
    releaseUrl: "https://github.test/acme/widget/releases/tag/v0.19.1",
    tagName: "v0.19.1",
    error: "",
    pinBumpPrUrl: "",
    pinBumpNote: "",
    requestedByEmail: "op@example.test",
    dispatchedAt: "2026-08-24T00:00:00Z",
    checkedAt: "",
  },
];

// A half-done cut: the state an operator has to ACT on rather than read.
const HALF_DONE_ROWS = [
  {
    ...CUT_ROWS[0],
    id: "v1:cluster:releaseCut:v0.19.2",
    version: "v0.19.2",
    status: "tag_created_release_failed",
    releaseUrl: "",
    tagName: "v0.19.2",
    error: "tag_created_release_failed: GitHub refused the Release",
  },
];

function rowsResult(rows: unknown[]) {
  return {
    rows: () => rows,
    rawNodes: () => rows,
    single: () => rows[0] ?? null,
    meta: () => null,
  };
}

interface Scripted {
  // What the dry run and the cut answer with, or an Error to throw.
  cut?: unknown;
  cuts?: unknown[];
  status?: unknown;
}

function fakeConnection(
  role: string,
  calls: Array<{ name: string; args: unknown }>,
  scripted: Scripted,
): Connection {
  const answer = (name: string, value: unknown, args: unknown) => {
    calls.push({ name, args });
    if (value instanceof Error) throw value;
    return rowsResult([value]);
  };
  const query = asQueryClient({
    listConcepts: vi.fn(async () => [
      {
        id: "v1:cluster:deployment",
        version: "v1",
        domain: "cluster",
        entity: "deployment",
        description: "A deployment.",
        type: "object",
        displayCard: { primary: "version", secondary: "deploymentId", status: "status" },
      },
    ]),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
    releaseCut: vi.fn(async (args: unknown) => answer("releaseCut", scripted.cut ?? PLAN, args)),
    releaseCuts: vi.fn(async (args: unknown) => {
      calls.push({ name: "releaseCuts", args });
      return rowsResult(scripted.cuts ?? []);
    }),
    releaseCutStatus: vi.fn(async (args: unknown) =>
      answer("releaseCutStatus", scripted.status ?? {}, args),
    ),
    executeNamed: vi.fn(async () => ({ rawNodes: () => [], meta: () => ({ cursor: "" }) })),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher: {
      send: vi.fn(),
      addEventListener: vi.fn(() => () => {}),
      registerStream: vi.fn(() => () => {}),
      sendAndWait: vi.fn(async () => ({
        correlateTo: "x",
        deployControlResult: {
          getDeploymentStatus: {
            version: "v0.19.1",
            engineVersion: "v0.19.1",
            syncStatus: "Synced",
            healthStatus: "Healthy",
            components: [],
          },
        },
      })),
    },
    subscriptions: { subscribeGraph: vi.fn(() => () => {}) },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderDeployments(
  role: string,
  scripted: Scripted = {},
  calls: Array<{ name: string; args: unknown }> = [],
) {
  const dial = vi.fn(async () =>
    fakeConnection(role, calls, scripted),
  ) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={["/views/deployments"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("release tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return calls;
}

describe("who sees the Releases card", () => {
  it("shows it to an owner", async () => {
    renderDeployments("owner", { cuts: CUT_ROWS });
    await waitFor(() => expect(screen.getByText("Releases")).toBeTruthy());
    expect(screen.getByRole("button", { name: "Cut a release" })).toBeTruthy();
  });

  // Every non-owner role, enumerated. Admin is the one that matters most: it
  // clears every OTHER gate on this page (view, ship), so a card gated on
  // "can see this page" rather than on the owner role would be visible to it.
  for (const role of ["admin", "developer", "writer", "reader"]) {
    it(`shows a ${role} nothing at all -- absent, not disabled`, async () => {
      renderDeployments(role, { cuts: CUT_ROWS });
      await waitFor(() => expect(screen.getByText("Deployments")).toBeTruthy());
      expect(screen.queryByText("Releases")).toBeNull();
      // Not "present and disabled": instanceActions' doctrine is never to
      // offer a button whose only outcome is a refusal.
      expect(screen.queryByRole("button", { name: "Cut a release" })).toBeNull();
    });
  }

  it("issues no release calls at all for a non-owner", async () => {
    // Both calls refuse for a non-owner, so firing them anyway would fill the
    // console with refusals for a card nobody is looking at -- which trains an
    // operator to ignore exactly the errors that matter.
    const calls = renderDeployments("admin", { cuts: CUT_ROWS });
    await waitFor(() => expect(screen.getByText("Deployments")).toBeTruthy());
    expect(calls.filter((c) => c.name.startsWith("release"))).toEqual([]);
  });
});

describe("cutting a release", () => {
  it("offers all three bumps as radios, defaulting to patch", async () => {
    // The DEFAULT is the assertion that matters: a form that opened on `major`
    // would ship a breaking version to anyone who confirmed without reading.
    // And all three are on screen rather than behind a dropdown click, so the
    // operator sees the range before choosing.
    renderDeployments("owner", { cuts: CUT_ROWS });
    await waitFor(() => expect(screen.getByText("Releases")).toBeTruthy());
    const radios = screen.getAllByRole("radio") as HTMLInputElement[];
    expect(radios.map((r) => r.value)).toEqual(["patch", "minor", "major"]);
    expect(radios.filter((r) => r.checked).map((r) => r.value)).toEqual(["patch"]);
  });

  it("requires the typed confirm phrase before the button arms", async () => {
    const calls = renderDeployments("owner", { cuts: CUT_ROWS });
    await waitFor(() => expect(screen.getByText("Releases")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Cut a release" }));
    const confirm = await screen.findByRole("button", { name: "Cut the release" });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);

    // A near-miss must not arm it. This is the whole point of a typed phrase:
    // the keystroke has to be deliberate.
    const input = screen.getByPlaceholderText(CUT_CONFIRM_PHRASE);
    fireEvent.change(input, { target: { value: "cut a release" } });
    expect((screen.getByRole("button", { name: "Cut the release" }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(input, { target: { value: CUT_CONFIRM_PHRASE } });
    expect((screen.getByRole("button", { name: "Cut the release" }) as HTMLButtonElement).disabled).toBe(false);

    // And nothing has been cut yet -- an armed button is not a pressed one.
    expect(calls.filter((c) => c.name === "releaseCut" && !isDryRun(c.args))).toEqual([]);
  });

  it("sends the bump, the notes and the pin flag, and never dryRun", async () => {
    const calls = renderDeployments("owner", { cuts: CUT_ROWS });
    await waitFor(() => expect(screen.getByText("Releases")).toBeTruthy());

    // A RADIO group, not a dropdown: all three are on screen, which is the
    // point (`major` must never be reached by accident). Selected by its
    // accessible name.
    fireEvent.click(screen.getByRole("radio", { name: /minor/ }));
    fireEvent.change(screen.getByPlaceholderText(/Prepended to GitHub/), {
      target: { value: "The workbench epic." },
    });
    fireEvent.click(screen.getByRole("checkbox"));

    fireEvent.click(screen.getByRole("button", { name: "Cut a release" }));
    fireEvent.change(await screen.findByPlaceholderText(CUT_CONFIRM_PHRASE), {
      target: { value: CUT_CONFIRM_PHRASE },
    });
    fireEvent.click(screen.getByRole("button", { name: "Cut the release" }));

    await waitFor(() => {
      const real = calls.filter((c) => c.name === "releaseCut" && !isDryRun(c.args));
      expect(real.length).toBe(1);
      expect(real[0]?.args).toEqual({
        bump: "minor",
        notes: "The workbench epic.",
        bumpExtensionPin: true,
      });
    });
  });

  it("reads the newest version from TAGS and says the list is only this cluster's history", async () => {
    // A release cut by hand creates a tag this cluster never hears about, so a
    // card that read "newest" off the newest ROW would name a superseded
    // version confidently.
    renderDeployments("owner", { cuts: CUT_ROWS });
    // findByText on the CONTENT, not waitFor on the band: the band renders as
    // soon as the role resolves, so waiting on it is a guard that is already
    // satisfied before the thing being asserted exists.
    expect(await screen.findByText(/newest tag on/)).toBeTruthy();
    expect(screen.getByText(/a patch cut would be/)).toBeTruthy();
    // v0.19.1 appears TWICE and that is the point: once as the newest tag
    // (read from GitHub) and once as this cluster's own row. They agree here;
    // after a hand cut they would not, which is why the card keeps them
    // separate rather than deriving one from the other.
    expect(screen.getAllByText("v0.19.1").length).toBe(2);
    expect(screen.getByText("v0.20.0")).toBeTruthy();
    expect(
      screen.getByText(/cut by hand is the newest tag above and has no row here/),
    ).toBeTruthy();
  });

  it("spells out the half-done state and what to do about it", async () => {
    renderDeployments("owner", { cuts: HALF_DONE_ROWS });
    expect(await screen.findByText("tag created, release failed")).toBeTruthy();
    // The instruction, not just the state: nothing is building, and a human
    // has to publish or delete.
    expect(screen.getByText(/Publish a Release for that tag/)).toBeTruthy();
    expect(screen.getByText(/delete the tag to undo the cut/)).toBeTruthy();
  });
});

describe("the setup states render instead of the button", () => {
  it("names the variable to seed when no repository is configured", async () => {
    renderDeployments("owner", {
      cut: new Error("release_repo_unconfigured: no repository is configured"),
    });
    // findByText, not getByText: the band renders as soon as the ROLE
    // resolves, and the release state arrives an async tick later.
    expect(await screen.findByText(/MEMQL_RELEASE_REPO/)).toBeTruthy();
    // Instead of, not beside.
    expect(screen.queryByRole("button", { name: "Cut a release" })).toBeNull();
  });

  it("names the secret to seed when no credential is available", async () => {
    renderDeployments("owner", {
      cut: new Error("credential_unavailable: no GitHub credential is available"),
    });
    expect(await screen.findByText(/MEMQL_GITHUB_RELEASE_TOKEN/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Cut a release" })).toBeNull();
  });

  it("treats anything else as an error, with the button still offered", async () => {
    // A setup state is a true statement about the installation; a failure is
    // a failure, and conflating them would either hide a real fault behind a
    // setup notice or tell an operator to seed a variable they already have.
    renderDeployments("owner", { cut: new Error("github_unreachable: GitHub returned HTTP 503") });
    expect(await screen.findByText(/Could not read the release state/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cut a release" })).toBeTruthy();
  });

  it("matches on the refusal CODE and never on prose", () => {
    // The codes are a contract; the messages around them are free to be
    // reworded. A card branching on wording breaks silently the first time
    // one is.
    expect(setupFrom("release_repo_unconfigured: anything at all")).toBe("release_repo_unconfigured");
    expect(setupFrom("credential_unavailable: anything at all")).toBe("credential_unavailable");
    expect(setupFrom("github_unreachable: GitHub returned HTTP 503")).toBe("");
    // Prose that merely SOUNDS like a setup problem is not one.
    expect(setupFrom("the repository is not configured properly")).toBe("");
    expect(setupFrom("no credential available")).toBe("");
  });
});

describe("checking whether the images exist", () => {
  it("says every image is published when they are", async () => {
    renderDeployments("owner", {
      cuts: CUT_ROWS,
      status: {
        version: "v0.19.1",
        status: "images_available",
        images: [{ repository: "acme/widget-bff", present: true }],
        age: "",
        checkError: "",
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Check images" }));
    expect(await screen.findByText("Every image is published.")).toBeTruthy();
  });

  it("says still building, with an age and the images that are missing", async () => {
    renderDeployments("owner", {
      cuts: CUT_ROWS,
      status: {
        version: "v0.19.1",
        status: "dispatched",
        images: [
          { repository: "acme/widget-identity", present: true },
          { repository: "acme/widget-agent", present: false },
        ],
        age: "4 minutes ago",
        checkError: "",
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Check images" }));
    expect(await screen.findByText(/Still building/)).toBeTruthy();
    expect(screen.getByText(/4 minutes ago/)).toBeTruthy();
    expect(screen.getByText(/acme\/widget-agent/)).toBeTruthy();
  });

  // THE CASE THE WHOLE DESIGN TURNS ON. A check that errored knows nothing
  // about whether the images exist, and rendering it as an absence would
  // report a good release as unbuilt -- while rendering it as a presence
  // would be the false green D5 exists to prevent.
  it("says the check could not tell, and never renders that as an absence", async () => {
    renderDeployments("owner", {
      cuts: CUT_ROWS,
      status: {
        version: "v0.19.1",
        status: "dispatched",
        images: [],
        age: "4 minutes ago",
        checkError: "registry_check_failed: the registry returned HTTP 500",
      },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Check images" }));

    expect(await screen.findByText(/The check could not tell/)).toBeTruthy();
    expect(screen.getByText(/the status above is unchanged|The status above is unchanged/)).toBeTruthy();
    // And NOT the still-building sentence, which would be a claim about the
    // registry that nothing established.
    expect(screen.queryByText(/Still building/)).toBeNull();
    expect(screen.queryByText("Every image is published.")).toBeNull();
  });
});

// isDryRun distinguishes the card's mount-time probe from a real cut. The
// probe is what establishes the setup state and the headline; a test asserting
// on "the cut" has to exclude it or it counts one call too many.
function isDryRun(args: unknown): boolean {
  return (args as Record<string, unknown> | null)?.["dryRun"] === true;
}
