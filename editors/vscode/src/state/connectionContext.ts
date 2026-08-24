// The two facts every view keys off: is a cluster selected, and is it up.
//
// Before this file each view answered both questions for itself, and answered
// them differently. Constructs asked whether the dispatcher existed and drew a
// synthetic "Not connected" row; Data listed whatever `listConcepts()` returned
// over a dead connection; Deployments ignored the question entirely and showed
// every registered instance whatever was selected; Runs listed workspace files.
// Four answers to two questions, and no `when` clause could reach any of them,
// because nothing in the extension ever called `setContext` -- so a
// `viewsWelcome` entry had nothing to be keyed on and none existed.
//
// THIS IS THE ONLY MAPPING. `ConnectionManager` publishes it (it is the one
// object that knows when the answer changes) and every provider is injected
// with a reader of it. A view that recomputed either answer would be a fifth
// opinion, and the failure that produces is silent: a provider that returns a
// row when the manifest's welcome says it should be empty suppresses the
// welcome, because VS Code renders welcome content ONLY over a genuinely empty
// tree.
//
// WHAT "SELECTED" MEANS HERE, precisely: this editor has a cluster in hand --
// it is dialing one, holds one, or tried and failed. It is NOT "clusters.yaml
// names a selectedCluster". A registry entry with no dial behind it is a name
// in a file, and the views keyed on this render CLUSTER DATA: a fresh window
// with a remembered selection and no connection has nothing to draw, and
// "Not connected" is the true thing to say about it.
//
// WHICH IS WHY `error` COUNTS AS SELECTED. A cluster that was chosen and did
// not answer is not an empty state -- it is a fact about something, and the
// views carry it in their own row-level and description-level affordances
// (design D2). Folding it in with `disconnected` would replace a cluster that
// is down with a screen saying nothing is chosen, which is the one reading that
// sends an operator to the wrong place.
//
// The `ConnectionState` import is TYPE-ONLY and the cycle it appears to make
// with connection/manager.ts is erased at compile time -- there is no runtime
// import in this direction.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4424 #4423

import type { ConnectionState } from "../connection/manager.js";

/**
 * The context keys' names, as the manifest spells them.
 *
 * Constants rather than literals at the call sites because there are three
 * places they have to agree -- the `setContext` calls, the manifest's `when`
 * clauses, and the tests that assert the two match. A typo in any one of them
 * is invisible: VS Code treats an unknown context key as unset, so a
 * `when: !memql.clusterSelcted` is permanently true and its welcome renders
 * over a connected cluster with nothing in the build complaining.
 */
export const CLUSTER_SELECTED_KEY = "memql.clusterSelected";
export const CONNECTED_KEY = "memql.connected";

export interface ConnectionContextKeys {
  /** A cluster is in hand: dialing, held, or tried and refused. */
  clusterSelected: boolean;
  /** That cluster's transport is up right now. */
  connected: boolean;
}

/**
 * What a view is injected with, so it can be driven without a live manager.
 *
 * A thunk rather than a value for the reason every other collaborator in this
 * extension is one: the connection changes without a view being told, and a
 * value captured at activation would leave a provider permanently answering
 * for the state the editor started in.
 */
export type ConnectionContextSource = () => ConnectionContextKeys;

/**
 * The mapping, whole. Four states in, two booleans out.
 *
 *   disconnected  ->  no cluster, no transport   -- the welcomes' state
 *   connecting    ->  a cluster, no transport    -- selected, not yet up
 *   connected     ->  a cluster and a transport
 *   error         ->  a cluster, no transport    -- selected and NOT answering
 *
 * `connecting` and `error` land on the same pair deliberately. The keys answer
 * what a SURFACE may draw, and the answer is the same for both: the view's
 * normal shape, with whatever it has. What separates a dial in flight from a
 * dial that failed is a message, and messages are the connection surface's
 * job -- putting the distinction in a context key would have every `when`
 * clause in the manifest re-deciding it.
 */
export function connectionContextKeys(state: ConnectionState): ConnectionContextKeys {
  return {
    clusterSelected: state.status !== "disconnected",
    connected: state.status === "connected",
  };
}

/**
 * The sentence a command says when it needs a cluster and there is not one.
 *
 * ONE STRING, because the surfaces that say it are unrelated to each other --
 * `memql.runs.execute` refuses with it (design D2's Runs exception) and the
 * welcomes open with it -- and two copies of a refusal are two refusals an
 * operator has to learn are the same one.
 */
export const NOT_CONNECTED_REFUSAL = "Not connected. Select a cluster first.";
