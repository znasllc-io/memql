import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { WORDS } from "./words";

// WHY A SEND AS A SENDER WOULD BE REFUSED, when a person fixes it on the sender
// or on its sending domain rather than by trying again. It is the engine's own
// sending check (`campaignSendingReadiness` for one sender), which anyone who
// may send can ask and which, for Azure, is the check a real send makes: the
// address registered on the domain, and the display name the one Azure holds.
// No hooks here: `useSenderChecks` (useEmail) reads it; these read the answer.
//
//   unregistered     Azure has not registered the address on the domain
//   renamed          it has, under another display name
//   azureOwnDomain   the organization sends through Azure's own domain, which
//                    registers no sender but its own
//
// "" is no such refusal: ready, refused for another reason, or not known.

export type SenderRefusal = "" | "unregistered" | "renamed" | "azureOwnDomain";

/** The refusal a readiness answer names, if it is one of these. */
export function senderRefusalFrom(row: Row): SenderRefusal {
  const named = flatten(row).senderRefusal;
  return named === "unregistered" || named === "renamed" || named === "azureOwnDomain" ? named : "";
}

/** The state word for a sender a send would be refused as; "" for none. */
export function senderRefusalWord(refusal: SenderRefusal): string {
  return refusal === "" ? "" : refusal === "renamed" ? WORDS.renamedInAzure : WORDS.notRegistered;
}
