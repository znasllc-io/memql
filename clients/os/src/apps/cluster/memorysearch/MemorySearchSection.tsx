import { useLayoutEffect, useRef, useState, type FocusEvent } from "react";
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
  changeModelState,
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
// owner's acts on it. The part's seed gives the page to owners; somebody granted
// the part reads it too. EVERY ACT IS AN ACTIVE OWNER'S, which a grant cannot
// say: the engine answers it from the reader's side (the status's mayAct), and
// the page draws no act where mayAct is false -- it would be refused.
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
// after the choices and the kept index's line, whichever asked. CHANGE MODEL IS
// ONE PLACE, whether it shows the choices or a switch: a refusal of an act
// pressed there is said at its end, so a read that swaps the two while the act
// is out still shows the answer. A refusal or a question left open lasts only
// while reads show the state it was given over (changeModelState), so it never
// comes back with the section hours later.

const items = (n: number) => `${formatCount(n)} ${n === 1 ? "item" : "items"}`;

/** Whose machines embed: the binding's indexer (the status row) or a running switch's (its summary). */
type Indexer = Pick<MemorySearchStatus, "indexedBy" | "indexedByName">;

function indexerIsMe(indexer: Indexer, me: string): boolean {
  return me !== "" && bareId(indexer.indexedBy) === me;
}

/**
 * Whose machines index the model, from the reader's side: the indexer's own
 * and the ones lent to them, which receive everyone's memory text too.
 */
function indexingLine(status: MemorySearchStatus, me: string): string {
  if (status.kind === "vendor") return "Paid by the cluster's vendor account";
  if (status.indexedBy === "") return "Nobody is named to index it yet";
  if (indexerIsMe(status, me)) return "Indexed on your machines and the ones lent to you";
  return `Indexed on ${status.indexedByName ? `${status.indexedByName}'s` : "another owner's"} machines and the ones lent to them`;
}

/**
 * The index job's wait, as the person reading it: the job says "the indexer's
 * machines" (memoryIndexWaitReason), which is "your machines" to the indexer
 * and the indexer's by name to anybody else. Which indexer is meant follows
 * which run waits: the backfill embeds as the BINDING's indexer, a switch's
 * rebuild as the RUN's, and the two need not be one person. Any other wording
 * is shown as the engine wrote it.
 */
function asReader(reason: string, indexer: Indexer, me: string): string {
  const whose = indexerIsMe(indexer, me) ? "your machines" : indexer.indexedByName ? `${indexer.indexedByName}'s machines` : "";
  return whose ? reason.replaceAll("the indexer's machines", whose) : reason;
}

function backlogLine(status: MemorySearchStatus, me: string): string {
  const reason = asReader(status.waitingReason, status, me);
  if (status.backlog > 0) return reason ? `${items(status.backlog)} waiting: ${reason}` : `${items(status.backlog)} waiting to be indexed`;
  return reason ? `Indexing waits: ${reason}` : "";
}

/**
 * Why the binding's take-over is offered (the row's takeOver). "Can no longer
 * index", never "its machines cannot": a demoted owner is a cause as often as
 * a machine that is off, and the status does not say which.
 */
function takeOverLine(status: MemorySearchStatus): string {
  if (status.indexedBy === "") return "Search and new items wait until an owner indexes it.";
  return `${status.indexedByName || "Its indexer"} can no longer index it, so search and new items wait.`;
}

/** Why a running switch's take-over is offered (its own takeOver): the RUN's indexer cannot index its target. */
function switchTakeOverLine(sw: MemorySearchSwitch): string {
  if (sw.indexedBy === "") return `The switch waits until an owner indexes ${sw.targetLabel}.`;
  return `${sw.indexedByName || "Its indexer"} can no longer index ${sw.targetLabel}, so the switch waits.`;
}

/** What choosing `target` will do, said before it is done (the brief's confirmation). */
function confirmationLine(status: MemorySearchStatus, target: MemorySearchChoice): string {
  const estimate = status.indexed + status.backlog;
  // A machine model is indexed on the chooser's machines AND the ones lent to
  // them, which receive everyone's memory text as well: said, not left out.
  const machines = "on your machines and the ones lent to you";
  if (status.providerRef === "") {
    return `Everything is indexed in the background ${target.kind === "vendor" ? "on the cluster's vendor account" : machines}. New items are indexed the same way from now on.`;
  }
  const where = target.kind === "vendor" ? `on the cluster's vendor account${estimate > 0 ? `, about ${items(estimate)}` : ""}` : machines;
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
    me={bareId(access?.userId ?? "")} />;
}

type Place = "inUse" | "change";

/**
 * Where keyboard focus goes once the page has removed the control that held
 * it: the model the choices hold checked (the model in use, or the one a
 * question is open about), as Fleet's board gives focus back to its task
 * after Clear; with nothing checked (no model yet, or one the reader's
 * machines do not offer), the first model that can be chosen; with no choices
 * drawn (a switch shows instead), the Change model section itself. Never an
 * act: Cancel under the focus of the Enter that started a switch would be one
 * keystroke from undoing it.
 */
function focusAfterLoss(page: HTMLElement | null): HTMLElement | null {
  if (page === null) return null;
  const radios = [...page.querySelectorAll<HTMLElement>('[role="radio"]')];
  return radios.find((radio) => radio.getAttribute("aria-checked") === "true")
    ?? radios.find((radio) => radio.getAttribute("aria-disabled") !== "true")
    ?? page.querySelector<HTMLElement>("[data-change-model]");
}

export function MemorySearchPage({ query, connected, visible, me }: {
  query: MemorySearchQueries | null;
  connected: boolean;
  visible: boolean;
  /** The reader's user id, bare. */
  me: string;
}) {
  const search = useMemorySearch(query, { visible, connected });
  const status = search.status;
  // Whether any act is drawn: the engine's word (mayAct), never the grant's.
  const mayChange = status?.mayAct === true;
  // The model a confirmation is open for, and the state it was picked over: a
  // read with another state drops it (changeModelState), in the same render.
  const [draft, setDraft] = useState<{ ref: string; over: string } | null>(null);
  if (draft !== null && changeModelState(status) !== draft.over) setDraft(null);
  const pick = (next: string | null) => setDraft(next === null || next === status?.providerRef ? null : { ref: next, over: changeModelState(status) });
  // Which part of the page the last act was pressed in: its refusal is said there.
  const [actedIn, setActedIn] = useState<Place>("change");
  const act = (place: Place, run: () => Promise<boolean>) => () => { setActedIn(place); return run(); };

  // KEYBOARD FOCUS SURVIVES WHAT THE PAGE TAKES AWAY (step 6 review). A
  // question that closes (Keep, Not now), an act that succeeds (a switch, a
  // take-over) and a read that moves the section each remove the control that
  // had focus, and the browser drops focus to the document's body, so the next
  // Tab starts over at the window's top. After a render that removed the
  // focused control, with focus left on nothing, focus goes where
  // focusAfterLoss says; focus somebody moved elsewhere stays there. Only while
  // the page is shown: a hidden pane takes nobody's focus.
  //
  // ONLY FOCUS THAT WAS STILL IN THE PAGE. Focus the reader took elsewhere --
  // another window, then nowhere -- left lastFocused naming a control here,
  // and a read that later took that control away (a switch flipping in a
  // background Cluster window) pulled focus back into this window. So focus
  // leaving the page forgets it. A control that loses focus because the page
  // is removing it was not left by the reader: it is the case above, and is
  // told apart by still being in the document once the blur is over (checked
  // after it, as some browsers blur a control they remove).
  const root = useRef<HTMLDivElement>(null);
  const lastFocused = useRef<HTMLElement | null>(null);
  useLayoutEffect(() => {
    const held = lastFocused.current;
    if (held === null || held.isConnected || !visible) return;
    lastFocused.current = null;
    if (document.activeElement === null || document.activeElement === document.body) focusAfterLoss(root.current)?.focus();
  });
  const focusLeft = (event: FocusEvent<HTMLDivElement>) => {
    const next = event.relatedTarget;
    if (next instanceof Node && event.currentTarget.contains(next)) return;
    const left = event.target as HTMLElement;
    queueMicrotask(() => {
      if (left.isConnected && lastFocused.current === left) lastFocused.current = null;
    });
  };
  return <div className="os-cluster os-memory-search" ref={root} onFocus={(event) => { lastFocused.current = event.target as HTMLElement; }} onBlur={focusLeft}>
    <Head title="Memory search" note={search.readError && status ? <HeadNote sentence="Memory search could not be refreshed." next="Showing the last reading." detail={search.readError} /> : null} />
    {status === null ? !connected ? <Notice sentence="Memory search loads when the cluster reconnects." />
      : search.readError ? <ReadRefused sentence="Memory search could not be read." reason={search.readError} retry={search.retry} />
      : <ContentSkeleton kind="detail" label="Loading memory search" />
    : <Details>
      <InUse status={status} me={me} search={search} mayChange={mayChange} refusal={actedIn === "inUse" ? search.error : ""} act={act} />
      {switchRunning(status) && !switchFlipped(status)
        ? <Switching status={status} sw={status.switch!} me={me} mayChange={mayChange} refusal={actedIn === "change" ? search.error : ""} search={search} act={act} />
        : mayChange ? <ChangeModel status={status} search={search} draft={draft?.ref ?? null} pick={pick}
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
 *
 * THE SWITCH'S OWN TAKE-OVER. The rebuild embeds as the RUN's
 * indexer, so a run whose indexer cannot index its target waits however healthy
 * the binding's indexer is. Choosing the target again makes the caller the
 * run's indexer (memorySearchTakeOverRun), and it is offered only where the
 * switch summary's takeOver says so. It stands with Cancel in one hold: it
 * arrives with a read and moves Cancel down, and a press aimed at Cancel must
 * not land on what took its place. The progress lines stay outside the hold,
 * because they change with every read and would hold Cancel each time.
 */
function Switching({ status, sw, me, mayChange, refusal, search, act }: { status: MemorySearchStatus; sw: MemorySearchSwitch; me: string; mayChange: boolean; refusal: string; search: MemorySearch; act: Act }) {
  const total = sw.estimatedTotal;
  const done = Math.min(sw.counts.done + sw.counts.skipped, total);
  const fraction = total > 0 ? done / total : 0;
  const held = sw.counts.failed + sw.counts.unsourced;
  const kinds = [sw.counts.failed > 0 ? `${formatCount(sw.counts.failed)} failed` : "", sw.counts.unsourced > 0 ? `${formatCount(sw.counts.unsourced)} cannot be rebuilt` : ""].filter(Boolean).join(", ");
  const deciding = sw.state === "needsDecision";
  return <DetailsSection title="Change model">
    <SettingRow label="Switching to">
      <div className="os-memory-search-stack" data-change-model tabIndex={-1}>
        <span className="os-mono os-memory-search-target">{sw.targetLabel}</span>
        {deciding ? <InPlaceAsk>
          <Notice tone="warn" sentence={`${items(held)} could not be indexed${kinds ? ` (${kinds})` : ""}.`}>
            <p className="os-caption">Switch anyway leaves them out of search until they are indexed again.</p>
            {mayChange ? <div className="os-panel-actions">
              <Button onClick={act("change", search.cancel)}>Cancel</Button>
              <Button tone="primary" onClick={act("change", search.switchAnyway)}>Switch anyway</Button>
            </div> : null}
          </Notice>
        </InPlaceAsk> : <div className="os-memory-search-progress">
          <span className="os-progress-track" role="progressbar" aria-label={`Re-indexing for ${sw.targetLabel}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(fraction * 100)}>
            <span className="os-progress-bar" style={{ width: `${fraction * 100}%` }} />
          </span>
          <p className="os-memory-search-line">{`Re-indexing ${formatCount(done)} of ${items(total)}. Search uses ${status.label} until this finishes.`}</p>
          {sw.state === "waiting" && sw.waitingReason ? <p className="os-caption os-memory-search-line">{`Waiting: ${asReader(sw.waitingReason, sw, me)}.`}</p> : null}
        </div>}
        {mayChange && (sw.takeOver || !deciding) ? <InPlaceAsk className="os-memory-search-stack">
          {sw.takeOver ? <Notice tone="warn" sentence={switchTakeOverLine(sw)}>
            <div className="os-panel-actions"><Button tone="primary" onClick={act("change", () => search.switchTo(sw.targetRef))}>Re-index on my machines</Button></div>
          </Notice> : null}
          {deciding ? null : <div className="os-panel-actions"><Button onClick={act("change", search.cancel)}>Cancel</Button></div>}
        </InPlaceAsk> : null}
        {refusal ? <Notice tone="error" sentence={refusal} /> : null}
      </div>
    </SettingRow>
  </DetailsSection>;
}

function choiceOption(choice: MemorySearchChoice): ChoiceOption {
  const machines = `${formatCount(choice.machines)} ${choice.machines === 1 ? "machine" : "machines"}`;
  const what = choice.kind === "vendor" ? "Paid by the cluster's vendor account" : machines;
  return {
    value: choice.providerRef,
    label: choice.label,
    description: choice.available ? what : `${what}. ${choice.hint}`,
    // A model the engine would refuse -- a machine model with none of its
    // machines online, which cannot be checked, or one whose vectors are wider
    // than the index holds -- is visible, with the reason, and not choosable.
    unavailable: !choice.available,
  };
}

function ChangeModel({ status, search, draft, pick, refusal, act }: {
  status: MemorySearchStatus;
  search: MemorySearch;
  /** The model a confirmation is open for. */
  draft: string | null;
  /** Opens the confirmation for a model, or closes it (null, or the model in use). */
  pick: (next: string | null) => void;
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
  // Absent while its own question is open, which already asks what it offers.
  const back = previous && target?.providerRef !== previous.providerRef
    ? choices?.find((choice) => choice.providerRef === previous.providerRef && choice.available) ?? null : null;
  const bound = status.providerRef !== "";
  return <DetailsSection title="Change model">
    <SettingRow label="Model">
      <div className="os-memory-search-stack" data-change-model tabIndex={-1}>
        {choices === null
          ? search.choicesError
            ? <ReadRefused sentence="The models you can choose could not be read." reason={search.choicesError} retry={search.retry} />
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
              <Button onClick={() => pick(null)}>{bound ? `Keep ${status.label}` : "Not now"}</Button>
              <Button tone="primary" onClick={act("change", async () => {
                const done = await search.switchTo(target.providerRef);
                if (done) pick(null);
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
 * is showing. Before its first read lands it answers as the page does: a
 * cluster that is away gets a sentence, not a skeleton (DESIGN.md: a skeleton
 * never implies that a disconnected read is progressing), then a refusal, then
 * the shape of the value while the read is out.
 */
export function MemorySearchLink({ visible = true, onOpen }: { visible?: boolean; onOpen: () => void }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const connected = useConnectionStatus() === "connected";
  const { status, readError } = useMemorySearch(connection?.query ?? null, { visible, connected, choices: false });
  const model = status === null ? !connected ? "Loads when the cluster reconnects" : readError ? "Could not be read" : null
    : status.providerRef ? status.label : "No model chosen yet";
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
