import type { ReactNode } from "react";

import { useAdminAccess, useAdminWrites } from "../admin/useAdminConsole";
import { ErrorNotice } from "../ui";
import { InvitePerson, PendingInvitations } from "../people/InvitePerson";
import { usePendingInvitations } from "../people/usePendingInvitations";

// Inviting somebody, as a widget (epic memql#4661).
//
// This is the whole of what the Users view's hand-built layout carried beyond
// its elements: the invite button in the header and the pending list under the
// population. As a widget it is ONE thing in an arrangement, which is why the
// Users page no longer needs a body module.
//
// A WIDGET MAY RENDER NOTHING, and this one does for anybody who cannot
// administer the cluster. That is not an error state -- most people cannot --
// so it renders empty rather than saying "you may not do this", which would
// put a permission notice on a page that is otherwise about users.
export function InvitePersonWidget(): ReactNode {
  const { canAdminister } = useAdminAccess();
  const invitations = usePendingInvitations(canAdminister);
  const writes = useAdminWrites();

  if (!canAdminister) return null;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-start">
        <InvitePerson onInvited={invitations.reload} />
      </div>
      {invitations.error !== "" ? (
        <ErrorNotice
          sentence="Could not read the pending invitations."
          next="Anyone already invited is unaffected; reload the page to read them again."
          detail={invitations.error}
        />
      ) : (
        <PendingInvitations
          rows={invitations.rows}
          writes={writes}
          onChanged={invitations.reload}
        />
      )}
    </div>
  );
}
