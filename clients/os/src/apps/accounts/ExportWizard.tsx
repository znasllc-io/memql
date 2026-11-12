import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Box } from "lucide-react";

import {
  AutoRefresh,
  Caption,
  Check,
  ContentSkeleton,
  CopyValue,
  Fact,
  Facts,
  Notice,
  Subhead,
  Switch,
  formatBytes,
  useNow,
} from "../../kit";
import type { Act } from "../../kit/ActionBar";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import type { Stop } from "../../kit/Rail";
import { Wizard } from "../../kit/Wizard";
import type { Reading } from "../../cluster/reading";
import { useAuthSource } from "../../auth/context";
import {
  LEFT_OUT,
  downloadAccountExport,
  exportIsActive,
  exportProgress,
  exportStateWord,
  failureSentence,
  refusalOf,
  type ExportRow,
} from "./exports";
import { rowsPhrase, type Inventory } from "./inventory";
import { useExportCalls } from "./useAccounts";

// "EXPORT THIS ACCOUNT": the kit wizard over one export (design record
// 2026-11-12-account-export, section K), the orb a box.
//
//   Contents     kinds, rows and file bytes from the inventory the Data tab
//                reads (the page holds one reading for both), and what a
//                file leaves out
//   Protection   one switch, on. Switching it off asks in the bar first.
//   Passphrase   only when protected. Prepare is asked on the way in; the
//                passphrase is shown once, with Copy; Back is held, and Start
//                export is absent until "I have saved it" is ticked.
//   Export       the cluster's turn: state, phase and count and the elapsed
//                time in the bar's meta. Leave keeps it running; Cancel asks
//                first, by name.
//   Download     the file's name, size and checksum, what the report flags,
//                and Download.
//
// ===========================================================================
// THE PASSPHRASE LIVES IN THIS COMPONENT'S STATE, FOR ONE STEP
// ===========================================================================
// It arrives on the one answer to `accountExportPrepare` and is never asked
// for again. It is held in `useState` only, never in storage of any kind,
// never logged, never in a URL, and set to "" the moment the person leaves
// the step forward (Start export) or the wizard altogether. Leaving the step
// any other way is not offered: Back and the trail are held while it shows,
// because a passphrase gone from under somebody who had not copied it is a
// file nobody can open.
//
// A wizard REOPENED on an active export starts at Export: a run that is going
// has no Contents or Protection left to answer, and its passphrase is gone.

export interface ExportWizardProps {
  accountId: string;
  accountName: string;
  /** The inventory the Data tab reads: one reading, held by the page. */
  inventory: Reading<Inventory>;
  /** An export to follow from where it stands; absent to start a new one. */
  existing?: ExportRow;
  breadcrumbs?: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  /** An export's newest answer, for the list behind the wizard. */
  onChanged: (x: ExportRow) => void;
  onClose: () => void;
}

type StepId = "contents" | "protection" | "passphrase" | "export" | "download";

const NAMES: Record<StepId, string> = {
  contents: "Contents",
  protection: "Protection",
  passphrase: "Passphrase",
  export: "Export",
  download: "Download",
};
const SUBTITLES: Record<StepId, string> = {
  contents: "What the file holds",
  protection: "Whether it needs a passphrase",
  passphrase: "Shown once, to keep",
  export: "The cluster writes the file",
  download: "Save the file",
};

/** How often the wizard asks how the run is going. */
export const STATUS_EVERY_MS = 2_000;

/** "0:42", "12:05": the measure of a wait. */
function elapsed(fromIso: string, now: number): string {
  const from = Date.parse(fromIso);
  if (!Number.isFinite(from)) return "";
  const s = Math.max(0, Math.floor((now - from) / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

type Question = "" | "unprotect" | "cancelDraft" | "cancelRun";

const SHOWN_ONCE = "Its passphrase was shown once, when the export was prepared, and is not shown again.";

export function ExportWizard({ accountId, accountName, inventory, existing, breadcrumbs, back, onChanged, onClose }: ExportWizardProps) {
  const calls = useExportCalls();
  const auth = useAuthSource();
  const [step, setStep] = useState<StepId>(() => (existing === undefined ? "contents" : existing.state === "complete" ? "download" : "export"));
  const [protect, setProtect] = useState(existing?.protected ?? true);
  const [question, setQuestion] = useState<Question>("");
  const [x, setX] = useState<ExportRow | null>(existing ?? null);
  // THE ONE COPY. See the header: one step, then "".
  const [passphrase, setPassphrase] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState<"" | "prepare" | "start" | "cancel" | "download">("");
  // The act a refusal answered decides what the floor offers next.
  const [refusal, setRefusal] = useState<{ act: "prepare" | "start" | "cancel" | "download"; sentence: string; detail: string } | null>(null);
  // THE DRAFT WENT BEFORE IT WAS STARTED. The engine removes a prepared export
  // after fifteen minutes and then refuses Start with `export_draft_expired`:
  // its passphrase is void, so Try again could never work. The way on is a new
  // export with a new passphrase, and the way out is always there.
  const [expired, setExpired] = useState(false);
  // A wizard opened on an export that already existed. Its passphrase, if it
  // had one, was shown when it was prepared and is never shown again.
  const reopened = existing !== undefined;
  const now = useNow(1_000).getTime();

  // Leaving the wizard in any way drops whatever it held.
  const close = useCallback(() => {
    setPassphrase("");
    onClose();
  }, [onClose]);
  useEffect(() => () => setPassphrase(""), []);

  const land = useCallback((next: ExportRow) => {
    setX(next);
    onChanged(next);
    if (next.state === "complete") setStep("download");
  }, [onChanged]);

  // THE RUN, FOLLOWED. Asked once on arriving at Export and then on a short
  // cadence while the cluster still has it; a finished run is asked nothing.
  const following = step === "export" && x !== null && exportIsActive(x) && x.state !== "draft";
  const asking = useRef(false);
  const readStatus = useCallback(async () => {
    if (calls === null || x === null || asking.current) return;
    asking.current = true;
    try {
      land(await calls.status(x.id));
    } catch {
      // The next tick asks again; the bar keeps the last answer.
    } finally {
      asking.current = false;
    }
  }, [calls, x, land]);
  useEffect(() => {
    if (following) void readStatus();
    // Once per arrival at the step; the cadence below does the rest.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [following, x?.id]);

  async function prepare(): Promise<ExportRow | null> {
    if (calls === null) return null;
    setBusy("prepare");
    setRefusal(null);
    try {
      const answer = await calls.prepare(accountId, protect);
      setX(answer.export);
      onChanged(answer.export);
      if (protect) setPassphrase(answer.passphrase);
      return answer.export;
    } catch (err: unknown) {
      setRefusal({ act: "prepare", ...refusalOf(err) });
      return null;
    } finally {
      setBusy("");
    }
  }

  async function start(draft: ExportRow) {
    if (calls === null) return;
    setBusy("start");
    setRefusal(null);
    try {
      const started = await calls.start(draft.id);
      // LEFT FORWARD: the passphrase goes now.
      setPassphrase("");
      setSaved(false);
      setX(started);
      onChanged(started);
      setStep("export");
    } catch (err: unknown) {
      const r = refusalOf(err);
      if (r.code === "export_draft_expired") {
        // Only this code: any other refusal keeps the engine's words and Try again.
        setPassphrase("");
        setSaved(false);
        setExpired(true);
      } else {
        setRefusal({ act: "start", ...r });
      }
    } finally {
      setBusy("");
    }
  }

  /** A new export, from Protection, with a new passphrase. */
  function prepareAgain() {
    setExpired(false);
    setRefusal(null);
    setSaved(false);
    setPassphrase("");
    setX(null);
    setStep("protection");
  }

  async function cancel() {
    if (calls === null || x === null) return;
    setBusy("cancel");
    setRefusal(null);
    try {
      const cancelled = await calls.cancel(x.id);
      setPassphrase("");
      setQuestion("");
      setX(cancelled);
      onChanged(cancelled);
      setStep("export");
    } catch (err: unknown) {
      // The question is withdrawn and the refusal said; the step's own way
      // out stays, so a refused cancel is never a dead end.
      setQuestion("");
      setRefusal({ act: "cancel", ...refusalOf(err) });
    } finally {
      setBusy("");
    }
  }

  async function download() {
    if (x === null) return;
    setBusy("download");
    setRefusal(null);
    try {
      await downloadAccountExport(x, () => auth.bearer());
    } catch (err: unknown) {
      setRefusal({ act: "download", sentence: "The file was not downloaded.", detail: refusalOf(err).detail });
    } finally {
      setBusy("");
    }
  }

  // ---- what each step holds --------------------------------------------------

  const order: StepId[] = protect ? ["contents", "protection", "passphrase", "export", "download"] : ["contents", "protection", "export", "download"];
  const showingPassphrase = step === "passphrase" && passphrase !== "";
  const prepared = x !== null;

  const bodies: Record<StepId, ReactNode> = {
    contents: <Contents inventory={inventory} />,
    protection: (
      <div className="os-app-stack">
        <Switch
          checked={protect}
          onChange={(next) => {
            if (prepared) return;
            if (next) {
              setProtect(true);
              setQuestion("");
            } else {
              setQuestion("unprotect");
            }
          }}
        >
          Protect the file with a passphrase
        </Switch>
        <Caption>
          {protect
            ? "The file is encrypted. Its passphrase is shown once, on the next step."
            : "The file is not encrypted: anyone who has it can read everything in it."}
        </Caption>
      </div>
    ),
    passphrase: passphrase === "" ? null : (
      <div className="os-app-stack">
        <Notice tone="warn" sentence="Copy the passphrase now. This is the only time it is shown." next="Without it, the file cannot be opened.">
          <CopyValue value={passphrase} label="passphrase" displayValue={<span className="os-mono">{passphrase}</span>} />
        </Notice>
        <Check checked={saved} onChange={setSaved}>I have saved it</Check>
        <Caption>Start the export within fifteen minutes: a prepared export that is not started is removed then.</Caption>
      </div>
    ),
    export: x === null ? null : (
      <div className="os-app-stack">
        <RunFacts x={x} />
        {reopened && x.protected ? <Caption>{SHOWN_ONCE}</Caption> : null}
        {reopened && x.protected && x.state === "draft" ? (
          <>
            <Caption>Start it only if the passphrase was saved then; without it the file cannot be opened. Otherwise cancel it and prepare a new export.</Caption>
            <Check checked={saved} onChange={setSaved}>I have saved the passphrase</Check>
          </>
        ) : null}
      </div>
    ),
    download: x === null ? null : (
      <div className="os-app-stack">
        <DownloadFacts x={x} />
        {reopened && x.protected ? <Caption>{SHOWN_ONCE}</Caption> : null}
      </div>
    ),
  };

  const reached = (id: StepId): boolean => {
    // HELD while the passphrase shows, and once the export is under way:
    // nothing behind either can be changed any more.
    if (showingPassphrase || prepared) return id === step || (id === "download" && x?.state === "complete");
    return order.indexOf(id) <= order.indexOf(step);
  };
  const stateOf = (id: StepId): Stop["state"] => {
    if (id === step) return id === "export" && x !== null && exportIsActive(x) && x.state !== "draft" ? "current" : "open";
    if (order.indexOf(id) < order.indexOf(step)) return "done";
    return "ahead";
  };
  const steps: Stop[] = order.map((id) => ({
    id,
    name: NAMES[id],
    sentence: SUBTITLES[id],
    answer: id === "protection" && order.indexOf(step) > 1 ? (protect ? "With a passphrase" : "Without a passphrase") : undefined,
    state: stateOf(id),
    body: bodies[id],
  }));

  // ---- the floor ---------------------------------------------------------------

  const acts: Act[] = [];
  let status: { word: string; detail?: string; tone?: "busy" | "none"; meta?: string };
  let confirm: ReactNode = null;
  const cancelText: Act = { label: "Cancel", text: true, onAct: close };

  if (question === "unprotect") {
    status = { word: "Without a passphrase?" };
    confirm = <div className="os-actbar-confirm">Export without a passphrase? Anyone who has the file can read everything in it.</div>;
    acts.push(
      { label: "Keep protection", text: true, onAct: () => setQuestion("") },
      { label: "Export without one", tone: "primary", onAct: () => { setProtect(false); setQuestion(""); } },
    );
  } else if (question === "cancelDraft" || question === "cancelRun") {
    status = { word: "Cancel?" };
    confirm = (
      <div className="os-actbar-confirm">
        {question === "cancelRun"
          ? `Cancel the export of ${accountName}? What has been written so far is removed.`
          : `Cancel the export of ${accountName}? Its passphrase is discarded.`}
      </div>
    );
    acts.push(
      { label: question === "cancelRun" ? "Keep it running" : "Keep it", text: true, onAct: () => setQuestion("") },
      { label: "Cancel export", tone: "primary", busy: busy === "cancel", onAct: () => void cancel() },
    );
  } else if (expired) {
    status = { word: "Not started in time" };
    acts.push({ label: "Leave", text: true, onAct: close }, { label: "Prepare again", tone: "primary", onAct: prepareAgain });
  } else if (busy === "prepare") {
    status = { word: "Preparing the export", tone: "busy" };
  } else if (busy === "start") {
    status = { word: "Starting the export", tone: "busy" };
  } else if (step === "contents") {
    status = { word: "What the file holds" };
    acts.push(cancelText, { label: "Continue", tone: "primary", onAct: () => setStep("protection") });
  } else if (step === "protection") {
    status = refusal !== null ? { word: "Not prepared" } : { word: protect ? "With a passphrase" : "Without a passphrase" };
    acts.push(cancelText);
    if (refusal !== null) {
      acts.push({ label: "Try again", tone: "primary", onAct: () => void forward() });
    } else {
      acts.push({ label: protect ? "Continue" : "Start export", tone: "primary", onAct: () => void forward() });
    }
  } else if (step === "passphrase") {
    status = saved ? { word: "Ready to start" } : { word: "Save the passphrase", detail: "Start export comes once it is saved." };
    // A refused cancel adds the way out the held Back cannot be.
    if (refusal?.act === "cancel") acts.push({ label: "Leave", text: true, onAct: close });
    acts.push({ label: "Cancel", text: true, onAct: () => setQuestion("cancelDraft") });
    if (saved && x !== null) acts.push({ label: refusal?.act === "start" ? "Try again" : "Start export", tone: "primary", onAct: () => void start(x) });
  } else if (step === "export" && x !== null) {
    const stopped = x.state === "failed" || x.state === "cancelled";
    if (stopped) {
      status = { word: exportStateWord(x) };
      acts.push({ label: "Done", tone: "primary", onAct: close });
    } else if (x.state === "draft") {
      status = { word: "Prepared, not started", detail: "A prepared export is removed after fifteen minutes unless it is started." };
      // A protected draft starts only once the person says they saved its
      // passphrase when it was shown; an unprotected one starts directly.
      const startable = !x.protected || saved;
      acts.push({ label: "Leave", text: true, onAct: close }, { label: "Cancel", text: true, onAct: () => setQuestion("cancelDraft") });
      if (startable) acts.push({ label: refusal?.act === "start" ? "Try again" : "Start export", tone: "primary", onAct: () => void start(x) });
    } else {
      const measure = [exportProgress(x), elapsed(x.startedAt || x.requestedAt, now)].filter((s) => s !== "").join(" · ");
      status = { word: exportStateWord(x), tone: "busy", meta: measure };
      acts.push({ label: "Cancel", text: true, onAct: () => setQuestion("cancelRun") }, { label: "Leave", tone: "primary", onAct: close });
    }
  } else if (step === "download" && x !== null) {
    status = { word: "Complete" };
    acts.push({ label: "Done", text: true, onAct: close }, { label: "Download", tone: "primary", busy: busy === "download", onAct: () => void download() });
  } else {
    status = { word: "" };
    acts.push(cancelText);
  }

  async function forward() {
    const draft = x ?? (await prepare());
    if (draft === null) return;
    if (protect) setStep("passphrase");
    else await start(draft);
  }

  const failure = x !== null && x.state === "failed"
    ? <Notice tone="error" sentence={failureSentence(x.failureCode)} detail={x.failureDetail || undefined} />
    : null;
  const notices = (
    <>
      {failure}
      {expired ? (
        <Notice
          tone="warn"
          sentence="The prepared export was removed because it was not started in time."
          next="Its passphrase is void. Prepare again for a new export with a new passphrase."
        />
      ) : null}
      {refusal === null ? null : <Notice tone="error" sentence={refusal.sentence} detail={refusal.detail || undefined} />}
    </>
  );

  // BACK AND THE TRAIL ARE HELD while the passphrase shows: drawn, but going
  // nowhere, as the access-code panel holds its page's Back.
  const held = showingPassphrase || busy !== "";
  return (
    <>
      {following ? <AutoRefresh onRefresh={readStatus} intervalMs={STATUS_EVERY_MS} resume={false} /> : null}
      <Wizard
        icon={<Box size={20} aria-hidden />}
        title="Export this account"
        label="Exporting an account"
        breadcrumbs={held ? breadcrumbs?.map((c) => ({ label: c.label })) : breadcrumbs?.map((c) => (c.onSelect ? { ...c, onSelect: () => { setPassphrase(""); c.onSelect!(); } } : c))}
        back={held ? undefined : { label: back.label, onSelect: () => { setPassphrase(""); back.onSelect(); } }}
        steps={steps}
        open={step}
        onOpen={(id) => {
          if (!held && reached(id as StepId)) setStep(id as StepId);
        }}
        status={status}
        acts={acts}
        confirm={confirm}
        notices={notices}
      />
    </>
  );
}

function Contents({ inventory }: { inventory: Reading<Inventory> }) {
  let counts;
  if (inventory.state === "failed") {
    counts = <Notice tone="error" sentence="What this account holds was not read." detail={inventory.error} />;
  } else if (inventory.value === null) {
    counts = inventory.state === "unread"
      ? <Caption>What this account holds is not available here.</Caption>
      : <ContentSkeleton kind="detail" label="Loading what the account holds" />;
  } else {
    const kinds = inventory.value.concepts.filter((c) => c.rows > 0);
    const rows = kinds.reduce((sum, c) => sum + c.rows, 0);
    counts = (
      <Facts>
        <Fact label="Kinds" value={kinds.length === 1 ? "1 kind" : `${kinds.length.toLocaleString("en-US")} kinds`} />
        <Fact label="Rows" value={rowsPhrase(rows)} />
        <Fact label="File bytes" value={formatBytes(inventory.value.fileBytes)} />
      </Facts>
    );
  }
  return (
    <div className="os-app-stack">
      {counts}
      <Subhead>Left out</Subhead>
      <Facts>
        {LEFT_OUT.map((l) => <Fact key={l.what} label={l.what} value={l.why} />)}
      </Facts>
    </div>
  );
}

function RunFacts({ x }: { x: ExportRow }) {
  return (
    <Facts>
      <Fact label="State" value={exportStateWord(x)} />
      {exportIsActive(x) && x.state !== "draft" ? <Fact label="Progress" value={exportProgress(x)} /> : null}
      <Fact label="Protected" value={x.protected ? "Yes" : "No"} />
    </Facts>
  );
}

function DownloadFacts({ x }: { x: ExportRow }) {
  const flags: string[] = [];
  const r = x.report;
  if (r !== null) {
    if (r.missingObjects > 0) {
      flags.push(r.missingObjects === 1
        ? "1 file was missing at the source and is not in the file."
        : `${r.missingObjects.toLocaleString("en-US")} files were missing at the source and are not in the file.`);
    }
    if (r.notCarriedObjects > 0) {
      flags.push(`${r.notCarriedObjects.toLocaleString("en-US")} older or attached files were counted, not carried (${formatBytes(r.notCarriedBytes)}).`);
    }
    if (r.scrubbed.length > 0) flags.push(`Secret values removed: ${r.scrubbed.join(", ")}.`);
  }
  return (
    <div className="os-app-stack">
      <Facts>
        <Fact label="File" value={<CopyValue value={x.fileName} label="file name" displayValue={x.fileName} />} />
        <Fact label="Size" value={formatBytes(x.sizeBytes)} />
        <Fact label="Checksum" value={<CopyValue value={x.sha256} label="checksum" displayValue={<span className="os-mono">{x.sha256}</span>} />} />
        <Fact label="Protected" value={x.protected ? "Yes" : "No"} />
      </Facts>
      {flags.map((f) => <Caption key={f}>{f}</Caption>)}
    </div>
  );
}
