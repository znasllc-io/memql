import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { Badge, Band, Button, ButtonLink, Callout, DataText, EmptyState, Panel, Skeleton } from "../ui";
import { ExternalLink, KeyRound } from "../ui/icons";
import { formatDay } from "./MeLayout";
import {
  IDENTITY_DEVICES,
  IDENTITY_EXPORT,
  IDENTITY_SETTINGS,
  IDENTITY_TOKENS,
  identityPath,
} from "./urls";
import type { MeState } from "./useMe";

// /me/security -- how this account can be entered, and what to do about it
// (memql#4318 for the passkey summary and the links, memql#4319 for the
// switch).
//
// # The switch is the one control here that WRITES
//
// passkey_only disables sign-in LINKS: a request for one writes no row, sends
// no link, and mails a notice instead -- informative to the account holder,
// useless to anyone else reading the same mailbox. It is the remedy the
// shared-mailbox note on the Account tab points at.
//
// It is disabled with zero passkeys enrolled, AND the server refuses the
// change independently (component/identity/adminops/self_signin_policy.go).
// Both, because a disabled control is a suggestion: turning this on without a
// passkey locks a person out of their own account, as far as they can tell
// permanently.
//
// A REFUSAL SURFACES, never a silent no-op. The server's own sentence is
// rendered verbatim -- it distinguishes "add a passkey first" (a fact about
// the account) from "we could not check just now" (a fact about the moment),
// and those ask the reader for different next steps.
//
// # Everything else here is a LINK
//
// Enrolling, renaming and revoking a passkey; minting a personal access
// token; exporting or deleting the account. Those are identity's pages and
// they stay there -- the documented split. The portal names them and says
// where they are, which is the difference between a capability that moved and
// one a reader concludes is gone.

export function SecurityTab({ me }: { me: MeState }): ReactNode {
  const { config } = useAuth();
  const link = (path: string): string => identityPath(config.identityUrl, path);

  const passkeyOnly = me.account?.signInPolicy === "passkey_only";
  const activePasskeys = me.passkeys.filter((key) => key.id !== "").length;
  // FAIL CLOSED, matching the server: an unreadable list must not enable a
  // control whose precondition it could not check.
  const canGoPasskeyOnly = me.passkeyCountKnown && activePasskeys > 0;

  return (
    <div className="flex flex-col gap-6">
      {me.policyError === "" ? null : (
        <Callout tone="danger" title="That change was refused">
          {me.policyError}
        </Callout>
      )}

      <Band title="Sign-in links">
        <div className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-3">
          <p className="max-w-prose text-sm text-muted">
            {passkeyOnly
              ? "Sign-in links are off for this account. Requests for one write nothing and send nothing -- the address is mailed a notice instead. Your passkey is how you get in."
              : "Anyone who can read your inbox can request a sign-in link and enter this account. Turning links off leaves your passkey as the only way in."}
          </p>

          {me.account === null ? (
            <Skeleton variant="text" width="w-56" />
          ) : (
            <div className="flex flex-wrap items-center gap-3">
              {/* A COMMAND, not a toggle. Its label says what pressing it
                  will DO, and the Badge beside it says what is true now --
                  so `aria-pressed` on top of that would announce "Turn
                  sign-in links back on, pressed", which is a contradiction.
                  A toggle needs a stable label; this one is clearer with a
                  changing one. */}
              <Button
                tone={passkeyOnly ? "quiet" : "primary"}
                busy={me.policyBusy}
                busyLabel="Saving"
                disabled={!passkeyOnly && !canGoPasskeyOnly}
                onClick={() => me.setSignInPolicy(passkeyOnly ? "any" : "passkey_only")}
              >
                {passkeyOnly ? "Turn sign-in links back on" : "Turn sign-in links off"}
              </Button>
              <Badge tone={passkeyOnly ? "ok" : "neutral"}>
                {passkeyOnly ? "Passkey only" : "Links and passkeys"}
              </Badge>
            </div>
          )}

          {passkeyOnly || canGoPasskeyOnly || me.account === null ? null : (
            <p className="max-w-prose text-sm text-muted">
              {me.passkeyCountKnown
                ? "Add a passkey first. Turning off sign-in links with no passkey enrolled would leave you unable to sign in at all."
                : "We could not check your passkeys just now, so this is held back rather than offered blind."}
            </p>
          )}
        </div>
      </Band>

      <Band
        title="Passkeys"
        meta={me.passkeyCountKnown ? `${activePasskeys} enrolled` : "count unavailable"}
        panel
      >
        {me.loading && me.passkeys.length === 0 ? (
          <Skeleton variant="rows" rows={2} />
        ) : me.passkeys.length === 0 ? (
          <div className="p-3">
            <EmptyState
              icon={<KeyRound size={20} aria-hidden="true" />}
              statement="No passkeys are enrolled on this account. A passkey is what lets you sign in without a link -- and what makes turning links off safe."
              {...(link(IDENTITY_DEVICES) === ""
                ? {}
                : {
                    action: (
                      <ButtonLink
                        href={link(IDENTITY_DEVICES)}
                        target="_blank"
                        rel="noreferrer noopener"
                      >
                        Add a passkey
                        <ExternalLink size={14} aria-hidden="true" />
                      </ButtonLink>
                    ),
                  })}
            />
          </div>
        ) : (
          <ul className="divide-y divide-line">
            {me.passkeys.map((key) => (
              <li key={key.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-3 py-2">
                <span className="text-sm">{key.label}</span>
                <Badge tone={key.backedUp ? "ok" : "neutral"}>
                  {key.backedUp ? "Recoverable" : "This device only"}
                </Badge>
                <span className="ml-auto text-sm">
                  <DataText kind="time">added {formatDay(key.addedAt)}</DataText>
                </span>
              </li>
            ))}
          </ul>
        )}
      </Band>

      <Band title="Managed on identity">
        <div className="flex flex-col gap-3">
          <p className="max-w-prose text-sm text-muted">
            These are self-service pages the identity service owns. This console links to them
            rather than duplicating them -- one implementation, one place a change actually
            happens.
          </p>
          <div className="flex flex-wrap gap-2">
            <IdentityLink href={link(IDENTITY_DEVICES)}>Manage passkeys</IdentityLink>
            <IdentityLink href={link(IDENTITY_TOKENS)}>Personal access tokens</IdentityLink>
            <IdentityLink href={link(IDENTITY_SETTINGS)}>Account settings</IdentityLink>
            <IdentityLink href={link(IDENTITY_EXPORT)}>Export or delete your data</IdentityLink>
          </div>
          {link(IDENTITY_DEVICES) === "" ? (
            <Panel>
              <p className="max-w-prose text-sm text-muted">
                This cluster has no identity origin configured, so there is nothing to link to.
                That is the auth-disabled case: every connection is admitted as the local-dev
                cluster owner and there are no credentials to manage.
              </p>
            </Panel>
          ) : null}
        </div>
      </Band>
    </div>
  );
}

// A link is rendered only when there is somewhere for it to go. A dead link
// is worse than an absent one: the reader concludes the capability is broken
// rather than that it is not configured here.
function IdentityLink({ href, children }: { href: string; children: ReactNode }): ReactNode {
  if (href === "") return null;
  return (
    <ButtonLink href={href} target="_blank" rel="noreferrer noopener">
      {children}
      <ExternalLink size={14} aria-hidden="true" />
    </ButtonLink>
  );
}
