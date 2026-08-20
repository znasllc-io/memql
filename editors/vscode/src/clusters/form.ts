// The edit-cluster form's CREDENTIAL fields, as data (memql#4194).
//
// The add/edit flow used to PREFILL the access-token and refresh-token input
// boxes with the stored values. `password: true` masks the box, but the value
// is still live in the input -- one reveal toggle, one paste-elsewhere, one
// screen reader away from disclosure -- and prefilling serves nothing: the
// operator either wants to KEEP the credential (types nothing) or REPLACE it
// (types the new one). Neither needs the old value on screen.
//
// So the fields are described here, pure and testable: the input is ALWAYS
// empty, the prompt says what is stored, and an empty submission KEEPS the
// stored value instead of deleting it. Deleting a credential is sign-out's
// job, where the SecretStorage half is cleared too -- a cleared text box that
// silently left the 30-day refresh token in the keyring would be worse than
// the prefill it replaced.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go);
// src/extension.ts consumes these plans in its input boxes.

/** What one credential input box shows. `value` is the no-prefill invariant. */
export interface CredentialFieldPlan {
  prompt: string;
  /** Always the empty string: a stored credential is never placed in an input. */
  value: "";
}

/** The access-token field: what to show given whether a token is stored. */
export function tokenFieldPlan(existingToken: string | undefined): CredentialFieldPlan {
  const stored =
    existingToken !== undefined && existingToken.trim() !== ""
      ? "A token is stored for this cluster; leave empty to keep it, or paste a new one to replace it. Sign out to remove it. "
      : "";
  return {
    prompt:
      `Access token (optional): the identity-issued JWT from POST <identity>/oauth/token. ${stored}` +
      'Leave empty and run "MemQL: Sign In" to authenticate through your browser. A PAT (mql_pat_...) will not work -- the mesh verifies bearers via JWKS.',
    value: "",
  };
}

/** The refresh-token field's plan, same shape and same invariant. */
export function refreshTokenFieldPlan(existingRefreshToken: string | undefined): CredentialFieldPlan {
  const stored =
    existingRefreshToken !== undefined && existingRefreshToken.trim() !== ""
      ? "One is pending in clusters.yaml; leave empty to keep it. "
      : "";
  return {
    prompt:
      `Refresh token (optional): the refresh_token from the same response. ${stored}` +
      "Stored in the editor's secret storage and used to renew the access token as it expires.",
    value: "",
  };
}

/**
 * What an input-box answer means for a credential field.
 *
 *   undefined  the operator cancelled -- the caller aborts the whole form
 *   ""         keep whatever is stored (which may itself be nothing)
 *   text       replace with the trimmed text
 */
export function resolveCredentialInput(
  entered: string | undefined,
  existing: string | undefined,
): string | undefined {
  if (entered === undefined) return undefined;
  const typed = entered.trim();
  if (typed === "") return (existing ?? "").trim();
  return typed;
}
