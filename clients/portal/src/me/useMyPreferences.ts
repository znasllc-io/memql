import { useCallback, useState } from "react";
import type { UpdateMyPreferencesArgs } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import type { MeState } from "./useMe";

// The Settings tab's two writes (memql#4523), and they are deliberately two.
//
// # updateMyPreferences never touches the kill switch
//
// `computerUseEnabled` is written ONLY by toggleComputerUseEnabled. That is not
// a convention this file upholds -- updateMyPreferences cannot express the key
// at all (memql#4522: it is not in the mutation's args and not in its template,
// and @mergeFields("preferences") preserves what a call does not carry). Two
// functions here because they are two different decisions with two different
// consequences, and the confirm belongs to only one of them.
//
// # A save sends ONE group, not the whole bag
//
// Every arg is optional and the server deep-merges, so a group's Save sends
// exactly its own fields. Nothing else is transmitted, which means a second tab
// open on the same account cannot have its unrelated edits overwritten by a
// stale draft this one happened to be holding.
//
// # Saving RE-READS
//
// Every write ends in me.reload() -- on success and on refusal alike. The form
// then shows the row rather than the position the click left the control in,
// which is the difference between a control that reports what the cluster holds
// and one that reports what the browser hoped. A refusal that left the select
// showing the rejected value would be the worst of the three outcomes.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface PreferenceWrites {
  // The group id currently in flight, or "" -- so each Save button shows its
  // own busy state instead of all of them flickering together.
  busyGroup: string;
  // Keyed by group id. The SERVER's sentence, never a paraphrase: the
  // mutation's refusals name the argument and the constraint it failed
  // ("value 9999 is greater than maximum 2500"), which tells a reader what to
  // change. "Something went wrong" does not.
  errors: Record<string, string>;
  save: (group: string, patch: UpdateMyPreferencesArgs) => void;
  setComputerUseEnabled: (enabled: boolean) => void;
}

export const COMPUTER_USE_GROUP = "computerUse";

export function useMyPreferences(me: MeState): PreferenceWrites {
  const { query } = useCluster();
  const [busyGroup, setBusyGroup] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});

  const finish = useCallback(
    (group: string, message: string) => {
      setErrors((prev) => ({ ...prev, [group]: message }));
      setBusyGroup("");
      me.reload();
    },
    [me],
  );

  const save = useCallback(
    (group: string, patch: UpdateMyPreferencesArgs) => {
      if (query === null) return;
      setBusyGroup(group);
      setErrors((prev) => ({ ...prev, [group]: "" }));
      void query
        .updateMyPreferences(patch)
        .then(() => finish(group, ""))
        .catch((err: unknown) => finish(group, describe(err)));
    },
    [query, finish],
  );

  const setComputerUseEnabled = useCallback(
    (enabled: boolean) => {
      if (query === null) return;
      setBusyGroup(COMPUTER_USE_GROUP);
      setErrors((prev) => ({ ...prev, [COMPUTER_USE_GROUP]: "" }));
      void query
        .toggleComputerUseEnabled({ enabled })
        .then(() => finish(COMPUTER_USE_GROUP, ""))
        .catch((err: unknown) => finish(COMPUTER_USE_GROUP, describe(err)));
    },
    [query, finish],
  );

  return { busyGroup, errors, save, setComputerUseEnabled };
}
