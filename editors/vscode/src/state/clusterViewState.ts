// What a cluster-backed view shows for each state of the connection.
//
// THE VIEWS SAY WHY THEY ARE EMPTY, AND OFFER THE ONE ACT. Constructs and Data
// draw rows only for a cluster this editor is talking to. For every other
// state the tree is EMPTY on purpose, so the manifest's `viewsWelcome` entry
// for that state renders over it: one short line and the button that fixes it
// ("Sign in to see constructs." [Sign In]). A row cannot do that job -- VS Code
// draws welcome content only over an empty tree, and a row carries no button.
//
// THE WELCOMES ARE KEYED ON `memql.connectionState`, which the connection
// layer publishes with exactly these six values. This mapping is the views'
// half of the same contract: a tree that drew a row where the manifest expects
// a welcome would hide the welcome, and a tree that went empty where no
// welcome matches would be a blank panel. test/clusterViewState.test.ts holds
// the manifest to these values.
//
//   none           no cluster in hand            welcome: Select / Add a cluster
//   connecting     dialing, or retrying a        no welcome: the view's progress bar
//                  dropped connection
//   connected      a live session                rows
//   signIn         credential missing, expired
//                  or refused                    welcome: Sign In
//   unreachable    the dial failed, or a dropped
//                  session stopped retrying      welcome: Retry
//   notConfigured  nothing to dial               welcome: Edit Cluster
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { ConnectionState } from "../connection/manager.js";
import {
  CONNECTION_STATE_KEY as PUBLISHED_KEY,
  connectionStateWord,
  type ConnectionStateWord,
} from "./connectionContext.js";

/** The context key the welcomes are keyed on. */
export const CONNECTION_STATE_KEY = PUBLISHED_KEY;

export type ClusterViewState = ConnectionStateWord;

/**
 * ONE MAPPING, NOT TWO. The views read the connection through the same
 * function the connection layer publishes the key with, so a view can never
 * go empty in a state no welcome is keyed on. A dropped connection that is
 * still retrying reads `connecting` in both: the view's progress bar, not a
 * welcome offering Retry for a retry already under way.
 */
export function clusterViewState(state: ConnectionState): ClusterViewState {
  return connectionStateWord(state);
}
