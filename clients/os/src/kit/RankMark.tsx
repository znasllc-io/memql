import { roleLadder, roleRungOf } from "../system/roles";

/**
 * THE RUNG MARK -- the shell's one visual for rank (epic memql#4832).
 *
 * A short stack of ticks, one per rung of THIS cluster's ladder, lowest at the
 * bottom. The actor's rung is drawn in the accent; another named rung -- the
 * owner of the row being looked at -- is drawn in the ink; everything else is
 * a hairline.
 *
 * # Why a stack and not a coloured badge
 *
 * A badge per role is the obvious answer and it cannot work here. Roles are
 * CLUSTER STATE now (D5): an operator may define `field-engineer` at rank 250,
 * and a palette of five hard-coded colours has nothing to give it. The stack
 * is drawn FROM the ladder, so a seven-rung cluster draws seven ticks and a
 * custom rung sits in its real position with no client release.
 *
 * It also encodes the thing a badge cannot: POSITION. Rank is an ordering, and
 * an ordering rendered as a colour is an ordering the reader has to memorise.
 *
 * # Why it carries two marks
 *
 * The one state this shell had no vocabulary for is "you can see this row and
 * you cannot change it". Drawn with both rungs lit -- yours in the accent,
 * the owner's in the ink -- the reason is visible before the sentence beside
 * it is read: they are level with you, or above you.
 *
 * # Colour
 *
 * No new hues, and rank is deliberately NOT a status. It is not good, bad or
 * pending, so the amber/red families stay out of it; the brand green is the
 * shell's SIGNAL colour and marks exactly one thing here -- where YOU are.
 * Everything else is the neutral ramp the rest of the chrome is drawn in.
 */
export function RankMark({
  actorRole,
  ownerRole,
  className,
}: {
  /** The signed-in person's role. Their rung is the accent one. */
  actorRole: string;
  /** A second role to mark -- typically a row's owner. Optional. */
  ownerRole?: string;
  className?: string;
}) {
  const rungs = roleLadder();
  // NOTHING RENDERS BEFORE THE LADDER LANDS. An empty ladder means the shell
  // does not know the ordering yet, and a mark drawn from no rungs would be a
  // confident picture of nothing.
  if (rungs.length === 0) return null;

  const actor = roleRungOf(actorRole);
  const owner = ownerRole ? roleRungOf(ownerRole) : null;

  return (
    <span
      className={className ? `os-rank-mark ${className}` : "os-rank-mark"}
      role="img"
      aria-label={markLabel(actor?.name ?? actorRole, owner?.name ?? ownerRole)}
    >
      {/* Highest rung first: the stack reads top-down like a ladder does. */}
      {[...rungs].reverse().map((rung) => {
        const isActor = actor !== null && rung.slug === actor.slug;
        const isOwner = owner !== null && rung.slug === owner.slug;
        return (
          <i
            key={rung.slug}
            aria-hidden="true"
            className="os-rank-tick"
            data-actor={isActor ? "" : undefined}
            data-owner={isOwner ? "" : undefined}
          />
        );
      })}
    </span>
  );
}

/**
 * The mark's whole meaning as one sentence, for anyone not reading pixels.
 *
 * It names the ROLES rather than describing the drawing: "your role is Owner"
 * is the fact; "a stack of five ticks with the top one lit" is the rendering,
 * and a screen reader given the rendering has to reconstruct the fact.
 */
function markLabel(actorName: string, ownerName?: string): string {
  if (!actorName) return "Role unknown";
  if (!ownerName || ownerName === actorName) return `Your role: ${actorName}`;
  return `Your role: ${actorName}. Owner's role: ${ownerName}`;
}

/**
 * A role, in the DATA VOICE, beside its rung mark.
 *
 * The slug is set in the mono face for the reason the tokens file gives that
 * face: a role slug is cluster state -- an identifier read character by
 * character -- not chrome copy. It also makes a custom role look native here
 * rather than like a string that escaped from somewhere.
 */
export function RoleTag({
  role,
  actorRole,
  title,
}: {
  role: string;
  /** When given, the mark also shows where the viewer stands against `role`. */
  actorRole?: string;
  title?: string;
}) {
  const rung = roleRungOf(role);
  return (
    <span className="os-role-tag" title={title}>
      {actorRole === undefined ? null : <RankMark actorRole={actorRole} ownerRole={role} />}
      <span className="os-role-slug">{rung?.slug ?? role}</span>
    </span>
  );
}
