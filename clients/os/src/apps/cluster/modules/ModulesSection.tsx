import { useCallback, useMemo, useState } from "react";
import { ModulesClient, type ModulesInventory } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Head, Notice, Panel, RecordList, RecordRow, Subhead, setAsideLabel, stateWords, verdictDetail } from "../../../kit";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { ModuleDetail } from "./ModuleDetail";
import {
  groupModules,
  moduleStateNeedsAttention,
  moduleStateSentence,
  readinessForModule,
} from "./rows";

// Modules: what this cluster is MADE OF, as the answering binary knows it.
//
// ===========================================================================
// THE ATTRIBUTION IS NOT DECORATION
// ===========================================================================
// `listModules` answers from the registries and the environment of the ONE
// node that handled the request, and a `scope: "node"` row is that binary's
// own truth. The same question asked a second later can land on a sibling
// replica and answer differently -- one restarted after a key was seeded and
// one not is the ordinary case, not a fault. So the reporting node is on the
// Head, beside the title, every time: a page that showed this inventory
// without saying whose it is would present a per-node reading as a cluster
// fact, and the disagreement it hides is the single most useful thing an
// operator could learn from it.
//
// ===========================================================================
// RULE 11: THE DETAIL REPLACES THE LIST
// ===========================================================================
// Both shapes in DESIGN.md rule 11 are right and the rule says the choice is
// "how tall the detail is". A module's detail is its manifest-declared
// environment, and a real one -- identity's, the bff's -- is dozens of
// entries each carrying a description sentence, a scope, a requiredFor list
// and a value. That does not fit in the Bin's 380px column at a readable
// measure, and squeezing it there would wrap every description to four lines.
//
// The other half of the choice is DESIGN.md rule 12: the pack switch is a
// lifecycle act, it belongs on ONE bar on the window's bottom edge with the
// state in words beside it, and an ActionBar is a property of a PANE -- it
// cannot sit under a 380px column beside a list that has its own scroller.
//
// So this is the `DeployablePage` shape: the detail replaces the list and
// carries a quiet `<- Modules` in its Head. The list is grouped and scanned
// rather than browsed row by row, so losing your place costs little.

export function ModulesSection() {
  const connection = useOsConnection();
  // The readiness feed the shell already retains -- not a second read of the
  // same rows, which is the rule the feed's own file states.
  const { readiness } = useSession();

  // The client is constructed from the dispatcher, not from `query`: the
  // module registry is its own surface with its own authorization tier
  // (reads owner/admin, the write owner-only), which is why the SDK keeps it
  // out of the generated query vocabulary.
  const modules = useMemo(
    () => (connection === null ? null : new ModulesClient(connection.dispatcher)),
    [connection],
  );

  const read = useCallback(
    (signal: AbortSignal): Promise<ModulesInventory> => {
      if (modules === null) return Promise.reject(new Error("not connected"));
      return modules.listModules({ signal });
    },
    [modules],
  );

  const inventory = useReading<ModulesInventory>(
    "cluster:modules",
    modules === null ? null : read,
  );

  const [openKey, setOpenKey] = useState("");

  const groups = useMemo(
    () => groupModules(inventory.value?.modules ?? []),
    [inventory.value],
  );

  const open = useMemo(() => {
    if (openKey === "") return null;
    return (inventory.value?.modules ?? []).find((m) => `${m.kind}/${m.name}` === openKey) ?? null;
  }, [inventory.value, openKey]);

  if (open !== null && modules !== null) {
    return (
      <ModuleDetail
        client={modules}
        module={open}
        reportingNodeId={inventory.value?.reportingNodeId ?? ""}
        reportingNodeType={inventory.value?.reportingNodeType ?? ""}
        onBack={() => setOpenKey("")}
        onFlipped={() => inventory.reread()}
      />
    );
  }

  const answeredBy =
    inventory.value === null
      ? null
      : `answered by ${inventory.value.reportingNodeId || "an unnamed node"} (${
          inventory.value.reportingNodeType || "unknown type"
        })`;

  return (
    <div className="os-cluster">
      {/* The one control on the Head, and it is quiet rather than primary:
          this reading is not live, so "look again" is the honest companion to
          printing when we last looked. It also re-asks after a reconnect,
          which the reading's key deliberately does not do on its own. */}
      <Head title="Modules" meta={inventory.state === "read" && !inventory.error ? inventory.value?.modules.length : undefined}>
        <Button
          tone="quiet"
          busy={inventory.state === "reading"}
          busyLabel="Reading"
          onClick={() => inventory.reread()}
        >
          Read again
        </Button>
      </Head>

      {inventory.state === "failed" ? (
        // The server's own sentence, verbatim. A wrapper of ours ("could not
        // load modules") replaces the message naming the actual refusal with
        // one naming the surface.
        <Notice
          tone="error"
          sentence="The cluster did not answer the module inventory."
          detail={inventory.error}
        />
      ) : null}

      {inventory.state === "reading" && inventory.value === null ? (
        <Caption>Reading the inventory from the cluster.</Caption>
      ) : null}

      {answeredBy ? <Caption>{answeredBy}</Caption> : null}
      {groups.map((group) => (
        <Panel key={group.kind} label={group.name}>
          <Subhead meta={inventory.state === "read" && !inventory.error ? group.modules.length : undefined}>{group.name}</Subhead>
          <RecordList as="ul" label={group.name}>
          {group.modules.map((module) => {
            const sentence = moduleStateSentence(module.state);
            const verdict = readinessForModule(module, readiness);
            return (
              <RecordRow
                key={`${module.kind}/${module.name}`}
                name={module.name}
                onOpen={() => setOpenKey(`${module.kind}/${module.name}`)}
                secondary={module.stateDetail || module.description}
                state={module.state || "unstated"}
                tone={moduleStateNeedsAttention(module.state) ? "warn" : "muted"}
                stateTitle={sentence || undefined}
                stateExtra={
                  <>
                    <Chip tone="muted" title={scopeTitle(module.scope)}>
                      {module.scope || "unscoped"}
                    </Chip>
                    {/* The CLUSTER-WIDE reading, beside this node's own, so
                        the operator's inventory and the apps' marks are one
                        answer rather than two that can quietly differ. The
                        nodes behind it are the detail's, one click away; here
                        they are a hover and, when the fold set any aside, a
                        count -- never the raw nodeId=state pairs, which put a
                        row's width into a list of pod names. */}
                    {verdict ? (
                      <Chip
                        tone={verdict.state === "configured" ? "muted" : "accent"}
                        title={
                          verdictDetail(verdict) ||
                          "Every live node's answer, folded. This row's other chips are the answering node's own."
                        }
                      >
                        {stateWords(verdict)}
                      </Chip>
                    ) : null}
                    {setAsideLabel(verdict) ? (
                      <Chip tone="muted" title={verdictDetail(verdict)}>
                        {setAsideLabel(verdict)}
                      </Chip>
                    ) : null}
                  </>
                }
              >
                {moduleStateNeedsAttention(module.state) ? (
                  <span className="os-cluster-row-attention">{sentence}</span>
                ) : null}
              </RecordRow>
            );
          })}
          </RecordList>
        </Panel>
      ))}

      {inventory.state === "read" && groups.length === 0 ? (
        <Caption>This node reported no modules at all, which is not a state a running engine reaches -- read its logs.</Caption>
      ) : null}

      {inventory.at === null ? null : (
        <Caption>
          Read {inventory.at.toLocaleTimeString()}. This inventory is not live; nothing broadcasts a
          registry change.
        </Caption>
      )}
    </div>
  );
}

/** What a scope word MEANS, one hover away. `node` is the one that matters:
 *  it says this row is the answering binary's own truth rather than the
 *  cluster's. */
function scopeTitle(scope: string): string {
  if (scope === "node") {
    return "This state is the answering binary's own: its registries and its environment. A sibling replica can answer differently.";
  }
  if (scope === "cluster") {
    return "This state is the shared graph's, so every node agrees about it.";
  }
  return "";
}
