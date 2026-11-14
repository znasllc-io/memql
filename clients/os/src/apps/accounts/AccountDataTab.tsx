import { Details } from "../../kit";
import { ExistingDataSection, InventorySection } from "./AccountInventory";
import type { InventoryArea, InventoryConcept } from "./inventory";
import type { OpenConflict } from "./moves";
import type { UnplacedKindChoice } from "./UnplacedKindPicker";
import { accountIsSelf, type AccountRow } from "./rows";
import { UnplacedSection } from "./UnplacedSection";
import { useAccountAssignmentStatus, useAccountUnplaced } from "./useAccounts";
import type { Reading } from "../../cluster/reading";
import type { Inventory } from "./inventory";

// THE DATA TAB: what the account holds (design record
// 2026-11-12-account-export, section K), from the inventory builtin.
//
// DRAWN ONLY FOR A HOLDER of `execute app:accounts/inventory`, and the page
// decides that: for anyone else there is no Data tab at all (DESIGN.md rule
// 12, absent rather than disabled), so this component assumes the part is
// held. The inventory reading itself is the page's (AccountPage.tsx), shared
// with the export wizard's Contents step.
//
// MOVING IS A SECOND PART, `execute app:accounts/move`, and the page decides
// that too: `move` is given only to a holder. Without it the inventory is
// counts and nothing opens, and the primary account's page has no rows
// awaiting a decision or in no account -- not even the read.

/** What a holder of the move part can do from this tab. */
export interface AccountDataMoves {
  /** A kind's line in the inventory opens its rows. */
  onOpenKind: (concept: InventoryConcept, area: InventoryArea) => void;
  /** A listed or explicitly chosen kind opens its rows in no account. */
  onOpenUnplaced: (kind: UnplacedKindChoice) => void;
  /** A conflict's Place opens the wizard. */
  onPlace: (conflict: OpenConflict) => void;
  /** A conflict was left out. */
  onChanged: () => void;
}

export interface AccountDataTabProps {
  account: AccountRow;
  /** The accounts the app already holds, to name an account by. */
  accounts: readonly AccountRow[];
  /** The page's one inventory reading, which the export wizard counts from too. */
  inventory: Reading<Inventory>;
  /** Present only for a holder of `execute app:accounts/move`. */
  moves?: AccountDataMoves;
  /** Bumped after a move or a decision: every read here is taken again. */
  revision?: number;
}

export function AccountDataTab({ account, accounts, inventory, moves, revision = 0 }: AccountDataTabProps) {
  const self = accountIsSelf(account);
  const assignment = useAccountAssignmentStatus(self, revision);
  const unplaced = useAccountUnplaced(self && moves !== undefined, revision);
  return (
    <Details>
      <InventorySection reading={inventory} onOpenKind={moves?.onOpenKind} />
      {/* EXISTING DATA, ON THE PRIMARY ACCOUNT ONLY. Assigning rows written
          before accounts were recorded is the cluster's work, not any one
          client's, and the primary account is the cluster's own page. */}
      {self ? <ExistingDataSection reading={assignment} accounts={accounts} showConflicts={moves === undefined} /> : null}
      {self && moves !== undefined ? (
        <UnplacedSection
          reading={unplaced}
          onPlace={moves.onPlace}
          onOpenKind={moves.onOpenUnplaced}
          onLeftOut={moves.onChanged}
        />
      ) : null}
    </Details>
  );
}
