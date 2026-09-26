import { useEffect, useRef, useSyncExternalStore } from "react";
import { useOs } from "../chrome/state";
import { useAsk } from "./AskProvider";
import { appById, accessAdmits } from "../system/registry";

/** Navigation consumes server execution events. It never clicks a DOM element
 * to repeat an already-executed mutation or infers intent from generated text. */
export function AskNavigator() {
  const { conversation } = useAsk();
  const activity = useSyncExternalStore(conversation.subscribe, () => conversation.getSnapshot().activity);
  const { actions, registry } = useOs();
  const shown = useRef("");
  useEffect(() => {
    if (!activity?.navigate || !activity.app || shown.current === activity.id) return;
    const app = appById(registry, activity.app);
    if (!app || (app.requires && !accessAdmits(app.requires))) return;
    shown.current = activity.id;
    const args = activity.arguments ?? {};
    const section = typeof args.section === "string" ? args.section : activity.app === "users" ? (args.groupId ? "groups" : "people") : undefined;
    actions.openApp(activity.app, section, args);
  }, [activity, actions, registry]);
  return null;
}
