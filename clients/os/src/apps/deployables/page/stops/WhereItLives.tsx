import { Field, Notice } from "../../../../kit";
import { AccountChip, AccountPicker } from "../../../accounts/AccountPicker";
import { accountNameFrom, type AccountRow } from "../../../accounts/rows";
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
  isClusterOwner,
  clusterDomain,
}: {
  site: SiteRow;
  accounts: AccountRow[];
  /** The client's own domain is a cluster owner's act (memql#4805, D1). */
  isClusterOwner: boolean;
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
      {tie.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The client was not changed."
          next="This deployable is still tied to whatever it was."
          detail={tie.error}
        />
      )}

      {/* Binding a client's own domain is cluster-owner territory in v1
          (design D1), enforced by the concept's clusterOwner tier and the
          three Go guards; rendering it for one is the presentation half. */}
      {isClusterOwner ? <DomainsContent site={site} domain={clusterDomain} /> : null}
    </div>
  );
}
