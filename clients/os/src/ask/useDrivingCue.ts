import { useEffect, useState, useSyncExternalStore } from "react";
import { useAsk } from "./AskProvider";

/** One quiet cue, held long enough to see and restarted for a new operation.
 * Unrelated tool completions cannot erase it before the browser paints. */
export function useDrivingCue(app: string) {
 const { conversation } = useAsk();
 const activity = useSyncExternalStore(conversation.subscribe, () => conversation.getSnapshot().activity);
 const [id, setId] = useState("");
 useEffect(() => {
  if (!activity?.navigate || activity.app !== app) { setId(""); return; }
  setId(activity.id);
  const timer = setTimeout(() => setId(""), activity.phase === "running" ? 10000 : 2200);
  return () => clearTimeout(timer);
 }, [activity?.id, activity?.phase, activity?.app, app]);
 return id && activity?.id === id ? activity : null;
}
