import { useRef, useState, type ReactNode } from "react";

import {
  Button,
  Callout,
  ErrorNotice,
  Field,
  FormRow,
  LabelChips,
  Panel,
  Select,
  Skeleton,
} from "../ui";
import { chipsFromMap, mapFromChips, parseLabelChip } from "./labels";
import {
  FALLBACK_BLURB,
  ROUTING_FALLBACKS,
  ROUTING_STRATEGIES,
  STRATEGY_BLURB,
  type RoutingFallback,
  type RoutingStrategy,
} from "./rows";
import { useRoutingPolicy } from "./useRoutingPolicy";

// The routing policy editor: which of your machines a piece of work lands on.
//
// ===========================================================================
// THE DEFAULTS ARE THE ROUTER'S, NOT THIS FORM'S
// ===========================================================================
// firstFit + nextMatching is what the router applies to a person with no
// policy row, so those are the values the empty form opens on. That agreement
// matters: a form whose defaults differed from the no-policy behaviour would
// mean pressing Save with nothing typed CHANGED the routing, which is the last
// thing a settings form should do.
//
// ===========================================================================
// LABELS ARE A DRAFT HERE, UNLIKE ON A MACHINE
// ===========================================================================
// A machine's operator labels write on every add and remove -- one chip, one
// mutation, because each is independently meaningful. A policy is one
// decision: strategy, requirements, preferences and fallback are read together
// by the router on every dispatch, and half-applying them would route work
// somewhere nobody chose. So the chips edit local state and Save writes the
// whole policy.

const NO_POLICY_STRATEGY: RoutingStrategy = "firstFit";
const NO_POLICY_FALLBACK: RoutingFallback = "nextMatching";

export function RoutingPolicyEditor(): ReactNode {
  const state = useRoutingPolicy();
  const { policy } = state;

  const [strategy, setStrategy] = useState<string>(NO_POLICY_STRATEGY);
  const [fallback, setFallback] = useState<string>(NO_POLICY_FALLBACK);
  const [requireChips, setRequireChips] = useState<string[]>([]);
  const [preferChips, setPreferChips] = useState<string[]>([]);
  const [chipError, setChipError] = useState("");

  // Re-seed the form when the row underneath it changes identity -- the first
  // read landing, and the re-read after a save. Adjusting state during render
  // rather than in an effect: an effect would paint the empty form once with
  // the row already in hand, which reads as "you have no policy" for a frame
  // on every visit.
  const seededFrom = useRef<string | null>(null);
  const seed = policy === null ? "" : policy.id + "|" + policy.createdAt;
  if (!state.loading && seededFrom.current !== seed) {
    seededFrom.current = seed;
    setStrategy(policy?.strategy || NO_POLICY_STRATEGY);
    setFallback(policy?.fallback || NO_POLICY_FALLBACK);
    setRequireChips(policy === null ? [] : chipsFromMap(policy.requireLabels));
    setPreferChips(policy === null ? [] : chipsFromMap(policy.preferLabels));
  }

  // LabelChips rejects blanks and duplicates itself; what it cannot know is
  // that a chip here has to be a PAIR. A bare word is a value with no key, and
  // guessing which half the operator meant is how a routing rule ends up
  // matching something nobody wrote -- so it is refused, out loud.
  function addChip(
    text: string,
    setChips: (update: (current: string[]) => string[]) => void,
  ): void {
    if (parseLabelChip(text) === null) {
      setChipError(`"${text}" is not a label. Write it as key=value, for example os=darwin.`);
      return;
    }
    setChipError("");
    setChips((current) => [...current, text]);
  }

  function save(): void {
    state.save({
      strategy,
      fallback,
      requireLabels: mapFromChips(requireChips),
      preferLabels: mapFromChips(preferChips),
    });
  }

  if (state.loading && policy === null) {
    return (
      <Panel>
        <Skeleton variant="rows" rows={3} />
      </Panel>
    );
  }

  return (
    <Panel>
      <div className="flex flex-col gap-4">
        {state.error === "" ? null : (
          <ErrorNotice
            sentence="Could not read your routing policy."
            next="Your work still routes the way you set it -- this page could not read the setting, not change it."
            detail={state.error}
          />
        )}

        {state.saveError === "" ? null : (
          <ErrorNotice
            sentence="The policy was not saved."
            next="What is on screen is your edit, not what the cluster holds."
            detail={state.saveError}
          />
        )}

        {policy === null ? (
          <Callout tone="neutral" title="You have no routing policy, and that is a normal state">
            Work goes to the first machine that fits, in registration order, and moves on to the
            next one if that machine refuses before starting. Saving below writes a policy that
            replaces that behaviour for every call dispatched on your behalf.
          </Callout>
        ) : null}

        <FormRow>
          <Field label="Strategy" hint={STRATEGY_BLURB[strategy as RoutingStrategy] ?? ""}>
            <Select value={strategy} onChange={setStrategy} ariaLabel="Strategy">
              {ROUTING_STRATEGIES.map((one) => (
                <option key={one} value={one}>
                  {one}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="When the chosen machine refuses"
            hint={FALLBACK_BLURB[fallback as RoutingFallback] ?? ""}
          >
            <Select value={fallback} onChange={setFallback} ariaLabel="Fallback">
              {ROUTING_FALLBACKS.map((one) => (
                <option key={one} value={one}>
                  {one}
                </option>
              ))}
            </Select>
          </Field>
        </FormRow>

        {chipError === "" ? null : (
          <p role="alert" className="text-xs text-danger">
            {chipError}
          </p>
        )}

        <div>
          <p className="text-xs font-medium text-muted">Required labels</p>
          <p className="mt-0.5 mb-1.5 max-w-prose text-xs text-subtle">
            A machine must carry every one of these to be a candidate at all, on top of whatever
            the calling agent already asked for. This narrows and never widens -- a policy cannot
            make a machine eligible for work the agent did not ask to run there.
          </p>
          <LabelChips
            labels={requireChips}
            busy={state.saving}
            onAdd={(text) => addChip(text, setRequireChips)}
            onRemove={(text) => setRequireChips((current) => current.filter((one) => one !== text))}
          />
        </div>

        <div>
          <p className="text-xs font-medium text-muted">Preferred labels</p>
          <p className="mt-0.5 mb-1.5 max-w-prose text-xs text-subtle">
            An ordering hint, not a filter. Under labelMatch the candidates matching more of
            these sort first; under the other strategies they break ties.
          </p>
          <LabelChips
            labels={preferChips}
            busy={state.saving}
            onAdd={(text) => addChip(text, setPreferChips)}
            onRemove={(text) => setPreferChips((current) => current.filter((one) => one !== text))}
          />
        </div>

        <div className="flex items-center gap-3">
          <Button tone="primary" busy={state.saving} busyLabel="Saving…" onClick={save}>
            {policy === null ? "Create routing policy" : "Save routing policy"}
          </Button>
          <span role="status" className="text-xs text-muted">
            {state.announcement}
          </span>
        </div>
      </div>
    </Panel>
  );
}
