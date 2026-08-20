// ModulesClient is the module-registry surface (epic memql#4183): the
// engine reporting what a cluster RUNS -- components, integrations, packs,
// node-type modules -- and the one registry write, flipping a pack's
// per-instance enablement.
//
// Kept as its own client rather than folded into QueryClient for the same
// reason sdk/go/modules stands alone: the registry is an operator surface
// with its own authorization tier (reads owner/admin, write owner-only),
// not part of the generated query/mutation vocabulary. SDK-owned value
// types below normalize the optional wire fields once, so consumers never
// touch `| undefined` soup; no wire type leaks out of the exported
// surface except through ./wire re-exports the portal deliberately avoids.
//
// Error semantics mirror the proto contract: a result carries
// errorCode/errorMessage INSIDE the payload (0 = OK), and the methods
// here throw on a non-zero code -- the caller sees one failure channel.

import type { Dispatcher } from "./dispatcher.js";
import { newShortId } from "./id.js";
import {
  readServerPayload,
  type ModuleEnvVarWire,
  type ModuleInfoWire,
} from "./wire.js";

export type ModuleKind = "component" | "integration" | "pack" | "node-type";

// Module is one registry row. `scope` says which truth tier the state is:
// "node" (the answering binary's registries and env) or "cluster" (the
// shared graph). stateDetail is the engine's one honest sentence about
// anything non-obvious (e.g. enabled-but-not-loaded pending restart).
export interface Module {
  kind: string;
  name: string;
  description: string;
  state: string;
  stateDetail: string;
  scope: string;
  envComponents: string[];
  fqnPrefixes: string[];
  codeReference: string;
}

// ModuleEnvVar is one manifest-declared environment variable, evaluated on
// the ANSWERING node. A secret entry carries set/unset and nothing else --
// `value` is always "" for secrets, by engine contract, and the SDK does
// not soften that into an optional the UI might "fill in" from elsewhere.
export interface ModuleEnvVar {
  name: string;
  description: string;
  secret: boolean;
  scope: string;
  requiredFor: string[];
  set: boolean;
  value: string;
  defaultValue: string;
}

export interface ModulesInventory {
  modules: Module[];
  reportingNodeId: string;
  reportingNodeType: string;
}

export interface ModuleDetail {
  module: Module;
  envVars: ModuleEnvVar[];
  reportingNodeId: string;
  reportingNodeType: string;
}

// SetPackEnabledOutcome echoes the graph state before and after the flip.
// restartRequired is ALWAYS true in v1 -- a flip changes what each node
// reads at its next boot, never what a running node has loaded -- and the
// UI must render that honestly rather than implying a live toggle.
export interface SetPackEnabledOutcome {
  packDomain: string;
  priorEnabled: boolean;
  enabled: boolean;
  restartRequired: boolean;
}

export interface ModulesCallOptions {
  signal?: AbortSignal;
}

function moduleFromWire(w: ModuleInfoWire | null | undefined): Module {
  return {
    kind: w?.kind ?? "",
    name: w?.name ?? "",
    description: w?.description ?? "",
    state: w?.state ?? "",
    stateDetail: w?.stateDetail ?? "",
    scope: w?.scope ?? "",
    envComponents: w?.envComponents ?? [],
    fqnPrefixes: w?.fqnPrefixes ?? [],
    codeReference: w?.codeReference ?? "",
  };
}

function envVarFromWire(w: ModuleEnvVarWire): ModuleEnvVar {
  return {
    name: w.name ?? "",
    description: w.description ?? "",
    secret: w.secret ?? false,
    scope: w.scope ?? "",
    requiredFor: w.requiredFor ?? [],
    set: w.set ?? false,
    value: w.value ?? "",
    defaultValue: w.defaultValue ?? "",
  };
}

function throwOnPayloadError(
  what: string,
  errorCode: number | undefined,
  errorMessage: string | undefined,
): void {
  if ((errorCode ?? 0) !== 0) {
    throw new Error(`${what}: ${errorMessage || `error code ${errorCode}`}`);
  }
}

export class ModulesClient {
  constructor(private readonly dispatcher: Dispatcher) {}

  async listModules(opts: ModulesCallOptions = {}): Promise<ModulesInventory> {
    const resp = await this.dispatcher.sendAndWait(
      { modulesList: { requestId: newShortId() } },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind !== "modulesListResult") {
      throw new Error("list modules: unexpected reply payload");
    }
    throwOnPayloadError("list modules", payload.value.errorCode, payload.value.errorMessage);
    return {
      modules: (payload.value.modules ?? []).map(moduleFromWire),
      reportingNodeId: payload.value.reportingNodeId ?? "",
      reportingNodeType: payload.value.reportingNodeType ?? "",
    };
  }

  async getModuleDetail(
    kind: string,
    name: string,
    opts: ModulesCallOptions = {},
  ): Promise<ModuleDetail> {
    const resp = await this.dispatcher.sendAndWait(
      { moduleDetail: { requestId: newShortId(), kind, name } },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind !== "moduleDetailResult") {
      throw new Error("module detail: unexpected reply payload");
    }
    throwOnPayloadError("module detail", payload.value.errorCode, payload.value.errorMessage);
    return {
      module: moduleFromWire(payload.value.module),
      envVars: (payload.value.envVars ?? []).map(envVarFromWire),
      reportingNodeId: payload.value.reportingNodeId ?? "",
      reportingNodeType: payload.value.reportingNodeType ?? "",
    };
  }

  async setPackEnabled(
    packDomain: string,
    enabled: boolean,
    reason: string,
    opts: ModulesCallOptions = {},
  ): Promise<SetPackEnabledOutcome> {
    const resp = await this.dispatcher.sendAndWait(
      { setPackEnabled: { requestId: newShortId(), packDomain, enabled, reason } },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind !== "setPackEnabledResult") {
      throw new Error("set pack enabled: unexpected reply payload");
    }
    throwOnPayloadError("set pack enabled", payload.value.errorCode, payload.value.errorMessage);
    return {
      packDomain: payload.value.packDomain ?? packDomain,
      priorEnabled: payload.value.priorEnabled ?? false,
      enabled: payload.value.enabled ?? enabled,
      restartRequired: payload.value.restartRequired ?? true,
    };
  }
}
