import { Caption, Details, DetailsSection, Fact, Facts, formatMoment } from "../../kit";
import { PageHelp } from "../../kit/pageHelp";
import { accountDefaultDomain, accountDomains } from "./defaults";
import { currentDoor, doorSentence, reservationReasonSentence, type FrontDoorRow } from "./frontDoor";
import { accountIsArchived, accountIsSelf, domainIsVerified, type AccountRow } from "./rows";

// THE DETAILS TAB: the account's facts, read-only (design record
// 2026-11-12-account-export, section K). The Fleet machine page's Details is
// the reference: open, named sections, one alignment, and quiet copy on what
// somebody would rather paste than retype -- names, the domain, the address,
// ids and exact times. A status is read, not copied.
//
// EVERY EDIT IS ON SETTINGS. A fact shown here and changed there is the
// machine page's split too (its name is a Detail, its rename a Setting): this
// tab answers "what is true", and offers nothing a person could act on.

export interface AccountDetailsTabProps {
  account: AccountRow;
  /** Every front door this caller may read, live; held by the page. */
  doors: FrontDoorRow[];
  /** Whether this viewer can read front doors at all (cluster owner only). */
  canReadDoors: boolean;
}

export function AccountDetailsTab({ account, doors, canReadDoors }: AccountDetailsTabProps) {
  const self = accountIsSelf(account);
  const assigned = accountDomains(account);
  const defaultDomain = accountDefaultDomain(account);
  return (
    <Details>
      <PageHelp title="Account details">
        <p>Names, the domain, addresses, ids and exact times can be copied: point to a row, or tab to it, to show its copy icon. Statuses are not copied. Everything here is changed on Settings.</p>
      </PageHelp>
      <DetailsSection title="Profile">
        <Facts>
          <Fact label="Name" value={account.name} copyValue={account.name || undefined} />
          <Fact label="Domain" value={account.domain} copyValue={account.domain || undefined} mono />
          <Fact label="Contact" value={account.primaryContactName} copyValue={account.primaryContactName || undefined} />
          <Fact label="Email" value={account.primaryContactEmail} copyValue={account.primaryContactEmail || undefined} mono />
          <Fact label="Notes" value={account.notes} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Domain">
        <Facts>
          <Fact label="Ownership" value={ownership(account)} />
          {self || !domainIsVerified(account) ? null : (
            <Fact label="Proven on" value={formatMoment(account.domainVerifiedAt)} copyValue={account.domainVerifiedAt || undefined} />
          )}
          {self ? null : (
            <Fact
              label="Last checked"
              value={account.domainLastCheckedAt === "" ? "No check has run yet" : formatMoment(account.domainLastCheckedAt)}
              copyValue={account.domainLastCheckedAt || undefined}
            />
          )}
          <Fact label="Joining" value={account.joinOnDomain ? `On for @${account.domain}` : "Off"} />
          <Fact label="Assigned domains" value={assigned.join(", ")} copyValue={assigned.length ? assigned.join(", ") : undefined} />
          <Fact label="Default domain" value={defaultDomain} copyValue={defaultDomain || undefined} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Front door">
        <Facts>
          <Fact label="MemQL address" value={account.memqlDomain} copyValue={account.memqlDomain || undefined} mono />
          <Fact label="Reservation" value={reservation(account)} title={account.memqlReservedAt || undefined} />
          {account.memqlDomain === "" ? null : <Fact label="Serving" value={serving(account, doors, canReadDoors)} />}
        </Facts>
      </DetailsSection>
      <DetailsSection title="Status">
        <Facts>
          <Fact label="Status" value={accountIsArchived(account) ? "Archived" : "Active"} />
          <Fact label="Created" value={formatMoment(account.createdAt)} copyValue={account.createdAt || undefined} />
          <Fact label="Account id" value={account.id} copyValue={account.id} mono />
        </Facts>
        {self ? (
          <Caption>
            This is your own company -- the one this instance belongs to. It was created when the
            cluster first started.
          </Caption>
        ) : null}
      </DetailsSection>
    </Details>
  );
}

/** The domain's proof, in words. An empty status is a row the walk never saw. */
function ownership(account: AccountRow): string {
  if (accountIsSelf(account)) return "Verified by this cluster";
  if (account.domain.trim() === "") return "No domain recorded";
  if (domainIsVerified(account)) return "Proven";
  if (account.domainStatus === "verifying") return "Not seen yet";
  return "Not proven yet";
}

/** Whether the MemQL address is held, and when not, why. */
function reservation(account: AccountRow): string {
  if (account.memqlDomain === "") return "No address set";
  if (account.memqlReservedAt !== "") return `Held since ${formatMoment(account.memqlReservedAt)}`;
  return reservationReasonSentence(account.memqlReservationReason) || "Not held";
}

/**
 * Whether the address is being served.
 *
 * NOT READABLE IS SAID, not drawn as an absence: the door is cluster-owner
 * tier, so anybody else reads zero rows and no error, and "not served" would
 * be a fact this window invented about a door that may be serving.
 */
function serving(account: AccountRow, doors: FrontDoorRow[], canRead: boolean): string {
  if (accountIsSelf(account)) return "This cluster's own";
  if (!canRead) return "Only a cluster owner can see this";
  return doorSentence(currentDoor(doors, account.id)) || "Not served yet";
}
