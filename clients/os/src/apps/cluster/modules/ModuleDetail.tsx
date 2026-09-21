import { useCallback, useMemo, useState } from "react";
import type { Module, ModuleDetail as ModuleDetailWire, ModulesClient } from "@znasllc-io/memql-sdk-core/client";

import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import { Button, Caption, Chip, Fact, Facts, Head, Notice, Panel, Subhead, roleAdmits, stateWords } from "../../../kit";
import { useSession } from "../../../chrome/access";
import { useReading } from "../../../cluster/reading";
import type { Verdict } from "../../../system/readinessFold";
import {
  envVarReading,
  flipOutcomeSentence,
  isFlippable,
  noSwitchSentence,
  packBarDetail,
  packOffReading,
  readinessForModule,
  readinessNodeLines,
} from "./rows";

// One module, and the only registry WRITE in the product.
//
// ===========================================================================
// A SECRET RENDERS set / unset AND NEVER A VALUE
// ===========================================================================
// This is a property of the wire, not a decision taken here: the engine's
// contract is that a secret env var's `value` is always "", there is no
// reveal call anywhere in the protocol, and the proto carries no field one
// could be added to. So the page says so in words beside the reading, rather
// than leaving a reader to wonder whether the blank is a missing value or a
// hidden one. `envVarReading` is where the refusal lives, so it holds even if
// something upstream ever put a value in that field.
//
// ===========================================================================
// THE PACK SWITCH IS NOT A LIVE TOGGLE, AND IT SAYS SO
// ===========================================================================
// `setPackEnabled` returns `restartRequired`, and in v1 that is ALWAYS true:
// the flip writes a graph row that each node reads at its NEXT BOOT. A switch
// that read as live would tell an operator the pack is off while every
// running node still has it loaded -- which is the one thing they came here
// to find out. So the bar says it before the act, the confirmation says it
// during, and the outcome says it after.

export function ModuleDetail({
  client,
  module,
  reportingNodeId,
  reportingNodeType,
  onBack,
  onFlipped,
}: {
  client: ModulesClient;
  module: Module;
  reportingNodeId: string;
  reportingNodeType: string;
  onBack: () => void;
  onFlipped: () => void;
}) {
  const session = useSession();
  // OWNER-ONLY, AND ABSENT RATHER THAN DISABLED (DESIGN.md rule 12). The
  // engine's write gate on the registry is cluster-owner; an admin who can
  // READ this page cannot flip a pack. A greyed-out switch would be a control
  // they have to read past to learn it is not for them, and an enabled one
  // would be a refusal they find out about by being told no.
  const isOwner = roleAdmits(session.access?.role ?? "", { min: "owner" });
  // The cluster-wide reading of this module, when the registry's name is also
  // a readiness module -- the one feed the shell retains, never a second read.
  const verdict = readinessForModule(module, session.readiness);

  const read = useCallback(
    (signal: AbortSignal): Promise<ModuleDetailWire> =>
      client.getModuleDetail(module.kind, module.name, { signal }),
    [client, module.kind, module.name],
  );
  const detail = useReading<ModuleDetailWire>(`cluster:module:${module.kind}/${module.name}`, read);

  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState("");
  const [refusal, setRefusal] = useState("");

  const enabled = isOn(module.state);
  const flippable = isFlippable(module);

  const flip = useCallback(async () => {
    setBusy(true);
    setRefusal("");
    setOutcome("");
    try {
      const result = await client.setPackEnabled(
        module.name,
        !enabled,
        "Flipped from MemQL OS, Cluster -> Modules.",
      );
      setOutcome(flipOutcomeSentence(result.packDomain, result.enabled));
      setConfirming(false);
      onFlipped();
    } catch (err: unknown) {
      setRefusal(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [client, module.name, enabled, onFlipped]);

  // The acts legal from this state, computed rather than rendered-then-hidden.
  const acts: Act[] = useMemo(() => {
    if (!flippable || !isOwner) return [];
    return [
      {
        label: enabled ? "Disable this pack" : "Enable this pack",
        tone: enabled ? "danger" : "primary",
        onAct: () => setConfirming(true),
      },
    ];
  }, [flippable, isOwner, enabled]);

  const envVars = detail.value?.envVars ?? [];

  return (
    <div className="os-cluster-pane">
      <div className="os-cluster-scroll">
        <Panel label={`Module ${module.name}`}>
          {/* ONE Head, and its only control goes BACK. Every act that changes
              the module's state is on the bar (rule 12). */}
          <Head title={module.name} meta={`${module.kind} -- ${module.scope || "unscoped"}`} back={{ label: "Modules", onSelect: onBack }} />

          <Facts>
            <Fact label="State" value={module.state || "unstated"} mono />
            <Fact label="What it is" value={module.description} />
            <Fact label="Engine says" value={module.stateDetail} />
            {flippable && !enabled && packOffReading(module.stateDetail) !== "" ? (
              <Fact label="Why it is off" value={packOffReading(module.stateDetail)} />
            ) : null}
            <Fact label="Code" value={module.codeReference} mono />
            <Fact
              label="Answered by"
              value={`${reportingNodeId || "an unnamed node"} (${reportingNodeType || "unknown type"})`}
              mono
              title="A node-scoped state is this binary's own. A sibling replica can answer differently."
            />
          </Facts>

          {flippable ? null : (
            // NOT A DISABLED BUTTON. The sentence names what DOES change the
            // state, so the reader leaves with somewhere to go.
            <Caption>{noSwitchSentence(module.kind)}</Caption>
          )}

          {flippable && !isOwner ? (
            <Caption>
              Only a cluster owner can change what a pack does. Nothing on this page will change it
              for you.
            </Caption>
          ) : null}

          {verdict ? <ReadinessAcross verdict={verdict} /> : null}

          <Subhead>Environment</Subhead>
          {detail.state === "failed" ? (
            <Notice
              tone="error"
              sentence="The cluster did not answer this module's environment."
              detail={detail.error}
            />
          ) : null}
          {detail.state === "reading" && detail.value === null ? (
            <Caption>Reading this module's environment.</Caption>
          ) : null}
          {detail.state === "read" && envVars.length === 0 ? (
            <Caption>This module declares no environment variables.</Caption>
          ) : null}

          {envVars.length === 0 ? null : (
            <>
              <div className="os-cluster-env" role="list" aria-label={`${module.name} environment`}>
                {envVars.map((v) => (
                  <div key={v.name} className="os-cluster-env-row" role="listitem">
                    <span className="os-cluster-env-name os-mono">{v.name}</span>
                    <span className="os-cluster-env-value os-mono" data-unset={!v.set || undefined}>
                      {envVarReading(v)}
                    </span>
                    <span className="os-cluster-env-marks">
                      {v.secret ? (
                        <Chip
                          tone="accent"
                          title="The value never leaves the engine: a secret entry comes back as set or unset and nothing else, and there is no call anywhere in the protocol that would return one."
                        >
                          secret
                        </Chip>
                      ) : null}
                      {v.scope === "" ? null : <Chip tone="muted">{v.scope}</Chip>}
                    </span>
                    <span className="os-cluster-env-note">
                      {v.description}
                      {!v.secret && v.defaultValue !== "" ? (
                        <span className="os-cluster-env-default os-mono">
                          {" "}
                          default {v.defaultValue}
                        </span>
                      ) : null}
                      {v.requiredFor.length === 0 ? null : (
                        <span className="os-cluster-env-required">
                          {" "}
                          required for {v.requiredFor.join(", ")}
                        </span>
                      )}
                    </span>
                  </div>
                ))}
              </div>
              <Caption>
                A secret reads set or unset and never a value -- the value never leaves the engine.
                Everything here was evaluated on the node named above, so another replica can hold a
                different environment.
              </Caption>
            </>
          )}

          {outcome === "" ? null : (
            <Notice tone="info" sentence={outcome} />
          )}
          {refusal === "" ? null : (
            <Notice
              tone="error"
              sentence="The cluster refused the change, and nothing was written."
              detail={refusal}
            />
          )}
        </Panel>
      </div>

      <ActionBar
        state={barState(module.state, flippable, enabled)}
        detail={flippable ? packBarDetail(module.stateDetail) : module.stateDetail}
        tone={barTone(module.state)}
        acts={confirming ? [] : acts}
      >
        {confirming ? (
          <span className="os-cluster-confirm">
            <span className="os-cluster-confirm-text">
              {enabled
                ? `Disable ${module.name}? Nodes keep running it until they restart.`
                : `Enable ${module.name}? Nodes pick it up when they restart.`}
            </span>
            <Button tone="quiet" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button
              tone={enabled ? "danger" : "primary"}
              busy={busy}
              busyLabel="Recording"
              onClick={() => void flip()}
            >
              {enabled ? "Disable" : "Enable"}
            </Button>
          </span>
        ) : null}
      </ActionBar>
    </div>
  );
}

/** Whether the engine's state word means the module is going. */
function isOn(state: string): boolean {
  return state === "enabled" || state === "active" || state === "built_in" || state === "running";
}

/** The state in the words the bar wants: what it IS, not the enum member.
 *  For a pack the enablement is the lifecycle, so that is what is named. */
function barState(state: string, flippable: boolean, enabled: boolean): string {
  if (flippable) return enabled ? "Enabled" : "Disabled";
  return state === "" ? "Unstated" : state;
}

/**
 * The dot. `live` for going, `paused` for not -- including the two states an
 * operator has something to do about, because a credential-gated module is
 * not running either. An unrecognised state gets NO dot rather than the
 * nearest one this build knows: a guessed colour is a claim.
 */
function barTone(state: string): ActionBarTone {
  if (isOn(state)) return "live";
  if (
    state === "disabled" ||
    state === "compiled_out" ||
    state === "scaled_to_zero" ||
    state === "not_deployed" ||
    state === "credential_gated" ||
    state === "opted_out"
  ) {
    return "paused";
  }
  return "none";
}

/**
 * This module on every live node (memql#5259).
 *
 * The Facts above are ONE node's -- the one that answered the inventory -- and
 * the verdict every app's mark reads is every live node's, folded. This is the
 * fold opened up: each node's own answer and how fresh it is, and, drawn
 * quieter, the nodes the fold set aside. A node behind the cluster used to
 * surface only as "Partly set up" beside a list of pod names; here it is named
 * with the time it last checked and what it will do about it, which is the
 * difference between an operator waiting ten minutes and an operator
 * restarting a pod that needed nothing.
 *
 * READ FROM THE LIVE FEED, so it moves when a node re-checks; the freshness
 * words are computed at render, which the feed's own changes drive.
 */
function ReadinessAcross({ verdict }: { verdict: Verdict }) {
  const lines = readinessNodeLines(verdict, new Date());
  return (
    <>
      <Subhead>Across the cluster</Subhead>
      <p className="os-cluster-fact">{acrossSentence(verdict)}</p>
      {lines.length === 0 ? null : (
        <div className="os-cluster-readiness" role="list" aria-label={`${verdict.module} on each live node`}>
          {lines.map((line) => (
            <div
              key={line.nodeId}
              className="os-cluster-readiness-row"
              role="listitem"
              data-aside={line.counted ? undefined : true}
            >
              <span className="os-cluster-readiness-node os-mono">{line.nodeId}</span>
              <span className="os-cluster-readiness-words">{line.words}</span>
              <span className="os-cluster-readiness-note">
                {line.nodeType} &middot; {line.note}
              </span>
            </div>
          ))}
        </div>
      )}
      <Caption>
        Every live node checks this module for itself. A node that has not re-checked since the
        cluster last changed, or that could not check, is listed and not counted.
      </Caption>
    </>
  );
}

/**
 * The verdict as one sentence: the words, and how many nodes stand behind
 * them. "All N nodes that count agree" rather than "on all N nodes", which
 * reads wrong the moment the words are negative -- "Not set up on all 5"
 * says "not on every one", which is not what five nodes agreeing means.
 */
function acrossSentence(v: Verdict): string {
  const words = stateWords(v);
  if (v.state === "unreported") {
    return v.unknown.length > 0
      ? `${words}. No live node could finish it.`
      : `${words}. No live node has reported this module.`;
  }
  const counted = v.nodes.length;
  if (v.state === "partial") {
    const setUp = v.nodes.filter((n) => n.state === "configured").length;
    if (setUp > 0 && setUp < counted) return `${words}: ${setUp} of the ${counted} that count.`;
  }
  return counted === 1 ? `${words}. The one node that counts says so.` : `${words}. All ${counted} nodes that count agree.`;
}
