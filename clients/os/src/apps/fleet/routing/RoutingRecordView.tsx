import { Chip, Chips } from "../../../kit";
import type { LabelMap } from "../labels";
import type { RoutingRecord } from "../rows";

// How one call's destination was chosen, rendered as a try-order list.
//
// ===========================================================================
// AN ABSENT RECORD IS SAID, NOT DRAWN
// ===========================================================================
// `routing` is an empty object on two kinds of row: one written before the
// router existed, and one whose path never picked -- a denial that happened
// before the choice. Rendering either as an empty candidate table would say
// "the router considered nobody", which is a claim about a decision that was
// never made. `present` is the discriminator and this component's first
// branch, so the wrong sentence has nowhere to come from.
//
// The candidates are rendered as an ORDERED list because the order IS the
// content: `candidatesConsidered` is the sequence the router would have tried,
// and `attempts` says how far down it got. A set would lose the one fact an
// operator opened this panel to see.

export function RoutingRecordView({ routing }: { routing: RoutingRecord }) {
  if (!routing.present) {
    return (
      <p className="os-caption">
        No routing decision recorded. Either this call predates the router, or it was refused
        before a machine was chosen.
      </p>
    );
  }

  const attempted = routing.attempts > 0 ? routing.attempts : 1;

  return (
    <div className="os-fleet-routing-record">
      <dl className="os-facts">
        <dt>Strategy</dt>
        <dd className="os-mono">{routing.strategy || "--"}</dd>
        <dt>Policy</dt>
        {/* An empty policyId is not a missing value: it means no policy row
            applied and the router used its defaults. Rendering it as "--"
            would read as "we could not tell", which is a different answer. */}
        <dd className="os-mono">{routing.policyId || "default policy"}</dd>
        <dt>Chosen by</dt>
        <dd className="os-mono">{routing.selectedBy || "--"}</dd>
        <dt>Attempts</dt>
        <dd>{attempted}</dd>
      </dl>

      {routing.reroutedFrom ? (
        <p className="os-fleet-reroute">
          Rerouted from <span className="os-mono">{routing.reroutedFrom}</span>.
        </p>
      ) : null}

      <p className="os-caption">
        {routing.candidatesConsidered.length === 0
          ? "No candidates were recorded."
          : `Candidates, in the order the router would try them (${routing.candidatesConsidered.length}):`}
      </p>
      {routing.candidatesConsidered.length > 0 ? (
        <ol className="os-fleet-candidates">
          {routing.candidatesConsidered.map((id, index) => (
            <li key={id} className="os-mono" data-tried={index < attempted || undefined}>
              {id}
              {index < attempted ? <span className="os-fleet-tried">tried</span> : null}
            </li>
          ))}
        </ol>
      ) : null}

      <LabelSet label="Required" labels={routing.requireLabels} />
      <LabelSet label="Preferred" labels={routing.preferLabels} />
    </div>
  );
}

function LabelSet({ label, labels }: { label: string; labels: LabelMap }) {
  const keys = Object.keys(labels).sort();
  if (keys.length === 0) return null;
  return (
    <div className="os-fleet-labelset">
      <span className="os-caption">{label}</span>
      <Chips label={`${label} labels`}>
        {keys.map((key) => (
          <Chip key={key} tone="neutral">{`${key}=${labels[key] ?? ""}`}</Chip>
        ))}
      </Chips>
    </div>
  );
}
