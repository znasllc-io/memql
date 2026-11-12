import { useEffect, useRef, useState } from "react";

import {
  Button,
  Caption,
  Head,
  NextPage,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
  Refine,
} from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import { useDebouncedValue, TEXT_DEBOUNCE_MS } from "../../logs/useDebouncedValue";
import { rowsPhrase } from "./inventory";
import { kindName, MOVE_LIMIT, rowKey, type KindRow, type RowRef } from "./moves";
import { useKindRows } from "./useAccounts";

// THE ROWS OF ONE KIND IN ONE ACCOUNT, as a page under the trail (design record
// 2026-11-12-account-export, section K): `Accounts > <name> > Data > <area> >
// <kind>`. Opened from a kind's line in the inventory, or from "In no account"
// on the primary account's page, where the rows belong to no account at all.
//
// A PAGE, NOT AN EXPANSION AND NOT A POP-UP (DESIGN.md rule 11): a kind can
// hold thousands of rows, and a list that tall inside the inventory would put
// two lists in one scroll column. The account page keeps its tabs mounted and
// hidden behind this one, so Back lands on the Data tab where it was read.
//
// DRAWN ONLY FOR A HOLDER of `execute app:accounts/move`; the account page
// decides that, and for anybody else a kind's line does not open.
//
// SELECTING IS A CHOICE ON THE ROW (`RecordRow` `selected`, its pressed
// state), and the one act it brings stands in the window's bottom bar (rule
// 12): the count in words, and Move to account. A row that follows a parent
// is a plain line, not a choice: it moves when its parent does, and says so on
// the row.

export interface AccountRowsProps {
  /** The account whose rows these are; empty is the rows in no account. */
  accountId: string;
  concept: string;
  /** The kind's name, which titles the page. */
  kind: string;
  /** Published to the window's one trail row, this page last. */
  breadcrumbs: readonly Breadcrumb[];
  /** Back to the Data tab. */
  onBack: () => void;
  /** Opens the wizard on the chosen rows. */
  onMove: (rows: readonly RowRef[]) => void;
  /** Bumped after a move or a decision: read again, and let go of the choice. */
  revision: number;
  /**
   * The rows the last move refused (`rowKey`). They stay chosen through the
   * revision that follows, so what is still chosen is what still needs a
   * decision; everything else is let go.
   */
  refused?: ReadonlySet<string>;
}

const NOTHING_REFUSED: ReadonlySet<string> = new Set();

export function AccountRows({ accountId, concept, kind, breadcrumbs, onBack, onMove, revision, refused = NOTHING_REFUSED }: AccountRowsProps) {
  const [searchText, setSearchText] = useState("");
  const search = useDebouncedValue(searchText.trim(), TEXT_DEBOUNCE_MS);
  const feed = useKindRows(accountId, concept, search, revision);
  // Chosen rows by id. KEPT ACROSS A SEARCH: narrowing the list to find one
  // more row must not drop the ones already chosen.
  const [chosen, setChosen] = useState<ReadonlyMap<string, KindRow>>(() => new Map());
  // A move has happened: what moved is let go, what was refused stays chosen,
  // and the list is read again. Keyed on the revision alone: the refused set
  // arrives with it.
  const refusedNow = useRef(refused);
  refusedNow.current = refused;
  const firstRevision = useRef(true);
  useEffect(() => {
    if (firstRevision.current) {
      firstRevision.current = false;
      return;
    }
    setChosen((held) => new Map([...held].filter(([, r]) => refusedNow.current.has(rowKey(r.concept || concept, r.id)))));
  }, [revision]);

  const toggle = (row: KindRow) =>
    setChosen((held) => {
      const next = new Map(held);
      if (next.has(row.id)) next.delete(row.id);
      else next.set(row.id, row);
      return next;
    });

  const searching = searchText.trim() !== "" || search !== "";

  let body;
  if (feed.state === "failed") {
    body = (
      <Notice tone="error" sentence="The rows were not read." next="Nothing below is current." detail={feed.error}>
        <Button onClick={feed.reread}>Try again</Button>
      </Notice>
    );
  } else if (feed.state === "unread") {
    body = <Caption>Not connected to the cluster.</Caption>;
  } else if (feed.state === "reading" && feed.rows.length === 0) {
    body = <RecordListSkeleton label="Loading rows" rows={5} />;
  } else if (feed.rows.length === 0) {
    body = <Caption>{searching ? "No rows match." : "No rows of this kind here."}</Caption>;
  } else {
    body = (
      <>
        <RecordList as="ul" label={`Rows of ${kind}`}>
          {feed.rows.map((row) => (
            <li key={row.id}>
              <KindRowLine row={row} chosen={chosen.has(row.id)} onToggle={() => toggle(row)} />
            </li>
          ))}
        </RecordList>
        <NextPage feed={feed} label="Loading more rows" />
      </>
    );
  }

  const picked = [...chosen.values()];
  const tooMany = picked.length > MOVE_LIMIT;
  // CHOSEN BUT NOT SHOWN: a search can hide rows already chosen, and they
  // still move. The bar says how many, and Clear lets go of all of them, so
  // nothing moves that the person cannot account for.
  const shown = new Set(feed.rows.map((r) => r.id));
  const hidden = picked.filter((r) => !shown.has(r.id)).length;
  const acts: Act[] = [{ label: "Clear", text: true, onAct: () => setChosen(new Map()) }];
  if (!tooMany) {
    acts.push({ label: "Move to account", tone: "primary", onAct: () => onMove(picked.map((r) => ({ concept: r.concept || concept, id: r.id }))) });
  }

  // NO FILTER CHROME OVER NO CONTENT (rule 2): the search is offered once
  // there is something to search, and stays while a search is being asked.
  const offerSearch = searching || feed.rows.length > 0;

  return (
    <div className="os-app-stack os-account-rows">
      <Head
        navigationKey={`account:${accountId}:rows:${concept}`}
        title={kind}
        breadcrumbs={breadcrumbs}
        back={{ label: "Data", onSelect: onBack }}
      >
        {offerSearch ? <Refine search={searchText} onSearch={setSearchText} label={`Search rows of ${kind}`} /> : null}
      </Head>
      {body}
      {picked.length === 0 ? null : (
        <ActionBar
          state={`${rowsPhrase(picked.length)} selected${hidden === 0 ? "" : `, ${hidden.toLocaleString("en-US")} not shown`}`}
          detail={tooMany ? `At most ${MOVE_LIMIT.toLocaleString("en-US")} rows move at once.` : undefined}
          acts={acts}
        />
      )}
    </div>
  );
}

function KindRowLine({ row, chosen, onToggle }: { row: KindRow; chosen: boolean; onToggle: () => void }) {
  const name = row.primary || row.id;
  if (row.followsParent) {
    // NOT A CHOICE, AND IT SAYS WHY ON THE ROW rather than in a tooltip only:
    // the kind it moves with, named as the inventory names kinds, when the
    // answer carries it (`parentConcept`), and a plain sentence when not.
    return (
      <RecordRow name={name} secondary={row.secondary || undefined}>
        <span className="os-caption">
          {row.parentConcept === "" ? "Moves with the row it belongs to" : `Moves with its ${kindName(row.parentConcept)}`}
        </span>
        {row.status === "" ? null : <span className="os-caption">{row.status}</span>}
      </RecordRow>
    );
  }
  return (
    <RecordRow name={name} secondary={row.secondary || undefined} selected={chosen} onOpen={onToggle}>
      {row.status === "" ? null : <span className="os-caption">{row.status}</span>}
    </RecordRow>
  );
}
