import { roleRungOf } from "../system/roles";

/** Human names, shared by tags, selectors and account membership lists.
 * Numeric ranks remain authorization data; they are not a visual status. */
export function roleDisplayName(role: string): string {
  const name = roleRungOf(role)?.name;
  if (name) return name;
  const standard: Record<string, string> = {
    owner: "Owner", developer: "Developer", admin: "Admin",
    member: "Member", user: "Member", writer: "Member",
    viewer: "Viewer", reader: "Viewer",
  };
  return standard[role] ?? (role || "Role unavailable");
}

/** Describes the role associated with a row. The same
 * quiet role label is used everywhere; no stack of ranks or numeric level. */
export function RankMark({ actorRole, ownerRole, className }: {
  actorRole: string;
  ownerRole?: string;
  className?: string;
}) {
  const actor = roleDisplayName(actorRole);
  const owner = ownerRole ? roleDisplayName(ownerRole) : actor;
  return <span className={["os-role-tag", className].filter(Boolean).join(" ")}
    title={ownerRole && ownerRole !== actorRole ? `Your role: ${actor}. Owner's role: ${owner}` : `Your role: ${actor}`}>
    {owner}
  </span>;
}

export function RoleTag({ role, actorRole, title }: {
  role: string;
  actorRole?: string;
  title?: string;
}) {
  return <span className="os-role-tag" title={title ?? (actorRole ? `Your role: ${roleDisplayName(actorRole)}` : undefined)}>
    {roleDisplayName(role)}
  </span>;
}
