import { useMemo, useState } from "react";
import { ArrowLeft, Landmark } from "lucide-react";

import {
  Button,
  Caption,
  Chip,
  Fact,
  Facts,
  Head,
  Notice,
  Panel,
  Row,
  formatMoment,
} from "../../kit";
import { CredentialsPanel } from "./CredentialsPanel";
import {
  billingAccountIsArchived,
  billingAccountIsSuspended,
  billingAccountName,
  type BillingAccountRow,
} from "./billing";
import { useBillingAccounts } from "./useBillingAccounts";

// Credentials: the billing accounts a credential can be issued against, and
// each one's credentials beneath it (memql#5013).
//
// ===========================================================================
// THE WHOLE SECTION EXISTS BECAUSE TWO CONCEPTS SHARE A WORD
// ===========================================================================
// This app's Accounts section lists `v1:accounts:account` -- the CLIENT
// REGISTRY (epic memql#4800): companies this instance's owner does work for.
// Everything here is `v1:identity:account` -- the PAYING account of the
// account-isolation model. `dsl/accounts/concepts.memql` states the whole
// relationship in one line: "they share a word and nothing else -- no field,
// no reference, no lifecycle."
//
// The credentials panel was first hung inside a CLIENT's detail, where it
// would have been refused on every mint: `mintAccountToken` runs
// `query accountById` as the caller (component/grpc/account_token_handlers.go)
// and that query binds `v1:identity:account`, so a client-registry id resolves
// ZERO ROWS -- and zero rows IS the refusal. Nothing would have said why; the
// operator would have read a permission error about an account they own.
//
// So the surface names which one it means, in ONE sentence, at the head. Not
// per row: a caveat repeated on every line is a caveat nobody reads, and it
// would also imply the distinction is per-account when it is per-CONCEPT.
//
// ===========================================================================
// DESIGN.md RULE 11: THE PAGE REPLACES THE LIST -- THE BACK-HEAD SHAPE
// ===========================================================================
// A list and its detail never share a scroll column, and there are two right
// answers: beside the list with its own scroller (`.os-bin-list`), or
// replacing it with a quiet back-Head (DeployablePage, and this epic's own
// fleet/apps/SessionPage). WHICH ONE DEPENDS ON HOW TALL THE DETAIL IS, and
// this detail is tall: an issue form, a one-time reveal carrying four facts
// and two controls, a refusal notice, and a list of credentials each of which
// can expand an in-surface revoke confirm. None of that fits the 380px column
// the beside-the-list shape caps its detail at, and the reveal in particular
// must never be the thing that has to be scrolled sideways to read.
//
// So the page REPLACES the list, with `<- Billing accounts` in its Head. ONE
// Head per rendered view; two Heads in one scroller is the tell that neither
// shape happened.

export function CredentialsSection() {
  const feed = useBillingAccounts();
  const [openId, setOpenId] = useState("");

  const open = useMemo(
    () => feed.accounts.find((a) => a.id === openId) ?? null,
    [feed.accounts, openId],
  );

  // THE PAGE, when one is chosen AND still in the answer. A re-read that no
  // longer carries the open row falls back to the list rather than rendering a
  // page about nothing -- an account can be re-read away, and a page whose
  // subject vanished would offer an issue control bound to an id the cluster
  // just declined to return.
  if (open !== null) {
    return <BillingAccountPage account={open} onBack={() => setOpenId("")} />;
  }

  return <BillingAccountList feed={feed} onOpen={setOpenId} />;
}

function BillingAccountList({
  feed,
  onOpen,
}: {
  feed: ReturnType<typeof useBillingAccounts>;
  onOpen: (accountId: string) => void;
}) {
  return (
    <div className="os-app-stack">
      <Head
        title="Credentials"
        // SAY IT ONCE (rule 8's sibling, rule 7). The count is absent at zero
        // rather than reading "0 billing accounts" above a sentence that
        // already says there are none -- and absent while the read is still in
        // flight, because a count nobody has been given yet is not zero.
        meta={
          feed.state === "ready" && feed.accounts.length > 0
            ? countLabel(feed.accounts.length)
            : undefined
        }
      >
        {/* The one control, and it is not a primary act: nothing on this
            surface CREATES a billing account. `createAccount` is an identity
            mutation with no client-reachable surface in this OS, so offering
            an Add here would be a button that could only fail. */}
        <Button onClick={feed.reload} disabled={feed.state === "loading"}>
          Re-read
        </Button>
      </Head>

      {/* THE DISAMBIGUATION, SAID ONCE. Body ink rather than a notice: it is
          not a warning about something going wrong, it is the one fact a
          reader needs before the list means anything. `.os-bin-lede` sets the
          precedent for prose that is the app talking. */}
      <p className="os-credentials-lede">
        These are <strong>billing accounts</strong> (<code className="os-mono">
          v1:identity:account
        </code>) -- the paying subjects a credential is issued against -- and not the clients
        listed under Accounts in this app, which share the word and nothing else.
      </p>

      <BillingAccountRows feed={feed} onOpen={onOpen} />
    </div>
  );
}

/**
 * The list, or the state that stands in for it.
 *
 * A REFUSAL IS NOT AN EMPTY LIST -- the rule the credentials read one level
 * down already states. "You have no billing accounts" and "the cluster would
 * not tell you" are different answers, and rendering the second as the first
 * is this window inventing a fact about what somebody owns.
 */
function BillingAccountRows({
  feed,
  onOpen,
}: {
  feed: ReturnType<typeof useBillingAccounts>;
  onOpen: (accountId: string) => void;
}) {
  if (feed.state === "error") {
    return (
      <Notice
        tone="error"
        sentence="This cluster did not return your billing accounts."
        next="This is not the same as there being none -- nothing was read, so nothing below is missing on purpose."
        detail={feed.error}
      >
        <Button onClick={feed.reload}>Try again</Button>
      </Notice>
    );
  }

  if (feed.state === "idle" || feed.state === "loading") {
    return <p className="os-caption">Reading</p>;
  }

  if (feed.accounts.length === 0) {
    // AN EMPTY ANSWER IS SAID OUT LOUD, never rendered as nothing. An empty
    // region is indistinguishable from a region that failed to render, and
    // this one has a second reading a reader needs: the read succeeded and
    // came back with none, which is what a cluster that has never created one
    // looks like.
    return (
      <p className="os-caption os-credentials-empty">
        You have no billing accounts, so there is nothing a credential can be issued against yet.
        These are created by the identity service, not from this window -- the clients under
        Accounts are a different set of rows and cannot stand in for one.
      </p>
    );
  }

  return (
    <>
      <ul className="os-credentials-accounts" aria-label="Billing accounts">
        {feed.accounts.map((account) => (
          <li key={account.id}>
            <BillingAccountLine account={account} onOpen={() => onOpen(account.id)} />
          </li>
        ))}
      </ul>
      {feed.readAt === "" ? null : (
        <Caption>
          Read at {new Date(feed.readAt).toLocaleTimeString()}. This list is not live -- Re-read is
          what refreshes it.
        </Caption>
      )}
    </>
  );
}

function BillingAccountLine({
  account,
  onOpen,
}: {
  account: BillingAccountRow;
  onOpen: () => void;
}) {
  const archived = billingAccountIsArchived(account);
  const suspended = billingAccountIsSuspended(account);
  return (
    <Row
      icon={<Landmark size={16} aria-hidden />}
      name={billingAccountName(account)}
      // `current` is the row's own liveness. A closed account keeps its facts
      // and loses its ink -- and stays reachable, because its credentials
      // still work until somebody revokes them.
      current={!archived}
      dim={archived}
      onOpen={onOpen}
      state={
        <>
          {archived ? <Chip tone="muted">archived</Chip> : null}
          {suspended ? <Chip tone="muted">suspended</Chip> : null}
        </>
      }
    >
      {account.description === "" ? null : (
        <span className="os-caption">{account.description}</span>
      )}
      {account.externalRef === "" ? null : (
        <span
          className="os-caption os-mono"
          title="The id for this billing account in whatever system is upstream of MemQL"
        >
          {account.externalRef}
        </span>
      )}
    </Row>
  );
}

/**
 * One billing account, and its credentials.
 *
 * THE HEAD CARRIES THE BACK, and this view renders exactly one Head
 * (DESIGN.md rule 11). The facts panel above the credentials is what makes the
 * subject unambiguous at the moment somebody is about to issue something: the
 * id shown here is the id the reveal will echo back as "On behalf of".
 *
 * THE TWO `@pii` FIELDS ARE NOT HERE. `primaryContactName` and
 * `primaryContactEmail` are `@pii` on the concept -- personal data about a
 * third party -- and a credentials surface has no use for either. They are
 * absent from the PROJECTION as well (see billing.ts), so this is not a
 * markup omission that a later edit could undo by accident.
 */
function BillingAccountPage({
  account,
  onBack,
}: {
  account: BillingAccountRow;
  onBack: () => void;
}) {
  const name = billingAccountName(account);
  const archived = billingAccountIsArchived(account);
  return (
    <div className="os-app-stack">
      <Head title={name}>
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Billing accounts
        </Button>
      </Head>

      <Panel label={`About ${name}`}>
        <Facts>
          <Fact label="Status" value={account.status} />
          <Fact
            label="Billing account"
            value={account.id}
            mono
            title="The v1:identity:account row a credential is issued against"
          />
          <Fact label="Description" value={account.description} />
          <Fact label="External reference" value={account.externalRef} mono />
          <Fact
            label="Last updated"
            value={account.updatedAt === "" ? "" : formatMoment(account.updatedAt)}
          />
          {archived ? (
            <Fact
              label="Closed"
              value={account.archivedAt === "" ? "" : formatMoment(account.archivedAt)}
            />
          ) : null}
        </Facts>
        {archived ? (
          // STATED, NOT ENFORCED. Whether the engine still mints against a
          // closed account is the engine's answer to give -- `accountById`
          // carries no status conjunct, so it very likely does -- and guessing
          // it here would hide a control that works. Rule 12 governs acts this
          // surface HOLDS the legality of; this is not one.
          <Caption>
            This billing account is closed. Its credentials still exist and still work until they
            are revoked, which is why they are still reachable here.
          </Caption>
        ) : null}
      </Panel>

      <CredentialsPanel accountId={account.id} accountLabel={name} />
    </div>
  );
}

/** A quiet count for the Head's meta slot. */
function countLabel(n: number): string {
  return n === 1 ? "1 billing account" : `${n} billing accounts`;
}
