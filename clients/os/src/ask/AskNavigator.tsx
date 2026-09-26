import { useEffect, useRef, useSyncExternalStore } from "react";
import { useOs } from "../chrome/state";
import { useAsk } from "./AskProvider";
import { navigationTarget } from "../system/navigation";

/** Uses app-owned intents, never DOM clicks or generated response text. */
export function AskNavigator() {
  const { conversation } = useAsk();
  const activity = useSyncExternalStore(conversation.subscribe, () => conversation.getSnapshot().activity);
  const { actions, registry } = useOs();
  const shown = useRef("");
  useEffect(() => {
    if (!activity?.navigate || !activity.app || shown.current === activity.id) return;
    shown.current = activity.id;
    const target = navigationTarget(registry, activity.app, activity.arguments ?? {});
    if (!target) { conversation.navigationFailed("That destination is unavailable in this version or for your permissions."); return; }
    actions.openApp(target.app, target.section, target.payload);
  }, [activity, actions, registry, conversation]);
  return null;
}
