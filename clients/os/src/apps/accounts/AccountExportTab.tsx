import { useState } from "react";
import { ArrowUpRight, Trash2 } from "lucide-react";

import {
  AutoRefresh,
  Button,
  Caption,
  ContentSkeleton,
  Details,
  DetailsSection,
  Fact,
  Facts,
  NextPage,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
  Subhead,
  formatMoment,
} from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { IconButton } from "../../kit/IconButton";
import { InPlaceAsk } from "../../kit/pressHold";
import type { Reading } from "../../cluster/reading";
import { useAuthSource } from "../../auth/context";
import {
  CONDITION_ORDER,
  conditionLabel,
  conditionState,
  downloadAccountExport,
  exportIsActive,
  exportIsRemoved,
  exportProgress,
  exportSize,
  exportStateWord,
  failureSentence,
  refusalOf,
  type ExportReadiness,
  type ExportRow,
} from "./exports";
import { rowsPhrase } from "./inventory";
import { useExportCalls, type ExportsFeed } from "./useAccounts";

// THE EXPORT TAB (design record 2026-11-12-account-export, section K).
//
// DRAWN ONLY FOR A HOLDER of `execute app:accounts/export`; the account page
// decides that, and for anybody else there is no tab.
//
// TWO STATES, AND THE READINESS ANSWER DECIDES WHICH:
//
//   Not ready   the five conditions as facts, each in words with its count.
//               No Add control: an export the engine would refuse is not
//               offered (rule 12). It reads itself again on the shared
//               recovery cadence, so the tab turns ready without a press.
//   Ready       "Exports" with the Add control, and the account's exports,
//               newest first, loaded on scroll. Read again every three
//               seconds while one is active and every fifteen otherwise.
//
// Under either, one quiet line counts the rows in no account and leads to
// the primary account's Data tab, which is where they are placed.

/** While an export is prepared, waiting or running. */
export const ACTIVE_REFRESH_MS = 3_000;
export const IDLE_REFRESH_MS = 15_000;

export function AccountExportTab({
  readiness,
  feed,
  onAdd,
  onOpenExport,
  onOpenUnplaced,
}: {
  readiness: Reading<ExportReadiness>;
  /** The account's exports, held by the page so the wizard's acts land in it. */
  feed: ExportsFeed;
  /** Opens "Export this account". */
  onAdd: () => void;
  /** Opens the wizard where an active export stands. */
  onOpenExport: (x: ExportRow) => void;
  /** The primary account's Data tab. */
  onOpenUnplaced: () => void;
}) {
  let body;
  if (readiness.state === "failed") {
    body = (
      <DetailsSection title="Readiness">
        <Notice tone="error" sentence="Whether this account can be exported was not read." next="Nothing below is current." detail={readiness.error}>
          <Button onClick={readiness.reread}>Try again</Button>
        </Notice>
      </DetailsSection>
    );
  } else if (readiness.value === null) {
    body = readiness.state === "unread" ? (
      <Caption>Not connected to the cluster.</Caption>
    ) : (
      <ContentSkeleton kind="detail" label="Loading readiness" />
    );
  } else if (!readiness.value.ready) {
    body = <NotReady readiness={readiness.value} />;
  } else {
    body = <ExportsList feed={feed} onAdd={onAdd} onOpenExport={onOpenExport} />;
  }

  const unplaced = readiness.value?.unplaced ?? 0;
  return (
    <Details>
      {/* Not ready: read again on the shared cadence until it is. */}
      {readiness.value !== null && !readiness.value.ready ? (
        <AutoRefresh onRefresh={readiness.reread} busy={readiness.state === "reading"} />
      ) : null}
      {body}
      {unplaced === 0 ? null : (
        <div className="os-account-ledger-head">
          <Caption>{rowsPhrase(unplaced)} in no account</Caption>
          <IconButton label="Rows in no account" onClick={onOpenUnplaced}>
            <ArrowUpRight size={16} aria-hidden />
          </IconButton>
        </div>
      )}
    </Details>
  );
}

function NotReady({ readiness }: { readiness: ExportReadiness }) {
  // In the page's order, then anything the engine added after them.
  const named = CONDITION_ORDER.flatMap((name) => readiness.conditions.filter((c) => c.name === name));
  const rest = readiness.conditions.filter((c) => !(CONDITION_ORDER as readonly string[]).includes(c.name));
  return (
    <DetailsSection title="Readiness">
      <Facts>
        {[...named, ...rest].map((c) => (
          <Fact key={c.name} label={conditionLabel(c.name)} value={conditionState(c)} />
        ))}
      </Facts>
    </DetailsSection>
  );
}

function ExportsList({ feed, onAdd, onOpenExport }: { feed: ExportsFeed; onAdd: () => void; onOpenExport: (x: ExportRow) => void }) {
  const active = feed.exports.some(exportIsActive);
  let list;
  if (feed.state === "failed") {
    list = (
      <Notice tone="error" sentence="The exports were not read." next="Nothing below is current." detail={feed.error}>
        <Button onClick={feed.reread}>Try again</Button>
      </Notice>
    );
  } else if (feed.state === "unread") {
    list = <Caption>Not connected to the cluster.</Caption>;
  } else if (feed.state === "reading" && feed.exports.length === 0) {
    list = <RecordListSkeleton label="Loading exports" rows={3} />;
  } else if (feed.exports.length === 0) {
    list = <Caption>No exports yet.</Caption>;
  } else {
    list = (
      <>
        <RecordList as="ul" label="Exports">
          {feed.exports.map((x) => (
            <li key={x.id}>
              <ExportLine x={x} onOpen={() => onOpenExport(x)} onChanged={feed.upsert} />
            </li>
          ))}
        </RecordList>
        <NextPage feed={feed} label="Loading more exports" />
      </>
    );
  }
  return (
    <section aria-label="Exports">
      <AutoRefresh
        onRefresh={feed.refresh}
        busy={feed.state === "reading"}
        enabled={feed.state === "read"}
        intervalMs={active ? ACTIVE_REFRESH_MS : IDLE_REFRESH_MS}
      />
      <Subhead actions={<AddButton label="Export this account" onClick={onAdd} />}>Exports</Subhead>
      {list}
    </section>
  );
}

function ExportLine({ x, onOpen, onChanged }: { x: ExportRow; onOpen: () => void; onChanged: (x: ExportRow) => void }) {
  const auth = useAuthSource();
  const calls = useExportCalls();
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ sentence: string; detail: string } | null>(null);
  const when = formatMoment(x.requestedAt);
  const removed = exportIsRemoved(x);
  const downloadable = x.state === "complete" && !removed;
  const active = exportIsActive(x);

  async function download() {
    setBusy(true);
    setError(null);
    try {
      await downloadAccountExport(x, () => auth.bearer());
    } catch (err: unknown) {
      setError({ sentence: "The file was not downloaded.", detail: refusalOf(err).detail });
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (calls === null || busy) return;
    setBusy(true);
    setError(null);
    try {
      onChanged(await calls.remove(x.id));
      setAsking(false);
    } catch (err: unknown) {
      setError({ sentence: "The export was not removed.", detail: refusalOf(err).detail });
    } finally {
      setBusy(false);
    }
  }

  const facts: string[] = [];
  // NAMES, NEVER IDS: a fact the answer cannot name a person for is left out
  // rather than printed as an id.
  if (removed) facts.push(x.removedByName !== "" ? `Removed by ${x.removedByName}, ${formatMoment(x.removedAt)}` : `Removed, ${formatMoment(x.removedAt)}`);
  else if (active && x.state !== "draft") facts.push(exportProgress(x));
  if (!removed && exportSize(x) !== "") facts.push(exportSize(x));
  if (x.protected) facts.push("Protected");
  if (x.requestedByName !== "") facts.push(`Started by ${x.requestedByName}`);

  return (
    <>
      <RecordRow
        name={when}
        state={exportStateWord(x)}
        tone={x.state === "failed" ? "warn" : x.state === "complete" && !removed ? "accent" : "muted"}
        dim={removed || x.state === "cancelled"}
        onOpen={active ? onOpen : undefined}
        label={active ? `${when}, ${exportStateWord(x)}` : undefined}
        actions={downloadable && !asking ? (
          <>
            <Button onClick={() => void download()} busy={busy} ariaLabel={`Download the export of ${when}`}>Download</Button>
            <IconButton label="Remove this export" onClick={() => { setError(null); setAsking(true); }}>
              <Trash2 size={16} aria-hidden />
            </IconButton>
          </>
        ) : undefined}
      >
        {facts.map((f) => <span key={f} className="os-caption">{f}</span>)}
      </RecordRow>
      {x.state === "failed" ? (
        <Notice tone="warn" sentence={failureSentence(x.failureCode)} detail={x.failureDetail || undefined} />
      ) : null}
      {asking ? (
        <InPlaceAsk role="group" aria-label="Remove this export?">
          <Caption>Remove the file of {when}? It cannot be downloaded again; the record of it stays.</Caption>
          <div className="os-panel-actions">
            <Button onClick={() => { setAsking(false); setError(null); }}>Keep it</Button>
            <Button tone="danger" busy={busy} onClick={() => void remove()}>Remove</Button>
          </div>
        </InPlaceAsk>
      ) : null}
      {error === null ? null : <Notice tone="error" sentence={error.sentence} detail={error.detail} />}
    </>
  );
}
