import type { AccountRow } from "./rows";

/** Assigned names are suggestions for new work, never ownership or sending proof. */
export function normalizeAccountDomain(value: string): string {
  const name = value.trim().toLowerCase().replace(/\.$/, "");
  return name.length <= 253 && name.includes(".") && !/^\d+(\.\d+){3}$/.test(name)
    && name.split(".").every(label => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)) ? name : "";
}

export function accountDomains(account: AccountRow | undefined): string[] {
  if (!account || account.status === "archived") return [];
  return [...new Set((account.domains ?? [account.domain ?? ""]).map(normalizeAccountDomain).filter(Boolean))];
}

export function accountDefaultDomain(account: AccountRow | undefined): string {
  const domains = accountDomains(account);
  return domains.find(domain => domain === normalizeAccountDomain(account?.defaultDomain ?? "")) ?? domains[0] ?? "";
}

export function accountDeployableDomain(account: AccountRow | undefined, slug: string): string {
  const domain = accountDefaultDomain(account);
  return domain && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(slug.trim())
    ? normalizeAccountDomain(`${slug.trim()}.${domain}`) : "";
}

type Sender = { id: string; accountId: string; address: string; status: string };

/** An explicit default never silently switches to a different sender. */
export function accountDefaultSender(account: AccountRow | undefined, senders: readonly Sender[]): string {
  if (!account || account.status === "archived") return "";
  const available = senders.filter(sender => sender.accountId === account.id && sender.status === "active");
  if (account.defaultSenderIdentityId) return available.find(sender => sender.id === account.defaultSenderIdentityId)?.id ?? "";
  const domain = accountDefaultDomain(account);
  const matching = available.filter(sender => sender.address.split("@")[1]?.toLowerCase() === domain);
  if (matching.length === 1) return matching[0]!.id;
  return "";
}
