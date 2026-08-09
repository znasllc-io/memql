import type { ConceptLike, RowLike } from "@znasllc-io/memql-view-kit";

import type {
  IntegrationCredential,
  IntegrationReport,
  IntegrationSetting,
} from "./calls";

// Adapting the integration-status reply into row sets the element library can
// render (memql#3323).
//
// Same job, same reasoning, and deliberately the same shape as
// src/deploy/rows.ts: an integration's registration and configuration is
// PROCESS STATE, not graph state -- it differs per replica, per node-type
// build, and it is recomputed at boot. There is no concept declaring it and
// there should not be. So it is adapted here into a flat row set plus a
// ConceptLike naming which field is the primary, the secondary and the status,
// and from that point on it renders through exactly the same table and
// checklist every concept row does.
//
// THE DESCRIPTORS ARE NOT CONCEPTS and their ids say so: `integrations.email`,
// never a `{version}:{domain}:{entity}` id. Nothing in the DSL declares them
// and an id in that grammar would be a claim that something does.
//
// Campaign rows are NOT adapted here -- they are real concept rows and arrive
// with a real concept id and a declared @displayCard. They go through
// useConcepts / the registry like everything else.

export const INTEGRATION_CONCEPT: ConceptLike = {
  id: "integrations.integration",
  entity: "integration",
  // `configured` is the status slot rather than `health`, and the choice is
  // load-bearing: the registry list answers "is this set up", and health is a
  // separate, later question that most rows honestly cannot answer. Binding
  // the badge to health would paint every unprobed integration the same
  // colour as a broken one.
  displayCard: { primary: "name", secondary: "mode", tertiary: "health", status: "configured" },
};

export const SETTING_CONCEPT: ConceptLike = {
  id: "integrations.setting",
  entity: "setting",
  displayCard: { primary: "name", secondary: "value", tertiary: "envVar", status: "source" },
};

export const CREDENTIAL_CONCEPT: ConceptLike = {
  id: "integrations.credential",
  entity: "credential",
  // `present` is the status slot, which is what lets the checklist bind its
  // `done` requirement without this module naming a field. There is no value
  // field to bind, and there never will be.
  displayCard: { primary: "name", secondary: "purpose", tertiary: "envVar", status: "present" },
};

// integrationRows: one row per registered plug-in.
export function integrationRows(reports: readonly IntegrationReport[]): RowLike[] {
  return reports.map((report) => ({
    id: report.name,
    name: report.name,
    configured: report.configured,
    health: report.health,
    mode: report.mode === "" ? "-" : report.mode,
    capabilities: report.capabilities.length,
    detail: report.detail,
  }));
}

// settingRows: one row per NON-SECRET configuration value, with its value.
export function settingRows(settings: readonly IntegrationSetting[]): RowLike[] {
  return settings.map((setting) => ({
    id: setting.name,
    name: setting.name,
    // An unset setting renders as an em dash rather than as an empty cell, so
    // "nobody set this" is distinguishable from "the table failed to load".
    value: setting.value === "" ? "—" : setting.value,
    source: setting.source,
    envVar: setting.envVar,
    purpose: setting.purpose,
  }));
}

// credentialRows: one row per SECRET slot.
//
// Note what is absent and note that it is absent BY CONSTRUCTION rather than by
// omission: the IntegrationCredential type this reads has no value field, the
// Go handler that produces it never writes one, and a test there sweeps the
// serialized reply for planted credential values. There is nothing here to
// forget to leave out.
export function credentialRows(credentials: readonly IntegrationCredential[]): RowLike[] {
  return credentials.map((credential) => ({
    id: credential.name,
    name: credential.name,
    present: credential.present,
    source: credential.source,
    envVar: credential.envVar,
    purpose: credential.purpose,
    rotate: credential.rotate,
  }));
}

// emailReport picks the email line out of the roll-call. Undefined when this
// node did not register the email plug-in at all, which is a real state for a
// node type whose build tags leave it out -- and a different statement from
// "email is not configured".
export function emailReport(
  reports: readonly IntegrationReport[],
): IntegrationReport | undefined {
  return reports.find((report) => report.name === "email");
}
