import { IconButton } from "../../kit/IconButton";
import { useEffect, useState } from "react";
import { Trash2 } from "lucide-react";
import { Button, Caption, ContentSkeleton, Input, MultiPicker, Notice, RoleSelect, RoleTag, accountRoleChoices } from "../../kit";
import { useAccountAccess, type AccountPerson } from "./useAccountAccess";
import { useUsersActions } from "../users/actions";
import { OS_REGISTRY } from "../registry";
import { InvitationDelivery } from "../users/InvitationDelivery";
import type { UserInvitationResult } from "@znasllc-io/memql-sdk-core/identityadmin";

const rank: Record<string, number> = { owner: 400, developer: 300, admin: 200, member: 100, viewer: 50 };
const nameOf = (person: AccountPerson) => person.name || person.email || "Unnamed person";

/** The same account controls are used from Accounts and the canonical group.
 * Owner is a single assignment; transfer is explicit and never a role option. */
export function AccountAccessEditor({ accountId, groupId, memberIds, onChanged, onOpenGroup, onAuthority }: {
  accountId: string; groupId?: string; memberIds?: readonly string[]; onChanged?: () => void; onOpenGroup?: (id: string) => void; onAuthority?: (canManage: boolean) => void;
}) {
  const access = useAccountAccess(accountId), invites = useUsersActions();
  const [selected, setSelected] = useState<string[]>([]), [role, setRole] = useState("viewer"), [email, setEmail] = useState("");
  const [transfer, setTransfer] = useState<string[]>([]), [transferring, setTransferring] = useState(false), [editingApps, setEditingApps] = useState<string | null>(null), [apps, setApps] = useState<string[]>([]);
  const [invited, setInvited] = useState<UserInvitationResult | null>(null), [groupName, setGroupName] = useState("");
  const custom = !!groupId && groupId !== `acct-${accountId}`;
  const data = access.value;
  useEffect(() => { onAuthority?.(!!data && (rank[data.actorRole] ?? 0) >= 200); }, [data, onAuthority]);
  if (!data) return access.error ? <Notice tone="warn" sentence="Account access could not be loaded." detail={access.error}><Button onClick={access.reload}>Try again</Button></Notice> : <ContentSkeleton kind="detail" label="Loading account access" />;
  const actorRank = rank[data.actorRole] ?? 0;
  const options = accountRoleChoices.filter(option => (rank[option.value] ?? 0) < actorRank);
  const group = data.groups?.find(item => item.id === groupId);
  const currentMembers = group?.userIds ?? memberIds;
  const people = custom ? data.people.filter(person => currentMembers?.includes(person.id)) : data.people;
  const candidates = (custom ? data.people.filter(person => !currentMembers?.includes(person.id)) : data.candidates).filter(person => (rank[person.role] ?? 0) < actorRank);
  const availableApps = OS_REGISTRY.apps.filter(app => !["cluster", "concepts", "logs", "setup", "fleet", "accounts", "users", "settings"].includes(app.id)).map(app => ({ id: `app:${app.id}`, label: app.name }));
  // Retain grants to installed/product apps the shell does not ship a tile for.
  for (const app of data.apps) if (!availableApps.some(choice => choice.id === app)) availableApps.push({ id: app, label: app.replace(/^app:/, "") });
  const appChoices = editingApps === "account" ? availableApps : availableApps.filter(choice => data.apps.includes(choice.id));
  const commit = async (args: Record<string, unknown>) => { const saved = await access.change(args); if (saved) onChanged?.(); return saved; };
  return <div className="os-account-access">
    <div className="os-account-access-actions"><Caption>{data.primary ? "Primary account" : "Account roles"}</Caption><Button disabled={access.busy} onClick={access.reload}>Refresh</Button></div>
    {access.error ? <Notice tone="error" sentence="The change was not saved." detail={access.error}><Button onClick={access.reload}>Refresh access</Button></Notice> : null}
    <section aria-label="Membership">
      <MultiPicker label="People" choices={candidates.map(person => ({ id: person.id, label: nameOf(person), description: person.email }))} selected={selected} onChange={setSelected} disabled={access.busy}
        add={{ label: "Add selected people", onClick: () => { void commit({ operation: custom ? "groupMembers" : "members", groupId, userIds: selected, role }).then(saved => { if (saved) setSelected([]); }); } }} />
      {!custom && selected.length ? <RoleSelect label="Role for selected people" value={role} onChange={setRole} options={options} disabled={access.busy} /> : null}
      <div role="list" aria-label="Account members">
        {people.map(person => <div className="os-account-person" role="listitem" key={person.id}>
          <span className="os-account-person-name"><strong>{nameOf(person)}</strong><Caption>{person.email}</Caption></span>
          {!custom && person.role !== "owner" && (rank[person.role] ?? 0) < actorRank ? <RoleSelect label={`Role for ${nameOf(person)}`} value={person.role} options={options} disabled={access.busy} onChange={next => void commit({ operation: "members", userIds: [person.id], role: next })} /> : <RoleTag role={person.role} />}
          <span className="os-account-access-actions">
            {!custom && (rank[person.role] ?? 0) < rank.admin! && (rank[person.role] ?? 0) < actorRank && !data.primary ? <Button disabled={access.busy} onClick={() => { setEditingApps(person.id); setApps(person.apps ?? []); }}>Apps</Button> : null}
            {person.role !== "owner" && (rank[person.role] ?? 0) < actorRank ? <IconButton label={`Remove ${nameOf(person)}`} disabled={access.busy} onClick={() => void commit({ operation: custom ? "groupMembers" : "members", groupId, userIds: [person.id], remove: true })}><Trash2 size={15} aria-hidden /></IconButton> : null}
          </span>
        </div>)}
      </div>
      {!people.length ? <Caption>No people in this group yet.</Caption> : null}
    </section>
    {!data.primary ? <section aria-label="App access"><div className="os-account-access-actions"><strong>Apps</strong><Caption>{data.apps.length ? data.apps.map(app => availableApps.find(choice => choice.id === app)?.label ?? app).join(", ") : "No apps assigned"}</Caption>
      {data.assignsApps && !custom ? <Button disabled={access.busy} onClick={() => { setEditingApps("account"); setApps(data.apps); }}>Edit apps</Button> : null}
      {custom ? <Button disabled={access.busy} onClick={() => { setEditingApps("group"); setApps(group?.apps ?? []); }}>Assign apps to group</Button> : null}
    </div></section> : null}
    {editingApps ? <section aria-label="Choose apps"><MultiPicker label="Apps" choices={appChoices} selected={apps} onChange={setApps} disabled={access.busy} />
      <div className="os-account-access-actions"><Button disabled={access.busy} onClick={() => setEditingApps(null)}>Cancel</Button><Button busy={access.busy} onClick={() => void commit(editingApps === "account" ? { operation: "apps", apps } : editingApps === "group" ? { operation: "groupApps", groupId, apps } : { operation: "members", userIds: [editingApps], role: data.people.find(person => person.id === editingApps)?.role, apps }).then(saved => { if (saved) setEditingApps(null); })}>Save apps</Button></div>
    </section> : null}
    {!custom ? <section aria-label="Invite a person"><Input id={`invite-${accountId}`} label="Invite by email" value={email} onChange={setEmail} placeholder="name@example.com" />
      <div className="os-account-access-actions"><Button busy={!!invites.busyKey} disabled={!email.trim()} onClick={() => void invites.issueInvitation(email.trim(), "viewer", [`acct-${accountId}`]).then(result => { if (result) { setEmail(""); setInvited(result); } })}>Invite</Button><Caption>New people join as viewers. Choose their role after they join.</Caption></div>
      {invited ? <InvitationDelivery result={invited} /> : null}{invites.refusal ? <Notice tone="error" sentence="The invitation could not be created." detail={invites.refusal.detail} /> : null}
    </section> : null}
    {(data.invitations ?? []).some(invite => !custom || invite.groupIds.includes(groupId!)) ? <section aria-label="Invited people"><strong>Invited</strong>
      {(data.invitations ?? []).filter(invite => !custom || invite.groupIds.includes(groupId!)).map(invite => <div className="os-account-access-actions" key={invite.id}><span>{invite.email}</span><Caption>Expires {new Date(invite.expiresAt).toLocaleDateString()}</Caption><Button disabled={access.busy} onClick={() => void commit({ operation: "revokeInvitation", invitationId: invite.id })}>Revoke invitation</Button></div>)}
    </section> : null}
    {!custom ? <section aria-label="Groups"><strong>Groups</strong>
      {(data.groups ?? []).filter(item => item.kind === "custom").map(item => <div key={item.id} className="os-account-access-actions"><Button onClick={() => onOpenGroup?.(item.id)} disabled={!onOpenGroup}>{item.name}</Button></div>)}
      <Input id={`group-${accountId}`} label="New group" value={groupName} onChange={setGroupName} placeholder="Group name" />
      <Button disabled={!groupName.trim()} busy={access.busy} onClick={() => void commit({ operation: "createGroup", name: groupName.trim() }).then(saved => { if (saved) setGroupName(""); })}>Create group</Button>
    </section> : null}
    {!custom && data.actorRole === "owner" ? <section aria-label="Ownership">
      {!transferring ? <Button disabled={access.busy} onClick={() => setTransferring(true)}>Transfer ownership</Button> : <>
        <Caption>Choose the new owner. The current owner will become a developer.</Caption>
        <MultiPicker label="New owner" single choices={data.people.filter(person => person.id !== data.ownerUserId).map(person => ({ id: person.id, label: nameOf(person), description: person.email }))} selected={transfer} onChange={setTransfer} disabled={access.busy} />
        <div className="os-account-access-actions"><Button disabled={access.busy} onClick={() => { setTransferring(false); setTransfer([]); }}>Cancel</Button><Button disabled={transfer.length !== 1} busy={access.busy} onClick={() => void commit({ operation: "transfer", userId: transfer[0] }).then(saved => { if (saved) { setTransferring(false); setTransfer([]); } })}>Transfer ownership</Button></div>
      </>}
    </section> : null}
    {access.readAt ? <Caption>Read at {new Date(access.readAt).toLocaleTimeString()}. Refresh to see changes from other managers.</Caption> : null}
  </div>;
}
