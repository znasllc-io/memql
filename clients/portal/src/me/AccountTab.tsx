import { Link } from "react-router-dom";
import type { ReactNode } from "react";

import { Band, Callout, DataText, ErrorNotice, Panel, Skeleton } from "../ui";
import { formatDay, formatMoment } from "./MeLayout";
import { mePath } from "./urls";
import type { MeState } from "./useMe";

// /me -- the facts about the account (memql#4318), plus the shared-mailbox
// note (memql#4319).
//
// # It renders; it does not edit, and it no longer links either
//
// Changing a display name or an email is identity's job and stays there
// (docs/public/operate/portal.md). This tab shows what the cluster resolved.
//
// The "Edit on identity" link that used to sit at the bottom of this tab has
// MOVED to /me/settings, into its "Identity and data" band (memql#4523, one
// door per destination -- memql#4264). It was here because this was the only
// facet with anywhere to put it; now that a settings surface exists, an
// identity link-out belongs beside the other settings rather than under the
// facts. Nothing was removed: the same destination is one tab away, named
// together with the export and deletion links that share its origin.
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
  if (me.error !== "") {
    return (
      <ErrorNotice
        sentence="Could not read your account."
        next="Reload the page to read it again."
        detail={me.error}
      />
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
