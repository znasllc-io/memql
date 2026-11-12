import { useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";

import { Head } from "../../kit";
import { LocalTabs } from "../../kit/LocalTabs";
import { usePageScroll } from "../../kit/pageNavigation";
import { useSession } from "../../chrome/access";
import { useLiveView } from "../../live/liveView";
import { holds } from "../../system/roles";
import type { ArchiveAccountState, UpdateAccountState } from "./actions";
import { AccountDataTab } from "./AccountDataTab";
import { AccountDetailsTab } from "./AccountDetailsTab";
import { AccountPeopleTab } from "./AccountPeopleTab";
import { AccountSettingsTab } from "./AccountSettingsTab";
import { frontDoorFromRow, type FrontDoorRow } from "./frontDoor";
import { accountName, type AccountRow } from "./rows";
import { useAccountFrontDoors } from "./useAccountFrontDoors";

// ONE ACCOUNT, AS A PAGE OF TABS (design record 2026-11-12-account-export,
// section K). The Fleet machine page's shape: `LocalTabs` directly under the
// heading, the trail reading `Accounts > <name> > <tab>`, and the tab kept as
// React state by whoever opened the page.
//
// THIS FILE IS THE ONE PLACE THAT KNOWS THE TABS AND THEIR ORDER. Each tab is
// its own file with a small props interface, so a tab can grow (the Data
// tab's rows, the Export tab) without this page learning what it holds.
//
// ===========================================================================
// THE LEDGER IS THIS APP'S ONE IDEA
// ===========================================================================
// Every other app in the OS lists things the cluster OWNS. This one lists the
// people the cluster works FOR, and what of all that is theirs: People says
// who, Data says what. The inventory is the account's, not the viewer's -- it
// counts rows by the account recorded on them, which is why its tab exists
// only for a holder of `execute app:accounts/inventory`.

export type AccountTab = "details" | "people" | "data" | "settings";

const TAB_NAMES: Record<AccountTab, string> = {
  details: "Details",
  people: "People",
  data: "Data",
  settings: "Settings",
};

// Export joins between Data and Settings when its tab is built; Settings stays
// last, as it is on a machine.
const TAB_ORDER: readonly AccountTab[] = ["details", "people", "data", "settings"];

// A stable empty array: useSyncExternalStore compares snapshots by identity,
// and a fresh `[]` on every read is an infinite render loop.
const EMPTY_DOORS: FrontDoorRow[] = [];

export interface AccountPageProps {
  account: AccountRow;
  /** The accounts the app already holds, to name an account by. */
  accounts: readonly AccountRow[];
  /** The tab chosen on this account, or undefined for the first one offered. */
  tab?: AccountTab;
  onTab: (tab: AccountTab) => void;
  /** Back to the list of accounts. */
  onBack: () => void;
  update: UpdateAccountState;
  archive: ArchiveAccountState;
  onArchived: () => void;
  /** Opens the Users app on one of this client's groups. */
  onOpenGroup?: (groupId: string) => void;
  /** Opens the domain setup wizard. */
  onConfigureDomains?: () => void;
}

export function AccountPage({
  account,
  accounts,
  tab,
  onTab,
  onBack,
  update,
  archive,
  onArchived,
  onOpenGroup,
  onConfigureDomains,
}: AccountPageProps) {
  // WHO MAY SEE THE INVENTORY. The builtins carry the part; this is the
  // courtesy half, so a non-holder is offered no tab that would only be
  // refused, and with nothing else on it the Data tab is absent (DESIGN.md
  // rule 12). `accessEpoch` is the reactivity signal: the held set is read out
  // of band, and a grant written in this browser bumps it.
  const { access, accessEpoch } = useSession();
  const canInventory = useMemo(() => holds("execute", "app:accounts/inventory"), [accessEpoch]);
  const offered = TAB_ORDER.filter((t) => t !== "data" || canInventory);
  const current: AccountTab = tab !== undefined && offered.includes(tab) ? tab : offered[0]!;

  // VISITED TABS STAY MOUNTED, hidden, as a machine's views do: a profile
  // draft or an open Archive question survives a look at another tab, and a
  // read already taken is not taken again for a glance back.
  const [visited, setVisited] = useState<AccountTab[]>([current]);
  useEffect(() => {
    setVisited((held) => (held.includes(current) ? held : [...held, current]));
  }, [current]);
  const mounted = (visited.includes(current) ? visited : [...visited, current]).filter((t) => offered.includes(t));

  // EACH TAB ITS OWN SCROLL POSITION: the window's navigation restores a
  // position by this identity, so Back to a tab returns to where it was read.
  const scrollRoot = usePageScroll(`account:${account.id}:${current}`);

  // The front doors, live (epic memql#5168). RETAINED HERE, above the tabs,
  // rather than inside the rail or the Details facts: a collection does
  // nothing until retain() (clients/os/README.md), both tabs read it, and a
  // subscription torn down on every tab switch is the arrival cue claiming
  // news that did not arrive.
  const { source: doorSource } = useAccountFrontDoors();
  const doorView = useLiveView<Record<string, unknown>, FrontDoorRow>(
    doorSource,
    // KEYED ON NOTHING THAT CHANGES. `accountFrontDoorsOpen` takes no
    // arguments, so selecting a different account changes no read.
    "accounts:frontDoors",
    (rows) => rows.map(frontDoorFromRow).filter((d) => d.id !== ""),
  );
  const doors = useSyncExternalStore(
    useMemo(() => (doorView ? doorView.subscribe.bind(doorView) : () => () => {}), [doorView]),
    () => doorView?.snapshot.rows ?? EMPTY_DOORS,
  ) as FrontDoorRow[];
  // `role === "owner"` rather than a rank floor: v1:platform:accountFrontDoor
  // is clusterOwner tier, which mirrors auth.IsClusterOwner -- one role, not a
  // rung. An admin reads zero rows and no error, so the door's facts have to
  // be told which kind of empty they are looking at.
  const canReadDoors = (access?.role ?? "") === "owner";

  const name = accountName(account);
  const first = offered[0]!;

  function render(t: AccountTab): ReactNode {
    switch (t) {
      case "details":
        return <AccountDetailsTab account={account} doors={doors} canReadDoors={canReadDoors} />;
      case "people":
        return <AccountPeopleTab account={account} onOpenGroup={onOpenGroup} />;
      case "data":
        return <AccountDataTab account={account} accounts={accounts} />;
      case "settings":
        return (
          <AccountSettingsTab
            account={account}
            update={update}
            archive={archive}
            onArchived={onArchived}
            onConfigureDomains={onConfigureDomains}
            doors={doors}
            canReadDoors={canReadDoors}
          />
        );
    }
  }

  return (
    <div ref={scrollRoot} className="os-app-stack os-account-page">
      <Head
        navigationKey={`account:${account.id}:${current}`}
        title={name}
        breadcrumbs={[
          { label: "Accounts", onSelect: onBack },
          // The account's own crumb leads to its first tab, and is plain there:
          // a crumb to the page already showing goes nowhere.
          { label: name, onSelect: current === first ? undefined : () => onTab(first) },
          { label: TAB_NAMES[current] },
        ]}
        back={{ label: "Accounts", onSelect: onBack }}
      />
      <LocalTabs
        label="Account views"
        value={current}
        onChange={onTab}
        options={offered.map((t) => [t, TAB_NAMES[t]] as const)}
      />
      {mounted.map((t) => (
        <section key={t} className="os-account-tab" aria-label={TAB_NAMES[t]} hidden={t !== current}>
          {render(t)}
        </section>
      ))}
    </div>
  );
}
