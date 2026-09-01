import { useEffect, useMemo, useRef, useState } from "react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import { Check, Head, Panel } from "../../kit";
import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { useLiveView } from "../../live/liveView";
import type { OsAppProps } from "../../system/registry";
import { useChunkDecisions } from "./actions";
import {
  EdgeAttachmentUploadProvider,
  type AttachmentUploadProvider,
} from "./attachmentUpload";
import type { ChunkDecision } from "./concepts";
import { DomainsSection } from "./DomainsSection";
import { ReviewSection } from "./ReviewSection";
import { planBelongsHere, planFromRow, type AnalysisPlan } from "./rows";
import {
  DEFAULT_TRAINING_SETTINGS,
  LocalTrainingSettingsStore,
  TRAINING_SECTIONS,
  type TrainingSettings,
  type TrainingSettingsStore,
} from "./settings";
import { UploadSection } from "./UploadSection";
import { useActiveSpace } from "./useActiveSpace";
import { useAnalysisPlans } from "./useAnalysisPlans";
import { useDomains } from "./useDomains";
import { useReviewQueue } from "./useReviewQueue";
import { useUploads } from "./useUploads";

// Training: teach MemQL from files (epic memql#4737).
//
// ===========================================================================
// TWO FEEDS, HELD HERE, READ BY THREE SECTIONS
// ===========================================================================
// The plan feed is live and the domain feed is not, and both are retained at
// this root rather than inside the sections that read them.
//
// For the DOMAINS read that is the Deployables lesson applied: the Domains
// cards and the Review queue's walk are two readings of one answer, and a
// second `useDomains()` inside the queue would be free to disagree with the
// list beside it about which domains this cluster holds.
//
// For the PLAN feed it is also what makes switching sections free: the
// collection stays retained for the life of the window rather than re-seeding
// every time somebody looks at the queue and comes back.
//
// Sections are the app's own navigation. It never opens a window.

const EMPTY_SNAPSHOT: LiveSnapshot<AnalysisPlan> = {
  rows: [],
  state: "disconnected",
  error: "",
  version: 0,
};

export function TrainingApp({
  sectionId,
  navigate,
  askContext,
  store,
  uploads: injectedUploads,
}: OsAppProps & {
  store?: TrainingSettingsStore;
  uploads?: AttachmentUploadProvider;
}) {
  // Injectable for tests, which is the whole reason the parameters exist --
  // nothing in the shell passes either.
  const settingsStore = useMemo(() => store ?? new LocalTrainingSettingsStore(), [store]);
  const [settings, setSettings] = useState<TrainingSettings>(() => settingsStore.load());

  const { access } = useSession();
  const viewerUserId = access?.userId ?? "";
  const authSource = useAuthSource();

  const provider = useMemo<AttachmentUploadProvider>(
    () => injectedUploads ?? new EdgeAttachmentUploadProvider(() => authSource.bearer()),
    [injectedUploads, authSource],
  );

  const space = useActiveSpace(viewerUserId);
  const uploads = useUploads(provider);

  const { source: collection, reseed } = useAnalysisPlans();

  // PROJECT, THEN NARROW, IN ONE PASS. The collection holds RAW wire rows --
  // its fold upserts an arriving event's payload AS the row type with no
  // projection hook -- so every predicate has to run on a `planFromRow`
  // result.
  //
  // The view KEY carries the viewer id, and that is deliberate even though a
  // key change re-baselines the arrival cue: `planBelongsHere` refuses a blank
  // viewer id outright, so the transform genuinely MEANS something different
  // once access resolves, and a view that did not re-run would show an empty
  // list forever. It is the transform's identity, not the collection's -- the
  // subscription and the seed are untouched, so nothing is re-read.
  const plans = useLiveView<Row, AnalysisPlan>(collection, `plans:${viewerUserId}`, (rows) =>
    rows.map(planFromRow).filter((plan) => planBelongsHere(plan, viewerUserId)),
  );
  const snapshot = plans?.snapshot ?? EMPTY_SNAPSHOT;

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

  // AUTO-OPEN REVIEW, off by default and driven by a SUCCEEDED transition.
  //
  // The trigger is a plan id arriving in the succeeded set that was not in it
  // before -- not a count, and not "the newest plan is succeeded". A count
  // would fire again on a resync, and reading the newest row would fire on
  // every render for a plan that finished an hour ago.
  //
  // It fires only on `succeeded`: there is nothing to review after a failure,
  // and navigating away from the error message would hide the only account of
  // what happened.
  const succeeded = useRef<Set<string> | null>(null);
  useEffect(() => {
    const finished = new Set(
      snapshot.rows.filter((p) => p.status === "succeeded").map((p) => p.id),
    );
    const held = succeeded.current;
    succeeded.current = finished;
    // The FIRST observation is a baseline, exactly as the arrival cue treats
    // its first snapshot: everything already succeeded when this window opened
    // is history, not news.
    if (held === null) return;
    if (!settings.autoOpenReview) return;
    for (const id of finished) {
      if (!held.has(id)) {
        navigate("review");
        return;
      }
    }
  }, [snapshot.rows, settings.autoOpenReview, navigate]);

  if (sectionId === "settings") {
    return <TrainingSettingsSection settings={settings} update={update} />;
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
      space={space}
      uploads={uploads}
      source={plans}
      snapshot={snapshot}
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
            Upload is the default because this app is for teaching MemQL from files. Applies the
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
