import { profileInitial, profileName, useMyProfile } from "../modules/profile/myProfile";
import { useSessionIfPresent } from "./access";

/** The same account identity in the dock, conversations and the Profile:
 *  the person's photo when they have one, their initial otherwise. */
export function AccountAvatar({ className = "" }: { className?: string }) {
  const session = useSessionIfPresent();
  const { profile } = useMyProfile();
  const name = profile ? profileName(profile) : session?.access?.primaryEmail || "";
  const classes = className ? `os-avatar ${className}` : "os-avatar";
  // Decorative here: every place that draws it names the person beside it or
  // on the control around it.
  if (profile?.avatar) return <img className={`${classes} os-avatar-photo`} src={profile.avatar} alt="" aria-hidden draggable={false} />;
  return <span className={classes} aria-hidden>{profileInitial(name)}</span>;
}
