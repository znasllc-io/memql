import { RankMark } from "./RankMark";
import { describeRequirement, roleRungOf } from "../system/roles";
import type { RoleRequirement } from "../system/roles";

/**
 * A SURFACE THIS PERSON MAY NOT REACH (epic memql#4832, D6).
 *
 * The shell hides what a caller cannot reach, and that stays the first line of
 * defence -- hiding an action beats letting somebody click it and read a
 * refusal. This is the second: a window that is open on a surface the actor's
 * rank does not clear, which is reachable two ways that both matter. A desk
 * restored from storage can name an app whose requirement the person no longer
 * meets, and a role can change while somebody is signed in.
 *
 * # It is not an error, and it is not styled as one
 *
 * Nothing has gone wrong. The person is not owed an apology, a warning
 * triangle or a red border -- amber and red are STATUS in this shell, and
 * "your role does not include this" is not a fault, it is a fact. So this is
 * quiet chrome: the mark, one sentence of what is required, one of where they
 * stand, and the one action that actually resolves it.
 *
 * # The copy names the fix, not the failure
 *
 * "Access denied" tells somebody what already happened. What they need is the
 * next move, and here it is a person rather than a button -- so the line says
 * who can change it and where, in the vocabulary of the app that does it.
 */
export function SurfaceRefused({
  surface,
  requirement,
  actorRole,
}: {
  /** What they tried to open, named as they saw it -- "Accounts", not "accounts". */
  surface: string;
  /** What the surface asks of the actor, in the manifest's own form. */
  requirement?: RoleRequirement;
  /** The role the cluster reported for them. Empty while unresolved. */
  actorRole: string;
}) {
  // IT TAKES THE REQUIREMENT, NOT A FLOOR, and the difference is a sentence
  // that was false. This used to receive `requirementFloor(manifest.roles)`,
  // which reports a SET's weakest member -- so the Users app's
  // `{ any: ["admin", "owner"] }` arrived as "admin" and this panel told a
  // developer the app was "open to admin and above" while refusing them.
  // Developer is 300 and admin 200, so the explanation contradicted the
  // refusal, and the RankMark drew their tick ABOVE the required one.
  //
  // A floor genuinely is "and above"; a set is not, and only the requirement
  // itself knows which it is.
  const isSet = requirement !== undefined && "any" in requirement;
  const requiredRung = isSet ? null : roleRungOf((requirement as { min: string } | undefined)?.min ?? "");
  const actorRung = roleRungOf(actorRole);
  return (
    <div className="os-rank-refused" data-os-rank-refused>
      {/* No ownerRole for a set: there is no single required rung, and marking
          its weakest member draws the same false claim as a picture. */}
      <RankMark
        actorRole={actorRole}
        ownerRole={isSet ? undefined : requiredRung?.slug}
        className="os-rank-mark-lg"
      />
      <h2 className="os-rank-refused-head">
        {surface} needs a higher role
      </h2>
      <p className="os-rank-refused-body">
        This app is open to{" "}
        <span className="os-role-slug">{describeRequirement(requirement)}</span>
        {isSet ? "." : " and above."}{" "}
        {actorRung
          ? <>You are signed in as <span className="os-role-slug">{actorRung.slug}</span>.</>
          : <>Your role has not been reported by the cluster.</>}
      </p>
      <p className="os-caption os-rank-refused-next">
        An owner can change your role in Users.
      </p>
    </div>
  );
}

/**
 * A ROW THIS PERSON CAN READ AND CANNOT CHANGE (epic memql#4832, D2/D3).
 *
 * Brand-new state, and the reason it needs saying out loud: before rank-visible
 * reads, a row you could not edit was a row you could not SEE. "Visible but
 * read-only" had never happened, so an editor that simply refused to save
 * would read as a bug.
 *
 * # Why the reason is structural, and why the copy says so
 *
 * This is not a permission somebody can be granted. Under D3 peer rows are
 * read-only at every rung -- owner-to-owner included -- so "ask an admin" is
 * the wrong advice and "you don't have permission" implies a permission
 * exists. What resolves it is a change of OWNER, so that is what the line
 * offers.
 */
export function PeerRowReadOnly({
  actorRole,
  ownerRole,
  ownerName,
}: {
  /** The viewer's role. Their rung is the accent one on the mark. */
  actorRole: string;
  /**
   * The owner's role, when the surface happens to know it.
   *
   * OPTIONAL, and usually absent -- deliberately. Resolving a row owner into a
   * person costs a read of the principal directory, which is itself gated, so
   * a surface that does not already hold one must NOT open it just to caption
   * a ribbon: that would add an authorization surface to explain an
   * authorization outcome. The sentence below is true either way; knowing the
   * owner's rung only makes the mark say it too.
   */
  ownerRole?: string;
  /** The owner as a person, when the surface already has them. */
  ownerName?: string;
}) {
  const owner = ownerRole ? roleRungOf(ownerRole) : null;
  const actor = roleRungOf(actorRole);
  const samerung = owner !== null && actor !== null && owner.slug === actor.slug;
  return (
    <div className="os-rank-readonly" data-os-rank-readonly>
      <RankMark actorRole={actorRole} ownerRole={ownerRole} />
      <div className="os-rank-readonly-copy">
        <p className="os-rank-readonly-head">
          Read-only — {ownerName ?? "someone else"} owns this.
        </p>
        <p className="os-caption">
          {samerung ? (
            <>
              They are a <span className="os-role-slug">{owner?.slug}</span>, the same rank
              as you. You can change rows owned by people below your rank.
            </>
          ) : (
            <>You can read it because of your rank, and change only rows you own.</>
          )}
        </p>
      </div>
    </div>
  );
}
