// What things are CALLED on the Integrations section (issue #4826).
//
// The engine names a slot for the value it holds (`senderAddress`) and names
// the environment variable that supplies it (`MEMQL_EMAIL_SENDER`). Neither
// is what a person controls, and neither is what they would go looking for.
// So the label is the thing -- "Sending mailbox" -- and the variable name
// rides along as secondary text, because an operator setting it in a cluster
// needs exactly that string.
//
// A NAME THIS MAP DOES NOT KNOW IS STILL RENDERED AS WORDS, never blank and
// never dropped. Stream A adds slots; a section that showed only the slots it
// had been told about would hide a required credential and read as complete.

import type { IntegrationState } from "./integrationsReport";

const SLOT_LABELS: Readonly<Record<string, string>> = {
  tenantId: "Microsoft tenant",
  clientId: "Application ID",
  clientSecret: "Application secret",
  senderAddress: "Sending mailbox",
  fromName: "From name",
  smtpHost: "Relay host",
  smtpPort: "Relay port",
  smtpUsername: "Relay username",
  smtpPassword: "Relay password",
  smtpFromAddress: "From address",
  smtpFromName: "Relay from name",
};

const INTEGRATION_LABELS: Readonly<Record<string, string>> = {
  email: "Email",
};

/**
 * What this integration would let the cluster do, in the reader's terms.
 *
 * Shown above an unconfigured card, because an unconfigured integration is a
 * normal state and the card should read as an invitation rather than a fault.
 * An integration with no entry here gets NO invented sentence -- the engine's
 * own detail is the whole message there, which is the same rule the rest of
 * the section follows.
 */
const INTEGRATION_BLURBS: Readonly<Record<string, string>> = {
  email:
    "Sends this cluster's mail: sign-in links, guest invitations, new-sign-in notices and campaign sends.",
};

export function slotLabel(name: string): string {
  return SLOT_LABELS[name] ?? humanize(name);
}

export function integrationLabel(name: string): string {
  return INTEGRATION_LABELS[name] ?? humanize(name);
}

export function integrationBlurb(name: string): string {
  return INTEGRATION_BLURBS[name] ?? "";
}

/** The state, as a word. */
export function stateLabel(state: IntegrationState): string {
  if (state === "configured") return "Configured";
  if (state === "unhealthy") return "Unhealthy";
  if (state === "needs_configuration") return "Needs configuration";
  return "Not reported";
}

/**
 * Where a value came from, which is what decides how somebody changes it.
 *
 * `env` is the one that has to say more than its own name: it is not just a
 * source, it is the reason the field below it is not a field.
 */
export function sourceLabel(source: string): string {
  // None of these starts with "Set". The presence chip beside it says "Set" or
  // "Not set", and a source reading "Set in this node's environment" put two
  // chips saying almost the same word next to each other -- which is how a
  // reader stops reading either.
  if (source === "env") return "From this node's environment";
  if (source === "globalVariable") return "Stored in the cluster";
  if (source === "globalSecret") return "Sealed in the cluster";
  return "No source";
}

/** camelCase to a sentence: `senderAddress` becomes `Sender address`. */
function humanize(name: string): string {
  const spaced = name.replace(/([a-z0-9])([A-Z])/g, "$1 $2").trim();
  if (spaced === "") return "";
  return spaced.charAt(0).toUpperCase() + spaced.slice(1).toLowerCase();
}
