import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { Subhead } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { pullProgressFraction, pullStepLabel, type ModelPull } from "./models";

export function LivePull({ pull }: { pull: ModelPull }) {
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
export function PullHistory({
  pulls,
  live,
  loading,
  error,
}: {
  pulls: ModelPull[];
  live: ModelPull | null;
  loading: boolean;
  error: string;
}) {
  const past = pulls.filter((p) => p.pullId !== live?.pullId);
  if (loading && pulls.length === 0) {
    return <RecordListSkeleton label="Loading this machine's pulls" />;
  }
  if (past.length === 0) return null;

  return (
    <><Subhead meta={!loading && !error ? past.length : undefined}>Past downloads</Subhead>
    <RecordList as="ul" label="Past downloads">
      {past.map(pull => <RecordRow key={pull.pullId} name={pull.model} secondary={formatMoment(pull.endedAt || pull.requestedAt)}
        state={pull.status} tone={pull.status === "failed" ? "warn" : "muted"}>{pastPullSentence(pull)}</RecordRow>)}
    </RecordList></>
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
