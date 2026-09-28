import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";

import { ActionBar, type Act } from "../../../kit/ActionBar";
import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { InfoDetail } from "../../../kit/InfoDetail";
import { Notice, RefreshButton, Select } from "../../../kit";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { MapEditor } from "../MapEditor";
import { chipsFromMap, type LabelMap } from "../labels";
import {
  DEFAULT_FALLBACK,
  DEFAULT_STRATEGY,
  FALLBACK_BLURB,
  ROUTING_FALLBACKS,
  ROUTING_STRATEGIES,
  STRATEGY_BLURB,
  type RoutingFallback,
  type RoutingStrategy,
} from "../rows";
import { useRoutingPolicy, type RoutingPolicyDraft } from "./useRoutingPolicy";

// Fleet > Routing > Machines: which of YOUR machines takes a call once a route
// has sent it to your machines (was Fleet's "Machine routing" section). It
// decides WHICH MACHINE and nothing else; which source answers is the route's,
// and which route is the rule's.
//
// The row is per person (`v1:worker:routingPolicy`), live, and edited in place:
//
// ABSENT IS A VALID STATE. A person who never saved has no row and the router
// applies the defaults; the draft opens FROM those defaults, so a first save
// never silently changes behaviour the bar said was already in force.
//
// STALENESS RESOLVES TOWARD THE ROW, but only into an UNTOUCHED draft. An edit
// from another tab reaches this editor; a draft somebody is typing into is
// left alone and the disagreement is shown instead.

export function MachinesTab({ header }: { header: (actions: ReactNode, meta?: ReactNode) => ReactNode }) {
  const state = useRoutingPolicy();
  const { policy } = state;
  const [draft, setDraft] = useState<RoutingPolicyDraft>(() => fromPolicy(null));
  const [touched, setTouched] = useState(false);

  // The row's value-identity: a fold that changes nothing must not reset a
  // draft somebody is editing, and depending on the object would.
  const rowIdentity = useMemo(
    () =>
      policy === null
        ? ""
        : [policy.id, policy.strategy, policy.fallback, chipsFromMap(policy.requireLabels).join(","), chipsFromMap(policy.preferLabels).join(","), policy.modelPreference.join(",")].join("|"),
    [policy],
  );
  // The identity the CURRENT draft was seeded from. "Changed elsewhere" is a
  // question about the ROW moving under an edit, not about the edit differing
  // from the row -- which it does the instant anybody types.
  const baseline = useRef(rowIdentity);
  useEffect(() => {
    if (touched) return;
    setDraft(fromPolicy(policy));
    baseline.current = rowIdentity;
  }, [rowIdentity, touched]);
  const diverged = touched && policy !== null && rowIdentity !== baseline.current;

  function edit(patch: Partial<RoutingPolicyDraft>) {
    setTouched(true);
    setDraft((held) => ({ ...held, ...patch }));
  }

  const loading = state.loading && policy === null && !touched;
  const acts: Act[] = !touched
    ? []
    : [
        { label: "Discard changes", text: true, onAct: () => { setTouched(false); setDraft(fromPolicy(policy)); } },
        { label: "Save", tone: "primary", busy: state.saving, onAct: () => { void state.save(draft).then((ok) => { if (ok) setTouched(false); }); } },
      ];
  const barState = loading ? "" : touched ? (state.saving ? "Saving" : "Unsaved") : policy === null ? "Default" : "Saved";
  const barDetail = !touched && policy === null && !loading ? "First eligible machine, then the next match" : "";

  return (
    <>
      <div className="os-deploy-scroll">
        <ActivityTarget target="fleet:machine-routing" className="fleet-routing-machines">
          {header(
            <>
              <InfoDetail title="How a machine is chosen">
                <p>Required labels filter which of your machines can take a call. Preferred labels and the strategy order them. A refusal may try the next matching machine only before a call starts; a started call is never replayed.</p>
                <p>Routes choose the source. The model order ranks your local models when a route reaches your machines.</p>
              </InfoDetail>
              {feedIsBehind(state.liveState) ? <RefreshButton label="Reconnect machine choice" onClick={state.reseed} /> : null}
            </>,
          )}
          {loading ? (
            <ContentSkeleton kind="form" label="Loading how your machines are chosen" />
          ) : (
            <>
              {state.error ? <Notice tone="error" sentence="How your machines are chosen could not be read." next="The controls show the defaults until it loads." detail={state.error} /> : null}
              {diverged ? <Notice tone="warn" sentence="This changed somewhere else. Your draft is kept." next="Saving overwrites the newer version; discard your changes to load it first." /> : null}
              <div className="fleet-routing-stages">
                <details className="fleet-routing-stage">
                  <summary><small>1 · Eligibility</small><strong>Required labels</strong><span>{chipsFromMap(draft.requireLabels).join(", ") || "Any eligible machine"}</span></summary>
                  <p className="os-caption">Every label must match exactly.</p>
                  <MapEditor value={draft.requireLabels} onChange={(requireLabels) => edit({ requireLabels })} busy={state.saving} label="Required labels" idPrefix="fleet-require" tone="neutral" />
                </details>
                <details className="fleet-routing-stage">
                  <summary><small>2 · Preference</small><strong>Preferred labels</strong><span>{chipsFromMap(draft.preferLabels).join(", ") || "No preference"}</span></summary>
                  <p className="os-caption">Matching labels move a machine up; they never rule one out.</p>
                  <MapEditor value={draft.preferLabels} onChange={(preferLabels) => edit({ preferLabels })} busy={state.saving} label="Preferred labels" idPrefix="fleet-prefer" tone="neutral" />
                </details>
                <details className="fleet-routing-stage">
                  <summary><small>3 · Order</small><strong>Strategy</strong><span>{strategyLabel(draft.strategy)}</span></summary>
                  <Select id="fleet-strategy" label="Routing strategy" value={draft.strategy} onChange={(strategy) => edit({ strategy })}>
                    {ROUTING_STRATEGIES.map((one) => <option key={one} value={one}>{strategyLabel(one)}</option>)}
                  </Select>
                  <p className="os-caption">{STRATEGY_BLURB[draft.strategy as RoutingStrategy]}</p>
                </details>
                <details className="fleet-routing-stage">
                  <summary><small>4 · Refusal</small><strong>Fallback</strong><span>{fallbackLabel(draft.fallback)}</span></summary>
                  <Select id="fleet-fallback" label="Routing fallback" value={draft.fallback} onChange={(fallback) => edit({ fallback })}>
                    {ROUTING_FALLBACKS.map((one) => <option key={one} value={one}>{fallbackLabel(one)}</option>)}
                  </Select>
                  <p className="os-caption">{FALLBACK_BLURB[draft.fallback as RoutingFallback]}</p>
                </details>
              </div>
              <section className="fleet-routing-models">
                <h4>Preferred model order</h4>
                <p className="os-caption">One model per line. Other compatible models remain eligible.</p>
                <label className="os-sr-only" htmlFor="fleet-model-preference">Preferred model order, one model id per line</label>
                <textarea id="fleet-model-preference" className="os-input os-fleet-modelorder" rows={4} placeholder={"llama3.3:70b\nqwen2.5:7b"} value={draft.modelPreference.join("\n")} onChange={(e) => edit({ modelPreference: e.target.value.split("\n") })} />
              </section>
              {state.saveError ? <Notice tone="error" sentence="That was not saved." next="Nothing was written; your edits are still here." detail={state.saveError} /> : null}
              <p role="status" className="os-status-line">{state.announcement}</p>
            </>
          )}
        </ActivityTarget>
      </div>
      <ActionBar state={barState} detail={barDetail} tone={state.saving ? "busy" : touched ? "paused" : "none"} acts={acts} />
    </>
  );
}

function fromPolicy(
  policy: { strategy: string; fallback: string; requireLabels: LabelMap; preferLabels: LabelMap; modelPreference: string[] } | null,
): RoutingPolicyDraft {
  if (policy === null) {
    // Empty model order is the DEFAULT ORDER, not an absent setting: seeding a
    // list would turn "no preference" into "preferred what the default does".
    return { strategy: DEFAULT_STRATEGY, fallback: DEFAULT_FALLBACK, requireLabels: {}, preferLabels: {}, modelPreference: [] };
  }
  return {
    // A value this build does not know falls back IN THE PICKER only; the row
    // is untouched until a save.
    strategy: (ROUTING_STRATEGIES as readonly string[]).includes(policy.strategy) ? policy.strategy : DEFAULT_STRATEGY,
    fallback: (ROUTING_FALLBACKS as readonly string[]).includes(policy.fallback) ? policy.fallback : DEFAULT_FALLBACK,
    requireLabels: { ...policy.requireLabels },
    preferLabels: { ...policy.preferLabels },
    modelPreference: [...policy.modelPreference],
  };
}

function strategyLabel(value: string): string {
  return ({ firstFit: "First eligible", leastLoaded: "Least busy", labelMatch: "Best label match", roundRobin: "Take turns" } as Record<string, string>)[value] ?? value;
}
function fallbackLabel(value: string): string {
  return ({ nextMatching: "Try the next match", none: "Return the refusal" } as Record<string, string>)[value] ?? value;
}
