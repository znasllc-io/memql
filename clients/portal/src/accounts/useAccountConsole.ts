import { useCallback, useEffect, useMemo, useState } from "react";
import type { Role, Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { flattenForList } from "../viewkit/rows";
import {
  accountTokenViews,
  selectedAccount,
  type AccountTokenView,
  type SelectedAccount,
} from "./rows";
import {
  mintAccountToken,
  namedCall,
  newAccountId,
  revokeAccountToken,
  type AccountTokenMintResult,
} from "./wire";

// The customer-management console: account CRUD plus the per-account credential
// list, wired to the stream.
//
// ===========================================================================
// THE ISOLATION IS THE CLUSTER'S. THIS FILE ONLY ASKS.
// ===========================================================================
// v1:identity:account declares @rowAuthz(owner="ownerUserId"), so the engine
// ANDs ownerUserId == actor.userId into every read of it and refuses every
// update whose target row the caller does not own
// (component/memql/rowauthz_enforce.go, rowauthz_write_guard.go). Nothing here
// filters, and nothing here compares an owner id -- doing either would put a
// second copy of the tier in a browser, where it cannot be enforced and can
// only drift.
//
// The role check below is the same courtesy useDeployConsole documents at
// length: hiding an action a caller cannot take beats letting them click it and
// showing them a refusal. It is NOT the control. Every call below issues the
// same request whatever this file believes, and the cluster answers.
//
// ===========================================================================
// WHAT AN ACCOUNT TOKEN IS -- because this is the file that mints one
// ===========================================================================
// A credential issued TO THE OPERATOR, ON BEHALF OF an account. The subject is
// the operator's user; the account is a binding. Nothing authenticates as an
// account, and no surface admits an mql_acct_ bearer today. The UI copy says
// so where an operator will read it, and the reply carries subjectUserId so the
// client cannot claim otherwise. See
// docs/public/operate/auth/account-tokens.md.

// Roles that may write to the data plane at all. A reader is refused by the
// coarse capability gate before row-authz is ever consulted, so offering them
// an "Add customer" button would be offering a guaranteed failure.
const CAN_MANAGE: readonly Role[] = ["owner", "admin", "writer"];

export interface AccountDraft {
  name: string;
  description: string;
  primaryContactName: string;
  primaryContactEmail: string;
  externalRef: string;
}

export const EMPTY_DRAFT: AccountDraft = {
  name: "",
  description: "",
  primaryContactName: "",
  primaryContactEmail: "",
  externalRef: "",
};

export interface AccountConsoleState {
  role: Role;
  canManage: boolean;
  // The customer the operator has selected in the ledger, or null.
  selected: SelectedAccount | null;
  // The selected customer's credentials, revoked ones included.
  tokens: AccountTokenView[];
  tokensLoading: boolean;
  tokensError: string;
  busy: boolean;
  // Outcome of the last action, either way. One field for each because an
  // operator wants the most recent thing that happened, not a log.
  message: string;
  error: string;
  // THE ONE-TIME PLAINTEXT. Non-null only between a successful mint and the
  // operator dismissing it. Held in component state and nowhere else: not in
  // storage, not in a URL, not in the row list.
  minted: AccountTokenMintResult | null;
  dismissMinted: () => void;
  // Rows re-read after a write, or null before the first one.
  //
  // ViewPage owns the keyset walk that produced the page's population, and it
  // does not re-run on a write it knows nothing about -- so a freshly created
  // customer would not appear in the ledger until a reload. Rather than reach
  // up and invalidate someone else's walk, the console re-reads the first page
  // itself and the view prefers that copy once it exists. The honest cost is
  // stated in the view: the header's "N loaded" describes the original walk.
  refreshedRows: Row[] | null;
  create: (draft: AccountDraft) => void;
  update: (draft: AccountDraft) => void;
  archive: () => void;
  mint: (label: string) => void;
  revoke: (identityId: string) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useAccountConsole(
  rows: readonly Row[],
  selectedRowId: string,
): AccountConsoleState {
  const { query, dispatcher } = useCluster();
  const { access } = useMyAccess();
  const role: Role = access?.clusterRole ?? "";
  const canManage = CAN_MANAGE.includes(role);

  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [minted, setMinted] = useState<AccountTokenMintResult | null>(null);
  const [refreshedRows, setRefreshedRows] = useState<Row[] | null>(null);

  const [tokens, setTokens] = useState<AccountTokenView[]>([]);
  const [tokensLoading, setTokensLoading] = useState(false);
  const [tokensError, setTokensError] = useState("");
  // Bumped by every action that changes the credential set.
  const [tokenEpoch, setTokenEpoch] = useState(0);

  const pool = refreshedRows ?? rows;
  const selected = useMemo(
    () => selectedAccount(pool, selectedRowId),
    [pool, selectedRowId],
  );
  const selectedId = selected?.id ?? "";

  // The credential list for the selected customer. A SECOND population, on its
  // own read, exactly as the People view's sessions and the Agents view's
  // grants are -- so it settles independently and a failure here does not read
  // as "the customer list is broken".
  useEffect(() => {
    if (query === null || selectedId === "") {
      setTokens([]);
      setTokensLoading(false);
      setTokensError("");
      return;
    }
    let live = true;
    setTokensLoading(true);
    setTokensError("");

    void query
      .executeNamed(
        "accountTokensForAccount",
        namedCall("query", "accountTokensForAccount", { accountId: selectedId }),
      )
      .then((result) => {
        if (live) setTokens(accountTokenViews(result.rows()));
      })
      .catch((err: unknown) => {
        if (live) setTokensError(describe(err));
      })
      .finally(() => {
        if (live) setTokensLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, selectedId, tokenEpoch]);

  // Re-read the first page of the caller's customers after a write. Deliberately
  // the SAME named query the page's walk uses, so the two cannot disagree about
  // what a customer row is; deliberately only the first page, because a write
  // lands at the top of a createdAt-descending list.
  const refetchAccounts = useCallback(() => {
    if (query === null) return;
    void query
      .executeNamed("accounts", namedCall("query", "accounts", {}))
      .then((result) => setRefreshedRows(result.rows().map(flattenForList)))
      .catch(() => {
        // A failed re-read is not a failed write. Leave whatever the view is
        // already showing rather than replacing a correct ledger with an
        // error the operator cannot act on -- the action's own outcome is
        // reported separately.
      });
  }, [query]);

  // run funnels every action through one place so the busy flag, the message
  // handling and the follow-up re-read cannot be forgotten on the fifth one.
  const run = useCallback(
    (what: (() => Promise<string>) | null, after: () => void) => {
      if (what === null) return;
      setBusy(true);
      setMessage("");
      setError("");
      void what()
        .then((note) => {
          setMessage(note);
          after();
        })
        .catch((err: unknown) => setError(describe(err)))
        .finally(() => setBusy(false));
    },
    [],
  );

  const create = useCallback(
    (draft: AccountDraft) => {
      if (query === null) return;
      const name = draft.name.trim();
      if (name === "") {
        setError("A customer needs a name.");
        return;
      }
      const accountId = newAccountId();
      run(
        () =>
          query
            .executeNamed(
              "createAccount",
              namedCall("mutation", "createAccount", {
                accountId,
                name,
                description: draft.description.trim(),
                primaryContactName: draft.primaryContactName.trim(),
                primaryContactEmail: draft.primaryContactEmail.trim(),
                externalRef: draft.externalRef.trim(),
              }),
            )
            .then(() => `Added ${name}.`),
        refetchAccounts,
      );
    },
    [query, run, refetchAccounts],
  );

  const update = useCallback(
    (draft: AccountDraft) => {
      if (query === null || selectedId === "") return;
      run(
        () =>
          query
            .executeNamed(
              "updateAccount",
              // Empty fields are DROPPED by namedCall rather than sent as "".
              // updateAccount is a partial read-merge, so an omitted field
              // keeps its value while an empty string would blank it.
              namedCall("mutation", "updateAccount", {
                accountId: selectedId,
                name: draft.name.trim(),
                description: draft.description.trim(),
                primaryContactName: draft.primaryContactName.trim(),
                primaryContactEmail: draft.primaryContactEmail.trim(),
                externalRef: draft.externalRef.trim(),
              }),
            )
            .then(() => "Saved."),
        refetchAccounts,
      );
    },
    [query, selectedId, run, refetchAccounts],
  );

  const archive = useCallback(() => {
    if (query === null || selectedId === "") return;
    run(
      () =>
        query
          .executeNamed(
            "archiveAccount",
            namedCall("mutation", "archiveAccount", { accountId: selectedId }),
          )
          .then(
            () =>
              "Archived. The record is kept in full -- memQL has no hard delete.",
          ),
      refetchAccounts,
    );
  }, [query, selectedId, run, refetchAccounts]);

  const mint = useCallback(
    (label: string) => {
      if (dispatcher === null || selectedId === "") return;
      const trimmed = label.trim();
      if (trimmed === "") {
        setError("Name the credential after the system that will hold it.");
        return;
      }
      run(
        () =>
          mintAccountToken(dispatcher, { accountId: selectedId, label: trimmed }).then(
            (result) => {
              if (!result.success) {
                throw new Error(
                  result.errorMessage || result.errorCode || "the cluster refused the mint",
                );
              }
              setMinted(result);
              return "Issued. Copy the credential now -- it is not shown again.";
            },
          ),
        () => setTokenEpoch((n) => n + 1),
      );
    },
    [dispatcher, selectedId, run],
  );

  const revoke = useCallback(
    (identityId: string) => {
      if (dispatcher === null || identityId === "") return;
      run(
        () =>
          revokeAccountToken(dispatcher, identityId).then((result) => {
            if (!result.success) {
              throw new Error(
                result.errorMessage || result.errorCode || "the cluster refused the revoke",
              );
            }
            return "Revoked.";
          }),
        () => setTokenEpoch((n) => n + 1),
      );
    },
    [dispatcher, run],
  );

  const dismissMinted = useCallback(() => setMinted(null), []);

  return {
    role,
    canManage,
    selected,
    tokens,
    tokensLoading,
    tokensError,
    busy,
    message,
    error,
    minted,
    dismissMinted,
    refreshedRows,
    create,
    update,
    archive,
    mint,
    revoke,
  };
}
