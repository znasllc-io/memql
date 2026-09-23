import { StrictMode, useState } from "react";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { useOs } from "../../src/chrome/state";
import { ConnectReturnDispatcher } from "../../src/apps/deployables/sources/ConnectReturnDispatcher";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { RepositorySource } from "../../src/apps/deployables/page/stops/compose/RepositorySource";
import { EMPTY_DRAFT } from "../../src/apps/deployables/page/compose";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { RepositoryPicker } from "../../src/apps/deployables/sources/RepositoryPicker";
import { useSourceRepositories } from "../../src/apps/deployables/sources/useGithubConnect";
import { copyFor } from "../../src/apps/deployables/packages/refusals";
import { APP_REGISTERED, ConnectReturnNotice } from "../../src/apps/deployables/sources/ConnectReturnNotice";
import {
  CONNECT_RESULT_PARAM,
  captureConnectReturn,
  clearParkedConnectReturn,
  connectSucceeded,
  readConnectReturn,
  returnPathFor,
  scrubbedSearch,
  takeParkedConnectReturn,
} from "../../src/apps/deployables/sources/connectReturn";
import {
  groupRepositories,
  repositoryCount,
  repositoryPageFrom,
  type RepositoryPage,
} from "../../src/apps/deployables/sources/repositories";
import {
  credentialFromRow,
  githubGrantOf,
  isGithubAppGrant,
  pastedCredentials,
} from "../../src/apps/deployables/sources/rows";
import {
  builtinReply,
  FIXTURE_GITHUB_PAT,
  click,
  credentialRow,
  emit,
  fakeConnection,
  githubGrantRow,
  probeReply,
  repositoriesReply,
  repositoryFixture,
  type as typeInto,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// GitHub Connect (epic memql#4915): connecting an account, picking a
// repository from a list, and the connected-account card that ends it.
//
// The DOM half goes through `connection.query` and `connection.subscriptions`
// exactly as production does -- the harness answers at `executeNamed`, so
// the real LiveCollection, the real projections and the real generated
// builders all run and the assertions are on the STRING that reaches the
// wire. The pure half is asserted directly, because "beta-corp sorts after
// acme and a pending organization sorts after both" is a statement about a
// function and not about a DOM.
//
// Everything a person is asked to believe is asserted as something they
// SEE: text, roles, `data-state`. Never a `.value`.

// ---------------------------------------------------------------------------
// The reply, read
// ---------------------------------------------------------------------------

describe("reading a sourceRepositories reply", () => {
  const REPLY = repositoriesReply({
    repositories: [
      repositoryFixture({ fullName: "acme/widget", private: true, visibility: "private" }),
      repositoryFixture({ fullName: "acme/docs" }),
    ],
    installations: [{ id: "i-acme", login: "acme", accountType: "Organization", repositorySelection: "all", suspended: false }],
    pending: [{ login: "beta-corp" }],
    nextPage: 2,
  });

  it("reads installation accounts and pending organization names from the engine reply", () => {
    const page = repositoryPageFrom(repositoriesReply({
      installations: [{ id: "42", account: "acme", accountType: "Organization", repositorySelection: "selected", suspended: false }],
      pending: ["beta-corp"],
    }));
    expect(page.installations[0]?.login).toBe("acme");
    expect(page.pending).toEqual([{ login: "beta-corp" }]);
    expect(groupRepositories(page, "").map(group => group.owner)).toContain("beta-corp");
  });

  it("projects every field a row is drawn from", () => {
    const page = repositoryPageFrom(REPLY);
    expect(page.repositories.map((r) => r.fullName)).toEqual(["acme/widget", "acme/docs"]);
    const widget = page.repositories[0]!;
    expect(widget.owner).toBe("acme");
    expect(widget.name).toBe("widget");
    expect(widget.private).toBe(true);
    expect(widget.defaultBranch).toBe("main");
    expect(widget.installationId).toBe("i-acme");
    expect(page.installations.map((i) => i.login)).toEqual(["acme"]);
    expect(page.pending.map((p) => p.login)).toEqual(["beta-corp"]);
    expect(page.nextPage).toBe(2);
    expect(page.reason).toBe("ok");
  });

  it("reads a list that crossed the wire as JSON text the same as a decoded one", () => {
    // The decoded reading above is the reachable positive for this one: both
    // shapes have to produce the same page, or the picker would render on a
    // seed and go blank on a re-read.
    const asText = repositoriesReply({
      repositories: JSON.stringify([{ fullName: "acme/widget", private: "true" }]),
      nextPage: "3",
    });
    const page = repositoryPageFrom(asText);
    expect(page.repositories.map((r) => r.fullName)).toEqual(["acme/widget"]);
    expect(page.repositories[0]!.private).toBe(true);
    expect(page.nextPage).toBe(3);
  });

  it("derives owner and name from the full name when the halves are absent", () => {
    const page = repositoryPageFrom(repositoriesReply({ repositories: [{ fullName: "octocat/dotfiles" }] }));
    expect(page.repositories[0]!.owner).toBe("octocat");
    expect(page.repositories[0]!.name).toBe("dotfiles");
  });

  it("drops a member with no name at all, and a pending entry naming nobody", () => {
    // A row the picker keys on `fullName` could not be chosen with none, so
    // rendering it would be a line that does nothing when clicked; a pending
    // entry with no login names nobody to ask, which is its whole content.
    const page = repositoryPageFrom(
      repositoriesReply({ repositories: [{ url: "https://github.com/x" }], pending: [{ login: "  " }] }),
    );
    expect(page.repositories).toEqual([]);
    expect(page.pending).toEqual([]);
  });

  it("answers an empty page for an unreadable list rather than throwing", () => {
    const page = repositoryPageFrom(repositoriesReply({ repositories: "{not json" }));
    expect(page.repositories).toEqual([]);
    // The reachable positive: the same reader DOES find a list when there is
    // one, so the empty answer above is the parse failing and not the reader.
    expect(repositoryPageFrom(REPLY).repositories.length).toBe(2);
  });

  it("answers an empty page for no row at all", () => {
    expect(repositoryPageFrom(undefined).repositories).toEqual([]);
    expect(repositoryPageFrom(null).reason).toBe("");
  });
});

// ---------------------------------------------------------------------------
// Grouping, ordering and search
// ---------------------------------------------------------------------------

describe("the picker's list", () => {
  const PAGE: Pick<RepositoryPage, "repositories" | "pending"> = repositoryPageFrom(
    repositoriesReply({
      repositories: [
        repositoryFixture({ fullName: "octocat/dotfiles" }),
        repositoryFixture({ fullName: "acme/widget" }),
        repositoryFixture({ fullName: "acme/Docs" }),
      ],
      pending: [{ login: "beta-corp" }],
    }),
  );

  it("groups by owner, owners alphabetically and repositories alphabetically inside one", () => {
    const groups = groupRepositories(PAGE, "");
    expect(groups.map((g) => g.owner)).toEqual(["acme", "octocat", "beta-corp"]);
    expect(groups[0]!.repositories.map((r) => r.name)).toEqual(["Docs", "widget"]);
  });

  it("puts a pending organization last, whatever its name would sort as", () => {
    // beta-corp sorts between acme and octocat by name, and comes after both
    // anyway: a group with nothing in it is not a choice, and interleaving it
    // would put a line nobody can act on among the ones they can.
    const groups = groupRepositories(PAGE, "");
    expect(groups[groups.length - 1]).toMatchObject({ owner: "beta-corp", pending: true });
    expect(groups[groups.length - 1]!.repositories).toEqual([]);
  });

  it("matches a search over the full name, case-insensitively", () => {
    expect(groupRepositories(PAGE, "WIDGET").map((g) => g.owner)).toEqual(["acme"]);
    expect(repositoryCount(groupRepositories(PAGE, "WIDGET"))).toBe(1);
    expect(groupRepositories(PAGE, "acme/do").map((g) => g.owner)).toEqual(["acme"]);
    expect(groupRepositories(PAGE, "acme/do")[0]!.repositories.map((r) => r.name)).toEqual(["Docs"]);
    expect(groupRepositories(PAGE, "nothing-like-this")).toEqual([]);
  });

  it("finds a pending organization by the name somebody is waiting on", () => {
    // Hiding it would take away the one sentence that explains why that
    // organization has no repositories.
    const groups = groupRepositories(PAGE, "beta");
    expect(groups.map((g) => g.owner)).toEqual(["beta-corp"]);
  });

  it("never renders one login twice, as rows and as a sentence", () => {
    const both = repositoryPageFrom(
      repositoriesReply({
        repositories: [repositoryFixture({ fullName: "acme/widget" })],
        pending: [{ login: "acme" }],
      }),
    );
    const groups = groupRepositories(both, "");
    expect(groups.map((g) => g.owner)).toEqual(["acme"]);
    expect(groups[0]!.pending).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The credential helpers
// ---------------------------------------------------------------------------

describe("telling a grant from a pasted token", () => {
  const grant = credentialFromRow(githubGrantRow({ id: "cred-grant" }));
  const token = credentialFromRow(credentialRow({ id: "cred-token" }));

  it("reads a row with no kind at all as the pasted kind", () => {
    // Every credential written before this epic carries none, and reading
    // that as a third state is how they would come to be listed nowhere.
    expect(token.kind).toBe("token");
    expect(isGithubAppGrant(token)).toBe(false);
    expect(isGithubAppGrant(grant)).toBe(true);
  });

  it("reads an unrecognised kind as not-a-grant, keeping the value verbatim", () => {
    const future = credentialFromRow(credentialRow({ id: "cred-x", kind: "gitlab_app" }));
    expect(future.kind).toBe("gitlab_app");
    expect(isGithubAppGrant(future)).toBe(false);
  });

  it("picks the ACTIVE grant as the connection, and lists the rest as pasted", () => {
    const dead = credentialFromRow(githubGrantRow({ id: "cred-old", status: "revoked" }));
    expect(githubGrantOf([dead, grant])?.id).toBe("cred-grant");
    expect(githubGrantOf([token])).toBeNull();
    expect(pastedCredentials([grant, token]).map((c) => c.id)).toEqual(["cred-token"]);
  });

  it("still answers a revoked grant when that is the only one, so the card can offer a reconnect", () => {
    const dead = credentialFromRow(githubGrantRow({ id: "cred-old", status: "revoked" }));
    expect(githubGrantOf([dead])?.id).toBe("cred-old");
  });
});

// ---------------------------------------------------------------------------
// The return from GitHub
// ---------------------------------------------------------------------------

function ReturnedWindow() {
  const { state, actions } = useOs();
  return <><output data-testid="window-count">{Object.keys(state.shell.windows).length}</output>
    {Object.values(state.shell.windows).map(win => win.appId === "deployables" ?
      <DeployablesApp key={win.id} sectionId={win.sectionId} navigate={() => {}} askContext={() => {}}
        intent={win.intent} consumeIntent={id => actions.consumeWindowIntent(win.id, id)} /> : null)}
  </>;
}

describe("the return from GitHub", () => {
  afterEach(() => {
    h.connection = null;
    clearParkedConnectReturn();
    history.replaceState({}, "", "/");
  });

  it("accepts the identity callback's actual wire marker and outcomes", () => {
    // Read the producer's constants so this contract cannot quietly drift
    // back to a frontend-only fixture such as github_connect=ok.
    const here = dirname(fileURLToPath(import.meta.url));
    const source = (path: string) => readFileSync(join(here, "../../../../component/identity", path), "utf8");
    const valueIn = (text: string, name: string) => {
      const match = text.match(new RegExp(`${name}\\s*=\\s*"([^"\\n]+)"`));
      expect(match, `identity callback constant ${name}`).not.toBeNull();
      return match![1]!;
    };
    // ONE MARKER FOR BOTH TRIPS, composed in one place on the identity side
    // (github_return.go) because two of its packages send somebody back.
    const param = valueIn(source("github_return.go"), "GithubResultParam");
    expect(param).toBe(CONNECT_RESULT_PARAM);

    const callback = source("http/github_callback.go");
    for (const name of ["resultConnected", "resultReconnected", "resultInstalled", "resultStateInvalid", "resultExchangeFailed"]) {
      const reason = valueIn(callback, name);
      const result = readConnectReturn(`?connect=deployables&${param}=${reason}`);
      expect(result).toEqual({ section: "deployables", reason });
      expect(connectSucceeded(result!)).toBe(["resultConnected", "resultReconnected"].includes(name));
    }

    // THE SETUP TRIP'S OUTCOMES, none of which is a connection: even the good
    // one only says the cluster now has an app, and this person's account is
    // still to connect.
    const setup = source("http/github_app_callback.go");
    expect(valueIn(setup, "resultAppRegistered")).toBe(APP_REGISTERED);
    for (const name of ["resultAppRegistered", "resultAppSetupStateInvalid", "resultAppSetupFailed", "resultAppManagedByEnv", "resultAppSetupNotAnOwner"]) {
      const reason = valueIn(setup, name);
      const result = readConnectReturn(`?connect=deployables&${param}=${reason}`);
      expect(result).toEqual({ section: "deployables", reason });
      expect(connectSucceeded(result!)).toBe(false);
      // Every refusal among them has this build's own headline: a setup that
      // failed under "This cluster refused" would be a fault with no repair.
      if (reason !== APP_REGISTERED) expect(copyFor(reason), reason).not.toBeNull();
    }
  });

  it("says what is left to do when a registration could not go on to connecting", () => {
    render(<ConnectReturnNotice result={{ section: "deployables", reason: APP_REGISTERED }} />);
    expect(screen.getByText("GitHub is set up for this cluster. Connect your account to choose its repositories.")).toBeTruthy();
    cleanup();
    // ...and a setup that failed says which trip did not finish.
    render(<ConnectReturnNotice result={{ section: "deployables", reason: "github_app_setup_failed" }} />);
    expect(screen.getByText("GitHub could not be set up")).toBeTruthy();
    expect(screen.getByText("GitHub sent you back without setting this cluster up.")).toBeTruthy();
  });

  it("reopens Deployables once after the production reconnect redirect", async () => {
    const userId = "11111111-1111-4111-8111-111111111111";
    const myGrant = "22222222-2222-4222-8222-222222222222";
    const connection = fakeConnection({ credentials: [
      githubGrantRow({ id: "colleague-grant", ownerUserId: "v1:identity:user:colleague", login: "colleague" }),
      githubGrantRow({ id: myGrant, ownerUserId: `v1:identity:user:${userId}`, login: "owner" }),
    ], repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/private-site" })] }) });
    h.connection = connection;
    history.replaceState({}, "", "/?connect=deployables&github=reconnected");
    captureConnectReturn(window);
    render(withSession(<StrictMode><ConnectReturnDispatcher /><ReturnedWindow /></StrictMode>, { userId }));
    expect(await screen.findByRole("button", { name: /private-site/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(screen.getByTestId("window-count").textContent).toBe("1");
    expect(window.location.search).toBe("");
    expect(connection.callsNamed("sourceRepositories").every(call => call.includes(myGrant))).toBe(true);
    h.connection = null;
  });

  it.each(["connected", "reconnected"])("waits for the credential feed after %s instead of inventing a grant", async reason => {
    const connection = fakeConnection({ credentials: [], repositories: repositoriesReply({
      repositories: [repositoryFixture({ fullName: "acme/private-site" })],
    }) });
    h.connection = connection;
    history.replaceState({}, "", `/?connect=deployables&github=${reason}`);
    captureConnectReturn(window);
    render(withSession(<><ConnectReturnDispatcher /><ReturnedWindow /></>));
    await screen.findByRole("button", { name: "Connect GitHub" });
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    await emit(connection, "v1:platform:sourceCredential",
      githubGrantRow({ id: "my-new-grant", ownerUserId: "v1:identity:user:u-me" }), "NODE_CREATED");
    expect(await screen.findByRole("button", { name: /private-site/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(connection.callsNamed("sourceRepositories")[0]).toContain("my-new-grant");
  });

  it("shows a pending credential read, then the connected repositories", async () => {
    const connection = fakeConnection({ credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/private-site" })] }) });
    const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    let finishRead!: () => void;
    const pending = new Promise<void>(resolve => { finishRead = resolve; });
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
      if (name === "sourceCredentialsMine") await pending;
      return execute(name, call, options);
    });
    h.connection = connection;
    history.replaceState({}, "", "/?connect=deployables&github=reconnected");
    captureConnectReturn(window);
    render(withSession(<><ConnectReturnDispatcher /><ReturnedWindow /></>));
    expect(await screen.findByText("Loading your source connections")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    await act(async () => finishRead());
    expect(await screen.findByRole("button", { name: /private-site/ })).toBeTruthy();
  });

  it("shows a failed credential read and retries it instead of offering a new connection", async () => {
    const connection = fakeConnection({ credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/private-site" })] }) });
    const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    let fail = true;
    vi.spyOn(connection.query, "executeNamed").mockImplementation((name, call, options) => {
      if (name === "sourceCredentialsMine" && fail) return Promise.reject(new Error("source connection read unavailable"));
      return execute(name, call, options);
    });
    h.connection = connection;
    history.replaceState({}, "", "/?connect=deployables&github=reconnected");
    captureConnectReturn(window);
    render(withSession(<><ConnectReturnDispatcher /><ReturnedWindow /></>));
    expect(await screen.findByText("Your source connections could not be read.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(0);
    fail = false;
    await click(screen.getByRole("button", { name: "Read source connections again" }));
    expect(await screen.findByRole("button", { name: /private-site/ })).toBeTruthy();
    expect(screen.queryByText("Your source connections could not be read.")).toBeNull();
  });

  it.each(["settings", "deployables"])("keeps an installation-only return neutral in %s", async section => {
    h.connection = fakeConnection({ credentials: [] });
    history.replaceState({}, "", `/?connect=${section}&github=installed`);
    captureConnectReturn(window);
    render(withSession(<><ConnectReturnDispatcher /><ReturnedWindow /></>));
    expect(await screen.findByText("GitHub installation finished. Connect your account to choose its repositories.")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Connect GitHub" })).toBeTruthy();
    expect(screen.queryByText("GitHub sent you back without completing the connection.")).toBeNull();
  });

  it("returns an OAuth failure to the repository step with its repair", async () => {
    h.connection = fakeConnection({ credentials: [] });
    history.replaceState({}, "", "/?connect=deployables&github=connect_state_invalid");
    captureConnectReturn(window);
    render(withSession(<><ConnectReturnDispatcher /><ReturnedWindow /></>));
    expect(await screen.findByText("That sign-in link is no longer valid")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Connect GitHub" })).toBeTruthy();
  });

  it("does not open unrelated sections from a callback parameter", () => {
    expect(readConnectReturn("?github=installed&connect=unrecognised")).toEqual({ reason: "installed", section: "settings" });
  });

  it("builds a return PATH, never a URL", () => {
    // The cluster composes the origin from its own domain, so nothing this
    // browser says can redirect somebody off-cluster.
    expect(returnPathFor("settings")).toBe("/?connect=settings");
  });

  it("reads the marker, and answers null when there is none", () => {
    expect(readConnectReturn("?github=reconnected&connect=settings")).toEqual({
      reason: "reconnected",
      section: "settings",
    });
    expect(readConnectReturn("?github=connect_state_invalid")).toEqual({
      reason: "connect_state_invalid",
      // The section hint is a courtesy: a callback that rebuilt the URL and
      // dropped it still returns somebody to a sensible place.
      section: "settings",
    });
    // Somebody who navigated to the OS directly has not failed at anything.
    expect(readConnectReturn("")).toBeNull();
    expect(readConnectReturn("?code=abc&state=xyz")).toBeNull();
  });

  it("scrubs only its own two parameters", () => {
    // AuthProvider reads `code` and `state` out of the same query, so a
    // blanket scrub here would eat a sign-in mid-flight.
    expect(scrubbedSearch("?github=reconnected&connect=settings&code=abc")).toBe("?code=abc");
    expect(scrubbedSearch("?github=reconnected")).toBe("");
  });

  it("takes the marker out of the address bar at boot, keeping the path", () => {
    history.replaceState({}, "", "/somewhere?github=reconnected&connect=settings&keep=1#frag");
    const captured = captureConnectReturn(window);
    expect(captured).toEqual({ reason: "reconnected", section: "settings" });
    expect(window.location.pathname).toBe("/somewhere");
    expect(window.location.search).toBe("?keep=1");
    expect(window.location.hash).toBe("#frag");
  });

  it("hands the parked return over exactly ONCE", () => {
    // The effect that consumes it runs again on a StrictMode remount; a
    // value that survived would open a second window every time.
    history.replaceState({}, "", "/?github=reconnected");
    captureConnectReturn(window);
    expect(takeParkedConnectReturn()).toEqual({ reason: "reconnected", section: "settings" });
    expect(takeParkedConnectReturn()).toBeNull();
  });

  it("parks nothing, and touches nothing, for a browser that arrived on its own", () => {
    history.replaceState({}, "", "/?code=abc");
    expect(captureConnectReturn(window)).toBeNull();
    expect(window.location.search).toBe("?code=abc");
    expect(takeParkedConnectReturn()).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The picker, rendered
// ---------------------------------------------------------------------------

const PICKER_PAGE = repositoryPageFrom(
  repositoriesReply({
    repositories: [
      repositoryFixture({ fullName: "acme/widget", private: true, visibility: "private" }),
      repositoryFixture({ fullName: "acme/docs", defaultBranch: "trunk" }),
      repositoryFixture({ fullName: "octocat/dotfiles", private: true, visibility: "private" }),
    ],
    pending: [{ login: "beta-corp" }],
  }),
);

function renderPicker(over: Partial<Parameters<typeof RepositoryPicker>[0]> = {}) {
  const onChoose = vi.fn();
  const onLookAgain = vi.fn();
  const onReadMore = vi.fn();
  const view = render(
    <RepositoryPicker
      page={over.page ?? PICKER_PAGE}
      readAt={over.readAt ?? "2026-09-03T00:00:00Z"}
      busy={over.busy ?? false}
      refusal={over.refusal ?? null}
      installUrl={over.installUrl ?? ""}
      chosen={over.chosen ?? ""}
      onChoose={over.onChoose ?? onChoose}
      onLookAgain={over.onLookAgain ?? onLookAgain}
      onReadMore={over.onReadMore ?? onReadMore}
    />,
  );
  return { ...view, onChoose, onLookAgain, onReadMore };
}

describe("the repository picker", () => {
  it("lets a connected person pick a private repository with nothing typed", async () => {
    const { onChoose, container } = renderPicker();
    // A row shows the SHORT name, because the group header already said the
    // owner once; the full name is on `title` for anyone who wants certainty.
    const row = screen.getByRole("button", { name: /widget/ });
    expect(within(row).getByText("widget")).toBeTruthy();
    expect(within(row).getByTitle("acme/widget")).toBeTruthy();
    expect(within(row).getByText("private")).toBeTruthy();
    await click(row);
    expect(onChoose).toHaveBeenCalledTimes(1);
    expect(onChoose.mock.calls[0]![0]).toMatchObject({ fullName: "acme/widget", url: "https://github.com/acme/widget" });
    // NOTHING WAS TYPED. The only field on this surface is the search box.
    expect(container.querySelectorAll("input")).toHaveLength(1);
    expect(screen.getByLabelText("Search repositories")).toBeTruthy();
  });

  it("says nothing at all for a public repository", () => {
    // Most repositories are public, so silence is the default state -- a
    // lock icon AND the word would be the same fact twice.
    renderPicker();
    const docs = screen.getByRole("button", { name: /docs/ });
    expect(within(docs).queryByText("public")).toBeNull();
    // The reachable positive for that absence, one row above.
    const widget = screen.getByRole("button", { name: /widget/ });
    expect(within(widget).getByText("private")).toBeTruthy();
  });

  it("marks the chosen row in the shell's own selection language", () => {
    renderPicker({ chosen: "acme/widget" });
    const row = screen.getByRole("button", { name: /widget/ });
    expect(row.getAttribute("data-current")).toBe("true");
    expect(row.getAttribute("aria-expanded")).toBe("true");
    expect(within(row).getByText("chosen")).toBeTruthy();
    expect(screen.getByRole("button", { name: /docs/ }).getAttribute("data-current")).toBeNull();
  });

  it("renders a pending organization BY NAME, as a sentence and not an error", () => {
    renderPicker();
    const group = screen.getByRole("group", { name: "beta-corp" });
    expect(within(group).getByText("Waiting for an owner of beta-corp to approve the app.")).toBeTruthy();
    // `--os-warn`, never `--os-error`: an organization owner has not clicked
    // yet, which is somebody's next step rather than a fault.
    expect(within(group).getByText("pending").getAttribute("data-tone")).toBe("warn");
    expect(group.querySelector("[role='alert']")).toBeNull();
    // And it is a group with a sentence INSTEAD of rows, not a hidden one.
    expect(within(group).queryAllByRole("button")).toEqual([]);
  });

  it("filters client-side over what was read, and says how much of it is showing", async () => {
    renderPicker();
    expect(screen.getByText(/Showing 3 of 3, read/)).toBeTruthy();
    await typeInto(screen.getByLabelText("Search repositories") as HTMLInputElement, "widget");
    expect(screen.getByText(/Showing 1 of 3, read/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /dotfiles/ })).toBeNull();
  });

  it("offers to look again, because this is a reading and not a feed", async () => {
    const { onLookAgain } = renderPicker();
    await click(screen.getByRole("button", { name: "Look again" }));
    expect(onLookAgain).toHaveBeenCalledTimes(1);
    // No walk to offer when the page said there was none.
    expect(screen.queryByRole("button", { name: "Read more" })).toBeNull();
  });

  it("offers the walk only when the reply named a next page", async () => {
    const paged = repositoryPageFrom(
      repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/widget" })], nextPage: 2 }),
    );
    const { onReadMore } = renderPicker({ page: paged });
    await click(screen.getByRole("button", { name: "Read more" }));
    expect(onReadMore).toHaveBeenCalledTimes(1);
  });

  it("draws no search box when there is nothing to search", () => {
    renderPicker({ page: repositoryPageFrom(repositoriesReply({})), readAt: "" });
    expect(screen.queryByLabelText("Search repositories")).toBeNull();
    expect(screen.getByText("This connection reaches no repositories yet.")).toBeTruthy();
  });

  it("makes empty an invitation, with a real anchor to a new tab", () => {
    renderPicker({
      page: repositoryPageFrom(repositoriesReply({})),
      installUrl: "https://github.com/apps/memql/installations/new",
    });
    const link = screen.getByRole("link", { name: "Install on another organization" });
    expect(link.getAttribute("href")).toBe("https://github.com/apps/memql/installations/new");
    expect(link.getAttribute("target")).toBe("_blank");
    // LOAD-BEARING: a new tab handed a live `window.opener` can navigate the
    // shell it came from.
    expect(link.getAttribute("rel")).toBe("noreferrer noopener");
  });

  it("offers no link at all on a cluster with no GitHub App", () => {
    renderPicker({ page: repositoryPageFrom(repositoriesReply({})), installUrl: "" });
    expect(screen.queryByRole("link", { name: "Install on another organization" })).toBeNull();
    cleanup();
    // With repositories listed, too: a link to nowhere is worse than no link.
    renderPicker({ installUrl: "" });
    expect(screen.queryByRole("link", { name: "Install on another organization" })).toBeNull();
  });

  // ANOTHER ORGANIZATION IS ANOTHER GROUP IN THIS LIST. The link was offered
  // when the list was EMPTY and nowhere else in the wizard, so somebody with
  // one organization connected, looking for a repository in a second, was
  // shown a complete-looking list and no way to make it longer.
  it("offers another organization under a list that already has some, as text beside the one button", () => {
    renderPicker({ installUrl: "https://github.com/apps/memql/installations/new" });
    const link = screen.getByRole("link", { name: "Install on another organization" });
    expect(link.getAttribute("href")).toBe("https://github.com/apps/memql/installations/new");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noreferrer noopener");
    // ON THE ROW THAT READS AGAIN, because reading again is what follows it...
    const row = link.closest(".os-refresh-row") as HTMLElement;
    expect(within(row).getByRole("button", { name: "Look again" })).toBeTruthy();
    // ...and NOT a second button there: it leaves the product, and the row has
    // its one act already.
    expect(link.classList.contains("os-button")).toBe(false);
    expect(link.classList.contains("os-link")).toBe(true);
  });

  it("reads the list again when the person comes back from GitHub, once, and only after following the link", async () => {
    const { onLookAgain } = renderPicker({ installUrl: "https://github.com/apps/memql/installations/new" });
    // Looking at the tab is not a reason to call GitHub...
    await act(async () => { window.dispatchEvent(new Event("focus")); });
    expect(onLookAgain).not.toHaveBeenCalled();

    // ...having gone to install the app somewhere is. jsdom follows no link,
    // so the navigation is stopped and only the click is kept.
    const link = screen.getByRole("link", { name: "Install on another organization" });
    link.addEventListener("click", (e) => e.preventDefault());
    await click(link);
    expect(onLookAgain).not.toHaveBeenCalled();
    await act(async () => { window.dispatchEvent(new Event("focus")); });
    expect(onLookAgain).toHaveBeenCalledTimes(1);

    // ONCE. The next look at the tab is just somebody looking at the tab.
    await act(async () => { window.dispatchEvent(new Event("focus")); });
    expect(onLookAgain).toHaveBeenCalledTimes(1);
  });

  it("keeps the last good list when a read is refused, because a refusal is not a zero", () => {
    renderPicker({
      refusal: { code: "reconnect_required", message: "GitHub refused this connection." },
    });
    // The OS headline above, the server's own sentence beneath, verbatim.
    expect(screen.getByText("Your GitHub connection needs renewing")).toBeTruthy();
    expect(screen.getByText("GitHub refused this connection.")).toBeTruthy();
    expect(screen.getByText(/Reconnect GitHub in Settings > Sources/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /widget/ })).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The picker on the wire
// ---------------------------------------------------------------------------
//
// The picker is prop-driven and the hook that fills it is the seam the
// Source stop's rebuild plugs into, so they are exercised TOGETHER here: a
// component wired exactly as that stop will wire it, over the same fake
// `executeNamed` production goes through, so the assertion is on the string
// that reaches the engine.

function PickerHost({ credentialId = "" }: { credentialId?: string }) {
  const repos = useSourceRepositories();
  const [chosen, setChosen] = useState("");
  return (
    <RepositoryPicker
      page={repos.page}
      readAt={repos.readAt}
      busy={repos.busy}
      refusal={repos.refusal}
      chosen={chosen}
      onChoose={(repo) => setChosen(repo.fullName)}
      onLookAgain={() => void repos.read(credentialId, 1)}
      onReadMore={() => void repos.read(credentialId, repos.page.nextPage)}
    />
  );
}

describe("reading the picker's list", () => {
  afterEach(() => {
    h.connection = null;
  });

  function mountPicker(seed: FakeSeed) {
    const connection = fakeConnection(seed);
    h.connection = connection;
    return { connection, ...render(<PickerHost />) };
  }

  it("asks the cluster with both arguments always, so there is one call shape", async () => {
    const { connection } = mountPicker({
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/widget" })] }),
    });
    expect(screen.getByText("Not read yet.")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Look again" }));
    // `credentialId: ""` is a documented value -- "the grant I hold" -- and
    // not an omission, so it is sent rather than left out.
    expect(connection.callsNamed("sourceRepositories")).toEqual([
      'builtin sourceRepositories(credentialId: "", page: 1)',
    ]);
    expect(await screen.findByRole("button", { name: /widget/ })).toBeTruthy();
  });

  it("appends on a walk and replaces on a re-read", async () => {
    const { connection } = mountPicker({
      repositories: repositoriesReply({
        repositories: [repositoryFixture({ fullName: "acme/widget" })],
        nextPage: 2,
      }),
    });
    await click(screen.getByRole("button", { name: "Look again" }));
    await screen.findByRole("button", { name: /widget/ });
    await click(screen.getByRole("button", { name: "Read more" }));
    expect(connection.callsNamed("sourceRepositories").at(-1)).toBe(
      'builtin sourceRepositories(credentialId: "", page: 2)',
    );
    // The same fixture came back, so the walk shows it twice -- which is
    // exactly what a re-read must NOT do.
    await waitFor(() => expect(screen.getAllByRole("button", { name: /widget/ })).toHaveLength(2));
    await click(screen.getByRole("button", { name: "Look again" }));
    await waitFor(() => expect(screen.getAllByRole("button", { name: /widget/ })).toHaveLength(1));
  });

  it("keeps the last good list when the next read is refused", async () => {
    // A refusal is not a zero: blanking the picker would say the grant
    // reaches nothing, which is a different and untrue answer.
    //
    // The seed is MUTATED between the two clicks rather than the connection
    // being swapped. `useWrite` closes over the connection it was given, so
    // a second `fakeConnection` would never reach the mounted hook -- and
    // the fake reads its seed at call time, which is exactly the seam for
    // "this one worked, the next one did not".
    const seed: FakeSeed = {
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/widget" })] }),
    };
    h.connection = fakeConnection(seed);
    render(<PickerHost />);
    await click(screen.getByRole("button", { name: "Look again" }));
    await screen.findByRole("button", { name: /widget/ });

    seed.repositoriesError = "reconnect_required: GitHub refused this connection.";
    await click(screen.getByRole("button", { name: "Look again" }));
    expect(await screen.findByText("Your GitHub connection needs renewing")).toBeTruthy();
    expect(screen.getByText("GitHub refused this connection.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /widget/ })).toBeTruthy();
    expect(screen.getByText(/Showing 1 of 1, read/)).toBeTruthy();
  });

  it("reports the choice, and marks it", async () => {
    mountPicker({
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "acme/widget" })] }),
    });
    await click(screen.getByRole("button", { name: "Look again" }));
    const row = await screen.findByRole("button", { name: /widget/ });
    await click(row);
    expect(screen.getByRole("button", { name: /widget/ }).getAttribute("data-current")).toBe("true");
    expect(screen.getByText("chosen")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Settings > Sources, through the real app
// ---------------------------------------------------------------------------

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mountSources(seed: FakeSeed) {
  const connection = fakeConnection(seed);
  h.connection = connection;
  const view = render(
    withSession(
      <DeployablesApp sectionId="settings" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: "owner", userId: "u-me" },
    ),
  );
  return { connection, ...view };
}

async function sourcesGroup(): Promise<HTMLElement> {
  // A fieldset named by its legend: the settings-group semantics DESIGN.md
  // rule 8 keeps, which is a `group` and not a `region`.
  return await screen.findByRole("group", { name: "Sources" });
}

/**
 * jsdom will not navigate, and its `Location.assign` is not redefinable, so
 * the whole object is swapped for the duration of one case.
 *
 * Stubbed rather than injected into the hook: navigating the whole page IS
 * the AuthProvider's convention, and inventing a seam to avoid it would test
 * something this shell does not do.
 */
let restoreLocation: (() => void) | null = null;

function stubNavigation(): string[] {
  const assigned: string[] = [];
  const original = window.location;
  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: {
      assign: (url: string) => void assigned.push(String(url)),
      href: original.href,
      origin: original.origin,
      pathname: original.pathname,
      search: original.search,
      hash: original.hash,
    },
  });
  restoreLocation = () => {
    Object.defineProperty(window, "location", { configurable: true, writable: true, value: original });
    restoreLocation = null;
  };
  return assigned;
}

const GRANT = githubGrantRow({ id: "cred-grant" });

describe("Settings > Sources", () => {
  beforeEach(() => {
    clearParkedConnectReturn();
  });

  afterEach(() => {
    restoreLocation?.();
    h.connection = null;
  });

  it("offers Connect GitHub, and the token fallback, to somebody with no connection", async () => {
    mountSources({ credentials: [] });
    const group = await sourcesGroup();
    expect(within(group).getByRole("button", { name: "Connect GitHub" })).toBeTruthy();
    expect(
      within(group).getByText("Pick repositories from a list instead of pasting a URL and a token."),
    ).toBeTruthy();
    // The token path is NOT behind "Advanced": it is a legitimate first
    // choice for a host the app does not cover, and calling it advanced
    // would be a judgement about the person. It is named, listed and
    // addable in the same breath as the connection.
    expect(within(group).getByText("Access tokens")).toBeTruthy();
    expect(within(group).getByRole("button", { name: "Add a credential" })).toBeTruthy();
    expect(within(group).getByText(/No credentials yet. A public repository needs none/)).toBeTruthy();
    // No connection means nothing was asked of the cluster.
    expect(h.connection).toBeTruthy();
  });

  it("begins a connect on the click and navigates the whole page", async () => {
    const assigned = stubNavigation();
    const { connection } = mountSources({
      credentials: [],
      connectUrl: "https://github.com/login/oauth/authorize?client_id=x&state=y",
    });
    const group = await sourcesGroup();
    await click(within(group).getByRole("button", { name: "Connect GitHub" }));
    // The wire string, rendered by hand until the generated builder lands.
    expect(connection.callsNamed("githubConnectBegin")).toEqual([
      'builtin githubConnectBegin(returnPath: "/?connect=settings")',
    ]);
    await waitFor(() =>
      expect(assigned).toEqual(["https://github.com/login/oauth/authorize?client_id=x&state=y"]),
    );
  });

  it("renders a refused connect in place, under the OS headline", async () => {
    const { connection } = mountSources({
      credentials: [],
      // ANSWERED, not thrown: the engine's connectResult carries the reason as a
      // typed code on the row, and a fake that threw it modelled a wire that
      // does not exist.
      connectReason: "github_app_not_configured",
    });
    const group = await sourcesGroup();
    await click(within(group).getByRole("button", { name: "Connect GitHub" }));
    expect(await within(group).findByText("This cluster has no GitHub connection set up")).toBeTruthy();
    // The sentence for the answered code, beneath.
    expect(within(group).getByText("This cluster has no GitHub App configured.")).toBeTruthy();
    // WHO CAN CHANGE IT, by the name this product gives them -- it said "ask
    // an operator" once, to a cluster owner.
    expect(within(group).getByText(/A cluster owner sets GitHub up in Settings > Sources/)).toBeTruthy();
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(1);
  });

  // THE CLUSTER'S GITHUB APP, as this group meets it. The case above is what a
  // cluster that cannot say in advance still gets; one that can is asked as
  // the group opens.
  it("offers a cluster owner Set up GitHub in place of a Connect that cannot work", async () => {
    const assigned = stubNavigation();
    const { connection } = mountSources({
      credentials: [],
      githubApp: { configured: false, canSetup: true },
      appSetupUrl: "https://identity.example.test/auth/github/app/new?state=s1",
    });
    const group = await sourcesGroup();
    const setUp = await within(group).findByRole("button", { name: "Set up GitHub" });
    expect(within(group).queryByRole("button", { name: "Connect GitHub" })).toBeNull();

    // An organization with no login has nothing to be registered under, so
    // the act is ABSENT until there is one.
    await click(within(group).getByRole("radio", { name: "An organization" }));
    expect(within(group).queryByRole("button", { name: "Set up GitHub" })).toBeNull();
    await click(within(group).getByRole("radio", { name: "Your account" }));

    await click(within(group).getByRole("button", { name: "Set up GitHub" }));
    expect(setUp).toBeTruthy();
    await waitFor(() => expect(assigned).toEqual(["https://identity.example.test/auth/github/app/new?state=s1"]));
    expect(connection.callsNamed("githubAppSetupBegin")).toEqual([
      'builtin githubAppSetupBegin(returnPath: "/?connect=settings", organization: "")',
    ]);
  });

  it("tells somebody who is not a cluster owner who can, and offers them nothing to press", async () => {
    mountSources({ credentials: [], githubApp: { configured: false, canSetup: false } });
    const group = await sourcesGroup();
    expect(await within(group).findByText(/A cluster owner sets that up once; until then, add an access token below/)).toBeTruthy();
    expect(within(group).queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(within(group).queryByRole("button", { name: "Set up GitHub" })).toBeNull();
    // The other way in is still here, and is what the sentence points at.
    expect(within(group).getByRole("button", { name: "Add a credential" })).toBeTruthy();
  });

  it("shows a cluster owner which app the cluster uses, and removes one registered from here", async () => {
    const { connection } = mountSources({
      credentials: [],
      githubApp: { configured: true, source: "cluster", slug: "memql-on-lab", canSetup: true },
    });
    const block = await screen.findByRole("region", { name: "GitHub App" });
    const link = within(block).getByRole("link", { name: "memql-on-lab" });
    expect(link.getAttribute("href")).toBe("https://github.com/apps/memql-on-lab");
    expect(within(block).getByText(/registered from here/)).toBeTruthy();

    // ARMED FIRST: the sentence says what stops working, and what removing
    // does NOT do -- the app stays at GitHub.
    await click(within(block).getByRole("button", { name: "Remove" }));
    expect(connection.callsNamed("githubAppRemove")).toHaveLength(0);
    expect(within(block).getByText(/stays at GitHub until it is deleted there/)).toBeTruthy();
    await click(within(block).getAllByRole("button", { name: "Remove" }).at(-1)!);
    await waitFor(() => expect(connection.callsNamed("githubAppRemove")).toHaveLength(1));
    // ...and the status is read again, because the answer just changed.
    await waitFor(() => expect(connection.callsNamed("githubAppStatus").length).toBeGreaterThan(1));
  });

  it("offers no Remove for an app the deployment's environment sets", async () => {
    mountSources({
      credentials: [],
      githubApp: { configured: true, source: "environment", slug: "memql-ops", canSetup: false },
    });
    const block = await screen.findByRole("region", { name: "GitHub App" });
    expect(within(block).getByText(/set by the deployment's environment/)).toBeTruthy();
    // Absent, never disabled: it is not changed from here by anybody.
    expect(within(block).queryByRole("button", { name: "Remove" })).toBeNull();
  });

  it("shows the connected account, its reach, and where to install another", async () => {
    const { connection } = mountSources({
      credentials: [GRANT],
      installUrl: "https://github.com/apps/memql/installations/new",
    });
    const card = await screen.findByRole("region", { name: "GitHub" });
    // ONCE. The accent chip IS the fact -- a `Connected as @octocat` row
    // directly beneath a chip reading `@octocat` is the same duplication this
    // design already removed when it dropped `Reaches`, whose content the
    // installation chips were carrying (rule 7).
    expect(within(card).getAllByText("@octocat")).toHaveLength(1);
    expect(within(card).queryByText("Connected as")).toBeNull();
    expect(within(card).queryByText("Reaches")).toBeNull();
    // Until somebody asks GitHub, the reach is the COUNT the row itself
    // carries -- which is the honest resolution of a fact this cluster holds
    // installation ids for and no logins.
    expect(within(card).getByText("2 installations")).toBeTruthy();
    const link = await within(card).findByRole("link", { name: "Install on another organization" });
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noreferrer noopener");
    // The install URL is only learnable from a begin call, asked once.
    expect(connection.callsNamed("githubConnectBegin")).toEqual([
      'builtin githubConnectBegin(returnPath: "/?connect=settings")',
    ]);
  });

  it("asks GitHub which organizations only when somebody presses, and says when it looked", async () => {
    const { connection } = mountSources({
      credentials: [GRANT],
      repositories: repositoriesReply({
        installations: [{ id: "i-acme", login: "acme", accountType: "Organization" }],
        pending: [{ login: "beta-corp" }],
      }),
    });
    const group = await sourcesGroup();
    const card = within(group).getByRole("region", { name: "GitHub" });

    // OPENING SETTINGS DIALS NOBODY. The row carries installation ids, not
    // logins, so the count is what it can honestly say -- and the caption
    // says whose answer the rest would be.
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    expect(within(card).getByText("2 installations")).toBeTruthy();
    expect(within(group).getByText(/Which organizations, and any waiting for an owner/)).toBeTruthy();

    await click(within(group).getByRole("button", { name: "Check what it reaches" }));
    expect(connection.callsNamed("sourceRepositories")).toEqual([
      'builtin sourceRepositories(credentialId: "cred-grant", page: 1)',
    ]);
    // The logins replace the count, and the organization still waiting for
    // its owner becomes reachable on the surface built to show it -- in the
    // warn tone, because nobody has done anything wrong.
    expect(await within(card).findByText("acme")).toBeTruthy();
    expect(within(card).queryByText("2 installations")).toBeNull();
    expect(within(card).getByText("beta-corp pending").getAttribute("data-tone")).toBe("warn");
    expect(within(card).getByText(/An owner of beta-corp has to approve/)).toBeTruthy();
    // ...and the reading dates itself, exactly as the picker's footer does.
    expect(within(group).getByText(/Asked GitHub/)).toBeTruthy();
  });

  it("follows the connection live, with no reload", async () => {
    const { connection } = mountSources({ credentials: [GRANT] });
    const card = await screen.findByRole("region", { name: "GitHub" });
    expect(within(card).getByText("2 installations")).toBeTruthy();
    // Somebody installed the app on a third organization. That IS the change
    // this card exists to show, so it arrives on the credential broadcast.
    await emit(
      connection,
      "v1:platform:sourceCredential",
      githubGrantRow({ id: "cred-grant", installationIds: ["i-acme", "i-octocat", "i-beta"] }),
    );
    expect(await within(card).findByText("3 installations")).toBeTruthy();
    expect(within(card).queryByText("2 installations")).toBeNull();
  });

  it("disconnects in two steps, naming what will break, and asks for no typed name", async () => {
    const { connection } = mountSources({
      credentials: [GRANT],
      packages: [
        { id: "pkg-a", ownerUserId: "u-me", name: "widget", sourceKind: "repo", credentialId: "cred-grant", status: "active", createdAt: "2026-08-01T00:00:00Z" } as unknown as Row,
        { id: "pkg-b", ownerUserId: "u-me", name: "docs", sourceKind: "repo", credentialId: "cred-grant", status: "active", createdAt: "2026-08-01T00:00:00Z" } as unknown as Row,
      ],
    });
    const card = await screen.findByRole("region", { name: "GitHub" });
    await click(within(card).getByRole("button", { name: "Disconnect" }));
    // Naming every affected thing is what archive really contributes, and it
    // is what was kept; the TYPING was not, because this is one click to undo.
    expect(within(card).getByText(/2 sources fetch under this connection: widget, docs\./)).toBeTruthy();
    expect(within(card).getByText(/They will ask you to reconnect at their next fetch/)).toBeTruthy();
    expect(within(card).queryByText(/to confirm/)).toBeNull();
    expect(card.querySelectorAll("input")).toHaveLength(0);
    const confirm = within(card).getAllByRole("button", { name: "Disconnect" }).at(-1)!;
    expect(confirm.getAttribute("data-tone")).toBe("danger");
    await click(confirm);
    expect(connection.callsNamed("sourceCredentialRevoke")).toEqual([
      'builtin sourceCredentialRevoke(credentialId: "cred-grant")',
    ]);
  });

  it("renders a refused disconnect beside the button that produced it", async () => {
    mountSources({
      credentials: [GRANT],
      credentialRevokeError: "credential_not_found: That credential is not one you can use.",
    });
    const card = await screen.findByRole("region", { name: "GitHub" });
    await click(within(card).getByRole("button", { name: "Disconnect" }));
    await click(within(card).getAllByRole("button", { name: "Disconnect" }).at(-1)!);
    expect(await within(card).findByText("This source's credential is not one you can use")).toBeTruthy();
    expect(within(card).getByText("That credential is not one you can use.")).toBeTruthy();
  });

  it("offers a reconnect once the connection has been ended", async () => {
    // `reconnect_required`'s copy sends a person to Settings > Sources, so
    // the control it names has to be here.
    mountSources({ credentials: [githubGrantRow({ id: "cred-grant", status: "revoked" })] });
    const group = await sourcesGroup();
    expect(await within(group).findByRole("button", { name: "Reconnect GitHub" })).toBeTruthy();
    const card = within(group).getByRole("region", { name: "GitHub" });
    expect(within(card).getByText("disconnected").getAttribute("data-tone")).toBe("warn");
    // Nothing to disconnect twice, and nothing to ask GitHub: a lapsed grant
    // reads nothing, so a control that could only refuse is not offered.
    expect(within(card).queryByRole("button", { name: "Disconnect" })).toBeNull();
    expect(within(group).queryByRole("button", { name: "Check what it reaches" })).toBeNull();
  });

  it("lists a pasted credential beside what fetches under it, and revokes it by name", async () => {
    const { connection } = mountSources({
      credentials: [credentialRow({ id: "cred-1" })],
      packages: [
        { id: "pkg-a", ownerUserId: "u-me", name: "widget", sourceKind: "repo", repoUrl: "https://github.com/acme/widget", repoRef: "main", credentialId: "cred-1", status: "active", createdAt: "2026-08-01T00:00:00Z" } as unknown as Row,
      ],
    });
    const group = await sourcesGroup();
    // The name and the digest are two nodes, because they are two kinds of
    // thing: the label somebody chose, and the id that tells two cards apart.
    expect(await within(group).findByText("acme deploy token")).toBeTruthy();
    expect(within(group).getByText("sha256:ab12cd34").className).toContain("os-mono");
    // What a revoke would break, named before it is offered.
    expect(within(group).getByText("acme/widget at main")).toBeTruthy();
    await click(within(group).getByRole("button", { name: /Revoke acme deploy token/ }));
    await click(within(group).getByRole("button", { name: "Revoke" }));
    expect(connection.callsNamed("sourceCredentialRevoke")).toEqual([
      'builtin sourceCredentialRevoke(credentialId: "cred-1")',
    ]);
  });

  it("never renders anything token-shaped, on either path", async () => {
    const { container } = mountSources({
      credentials: [
        credentialRow({ id: "cred-1", token: FIXTURE_GITHUB_PAT }),
        githubGrantRow({ id: "cred-grant", encryptedValue: FIXTURE_GITHUB_PAT }),
      ],
    });
    const group = await sourcesGroup();
    // The reachable positive: the seed really does carry a token-shaped
    // string, and the cards it is attached to really did render.
    expect(FIXTURE_GITHUB_PAT.startsWith("ghp_")).toBe(true);
    expect(within(group).getByText(/acme deploy token/)).toBeTruthy();
    expect((await within(group).findAllByText("@octocat")).length).toBeGreaterThan(0);
    expect(container.textContent).not.toContain("ghp_");
    expect(container.textContent).not.toContain(FIXTURE_GITHUB_PAT);
    // Nor does anything token-shaped go OUT.
    expect(h.connection).toBeTruthy();
  });

  it("mounts no toast and no dialog for any of it", async () => {
    const { container } = mountSources({ credentials: [GRANT] });
    await sourcesGroup();
    expect(container.querySelector("[data-toast], .os-toast, dialog, [role='dialog']")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The compose Source stop, through the real app
// ---------------------------------------------------------------------------

// The three readings of the repository answer (design sections A and C), and
// what a connection changes about the stop that names where a deployable
// comes from. The Compose epic (memql#4885) left the mount point and kept the
// URL-plus-token form separable exactly so this could land above it; what is
// asserted here is which of the three a person gets, and that the third --
// the only one reachable on a cluster with no GitHub App -- is unchanged.

const URL_FIELD = "The repository this deployable is built from";
const BRANCH_FIELD = "Which branch or tag to deploy";
const NAME_FIELD = "What this deployable is called";

async function composeSource(seed: FakeSeed): Promise<{ connection: FakeConnection; region: HTMLElement }> {
  const connection = fakeConnection(seed);
  h.connection = connection;
  render(
    withSession(
      <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: "owner", userId: "u-me" },
    ),
  );
  await click(await screen.findByRole("button", { name: /Add a deployable/ }));
  const region = await screen.findByRole("region", { name: "Add a deployable" });
  await click(within(region).getByRole("radio", { name: /A repository/ }));
  return { connection, region };
}

/**
 * An act on the wizard's FLOOR. Connecting is the repository step's forward
 * act, and a wizard's forward act lives on its floor and nowhere else -- so
 * that is where these tests look for it, and where they press it.
 */
function floorAct(name: string): HTMLButtonElement | null {
  const hit = [...document.querySelectorAll(".os-actbar-acts button")].find((b) => (b.textContent ?? "").trim() === name);
  return (hit as HTMLButtonElement | undefined) ?? null;
}

const WIDGET = repositoryFixture({ fullName: "acme/widget", private: true, visibility: "private" });

function personalSource(credentials: Row[], userId = "u-me") {
  return withSession(<RepositorySource
    draft={EMPTY_DRAFT} onDraft={vi.fn()}
    credentials={credentials.map(credentialFromRow)}
    probe={{ reply: null, error: "", busy: false, probe: vi.fn(async () => {}), clear: vi.fn() }}
    tokenFormOpen={false} onTokenFormOpenChange={vi.fn()}
    /* The connect is the page's: it is the step's forward act, and a wizard's
       forward act lives on its floor. A step mounted alone gets an idle one. */
    connect={{ busy: false, refusal: null, installUrl: "", connect: vi.fn(async () => false), learn: vi.fn(async () => false), clear: vi.fn() } as never}
  />, { userId });
}

describe("personal repository connection lifecycle", () => {
  afterEach(() => { h.connection = null; });

  it("reads the grant when the socket becomes available after mount", async () => {
    h.connection = null;
    const view = render(personalSource([GRANT]));
    h.connection = fakeConnection({ repositories: repositoriesReply({ repositories: [WIDGET] }) });
    view.rerender(personalSource([GRANT]));
    expect(await screen.findByRole("button", { name: /widget/ })).toBeTruthy();
  });

  it("drops the old grant's list and ignores its late answer after replacement", async () => {
    const connection = fakeConnection({
      repositories: repositoriesReply({ repositories: [repositoryFixture({ fullName: "new-owner/new-project" })] }),
    });
    let finishOld!: (value: ReturnType<typeof builtinReply>) => void;
    vi.spyOn(connection.query, "sourceRepositories").mockImplementationOnce(() =>
      new Promise(resolve => { finishOld = resolve; }));
    h.connection = connection;
    const view = render(personalSource([GRANT]));
    view.rerender(personalSource([githubGrantRow({ id: "new-grant" })]));
    expect(await screen.findByRole("button", { name: /new-project/ })).toBeTruthy();
    await act(async () => finishOld(builtinReply("sourceRepositories", [repositoriesReply({ repositories: [WIDGET] })])));
    expect(screen.queryByRole("button", { name: /widget/ })).toBeNull();
    expect(screen.getByRole("button", { name: /new-project/ })).toBeTruthy();
  });

  it("removes the previous person's list and installation link on a viewer change", async () => {
    h.connection = fakeConnection({
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      installUrl: "https://github.com/apps/memql/installations/new",
    });
    const view = render(personalSource([GRANT]));
    await screen.findByRole("button", { name: /widget/ });
    view.rerender(personalSource([GRANT], "other-user"));
    // Mounted alone there is no floor to carry the act; the step says what
    // connecting is for, which is the half of it that is the step's.
    expect(screen.getByText(/Connect your GitHub account and pick from a list/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /widget/ })).toBeNull();
    expect(screen.queryByRole("link", { name: "Install on another organization" })).toBeNull();
  });
});


describe("disconnecting from the repository chooser", () => {
  afterEach(() => { h.connection = null; });

  async function confirmDisconnect(region: HTMLElement) {
    await click(within(region).getByRole("button", { name: "Disconnect" }));
    await click(within(region).getByRole("button", { name: "Disconnect" }));
  }

  it("names the account, allows cancellation, and leaves shared installations alone", async () => {
    const { connection, region } = await composeSource({ credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }) });
    await within(region).findByRole("button", { name: /widget/ });
    expect(within(region).getByText(/Connected to GitHub as @/)).toBeTruthy();
    await click(within(region).getByRole("button", { name: "Disconnect" }));
    expect(within(region).getByText(/The GitHub App stays installed/)).toBeTruthy();
    await click(within(region).getByRole("button", { name: "Cancel" }));
    expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(0);
    expect(within(region).getByRole("button", { name: /widget/ })).toBeTruthy();
  });

  it("clears selected repository and pending probe immediately before any broadcast, then offers fresh authorization", async () => {
    const { connection, region } = await composeSource({ credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }), installUrl: "https://github.com/apps/memql/installations/new" });
    let finishProbe!: (reply: ReturnType<typeof builtinReply>) => void;
    vi.spyOn(connection.query, "sourceProbe").mockImplementationOnce(() => new Promise(resolve => { finishProbe = resolve; }));
    await click(await within(region).findByRole("button", { name: /widget/ }));
    await confirmDisconnect(region);
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    expect(within(region).queryByRole("button", { name: /widget/ })).toBeNull();
    expect(within(region).queryByText("acme/widget at default branch")).toBeNull();
    expect(within(region).queryByRole("link", { name: "Install on another organization" })).toBeNull();
    await act(async () => finishProbe(builtinReply("sourceProbe", [probeReply({ branches: ["old-secret-branch"] })])));
    expect(within(region).queryByLabelText(BRANCH_FIELD)).toBeNull();
    expect(connection.callsNamed("sourceCredentialRevoke")).toEqual(['builtin sourceCredentialRevoke(credentialId: "cred-grant")']);
    const begins = connection.callsNamed("githubConnectBegin").length;
    await click(floorAct("Reconnect GitHub")!);
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(begins + 1);
    expect(connection.callsNamed("githubAppRemove")).toHaveLength(0);
  });

  it("keeps the confirmed disconnect when the repository step unmounts and reopens before its feed update", async () => {
    const { region } = await composeSource({ credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }) });
    await within(region).findByRole("button", { name: /widget/ });
    await confirmDisconnect(region);
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    await click(within(region).getByRole("button", { name: "Source A repository" }));
    await click(within(region).getByRole("button", { name: /^Repository(?: |$)/ }));
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    expect(within(region).queryByRole("button", { name: /widget/ })).toBeNull();
    expect(within(region).queryByText(/Connected to GitHub as/)).toBeNull();
  });

  it("keeps the account and list after a refused disconnect, and permits retry", async () => {
    const seed: FakeSeed = { credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }), credentialRevokeError: "credential_not_found: You cannot disconnect this credential." };
    const { region } = await composeSource(seed);
    await within(region).findByRole("button", { name: /widget/ });
    await confirmDisconnect(region);
    expect(await within(region).findByText("You cannot disconnect this credential.")).toBeTruthy();
    expect(within(region).getByText(/Connected to GitHub as @/)).toBeTruthy();
    expect(within(region).getByRole("button", { name: /widget/ })).toBeTruthy();
    delete seed.credentialRevokeError;
    await click(within(region).getByRole("button", { name: "Disconnect" }));
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
  });

  it("discards a selected repository and probe when GitHub reports the grant has lapsed", async () => {
    const { connection, region } = await composeSource({ credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }), sourceProbe: { "cred-grant": probeReply({ branches: ["main", "private-old-branch"] }) } });
    await click(await within(region).findByRole("button", { name: /widget/ }));
    expect(await within(region).findByText("acme/widget at default branch")).toBeTruthy();
    vi.spyOn(connection.query, "sourceRepositories").mockResolvedValueOnce(builtinReply("sourceRepositories", [repositoriesReply({ reason: "reconnect_required" })]));
    await click(within(region).getByRole("button", { name: "Look again" }));
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    expect(within(region).queryByText("acme/widget at default branch")).toBeNull();
    expect(within(region).queryByLabelText(BRANCH_FIELD)).toBeNull();
  });

  it("does not erase a pasted-token draft in the automatic no-app fallback", async () => {
    h.connection = fakeConnection({});
    const onDraft = vi.fn();
    render(withSession(<RepositorySource
      draft={{ ...EMPTY_DRAFT, choice: "repo", repoUrl: "https://github.com/acme/token-repo", credentialId: "token-only" }}
      onDraft={onDraft} credentials={[credentialFromRow(credentialRow({ id: "token-only", ownerUserId: "u-me" }))]}
      probe={{ reply: null, error: "", busy: false, probe: vi.fn(async () => {}), clear: vi.fn() }}
      tokenFormOpen={false} onTokenFormOpenChange={vi.fn()}
      connect={{ busy: false, refusal: { code: "github_app_not_configured", message: "No app." }, installUrl: "", connect: vi.fn(async () => {}), learn: vi.fn(async () => false), clear: vi.fn() }}
    />, { userId: "u-me" }));
    expect(await screen.findByDisplayValue("https://github.com/acme/token-repo")).toBeTruthy();
    expect(onDraft).not.toHaveBeenCalled();
  });

  it("keeps the partial remote-revocation warning when the revoked feed event arrives", async () => {
    const { connection, region } = await composeSource({ credentials: [GRANT], repositories: repositoriesReply({ repositories: [WIDGET] }), credentialRevokeRemote: false });
    await within(region).findByRole("button", { name: /widget/ });
    await confirmDisconnect(region);
    emit(connection, "v1:platform:sourceCredential", githubGrantRow({ id: "cred-grant", status: "revoked" }));
    expect(await within(region).findByText(/GitHub did not confirm that your personal authorization ended/)).toBeTruthy();
    expect(floorAct("Reconnect GitHub")).toBeTruthy();
  });
});

describe("the compose Source stop, with a connection", () => {
  afterEach(() => {
    h.connection = null;
  });

  it("offers a personal connection when an owner sees only a colleague's grant", async () => {
    const { connection, region } = await composeSource({
      credentials: [githubGrantRow({ id: "colleague-grant", ownerUserId: "u-colleague", login: "colleague" })],
    });
    await waitFor(() => expect(floorAct("Connect GitHub")).toBeTruthy());
    expect(connection.callsNamed("sourceRepositories")).toEqual([]);
    expect(within(region).queryByText("This connection reaches no repositories yet.")).toBeNull();
    await click(within(region).getByRole("radio", { name: "A token" }));
    expect(within(region).queryByRole("option", { name: /colleague/ })).toBeNull();
  });

  it("chooses the current user's grant even when another user's card sorts first", async () => {
    const { connection, region } = await composeSource({
      credentials: [
        githubGrantRow({ id: "colleague-grant", ownerUserId: "u-colleague" }),
        githubGrantRow({ id: "my-grant", ownerUserId: "v1:identity:user:u-me" }),
      ],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));
    expect(connection.callsNamed("sourceRepositories")[0]).toContain('credentialId: "my-grant"');
    expect(connection.callsNamed("sourceProbe")[0]).toContain('credentialId: "my-grant"');
    expect(connection.callsNamed("sourceProbe").join()).not.toContain("colleague-grant");
  });

  it.each(["credential_not_found", "credential_revoked", "reconnect_required"])("offers reconnect for an answered %s refusal", async reason => {
    const { region } = await composeSource({
      credentials: [GRANT], repositories: repositoriesReply({ reason }),
    });
    // GITHUB'S OWN ANSWER ABOUT THE GRANT arrives on the repository read, inside
    // the step -- so the step tells the page, and the floor offers the way back.
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    expect(within(region).getByText(/Your GitHub connection has lapsed/)).toBeTruthy();
    expect(within(region).queryByText("This connection reaches no repositories yet.")).toBeNull();
  });

  it("offers the GitHub installation page when the personal grant reaches nothing", async () => {
    const { region } = await composeSource({ credentials: [GRANT], installUrl: "https://github.com/apps/memql/installations/new" });
    const link = await within(region).findByRole("link", { name: "Install on another organization" });
    expect(link.getAttribute("href")).toContain("github.com");
  });

  it("reads the list on its own, so a connected person opens the stop and picks", async () => {
    const { connection, region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
    });

    // NOTHING WAS TYPED AND NOTHING WAS PRESSED. The measure of this surface
    // is that a connected person never notices it.
    expect(await within(region).findByRole("button", { name: /widget/ })).toBeTruthy();
    expect(connection.callsNamed("sourceRepositories")).toEqual([
      'builtin sourceRepositories(credentialId: "cred-grant", page: 1)',
    ]);
    // The token form is under it, closed: one answer on screen at a time.
    expect(within(region).queryByLabelText(URL_FIELD)).toBeNull();
    expect(within(region).getByRole("radio", { name: "A token" })).toBeTruthy();
  });

  it("fills the URL, the credential and the branches from the one it was given", async () => {
    const { connection, region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      sourceProbe: {
        "cred-grant": probeReply({ private: true, branches: ["main", "release", "spike"] }),
      },
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));

    // The probe was asked about the repository that was just chosen, UNDER
    // the grant -- which is what makes its answer about this connection.
    await waitFor(() =>
      expect(connection.callsNamed("sourceProbe")).toEqual([
        'builtin sourceProbe(repoUrl: "https://github.com/acme/widget", credentialId: "cred-grant")',
      ]),
    );
    // The name came with it, so nothing else has to be typed -- read as what
    // is on screen in the field, never off a `.value`.
    expect(within(region).getByDisplayValue("widget")).toBeTruthy();
    // ...and the rail's own answer says what was chosen.
    expect(within(region).getByText("acme/widget at default branch")).toBeTruthy();
  });

  it("offers the branches the probe answered, default first, and following it as its own answer", async () => {
    const { region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      sourceProbe: { "cred-grant": probeReply({ branches: ["main", "release"] }) },
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));

    await click(await within(region).findByLabelText(BRANCH_FIELD));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    // Following the default is a DIFFERENT answer from pinning the branch
    // that is the default today, so both are offered and the order is the
    // engine's: the default branch leads the names.
    expect(options).toEqual(["Follow the default branch", "main", "release"]);
  });

  it("draws no branch picker when the probe answered no branches", async () => {
    const { region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      sourceProbe: { "cred-grant": probeReply() },
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));
    await within(region).findByLabelText(NAME_FIELD);
    // An empty select is a control that can only be wrong.
    expect(within(region).queryByLabelText(BRANCH_FIELD)).toBeNull();
  });

  it("previews what the manifest claims at What it is, in the report's own vocabulary", async () => {
    const { region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      sourceProbe: {
        "cred-grant": probeReply({
          branches: ["main"],
          manifest: {
            name: "acme-storefront",
            deployables: [{ name: "web", kind: "static", path: "clients/web" }],
            dslDomains: ["shop"],
          },
        }),
      },
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));

    await click(await within(region).findByRole("button", { name: /^Review/ }));
    expect(await within(region).findByText("acme-storefront")).toBeTruthy();
    expect(within(region).getByText("web")).toBeTruthy();
    expect(within(region).getByText("clients/web")).toBeTruthy();
    expect(within(region).getByText("shop")).toBeTruthy();
    // It says what it IS: a claim read from the manifest, not a finding.
    expect(within(region).getByText(/Analyze reads the tree itself and is the authority/)).toBeTruthy();
  });

  it("says nothing at all about a repository with no manifest", async () => {
    const { region } = await composeSource({
      credentials: [GRANT],
      repositories: repositoriesReply({ repositories: [WIDGET] }),
      sourceProbe: { "cred-grant": probeReply({ branches: ["main"] }) },
    });
    await click(await within(region).findByRole("button", { name: /widget/ }));
    await within(region).findByLabelText(NAME_FIELD);

    // No preview AND no complaint: the analysis is the authority, and a
    // warning here would report a manifest problem twice.
    expect(within(region).queryByText(/Analyze reads the tree itself/)).toBeNull();
    expect(within(region).queryByText(/manifest/i)).toBeNull();
  });
});

describe("the compose Source stop, without one", () => {
  afterEach(() => {
    restoreLocation?.();
    h.connection = null;
  });

  it("offers Connect above the token form, and mints nothing until something is pressed", async () => {
    const { connection, region } = await composeSource({ credentials: [] });

    await waitFor(() => expect(floorAct("Connect GitHub")).toBeTruthy());
    // THE TWO WAYS IN ARE ONE CHOICE, as equals, with GitHub chosen.
    expect(within(region).getByRole("radio", { name: "GitHub" }).getAttribute("aria-checked")).toBe("true");
    expect(within(region).getByRole("radio", { name: "A token" }).getAttribute("aria-checked")).toBe("false");
    // Beginning a connect mints a state row, so it is never how the wizard
    // finds out whether this cluster has an app. The status is a READ, asked
    // once as the wizard opens -- and this fake answers it no row, which is
    // "not known", which keeps Connect on offer.
    expect(connection.callsNamed("githubAppStatus")).toHaveLength(1);
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(0);
    expect(connection.callsNamed("sourceRepositories")).toHaveLength(0);
    // The fold is closed, and one click away.
    expect(within(region).queryByLabelText(URL_FIELD)).toBeNull();
    await click(within(region).getByRole("radio", { name: "A token" }));
    expect(within(region).getByLabelText(URL_FIELD)).toBeTruthy();
    await click(within(region).getByRole("radio", { name: "GitHub" }));
    expect(within(region).queryByLabelText(URL_FIELD)).toBeNull();
  });

  it("says Reconnect, not Connect, once a connection has lapsed", async () => {
    const { region } = await composeSource({
      credentials: [githubGrantRow({ id: "cred-grant", status: "revoked" })],
    });
    await waitFor(() => expect(floorAct("Reconnect GitHub")).toBeTruthy());
    // A lapsed grant reads no repositories, so it is offered no picker.
    expect(within(region).queryByRole("button", { name: "Look again" })).toBeNull();
  });

  it("makes the token form the whole stop on a cluster with no GitHub App", async () => {
    const { region } = await composeSource({
      credentials: [],
      // ANSWERED, not thrown: the engine's connectResult carries the reason as a
      // typed code on the row, and a fake that threw it modelled a wire that
      // does not exist.
      connectReason: "github_app_not_configured",
    });
    await waitFor(() => expect(floorAct("Connect GitHub")).toBeTruthy());
    await click(floorAct("Connect GitHub")!);

    // The OS headline, the sentence for the answered code beneath it.
    const headline = await within(region).findByText("This cluster has no GitHub connection set up");
    expect(within(region).getByText("This cluster has no GitHub App configured.")).toBeTruthy();
    // WARN, never ERROR: an operator's condition rather than this person's,
    // and the fault colour would say they broke the cluster.
    expect(headline.closest("[data-tone]")?.getAttribute("data-tone")).toBe("warn");
    // A disabled Connect would invite somebody to fix a cluster that is not
    // theirs, so the control is gone -- and the form is the stop, open, with
    // no fold to find it behind.
    expect(within(region).queryByRole("button", { name: "Connect GitHub" })).toBeNull();
    expect(within(region).queryByRole("radio", { name: "A token" })).toBeNull();
    expect(within(region).getByLabelText(URL_FIELD)).toBeTruthy();
  });
});

// A CLUSTER WITH NO GITHUB APP, KNOWN BEFORE ANYBODY PRESSES. The reading
// above -- press Connect, be refused -- is what an engine that cannot say in
// advance still gets. One that can is asked as the wizard opens, and the step
// offers what can actually be done.
describe("the compose Source stop, on a cluster with no GitHub App", () => {
  afterEach(() => {
    restoreLocation?.();
    h.connection = null;
  });

  const NO_APP_OWNER = { configured: false, canSetup: true };

  it("offers a cluster owner Set up GitHub on the floor, and never Connect", async () => {
    const assigned = stubNavigation();
    const { connection, region } = await composeSource({
      credentials: [],
      githubApp: NO_APP_OWNER,
      appSetupUrl: "https://identity.example.test/auth/github/app/new?state=s1",
    });

    await waitFor(() => expect(floorAct("Set up GitHub")).toBeTruthy());
    expect(floorAct("Connect GitHub")).toBeNull();
    expect(within(region).getByText(/This cluster is not linked to GitHub yet/)).toBeTruthy();
    // ONE QUESTION: whose GitHub account the app is registered under. Their
    // own is the default, and it asks for nothing more.
    expect(within(region).getByRole("radio", { name: "Your account" }).getAttribute("aria-checked")).toBe("true");
    expect(within(region).queryByLabelText("The organization's GitHub login")).toBeNull();
    // Nothing has been minted by looking.
    expect(connection.callsNamed("githubAppSetupBegin")).toHaveLength(0);

    await click(floorAct("Set up GitHub")!);
    await waitFor(() => expect(assigned).toEqual(["https://identity.example.test/auth/github/app/new?state=s1"]));
    // The return path is THIS step, and an own-account registration names no
    // organization -- sent empty rather than omitted, one call shape.
    const call = connection.callsNamed("githubAppSetupBegin")[0]!;
    expect(call).toContain(`returnPath: ${JSON.stringify(returnPathFor("deployables"))}`);
    expect(call).toContain('organization: ""');
  });

  it("asks for the organization by login, and has no act until it is named", async () => {
    const assigned = stubNavigation();
    const { connection, region } = await composeSource({
      credentials: [],
      githubApp: NO_APP_OWNER,
      appSetupUrl: "https://identity.example.test/auth/github/app/new?state=s2",
    });
    await waitFor(() => expect(floorAct("Set up GitHub")).toBeTruthy());

    await click(within(region).getByRole("radio", { name: "An organization" }));
    // ABSENT, never disabled: there is nothing to register the app under yet,
    // and the floor says what it is waiting for.
    expect(floorAct("Set up GitHub")).toBeNull();
    expect(screen.getByText("Name the organization")).toBeTruthy();

    await typeInto(within(region).getByLabelText("The organization's GitHub login") as HTMLInputElement, " acme-labs ");
    await click(floorAct("Set up GitHub")!);
    await waitFor(() => expect(assigned).toHaveLength(1));
    expect(connection.callsNamed("githubAppSetupBegin")[0]).toContain('organization: "acme-labs"');
  });

  it("renders a refused setup in place, and navigates nowhere", async () => {
    const assigned = stubNavigation();
    const { region } = await composeSource({
      credentials: [],
      githubApp: NO_APP_OWNER,
      appSetupReason: "github_app_setup_invalid",
    });
    await waitFor(() => expect(floorAct("Set up GitHub")).toBeTruthy());
    await click(floorAct("Set up GitHub")!);

    expect(await within(region).findByText("That is not a GitHub organization's login.")).toBeTruthy();
    expect(assigned).toEqual([]);
    // The act is still there: the person corrects the login and asks again.
    expect(floorAct("Set up GitHub")).toBeTruthy();
  });

  it("offers somebody who is not a cluster owner no act at all, and says who can", async () => {
    const { connection, region } = await composeSource({
      credentials: [],
      githubApp: { configured: false, canSetup: false },
    });

    expect(await within(region).findByText(/A cluster owner sets that up once; until then, choose A token/)).toBeTruthy();
    // Rule 12: absent, never disabled. Not Connect, which cannot work, and not
    // Set up, which is not theirs.
    expect(floorAct("Connect GitHub")).toBeNull();
    expect(floorAct("Set up GitHub")).toBeNull();
    expect(within(region).queryByRole("radio", { name: "Your account" })).toBeNull();
    expect(connection.callsNamed("githubConnectBegin")).toHaveLength(0);
    // THE TOKEN IS ONE CHOICE AWAY, and the step is an ordinary one again.
    await click(within(region).getByRole("radio", { name: "A token" }));
    expect(within(region).getByLabelText(URL_FIELD)).toBeTruthy();
    expect(within(region).queryByText(/not linked to GitHub/)).toBeNull();
  });

  it("offers Connect as ever on a cluster that has one", async () => {
    const { region } = await composeSource({
      credentials: [],
      githubApp: { configured: true, source: "cluster", slug: "memql-on-example", canSetup: true },
    });
    await waitFor(() => expect(floorAct("Connect GitHub")).toBeTruthy());
    expect(floorAct("Set up GitHub")).toBeNull();
    expect(within(region).queryByText(/not linked to GitHub/)).toBeNull();
  });

  it("keeps Connect on offer when the status could not be read", async () => {
    // "Did not answer" is not "no app": reading it that way would take Connect
    // away from every cluster whose status read merely failed.
    const { region } = await composeSource({ credentials: [], githubAppError: "unknown builtin githubAppStatus" });
    await waitFor(() => expect(floorAct("Connect GitHub")).toBeTruthy());
    expect(within(region).queryByText(/not linked to GitHub/)).toBeNull();
  });
});

describe("what a disconnect says about GitHub", () => {
  afterEach(() => {
    h.connection = null;
  });

  async function disconnect(seed: FakeSeed): Promise<{ card: HTMLElement; connection: FakeConnection }> {
    const { connection } = mountSources(seed);
    const card = await screen.findByRole("region", { name: "GitHub" });
    await click(within(card).getByRole("button", { name: "Disconnect" }));
    await click(within(card).getAllByRole("button", { name: "Disconnect" }).at(-1)!);
    await waitFor(() => expect(connection.callsNamed("sourceCredentialRevoke")).toHaveLength(1));
    return { card, connection };
  }

  it("says nothing extra when both halves happened", async () => {
    const { card } = await disconnect({ credentials: [GRANT] });
    expect(within(card).queryByText(/GitHub did not confirm/)).toBeNull();
  });

  it("names the half that did not, because only the person can finish it", async () => {
    // The engine revokes the local row first and then tries GitHub, so a
    // disconnect that could not reach GitHub succeeded HERE and left the
    // authorization standing THERE. `--os-warn` and not `--os-error`: the
    // disconnect worked, and this is somebody's next step.
    const { card } = await disconnect({ credentials: [GRANT], credentialRevokeRemote: false });
    const said = await within(card).findByText(/GitHub did not confirm the authorization was ended/);
    expect(said.getAttribute("data-tone")).toBe("warn");
    expect(said.textContent).toContain("Applications in your GitHub settings");
  });
});
