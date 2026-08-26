import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import type { SynapseScope } from "./types";

// The scope registry: which sections on this page can be filled, and which of
// them the button is pointing at right now.
//
// ===========================================================================
// LAST FOCUSED OR HOVERED WINS, AND THE POPOVER SAYS SO IN WORDS
// ===========================================================================
// A floating button that acts on "the form" has to answer WHICH form, and
// every automatic answer is wrong sometimes. So the rule is the simplest one a
// person can predict from watching it -- the section you last touched -- and
// the popover names that section before you type, so the target is never a
// surprise. The section itself wears an accent ring and a small tag while it
// holds the scope, which is the same fact rendered where your eyes already
// are.
//
// A PAGE WITH NO REGISTERED SCOPE gets page-level actions only. The button
// does not pretend: there is nothing on screen it could fill, and offering to
// fill it anyway is how an affordance loses a person's trust permanently.

interface SynapseState {
  readonly scopes: readonly SynapseScope[];
  readonly activeId: string;
  readonly active: SynapseScope | undefined;
  readonly register: (scope: SynapseScope) => () => void;
  readonly touch: (id: string) => void;
}

const SynapseContext = createContext<SynapseState | null>(null);

export function SynapseProvider({ children }: { children: ReactNode }): ReactNode {
  // The registry lives in a REF and the render-visible copy in state. A
  // register during another component's render must not schedule a render of
  // this one mid-commit, and a form re-registering on every keystroke (its
  // `fields` carry the current values) must not re-render the whole subtree.
  const registry = useRef(new Map<string, SynapseScope>());
  const [version, setVersion] = useState(0);
  const [activeId, setActiveId] = useState("");

  const register = useCallback((scope: SynapseScope) => {
    const had = registry.current.get(scope.id);
    registry.current.set(scope.id, scope);
    // Only a scope ARRIVING or LEAVING changes what the button offers. A
    // scope updating its field values does not, and bumping the version for
    // that would re-render the button on every keystroke in every form.
    if (had === undefined) setVersion((v) => v + 1);
    return () => {
      registry.current.delete(scope.id);
      setVersion((v) => v + 1);
      setActiveId((current) => (current === scope.id ? "" : current));
    };
  }, []);

  const touch = useCallback((id: string) => {
    setActiveId((current) => (current === id ? current : id));
  }, []);

  const value = useMemo<SynapseState>(() => {
    const scopes = [...registry.current.values()];
    // With nothing touched yet, the FIRST registered scope is the active one
    // -- registration order is source order, so on a page with one form that
    // is the form, and a person who fires immediately gets what they expect.
    const resolvedId = activeId !== "" && registry.current.has(activeId)
      ? activeId
      : (scopes[0]?.id ?? "");
    return {
      scopes,
      activeId: resolvedId,
      active: resolvedId === "" ? undefined : registry.current.get(resolvedId),
      register,
      touch,
    };
    // `version` is the dependency that means "the set of scopes changed".
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeId, version, register, touch]);

  return <SynapseContext.Provider value={value}>{children}</SynapseContext.Provider>;
}

// useSynapse reads the registry. Returns null OUTSIDE a provider rather than
// throwing: Synapse is an affordance, and a component that can be rendered
// without the shell (a kit test, a page mounted in isolation) must not fail
// because an optional feature's context is missing.
export function useSynapse(): SynapseState | null {
  return useContext(SynapseContext);
}
