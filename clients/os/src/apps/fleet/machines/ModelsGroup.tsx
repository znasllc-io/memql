import { useMemo, useState, type FormEvent } from "react";

import { useSession } from "../../../chrome/access";
import { Measure } from "../../../kit/MeasureView";
import { Button, Notice, Subhead } from "../../../kit";
import { formatBytes, formatMoment } from "../../../kit/format";
import { formatContext, formatParams } from "../models/ordering";
import { machineName, type MachineRow } from "../rows";
import {
  machineModelsFrom,
  machineRuntimesFrom,
  pullProgressFraction,
  pullStepLabel,
  type MachineModel,
  type ModelPull,
} from "./models";
import {
  formatRate,
  formatTps,
  levelLabel,
  type Measurement,
  type Recommendation,
  type RecommendedSet,
} from "./recommended";
import { runtimeLabel } from "./hardware";
import { AskIt } from "../addMachine/AskIt";
import { useMachineInference } from "./useMachineInference";
import { useModelPulls } from "./useModelPulls";

// The Models group on a machine's detail: what this machine runs, and the act
// that puts another model on it (epic memql#5103, design D5).
//
// ===========================================================================
// THE ACT BELONGS TO ITS SUBJECT
// ===========================================================================
// Pull lives HERE and not on the Fleet's Models section, though that section
// lists models too. A pull names ONE machine -- it writes gigabytes to one
// disk and edits one policy file -- so an act offered above a fleet-wide list
// would have to ask "onto which machine", which is the question this page's
// existence already answers.
//
// ===========================================================================
// WHAT IT SHOWS, AND THE ONE NUMBER IT REFUSES TO SHOW
// ===========================================================================
// A runtime pulls a model as a set of blobs and counts each from zero, and
// states no whole-pull total anywhere. So there is a bar for the CURRENT STEP,
// labelled as the current step, and no overall percentage -- which would be a
// number this page invented, wrong in a way nobody could check. When even the
// step's size is unstated the bar is ABSENT rather than empty: an empty track
// says "nothing has moved", and what actually happened is that nobody said how
// far there is to go.
//
// The runtime's own status line carries the rest, verbatim. It is what tells a
// person where a pull has got to, and it is not parsed into a phase vocabulary
// this side would have to keep in step with somebody else's release.

export function ModelsGroup({ machine }: { machine: MachineRow }) {
  const { access } = useSession();
  const { pulls, live, loading, feedError, start, starting } = useModelPulls(machine.id);
  const inference = useMachineInference(machine.id);

  const models = useMemo(() => machineModelsFrom(machine.reportedLabels), [machine.reportedLabels]);
  const chatModel = models.find((model) => !model.embeddings);
  // THE INVENTORY WINS OVER THE LABELS, and the screenshot is what showed why.
  // Since epic memql#5146 a machine reports its runtimes in two places -- the
  // hardware inventory, with versions, and the `runtime:` labels derived from
  // it -- and a machine whose cockpit reported an inventory but whose labels
  // had not yet been re-derived rendered "Served by a runtime this machine did
  // not name" directly beneath a Hardware group listing Ollama and Kokoro.
  // Two readings of one fact, disagreeing on the same screen.
  const runtimes = useMemo(() => {
    const reported = machine.hardware.runtimes.map((r) => runtimeLabel(r.name));
    return reported.length > 0 ? reported : machineRuntimesFrom(machine.reportedLabels);
  }, [machine.hardware, machine.reportedLabels]);

  // OWNER ONLY, and ABSENT rather than disabled for anybody else -- rule 12's
  // reading, which the engine enforces independently: fleetModelPull refuses a
  // machine that is not the caller's. A disabled control would advertise an
  // act nobody on this page can reach.
  const isOwner =
    access !== null && access.userId !== "" && sameSubject(access.userId, machine.ownerUserId);

  return (
    <div className="os-fleet-machinemodels">
      <Subhead>Models</Subhead>

      <RuntimeLine runtimes={runtimes} modelCount={models.length} />

      <RecommendedBlock
        set={inference.recommended}
        isOwner={isOwner}
        busy={inference.pullingSet}
        blocked={live !== null}
        onPull={inference.pullRecommended}
      />

      {/* THE ROUND TRIP (design record 2026-09-08-cockpit-install-wizard,
          D14): a machine serving a model is proved by using it, and this is
          the one act on the page that does. Targeted calls are owner-only;
          embedding models cannot answer the chat check. */}
      {isOwner && chatModel ? <AskIt modelId={chatModel.modelId} registrationId={machine.id} machineLabel={machineName(machine)} /> : null}

      {models.length > 0 ? (
        <ul className="os-fleet-machinemodel-list">
          {models.map((model) => (
            <ModelRow
              key={model.modelId}
              model={model}
              measurement={inference.measurements.get(model.modelId) ?? null}
              isOwner={isOwner}
              probing={inference.probing === model.modelId}
              onProbe={inference.probe}
            />
          ))}
        </ul>
      ) : null}

      {inference.error ? (
        <Notice
          tone="warn"
          sentence="The recommended set and the measured figures could not be read."
          next="The models above are still what this machine reports."
          detail={inference.error}
        />
      ) : null}

      {feedError ? (
        <Notice
          tone="error"
          sentence="This machine's pull history could not be read."
          next="The models above are still what it reports."
          detail={feedError}
        />
      ) : null}

      {live ? <LivePull pull={live} /> : null}

      {isOwner ? (
        <PullForm
          machineLabel={machineName(machine)}
          busy={starting}
          blocked={live !== null}
          onStart={start}
        />
      ) : (
        <p className="os-caption">
          Only this machine's owner can pull a model onto it.
        </p>
      )}

      <PullHistory pulls={pulls} live={live} loading={loading} />
    </div>
  );
}

/**
 * The runtime sentence.
 *
 * THREE STATES, NOT TWO, because "no runtime" and "a runtime serving nothing"
 * have different fixes and an empty models list collapses them into one blank.
 * The second is the state the whole pull feature exists for, so it is the one
 * that says what to do next.
 */
function RuntimeLine({ runtimes, modelCount }: { runtimes: string[]; modelCount: number }) {
  if (runtimes.length === 0 && modelCount === 0) {
    return (
      <p className="os-caption">
        No model runtime on this machine. Run <code className="os-mono">memql worker setup
        --inference</code> on it to install one -- or tick "will run local models" when you add a
        machine and the installer does it in the same terminal.
      </p>
    );
  }
  if (modelCount === 0) {
    return (
      <p className="os-caption">
        Running {runtimes.join(", ")} with no models yet. Pull one below and this machine starts
        serving it.
      </p>
    );
  }
  return (
    <p className="os-caption">
      Served by {runtimes.length === 0 ? "a runtime this machine did not name" : runtimes.join(", ")}.
      Only models the machine allows are advertised, so this list is what the cluster can actually
      route to.
    </p>
  );
}

/**
 * One model: what it says about itself, then what a probe found it does.
 *
 * SIZE, QUANTIZATION AND WINDOW READ AS FACTS, and an unreported one is
 * ABSENT rather than zero: a model that never said how big it is is not a
 * zero-parameter model, and printing 0 would make the unmeasured one look like
 * the smallest.
 *
 * THE MEASURED LINE IS THE SAME RULE ONE LEVEL ALONG, and it is the one the
 * whole probe exists for. A machine nobody has probed shows an em dash with
 * its reason on hover, never "0%" and never "0 tok/s" -- those read exactly
 * like a model that failed every case, and the two lead to opposite actions:
 * go and probe it, against do not route structured work here.
 */
function ModelRow({
  model,
  measurement,
  isOwner,
  probing,
  onProbe,
}: {
  model: MachineModel;
  measurement: Measurement | null;
  isOwner: boolean;
  probing: boolean;
  onProbe: (modelId: string) => Promise<string>;
}) {
  const [refusal, setRefusal] = useState("");
  const size = formatParams(model.params);
  const window = formatContext(model.contextWindow);
  const facts = [size, model.quant, window ? `${window} context` : ""].filter((f) => f !== "");
  const can = [
    model.structuredOutput ? "structured output" : "",
    model.tools ? "tool calling" : "",
    model.embeddings ? "embeddings" : "",
  ].filter((c) => c !== "");

  return (
    <li className="os-fleet-machinemodel">
      <span className="os-fleet-machinemodel-id os-mono">{model.modelId}</span>
      <span className="os-fleet-machinemodel-readings">
        <span className="os-fleet-machinemodel-facts">
          {facts.length > 0 ? facts.join(" \u00b7 ") : "size not reported"}
        </span>
        <span>{can.length > 0 ? can.join(" \u00b7 ") : "no capabilities advertised"}</span>
        <MeasuredLine measurement={measurement} />
      </span>
      {isOwner ? (
        <span className="os-fleet-machinemodel-act">
          <Button
            busy={probing}
            busyLabel="Measuring..."
            onClick={() => {
              setRefusal("");
              void onProbe(model.modelId).then(setRefusal);
            }}
          >
            Probe this model
          </Button>
        </span>
      ) : null}
      {refusal ? (
        <Notice
          tone="error"
          sentence="The probe did not start."
          next="Nothing ran on the machine."
          detail={refusal}
        />
      ) : null}
    </li>
  );
}

/**
 * What a probe found.
 *
 * THREE FIGURES AND NOT FOUR. Time to first token is measured and is not here:
 * it is a latency a person cannot act on from this page, where validity says
 * whether to route structured work at all and throughput says how long a turn
 * takes. A fourth number would spend a reader's attention without changing a
 * decision.
 *
 * The suite version rides the line, because two figures scored by different
 * suites are not comparable and a number with no provenance invites the
 * comparison.
 */
function MeasuredLine({ measurement }: { measurement: Measurement | null }) {
  if (measurement === null) {
    return (
      <span className="os-fleet-measured" data-unmeasured="true">
        Not measured on this machine yet.
      </span>
    );
  }
  return (
    <span className="os-fleet-measured">
      <span className="os-fleet-measured-pair">
        <span className="os-fleet-measured-label">Valid</span>
        <Measure figure={measurement.structuredValidity} format={formatRate} />
      </span>
      <span className="os-fleet-measured-pair">
        <span className="os-fleet-measured-label">Tools</span>
        <Measure figure={measurement.toolCallCorrectness} format={formatRate} />
      </span>
      <span className="os-fleet-measured-pair">
        <span className="os-fleet-measured-label">Speed</span>
        <Measure figure={measurement.throughputTps} format={formatTps} suffix=" tok/s" />
      </span>
      <span
        className="os-fleet-measured-when"
        title={`Suite ${measurement.suiteVersion}, measured ${measurement.measuredAt}`}
      >
        suite {measurement.suiteVersion}
      </span>
      {measurement.probeError ? (
        <span className="os-fleet-measured-partial">{measurement.probeError}</span>
      ) : null}
    </span>
  );
}

/**
 * What the catalog recommends for THIS machine, and one act that takes it.
 *
 * ===========================================================================
 * ONE ACT, NOT A BUTTON PER MODEL
 * ===========================================================================
 * Somebody who has just paired a Mac Studio should not have to decide which
 * four models to pull, in which order, and then press four buttons. The
 * catalog knows which models suit a machine of this class and the engine knows
 * the class; the whole of what is left for a person to say is yes.
 *
 * A BLOCKED ENTRY STAYS ON SCREEN with the sentence saying why. Dropping it
 * would answer "why can my machine not do voice" with silence, which is
 * indistinguishable from a catalog that never had one.
 */
function RecommendedBlock({
  set,
  isOwner,
  busy,
  blocked,
  onPull,
}: {
  set: RecommendedSet;
  isOwner: boolean;
  busy: boolean;
  blocked: boolean;
  onPull: () => Promise<string>;
}) {
  const [refusal, setRefusal] = useState("");

  // A machine that has not reported has nothing to recommend AND nothing to
  // explain. The Hardware group above already says the cockpit has not spoken;
  // repeating it here would be the same sentence twice, and rule 7 is that a
  // thing is said once.
  if (!set.reported || set.entries.length === 0) return null;

  const pullableCount = new Set(set.entries.filter((e) => e.pullable).map((e) => e.modelId)).size;

  return (
    <div className="os-fleet-recommended">
      <p className="os-fleet-recommended-lead">
        For a {set.machineClass} GB machine, the catalog recommends:
      </p>

      <ul className="os-fleet-recommendedlist">
        {set.entries.map((entry) => (
          <RecommendedRow key={`${entry.level}:${entry.modelId}`} entry={entry} />
        ))}
      </ul>

      {set.runtimeGap.length > 0 ? (
        <p className="os-caption">
          {set.runtimeGap.length === 1
            ? `Installing the ${runtimeLabel(set.runtimeGap[0] ?? "")} runtime on this machine unblocks the entry above. `
            : `Installing ${set.runtimeGap.map(runtimeLabel).join(" and ")} on this machine unblocks the entries above. `}
          The cockpit installs runtimes; the cluster never puts software on somebody&apos;s machine.
        </p>
      ) : null}

      {isOwner && pullableCount > 0 && !blocked ? (
        <div className="os-fleet-recommended-act">
          <Button
            tone="primary"
            busy={busy}
            busyLabel="Starting..."
            onClick={() => {
              setRefusal("");
              void onPull().then(setRefusal);
            }}
          >
            Pull recommended set
          </Button>
          <p className="os-caption">
            {pullableCount === 1
              ? "One model, pulled in the background."
              : `${pullableCount} models, pulled one after another in the background.`}{" "}
            Leaving this page does not stop them.
          </p>
        </div>
      ) : null}

      {refusal ? (
        <Notice
          tone="error"
          sentence="The set did not start."
          next="Nothing was downloaded."
          detail={refusal}
        />
      ) : null}
    </div>
  );
}

/**
 * One recommendation: what it is for, what it is, and what is in the way.
 *
 * THE LEVEL IS THE ONLY COLUMN; everything else stacks under the model id.
 * The first shape here was four columns, and the browser refuted it for the
 * reason `.os-fleet-machinemodel` beside it already records: a model id is an
 * unbreakable 38-character string, so an `auto` column sized to the longest
 * one left the two shorter rows' sizes and reasons at a different x from the
 * third's. Three readings that should scan as a column read as three
 * unrelated lines.
 *
 * Stacked, the level still leads -- which is the question a person is actually
 * asking, "what will serve my fast calls" -- and the id keeps its own line,
 * which it needs, being the one string here somebody may have to copy.
 */
function RecommendedRow({ entry }: { entry: Recommendation }) {
  const size = entry.sizeBytes > 0 ? formatBytes(entry.sizeBytes) : "";
  return (
    <li className="os-fleet-recommendedrow" data-blocked={!entry.pullable || undefined}>
      <span className="os-fleet-recommended-level">{levelLabel(entry.level)}</span>
      <span className="os-fleet-recommended-body">
        <span className="os-fleet-machinemodel-id os-mono">{entry.modelId}</span>
        <span className="os-fleet-recommended-readings">
          {size ? <span className="os-fleet-recommended-size">{size}</span> : null}
          {entry.pullable ? (
            <span className="os-fleet-recommended-note">{entry.notes}</span>
          ) : (
            <span className="os-fleet-recommended-blocked">{entry.blocked}</span>
          )}
        </span>
      </span>
    </li>
  );
}

/** A pull in flight: the runtime's own words, and how far through this step. */
function LivePull({ pull }: { pull: ModelPull }) {
  const fraction = pullProgressFraction(pull);
  const step = pullStepLabel(pull);

  return (
    <div className="os-fleet-pull" role="status" aria-label={`Pulling ${pull.model}`}>
      <div className="os-fleet-pull-line">
        <span className="os-fleet-pull-model os-mono">{pull.model}</span>
        <span className="os-fleet-pull-status">
          {pull.status === "requested"
            ? "waiting for the machine to start"
            : pull.statusLine || "downloading"}
        </span>
      </div>

      {fraction === null ? null : (
        <div
          className="os-progress-track"
          role="progressbar"
          aria-label={`Current step of ${pull.model}`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(fraction * 100)}
        >
          <div className="os-progress-bar" style={{ width: `${fraction * 100}%` }} />
        </div>
      )}

      {step ? <p className="os-fleet-pull-bytes os-mono">{step}</p> : null}

      <p className="os-caption">
        A model arrives in pieces and each one is counted from zero, so these numbers describe the
        piece being fetched rather than the whole download. Leaving this page does not stop it.
      </p>
    </div>
  );
}

/**
 * What has been pulled here before.
 *
 * A FAILURE IS THE POINT OF THIS LIST. A succeeded pull is already visible as
 * a model above; a failed one leaves nothing behind at all, so without this
 * the machine's models simply do not change and nothing anywhere says why.
 */
function PullHistory({
  pulls,
  live,
  loading,
}: {
  pulls: ModelPull[];
  live: ModelPull | null;
  loading: boolean;
}) {
  const past = pulls.filter((p) => p.pullId !== live?.pullId);
  if (loading && pulls.length === 0) {
    return <p className="os-caption">Reading this machine's pulls.</p>;
  }
  if (past.length === 0) return null;

  return (
    <ul className="os-fleet-pullhistory">
      {past.map((pull) => (
        <li key={pull.pullId} className="os-fleet-pastpull" data-status={pull.status}>
          <span className="os-fleet-machinemodel-id os-mono">{pull.model}</span>
          <span className="os-fleet-pastpull-when">
            {formatMoment(pull.endedAt || pull.requestedAt)}
          </span>
          <span className="os-fleet-pastpull-what">{pastPullSentence(pull)}</span>
        </li>
      ))}
    </ul>
  );
}

/**
 * What happened, in words.
 *
 * `readvertised` earns its own sentence: a model on disk that the cluster
 * cannot see yet is neither a success a person can use nor a failure, and
 * reporting it as plain success would leave them looking for a model that is
 * not in the catalog.
 */
function pastPullSentence(pull: ModelPull): string {
  switch (pull.status) {
    case "succeeded":
      return pull.readvertised
        ? "Pulled. The cluster can route to it now."
        : "Pulled. The cluster sees it when this machine next reconnects.";
    case "cancelled":
      return "Stopped. Whatever was fetched is still on the machine, so pulling again resumes.";
    case "failed":
      return pull.errorMessage || "Failed, and the machine said nothing about why.";
    default:
      return pull.status;
  }
}

/** The act. One control line, per the interface language's rule 5. */
function PullForm({
  machineLabel,
  busy,
  blocked,
  onStart,
}: {
  machineLabel: string;
  busy: boolean;
  blocked: boolean;
  onStart: (model: string) => Promise<string>;
}) {
  const [model, setModel] = useState("");
  const [refusal, setRefusal] = useState("");

  // ONE PULL AT A TIME PER MACHINE, and the control is ABSENT rather than
  // disabled while one runs -- there is nothing to press, and a greyed button
  // beside a live progress bar invites the click it exists to prevent.
  if (blocked) return null;

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (busy) return;
    setRefusal("");
    void onStart(model).then((message) => {
      if (message === "") {
        setModel("");
        return;
      }
      setRefusal(message);
    });
  }

  return (
    <form className="os-fleet-pullform" onSubmit={submit}>
      {/* ONE CONTROL LINE, and the caption is not on it. Rule 5 puts inputs
          and buttons on a line at one height; a sentence sharing that line
          crowds both and pushes the act away from the field it acts on. */}
      <div className="os-form-row">
        <label className="os-sr-only" htmlFor="fleet-pull-model">
          Model to pull onto {machineLabel}
        </label>
        <input
          id="fleet-pull-model"
          className="os-input"
          value={model}
          disabled={busy}
          placeholder="llama3.1:8b"
          onChange={(e) => setModel(e.target.value)}
        />
        <Button
          type="submit"
          tone="primary"
          busy={busy}
          busyLabel="Asking..."
          disabled={model.trim() === ""}
        >
          Pull
        </Button>
      </div>
      {refusal ? (
        <Notice
          tone="error"
          sentence="The pull did not start."
          next="Nothing was downloaded."
          detail={refusal}
        />
      ) : (
        <p className="os-caption">
          The model id as its runtime knows it, passed through untouched. A Hugging Face
          repository works too, as <code className="os-mono">hf.co/owner/repo:Q4_K_M</code>.
        </p>
      )}
    </form>
  );
}

/**
 * Compare two user ids that may differ only in canonicalisation.
 *
 * The engine bare-ifies ids on egress and the shell never composes them, so a
 * row's `ownerUserId` and the session's `userId` are routinely the same
 * subject in two spellings. Comparing them naively would hide the Pull act
 * from the person whose machine it is.
 */
function sameSubject(a: string, b: string): boolean {
  const bare = (id: string) => {
    const trimmed = id.trim();
    const at = trimmed.lastIndexOf(":");
    return at >= 0 ? trimmed.slice(at + 1) : trimmed;
  };
  return a.trim() === b.trim() || (bare(a) !== "" && bare(a) === bare(b));
}
