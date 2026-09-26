import { useEffect } from "react";
import { useOs } from "../../chrome/state";
import { identityEntry } from "../../auth/nativeIdentity";

export function IdentityOpenDispatcher() {
  const { actions } = useOs();
  useEffect(() => {
    const path = identityEntry()?.split("?")[0];
    if (!path?.startsWith("/me")) return;
    const section = path === "/me/tokens" ? "tokens" : path === "/me/settings" ? "settings" : path === "/me/devices" ? "devices" : "profile";
    actions.openApp("identity", section);
    window.history.replaceState({}, "", "/");
  }, [actions]);
  return null;
}
