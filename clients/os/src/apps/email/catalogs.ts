import { createContext, useContext, useEffect, useId } from "react";
import { LiveReadVisibility } from "../../live/useGraphRevision";

export type EmailCatalog = "campaigns" | "audiences" | "templates" | "senders" | "rules" | "journeys";
export const EMAIL_CATALOGS: readonly EmailCatalog[] = ["campaigns", "audiences", "templates", "senders", "rules", "journeys"];
export const EmailCatalogDemand = createContext<(id: string, catalogs: readonly EmailCatalog[]) => void>(() => {});

/** Complete pickers and aggregate screens request their catalogs explicitly.
 * Directory pages use independent ten-row reads and never imply completeness. */
export function useEmailCatalogs(catalogs: readonly EmailCatalog[], enabled = true) {
  const request = useContext(EmailCatalogDemand);
  const visible = useContext(LiveReadVisibility);
  const id = useId();
  const key = enabled && visible ? [...catalogs].sort().join(",") : "";
  useEffect(() => {
    request(id, key ? key.split(",") as EmailCatalog[] : []);
    return () => request(id, []);
  }, [id, key, request]);
}
