import { Field, Notice } from "../../../../kit";
import { AccountChip, AccountPicker } from "../../../accounts/AccountPicker";
import { accountNameFrom, type AccountRow } from "../../../accounts/rows";
import { Caption } from "../../../../kit";
import { useAccountPeople } from "../../../accounts/useAccounts";
import { useSiteAccount } from "../../actions";
import { liveUrlFor, type SiteRow } from "../../rows";
import { DomainsContent } from "./Domains";

// The Where-it-lives stop: the address, the client, and a cluster owner's
// domains.
//
// On a deployable that exists the stop is facts (design section C): the
// address it answers at, the client it is for, and each bound domain's
// stepped rail with its two records and what the sweep last saw -- the
// Domains panel's content mounted as the stop rather than as a panel beside
// it. "Chosen once. A later deploy of this source keeps the same addresses."
// is the compose reading's sentence; here the address is simply what it is.

export function WhereItLivesStop({
  site,
  accounts,
  canBindDomain,
  clusterDomain,
}: {
  site: SiteRow;
  accounts: AccountRow[];
  /** The `domains` part (epic memql#5289): binding or removing a client's own domain. */
  canBindDomain: boolean;
  clusterDomain: string;
}) {
  const tie = useSiteAccount();
  const url = liveUrlFor(site.hostname);

  return (
    <div className="os-stop-body">
      {/* The address is CONTENT, and it is the link: the one thing on the
          page whose text is the thing it opens. The client chip sits beside
          it (epic memql#4800, D5) because "who is this for" is the same class
          of fact as "where is it"; a site with no client renders exactly as
          it did before -- AccountChip draws nothing for an empty name. */}
      <p className="os-stop-address">
        {url === "" ? (
          <code className="os-mono">{site.hostname || "--"}</code>
        ) : (
          <a className="os-mono" href={url} target="_blank" rel="noreferrer noopener">
            {site.hostname}
          </a>
        )}
        <AccountChip name={accountNameFrom(accounts, site.accountId)} />
      </p>

      {/* The client picker. Presentation over engine truth: an account is a
          record with no read effect, so setting one changes who the work is
          FOR and nothing about who may read or write this deployable. It is
          NOT behind `canWrite` for that reason -- labelling a site with the
          client it belongs to is not a privileged act, and the engine's own
          write guard is what decides whether the write lands. */}
      <Field label="Client">
        <AccountPicker
          id={`os-deploy-account-${site.id}`}
          label="The client this deployable is for"
          value={site.accountId}
          accounts={accounts}
          disabled={tie.busy}
          onChange={(next) => void tie.setAccount(site.id, next)}
        />
      </Field>
      {/* WHO CAN SEE IT, in one sentence, read when the stop opens (epic
          memql#5167, section D). The tie decides more than filing now: a
          group tied to a client is what lets that client's people reach the
          rows tied to it, so "which client is this for" and "who can see it"
          became the same question -- and this stop is where the first is
          answered.

          READ ON OPEN, NOT LIVE. It is one number about a client, on a stop
          somebody opened deliberately; a subscription per open stop over two
          concepts would be the Deployables timeline argument again. */}
      <VisibleTo accountId={site.accountId} accounts={accounts} />

      {tie.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The client was not changed."
          next="This deployable is still tied to whatever it was."
          detail={tie.error}
        />
      )}

      {/* Binding a client's own domain is the `domains` part (epic memql#5289),
          seeded on owner and developer and grantable by name; the engine's
          capability gate on customDomainAdd / removeCustomDomain and the three
          Go guards are the authority, and rendering it is the presentation
          half. */}
      {canBindDomain ? <DomainsContent site={site} domain={clusterDomain} /> : null}
    </div>
  );
}

/**
 * The one sentence under the client picker.
 *
 * THREE READINGS, and the third is silence: a deployable with no client says
 * nothing at all, because "visible to nobody else" would be a claim about a
 * tie that does not exist. A client with no people says so plainly -- that is
 * a real state and the repair is in Users.
 *
 * IT DOES NOT COUNT THE STANDING STAFF, the same honesty the Accounts band
 * keeps: developer rank and above reach every client's work by RULE with no
 * rows anywhere, so a number that included them would be this stop deciding
 * who the cluster's staff are.
 */
function VisibleTo({
  accountId,
  accounts,
}: {
  accountId: string;
  accounts: AccountRow[];
}) {
  const people = useAccountPeople(accountId);
  if (accountId === "") return null;
  if (people.state !== "ready") return null;
  const name = accountNameFrom(accounts, accountId);
  if (people.people === 0) {
    return <Caption>Visible to nobody else yet.</Caption>;
  }
  return (
    <Caption>
      Visible to {name === "" ? "this client" : `${name}'s`}{" "}
      {people.people === 1 ? "1 person" : `${people.people} people`}.
    </Caption>
  );
}
