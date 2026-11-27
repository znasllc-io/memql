import { AccountAccessEditor } from "./AccountAccessEditor";
import type { AccountRow } from "./rows";
export interface AccountPeopleTabProps { account: AccountRow; onOpenGroup?: (groupId: string, accountId: string) => void }
export function AccountPeopleTab({ account, onOpenGroup }: AccountPeopleTabProps) {
  return <AccountAccessEditor key={account.id} accountId={account.id} onOpenGroup={id => onOpenGroup?.(id, account.id)} />;
}
