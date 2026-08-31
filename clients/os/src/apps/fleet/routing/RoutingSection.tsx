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
import { Button, FleetError, Panel, SectionHead } from "../ui";
import { useRoutingPolicy, type RoutingPolicyDraft } from "./useRoutingPolicy";

// The routing policy: how the router orders the machines it could send a call
// to, for this person's fleet.

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

  return (
    <div className="os-fleet">
      <SectionHead title="Routing">
        {/* Offered only when the feed is behind -- see the workbenches
            section for the reasoning. v1:worker:routingPolicy is broadcast,
            so a policy edited in another tab or in the portal arrives here on
            its own; a standing refresh button would say otherwise. */}
        {feedIsBehind(state.liveState) ? (
          <Button onClick={state.reseed}>Re-read</Button>
        ) : null}
      </SectionHead>

      <Panel label="Routing policy">
        {state.loading ? <p className="os-caption">Reading your routing policy.</p> : null}

        {state.error ? (
          <FleetError
            sentence="Your routing policy could not be read."
            next="The controls below show the defaults until it loads."
            detail={state.error}
          />
        ) : null}

        {/* THE ABSENT STATE IS STATED, NOT APOLOGISED FOR. Most people have
            no policy row, and the router applies these two values to them
            today. Saying so is the difference between "you have not
            configured this" and "this is not configured", which are
            different claims about whether routing is happening. */}
        {policy === null && !state.loading ? (
          <p className="os-fleet-note">
            No policy set -- the defaults apply: <strong>{DEFAULT_STRATEGY}</strong> ordering with{" "}
            <strong>{DEFAULT_FALLBACK}</strong> on a refusal. Nothing is written until you save.
          </p>
        ) : null}

        {diverged ? (
          <p className="os-fleet-note" role="status">
            This policy changed somewhere else while you were editing. Your edits are still here;
            saving will overwrite the newer row.
          </p>
        ) : null}

        <fieldset className="os-field-group">
          <legend>Strategy</legend>
          <div className="os-choice-column" role="radiogroup" aria-label="Routing strategy">
            {ROUTING_STRATEGIES.map((one) => (
              <label key={one} className="os-fleet-radio">
                <input
                  type="radio"
                  name="fleet-strategy"
                  value={one}
                  checked={draft.strategy === one}
                  onChange={() => edit({ strategy: one })}
                />
                <span>
                  <span className="os-mono">{one}</span>
                  <span className="os-caption">{STRATEGY_BLURB[one as RoutingStrategy]}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>When the chosen machine refuses before the call starts</legend>
          <div className="os-choice-column" role="radiogroup" aria-label="Routing fallback">
            {ROUTING_FALLBACKS.map((one) => (
              <label key={one} className="os-fleet-radio">
                <input
                  type="radio"
                  name="fleet-fallback"
                  value={one}
                  checked={draft.fallback === one}
                  onChange={() => edit({ fallback: one })}
                />
                <span>
                  <span className="os-mono">{one}</span>
                  <span className="os-caption">{FALLBACK_BLURB[one as RoutingFallback]}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Required labels</legend>
          <p className="os-caption">
            A machine must carry all of these to be a candidate at all, on top of whatever the
            call itself requires. This narrows and never widens -- a policy cannot make a machine
            eligible for work the agent did not ask to run there. Values match exactly; there is
            no wildcard.
          </p>
          <MapEditor
            value={draft.requireLabels}
            onChange={(requireLabels) => edit({ requireLabels })}
            busy={state.saving}
            label="Required labels"
            idPrefix="fleet-require"
            tone="neutral"
          />
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Preferred labels</legend>
          <p className="os-caption">
            An ordering hint, not a filter. Under labelMatch, candidates matching more of these
            sort first; under the other strategies they break ties.
          </p>
          <MapEditor
            value={draft.preferLabels}
            onChange={(preferLabels) => edit({ preferLabels })}
            busy={state.saving}
            label="Preferred labels"
            idPrefix="fleet-prefer"
            tone="neutral"
          />
        </fieldset>

        {state.saveError ? (
          <FleetError
            sentence="The policy was not saved."
            next="Nothing was written; your edits are still here."
            detail={state.saveError}
          />
        ) : null}

        <div className="os-fleet-head-actions">
          <Button
            tone="primary"
            busy={state.saving}
            busyLabel="Saving..."
            onClick={() => {
              void state.save(draft).then((ok) => {
                // Released ONLY on success, so the row's own echo becomes
                // authoritative again. Releasing after a REFUSAL would hand
                // the draft back to the row and discard the operator's edits
                // in the same beat as an error saying they were kept.
                // Holding it forever would be the opposite failure: an editor
                // frozen against every later change from anywhere else.
                if (ok) setTouched(false);
              });
            }}
          >
            {policy === null ? "Create policy" : "Save policy"}
          </Button>
          <Button
            disabled={!touched}
            onClick={() => {
              setTouched(false);
              setDraft(fromPolicy(policy));
            }}
          >
            Discard changes
          </Button>
        </div>

        <p role="status" className="os-fleet-status">
          {state.announcement}
        </p>

        <p className="os-caption">
          One active policy per person. Saving an existing one edits it in place rather than
          adding another, because a call's routing record points at whichever row made the choice
          -- a second active row would leave every edit made against the older one stranded.
        </p>
      </Panel>
    </div>
  );
}

function fromPolicy(
  policy: { strategy: string; fallback: string; requireLabels: LabelMap; preferLabels: LabelMap } | null,
): RoutingPolicyDraft {
  if (policy === null) {
    return {
      strategy: DEFAULT_STRATEGY,
      fallback: DEFAULT_FALLBACK,
      requireLabels: {},
      preferLabels: {},
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
  };
}
