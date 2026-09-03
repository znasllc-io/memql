import { Caption, Field, Input, Subhead } from "../../../../../kit";
import { AccountPicker } from "../../../../accounts/AccountPicker";
import { accountNameFrom, type AccountRow } from "../../../../accounts/rows";
import { normalizeHostname } from "../../../domains";
import { hostnameFor, validateSlug } from "../../../hostname";
import { ProblemNotice } from "../../../packages/ReportView";
import type { DeployableOutcome } from "../../../packages/rows";
import { EMPTY_ADDRESS, type AddressDraft } from "../../compose";

// The compose Where-it-lives stop: the address each app answers at, chosen
// once (epic memql#4885, design section C).
//
// ===========================================================================
// CHOSEN ONCE, AND THE CAPTION SAYS SO
// ===========================================================================
// A hostname is permanent for a site: a later deploy of the same source
// finds each app through the (packageId, deployable name) key the first
// deploy recorded, and never asks again. Somebody typing here is making a
// decision they will not be offered a second time, so the stop says that
// where they are typing rather than in a doc.
//
// ===========================================================================
// THE SLUG IS ANSWERED AT KEYSTROKE RATE. THE SERVER STILL DECIDES
// ===========================================================================
// `validateSlug` is the mirrored half of the Go hostname policy and states
// its own limits (hostname.ts): cluster-wide uniqueness and the cluster-owner
// exemption are deliberately NOT mirrored, because a browser cannot answer
// either. A taken slug passes here and is refused on Deploy, and the server's
// sentence -- which names the colliding site -- renders verbatim.
//
// ===========================================================================
// A REFUSED HALF IS NOT A FAILED DEPLOY
// ===========================================================================
// The pipeline applies the client tie and the own domain AFTER the publish,
// under the caller's own actor, so the guards that already run decide. Either
// can be refused while the app is live at its cluster address, and the
// refusal lands on the outcome. It renders HERE, at the stop the two halves
// belong to, with copy that says the deployable is serving.

export function ComposeWhereItLivesStop({
  apps,
  sourceName,
  addresses,
  onAddress,
  accounts,
  isClusterOwner,
  clusterDomain,
  outcomes,
  locked,
}: {
  /** Every app that needs an address. One entry, named "", for a hand-made deployable. */
  apps: readonly string[];
  /** The source's own name, for the slug suggestion. */
  sourceName: string;
  addresses: Readonly<Record<string, AddressDraft>>;
  onAddress: (app: string, patch: Partial<AddressDraft>) => void;
  accounts: AccountRow[];
  /** The client's own domain is a cluster owner's act (memql#4805, D1). */
  isClusterOwner: boolean;
  clusterDomain: string;
  /** The run's outcomes once it has finished: where the two placement refusals live. */
  outcomes: readonly DeployableOutcome[];
  /** After Deploy the stop is facts, not fields. */
  locked: boolean;
}) {
  if (locked) {
    return (
      <div className="os-stop-body">
        {outcomes.map((outcome) => (
          <PlacedApp key={outcome.name} outcome={outcome} accounts={accounts} many={outcomes.length > 1} />
        ))}
      </div>
    );
  }

  return (
    <div className="os-stop-body">
      {apps.map((app) => (
        <AppAddress
          key={app}
          app={app}
          sourceName={sourceName}
          address={addresses[app] ?? EMPTY_ADDRESS}
          onAddress={(patch) => onAddress(app, patch)}
          accounts={accounts}
          isClusterOwner={isClusterOwner}
          clusterDomain={clusterDomain}
          many={apps.length > 1}
        />
      ))}
      <Caption>Chosen once. A later deploy of this source keeps the same addresses.</Caption>
    </div>
  );
}

function AppAddress({
  app,
  sourceName,
  address,
  onAddress,
  accounts,
  isClusterOwner,
  clusterDomain,
  many,
}: {
  app: string;
  sourceName: string;
  address: AddressDraft;
  onAddress: (patch: Partial<AddressDraft>) => void;
  accounts: AccountRow[];
  isClusterOwner: boolean;
  clusterDomain: string;
  many: boolean;
}) {
  const slug = address.slug.trim();
  const complaint = validateSlug(address.slug, clusterDomain);
  const preview = hostnameFor(address.slug, clusterDomain);
  const key = app === "" ? "one" : app;
  const heading = app === "" ? sourceName : app;

  return (
    <section className="os-report-part" aria-label={`Where ${heading || "it"} lives`}>
      {/* A SINGLE APP NEEDS NO HEADING: the Head already names what is being
          composed, and a subhead repeating it says the scope twice (rule 7). */}
      {many ? <Subhead>{heading}</Subhead> : null}

      <Field label="Address">
        <Input
          id={`os-compose-slug-${key}`}
          label={`The name ${heading || "this deployable"} answers at`}
          value={address.slug}
          onChange={(next) => onAddress({ slug: next })}
          placeholder="shop"
        />
      </Field>
      {/* THE PREVIEW IS THE ANSWER, at keystroke rate: what a person is
          choosing is a hostname, not a label, and the label alone does not
          show them the thing they will type into a browser. */}
      {slug === "" ? (
        <Caption>One label under {clusterDomain || "this cluster's domain"}.</Caption>
      ) : complaint === "" ? (
        <p className="os-stop-verdict" data-tone="ok">
          <span className="os-mono">{preview}</span>
        </p>
      ) : (
        <p className="os-stop-verdict" data-tone="warn" role="status">
          {complaint}
        </p>
      )}

      <Field label="Client">
        <AccountPicker
          id={`os-compose-account-${key}`}
          label={`The client ${heading || "this deployable"} is for`}
          value={address.accountId}
          accounts={accounts}
          onChange={(accountId) => onAddress({ accountId, ...prefillDomain(accounts, accountId, address) })}
        />
      </Field>

      {/* Binding a client's own domain is cluster-owner territory (design D1),
          enforced by the concept's clusterOwner tier and the three Go guards;
          rendering the field for one is the presentation half. */}
      {isClusterOwner ? (
        <>
          <Field label="Their own domain">
            <Input
              id={`os-compose-domain-${key}`}
              label={`A domain of the client's own for ${heading || "this deployable"}`}
              value={address.ownDomain}
              onChange={(ownDomain) => onAddress({ ownDomain })}
              placeholder="shop.acme.com"
            />
          </Field>
          <Caption>
            Optional, and the deploy never waits on it: the deployable goes live at its cluster address, and the domain
            stays waiting on your DNS records until both check out. The two records to create are on this stop once it
            exists.
          </Caption>
        </>
      ) : null}
    </section>
  );
}

/**
 * The address as it LANDED, plus either placement half that did not.
 *
 * The two refusals read as what they are -- the app is serving and one thing
 * asked for beside the address was refused -- because that is what the copy
 * table says for those two codes, and the guard's own sentence names which
 * guard refused.
 */
function PlacedApp({
  outcome,
  accounts,
  many,
}: {
  outcome: DeployableOutcome;
  accounts: AccountRow[];
  many: boolean;
}) {
  const tied = accountNameFrom(accounts, outcome.accountId ?? "");
  return (
    <section className="os-report-part" aria-label={`Where ${outcome.name || "it"} lives`}>
      {many ? <Subhead>{outcome.name}</Subhead> : null}
      <p className="os-stop-address">
        <code className="os-mono">{outcome.hostname || "--"}</code>
      </p>
      {tied === "" ? null : <Caption>For {tied}.</Caption>}
      {outcome.ownDomain ? <Caption>Bound to {outcome.ownDomain}, once its DNS records check out.</Caption> : null}
      {outcome.accountRefusal ? <ProblemNotice problem={outcome.accountRefusal} tone="warn" /> : null}
      {outcome.domainRefusal ? <ProblemNotice problem={outcome.domainRefusal} tone="warn" /> : null}
    </section>
  );
}

/**
 * Choosing a client PREFILLS their own domain, and never overwrites one.
 *
 * The account record carries the client's domain (`v1:accounts:account.domain`),
 * and typing it again by hand is how a typo gets into a DNS binding. It fills
 * only an empty field: somebody who has already typed a subdomain of their own
 * must not have it replaced by picking the client it belongs to.
 */
function prefillDomain(
  accounts: AccountRow[],
  accountId: string,
  held: AddressDraft,
): Partial<AddressDraft> {
  if (held.ownDomain.trim() !== "") return {};
  const account = accounts.find((a) => a.id === accountId.trim());
  const domain = normalizeHostname(account?.domain ?? "");
  return domain === "" ? {} : { ownDomain: domain };
}
