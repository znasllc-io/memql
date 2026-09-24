import { useCallback, useState } from "react";
import type { DotTone } from "../kit";
import type { AppSetupState } from "../system/registry";
import { useSession } from "./access";

/** Personal app setup never leaks across apps or signed-in identities. */
export function useAppSetupMark(appId: string, fallback: DotTone | null) {
  const { access } = useSession();
  const boundary = JSON.stringify([appId, access?.userId ?? ""]);
  const [reported, setReported] = useState<{ boundary: string; state: AppSetupState } | null>(null);
  const reportSetupState = useCallback((state: AppSetupState) => {
    setReported(held => held?.boundary === boundary && held.state === state ? held : { boundary, state });
  }, [boundary]);
  const settingsTone: DotTone | null = fallback ?? (reported?.boundary === boundary && reported.state === "partial" ? "partlySetUp" : null);
  return { settingsTone, reportSetupState };
}
