import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({
  connection: null as unknown,
  mint: vi.fn(),
  revoke: vi.fn(),
}));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The two credential writes are the app's ONLY calls that are not named
// queries: they ride the connection's dispatcher through the SDK's identity
// surface, because the mint's plaintext must never reach the engine and the
// revoke's audit id comes back on the reply.
vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  mintAccountToken: (...args: unknown[]) => h.mint(...args),
  revokeAccountToken: (...args: unknown[]) => h.revoke(...args),
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { accountTokenRow, billingAccountRow, fakeConnection, rowsResult, withSession } =
  await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

// A BILLING account (`v1:identity:account`), which is what `mintAccountToken`
// gates on. It was a `v1:accounts:account` -- a CLIENT -- while this panel
// lived in the client detail, and every mint against one of those resolves
// ZERO ROWS through `query accountById`, which IS the refusal (memql#5013).
const ACCOUNT_ID = "v1:identity:account:b1";
const ACCOUNT_NAME = "Northwind Trading";
const SUBJECT = "v1:identity:user:me";
const CREDENTIAL_ID = "v1:identity:identity:tok1";

// ASSEMBLED FROM PARTS, deliberately. The repo's secret scanner matches
// `mql_<kind>_<43 base64url chars>` as one literal, and a test fixture that
// happens to reach that length would red the gitleaks lane on a file that
// contains no secret. Joining at runtime means no line here can ever match,
// whatever the fixture is later edited to say.
const PLAINTEXT = ["mql", "acct", "notARealCredentialOnlyATestFixture"].join("_");

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalAccountsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  return render(
    withSession(
      <AccountsApp
        sectionId="credentials"
        navigate={vi.fn()}
        askContext={() => {}}
        store={memoryStore()}
      />,
    ),
  );
}

/** Open the one billing account and wait for its credentials panel. */
async function openCredentials(connection: Conn): Promise<HTMLElement> {
  mount(connection);
  fireEvent.click(await screen.findByText(ACCOUNT_NAME));
  return screen.findByLabelText("Credentials");
}

function connectionWith(over: Partial<Parameters<typeof fakeConnection>[0]> = {}) {
  return fakeConnection({
    // `accounts`, NOT `clientAccountsAll`. The Credentials section reads
    // `v1:identity:account`; the client registry is a different concept that
    // happens to share the word.
    accounts: [billingAccountRow({ id: ACCOUNT_ID, name: ACCOUNT_NAME })],
    ...over,
  });
}

async function issue(panel: HTMLElement, label: string) {
  fireEvent.change(within(panel).getByLabelText("What is this credential for"), {
    target: { value: label },
  });
  await act(async () => {
    fireEvent.click(within(panel).getByRole("button", { name: "Issue a credential" }));
  });
}

/**
 * Every key and value held in this browser's storages.
 *
 * Used to assert the plaintext reaches NONE of them. The test that reads it
 * proves the sweep can see a value first -- an empty shim would make the
 * negative pass while measuring nothing.
 */
function storedStrings(): string[] {
  const out: string[] = [];
  for (const storage of [globalThis.localStorage, globalThis.sessionStorage]) {
    for (let i = 0; i < storage.length; i += 1) {
      const key = storage.key(i);
      if (key === null) continue;
      out.push(key, storage.getItem(key) ?? "");
    }
  }
  return out;
}

beforeEach(() => {
  h.connection = null;
  h.mint.mockReset();
  h.revoke.mockReset();
  h.mint.mockResolvedValue({
    success: true,
    plainToken: PLAINTEXT,
    identityId: CREDENTIAL_ID,
    accountId: ACCOUNT_ID,
    subjectUserId: SUBJECT,
    auditEventId: "v1:identity:auditEvent:ae1",
    errorCode: "",
    errorMessage: "",
  });
  h.revoke.mockResolvedValue({
    success: true,
    auditEventId: "v1:identity:auditEvent:ae2",
    errorCode: "",
    errorMessage: "",
  });
  globalThis.localStorage.clear();
  globalThis.sessionStorage.clear();
});

describe("issuing a credential", () => {
  it("mints against this billing account, under the label the operator typed", async () => {
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await issue(panel, "Nightly export job");

    expect(h.mint).toHaveBeenCalledTimes(1);
    expect(h.mint.mock.calls[0]?.[1]).toEqual({
      accountId: ACCOUNT_ID,
      label: "Nightly export job",
    });
  });

  it("SHOWS THE PLAINTEXT ONCE, and it is gone from the DOM the moment Done is pressed", async () => {
    // THE REGRESSION THIS EXISTS FOR. The credential crosses the wire once,
    // in the mint reply, and the cluster kept only its hash -- so the DOM is
    // the last place it lives, and "Done" has to be a discard rather than a
    // collapse. The assertion is over innerHTML rather than visible text
    // because a value tucked into a `title` or an `aria-label` is still a
    // copy of the secret sitting in the document.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await issue(panel, "Nightly export job");

    expect(await screen.findByText(PLAINTEXT)).toBeTruthy();
    expect(document.body.innerHTML).toContain(PLAINTEXT);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Done" }));
    });

    expect(screen.queryByText(PLAINTEXT)).toBeNull();
    expect(document.body.innerHTML).not.toContain(PLAINTEXT);
  });

  it("says out loud that it cannot be shown again", async () => {
    // Somebody who closes the panel expecting to come back for it has lost
    // the credential, so the sentence has to be on screen while it is.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await issue(panel, "Nightly export job");

    expect(
      await screen.findByText("Here is the credential. This is the only time it is shown."),
    ).toBeTruthy();
    expect(screen.getByText(/nowhere to look it up/)).toBeTruthy();
  });

  it("NEVER WRITES THE PLAINTEXT TO BROWSER STORAGE", async () => {
    // The OS persists a great deal to localStorage -- desks, surfaces, pins,
    // app settings -- which is exactly why this is asserted rather than
    // assumed: the habit of the code around this panel is to persist.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await issue(panel, "Nightly export job");
    await screen.findByText(PLAINTEXT);

    // A NEGATIVE OBSERVATION NEEDS A REACHABLE POSITIVE. An empty shim, or a
    // sweep that could not see into it, would pass the assertion below while
    // measuring nothing at all.
    globalThis.localStorage.setItem("os-test-probe", "a value the sweep must find");
    expect(storedStrings().some((v) => v.includes("a value the sweep must find"))).toBe(true);

    expect(storedStrings().some((v) => v.includes(PLAINTEXT))).toBe(false);
  });

  it("re-reads the list once a credential is issued", async () => {
    // The list is a read, not a feed: `v1:identity:identity` carries no
    // routing rule, so nothing broadcasts a new credential. Without the
    // re-read the panel would show a reveal for a row that never appears.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await waitFor(() => expect(conn.query.accountTokensForAccount).toHaveBeenCalledTimes(1));

    conn.query.accountTokensForAccount = vi.fn(async (_args: Record<string, unknown>) =>
      rowsResult([accountTokenRow({ id: CREDENTIAL_ID, label: "Nightly export job" })]),
    );
    await issue(panel, "Nightly export job");

    expect(await screen.findByText("Nightly export job")).toBeTruthy();
  });

  it("renders a refusal in the SERVER'S OWN WORDS", async () => {
    // A refusal comes back as an ordinary reply carrying success=false, not
    // as a rejection -- so a `try` alone reads it as a success and renders a
    // reveal with no credential in it. And the sentence is the server's:
    // authorization here is `accountById` run as the caller, with no
    // cluster-owner escape, so a guess composed in this browser would be a
    // guess printed in the cluster's voice.
    h.mint.mockResolvedValue({
      success: false,
      plainToken: "",
      identityId: "",
      accountId: ACCOUNT_ID,
      subjectUserId: "",
      auditEventId: "v1:identity:auditEvent:blocked1",
      errorCode: "permission_denied",
      errorMessage: "no account with that id is yours",
    });
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    await issue(panel, "Nightly export job");

    expect(await screen.findByText("no account with that id is yours")).toBeTruthy();
    // The audit id comes back on a BLOCKED attempt too, and it is what an
    // operator quotes when they ask why.
    expect(screen.getByText(/v1:identity:auditEvent:blocked1/)).toBeTruthy();
    // Nothing was revealed.
    expect(screen.queryByText(PLAINTEXT)).toBeNull();
  });

  it("does not offer to issue an unlabelled credential", async () => {
    // The cluster refuses one -- "an unlabelled credential cannot be revoked
    // with confidence" -- so this is the round trip the form can spare.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    const button = within(panel).getByRole("button", { name: "Issue a credential" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("what the surface says this credential IS", () => {
  it("names the operator's own user as the authenticated subject, and never the account", async () => {
    // The server echoes `subjectUserId` back precisely so a browser cannot
    // render "signed in as this account" without contradicting a field it was
    // handed. It is shown on the reveal and on every listed row.
    const conn = connectionWith({
      accountTokensForAccount: [accountTokenRow({ id: CREDENTIAL_ID })],
    });
    const panel = await openCredentials(conn);

    expect(await within(panel).findByText(SUBJECT)).toBeTruthy();
    expect(
      within(panel).getByText(/Nothing authenticates as a billing account/),
    ).toBeTruthy();

    await issue(panel, "Nightly export job");
    expect(await screen.findByText("Authenticates as")).toBeTruthy();
    // Two places now say the subject: the row and the reveal.
    expect(screen.getAllByText(SUBJECT).length).toBeGreaterThan(1);
  });

  it("says plainly that nothing on this cluster admits one yet", async () => {
    // A credential that grants nothing is a fact the person holding it needs
    // BEFORE they wire it into something, not after they discover it by
    // trying.
    const conn = connectionWith();
    const panel = await openCredentials(conn);
    expect(within(panel).getByText(/Nothing on this cluster accepts one of these yet/)).toBeTruthy();
  });
});

describe("revoking", () => {
  it("OFFERS NO REVOKE on a credential already revoked -- absent, not disabled", async () => {
    // DESIGN.md rule 12. A greyed control here would be a question the
    // surface refuses to answer, and the answer is that there is nothing
    // left to do.
    const conn = connectionWith({
      accountTokensForAccount: [
        accountTokenRow({ id: CREDENTIAL_ID, label: "Retired job", active: false }),
      ],
    });
    const panel = await openCredentials(conn);

    expect(await within(panel).findByText("Retired job")).toBeTruthy();
    // Still listed and MARKED: it is the record of a credential that existed,
    // and hiding it would make a revocation impossible to see taking effect.
    expect(within(panel).getByText("revoked")).toBeTruthy();
    expect(within(panel).queryByRole("button", { name: "Revoke Retired job" })).toBeNull();
    // Not merely hidden from the accessible name -- there is no revoke
    // control on this row at all.
    expect(within(panel).queryByText("Revoke")).toBeNull();
  });

  it("confirms in surface, naming the credential, then revokes by identity id", async () => {
    const conn = connectionWith({
      accountTokensForAccount: [accountTokenRow({ id: CREDENTIAL_ID, label: "Nightly export job" })],
    });
    const confirmSpy = vi.spyOn(globalThis, "confirm" as never);
    const panel = await openCredentials(conn);

    fireEvent.click(await within(panel).findByRole("button", { name: "Revoke Nightly export job" }));
    // IN SURFACE and NAMING the credential -- a browser confirm() blocks the
    // whole shell, and a generic "are you sure" invites the mistake it exists
    // to prevent when four credentials are listed.
    expect(
      await within(panel).findByRole("group", { name: "Revoke Nightly export job" }),
    ).toBeTruthy();
    expect(confirmSpy).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(
        within(panel).getByRole("button", { name: "Revoke Nightly export job now" }),
      );
    });

    expect(h.revoke).toHaveBeenCalledTimes(1);
    expect(h.revoke.mock.calls[0]?.[1]).toBe(CREDENTIAL_ID);
  });

  it("keeps the confirm open on a refusal, with the cluster's sentence beside it", async () => {
    // Closing it would leave an operator believing a revocation happened that
    // did not.
    h.revoke.mockResolvedValue({
      success: false,
      auditEventId: "",
      errorCode: "permission_denied",
      errorMessage: "that credential is not yours to revoke",
    });
    const conn = connectionWith({
      accountTokensForAccount: [accountTokenRow({ id: CREDENTIAL_ID, label: "Nightly export job" })],
    });
    const panel = await openCredentials(conn);

    fireEvent.click(await within(panel).findByRole("button", { name: "Revoke Nightly export job" }));
    await act(async () => {
      fireEvent.click(
        within(panel).getByRole("button", { name: "Revoke Nightly export job now" }),
      );
    });

    expect(await screen.findByText("that credential is not yours to revoke")).toBeTruthy();
    expect(within(panel).getByRole("button", { name: "Revoke Nightly export job now" })).toBeTruthy();
  });
});

describe("the read", () => {
  it("asks about this billing account", async () => {
    const conn = connectionWith();
    await openCredentials(conn);
    await waitFor(() =>
      expect(conn.query.accountTokensForAccount).toHaveBeenCalledWith(
        { accountId: ACCOUNT_ID },
        expect.anything(),
      ),
    );
  });

  it("A REFUSAL IS NOT AN EMPTY LIST", async () => {
    // "None" and "the cluster would not tell you" are different answers, and
    // rendering the second as the first is this window inventing a fact.
    const conn = connectionWith({
      accountTokensForAccount: new Error("reading credentials is not permitted here"),
    });
    const panel = await openCredentials(conn);

    expect(
      await within(panel).findByText(
        "The credentials for this billing account were not returned.",
      ),
    ).toBeTruthy();
    expect(within(panel).getByText("reading credentials is not permitted here")).toBeTruthy();
    expect(
      within(panel).queryByText("No credentials have been issued for this billing account."),
    ).toBeNull();
  });

  it("says so when a billing account has none", async () => {
    const conn = connectionWith({ accountTokensForAccount: [] });
    const panel = await openCredentials(conn);
    expect(
      await within(panel).findByText(
        "No credentials have been issued for this billing account.",
      ),
    ).toBeTruthy();
  });
});

describe("the projection", () => {
  it("reads an ABSENT `active` as active, so the revoke stays reachable", async () => {
    // The two wrong answers are not symmetric: reading an unreadable value as
    // "revoked" hides the only control that can stop a LIVE credential, while
    // reading it as "active" offers a revoke on something already revoked --
    // which the server answers idempotently. The recoverable mistake wins.
    const { accountTokenFromRow, accountTokenIsRevoked } = await import(
      "../../src/apps/accounts/credentials"
    );
    const projected = accountTokenFromRow({ id: CREDENTIAL_ID });
    expect(projected.active).toBe(true);
    expect(accountTokenIsRevoked(projected)).toBe(false);
    expect(accountTokenIsRevoked(accountTokenFromRow({ id: CREDENTIAL_ID, active: false }))).toBe(
      true,
    );
  });

  it("renames `userId` to `subjectUserId`, which is what it is", async () => {
    const { accountTokenFromRow } = await import("../../src/apps/accounts/credentials");
    expect(accountTokenFromRow(accountTokenRow({ id: CREDENTIAL_ID })).subjectUserId).toBe(SUBJECT);
  });

  it("never renders a blank name for an unlabelled credential", async () => {
    const { accountTokenFromRow, accountTokenLabel } = await import(
      "../../src/apps/accounts/credentials"
    );
    const projected = accountTokenFromRow({ id: CREDENTIAL_ID, label: "" });
    expect(accountTokenLabel(projected)).toBe("Unlabelled credential (tok1)");
  });

  it("orders newest first, so a fresh credential is where somebody looks for it", async () => {
    const { accountTokenFromRow, sortAccountTokens } = await import(
      "../../src/apps/accounts/credentials"
    );
    const rows = [
      accountTokenRow({ id: "v1:identity:identity:old", createdAt: "2026-08-01T00:00:00Z" }),
      accountTokenRow({ id: "v1:identity:identity:new", createdAt: "2026-09-05T00:00:00Z" }),
    ].map(accountTokenFromRow);
    expect(sortAccountTokens(rows).map((t) => t.id)).toEqual([
      "v1:identity:identity:new",
      "v1:identity:identity:old",
    ]);
  });
});
