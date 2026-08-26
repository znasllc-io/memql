import {
  capabilityScriptPath,
  runCapabilityScript,
  withInstalledTools,
  type ScriptOutcome,
} from "./runner";

// Detecting a local model runtime, and offering to use it (epic memql#4676,
// task memql#4686).
//
// ===========================================================================
// THIS IS AN OFFER. NOTHING HERE IS A STEP OF ANY FLOW.
// ===========================================================================
// Install, uninstall, repair and update NEVER require inference (design D8),
// and that is a property with a test rather than an intention: see
// `inferenceFreeInstall.test.ts`, which drives the real flow paths with the
// inference seams stubbed to THROW if anything touches them.
//
// So this probe is deliberately outside the step graph. A graph step declares
// a verify obligation and a failure stops the run; a machine without Ollama
// must stop nothing. It is called alongside a flow, its answer steers an
// offer, and a decline changes nothing about whether the flow completes.
//
// ===========================================================================
// SAME BEHAVIOUR AS THE INSTALLER'S PROBE, BECAUSE IT IS THE SAME PROBE
// ===========================================================================
// This runs `scripts/install/detect-ollama.sh`, the capability the shell
// installer runs. One endpoint, one parse, one wording -- two entry points
// into one behaviour, rather than two behaviours that agree today.
//
// ===========================================================================
// WHAT IT DOES NOT DO
// ===========================================================================
// It does not install a runtime, and it does not register models with the
// cluster. Advertising a machine's models is the cockpit's job, over the
// worker stream it already holds open (memql-cockpit); this side detects,
// offers, and hands off.

/** What the probe found. */
export interface OllamaProbe {
  /** True when a local model runtime answered. */
  found: boolean;
  endpoint: string;
  runtime: string;
  models: string[];
  /**
   * Set when the probe itself could not run or the endpoint answered with
   * something that is not a tag list. NOT set when there is simply no runtime
   * -- that is `found: false` and no error at all.
   */
  error: string;
}

export interface OllamaProbeOptions {
  root: string;
  endpoint?: string;
  timeoutMs?: number;
  toolDir?: string;
  env?: NodeJS.ProcessEnv;
}

export interface OllamaProbeHooks {
  run?: typeof runCapabilityScript;
}

const NOT_FOUND: OllamaProbe = {
  found: false,
  endpoint: "",
  runtime: "",
  models: [],
  error: "",
};

function readProbe(outcome: ScriptOutcome): OllamaProbe {
  const envelope = outcome.envelope;
  if (!envelope) {
    return {
      ...NOT_FOUND,
      error: outcome.parseError ?? outcome.stderr.trim() ?? "the probe printed no result envelope",
    };
  }
  if (!envelope.ok) {
    return { ...NOT_FOUND, error: envelope.error?.message ?? "the probe refused" };
  }
  const result = (envelope.result ?? {}) as Record<string, unknown>;
  const models = Array.isArray(result.models)
    ? result.models.filter((m): m is string => typeof m === "string")
    : [];
  return {
    found: result.found === true,
    endpoint: typeof result.endpoint === "string" ? result.endpoint : "",
    runtime: typeof result.runtime === "string" ? result.runtime : "",
    models,
    error: "",
  };
}

/**
 * Probes for a local model runtime.
 *
 * NEVER THROWS AND NEVER FAILS A FLOW. Every answer -- found, not found, could
 * not run -- comes back as a value. A probe that could abort an install would
 * make an inference-free machine unable to install MemQL, which is the exact
 * property D8 exists to guarantee.
 */
export async function probeForLocalModels(
  opts: OllamaProbeOptions,
  hooks: OllamaProbeHooks = {},
): Promise<OllamaProbe> {
  const run = hooks.run ?? runCapabilityScript;
  const params: Record<string, string> = {};
  if (opts.endpoint) params.endpoint = opts.endpoint;
  try {
    const outcome = await run({
      scriptPath: capabilityScriptPath("install.detectOllama", opts.root),
      params,
      capability: "install.detectOllama",
      cwd: opts.root,
      env: withInstalledTools({ ...process.env, ...(opts.env ?? {}) }, opts.toolDir),
      timeoutMs: opts.timeoutMs,
    });
    return readProbe(outcome);
  } catch (err) {
    return { ...NOT_FOUND, error: err instanceof Error ? err.message : String(err) };
  }
}

/** What the extension should put in front of a person, given a probe. */
export interface LocalModelOffer {
  /** False means say nothing at all -- there is nothing to offer. */
  show: boolean;
  title: string;
  detail: string;
  /** The label of the affirmative action; empty when show is false. */
  accept: string;
}

// MODEL_FLOOR is what onboarding RECOMMENDS installing. It is not what
// ENFORCES anything: the engine's catalog capability gating decides per call
// whether a model can serve a given prompt (memql#4679). Stating both here
// keeps this copy from drifting into a promise the runtime does not make.
export const MODEL_FLOOR = "a 7-8B instruct model with structured output (llama3.1:8b or qwen2.5:7b class)";

/**
 * Turns a probe into an offer.
 *
 * NOTHING IS OFFERED WHEN NOTHING WAS FOUND, and that is the whole of the
 * inference-optional posture at the UI. A machine with no runtime sees no
 * prompt, no warning and no nudge during an install -- an install is not the
 * moment to sell somebody a capability they did not ask for, and a dialog that
 * appears only on the machines that CANNOT use the feature is the worst
 * possible place to put one.
 *
 * A probe that ERRORED offers nothing either. We could not tell, so we say
 * nothing rather than guessing in either direction.
 */
export function offerFromProbe(probe: OllamaProbe): LocalModelOffer {
  if (!probe.found || probe.error !== "") {
    return { show: false, title: "", detail: "", accept: "" };
  }
  const count = probe.models.length;
  if (count === 0) {
    return {
      show: true,
      title: "This machine has a model runtime, but no models",
      detail:
        `${probe.runtime || "A local runtime"} is answering at ${probe.endpoint}, with nothing ` +
        `installed. Pull ${MODEL_FLOOR} and MemQL can run its planning, routing and suggestions ` +
        `here at no per-token cost.`,
      accept: "Show me how",
    };
  }
  return {
    show: true,
    title: `Use the ${count === 1 ? "model" : "models"} already on this machine`,
    detail:
      `${probe.runtime || "A local runtime"} is hosting ${probe.models.join(", ")}. MemQL can run ` +
      `its own planning, routing, suggestions and embeddings on ${count === 1 ? "it" : "them"} ` +
      `instead of a metered API. You can change this later.`,
    accept: "Use local models",
  };
}
