import { useEffect, useMemo, useRef, useState } from "react";
import { Concepts, type LiveSnapshot, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Check, Head, Panel } from "../../kit";
import { useAuthSource } from "../../auth/context";
import { useLiveView } from "../../live/liveView";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { EdgeUploadProvider } from "../../items/edgeUpload";
import type { UploadProvider } from "../../items/upload";
import { useChunkDecisions } from "./actions";
import { CHUNK_CONCEPT, FILE_CONCEPT, RUN_CONCEPT, type ChunkDecision } from "./concepts";
import { DomainsSection } from "./DomainsSection";
import { ReviewSection } from "./ReviewSection";
import {
  fileBelongsHere,
  fileFromRow,
  runBelongsHere,
  runFromRow,
  runsByFile,
  type AnalysisRun,
  type TrainingFile,
} from "./rows";
import {
  DEFAULT_TRAINING_SETTINGS,
  LocalTrainingSettingsStore,
  TRAINING_SECTIONS,
  type TrainingSettings,
  type TrainingSettingsStore,
} from "./settings";
import { UploadSection } from "./UploadSection";
import { useAnalysisRuns } from "./useAnalysisRuns";
import { useDomains } from "./useDomains";
import { useLibraryFiles } from "./useLibraryFiles";
import { useReviewQueue } from "./useReviewQueue";
import { useTrain } from "./useTrain";
import { useUploads } from "./useUploads";

// Training: teach MemQL from files (epic memql#4737, re-keyed to the Library
// in epic memql#4970).
//
// ===========================================================================
// THREE FEEDS, HELD HERE, READ BY THREE SECTIONS
// ===========================================================================
// The file feed and the run feed are live and the domain feed is not, and all
// three are retained at this root rather than inside the sections that read
// them.
//
// For the DOMAINS read that is the Deployables lesson applied: the Domains
// cards, the Review queue's walk and now the Teach picker are three readings
// of one answer, and a second `useDomains()` inside any of them would be free
// to disagree with the others about which domains this cluster holds.
//
// For the two LIVE feeds it is also what makes switching sections free: the
// collections stay retained for the life of the window rather than re-seeding
// every time somebody looks at the queue and comes back.
//
// WHY THE FILE FEED AND THE RUN FEED ARE TWO, rather than one read that joins
// them: they are written by different things at different times. The upload
// route writes the file row synchronously, inside the request; the analysis
// pass writes the run from a detached goroutine on whichever node took the
// upload. A surface that waited for both would show nothing for the first
// moments of every upload, so the FILE leads and the run decorates
// (`stageOf`, rows.ts).
//
// Sections are the app's own navigation. It never opens a window.

const EMPTY_SNAPSHOT: LiveSnapshot<TrainingFile> = {
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
};

/** The concepts this app owns, for its Logs section: the files it teaches
 *  from, the runs that read them, the knowledge it feeds and the chunks it
 *  reviews. `v1:planner:plan` is gone from this list because it is gone from
 *  this app -- nothing here names a plan or a space any more. */
const TRAINING_LOG_CONCEPTS = [
  FILE_CONCEPT,
  RUN_CONCEPT,
  Concepts.KNOWLEDGE_KNOWLEDGE_DOMAIN,
  CHUNK_CONCEPT,
] as const;

export function TrainingApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
  store,
  uploads: injectedUploads,
}: OsAppProps & {
  store?: TrainingSettingsStore;
  uploads?: UploadProvider;
}) {
  // Injectable for tests, which is the whole reason the parameters exist --
  // nothing in the shell passes either.
  const settingsStore = useMemo(() => store ?? new LocalTrainingSettingsStore(), [store]);
  const [settings, setSettings] = useState<TrainingSettings>(() => settingsStore.load());

  const authSource = useAuthSource();

  // THE LIBRARY'S OWN PROVIDER, not a second one. Every upload in the OS
  // rides `EdgeUploadProvider` (the desk's drops, the Files browse,
  // drop-onto-window), and this surface now does too -- which is where the
  // chunked resumable session, the per-chunk retry and the byte progress come
  // from. The space attachment provider this replaced had none of them and
  // capped at 25 MB.
  const provider = useMemo<UploadProvider>(
    () => injectedUploads ?? new EdgeUploadProvider(() => authSource.bearer()),
    [injectedUploads, authSource],
  );

  const uploads = useUploads(provider);

  const { source: fileCollection, reseed } = useLibraryFiles();
  const { source: runCollection } = useAnalysisRuns();

  // PROJECT, THEN NARROW, IN ONE PASS. A collection holds RAW wire rows --
  // its fold upserts an arriving event's payload AS the row type with no
  // projection hook -- so every predicate has to run on a projected result.
  //
  // THE VIEW KEYS ARE CONSTANTS NOW, and that is the re-key's quiet security
  // gain rather than a simplification. The plan feed's key carried the viewer
  // id because its transform had to filter other people's rows out
  // client-side: `v1:planner:plan` declares no row-authz tier, and a concept
  // that declares nothing admits every subscriber. `v1:library:file` and
  // `v1:work:run` both declare the composite owner tier, so admission runs on
  // the SUBSCRIPTION as well as the read (memql#4309) and nobody else's rows
  // arrive at all. There is no residual left to filter, so nothing about the
  // transform changes when access resolves.
  const files = useLiveView<Row, TrainingFile>(fileCollection, "files", (rows) =>
    rows.map(fileFromRow).filter(fileBelongsHere),
  );
  const snapshot = files?.snapshot ?? EMPTY_SNAPSHOT;

  const runs = useLiveView<Row, AnalysisRun>(runCollection, "runs", (rows) =>
    rows.map(runFromRow).filter(runBelongsHere),
  );
  const runRows = runs?.snapshot.rows ?? [];
  // NEWEST RUN PER FILE. A re-analysis is a second run of the same goal, so a
  // file legitimately has several, and the one that describes it now is the
  // newest -- which is the first, because the feed is newest-first.
  const runsByFileId = useMemo(() => runsByFile(runRows), [runRows]);

  const domains = useDomains();
  // Only the domains that actually hold work. This is what the
  // `validationStatus` key added to `documentChunkDomainLite` buys: without it
  // the queue would have to page every domain to find out which had any.
  const domainsWithWork = useMemo(
    () => domains.rollups.filter((r) => r.unvalidated > 0).map((r) => r.domainId),
    [domains.rollups],
  );
  // HELD AT THE ROOT, WHICH MEANS THE WALK STARTS BEFORE ANYBODY OPENS THE
  // QUEUE. That is a deliberate trade rather than an oversight.
  //
  // The queue holds DECISION state -- which chunks were decided, and the
  // cards collapsed in place to record it. Mounted inside `ReviewSection`, a
  // trip to Domains and back would discard the loaded pages and every one of
  // those collapsed cards, re-walk from page one, and offer the same chunks
  // again as though nothing had been decided. Losing a record of work
  // somebody just did is worse than an early read.
  //
  // The read it costs is bounded: on a cluster with nothing unvalidated
  // `domainsWithWork` is empty and this walks nothing at all, and where there
  // IS work one step reads at most PAGES_PER_STEP pages -- which is the work
  // the person opened this app to do.
  const queue = useReviewQueue(domainsWithWork);
  const decisions = useChunkDecisions();
  const train = useTrain();

  async function decide(chunkId: string, status: ChunkDecision) {
    // The card updates FROM THE REPLY, never optimistically. `v1:knowledge:*`
    // carries no broadcast routing, so nothing would correct an optimistic
    // flip the engine had in fact refused -- the card would read as approved
    // forever while the chunk stayed out of retrieval.
    const accepted = await decisions.decide(chunkId, status);
    if (accepted) queue.applyDecision(chunkId, status);
  }

  function update(patch: Partial<TrainingSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- Fleet's
  // pattern, and its reasoning holds unchanged. The shell opens an app on its
  // manifest's FIRST section, so an app-level "open me here" can only be the
  // app navigating itself on the first render of this component instance.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on a
    // named section was opened by somebody who said where they wanted to be --
    // the Settings apps index deep-linking to this app's own settings, say --
    // and a preference that overrode that would make the deep link silently
    // not work (memql#4743).
    const shellDefault = TRAINING_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW. Re-running on a section change
    // would drag somebody back to their default the moment they navigated
    // away.
  }, []);

  // AUTO-OPEN REVIEW, off by default and driven by a TRAINED transition.
  //
  // RE-KEYED, and the trigger moved for a reason rather than a rename. It
  // used to fire when an analysis plan reached `succeeded`, which under the
  // Library route means only that a file was read -- and a file that has been
  // read has taught nothing yet, so the queue it opened would be empty. The
  // act that puts chunks in the queue is teaching a domain, so that is what
  // this watches: a file id arriving in the trained set that was not in it
  // before.
  //
  // Not a count, and not "the newest file is trained". A count would fire
  // again on a resync, and reading the newest row would fire on every render
  // for a file taught an hour ago.
  //
  // A SNAPSHOT THAT FOLLOWS A NON-LIVE ONE IS A BASELINE, which is
  // `arrival.ts`'s rule applied to a second consumer and for the same reason.
  // Keying the baseline on "the first time this effect ran" is what does NOT
  // work: the effect runs once on mount with an EMPTY snapshot, so the seed
  // that lands a moment later looks like every file in it was just taught --
  // opening the window on a history of trained files would bounce somebody
  // straight to the queue.
  const trained = useRef<{ ids: Set<string>; wasLive: boolean }>({
    ids: new Set(),
    wasLive: false,
  });
  useEffect(() => {
    const taught = new Set(
      snapshot.rows.filter((f) => f.trainedIntoDomainIds.length > 0).map((f) => f.id),
    );
    const held = trained.current;
    trained.current = { ids: taught, wasLive: snapshot.state === "live" };
    if (!held.wasLive) return;
    if (!settings.autoOpenReview) return;
    for (const id of taught) {
      if (!held.ids.has(id)) {
        navigate("review");
        return;
      }
    }
  }, [snapshot.rows, snapshot.state, settings.autoOpenReview, navigate]);

  // THE DROPZONE HANDS OFF TO THE LIST when the file it produced arrives.
  //
  // The upload entry is retired by the ROW APPEARING rather than by the 201,
  // which is the stronger handover: on a 201 the file row is on its way and
  // this browser has nothing to show, so an entry that vanished there would
  // leave a gap. Nothing is inserted locally either way -- the row arrives
  // with the arrival cue, exactly like one raised by an upload from
  // somebody's phone.
  const arrivedFileIds = useMemo(
    () => new Set(snapshot.rows.map((f) => f.id)),
    [snapshot.rows],
  );
  useEffect(() => {
    uploads.settle(arrivedFileIds);
  }, [arrivedFileIds, uploads]);

  if (sectionId === "settings") {
    return <TrainingSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="training"
        subjectConcepts={TRAINING_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  if (sectionId === "review") {
    return (
      <ReviewSection
        queue={queue}
        decisions={decisions}
        onDecide={(chunkId, status) => void decide(chunkId, status)}
        domainsError={domains.error}
      />
    );
  }
  if (sectionId === "domains") {
    return <DomainsSection feed={domains} onNavigate={navigate} />;
  }
  return (
    <UploadSection
      uploads={uploads}
      files={files}
      snapshot={snapshot}
      runsByFileId={runsByFileId}
      domains={domains}
      train={train}
      onReseed={reseed}
      onAsk={askContext}
    />
  );
}

function TrainingSettingsSection({
  settings,
  update,
}: {
  settings: TrainingSettings;
  update: (patch: Partial<TrainingSettings>) => void;
}) {
  // EVERY SECTION IS OFFERED. Unlike Deployables and Users, no section of this
  // app carries a role of its own -- the app-level `writer` gate is the only
  // one -- so there is no section a reader of this settings page could pick
  // and then not be admitted to.
  return (
    <div className="os-settings">
      <Head title="Training settings" />
      <Panel label="Training settings">
        <fieldset className="os-field-group">
          <legend>Open Training on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {TRAINING_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Teach is the default because this app is for teaching MemQL from files. Applies the
            next time a Training window opens; it does not move the window you are looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>After an analysis</legend>
          <Check
            checked={settings.autoOpenReview}
            onChange={(next) => update({ autoOpenReview: next })}
          >
            Open the review queue when an analysis succeeds
          </Check>
          <p className="os-caption">
            Off by default: an analysis finishing is not a reason to move you out of the dropzone
            mid-batch. It never fires on a failure -- there is nothing to review, and moving you
            would hide the error.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser, separately from your desktop, so an app learning a
          preference can never cost you your desks. The defaults are{" "}
          {DEFAULT_TRAINING_SETTINGS.defaultSection}, with auto-open off.
        </p>
      </Panel>
    </div>
  );
}
