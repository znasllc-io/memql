import { useEffect, useRef } from "react";
import { Shuffle } from "lucide-react";

import { Button, Caption, Field, Input, Subhead } from "../../../../../kit";
import { generateNickname } from "../../../packages/nickname";
import { AccountPicker } from "../../../../accounts/AccountPicker";
import { accountNameFrom, type AccountRow } from "../../../../accounts/rows";
import { normalizeHostname } from "../../../domains";
import { hostnameFor, validateSlug } from "../../../hostname";
import { ProblemNotice } from "../../../packages/ReportView";
import type { DeployableOutcome } from "../../../packages/rows";
import type { AddressCheckHandle } from "../../../sources/useProbes";
import { EMPTY_ADDRESS, addressKeyFor, type AddressDraft, type AddressVerdict, type AddressVerdicts } from "../../compose";

// The compose Where-it-lives stop: the address each app answers at, chosen
// once (epic memql#4885, design section C; the checks of 2026-09-05, D7).
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
// THE NAME IS CHECKED WHILE IT IS TYPED, AND A GENERATED ONE BEFORE IT LANDS
// ===========================================================================
// `validateSlug` still answers the SHAPE at keystroke rate. What the cluster
// alone can answer -- is this name taken -- used to arrive on Deploy, at the
// end of the flow, as a refusal naming the colliding site. It arrives here
// now: a short pause after typing asks `siteHostnameCheck`, the line beneath
// the field says "checking", then the answer, and Deploy is out of reach
// until every address has checked out. Generate draws again on its own
// until a free name comes back, so a person never sees a generated name
// they cannot have. The write guard is unchanged and still decides.
//
// ===========================================================================
// A REFUSED HALF IS NOT A FAILED DEPLOY
// ===========================================================================
// The pipeline applies the client tie and the own domain AFTER the files are
// in place, under the caller's own actor, so the guards that already run
// decide. Either can be refused while the app is in place at its cluster
// address, and the refusal lands on the outcome. It renders HERE, at the
// stop the two halves belong to.

/** How long typing pauses before the cluster is asked. */
export const CHECK_DEBOUNCE_MS = 350;

/** How many times Generate draws again on a taken name before giving up. */
const GENERATE_TRIES = 6;

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
  checks,
  verdicts,
}: {
  /** Every app that needs an address. One entry, named "", for a hand-made deployable. */
  apps: readonly string[];
  /** The source's own name, for the headings. */
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
  /** The cluster's answers about names, asked from here. */
  checks: AddressCheckHandle;
  /** The same answers, per app, as the page folds them. */
  verdicts: Readonly<Record<string, AddressVerdicts>>;
}) {
  if (locked) {
    // THE RUN'S OWN ANSWER WHERE THERE IS ONE. A hand-made deployable never
    // produces outcomes -- there is no run -- and a package run that named no
    // app produces none either, so the drafts stand in: they are what was
    // WRITTEN, and rendering nothing would leave the stop blank at exactly
    // the moment somebody wants to read the address back.
    const placed: DeployableOutcome[] =
      outcomes.length > 0
        ? [...outcomes]
        : apps.map((app) => {
            const held = addresses[app] ?? EMPTY_ADDRESS;
            return {
              name: app === "" ? sourceName : app,
              siteId: "",
              hostname: hostnameFor(held.slug, clusterDomain),
              bundleRef: "",
              version: "",
              created: true,
              accountId: held.accountId,
              ownDomain: normalizeHostname(held.ownDomain),
            };
          });
    return (
      <div className="os-stop-body">
        {placed.map((outcome) => (
          <PlacedApp key={outcome.name} outcome={outcome} accounts={accounts} many={placed.length > 1} />
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
          checks={checks}
          verdicts={verdicts[app] ?? {}}
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
  checks,
  verdicts,
}: {
  app: string;
  sourceName: string;
  address: AddressDraft;
  onAddress: (patch: Partial<AddressDraft>) => void;
  accounts: AccountRow[];
  isClusterOwner: boolean;
  clusterDomain: string;
  many: boolean;
  checks: AddressCheckHandle;
  verdicts: AddressVerdicts;
}) {
  const slug = address.slug.trim();
  const complaint = validateSlug(address.slug, clusterDomain);
  const preview = hostnameFor(address.slug, clusterDomain);
  const key = addressKeyFor(app);
  const heading = app === "" ? sourceName : app;
  const skipped = address.skip === true;
  const slugKey = `${key}:slug`;
  const domainKey = `${key}:domain`;
  const ownDomain = normalizeHostname(address.ownDomain);

  // THE SLUG, ASKED ABOUT AFTER A PAUSE. A shape complaint is answered at
  // keystroke rate and asks the cluster nothing; a blank clears the verdict.
  // Keyed on the composed hostname, so a domain that arrives late re-asks.
  const check = checks.check;
  const clear = checks.clear;
  useEffect(() => {
    if (skipped || slug === "" || complaint !== "" || preview === "") {
      clear(slugKey);
      return;
    }
    const timer = setTimeout(() => void check(slugKey, preview, "site"), CHECK_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [slugKey, preview, slug, complaint, skipped, check, clear]);

  // THE CLIENT'S OWN DOMAIN, the same way, only where the field exists.
  useEffect(() => {
    if (!isClusterOwner || skipped || ownDomain === "") {
      clear(domainKey);
      return;
    }
    const timer = setTimeout(() => void check(domainKey, ownDomain, "domain"), CHECK_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [domainKey, ownDomain, isClusterOwner, skipped, check, clear]);

  // GENERATE DRAWS AGAIN UNTIL A FREE NAME COMES BACK. Each draw asks the
  // cluster under the slug's own key, so the field reads "checking" while it
  // runs and lands on a verdict for the name it finally fills. The last draw
  // is filled even when taken -- a person who saw six tries fail should see
  // the taken name and its sentence, not an empty field.
  const generating = useRef(false);
  async function generate() {
    if (generating.current) return;
    generating.current = true;
    try {
      let candidate = "";
      for (let i = 0; i < GENERATE_TRIES; i += 1) {
        candidate = generateNickname();
        const verdict = await check(slugKey, hostnameFor(candidate, clusterDomain), "site");
        if (verdict.state === "ok") break;
      }
      onAddress({ slug: candidate });
    } finally {
      generating.current = false;
    }
  }

  return (
    <section className="os-report-part" aria-label={`Where ${heading || "it"} lives`} data-skipped={skipped || undefined}>
      {/* A SINGLE APP NEEDS NO HEADING: the Head already names what is being
          composed, and a subhead repeating it says the scope twice (rule 7). */}
      {many ? (
        <div className="os-place-head">
          <Subhead>{heading}</Subhead>
          {/* DEPLOY OR SKIP, PER APP (memql#4930). Offered only when the
              source declares more than one -- a choice between deploying the
              only app and deploying nothing is not a choice, it is a
              differently-spelled Cancel.

              A two-way pill rather than a checkbox: this is a CHOICE between
              two named outcomes, and each of them is a thing that happens.
              "Skip" as an unchecked box would read as the absence of a
              decision rather than as one. */}
          <div className="os-choice-row" role="radiogroup" aria-label={`Deploy or skip ${heading}`}>
            <button
              type="button"
              role="radio"
              className="os-choice"
              aria-checked={!skipped}
              onClick={() => onAddress({ skip: false })}
            >
              Deploy
            </button>
            <button
              type="button"
              role="radio"
              className="os-choice"
              aria-checked={skipped}
              onClick={() => onAddress({ skip: true })}
            >
              Skip
            </button>
          </div>
        </div>
      ) : null}

      {/* A SKIPPED APP ASKS FOR NOTHING, and the sentence says what skipping
          IS (2026-09-05, D5): the app becomes inactive, and activating it
          later is what asks where it should live. */}
      {skipped ? (
        <Caption>
          Skipped. Nothing is built for {heading} and it gets no address -- it stays inactive under its source
          until you activate it from its row. Anything it already serves is untouched.
        </Caption>
      ) : (
        <>
          <Field label="Address">
            <div className="os-compose-slug">
              <Input
                id={`os-compose-slug-${key}`}
                label={`The name ${heading || "this deployable"} answers at`}
                value={address.slug}
                onChange={(next) => onAddress({ slug: next })}
                placeholder="shop"
              />
              {/* A NAME THAT SAYS NOTHING, on purpose. Sometimes an address
                  should not describe what it serves -- a demo, a preview, a
                  thing not ready to be found. A random string does that and is
                  unusable: nobody can read one over a desk. Two ordinary
                  words are the Docker-container shape and memorable for the
                  same reason a phrase is. It fills the field rather than
                  replacing it, so it is a starting point like the seed, not a
                  decision -- and it is checked before it lands. */}
              <Button
                tone="quiet"
                ariaLabel={`Generate an address for ${heading || "this deployable"}`}
                onClick={() => void generate()}
              >
                <Shuffle size={12} aria-hidden /> Generate
              </Button>
            </div>
          </Field>
          {/* THE PREVIEW IS THE ANSWER, at keystroke rate, and the cluster's
              verdict follows a beat behind it: what a person is choosing is a
              hostname, and the one fact only the cluster holds is whether it
              is free. */}
          <VerdictLine slug={slug} complaint={complaint} preview={preview} verdict={verdicts.slug} clusterDomain={clusterDomain} />

          <Field label="Client">
            <AccountPicker
              id={`os-compose-account-${key}`}
              label={`The client ${heading || "this deployable"} is for`}
              value={address.accountId}
              accounts={accounts}
              onChange={(accountId) => onAddress({ accountId, ...prefillDomain(accounts, accountId, address) })}
            />
          </Field>

          {/* Binding a client's own domain is cluster-owner territory (design
              D1), enforced by the concept's clusterOwner tier and the three Go
              guards; rendering the field for one is the presentation half. */}
          {isClusterOwner ? (
            <>
              <Field label="Their own domain">
                <Input
                  id={`os-compose-domain-${key}`}
                  label={`A domain of the client's own for ${heading || "this deployable"}`}
                  value={address.ownDomain}
                  onChange={(next) => onAddress({ ownDomain: next })}
                  placeholder="shop.acme.com"
                />
              </Field>
              {ownDomain === "" ? null : <DomainVerdictLine hostname={ownDomain} verdict={verdicts.ownDomain} />}
              <Caption>
                Optional, and the deploy never waits on it: the deployable goes live at its cluster address, and the
                domain stays waiting on your DNS records until both check out. The two records to create are on this
                stop once it exists.
              </Caption>
            </>
          ) : null}
        </>
      )}
    </section>
  );
}

/**
 * The address line: the shape complaint first (it needs no cluster), then
 * the composed hostname with the cluster's verdict beside it.
 */
function VerdictLine({
  slug,
  complaint,
  preview,
  verdict,
  clusterDomain,
}: {
  slug: string;
  complaint: string;
  preview: string;
  verdict: AddressVerdict | undefined;
  clusterDomain: string;
}) {
  if (slug === "") return <Caption>One label under {clusterDomain || "this cluster's domain"}.</Caption>;
  if (complaint !== "") {
    return (
      <p className="os-stop-verdict" data-tone="warn" role="status">
        {complaint}
      </p>
    );
  }
  if (verdict === undefined || verdict.state === "checking") {
    return (
      <p className="os-stop-verdict" data-tone="busy" role="status">
        <span className="os-mono">{preview}</span> -- checking whether it is free
      </p>
    );
  }
  if (verdict.state === "ok") {
    return (
      <p className="os-stop-verdict" data-tone="ok" role="status">
        <span className="os-mono">{preview}</span> -- free
      </p>
    );
  }
  return (
    <p className="os-stop-verdict" data-tone="warn" role="status">
      {verdict.problem || `${preview} cannot be used.`}
    </p>
  );
}

function DomainVerdictLine({ hostname, verdict }: { hostname: string; verdict: AddressVerdict | undefined }) {
  if (verdict === undefined || verdict.state === "checking") {
    return (
      <p className="os-stop-verdict" data-tone="busy" role="status">
        <span className="os-mono">{hostname}</span> -- checking whether it can be bound
      </p>
    );
  }
  if (verdict.state === "ok") {
    return (
      <p className="os-stop-verdict" data-tone="ok" role="status">
        <span className="os-mono">{hostname}</span> -- can be bound
      </p>
    );
  }
  return (
    <p className="os-stop-verdict" data-tone="warn" role="status">
      {verdict.problem || `${hostname} cannot be bound.`}
    </p>
  );
}

/**
 * The address as it LANDED, plus either placement half that did not.
 *
 * The two refusals read as what they are -- the app is in place and one thing
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
      {/* SAID ONCE (rule 7). With ONE app the rail's own note is the address,
          directly above this body, so repeating it here is a stutter. With
          several the note joins them and each needs naming under its own
          heading, which is the only reading the note cannot give. */}
      {many ? (
        <>
          <Subhead>{outcome.name}</Subhead>
          <p className="os-stop-address">
            <code className="os-mono">{outcome.hostname || "--"}</code>
          </p>
        </>
      ) : null}
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
