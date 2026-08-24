import type { ReactNode } from "react";

import { Badge, type StatusTone } from "../ui";

// The origin badge (epic memql#4378).
//
// One pill, on a concept header and on a row's detail, saying what MemQL's
// relationship to this data is. Three states, no fourth:
//
//   Mirror of shopify   an external system owns it; MemQL's copy is
//                       READ-ONLY BY CONSTRUCTION -- the engine refuses
//                       every write to it that is not that system's
//                       connector's
//   Origin -> shopify   MemQL owns it and pushes changes out
//   Native              MemQL owns it and nobody else has a copy
//
// # Why "mirror" is the only one that gets a tone
//
// A badge that colours all three teaches a reader to skim past it. Only one
// of the three changes what a person may DO, and it is the one that has to
// register: a mirror is the concept where the edit button is missing and the
// reason is not the reader's permissions. Origin and native are the ordinary
// case, so they are neutral.
//
// # It reads the registry, never the DSL
//
// dataState / dataOrigin / dataMirroredTo arrive on the concept descriptor
// (ConceptInfo). Rendering from anything else -- a name convention, a hard
// coded list -- would be a second answer to a question the server already
// answers, and the two would disagree the first time a declaration changed.
//
// A descriptor from a server that predates the fields carries "", and this
// renders NOTHING rather than guessing "Native". An empty badge is honest;
// a wrong one is worse than none.

export type DataState = "mirror" | "origin" | "native" | "";

export interface OriginBadgeProps {
  dataState?: DataState | string;
  dataOrigin?: string;
  dataMirroredTo?: readonly string[];
  // syncedAgo renders beside a mirror's label ("synced 2 min ago") when the
  // caller has health for it. Omitted everywhere the health read is not in
  // scope, which is most places -- a badge is not worth a query.
  syncedAgo?: string;
}

export function originBadgeLabel(props: OriginBadgeProps): string {
  const origin = (props.dataOrigin ?? "").trim();
  switch (props.dataState) {
    case "mirror":
      return origin === "" ? "Mirror" : `Mirror of ${origin}`;
    case "origin": {
      const targets = (props.dataMirroredTo ?? []).filter((t) => t.trim() !== "");
      return targets.length === 0 ? "Origin" : `Origin → ${targets.join(", ")}`;
    }
    case "native":
      return "Native";
    default:
      return "";
  }
}

export function OriginBadge(props: OriginBadgeProps): ReactNode {
  const label = originBadgeLabel(props);
  if (label === "") return null;
  // Only a mirror is toned. See the header: a badge that colours everything
  // teaches a reader to skim past it.
  const tone: StatusTone = props.dataState === "mirror" ? "warn" : "neutral";
  const synced = (props.syncedAgo ?? "").trim();
  return (
    <Badge tone={tone}>
      {label}
      {props.dataState === "mirror" && synced !== "" ? ` · synced ${synced}` : ""}
    </Badge>
  );
}

// isMirror is the one question the rest of the portal asks of a descriptor:
// may a person write this? Exported so a caller reads the STATE rather than
// re-deriving it from the origin name, which would answer "shopify" for both
// a mirror of Shopify and an origin mirrored to it.
export function isMirror(concept: { dataState?: string } | null | undefined): boolean {
  return concept?.dataState === "mirror";
}
