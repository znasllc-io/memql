import { Link } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { Band, ButtonLink, Callout, DataText, Panel, Skeleton } from "../ui";
import { ExternalLink } from "../ui/icons";
import { formatDay, formatMoment } from "./MeLayout";
import { IDENTITY_SETTINGS, identityPath, mePath } from "./urls";
import type { MeState } from "./useMe";

// /me -- the facts about the account (memql#4318), plus the shared-mailbox
// note (memql#4319).
//
// # It renders; it does not edit
//
// Changing a display name or an email is identity's job and stays there
// (docs/public/operate/portal.md). The portal shows what the cluster resolved
// and links to the page that changes it. That is the documented split, and
// the link is composed from the CONFIGURED identity origin -- never a literal
// host, which would send an operator to somebody else's cluster to edit their
// own name.
//
// # The shared-mailbox note
//
// `sharedMailbox` gates nothing. It is a hint set by a local-part heuristic
// at registration, and its entire job is to put a fact in front of the
// account holder that is otherwise invisible: anybody who can read the
// mailbox can request a sign-in link and enter the account, so the account's
// sign-in surface is the mailbox's reader list. The note therefore points at
// the remedy rather than just stating the risk -- the Security tab, where
// sign-in links can be turned off.

export function AccountTab({ me }: { me: MeState }): ReactNode {
  const { config } = useAuth();
  const settingsUrl = identityPath(config.identityUrl, IDENTITY_SETTINGS);

  if (me.error !== "") {
    return (
      <Callout tone="danger" title="We could not read your account">
        {me.error}
      </Callout>
    );
  }
  if (me.account === null) {
    return (
      <Panel>
        <Skeleton variant="kv" rows={5} />
      </Panel>
    );
  }

  const account = me.account;
  return (
    <div className="flex flex-col gap-6">
      {account.sharedMailbox ? (
        <Callout tone="warn" title="This looks like a shared mailbox">
          Anyone who can read {account.primaryEmail || "this address"} can request a sign-in link
          and enter this account, so the account&rsquo;s sign-in surface is the mailbox&rsquo;s
          reader list. You can turn sign-in links off on the{" "}
          <Link to={mePath("security")} className="underline">
            Security tab
          </Link>
          .
        </Callout>
      ) : null}

      <Band title="Account" panel>
        <dl className="grid grid-cols-1 gap-x-6 gap-y-3 p-3 sm:grid-cols-[max-content_1fr]">
          <Fact label="Display name" value={account.displayName || "Not set"} />
          <Fact label="Email" value={account.primaryEmail || "Not set"} />
          <Fact label="Cluster role" value={account.role || "Not set"} />
          <Fact label="Member since" value={formatDay(account.memberSince)} kind="time" />
          <Fact label="Last seen" value={formatMoment(account.lastSeenAt)} kind="time" />
        </dl>
      </Band>

      {settingsUrl === "" ? null : (
        <div>
          <ButtonLink href={settingsUrl} target="_blank" rel="noreferrer noopener">
            Edit on identity
            <ExternalLink size={14} aria-hidden="true" />
          </ButtonLink>
          <p className="mt-2 max-w-prose text-sm text-muted">
            Your name and email are changed on the identity service, which owns them. This console
            reads them.
          </p>
        </div>
      )}
    </div>
  );
}

function Fact({
  label,
  value,
  kind = "string",
}: {
  label: string;
  value: string;
  kind?: "string" | "time";
}): ReactNode {
  return (
    <>
      <dt className="text-sm text-muted">{label}</dt>
      <dd className="min-w-0 text-sm">
        <DataText kind={kind}>{value}</DataText>
      </dd>
    </>
  );
}
