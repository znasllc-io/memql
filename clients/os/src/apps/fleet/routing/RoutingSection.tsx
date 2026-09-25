import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { useEffect, useMemo, useRef, useState } from "react";

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
import { Button, Head, Notice, Select } from "../../../kit";
import { InfoDetail } from "../../../kit/InfoDetail";
import { RefreshButton } from "../FleetControls";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { useRoutingPolicy, type RoutingPolicyDraft } from "./useRoutingPolicy";

// The routing policy: how the router orders the machines it could send a call
// to, for this person's fleet. It decides WHICH MACHINE and nothing else --
// which model answers, and which door a call goes through, are Settings ->
// Rules.

export function RoutingSection() {
  const state = useRoutingPolicy();
  const { policy } = state;

  // The draft opens from the row, or -- when there is no row -- from the
  // DEFAULTS THE ROUTER ALREADY APPLIES. Not from a blank: a first save that
  // silently changed behaviour the caption said was already in force would
  // make this editor a trap, and the two values are named once in rows.ts so
  // the caption and the draft cannot drift apart.
  const [draft, setDraft] = useState<RoutingPolicyDraft>(() => fromPolicy(null));
  const [touched, setTouched] = useState(false);

  // The row's value-identity. A fold that changes nothing about the policy
  // must not reset a draft somebody is editing, and depending on the object
  // would do exactly that.
  const rowIdentity = useMemo(
    () =>
      policy === null
        ? ""
        : [
            policy.id,
            policy.strategy,
            policy.fallback,
            chipsFromMap(policy.requireLabels).join(","),
            chipsFromMap(policy.preferLabels).join(","),
            policy.modelPreference.join(","),
          ].join("|"),
    [policy],
  );

  // The row identity the CURRENT draft was seeded from. Divergence is a
  // question about the ROW moving under an edit, not about the edit itself
  // differing from the row -- which it does the instant anybody types. A
  // comparison against the live row would put "this changed somewhere else"
  // on screen for every local change, which trains an operator to ignore the
  // one message that means their save is about to overwrite somebody.
  const baseline = useRef(rowIdentity);

  // STALENESS RESOLVES TOWARD THE ROW -- but only into an UNTOUCHED draft.
  // A policy edited elsewhere (another tab, the portal) has to reach this
  // editor, or an operator saves a set assembled from a state the cluster
  // never had. Discarding typing somebody is in the middle of would be worse
  // than either, so a touched draft is left alone and the disagreement is
  // shown instead.
  useEffect(() => {
    if (touched) return;
    setDraft(fromPolicy(policy));
    baseline.current = rowIdentity;
    // DEPS ARE (rowIdentity, touched) ON PURPOSE: rowIdentity IS the policy's
    // value-identity, while `policy` is a fresh object on every fold -- so
    // depending on the object would reset the draft on a change that touched
    // nothing in it.
  }, [rowIdentity, touched]);

  const diverged = touched && policy !== null && rowIdentity !== baseline.current;

  function edit(patch: Partial<RoutingPolicyDraft>) {
    setTouched(true);
    setDraft((held) => ({ ...held, ...patch }));
  }

  return <ActivityTarget target="fleet:machine-routing" className="os-fleet fleet-routing">
    <Head title="Machine routing">
      <InfoDetail title="Machine routing"><p>Required labels filter candidates. Preferred labels and strategy order them. A refusal may try the next matching machine only before a call starts; a started call is never replayed.</p><p>Source policies choose the inference source. Model order ranks compatible models after a policy sends the call to your fleet.</p></InfoDetail>
      {feedIsBehind(state.liveState) ? <RefreshButton label="Reconnect machine routing" onClick={state.reseed} /> : null}
    </Head>
    {state.loading && policy === null && !touched ? <ContentSkeleton kind="form" label="Loading your routing policy" /> : <>
    {state.error ? <Notice tone="error" sentence="Your routing policy could not be read." next="The controls show defaults until it loads." detail={state.error} /> : null}
    {policy === null && !state.loading && !state.error ? <p className="os-caption">Using the default: choose the first eligible machine and try the next match if it refuses. Save to apply your preferences.</p> : null}
    {diverged ? <Notice tone="warn" sentence="This policy changed somewhere else. Your draft is retained." next="Saving overwrites the newer row; discard your changes to load it first." /> : null}
    <div className="fleet-routing-stages">
      <details className="fleet-routing-stage"><summary><small>1 · Eligibility</small><strong>Required labels</strong><span>{chipsFromMap(draft.requireLabels).join(", ") || "Any eligible machine"}</span></summary>
        <p className="os-caption">Every label must match exactly. This narrows the call’s existing eligibility.</p>
        <MapEditor value={draft.requireLabels} onChange={requireLabels => edit({ requireLabels })} busy={state.saving} label="Required labels" idPrefix="fleet-require" tone="neutral" />
      </details>
      <details className="fleet-routing-stage"><summary><small>2 · Preference</small><strong>Preferred labels</strong><span>{chipsFromMap(draft.preferLabels).join(", ") || "No preference"}</span></summary>
        <p className="os-caption">Matching labels improve order; they never filter a machine out.</p>
        <MapEditor value={draft.preferLabels} onChange={preferLabels => edit({ preferLabels })} busy={state.saving} label="Preferred labels" idPrefix="fleet-prefer" tone="neutral" />
      </details>
      <details className="fleet-routing-stage"><summary><small>3 · Order</small><strong>Strategy</strong><span>{strategyLabel(draft.strategy)}</span></summary>
        <Select id="fleet-strategy" label="Routing strategy" value={draft.strategy} onChange={strategy => edit({ strategy })}>{ROUTING_STRATEGIES.map(one => <option key={one} value={one}>{strategyLabel(one)}</option>)}</Select>
        <p className="os-caption">{STRATEGY_BLURB[draft.strategy as RoutingStrategy]}</p>
      </details>
      <details className="fleet-routing-stage"><summary><small>4 · Refusal</small><strong>Fallback</strong><span>{fallbackLabel(draft.fallback)}</span></summary>
        <Select id="fleet-fallback" label="Routing fallback" value={draft.fallback} onChange={fallback => edit({ fallback })}>{ROUTING_FALLBACKS.map(one => <option key={one} value={one}>{fallbackLabel(one)}</option>)}</Select>
        <p className="os-caption">{FALLBACK_BLURB[draft.fallback as RoutingFallback]}</p>
      </details>
    </div>
    <section className="fleet-routing-models"><h4>Preferred model order</h4><p className="os-caption">One model per line. Other compatible models remain eligible.</p>
      <label className="os-sr-only" htmlFor="fleet-model-preference">Preferred model order, one model id per line</label>
      <textarea id="fleet-model-preference" className="os-input os-fleet-modelorder" rows={4} placeholder={"llama3.3:70b\nqwen2.5:7b"} value={draft.modelPreference.join("\n")} onChange={e => edit({ modelPreference: e.target.value.split("\n") })} />
    </section>
    {state.saveError ? <Notice tone="error" sentence="The policy was not saved." next="Nothing was written; your edits are still here." detail={state.saveError} /> : null}
    <div className="fleet-save-row"><span className="os-caption">{touched ? "Unsaved draft" : "Personal fleet policy"}</span><div className="os-head-actions">
      <Button disabled={!touched || state.saving} onClick={() => { setTouched(false); setDraft(fromPolicy(policy)); }}>Discard changes</Button>
      <Button tone="primary" busy={state.saving} busyLabel="Saving…" onClick={() => { void state.save(draft).then(ok => { if (ok) setTouched(false); }); }}>{policy === null ? "Create policy" : "Save policy"}</Button>
    </div></div>
    <p role="status" className="os-status-line">{state.announcement}</p>
    </>}
  </ActivityTarget>;
}

function fromPolicy(
  policy: {
    strategy: string;
    fallback: string;
    requireLabels: LabelMap;
    preferLabels: LabelMap;
    modelPreference: string[];
  } | null,
): RoutingPolicyDraft {
  if (policy === null) {
    return {
      strategy: DEFAULT_STRATEGY,
      fallback: DEFAULT_FALLBACK,
      requireLabels: {},
      preferLabels: {},
      // Empty is the DEFAULT ORDER, not an absent setting: the router ranks
      // by parameters, then context window, then id. Seeding a list here
      // would turn "you have expressed no preference" into "you preferred
      // exactly what the default already does".
      modelPreference: [],
    };
  }
  return {
    // A row carrying a strategy this build does not know about falls back to
    // the default IN THE PICKER only -- the row is untouched until a save.
    // Rendering no selection at all would look like a broken control.
    strategy: (ROUTING_STRATEGIES as readonly string[]).includes(policy.strategy)
      ? policy.strategy
      : DEFAULT_STRATEGY,
    fallback: (ROUTING_FALLBACKS as readonly string[]).includes(policy.fallback)
      ? policy.fallback
      : DEFAULT_FALLBACK,
    requireLabels: { ...policy.requireLabels },
    preferLabels: { ...policy.preferLabels },
    modelPreference: [...policy.modelPreference],
  };
}

function strategyLabel(value: string): string { return ({ firstFit: "First eligible", leastLoaded: "Least busy", labelMatch: "Best label match", roundRobin: "Take turns" } as Record<string, string>)[value] ?? value; }
function fallbackLabel(value: string): string { return ({ nextMatching: "Try the next match", none: "Return the refusal" } as Record<string, string>)[value] ?? value; }
