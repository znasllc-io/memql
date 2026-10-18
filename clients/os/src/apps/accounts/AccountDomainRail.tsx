import { useState } from "react";

import {
  Check,
  Chip,
  CopyValue,
  Notice,
  Panel,
  Rail,
  Subhead,
  formatMoment,
  nextOpen,
  type Stop,
} from "../../kit";
import type { UpdateAccountState } from "./actions";
import { AccountFrontDoorStop } from "./AccountFrontDoorStop";
import type { FrontDoorRow } from "./frontDoor";
import { accountIsSelf, accountName, domainIsVerified, type AccountRow } from "./rows";

// THE DOMAIN RAIL: what a client's own domain buys them, in four stops.
//
// ===========================================================================
// A RAIL BECAUSE THE STOPS DEPEND ON EACH OTHER
// ===========================================================================
// There is a domain, ownership of it is proven, joining can then be turned on,
// and a MemQL name can then be reserved. Each stop is legal only once the one
// above it is answered, which is exactly what a rail says and what four panels
// would not.
//
// NOTHING HERE HAS A CHECK BUTTON, and that is the point of the footer
// sentence. The reconciler walks every unverified domain on its own schedule;
// a "Check now" control would either lie (it cannot make DNS propagate) or
// invite somebody to press it repeatedly while a TTL expires.

export function AccountDomainRail({
  account,
  update,
  clusterDomain,
  doors,
  canReadDoors,
}: {
  account: AccountRow;
  update: UpdateAccountState;
  /** This cluster's own domain, for the CNAME target the three names point at. */
  clusterDomain: string;
  /** Every front door this caller may read, live (epic memql#5168). */
  doors: FrontDoorRow[];
  /**
   * Whether this viewer can read front doors at all.
   *
   * v1:platform:accountFrontDoor is clusterOwner tier, so a non-owner gets
   * zero rows AND NO ERROR -- which the stop would otherwise render as "not
   * held" about a door that is serving. That is a lie, and worse than showing
   * nothing. The stop says which it is.
   */
  canReadDoors: boolean;
}) {
  const [openStop, setOpenStop] = useState("");
  const self = accountIsSelf(account);
  const verified = domainIsVerified(account) || self;

  if (account.domain.trim() === "" && !self) {
    return (
      <Panel label="This client's domain">
        <Subhead>Domain</Subhead>
        <p className="os-caption">
          No domain recorded. Edit the profile above to add one; proving it is what lets this
          client's people reach their work by arriving on it.
        </p>
      </Panel>
    );
  }

  const stops: Stop[] = [
    {
      id: "domain",
      name: "Domain",
      state: account.domain.trim() === "" ? "waiting" : "done",
      sentence: "The client's own domain",
      answer: account.domain,
      body: (
        <p className="os-caption">
          Recorded on the profile above. Changing it clears the proof: proof of one name is not
          proof of another.
        </p>
      ),
    },
    {
      id: "ownership",
      name: "Ownership",
      state: verified ? "done" : account.domainStatus === "verifying" ? "current" : "open",
      sentence: verified
        ? self
          ? "Verified by this cluster"
          : "Proven once; not checked again"
        : "Publish a record to prove it",
      answer: verified
        ? self
          ? "verified by this cluster"
          : `proven ${formatMoment(account.domainVerifiedAt)}`
        : account.domainStatus === "verifying"
          ? "not seen yet"
          : "not checked yet",
      body: self ? (
        <p className="os-caption">
          This is the cluster's own company, so nothing has to be proven to anybody.
        </p>
      ) : (
        <>
          {/* THE RECORD AS THREE PARTS, each copyable on its own: a person is
              pasting them into three different fields of a registrar's form,
              and one blob of text is one they have to split by hand. The
              Deployables domains stop set this shape. */}
          <dl className="os-dns-record">
            <div>
              <dt>Type</dt>
              <dd>
                <CopyValue value="TXT" label="Type" />
              </dd>
            </div>
            <div>
              <dt>Name</dt>
              <dd>
                <CopyValue value={`_memql-verify.${account.domain}`} label="Name" />
              </dd>
            </div>
            <div>
              <dt>Value</dt>
              <dd>
                <CopyValue value={account.domainToken} label="Value" />
              </dd>
            </div>
          </dl>
          {account.domainFailureReason === "" ? null : (
            <Notice
              tone="warn"
              sentence={reasonSentence(account.domainFailureReason)}
              detail={account.domainFailureDetail}
            />
          )}
          <p className="os-caption">
            {account.domainLastCheckedAt === ""
              ? "No check has run yet."
              : `Last looked ${formatMoment(account.domainLastCheckedAt)}.`}{" "}
            Checked every two minutes on its own; there is nothing to press.
          </p>
        </>
      ),
    },
    {
      id: "joining",
      name: "Joining",
      // `waiting`, never `pending`, before the proof: `pending` means NOT
      // REACHABLE, which dims the stop and gives it no disclosure at all -- so
      // the one sentence a person needs ("prove ownership first") would be
      // behind a stop they cannot open. Reachable-and-not-done is the truth.
      state: account.joinOnDomain ? "done" : verified ? "open" : "waiting",
      sentence: "Who joins this client's group",
      answer: account.joinOnDomain ? `on for @${account.domain}` : verified ? "off" : "",
      body: verified ? (
        <>
          <Check
            checked={account.joinOnDomain}
            onChange={(next) => void update.setJoining(account.id, next)}
            disabled={update.busy}
          >
            People with a verified address on @{account.domain} join {accountName(account)}'s group
          </Check>
          <p className="os-caption">
            It applies at arrival and to nobody already here. Somebody already on this cluster is
            placed by hand, in Users.
          </p>
          {/* THE REFUSAL LANDS AT THE STOP THAT OWNS THE VALUE, and it has a
              typed one worth naming: the engine's validation reads the MERGED
              payload, so flipping this on a row whose STORED status is not
              verified is refused `domain_not_verified` -- which happens when
              somebody changes the domain and turns joining on in the same
              sitting, where the walk has not yet run against the new name. A
              generic "that did not go through" would leave them looking at a
              checkbox that will not stay on with nothing to act on. */}
          {update.error === "" ? null : (
            <Notice
              tone="error"
              sentence={
                update.error.includes("domain_not_verified")
                  ? "This domain is not proven yet."
                  : "Joining was not changed."
              }
              next={
                update.error.includes("domain_not_verified")
                  ? "The walk checks again on its own. Joining can be turned on once it has seen the record."
                  : undefined
              }
              detail={update.error}
            />
          )}
        </>
      ) : (
        // NO CONTROL AT ALL before the proof, rather than a disabled one: the
        // engine refuses `domain_not_verified`, and a checkbox that can only
        // fail is one somebody has to read past to learn it is not for them.
        <p className="os-caption">Prove ownership first.</p>
      ),
    },
    {
      id: "memql",
      name: "MemQL address",
      state: account.memqlReservedAt === "" ? (verified ? "open" : "waiting") : "done",
      sentence: "Its reserved name here",
      answer: account.memqlDomain,
      body: self ? (
        // THE CLUSTER'S OWN ACCOUNT SERVES NO DOOR. Its reserved name IS the
        // cluster domain -- the hosts beneath it are the front door itself,
        // generated into the overlays, not provisioned per account (epic
        // memql#5168, D1). Rendering the stop here would offer three CNAMEs
        // for names this cluster already answers on.
        <>
          <p className="os-caption">{"This cluster's own."}</p>
          {account.memqlDomain === "" ? null : (
            <p className="os-account-hosts">
              {["app", "api", "id"].map((host) => (
                <Chip key={host}>{`${host}.${account.memqlDomain}`}</Chip>
              ))}
            </p>
          )}
        </>
      ) : (
        <AccountFrontDoorStop
          accountId={account.id}
          reservedName={account.memqlDomain}
          reservationReason={account.memqlReservationReason}
          held={account.memqlReservedAt !== ""}
          clusterDomain={clusterDomain}
          doors={doors}
          canRead={canReadDoors}
        />
      ),
    },
  ];

  return (
    <Panel label={`The domain of ${accountName(account)}`}>
      <Subhead>Domain</Subhead>
      {/* COLLAPSED, ONE OPEN, and the one it opens by default is the first
          stop still to do -- which on an unproven domain is Ownership, the
          only stop with something for a person to act on. This is the RECORD
          reading of the control (the Deployables page's), not the compose one:
          every stop here has an answer to show, and four open bodies would be
          four panels with a line drawn down them. */}
      <Rail
        stops={stops}
        label="What this client's domain buys them"
        openStop={openStop === "" ? nextOpen(stops) : openStop}
        onOpenStop={setOpenStop}
      />
    </Panel>
  );
}

/**
 * A typed failure reason, in words.
 *
 * NOT an enum on the row, deliberately -- a resolver reports things a closed
 * set written today does not anticipate -- so an unrecognised value keeps its
 * own name rather than being dressed as one of these.
 */
function reasonSentence(reason: string): string {
  switch (reason) {
    case "dns_token_missing":
      return "The record is not there yet.";
    default:
      return `The last check reported ${reason}.`;
  }
}
