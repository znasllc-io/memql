import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Accounts app's test harness: a connection-shaped double.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason
// the Users and Fleet harnesses record: every read goes through
// `connection.query.<generated method>` and every subscription through
// `connection.subscriptions`, so a fake that answers those exercises the real
// LiveCollection, the real retain/seed path, the real projections and the real
// arrival fold -- which is where the behaviour under test actually lives.

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

export function bundleResult(rows: Row[]): Result {
  const nodes = rows.map((row) => {
    const { id, createdAt, ...fields } = row as Record<string, unknown>;
    return { id, createdAt, payload: fields };
  });
  return new Result({ bundle: { nodes } } as never);
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  emit: (concept: string, payload: Row, kind?: string) => void;
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler, opts) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const handler of handlers.get(concept) ?? []) {
        handler({
          subscriptionId: "sub-1",
          kind,
          timestamp: new Date(),
          payload,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
        });
      }
    },
  };
}

export interface FakeSeed {
  clientAccountsAll?: Row[];
  senderIdentities?: Row[];
  /** v1:platform:accountFrontDoor rows, for the MemQL address stop. */
  accountFrontDoorsOpen?: Row[];
  sitesForAccount?: Row[];
  libraryItemsForAccount?: Row[];
  domainsForAccount?: Row[];
  /** Pass an Error to make the invitation rollup REFUSE, which is the
   *  interesting case: it is the one band the engine gates. */
  invitationsForAccount?: Row[] | Error;
  campaignsForAccount?: Row[] | Error;
  /** The credentials issued on behalf of a BILLING account (memql#5013). An
   *  Error makes the read REFUSE, which is a first-class state: "none" and
   *  "the cluster would not tell you" are different answers and the panel
   *  renders them differently. */
  accountTokensForAccount?: Row[] | Error;
  /**
   * `v1:identity:account` -- the BILLING accounts, which the Credentials
   * section lists (memql#5013).
   *
   * A SEPARATE KEY FROM `clientAccountsAll`, AND THAT IS THE POINT. The two
   * reads answer over different concepts that share the word "account", and a
   * harness with one key for both would let a surface read the wrong concept
   * and still pass -- which is exactly the defect memql#5013 fixed.
   */
  accounts?: Row[] | Error;
  /** The groups that grant this client -- the People band's first read. */
  groupsForAccount?: Row[] | Error;
  /** Membership rows keyed by group id -- the band's fan-out. */
  membersOfGroup?: Record<string, Row[]>;
  byId?: Record<string, Row>;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const rollup = (rows: Row[] | Error | undefined) =>
    vi.fn(async () => {
      if (rows instanceof Error) throw rows;
      return rowsResult(rows ?? []);
    });
  return {
    query: {
      configureAccountDomains: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      senderIdentities: rollup(seed.senderIdentities),
      clientAccountsAll: vi.fn(async () => rowsResult(seed.clientAccountsAll ?? [])),
      // The account front-door feed (epic memql#5168). Seeded like every other
      // rollup so a harness that says nothing about doors gets an empty feed
      // rather than an unstubbed call.
      accountFrontDoorsOpen: vi.fn(async () => rowsResult(seed.accountFrontDoorsOpen ?? [])),
      sitesForAccount: rollup(seed.sitesForAccount),
      libraryItemsForAccount: rollup(seed.libraryItemsForAccount),
      domainsForAccount: rollup(seed.domainsForAccount),
      invitationsForAccount: rollup(seed.invitationsForAccount),
      // The fifth band (epic memql#4819). Stubbed to EMPTY rather than left
      // unstubbed, because an unstubbed read throws and the band renders the
      // refusal -- which would make the "a refusal is not a zero" assertion
      // below match two elements and fail for a reason that has nothing to do
      // with what it is testing.
      campaignsForAccount: rollup(seed.campaignsForAccount),
      // The credentials panel's read (memql#5013). TYPED ARGS for the reason
      // the writes below carry them: a test that asserts the panel asked
      // about THIS client cannot do it through an empty parameter list. It is
      // re-read after every issue and revoke, so a test that wants the second
      // answer to differ from the first reassigns this the way `app.test.tsx`
      // reassigns `clientAccountsAll`.
      accountTokensForAccount: vi.fn(async (_args: Record<string, unknown>) => {
        if (seed.accountTokensForAccount instanceof Error) throw seed.accountTokensForAccount;
        return rowsResult(seed.accountTokensForAccount ?? []);
      }),
      // The Credentials section's read (memql#5013): `v1:identity:account`,
      // NOT the client registry. TYPED ARGS so a test can assert the section
      // asked with no status narrowing -- every status is wanted, because a
      // closed billing account's credentials still work until revoked.
      accounts: vi.fn(async (_args: Record<string, unknown>) => {
        if (seed.accounts instanceof Error) throw seed.accounts;
        return rowsResult(seed.accounts ?? []);
      }),
      // TYPED ARGS, so `.mock.calls[0][0]` is a record rather than `never` --
      // a test that asserts WHICH arguments a write received cannot do it
      // through a `vi.fn(async () => ...)` whose parameter list is empty.
      createClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      archiveClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      // The People band's two reads (epic memql#5167, section C). Stubbed to
      // EMPTY rather than left unstubbed, for the reason `campaignsForAccount`
      // is: an unstubbed read throws and the band renders a refusal, which
      // would make an unrelated "a refusal is not a zero" assertion match two
      // elements and fail for a reason that has nothing to do with it.
      groupsForAccount: vi.fn(async (_args: Record<string, unknown>) => {
        if (seed.groupsForAccount instanceof Error) throw seed.groupsForAccount;
        return rowsResult(seed.groupsForAccount ?? []);
      }),
      membersOfGroup: vi.fn(async (args: Record<string, unknown>) => {
        const groupId = typeof args["groupId"] === "string" ? args["groupId"] : "";
        return rowsResult(seed.membersOfGroup?.[groupId] ?? []);
      }),
      executeNamed: vi.fn(async (_name: string, filter: string) => {
        const match = /id==(\S+)/.exec(filter);
        const wanted = match?.[1] ?? "";
        const row = wanted === "" ? undefined : seed.byId?.[wanted];
        return bundleResult(row ? [row] : []);
      }),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export type FakeConnection = ReturnType<typeof fakeConnection>;

export function withSession(
  children: ReactNode,
  overrides: {
    role?: string;
    /** The groups MyAccess reports for this caller (epic memql#5165, H). */
    groups?: { id: string; name: string; kind: string; accountId: string; accountName: string }[];
  } = {},
) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          role: overrides.role ?? "owner",
          roleName: "",
          rank: 0,
          // ABSENT rather than empty by default, which is the difference the
          // type states: a harness constructing a session by hand is not
          // making a claim about anybody's groups.
          ...(overrides.groups === undefined ? {} : { groups: overrides.groups }),
        },
        config,
      }}
    >
      {children}
    </SessionProvider>
  );
}

/** An account row with sane defaults, overridable field by field. */
export function accountRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: "Acme Consulting",
    domain: "acme.com",
    primaryContactName: "Dana",
    primaryContactEmail: "dana@acme.com",
    notes: "",
    status: "active",
    // CONFIGURED BY DEFAULT, so a test that wants the first-run card has to
    // ASK for it. The card is the surprising state, and a harness whose
    // default produced it would make every unrelated test render a form.
    configuredAt: "2026-08-01T00:00:00Z",
    ownerUserId: "",
    // The domain walk's fields (epic memql#5165, section F). UNVERIFIED by
    // default: proving a domain is the state somebody has to reach, and a
    // harness whose default was "proven" would let every case about the
    // Ownership stop pass without the stop ever being unproven.
    // BUILT FROM OBVIOUSLY-FAKE PARTS, and joined rather than written whole.
    // A scanner judges a test fixture exactly like production: the plausible
    // token this used to hold ("memql-verify-abc123") tripped gitleaks'
    // generic-api-key rule at entropy 3.93, and a fixture cannot be dismissed
    // as a false positive without teaching everybody that this rule is noise.
    //
    // DO NOT "FIX" THIS TO LOOK REAL. The production value is
    // `base64.RawURLEncoding` over 32 crypto/rand bytes -- 43 characters at
    // maximal entropy with no prefix -- so a fixture that imitates it
    // faithfully trips the rule HARDER than the one that was caught here.
    // There is no realistic spelling that passes, which makes joined-from-words
    // the convention rather than a workaround.
    //
    // The stop renders whatever this says, so the words are the assertion too.
    domainToken: ["memql", "verify", "example", "not", "a", "real", "token"].join("-"),
    domainStatus: "unverified",
    domainFailureReason: "",
    domainFailureDetail: "",
    domainLastCheckedAt: "",
    domainVerifiedAt: "",
    joinOnDomain: false,
    memqlDomain: "",
    memqlReservedAt: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

/**
 * A credential row as `accountTokensForAccount` returns it, overridable field
 * by field.
 *
 * ACTIVE BY DEFAULT, so a test that wants a revoked one has to ASK. The
 * revoked case is the surprising state -- it is the one where an act
 * disappears from the surface -- and a harness whose default produced it
 * would make every unrelated assertion measure the wrong row.
 *
 * `userId` rather than `subjectUserId`: this is the WIRE shape
 * (`accountTokenSummary` keys `userId` bare), and a fixture that spelled it
 * the way the projection does would make `accountTokenFromRow` untested by
 * every test that uses it.
 */
/**
 * A `v1:identity:account` row as `accounts` returns it -- a BILLING account,
 * the paying subject of the isolation model.
 *
 * DELIBERATELY NOT `accountRow`, and the two must never be merged. That
 * fixture is `v1:accounts:account`, the CLIENT registry; these are different
 * concepts sharing a word, with no field and no reference between them. A
 * shared fixture would let a surface read the wrong concept and stay green,
 * which is the whole of memql#5013.
 *
 * ACTIVE BY DEFAULT, so a test that wants a closed one has to ASK.
 */
export function billingAccountRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: "Northwind Trading",
    status: "active",
    description: "The paying account behind the nightly export",
    externalRef: "CRM-4471",
    archivedAt: "",
    updatedAt: "2026-09-01T09:00:00Z",
    ownerUserId: "v1:identity:user:me",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

export function accountTokenRow(over: Partial<Row> & { id: string }): Row {
  return {
    userId: "v1:identity:user:me",
    label: "Nightly export job",
    active: true,
    // A BILLING account id (`v1:identity:account`), because that is what a
    // credential is bound to. A client-registry id here would make every
    // credentials test agree with the defect memql#5013 fixed.
    accountId: "v1:identity:account:b1",
    mintedBy: "v1:identity:user:me",
    expiresAt: "",
    lastUsedAt: "",
    createdAt: "2026-09-01T09:00:00Z",
    ...over,
  };
}
