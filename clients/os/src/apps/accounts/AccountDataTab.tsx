import { Details } from "../../kit";
import { ExistingDataSection, InventorySection } from "./AccountInventory";
import { accountIsSelf, type AccountRow } from "./rows";
import { useAccountAssignmentStatus, useAccountInventory } from "./useAccounts";

// THE DATA TAB: what the account holds (design record
// 2026-11-12-account-export, section K), from the inventory builtin.
//
// DRAWN ONLY FOR A HOLDER of `execute app:accounts/inventory`, and the page
// decides that: for anyone else there is no Data tab at all (DESIGN.md rule
// 12, absent rather than disabled), so this component assumes the part is
// held and asks for both reads unconditionally.

export interface AccountDataTabProps {
  account: AccountRow;
  /** The accounts the app already holds, to name an account by. */
  accounts: readonly AccountRow[];
}

export function AccountDataTab({ account, accounts }: AccountDataTabProps) {
  const self = accountIsSelf(account);
  const inventory = useAccountInventory(account.id, true);
  const assignment = useAccountAssignmentStatus(self);
  return (
    <Details>
      <InventorySection reading={inventory} />
      {/* EXISTING DATA, ON THE PRIMARY ACCOUNT ONLY. Assigning rows written
          before accounts were recorded is the cluster's work, not any one
          client's, and the primary account is the cluster's own page. */}
      {self ? <ExistingDataSection reading={assignment} accounts={accounts} /> : null}
    </Details>
  );
}
