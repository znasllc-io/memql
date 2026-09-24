import { Field, Notice } from "../../../../kit";
import { AccountChip, AccountPicker } from "../../../accounts/AccountPicker";
import { accountNameFrom, type AccountRow } from "../../../accounts/rows";
import { Caption } from "../../../../kit";
import { useAccountPeople } from "../../../accounts/useAccounts";
import { useSiteAccount } from "../../actions";
import type { SiteRow } from "../../rows";
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
  onOpenDomain,
  onAddDomain,
}: {
  site: SiteRow;
  accounts: AccountRow[];
  /** The `domains` part (epic memql#5289): binding or removing a client's own domain. */
  canBindDomain: boolean;
  clusterDomain: string;
  /** A domain row was opened: the page shows that binding's setup. */
  onOpenDomain: (domainId: string) => void;
  /** The list's Add control: the page shows the add form. */
  onAddDomain: () => void;
}) {
  const tie = useSiteAccount();

  return (
    <div className="os-stop-body">
      {/* THE ADDRESS IS IN THE DOMAINS LIST, as its first row. It used to stand
          alone here as a bare link while "Domains" below listed only the custom
          ones, so a seeded deployable read "No custom domains" with its own
          domain a few lines above. One list now, every name it answers on.

          EVERYBODY SEES THE LIST, because everybody could always see the
          address. What the `domains` part (epic memql#5289) decides is whether
          a binding can be ADDED.

          A SEEDED DEPLOYABLE TAKES BINDINGS LIKE ANY OTHER. What is fixed on
          MemQL OS or the VS Code site is the SITE ROW -- its own address, its
          status, its settings, which are re-seeded at boot and refuse a write.
          A binding is a separate record that points at the site, and none of
          the custom-domain policy's rules (component/memql/
          platform_custom_domain_policy.go) names a seeded site. So the
          built-in address is shown fixed, and Add is offered beside it. */}
      <DomainsContent
        site={site}
        domain={clusterDomain}
        bindings={canBindDomain}
        editable={canBindDomain}
        onOpenDomain={onOpenDomain}
        onAdd={onAddDomain}
        // WHO IT IS FOR sits beside WHERE IT IS, as it always did (epic
        // memql#4800, D5): the same class of fact. It is not the picker said
        // twice -- a client this reader cannot see still shows here by id,
        // which the picker alone does not make plain. AccountChip draws
        // nothing for an empty name, so a site with no client is unchanged.
        addressFacts={<AccountChip name={accountNameFrom(accounts, site.accountId)} />}
      />

      {/* Organization ownership is enforced by the engine on both the
          existing and requested organization. It cannot be cleared. */}
      <Field label="Organization">
        <AccountPicker
              required
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
          next="The previous client is still selected. Try again when the issue below is resolved."
          detail={tie.error}
        />
      )}

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
