import { useMemo } from "react";
import { ScrollText } from "lucide-react";

import { useOs, type OsActions } from "../chrome/state";
import { Button } from "../kit";
import { canOpen } from "../system/registry";

// The deep link into the Logs app (epic memql#4895, spec H "Deep links"):
// a quiet "Logs" action that opens Search narrowed to one subject.

export const LOGS_APP_ID = "logs";

export interface LogsSubject {
  subject: string;
  subjectConcept: string;
}

/**
 * Open the Logs app on Search, carrying the subject as a window intent.
 *
 * The Settings apps index's shape: open-by-id first, which focuses an
 * existing window rather than opening a second one and carries the
 * `canOpen` admission check; then, for the focus-existing case, navigate --
 * a fresh window was already created on the right section by the call.
 */
export function openLogsOn(
  actions: Pick<OsActions, "openApp" | "navigateSection">,
  payload: LogsSubject,
): void {
  const effect = actions.openApp(LOGS_APP_ID, "search", { ...payload });
  if (effect.kind === "focused-existing") actions.navigateSection(effect.windowId, "search");
}

export function OpenLogsButton({
  subject,
  subjectConcept,
  ariaLabel,
}: LogsSubject & {
  /** Names what the logs are OF: "Logs for shop.example.com". */
  ariaLabel: string;
}) {
  const { actions, registry, accessEpoch } = useOs();
  // `accessEpoch` in the deps for the launcher's reason (memql#4857): canOpen
  // reads the effective capability set out of band, so a memo without it
  // would keep the pre-read refusal after the set lands. The question is the
  // one the launcher asks -- may this person open the Logs app -- so the
  // button and the app can never disagree about it.
  const admitted = useMemo(
    () => canOpen(registry, LOGS_APP_ID),
    [registry, accessEpoch],
  );
  if (!admitted || subject.trim() === "") return null;
  return (
    <Button onClick={() => openLogsOn(actions, { subject, subjectConcept })} ariaLabel={ariaLabel}>
      <ScrollText size={13} aria-hidden /> Logs
    </Button>
  );
}
