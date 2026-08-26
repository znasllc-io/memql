import { useEffect, useMemo, useRef, type ReactNode } from "react";

import { useSynapse } from "./SynapseProvider";
import type { SynapseField, SynapsePatch } from "./types";

// A section Synapse can fill: the registration, the ring, and the wash.
//
// ONE COMPONENT rather than a hook plus a wrapper plus a tag, because every
// caller wants all three and a caller who forgot the ring would have a scope
// that works and shows no sign of being the target -- which is the one thing
// this affordance must never do.
//
// ===========================================================================
// THE WASH IS PER FIELD, IN ORDER, AND IT NEEDS ONE ATTRIBUTE FROM YOU
// ===========================================================================
// A form whose fields all changed at once looks like it reloaded. Washing
// each patched field in turn is what makes it read as "these three were
// filled in for you", which is the fact a person needs before they press
// submit. To find them, mark each control with
// `data-synapse-field="<the field name>"` -- the same name the scope
// declares. A field without the attribute is still PATCHED; it simply does
// not flash.

export function SynapseSection({
  id,
  label,
  fields,
  apply,
  className,
  children,
}: {
  id: string;
  label: string;
  fields: readonly SynapseField[];
  apply: (patches: readonly SynapsePatch[]) => void;
  className?: string;
  children: ReactNode;
}): ReactNode {
  const synapse = useSynapse();
  const host = useRef<HTMLDivElement>(null);

  // ===========================================================================
  // THE SCOPE OBJECT IS STABLE PER ID. The live values are read through refs.
  // ===========================================================================
  // `fields` carries each field's CURRENT value, so it is a new array on every
  // keystroke. Registering that object directly made the effect re-run per
  // character -- and its cleanup unregisters, which cleared the active scope.
  // Found in the visual QA pass (memql#4660): the ring blinked off and back on
  // as you typed, and a fill's own status line was wiped by the re-render the
  // fill itself caused.
  //
  // So the registered object is memoized on id and label alone, and `fields`
  // is a GETTER over a ref -- the registry always reads what is on screen now,
  // and nothing re-registers until the section genuinely changes identity.
  const fieldsRef = useRef(fields);
  fieldsRef.current = fields;
  const applyRef = useRef(apply);
  applyRef.current = apply;

  const scope = useMemo(
    () => ({
      id,
      label,
      get fields() {
        return fieldsRef.current;
      },
      apply: (patches: readonly SynapsePatch[]) => {
        applyRef.current(patches);
        try {
          washFields(host.current, patches);
        } catch {
          // The patches are already applied. A highlight that cannot run is
          // not a failed fill, and reporting it as one would send somebody
          // looking for a problem in the wrong half of the feature.
        }
      },
    }),
    [id, label],
  );

  const register = synapse?.register;
  useEffect(() => {
    if (register === undefined) return;
    return register(scope);
  }, [register, scope]);

  const active = synapse?.activeId === id;

  return (
    <div
      ref={host}
      data-synapse-scope={id}
      data-synapse-active={active ? "true" : "false"}
      // FOCUS CAPTURE, not focus: the focus lands on an input deep inside, and
      // the capture phase is what lets the section hear it without every
      // control having to report upward.
      onFocusCapture={() => synapse?.touch(id)}
      onPointerEnter={() => synapse?.touch(id)}
      className={"synapse-scope rounded-lg" + (className === undefined ? "" : ` ${className}`)}
    >
      {active && synapse !== null ? (
        <span className="synapse-tag" aria-hidden="true">
          AI scope
        </span>
      ) : null}
      {children}
    </div>
  );
}

// The wash. Deliberately DOM-level: the alternative is every caller threading
// a "recently patched" set through its own render, which is a lot of
// plumbing for a 400ms highlight -- and would put the animation's bookkeeping
// in the form's state, where a re-render could lose it.
function washFields(host: HTMLElement | null, patches: readonly SynapsePatch[]): void {
  if (host === null) return;
  // Matched in JS rather than through an attribute-value SELECTOR, which
  // would need CSS.escape -- a global jsdom does not implement, and one
  // older browsers do not either. It threw here once, INSIDE the scope's
  // apply and after the patches had landed, so the fields filled and the
  // popover reported a failure. A highlight must not be able to do that.
  const targets = new Map<string, Element>();
  for (const element of host.querySelectorAll("[data-synapse-field]")) {
    const name = element.getAttribute("data-synapse-field");
    if (name !== null && !targets.has(name)) targets.set(name, element);
  }
  patches.forEach((patch, index) => {
    const target = targets.get(patch.field);
    if (target === undefined) return;
    // Staggered in the order the model returned them, so the eye follows the
    // fill rather than seeing a single flash.
    globalThis.setTimeout?.(() => {
      target.classList.add("synapse-washed");
      globalThis.setTimeout?.(() => target.classList.remove("synapse-washed"), 700);
    }, index * 90);
  });
}
