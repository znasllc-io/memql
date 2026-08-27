import type { ProfileAccess } from "./access";

// Profile is a module. Data only: MyAccess fields. No portal chrome,
// no IdentityBadge, no rail, no header, no sign-out (memql#4706).

export function Profile({ access }: { access: ProfileAccess | null }) {
  return (
    <article className="os-profile" data-os-module="profile">
      <h2 className="os-profile-title">Profile</h2>
      {access ? (
        <dl className="os-profile-fields">
          <div>
            <dt>Email</dt>
            <dd>{access.primaryEmail}</dd>
          </div>
          <div>
            <dt>Role</dt>
            <dd>{access.clusterRole}</dd>
          </div>
          <div>
            <dt>User</dt>
            <dd>{access.userId}</dd>
          </div>
        </dl>
      ) : (
        <p className="os-empty">Identity unresolved</p>
      )}
    </article>
  );
}
