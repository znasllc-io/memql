import { useState } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../../chrome/access";
import { useConnectionStatus } from "../../../chrome/connection";
import { useOsConnection } from "../../../live/connection";
import { Button, ChoiceStack, ContentSkeleton, Details, DetailsSection, Fact, Facts, Head, HeadNote, InlineSkeleton, Notice, RecordList, RecordRow, SettingRow, type ChoiceOption } from "../../../kit";
import { formatCount } from "../../../kit/format";
import { InPlaceAsk } from "../../../kit/pressHold";
import { holds } from "../../../system/roles";
import { bareId } from "../../settings/access/model";
import {
  MEMORY_SEARCH_RESOURCE,
  switchFlipped,
  switchRunning,
  useMemorySearch,
  type MemorySearch,
  type MemorySearchChoice,
  type MemorySearchQueries,
  type MemorySearchStatus,
  type MemorySearchSwitch,
} from "./useMemorySearch";
import "./memorySearch.css";

// CLUSTER > SETTINGS > MEMORY SEARCH (design record
// docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md,
// section 8): the model memory search uses, whose machines index it, and the
// owner's acts on it. Owners only, by the part's seed; the engine refuses the
// builtins to everybody else, and this page draws nothing it would refuse.
//
// WHAT FOLLOWS THE STATE (DESIGN.md rule 12): an act that is not legal is
// absent. While a switch runs before its flip, no other model is offered (one
// switch at a time) and Switch back is absent; Cancel is offered, and refused
// by the engine only once the binding names the target, which is when this
// page stops offering it (a flipped switch reads as after a switch). Every
// question answered in place -- the confirmation, Keep, Switch anyway, the
// take-over, even Cancel and Switch back, which can appear on their own when a
// read lands -- is held a moment after it appears (InPlaceAsk), so the second
// click of a double-click never answers it.
//
// WHERE AN ANSWER APPEARS: below what was pressed, never above it, so nothing
// moves under the pointer. The confirmation stands at the end of Change model,
// after the choices and the kept index's line, whichever asked.

const items = (n: number) => `${formatCount(n)} ${n === 1 ? "item" : "items"}`;

function indexerIsMe(status: MemorySearchStatus, me: string): boolean {
  return me !== "" && bareId(status.indexedBy) === me;
}

/** Whose machines index the model, from the reader's side. */
function indexingLine(status: MemorySearchStatus, me: string): string {
  if (status.kind === "vendor") return "Paid by the cluster's vendor account";
  if (status.indexedBy === "") return "Nobody is named to index it yet";
  if (indexerIsMe(status, me)) return "Indexed on your machines";
  return status.indexedByName ? `Indexed on ${status.indexedByName}'s machines` : "Indexed on another owner's machines";
}

/**
 * The index job's wait, as the person reading it: the job says "the indexer's
 * machines" (memoryIndexWaitReason), which is "your machines" to the indexer
 * and the indexer's by name to anybody else. The backfill embeds as the
 * binding's indexer, so it is the binding's indexer who is meant. Any other
 * wording is shown as the engine wrote it.
 */
function asReader(reason: string, status: MemorySearchStatus, me: string): string {
  const whose = indexerIsMe(status, me) ? "your machines" : status.indexedByName ? `${status.indexedByName}'s machines` : "";
  return whose ? reason.replaceAll("the indexer's machines", whose) : reason;
}

function backlogLine(status: MemorySearchStatus, me: string): string {
  const reason = asReader(status.waitingReason, status, me);
  if (status.backlog > 0) return reason ? `${items(status.backlog)} waiting: ${reason}` : `${items(status.backlog)} waiting to be indexed`;
  return reason ? `Indexing waits: ${reason}` : "";
}

/** Why the take-over is offered: the status said choosing the model again takes over (takeOver). */
function takeOverLine(status: MemorySearchStatus): string {
  if (status.indexedBy === "") return "Search and new items wait until an owner indexes it.";
  return `${status.indexedByName ? `${status.indexedByName}'s` : "Its indexer's"} machines cannot index it, so search and new items wait.`;
}

/** What choosing `target` will do, said before it is done (the brief's confirmation). */
function confirmationLine(status: MemorySearchStatus, target: MemorySearchChoice): string {
  const estimate = status.indexed + status.backlog;
  if (status.providerRef === "") {
    return `Everything is indexed in the background ${target.kind === "vendor" ? "on the cluster's vendor account" : "on your machines"}. New items are indexed the same way from now on.`;
  }
  const where = target.kind === "vendor" ? `on the cluster's vendor account${estimate > 0 ? `, about ${items(estimate)}` : ""}` : "on your machines";
  return `Everything is re-indexed in the background ${where}. New items are indexed the same way from now on. Search keeps using ${status.label} until the rebuild finishes, then switches by itself. The current index is kept for 30 days in case you switch back.`;
}

function keptUntil(retireAt: string): string {
  const at = new Date(retireAt);
  return Number.isNaN(at.getTime()) ? "" : at.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

/** Memory search, gated on its part: nobody else is read for, or offered anything. */
export function MemorySearchSection({ visible = true }: { visible?: boolean }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const status = useConnectionStatus();
  if (!holds("read", MEMORY_SEARCH_RESOURCE)) {
    return <div className="os-cluster"><Head title="Memory search" /><Notice sentence="A cluster owner chooses memory search's model." /></div>;
  }
  return <MemorySearchPage key={access?.userId ?? ""} query={connection?.query ?? null} connected={status === "connected"} visible={visible}
    me={bareId(access?.userId ?? "")} mayChange={holds("execute", MEMORY_SEARCH_RESOURCE)} />;
}

type Place = "inUse" | "change" | "switch";

export function MemorySearchPage({ query, connected, visible, me, mayChange }: {
  query: MemorySearchQueries | null;
  connected: boolean;
  visible: boolean;
  /** The reader's user id, bare. */
  me: string;
  /** `execute` on the part: without it every act is absent. */
  mayChange: boolean;
}) {
  const search = useMemorySearch(query, { visible, connected });
  const [draft, setDraft] = useState<string | null>(null);
  // Which part of the page the last act was pressed in: its refusal is said there.
  const [actedIn, setActedIn] = useState<Place>("change");
  const status = search.status;
  const act = (place: Place, run: () => Promise<boolean>) => () => { setActedIn(place); return run(); };
  return <div className="os-cluster os-memory-search">
    <Head title="Memory search" note={search.readError && status ? <HeadNote sentence="Memory search could not be refreshed." next="Showing the last reading." detail={search.readError} /> : null} />
    {status === null ? !connected ? <Notice sentence="Memory search loads when the cluster reconnects." />
      : search.readError ? <ReadRefused sentence="Memory search could not be read." reason={refusalSentence(search.readError, "memorySearchStatus")} retry={search.retry} />
      : <ContentSkeleton kind="detail" label="Loading memory search" />
    : <Details>
      <InUse status={status} me={me} search={search} mayChange={mayChange} refusal={actedIn === "inUse" ? search.error : ""} act={act} />
      {switchRunning(status) && !switchFlipped(status)
        ? <Switching status={status} sw={status.switch!} mayChange={mayChange} refusal={actedIn === "switch" ? search.error : ""} search={search} act={act} />
        : mayChange ? <ChangeModel status={status} search={search} draft={draft === status.providerRef ? null : draft} setDraft={setDraft}
          refusal={actedIn === "change" ? search.error : ""} act={act} /> : null}
    </Details>}
  </div>;
}

type Act = (place: Place, run: () => Promise<boolean>) => () => Promise<boolean>;

/** A read that failed: what could not be read, the engine's reason, then Try again (the
 *  read also tries again on its own, at its cadence). */
function ReadRefused({ sentence, reason, retry }: { sentence: string; reason: string; retry: () => void }) {
  return <Notice tone="error" sentence={sentence}>
    {reason ? <p className="os-notice-detail os-mono">{reason}</p> : null}
    <div className="os-panel-actions"><Button onClick={retry}>Try again</Button></div>
  </Notice>;
}

/**
 * The model in use. Its two questions -- confirm a vendor model nobody chose,
 * take over an indexer who cannot index -- stand in the Model fact's own value,
 * under what they are about. The vendor question replaces the "paid by" line it
 * would repeat.
 */
function InUse({ status, me, search, mayChange, refusal, act }: { status: MemorySearchStatus; me: string; search: MemorySearch; mayChange: boolean; refusal: string; act: Act }) {
  if (status.providerRef === "") return <DetailsSection title="In use"><Facts><Fact label="Model" value="Not chosen yet" /></Facts></DetailsSection>;
  const backlog = backlogLine(status, me);
  const asking = status.needsConfirmation && mayChange;
  return <DetailsSection title="In use">
    <Facts>
      <Fact label="Model" value={<>
        <span className="os-mono">{status.label}</span>
        {asking ? null : <span className="os-caption os-memory-search-note">{indexingLine(status, me)}</span>}
        {asking || (status.takeOver && mayChange) || refusal ? <div className="os-memory-search-stack os-memory-search-asks">
          {asking ? <InPlaceAsk>
            <Notice sentence={`This cluster uses ${status.label}, paid by the cluster's vendor account. Keep it, or choose a model on your machines.`}>
              <div className="os-panel-actions"><Button tone="primary" onClick={act("inUse", search.confirm)}>Keep</Button></div>
            </Notice>
          </InPlaceAsk> : null}
          {status.takeOver && mayChange ? <InPlaceAsk>
            <Notice tone="warn" sentence={takeOverLine(status)}>
              <div className="os-panel-actions"><Button tone="primary" onClick={act("inUse", () => search.switchTo(status.providerRef))}>Index on my machines</Button></div>
            </Notice>
          </InPlaceAsk> : null}
          {refusal ? <Notice tone="error" sentence={refusal} /> : null}
        </div> : null}
      </>} />
      <Fact label="Index" value={<>{`${items(status.indexed)} indexed`}{backlog ? <span className="os-caption os-memory-search-note">{backlog}</span> : null}</>} />
    </Facts>
  </DetailsSection>;
}

/**
 * A switch before its flip. "Done" is what the new index holds so far in this
 * walk (embedded or already there), and is CLAMPED to the estimated total: the
 * estimate is the active index plus its backlog, not a count of the corpus, and
 * a walk that finds more than it would otherwise read "8,400 of 8,310".
 */
function Switching({ status, sw, mayChange, refusal, search, act }: { status: MemorySearchStatus; sw: MemorySearchSwitch; mayChange: boolean; refusal: string; search: MemorySearch; act: Act }) {
  const total = sw.estimatedTotal;
  const done = Math.min(sw.counts.done + sw.counts.skipped, total);
  const fraction = total > 0 ? done / total : 0;
  const held = sw.counts.failed + sw.counts.unsourced;
  const kinds = [sw.counts.failed > 0 ? `${formatCount(sw.counts.failed)} failed` : "", sw.counts.unsourced > 0 ? `${formatCount(sw.counts.unsourced)} cannot be rebuilt` : ""].filter(Boolean).join(", ");
  return <DetailsSection title="Change model">
    <SettingRow label="Switching to">
      <div className="os-memory-search-stack">
        <span className="os-mono os-memory-search-target">{sw.targetLabel}</span>
        {sw.state === "needsDecision" ? <InPlaceAsk>
          <Notice tone="warn" sentence={`${items(held)} could not be indexed${kinds ? ` (${kinds})` : ""}.`}>
            <p className="os-caption">Switch anyway leaves them out of search until they are indexed again.</p>
            {mayChange ? <div className="os-panel-actions">
              <Button onClick={act("switch", search.cancel)}>Cancel</Button>
              <Button tone="primary" onClick={act("switch", search.switchAnyway)}>Switch anyway</Button>
            </div> : null}
          </Notice>
        </InPlaceAsk> : <div className="os-memory-search-progress">
          <span className="os-progress-track" role="progressbar" aria-label={`Re-indexing for ${sw.targetLabel}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(fraction * 100)}>
            <span className="os-progress-bar" style={{ width: `${fraction * 100}%` }} />
          </span>
          <p className="os-memory-search-line">{`Re-indexing ${formatCount(done)} of ${items(total)}. Search uses ${status.label} until this finishes.`}</p>
          {sw.state === "waiting" && sw.waitingReason ? <p className="os-caption os-memory-search-line">{`Waiting: ${sw.waitingReason}.`}</p> : null}
          {mayChange ? <InPlaceAsk className="os-panel-actions"><Button onClick={act("switch", search.cancel)}>Cancel</Button></InPlaceAsk> : null}
        </div>}
        {refusal ? <Notice tone="error" sentence={refusal} /> : null}
      </div>
    </SettingRow>
  </DetailsSection>;
}

function choiceOption(choice: MemorySearchChoice): ChoiceOption {
  const machines = `${formatCount(choice.machines)} ${choice.machines === 1 ? "machine" : "machines"}`;
  return {
    value: choice.providerRef,
    label: choice.label,
    description: choice.kind === "vendor" ? "Paid by the cluster's vendor account" : choice.available ? machines : `${machines}. ${choice.hint}`,
    // A machine model with none of its machines online cannot be checked, so
    // the engine would refuse it: visible, with the reason, and not choosable.
    unavailable: !choice.available,
  };
}

function ChangeModel({ status, search, draft, setDraft, refusal, act }: {
  status: MemorySearchStatus;
  search: MemorySearch;
  draft: string | null;
  setDraft: (next: string | null) => void;
  refusal: string;
  act: Act;
}) {
  const choices = search.choices;
  const target = draft === null ? null : choices?.find((choice) => choice.providerRef === draft) ?? null;
  // The kept index Switch back names is the model the binding replaced, never a
  // cancelled switch's partial one; and it is offered only where choosing it
  // can work, which is where the engine can check it.
  const previous = status.previousRef === "" ? null : status.retained.find((kept) => kept.providerRef === status.previousRef) ?? null;
  const until = previous ? keptUntil(previous.retireAt) : "";
  const back = previous ? choices?.find((choice) => choice.providerRef === previous.providerRef && choice.available) ?? null : null;
  const pick = (next: string | null) => setDraft(next === status.providerRef ? null : next);
  const bound = status.providerRef !== "";
  return <DetailsSection title="Change model">
    <SettingRow label="Model">
      <div className="os-memory-search-stack">
        {choices === null
          ? search.choicesError
            ? <ReadRefused sentence="The models you can choose could not be read." reason={refusalSentence(search.choicesError, "memorySearchChoices")} retry={search.retry} />
            : <ContentSkeleton kind="form" label="Loading the models you can choose" />
          : choices.length === 0
            ? <p className="os-caption">None of your machines offers an embedding model, and no vendor account is set up for one.</p>
            : <ChoiceStack name="memory-search-model" label="Memory search model" value={target?.providerRef ?? status.providerRef} onChange={pick} options={choices.map(choiceOption)} />}
        {previous && until ? <div className="os-memory-search-kept">
          <p className="os-memory-search-line">{`The previous model's index is kept until ${until}.`}</p>
          {back ? <InPlaceAsk><Button onClick={() => pick(back.providerRef)}>Switch back</Button></InPlaceAsk> : null}
        </div> : null}
        {target ? <InPlaceAsk key={target.providerRef}>
          <Notice sentence={confirmationLine(status, target)}>
            <div className="os-panel-actions">
              <Button onClick={() => setDraft(null)}>{bound ? `Keep ${status.label}` : "Not now"}</Button>
              <Button tone="primary" onClick={act("change", async () => {
                const done = await search.switchTo(target.providerRef);
                if (done) setDraft(null);
                return done;
              })}>{bound ? `Switch to ${target.label}` : `Use ${target.label}`}</Button>
            </div>
          </Notice>
        </InPlaceAsk> : null}
        {refusal ? <Notice tone="error" sentence={refusal} /> : null}
      </div>
    </SettingRow>
  </DetailsSection>;
}

/**
 * Settings' link row: the model in use, opening Memory search. It reads the
 * status alone (no choices), at the page's own cadence, and only while Settings
 * is showing.
 */
export function MemorySearchLink({ visible = true, onOpen }: { visible?: boolean; onOpen: () => void }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const connected = useConnectionStatus() === "connected";
  const { status, readError } = useMemorySearch(connection?.query ?? null, { visible, connected, choices: false });
  const model = status === null ? readError ? "Could not be read" : null : status.providerRef ? status.label : "No model chosen yet";
  return <DetailsSection title="Memory search">
    <RecordList as="ul">
      <RecordRow
        name={model === null ? <InlineSkeleton label="Loading the model in use" /> : <span className={status?.providerRef ? "os-mono" : undefined}>{model}</span>}
        secondary={status?.providerRef ? indexingLine(status, bareId(access?.userId ?? "")) : undefined}
        label={model === null ? "Memory search" : `Memory search: ${model}`}
        onOpen={onOpen}
      />
    </RecordList>
  </DetailsSection>;
}
