import { useCallback, useMemo, useState } from "react";
import {
  IdentityAdminClient,
  type ClusterSettingsEdit,
} from "@znasllc-io/memql-sdk-core/identityadmin";

import { useOsConnection } from "../../live/connection";
import { describeRefusal, type ActionRefusal } from "../users/actions";

// The writes Settings makes against identity (epic memql#4984): revoking a
// credential, and editing the cluster's runtime settings.
//
// ===========================================================================
// THROUGH A GO GATE, NOT A MUTATION
// ===========================================================================
// A MemQL mutation cannot carry a role predicate -- `filter` is a read
// construct. So `revokePATIdentity` and `updateClusterSettings` name an
// arbitrary target under a coarse write check that admits every role from
// `writer` up, and calling either from a browser would hand a writer what an
// admin console reserves for an admin. component/identity/adminops holds the
// owner/admin rule and the audit write; component/grpc bridges it onto the
// stream as IdentityAdminMsg.
//
// So this module contains no mutation string, and nothing here checks a role.
// adminops refuses every one of these below owner/admin against the role the
// stream interceptor VERIFIED -- not against anything this file or its callers
// believe. The sections hide controls their operator cannot use because
// showing a button that always fails teaches nobody who can; that is
// presentation, and editing a boolean in a browser changes nothing about the
// answer.
//
// `describeRefusal` and `ActionRefusal` come from the Users app rather than
// being respelled here. They are the same shape over the same client, and a
// second copy is one that can disagree about what `auditEventId` means.

export interface SettingsWrites {
  /** The id currently being written, or "". One at a time, on purpose: these
   *  are non-idempotent from the person's side. */
  busyKey: string;
  /** The last refusal, or null. Cleared when a write starts, so it is never
   *  read as belonging to the current attempt. */
  refusal: ActionRefusal | null;
  /** What just succeeded, in this surface's voice. Cleared the same way. */
  done: string;
  clear: () => void;

  revokeToken: (identityId: string, label: string) => Promise<boolean>;
  revokeNodeToken: (identityId: string, node: string) => Promise<boolean>;
  saveClusterSettings: (settings: ClusterSettingsEdit) => Promise<boolean>;
}

export function useSettingsWrites(): SettingsWrites {
  const connection = useOsConnection();
  const [busyKey, setBusyKey] = useState("");
  const [refusal, setRefusal] = useState<ActionRefusal | null>(null);
  const [done, setDone] = useState("");

  const client = useMemo(() => {
    const transport = connection?.dispatcher ?? null;
    return transport === null ? null : new IdentityAdminClient(transport);
  }, [connection]);

  const clear = useCallback(() => {
    setRefusal(null);
    setDone("");
  }, []);

  const run = useCallback(
    async (
      key: string,
      succeeded: string,
      write: (c: IdentityAdminClient) => Promise<unknown>,
    ): Promise<boolean> => {
      if (client === null) {
        setRefusal({
          detail: "Not connected to the cluster, so nothing was written.",
          auditEventId: "",
          denied: false,
        });
        return false;
      }
      setBusyKey(key);
      setRefusal(null);
      setDone("");
      try {
        await write(client);
        setDone(succeeded);
        return true;
      } catch (err: unknown) {
        setRefusal(describeRefusal(err));
        return false;
      } finally {
        setBusyKey("");
      }
    },
    [client],
  );

  const revokeToken = useCallback(
    (identityId: string, label: string) =>
      run(identityId, `Revoked ${label}. It will not authenticate again.`, (c) =>
        c.revokePersonalAccessToken(identityId),
      ),
    [run],
  );

  const revokeNodeToken = useCallback(
    (identityId: string, node: string) =>
      run(
        identityId,
        `Revoked the credential for ${node}. That node cannot rejoin until it is re-bootstrapped.`,
        (c) => c.revokeNodeToken(identityId),
      ),
    [run],
  );

  const saveClusterSettings = useCallback(
    (settings: ClusterSettingsEdit) =>
      run("cluster-settings", "Saved. New sessions and links use these values.", (c) =>
        c.updateClusterSettings(settings),
      ),
    [run],
  );

  return useMemo(
    () => ({ busyKey, refusal, done, clear, revokeToken, revokeNodeToken, saveClusterSettings }),
    [busyKey, refusal, done, clear, revokeToken, revokeNodeToken, saveClusterSettings],
  );
}
